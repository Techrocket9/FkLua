//! The crate singleton, in a test binary OF ITS OWN.
//!
//! Its own file because `LINK` is a process-wide `static` whose whole soundness
//! argument is "a Factorio mod is single-threaded", and `cargo test` runs the
//! tests inside one binary in PARALLEL THREADS. Every other test drives an
//! independent `Link` through `attach` for that reason; this one cannot, so it
//! is alone in its binary and therefore alone on its link.

mod harness;

use std::cell::Cell;

use fkipc::wire::Type;
use fkipc::{Channel, Config, Link, Priority, Request, Status};

use harness::{ack, bus, inject, req, Bus, TestTransport};

thread_local! {
    /// What `Channel::send` answered when called from INSIDE a handler.
    static NESTED: Cell<Option<Status>> = const { Cell::new(None) };
}

const CH: Channel = Channel::new(2);

/// A handler that does the wrong thing on purpose: reaches the singleton while
/// the singleton is already borrowed.
fn reentrant(l: &mut Link, r: Request) -> &'static [u8] {
    NESTED.with(|c| c.set(Some(CH.send(b"through the singleton"))));
    // ...and the RIGHT thing, through the borrow it was handed.
    l.send(r.channel, b"through the link");
    b"ok"
}

/// A HANDLER THAT REACHES THE SINGLETON IS REFUSED, NOT ALIASED -- and the
/// `&mut Link` it was handed works.
///
/// The Go half has no such distinction: `state.Snapshot(...)` from inside
/// `OnResync` is its documented shape, because Go does not mind a second
/// reference to the package's link. Here that is two live `&mut Link` to one
/// object, which is undefined behaviour rather than a style question, so the
/// handler's own borrow is the route and the singleton's `busy` flag turns the
/// other one into a counted refusal.
#[test]
fn a_handler_reaching_the_singleton_is_refused_and_its_own_link_works() {
    bus(|b| *b = Bus::default());
    let st = fkipc::open_with(
        Config {
            port: 29434,
            name: "singleton",
            ..Default::default()
        },
        Box::new(TestTransport),
    );
    assert_eq!(st, Status::Ok);
    assert_eq!(CH.open(Priority::Control), Status::Ok);
    assert_eq!(CH.on_request(reentrant), Status::Ok);

    // The handshake, through the crate-level entry points a guest really uses.
    fkipc::pump(1);
    let corr = bus(|b| {
        let f = b.from_guest.last().expect("no HELLO").clone();
        fkipc::wire::decode(&f).unwrap().0.corr
    });
    ack(0x1BAD, corr);
    fkipc::pump(2);
    assert!(fkipc::stats().up, "the singleton never came up");

    inject(req(2, fkipc::stats().epoch, 1, 5, false, b"ask"));
    fkipc::pump(3);
    fkipc::pump(4);

    assert_eq!(
        NESTED.with(|c| c.get()),
        Some(Status::NotOpen),
        "a re-entrant reach into the singleton must be refused; anything else \
         means two live &mut Link to one object"
    );

    // The supported route reached the wire, so the refusal above is about
    // re-entrancy and not about the handler being unable to send at all.
    let sent: Vec<_> = bus(|b| {
        b.from_guest
            .iter()
            .filter_map(|f| fkipc::wire::decode(f).ok().map(|(h, p)| (h.ty, p.to_vec())))
            .collect()
    });
    assert!(
        sent.iter()
            .any(|(ty, p)| *ty == Type::MSG && p == b"through the link"),
        "the handler's own &mut Link did not reach the wire: {:?}",
        sent.iter().map(|(t, _)| t.as_str()).collect::<Vec<_>>()
    );
    assert!(
        sent.iter().any(|(ty, p)| *ty == Type::RESP && p == b"ok"),
        "the handler's response never went out"
    );
    assert!(
        !sent.iter().any(|(_, p)| p == b"through the singleton"),
        "the refused send reached the wire anyway"
    );
}
