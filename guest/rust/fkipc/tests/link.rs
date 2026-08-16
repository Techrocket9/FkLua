//! The state machine, over the native transport.
//!
//! The Go half's conformance suite drives `guest/go/fkipc` against the real
//! `sdk/go` SDK; there is no Rust SDK, so this plays the peer by hand -- which
//! is strictly weaker in one way (no second implementation of the protocol is
//! being exercised) and stronger in another (a hand-crafted peer can put a
//! header on the wire that no correct implementation would produce, which is
//! how the wrap arm and the nfrag disagreement get tested at all).
//!
//! Handler state lives in `thread_local!`s because handlers are `fn` pointers
//! with nothing to capture, and because `cargo test` gives each test its own
//! thread.

mod harness;

use std::cell::{Cell, RefCell};

use fkipc::wire::{self, Flags, Header, Type};
use fkipc::{Corr, Link, Message, Priority, Profile, Reply, ReplyError, Request, SessionEvent};
use fkipc::{Status, Version};

use fkipc::MIN_ENGINE_VERSION;
use harness::{ack, ack_named, bus, craft, inject, msg, new_harness, req, resp, snap, Opts};

thread_local! {
    static SEEN: RefCell<Vec<(u32, Vec<u8>, bool)>> = const { RefCell::new(Vec::new()) };
    static GAPS: RefCell<Vec<u32>> = const { RefCell::new(Vec::new()) };
    static REPLIES: RefCell<Vec<(u32, Vec<u8>, Option<ReplyError>)>> = const { RefCell::new(Vec::new()) };
    static SESSIONS: RefCell<Vec<SessionEvent>> = const { RefCell::new(Vec::new()) };
    static SERVED: RefCell<u32> = const { RefCell::new(0) };
    static ECHO: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static RESYNCS: RefCell<u32> = const { RefCell::new(0) };
    static NESTED_WRITE: Cell<Option<Status>> = const { Cell::new(None) };
}

fn record(_l: &mut Link, m: Message) {
    SEEN.with(|s| s.borrow_mut().push((m.seq, m.payload.to_vec(), m.snapshot)));
}

fn record_gap(_l: &mut Link, missed: u32) {
    GAPS.with(|g| g.borrow_mut().push(missed));
}

fn record_reply(_l: &mut Link, r: Reply) {
    REPLIES.with(|v| v.borrow_mut().push((r.corr.0, r.payload.to_vec(), r.err)));
}

fn record_session(_l: &mut Link, ev: SessionEvent) {
    SESSIONS.with(|v| v.borrow_mut().push(ev));
}

/// A handler that counts its invocations and answers a fixed body, so "the
/// handler ran once" is a number rather than an inference.
fn serve(_l: &mut Link, _r: Request) -> &'static [u8] {
    SERVED.with(|n| *n.borrow_mut() += 1);
    b"pong"
}

/// A response over `MAX_DEDUP_PAYLOAD`, which is the one that is remembered but
/// not cached.
fn serve_big(_l: &mut Link, _r: Request) -> &'static [u8] {
    SERVED.with(|n| *n.borrow_mut() += 1);
    static BIG: [u8; fkipc::MAX_DEDUP_PAYLOAD + 1] = [b'B'; fkipc::MAX_DEDUP_PAYLOAD + 1];
    &BIG
}

fn answer_resync(l: &mut Link) {
    RESYNCS.with(|n| *n.borrow_mut() += 1);
    l.snapshot(2, b"whole world");
}

// ---------------------------------------------------------------------------

/// `attach` refuses a configuration it cannot act on rather than producing a
/// link that silently never speaks.
#[test]
fn attach_refuses_a_bad_config() {
    let cfg = fkipc::Config {
        port: 0,
        ..Default::default()
    };
    assert!(matches!(
        fkipc::attach(cfg, Box::new(harness::TestTransport)),
        Err(Status::BadConfig)
    ));
}

/// THE HANDSHAKE, THE TOKEN, AND THE ONE EPOCH-TEST EXEMPTION.
///
/// HELLO_ACK carries an epoch the guest does not yet know, by definition, so it
/// is matched on corr against the outstanding HELLO instead. That exemption is
/// stated in the spec precisely because two implementations would otherwise
/// disagree about it, and the disagreement would present as "the handshake
/// never completes" with both sides looking correct in isolation.
#[test]
fn the_handshake_adopts_the_peers_token_and_only_hello_ack_skips_the_epoch_test() {
    let mut h = new_harness(Opts::default());
    h.g.set_on_session(record_session);

    h.step(1);
    assert_eq!(
        h.count(Type::HELLO),
        1,
        "the first pump sent no single HELLO"
    );
    let corr = h.last_of(Type::HELLO).unwrap().0.corr;
    ack(0x51C0FFEE, corr);
    h.step(1);

    let st = h.g.stats();
    assert!(
        st.up && st.epoch == 0x51C0FFEE,
        "no token adopted: {:?}",
        st
    );
    SESSIONS.with(|v| assert_eq!(*v.borrow(), vec![SessionEvent::Up]));

    // THE OTHER HALF, which is the one a wrong implementation passes: every
    // type EXCEPT HELLO_ACK is dropped when the epoch does not match.
    h.g.set_on_message(1, record);
    let before = h.g.stats().epoch_drops;
    inject(msg(
        1,
        st.epoch ^ 0xFFFF,
        1,
        b"from a session nobody remembers",
    ));
    h.step(1);
    SEEN.with(|s| assert!(s.borrow().is_empty(), "a foreign-epoch frame was delivered"));
    assert_eq!(h.g.stats().epoch_drops, before + 1);
}

/// A SESSION BOUNDARY mid-flight fails pending requests with `SessionLost` and
/// NEVER retries them.
///
/// The boundary is a BYE, and it is a BYE rather than a `reload` BECAUSE OF THE
/// JOIN FIX: every session boundary is now a REPLICATED signal, so the test has
/// to arrive through the wire like the real thing does.
#[test]
fn a_session_boundary_fails_pending_requests_and_never_retries_them() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(3, Priority::Control);
    h.up(0x1111);
    let ep = h.g.stats().epoch;

    let corr = h.g.request(3, b"q", Some(record_reply)).expect("request");
    inject(craft(
        Header {
            ty: Type::BYE,
            epoch: ep,
            ..Default::default()
        },
        &[],
    ));
    h.step(1);

    REPLIES.with(|v| {
        let v = v.borrow();
        assert_eq!(v.len(), 1, "the BYE did not complete the pending request");
        assert_eq!(v[0].0, corr.0);
        assert_eq!(v[0].2, Some(ReplyError::SessionLost));
    });

    let before = h.count(Type::REQ);
    h.step(200); // well past the whole retry schedule
    assert_eq!(
        h.count(Type::REQ) - before,
        0,
        "a lost request is never retried"
    );
    REPLIES.with(|v| assert_eq!(v.borrow().len(), 1, "the completion ran twice"));
}

/// A LOAD IS NOT A SESSION BOUNDARY, and that is the multiplayer-join fix
/// stated as a property rather than as a doc comment.
///
/// `fk_mod.lua` arms its `fk_after_load` one-shot from `script.on_load`, and
/// Factorio runs `script.on_load` on every peer that LOADS the state --
/// including a client joining a game in progress, one tick after it joins, and
/// on no other peer. So `reload` is a peer-local signal, guest memory is
/// `storage.fk_mem` under the default `--persist=table`, and Factorio CRCs
/// that. Anything `reload` writes is a desync.
///
/// `internal/guest`'s `TestAJoiningPeerStaysByteIdenticalToTheServer` is the
/// same property one layer down, through the verbatim runtime, over real linear
/// memory, and it drives THIS crate's example guest as one of its two arms.
#[test]
fn a_load_does_not_end_the_session() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(3, Priority::Control);
    h.up(0x1212);
    h.g.set_on_session(record_session);

    let before = h.g.stats();
    h.g.request(3, b"q", Some(record_reply)).expect("request");
    h.clear_sent();
    SESSIONS.with(|v| v.borrow_mut().clear());

    h.g.reload(); // fk_after_load, on the joining client and nowhere else

    let after = h.g.stats();
    assert!(
        after.up && after.epoch == before.epoch,
        "a load moved the session: {:?} -> {:?}",
        before,
        after
    );
    assert_eq!(
        after.boot, before.boot,
        "a load moved boot; boot is the SESSION generation now and a load is \
         not a session boundary"
    );
    SESSIONS.with(|v| {
        assert!(
            v.borrow().is_empty(),
            "a load raised {:?}; nothing peer-local may reach the application \
             either",
            v.borrow()
        )
    });
    REPLIES.with(|v| {
        assert!(
            v.borrow().is_empty(),
            "a load failed a request in flight: {:?}",
            v.borrow()
        )
    });
    h.step(4);
    assert_eq!(
        h.count(Type::HELLO),
        0,
        "a HELLO went out after a load; there is no session to open"
    );
    assert_eq!(
        h.g.stats().epoch,
        before.epoch,
        "the epoch moved after a load"
    );
}

/// THE GUEST'S OWN HALF OF ROLLBACK DETECTION: a clock that has gone backwards
/// past the last frame this link accepted is a session belonging to a future
/// that no longer happened.
///
/// It is legal on a joining client for the reason nothing peer-local is: it is
/// a function of guest state and the REPLICATED tick, so every peer decides it
/// identically on the same tick. What it cannot catch -- a save taken just
/// after an inbound frame -- is the peer's, which is why `sdk/go/fkipc` watches
/// the tick every HEARTBEAT carries.
#[test]
fn the_guest_notices_its_own_clock_going_backwards() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x1313);
    let ep = h.g.stats().epoch;
    // THE PEER MUST KEEP TALKING or this measures the wrong thing: the guest
    // declares it down after LIVENESS_TICKS of silence and the rollback below
    // then has no session to end.
    for _ in 0..10 {
        let mut body = Vec::new();
        wire::control::append_heartbeat(
            &mut body,
            wire::Heartbeat {
                tick: h.tick,
                ..Default::default()
            },
        );
        inject(craft(
            Header {
                ty: Type::HEARTBEAT,
                epoch: ep,
                ..Default::default()
            },
            &body,
        ));
        h.step(30);
    }
    assert!(h.g.stats().up, "the session died before the rollback");
    h.g.set_on_session(record_session);
    SESSIONS.with(|v| v.borrow_mut().clear());
    h.clear_sent();

    // The same guest, one call later, five hundred ticks earlier: a save
    // restored under a session it still believes in.
    h.tick = 20;
    h.g.pump(h.tick);

    assert!(
        !h.g.stats().up,
        "the guest kept a session across its own rollback: {:?}",
        h.g.stats()
    );
    SESSIONS.with(|v| assert_eq!(*v.borrow(), vec![SessionEvent::Down]));
    assert!(
        h.count(Type::HELLO) >= 1,
        "the guest did not go looking for a peer again"
    );
}

/// TWO LOADS OF ONE SAVE PRODUCE THE SAME boot AND DIFFERENT TOKENS, and the
/// peer resyncs on the HELLO rather than on the epoch value.
///
/// This is the theorem the whole epoch design rests on. Everything a guest can
/// compute is a deterministic function of its own state, and its own state
/// time-travels -- so there is no deterministic function of guest state that
/// distinguishes two loads of one save, and any function that did would be a
/// desync. The uniqueness has to come from the side with entropy.
///
/// `boot` is now the SESSION generation rather than a load counter, which makes
/// the theorem sharper rather than weaker: it is not merely equal across two
/// loads of one save modulo a bump, it is literally the number the save
/// carries.
#[test]
fn two_loads_of_one_save_share_a_boot_and_get_different_tokens() {
    let mut h = new_harness(Opts::default());
    const SAVED_BOOT: u32 = 7;
    let mut boots = Vec::new();
    let mut epochs = Vec::new();

    for load in 0..2u32 {
        h.new_guest();
        h.g.restore_boot(SAVED_BOOT);
        h.clear_sent();
        h.step(1);
        let (hdr, body) = h.last_of(Type::HELLO).expect("no HELLO after the load");
        let hello = wire::control::decode_hello(&body).expect("hello payload");
        boots.push(hello.boot);
        // The HELLO's own epoch field carries boot before a token exists.
        assert_eq!(hdr.epoch, hello.boot);
        ack(0x2000 + load, hdr.corr);
        h.step(1);
        assert!(h.g.stats().up, "load {} never came up", load);
        epochs.push(h.g.stats().epoch);
    }

    assert_eq!(
        boots[0], boots[1],
        "two loads of one save produced different boots, which is the thing \
         that cannot be true: boot is a function of saved state"
    );
    assert_eq!(
        boots[0], SAVED_BOOT,
        "a load moved boot; only a SESSION boundary does that now"
    );
    assert_ne!(epochs[0], epochs[1], "the peer reused a token");
}

/// DEDUP: a retried REQ replays the cached RESP and the handler runs ONCE.
#[test]
fn a_retried_request_replays_the_cached_response_and_runs_the_handler_once() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(4, Priority::Control);
    h.g.set_on_request(4, serve);
    h.up(0x3333);
    let ep = h.g.stats().epoch;

    inject(req(4, ep, 1, 77, false, b"ping"));
    h.step(1);
    inject(req(4, ep, 2, 77, true, b"ping"));
    h.step(1);

    SERVED.with(|n| assert_eq!(*n.borrow(), 1, "the handler ran more than once"));
    let resps: Vec<_> = h
        .frames()
        .into_iter()
        .filter(|(hd, _)| hd.ty == Type::RESP)
        .collect();
    assert_eq!(resps.len(), 2, "one RESP per REQ, replayed for the retry");
    assert_eq!(resps[0].1, b"pong");
    assert_eq!(resps[1].1, b"pong");
    assert!(
        resps[1].0.flags.has(Flags::RETRY),
        "a replayed RESP is marked as a retransmission"
    );
    assert_eq!(h.g.stats().dup_hits, 1);
}

/// A response above `MAX_DEDUP_PAYLOAD` is remembered but NOT cached, so a
/// retry answers DUPLICATE instead of re-executing.
///
/// The application learns that the operation EXECUTED and the result is gone,
/// which is strictly better than the two alternatives -- silently
/// re-executing, or growing the save without bound.
#[test]
fn an_uncacheable_response_answers_duplicate_on_retry() {
    let mut h = new_harness(Opts {
        max_frame: wire::MAX_FRAME_CEILING,
        ..Default::default()
    });
    h.g.open_channel(5, Priority::Control);
    h.g.set_on_request(5, serve_big);
    h.up(0x4444);
    let ep = h.g.stats().epoch;

    inject(req(5, ep, 1, 9, false, b"ask"));
    h.step(1);
    inject(req(5, ep, 2, 9, true, b"ask"));
    h.step(2);

    SERVED.with(|n| assert_eq!(*n.borrow(), 1, "the big handler re-executed"));
    let errs: Vec<_> = h
        .frames()
        .into_iter()
        .filter(|(hd, _)| hd.ty == Type::RESP && hd.flags.has(Flags::ERROR))
        .collect();
    assert_eq!(errs.len(), 1, "no DUPLICATE went out");
    let rec = wire::control::decode_error_record(&errs[0].1).unwrap();
    assert_eq!(rec.code, wire::CODE_DUPLICATE);
}

/// A channel with no handler answers NO_HANDLER rather than silence.
#[test]
fn a_request_on_a_handlerless_channel_answers_no_handler() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(6, Priority::Control);
    h.up(0x5555);
    let ep = h.g.stats().epoch;
    inject(req(6, ep, 1, 3, false, b"?"));
    h.step(2);
    let errs: Vec<_> = h
        .frames()
        .into_iter()
        .filter(|(hd, _)| hd.ty == Type::RESP && hd.flags.has(Flags::ERROR))
        .collect();
    assert_eq!(errs.len(), 1);
    assert_eq!(
        wire::control::decode_error_record(&errs[0].1).unwrap().code,
        wire::CODE_NO_HANDLER
    );
}

/// SERIAL ARITHMETIC AT THE WRAP, BOTH ARMS.
///
/// This is the one comparison two implementations silently disagree about, and
/// a disagreement does not fail -- it delivers or drops the wrong frames
/// forever. The frames are crafted rather than sent, because getting a real
/// sender to a seq of 2^32-2 would take two thousand years at one frame per
/// tick.
#[test]
fn serial_arithmetic_at_the_wrap_in_both_arms() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(9, Priority::Control);
    h.g.set_on_message(9, record);
    h.g.set_on_gap(9, record_gap);
    h.up(0x6666);
    let ep = h.g.stats().epoch;

    let send = |h: &mut harness::Harness, seq: u32| {
        inject(msg(9, ep, seq, &[seq as u8]));
        h.step(1);
    };

    // Walk rx_last up to the boundary in strides under 2^31, because that is
    // the only way there is: a delta of 2^31 or more IS the drop arm, so a
    // receiver cannot be jumped to the far side of the space in one frame.
    for s in [0x40000000u32, 0x80000000, 0xC0000000, u32::MAX - 2] {
        send(&mut h, s);
    }
    SEEN.with(|s| s.borrow_mut().clear());
    GAPS.with(|g| g.borrow_mut().clear());
    let drops = h.g.stats().stale_drops;

    send(&mut h, u32::MAX - 1); // d = 1
    send(&mut h, u32::MAX); // d = 1
    send(&mut h, 0); // d = 1 ACROSS THE WRAP
    send(&mut h, 2); // d = 2, a gap of one
    send(&mut h, u32::MAX); // d = -3: old
    send(&mut h, 0); // d = -2: old

    SEEN.with(|s| {
        let got: Vec<u32> = s.borrow().iter().map(|(seq, _, _)| *seq).collect();
        assert_eq!(got, vec![u32::MAX - 1, u32::MAX, 0, 2]);
    });
    // The wrap itself must not read as a gap; the deliberate skip must.
    GAPS.with(|g| assert_eq!(*g.borrow(), vec![1u32]));
    assert_eq!(h.g.stats().stale_drops - drops, 2);
}

/// A gap raises the handler, sends a RESYNC, and a SNAPSHOT clears it -- and
/// the SNAPSHOT is exempt from the staleness rule, which is the exemption the
/// Go half's seeded soak earned.
///
/// Without it a receiver whose `rx_last` ever jumped forward is deaf on that
/// channel FOREVER: every later frame reads as old, so no gap is raised, so no
/// RESYNC is sent, and nothing anywhere says anything.
#[test]
fn a_gap_resyncs_and_a_snapshot_clears_it() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(2, Priority::Control);
    h.g.set_on_message(2, record);
    h.g.set_on_gap(2, record_gap);
    h.up(0x7777);
    let ep = h.g.stats().epoch;

    inject(msg(2, ep, 1, b"a"));
    h.step(1);
    // Seq 2 and 3 are lost.
    inject(msg(2, ep, 4, b"d"));
    h.step(1);

    GAPS.with(|g| assert_eq!(*g.borrow(), vec![2u32], "one gap of two"));
    assert_eq!(h.count(Type::RESYNC), 1, "exactly one RESYNC");
    // A RESYNC consumes the channel's OWN seq: one sent with seq 0 would arrive
    // at the peer as d <= 0 and be dropped by the very rule it exists to
    // escape.
    assert_ne!(h.last_of(Type::RESYNC).unwrap().0.seq, 0);

    // A second gap while the first RESYNC is outstanding must not send another.
    inject(msg(2, ep, 7, b"g"));
    h.step(1);
    assert_eq!(
        h.count(Type::RESYNC),
        1,
        "a RESYNC was sent while one was open"
    );

    // The snapshot clears it -- and it arrives with a seq BEHIND rx_last, which
    // is the exemption.
    inject(snap(2, ep, 3, b"whole world"));
    h.step(1);
    SEEN.with(|s| {
        let last = s.borrow().last().cloned().expect("no snapshot delivered");
        assert!(last.2, "the snapshot was not flagged");
        assert_eq!(last.1, b"whole world");
    });

    inject(msg(2, ep, 6, b"h"));
    h.step(1);
    assert_eq!(
        h.count(Type::RESYNC),
        2,
        "after a snapshot cleared the gap, the next one must resync again"
    );
}

/// Channel 0 is the protocol's own: it carries no seq and is exempt from gap
/// detection, because a lost heartbeat is normal and must not read as a gap in
/// application state.
#[test]
fn channel_zero_is_exempt_from_the_seq_rules() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(0, Priority::Control);
    h.g.set_on_message(0, record);
    h.g.set_on_gap(0, record_gap);
    h.up(0x8888);
    let ep = h.g.stats().epoch;

    for seq in [0u32, 0, 5, 1, 0] {
        inject(msg(0, ep, seq, b"x"));
        h.step(1);
    }
    SEEN.with(|s| assert_eq!(s.borrow().len(), 5, "channel 0 dropped a frame as stale"));
    GAPS.with(|g| assert!(g.borrow().is_empty(), "channel 0 raised a gap"));
    assert_eq!(h.g.stats().stale_drops, 0);
    assert_eq!(h.g.stats().gaps, 0);

    // ...and outbound, a frame on channel 0 carries seq 0 forever.
    h.g.send(0, b"y");
    h.step(1);
    for (hd, _) in h.frames() {
        if hd.channel == 0 && hd.ty == Type::MSG {
            assert_eq!(hd.seq, 0);
        }
    }
}

/// A fragmented MSG that loses a fragment is LOST ENTIRELY and shows up as a
/// gap, not as a short message.
#[test]
fn a_lost_fragment_is_a_gap_and_not_a_short_message() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(6, Priority::Bulk);
    h.g.set_on_message(6, record);
    h.g.set_on_gap(6, record_gap);
    h.up(0x9999);
    let ep = h.g.stats().epoch;

    let frag = |seq: u32, corr: u32, i: u8, n: u8, body: &[u8]| {
        inject(craft(
            Header {
                ty: Type::MSG,
                channel: 6,
                epoch: ep,
                seq,
                corr,
                frag: i,
                nfrag: n,
                ..Default::default()
            },
            body,
        ));
    };

    frag(1, 55, 0, 3, b"AAA");
    // seq 2, fragment 1, is lost.
    frag(3, 55, 2, 3, b"CCC");
    h.step(1);

    SEEN.with(|s| assert!(s.borrow().is_empty(), "a short message was delivered"));
    GAPS.with(|g| assert_eq!(*g.borrow(), vec![1u32], "a lost fragment must be a gap"));

    // The whole message, intact, when nothing is lost -- so the failure above
    // is about the loss and not about fragmentation being broken. A NEW corr,
    // because reusing 55 with a different nfrag is the disagreement rule rather
    // than this one: `reassembly_times_out_disagrees_and_interleaves` owns that.
    frag(4, 56, 0, 2, b"DD");
    frag(5, 56, 1, 2, b"EE");
    h.step(1);
    SEEN.with(|s| {
        let v = s.borrow();
        assert_eq!(v.len(), 1, "the intact message did not reassemble");
        assert_eq!(v[0].1, b"DDEE");
    });
}

/// The three ways a reassembly is abandoned, each with its own consequence.
#[test]
fn reassembly_times_out_disagrees_and_interleaves() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(7, Priority::Bulk);
    h.g.set_on_message(7, record);
    h.up(0xAAAA);
    let ep = h.g.stats().epoch;
    let mut seq = 0u32;
    let mut frag = |h: &mut harness::Harness, corr: u32, i: u8, n: u8, body: &[u8]| {
        seq += 1;
        inject(craft(
            Header {
                ty: Type::MSG,
                channel: 7,
                epoch: ep,
                seq,
                corr,
                frag: i,
                nfrag: n,
                ..Default::default()
            },
            body,
        ));
        h.step(1);
    };

    // (1) TIMEOUT. Half a message, then silence past REASSEMBLY_TICKS; the rest
    // arrives and must not complete it.
    frag(&mut h, 100, 0, 2, b"AAA");
    h.step(fkipc::REASSEMBLY_TICKS + 2);
    frag(&mut h, 100, 1, 2, b"BBB");
    SEEN.with(|s| {
        assert!(
            s.borrow().is_empty(),
            "a reassembly crossed its own timeout"
        )
    });

    // (2) nfrag DISAGREEMENT. Nothing here can tell which of the two messages
    // is real, so neither is.
    frag(&mut h, 200, 0, 2, b"CCC");
    frag(&mut h, 200, 1, 3, b"DDD");
    SEEN.with(|s| {
        assert!(
            s.borrow().is_empty(),
            "a reassembly survived an nfrag disagreement"
        )
    });

    // (3) INTERLEAVE. At most one reassembly is open per channel, so a new corr
    // kills the old -- which is what bounds the buffer and what imposes the
    // rule that a peer must not interleave two fragmented messages on one
    // channel.
    frag(&mut h, 300, 0, 2, b"EEE");
    frag(&mut h, 400, 0, 2, b"FFF");
    frag(&mut h, 400, 1, 2, b"GGG");
    SEEN.with(|s| {
        let v = s.borrow();
        assert_eq!(v.len(), 1, "interleave delivered {} messages", v.len());
        assert_eq!(v[0].1, b"FFFGGG");
    });
    frag(&mut h, 300, 1, 2, b"HHH");
    SEEN.with(|s| assert_eq!(s.borrow().len(), 1, "the abandoned message came back"));
}

/// Retry budget exhaustion is `Timeout`, and it takes the whole schedule to get
/// there.
///
/// THE PEER MUST BE SEEN TO BE ALIVE, or this measures the wrong thing. The
/// guest's whole retry schedule is 15+30+60+60 ticks and it declares the
/// timeout one interval after the last retry, at 225 -- which is PAST
/// `LIVENESS_TICKS` (180). So a request whose answers are all lost AND whose
/// peer says nothing else dies with `SessionLost`, correctly, and only a peer
/// that is demonstrably alive isolates the retry budget. That known and stated
/// overlap is the as-built report's own note.
#[test]
fn retry_exhaustion_times_out() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(8, Priority::Control);
    h.up(0xBBBB);
    let ep = h.g.stats().epoch;

    h.g.request(8, b"q", Some(record_reply)).expect("request");

    for i in 0..240 {
        if REPLIES.with(|v| !v.borrow().is_empty()) {
            break;
        }
        if i % 30 == 0 {
            let mut body = Vec::new();
            wire::control::append_heartbeat(
                &mut body,
                wire::Heartbeat {
                    tick: h.tick,
                    ..Default::default()
                },
            );
            inject(craft(
                Header {
                    ty: Type::HEARTBEAT,
                    epoch: ep,
                    ..Default::default()
                },
                &body,
            ));
        }
        h.step(1);
    }

    REPLIES.with(|v| {
        let v = v.borrow();
        assert_eq!(v.len(), 1, "the request never completed");
        assert_eq!(v[0].2, Some(ReplyError::Timeout));
    });
    let st = h.g.stats();
    assert_eq!(st.retries, fkipc::MAX_RETRIES as u32);
    assert_eq!(st.timeouts, 1);
    assert!(
        st.up,
        "the session died; this test is about the retry budget"
    );
}

/// THE QUIESCE: with no peer, `send` is a COUNTED NO-OP rather than an error to
/// handle at every call site, and the guest keeps looking at `SEARCH_TICKS`.
#[test]
fn with_no_peer_send_is_a_counted_no_op_and_hello_keeps_searching() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Bulk);
    h.up(0xCCCC);
    h.g.set_on_session(record_session);

    h.step(fkipc::LIVENESS_TICKS + 2);
    assert!(!h.g.stats().up, "the guest still thinks the peer is there");
    SESSIONS.with(|v| assert_eq!(*v.borrow(), vec![SessionEvent::Down]));

    let before = h.g.stats().queue_drops;
    assert_eq!(h.g.send(1, b"into the void"), Status::NoSession);
    assert_eq!(
        h.g.stats().queue_drops,
        before + 1,
        "the drop was not counted"
    );
    assert!(matches!(h.g.request(1, b"q", None), Err(Status::NoSession)));
    assert_eq!(
        h.g.stats().queue_depth,
        0,
        "a peerless guest queued something"
    );

    // ...and it keeps looking, at SEARCH_TICKS, and sends nothing else.
    h.clear_sent();
    h.step(3 * fkipc::SEARCH_TICKS);
    let frames = h.frames();
    for (hd, _) in &frames {
        assert_eq!(
            hd.ty,
            Type::HELLO,
            "a quiesced guest sent {}",
            hd.ty.as_str()
        );
    }
    assert!(
        (2..=4).contains(&frames.len()),
        "{} HELLOs over three SEARCH_TICKS windows",
        frames.len()
    );
}

/// A peer's BYE takes the session down immediately.
#[test]
fn a_bye_takes_the_session_down() {
    let mut h = new_harness(Opts::default());
    h.up(0xDDDD);
    h.g.set_on_session(record_session);
    let ep = h.g.stats().epoch;
    inject(craft(
        Header {
            ty: Type::BYE,
            epoch: ep,
            ..Default::default()
        },
        &[],
    ));
    h.step(1);
    assert!(!h.g.stats().up);
    SESSIONS.with(|v| assert_eq!(*v.borrow(), vec![SessionEvent::Down]));
}

/// BELOW THE ENGINE FLOOR THE LINK IS INERT: not one datagram of any kind,
/// every API call refused deterministically, and one line in the log saying
/// why.
///
/// PUMPING IS FATAL WHERE IT IS NOT USELESS: on 2.0.77 a headless server
/// calling `recv_udp` with a packet queued aborts in C++ at
/// `TickClosure.cpp:91`, which no pcall can catch. This ran SEND-ONLY down
/// there until 2026-08-07, on the reasoning that outbound is free -- true of
/// the datagrams and false of the protocol, because a session is established by
/// an ACK and an ACK arrives INBOUND. So a send-only link HELLOed once a second
/// forever, never came up, refused every send for want of a session, and told
/// its author nothing beyond a counter.
///
/// The `from_guest` assertion is the one that matters: ZERO frames, not "only
/// HELLOs".
#[test]
fn below_the_engine_floor_the_link_is_inert() {
    let mut h = new_harness(Opts {
        base_version: Some(Version::new(2, 0, 77)),
        ..Default::default()
    });
    h.g.open_channel(1, Priority::Bulk);
    // A packet the guest must never look at.
    inject(msg(1, 1, 1, b"do not read me"));
    h.step(20);

    let st = h.g.stats();
    assert!(!st.enabled, "the gate opened below the floor");
    assert!(!h.g.enabled());
    bus(|b| {
        assert!(
            b.from_guest.is_empty(),
            "a disabled link put {} frame(s) on the wire; it must put none",
            b.from_guest.len()
        );
        assert_eq!(b.to_guest.len(), 1, "the queued packet was consumed");
    });
    assert_eq!(st.tx_frames, 0);
    assert_eq!(st.rx_frames, 0);
    assert!(
        !st.up,
        "a session cannot come up on a link that never HELLOs"
    );
    assert_eq!(st.base_version, Version::new(2, 0, 77));

    // EVERY OUTBOUND CALL ANSWERS THE SAME DETERMINISTIC REFUSAL, which is the
    // half a counter cannot give an author: Disabled at the call site says
    // "this engine", where NoSession would have said "your companion is down"
    // about a companion that is running fine.
    let before = h.g.stats().refusals;
    assert_eq!(h.g.send(1, b"into the void"), Status::Disabled);
    assert_eq!(h.g.snapshot(1, b"state"), Status::Disabled);
    assert_eq!(h.g.request(1, b"q", None), Err(Status::Disabled));
    assert_eq!(h.g.write_bulk(1, "bulk.bin", b"x"), Status::Disabled);
    assert_eq!(h.g.notify_file(1, "shot.png"), Status::Disabled);
    assert_eq!(
        h.g.stats().refusals - before,
        5,
        "five refused calls, five counted refusals"
    );
    // write_bulk refuses BEFORE the write, so nothing lands on disk either: the
    // notify that announces the file can never be sent, and a file the peer
    // will never hear about is worse than no file.
    bus(|b| {
        assert!(b.files.is_empty(), "a disabled link wrote {:?}", b.files);
        assert!(b.from_guest.is_empty(), "a refused call reached the wire");
    });

    // AND IT SAYS SO, ONCE. The game log is not CRC'd and is per-peer by
    // nature, which is what makes it the only sanctioned sink for this -- and
    // why the line is the one thing here a mod author actually reads. It is
    // BYTE-IDENTICAL to the Go half's -- the literal below and the one in
    // sdk/go/fkipc's TestBelowTheEngineFloorTheLinkIsInert are the same
    // sentence, because a mod author reading a log should not be able to tell
    // which language the guest was written in.
    bus(|b| {
        assert_eq!(
            b.logs,
            vec!["fkipc: disabled -- requires Factorio >= 2.1.14; this engine is 2.0.77"],
            "the gate's log line"
        );
    });

    // And at the floor it opens. Same harness shape, one constant different, so
    // the assertion above is about the gate and not about the wiring.
    let mut h2 = new_harness(Opts::default());
    h2.up(0xEEEE);
    assert!(h2.g.stats().enabled);
    assert!(h2.count(Type::HELLO) > 0);
    bus(|b| {
        assert!(
            b.logs.is_empty(),
            "an enabled link logged {:?}; the line is for the refusal only",
            b.logs
        )
    });
}

/// A SAVE MOVED ONTO A NEWER ENGINE COMES UP BY ITSELF, and this is the arm the
/// engine gate's re-read exists for.
///
/// An engine cannot change under a running game, so within one session the
/// gate's answer is fixed and re-reading it would be waste. A SAVE is the other
/// case and is an ordinary thing for a player to do: a map made on 2.0.77 and
/// then loaded on 2.1.14 carries guest state that says "disabled" into a game
/// where the library works. Under `--persist=table` that state IS
/// `storage.fk_mem` and comes back from the save, so it is not enough for
/// `open` to have asked -- `service_gate` re-asks on the REPLICATED tick, at
/// `SEARCH_TICKS`, and only while the gate is shut.
///
/// Modelled by changing what the transport reports under a LIVE link rather
/// than by rebuilding one, which is the honest shape: what a load does is carry
/// guest state across into a different engine, and this is that link meeting
/// that engine.
#[test]
fn a_save_moved_onto_a_newer_engine_comes_up_by_itself() {
    let mut h = new_harness(Opts {
        base_version: Some(Version::new(2, 0, 77)),
        ..Default::default()
    });
    h.step(20);
    assert!(!h.g.stats().enabled);
    assert_eq!(h.sent(), 0, "the link was not inert to begin with");

    // The load. Same link, same guest state, new engine underneath it.
    bus(|b| b.ver = Some(MIN_ENGINE_VERSION));

    // Up to a SEARCH_TICKS boundary, which is the whole worst case: the gate is
    // polled once a second of game time and not once a tick, because below the
    // floor a host call per tick would be the one cost this mode is meant not
    // to have.
    h.step(2 * fkipc::SEARCH_TICKS);
    assert!(
        h.g.stats().enabled,
        "the gate stayed shut over two SEARCH_TICKS windows"
    );
    assert!(h.count(Type::HELLO) > 0, "no HELLO after the gate opened");
    // Both lines, in order: the refusal from the old engine and its withdrawal.
    // The second matters more than it looks -- a reader who saw the first one
    // deserves to see it taken back rather than inferring it from traffic.
    bus(|b| {
        assert_eq!(b.logs.len(), 2, "the gate logged {:?}", b.logs);
        assert!(b.logs[0].contains("disabled"));
        assert!(b.logs[1].contains("enabled -- this engine is 2.1.14"));
    });

    // ...and it is never re-read once open. The gate is MONOTONE within a
    // session -- Factorio refuses a save written by a newer build, so a
    // restored "the link may run" can only have come from an engine at or below
    // this one -- so a host call per second here would buy nothing at all.
    bus(|b| b.ver = Some(Version::new(2, 0, 77)));
    h.step(2 * fkipc::SEARCH_TICKS);
    assert!(
        h.g.stats().enabled,
        "the gate shut again; it must never re-read once open"
    );
    bus(|b| assert_eq!(b.logs.len(), 2, "the gate logged again: {:?}", b.logs));
}

/// A version read that FAILS is treated as below the floor. Refusing to receive
/// costs a session; receiving when we should not costs the process.
#[test]
fn a_failed_version_read_closes_the_gate() {
    let mut h = new_harness(Opts {
        base_version: None,
        ..Default::default()
    });
    h.step(3);
    let st = h.g.stats();
    assert!(!st.enabled);
    assert_eq!(st.base_version, Version::ZERO);
    // "unreadable" rather than "0.0.0": a failed read is not a version, and
    // saying 0.0.0 would send somebody looking for a Factorio that never was.
    bus(|b| {
        assert_eq!(
            b.logs,
            vec!["fkipc: disabled -- requires Factorio >= 2.1.14; this engine is unreadable"]
        )
    });
}

/// A guest whose peer speaks a protocol version it does not is deaf but not
/// broken, and the frames are counted as bad rather than as an epoch mismatch.
#[test]
fn a_future_protocol_version_is_dropped_and_counted() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.g.set_on_message(1, record);
    h.up(0x1234);
    let ep = h.g.stats().epoch;

    let mut future = msg(1, ep, 1, b"hi");
    future[2] = wire::VERSION + 1;
    inject(future);
    // ...and a truncated frame, which is what the length field is for.
    let good = msg(1, ep, 1, b"hello");
    inject(good[..good.len() - 2].to_vec());
    h.step(1);

    SEEN.with(|s| assert!(s.borrow().is_empty(), "an undecodable frame was delivered"));
    assert_eq!(h.g.stats().bad_frames, 2);
}

/// An inbound FILE_NOTIFY aimed at the GUEST is dropped and counted.
///
/// A guest cannot read files -- there is no file-read API -- so a notify aimed
/// at one is meaningless and is counted rather than delivered to a handler that
/// could do nothing with it.
#[test]
fn an_inbound_file_notify_is_dropped_and_counted() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x2345);
    let ep = h.g.stats().epoch;
    let before = h.g.stats().bad_frames;
    let mut body = Vec::new();
    wire::control::append_file_notify(
        &mut body,
        &wire::FileNotify {
            bytes: 1,
            fnv1a32: 2,
            name: "x".into(),
        },
    )
    .unwrap();
    inject(craft(
        Header {
            ty: Type::FILE_NOTIFY,
            channel: 1,
            epoch: ep,
            seq: 1,
            ..Default::default()
        },
        &body,
    ));
    h.step(1);
    assert_eq!(h.g.stats().bad_frames, before + 1);
}

/// A message over the negotiated ceiling is refused by the SENDER, because the
/// transport will not report it: an oversized `send_udp` is accepted, raises
/// nothing, and never arrives.
#[test]
fn a_message_over_the_ceiling_is_refused_by_the_sender() {
    let mut h = new_harness(Opts {
        max_frame: 128,
        ..Default::default()
    });
    h.g.open_channel(1, Priority::Bulk);
    h.up(0xFFFF);
    // The peer's HELLO_ACK advertises DEFAULT_MAX_FRAME, and the SENDER obeys
    // the PEER's number -- so the ceiling here is the peer's, which is the
    // asymmetry worth pinning.
    let room = wire::DEFAULT_MAX_FRAME as usize - wire::HEADER_BYTES;
    assert_eq!(
        h.g.send(1, &vec![b'x'; room * wire::MAX_FRAGMENTS as usize]),
        Status::Ok,
        "a message at exactly the ceiling was refused"
    );
    assert_eq!(
        h.g.send(1, &vec![b'x'; room * wire::MAX_FRAGMENTS as usize + 1]),
        Status::TooLarge
    );
}

/// `write_bulk` writes the file and notifies with a digest the peer can verify
/// exactly; `notify_file` announces a file the guest never held and carries
/// none.
#[test]
fn write_bulk_digests_and_notify_file_does_not() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Bulk);
    h.up(0x0101);

    let body = b"payload".repeat(50);
    assert_eq!(h.g.write_bulk(1, "fkipc/dump.bin", &body), Status::Ok);
    assert_eq!(h.g.notify_file(1, "shot.png"), Status::Ok);
    h.step(2);

    bus(|b| {
        assert_eq!(
            b.files.get("fkipc/dump.bin").map(|v| v.as_slice()),
            Some(body.as_slice())
        )
    });

    let notifies: Vec<_> = h
        .frames()
        .into_iter()
        .filter(|(hd, _)| hd.ty == Type::FILE_NOTIFY)
        .collect();
    assert_eq!(notifies.len(), 2);
    let digested = &notifies[0];
    assert!(digested.0.flags.has(Flags::HAS_DIGEST));
    let fnote = wire::control::decode_file_notify(&digested.1).unwrap();
    assert_eq!(fnote.bytes as usize, body.len());
    assert_eq!(fnote.fnv1a32, wire::fnv1a32(&body));
    let bare = &notifies[1];
    assert!(!bare.0.flags.has(Flags::HAS_DIGEST));
    assert_eq!(
        wire::control::decode_file_notify(&bare.1).unwrap().name,
        "shot.png"
    );
}

/// DRAIN-ON-RESUME: a backlog piled up while the pump was stopped arrives in
/// ONE tick, in order, complete.
///
/// That is the measured shape -- twenty packets blasted in 0.34 ms all arrived
/// within the tick -- and it is why `DRAIN_MAX` is 1 rather than a loop bound.
/// The pause is the hazard the client profile carries: ticks stop, the pump
/// stops, the OS buffer fills, and the loss is silent.
#[test]
fn a_backlog_from_a_pause_drains_in_one_tick_in_order() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Bulk);
    h.g.set_on_message(1, record);
    h.up(0x0202);
    let ep = h.g.stats().epoch;

    // The game is paused: no pump for twenty packets' worth of peer.
    for i in 1..=20u32 {
        inject(msg(1, ep, i, &[i as u8]));
    }
    let before = h.tick;
    h.step(1);
    assert_eq!(h.tick, before + 1, "more than one tick was needed");

    SEEN.with(|s| {
        let v = s.borrow();
        assert_eq!(v.len(), 20, "the backlog did not drain in one tick");
        for (i, (seq, body, _)) in v.iter().enumerate() {
            assert_eq!(*seq, i as u32 + 1, "out of order");
            assert_eq!(body, &vec![i as u8 + 1]);
        }
    });
    assert_eq!(h.g.stats().gaps, 0, "an in-order backlog raised a gap");
}

/// A guest's own resync answer goes out through the `&mut Link` its handler was
/// handed, which is the Rust half of "OnResync is 'send me a snapshot'".
#[test]
fn a_resync_is_answered_with_a_snapshot_from_the_handlers_own_link() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(2, Priority::Bulk);
    h.g.set_on_resync(2, answer_resync);
    h.up(0x0303);
    let ep = h.g.stats().epoch;

    inject(craft(
        Header {
            ty: Type::RESYNC,
            channel: 2,
            epoch: ep,
            seq: 1,
            ..Default::default()
        },
        &[],
    ));
    h.step(2);

    RESYNCS.with(|n| assert_eq!(*n.borrow(), 1));
    let snaps: Vec<_> = h
        .frames()
        .into_iter()
        .filter(|(hd, _)| hd.ty == Type::MSG && hd.flags.has(Flags::SNAPSHOT))
        .collect();
    assert_eq!(
        snaps.len(),
        1,
        "the handler's snapshot never reached the wire"
    );
    assert_eq!(snaps[0].1, b"whole world");
}

/// The reply of a request whose channel was never registered is refused rather
/// than crediting some other channel's pending slot.
#[test]
fn a_response_on_an_unregistered_channel_is_dropped() {
    let mut h = new_harness(Opts::default());
    h.up(0x0404);
    let ep = h.g.stats().epoch;
    let before = h.g.stats().bad_frames;
    inject(resp(77, ep, 1, 5, b"nope"));
    h.step(1);
    assert_eq!(h.g.stats().bad_frames, before + 1);
}

/// A RESP with no matching pending request is stale, not bad -- it is what a
/// retry that crossed its own answer looks like.
#[test]
fn an_unmatched_response_is_stale_not_bad() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x0505);
    let ep = h.g.stats().epoch;
    let bad = h.g.stats().bad_frames;
    let stale = h.g.stats().stale_drops;
    inject(resp(1, ep, 1, 999, b"late"));
    h.step(1);
    assert_eq!(h.g.stats().bad_frames, bad);
    assert_eq!(h.g.stats().stale_drops, stale + 1);
}

/// A peer error record reaches the reply as a code plus the message bytes.
#[test]
fn a_peer_error_reaches_the_reply() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x0606);
    let ep = h.g.stats().epoch;
    let corr = h.g.request(1, b"q", Some(record_reply)).expect("request");

    let mut body = Vec::new();
    wire::control::append_error_record(
        &mut body,
        &wire::ErrorRecord {
            code: wire::CODE_DUPLICATE,
            message: "result was not cached".into(),
        },
    )
    .unwrap();
    inject(craft(
        Header {
            ty: Type::RESP,
            flags: Flags::ERROR,
            channel: 1,
            epoch: ep,
            seq: 1,
            corr: corr.0,
            ..Default::default()
        },
        &body,
    ));
    h.step(1);

    REPLIES.with(|v| {
        let v = v.borrow();
        assert_eq!(v.len(), 1);
        match v[0].2 {
            Some(ReplyError::Peer(pe)) => {
                assert!(pe.duplicate(), "code {}", pe.code);
            }
            other => panic!("reply error {:?}", other),
        }
        assert_eq!(v[0].1, b"result was not cached");
    });
}

/// The client profile changes both `for_player` defaults and the retry
/// interval, and the two are not mirrors of each other.
#[test]
fn the_client_profile_retries_faster() {
    let mut h = new_harness(Opts {
        profile: Profile::Client,
        ..Default::default()
    });
    h.g.open_channel(1, Priority::Control);
    h.up(0x0707);
    h.g.request(1, b"q", Some(record_reply)).expect("request");
    // One pump to FLUSH the original, so what follows counts retransmissions
    // and not the request itself: `request` queues, `pump` sends.
    h.step(1);
    let before = h.count(Type::REQ);
    h.step(fkipc::RETRY_TICKS_CLIENT + 1);
    assert_eq!(
        h.count(Type::REQ) - before,
        1,
        "a client-profile request did not retry at RETRY_TICKS_CLIENT"
    );

    // The server profile does NOT, in the same window, which is what makes the
    // assertion above about the profile rather than about retries existing.
    let mut s = new_harness(Opts::default());
    s.g.open_channel(1, Priority::Control);
    s.up(0x0707);
    s.g.request(1, b"q", None).expect("request");
    s.step(1);
    let before = s.count(Type::REQ);
    s.step(fkipc::RETRY_TICKS_CLIENT + 1);
    assert_eq!(
        s.count(Type::REQ) - before,
        0,
        "a server-profile request retried inside the client window"
    );
}

/// The pending table is bounded, because a retried request keeps its whole
/// message and at the message ceiling that is ~62 KB apiece -- an unbounded
/// pending table is an unbounded save.
#[test]
fn the_pending_table_is_bounded() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x0808);
    for i in 0..fkipc::MAX_PENDING {
        assert!(
            h.g.request(1, b"q", None).is_ok(),
            "request {} was refused early",
            i
        );
    }
    assert!(matches!(
        h.g.request(1, b"q", None),
        Err(Status::TooManyPending)
    ));
}

/// A heartbeat goes out once per window WHETHER OR NOT the guest has been
/// talking, and the busy arm is the one that matters.
///
/// It used to be suppressed by any other frame, on the grounds that any frame
/// is a liveness signal -- true, and not the whole job. The heartbeat is the
/// only frame carrying the guest's TICK, and the peer is the side with a real
/// clock and therefore the only side that can see the guest's clock go
/// backwards after a save is restored under a live session
/// (`sdk/go/fkipc`'s `RollbackTicks`). A telemetry-heavy guest heartbeating
/// never left that reading frozen at the HELLO for the whole session, along
/// with the rx/drops/gaps counters that are the flow-control signal.
#[test]
fn a_heartbeat_goes_out_every_window_even_when_the_guest_is_busy() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Bulk);
    h.up(0x0909);
    let ep = h.g.stats().epoch;
    h.clear_sent();

    // THE PEER MUST KEEP TALKING or this measures the wrong thing: the guest
    // declares it down after LIVENESS_TICKS of silence and then heartbeats
    // stop for a correct reason that has nothing to do with the window.
    let alive = |h: &mut harness::Harness| {
        let mut body = Vec::new();
        wire::control::append_heartbeat(
            &mut body,
            wire::Heartbeat {
                tick: h.tick,
                ..Default::default()
            },
        );
        inject(craft(
            Header {
                ty: Type::HEARTBEAT,
                epoch: ep,
                ..Default::default()
            },
            &body,
        ));
    };

    // Talking every tick makes no difference: this is the shape that used to
    // produce zero heartbeats for the life of the session.
    for _ in 0..(fkipc::HEARTBEAT_TICKS * 2) {
        alive(&mut h);
        h.g.send(1, b"chatter");
        h.step(1);
    }
    let busy = h.count(Type::HEARTBEAT);
    assert!(
        (2..=3).contains(&busy),
        "{} heartbeats over two windows from a guest sending every tick",
        busy
    );

    // ...and silence produces the same one per window.
    h.clear_sent();
    for _ in 0..(fkipc::HEARTBEAT_TICKS * 2 + 2) {
        alive(&mut h);
        h.step(1);
    }
    assert!(h.g.stats().up, "the session died, so this measured nothing");
    let n = h.count(Type::HEARTBEAT);
    assert!((2..=3).contains(&n), "{} heartbeats over two windows", n);
    let (_, body) = h.last_of(Type::HEARTBEAT).unwrap();
    wire::control::decode_heartbeat(&body).expect("a malformed heartbeat payload");
}

/// A `Corr` is minted from a counter and never from randomness -- determinism,
/// and it also makes the dedup window's arithmetic trivial.
#[test]
fn correlation_ids_come_from_a_counter() {
    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Control);
    h.up(0x0A0A);
    let a = h.g.request(1, b"1", None).unwrap();
    let b = h.g.request(1, b"2", None).unwrap();
    assert_eq!(b.0, a.0 + 1, "corr {:?} then {:?}", a, b);
    assert_ne!(a, Corr(0), "0 means 'no correlation'");
}

/// A LIMITATION, PINNED SO IT IS A DECISION RATHER THAN A SURPRISE: during the
/// poll the link holds no transport, so `write_bulk` and `notify_file` called
/// from inside an inbound handler answer `NoTransport`.
///
/// The Go half does not have this -- its handlers reach the package link, which
/// still owns the transport. Here the transport is out for the duration of the
/// poll precisely so the poll can re-enter the guest soundly (see
/// `Link::pump_begin`), and a file write from inside an event dispatch would be
/// a host call nested inside a host call anyway. The remedy is one line in the
/// guest: set a flag in the handler and write from `fk_on_tick`.
#[test]
fn write_bulk_from_inside_a_handler_is_refused_during_the_poll() {
    fn try_write(l: &mut Link, _m: Message) {
        NESTED_WRITE.with(|c| c.set(Some(l.write_bulk(1, "from-a-handler.bin", b"x"))));
    }

    let mut h = new_harness(Opts::default());
    h.g.open_channel(1, Priority::Bulk);
    h.g.set_on_message(1, try_write);
    h.up(0x0B0B);
    let ep = h.g.stats().epoch;
    inject(msg(1, ep, 1, b"go on then"));
    h.step(1);

    assert_eq!(
        NESTED_WRITE.with(|c| c.get()),
        Some(Status::NoTransport),
        "the limitation moved -- if write_bulk now works from inside a handler \
         that is an improvement, but it is one this comment and pump_begin's \
         both describe as impossible"
    );
    bus(|b| {
        assert!(
            !b.files.contains_key("from-a-handler.bin"),
            "the refused write reached the transport anyway"
        )
    });

    // ...and the same call from OUTSIDE a dispatch works, so the refusal is
    // about the window and not about write_bulk.
    assert_eq!(h.g.write_bulk(1, "from-a-tick.bin", b"x"), Status::Ok);
}

/// TWO IPC MODS IN ONE GAME SHARE ONE SOCKET, and this is the property that
/// makes that safe. `--enable-lua-udp` binds a single socket for the whole
/// game, so `on_udp_packet_received` fires in EVERY mod for EVERY mod's
/// datagrams, and the only thing in the event that distinguishes them is the
/// sender's port.
///
/// The frame injected here is not junk: it is a well-formed HELLO_ACK carrying
/// the corr of the HELLO this guest just sent. HELLO_ACK is the ONE frame
/// matched on corr with the epoch test skipped -- it must be, because it
/// carries an epoch the guest cannot yet know -- and corr is minted from a
/// counter, so a second freshly-loaded guest's first HELLO carries the same
/// `corr = 1`. Without the source-port test this link adopts the OTHER mod's
/// session token and then talks to its own companion under an epoch that
/// companion has never heard of.
///
/// The mirror of Go's `TestAFrameFromAnotherModsCompanionIsRefused`.
#[test]
fn a_frame_from_another_mods_companion_is_refused() {
    const OTHER_PEER: u16 = 29437;

    let mut h = new_harness(Opts::default());
    h.step(1); // sends the HELLO
    let corr = h
        .last_of(Type::HELLO)
        .expect("the first pump sent no HELLO")
        .0
        .corr;

    // The other mod's companion answers first, on the same corr.
    bus(|b| b.src_port = OTHER_PEER);
    ack(0xC0FF_EE01, corr);
    h.step(1);

    assert!(
        !h.g.stats().up,
        "the link adopted a session from a port that is not its peer's -- this \
         is the corr collision two IPC mods in one game produce, and the epoch \
         filter cannot catch it because HELLO_ACK is the one frame exempt from \
         the epoch test"
    );
    assert_eq!(
        h.g.stats().foreign_drops,
        1,
        "the refusal has to be countable or a misconfigured port is \
         indistinguishable from a dead companion"
    );
    assert_eq!(
        h.g.stats().rx_bytes,
        0,
        "a foreign datagram was charged to this session's own byte accounting"
    );

    // ...and the real companion, on the same corr, still works.
    bus(|b| b.src_port = harness::GUEST_PEER_PORT);
    ack(0xC0FF_EE02, corr);
    h.step(1);
    assert!(h.g.stats().up, "the configured peer's ACK was not adopted");
    assert_eq!(h.g.stats().epoch, 0xC0FF_EE02);

    // A BYE AT THE LIVE EPOCH FROM THE WRONG PORT, which is the exact frame
    // `scripts/run-ipcdemo.sh`'s foreign-port leg puts on the real wire. It is
    // the loudest thing a stray sender can do -- one datagram ends the session
    // -- and it passes the epoch test by construction, so the source port is
    // the ONLY thing standing between another mod's companion and this mod's
    // session. The positive control is the line below it.
    let bye = craft(
        Header {
            ty: Type::BYE,
            epoch: h.g.stats().epoch,
            ..Default::default()
        },
        &[],
    );
    bus(|b| b.src_port = OTHER_PEER);
    inject(bye.clone());
    h.step(1);
    assert!(
        h.g.stats().up,
        "a BYE from another mod's companion ended the session"
    );

    bus(|b| b.src_port = harness::GUEST_PEER_PORT);
    inject(bye);
    h.step(1);
    assert!(
        !h.g.stats().up,
        "the SAME BYE from the configured port did NOT end the session, so the \
         assertion above passes for the wrong reason"
    );
}

/// A source port of ZERO is accepted, deliberately: zero is not a valid UDP
/// source port, so it means "the engine did not say", and refusing on silence
/// would make a guest deaf on any build that stops reporting the field.
/// Deafness is silent and total; cross-talk is loud and recoverable.
#[test]
fn an_unreported_source_port_is_accepted() {
    let mut h = new_harness(Opts::default());
    h.step(1);
    let corr = h.last_of(Type::HELLO).expect("no HELLO").0.corr;
    bus(|b| b.src_port = 0);
    ack(0xABCD_1234, corr);
    h.step(1);
    assert!(
        h.g.stats().up,
        "a datagram whose source port the engine did not report was refused; a \
         build that stops filling the field must not make every IPC guest deaf"
    );
}

/// WHETHER AN OUTBOUND FRAME ACTUALLY WENT IS INVISIBLE TO GUEST STATE, and
/// this is the second half of the multiplayer-join fix.
///
/// `send_udp` works only if the peer running the guest was started with
/// `--enable-lua-udp`. In this project's own topology a headless server has it
/// and the graphical client joining that server does NOT -- and both peers run
/// the same guest, in lockstep, over one CRC'd copy of guest memory. So a link
/// that counted `tx_frames` on success and `queue_drops` on failure wrote a
/// different word on each peer, every frame, and the client desynced on the
/// first tick it simulated. Measured on 2.1.14 with no companion running at
/// all, which is what says it is the SEND and not anything inbound.
///
/// The two runs here differ in nothing but whether the transport works. Every
/// number either of them can ever show a peer has to match.
#[test]
fn a_failed_send_is_invisible_to_guest_state() {
    fn drive(fail: bool) -> fkipc::Stats {
        let mut h = new_harness(Opts::default());
        bus(|b| b.send_fails = fail);
        h.g.open_channel(1, Priority::Bulk);
        h.g.open_channel(2, Priority::Control);

        // Searching, with nobody listening: a HELLO every SEARCH_TICKS.
        h.step(3);
        // ...then a session, which arrives INBOUND and therefore identically on
        // every peer whatever the send did. corr = 1 is what a link at this
        // point must have minted, which is the only way to ACK the arm whose
        // HELLO never reached the wire.
        ack(0xC0FFEE01, 1);
        h.step(1);
        assert!(
            h.g.stats().up,
            "fail={}: the session never came up, so this compares nothing",
            fail
        );

        // Everything outbound, including the one that used to return early on a
        // failed write and skip the notify -- which would have desynced the
        // channel's seq as well as the counters.
        h.g.send(1, b"telemetry");
        let _ = h.g.request(2, b"q", None);
        h.g.write_bulk(1, "bulk.bin", &[b'x'; 300]);
        h.g.notify_file(1, "shot.png");
        h.step(200);
        h.g.stats()
    }

    let ok = drive(false);
    let broken = drive(true);
    assert_eq!(
        ok, broken,
        "a link whose sends FAILED holds different state than one whose sends \
         worked, and both are the same guest on two peers of one lockstep game"
    );
    assert!(
        ok.tx_frames > 0,
        "neither link sent anything, so the comparison is vacuous"
    );
}

/// THE NAME IS THE SCHEMA FILTER, and it is the only one of the four mechanisms
/// that can refuse a peer whose TRANSPORT is entirely correct.
///
/// The frame injected here comes from the configured port, carries the corr of
/// the HELLO this guest just sent, and decodes cleanly -- it passes the mod
/// filter, it passes the one epoch-test exemption, and every layer below this
/// one is satisfied. What is wrong with it is the only thing left: it is a
/// different application. That is a swapped port config or a companion left
/// running from last week, and without this check it is a session that comes up
/// and then disagrees with itself about what channel 1 means.
///
/// The mirror of Go's `TestAHelloAckFromTheWrongApplicationIsNotAdopted`.
#[test]
fn a_hello_ack_from_the_wrong_application_is_not_adopted() {
    // THE CONTROL FIRST, because it is what makes the refusal below a fact
    // about the TOKEN rather than about the frame: the identical ACK, at a link
    // that states no expectation, is adopted.
    let mut loose = new_harness(Opts {
        name: "app/1",
        ..Default::default()
    });
    loose.step(1);
    let corr = loose.last_of(Type::HELLO).expect("no HELLO").0.corr;
    ack_named(0xAAAA_0001, corr, "somebody-else/9");
    loose.step(1);
    assert!(
        loose.g.stats().up,
        "a link with no expect_peer refused an ACK: the check is running when \
         nobody asked for it, and every guest written before it existed would \
         stop connecting"
    );

    let mut h = new_harness(Opts {
        name: "app/1",
        expect_peer: "app/1",
        ..Default::default()
    });
    h.step(1);
    let corr = h.last_of(Type::HELLO).expect("no HELLO").0.corr;

    ack_named(0xBBBB_0001, corr, "somebody-else/9");
    h.step(1);
    assert!(
        !h.g.stats().up,
        "the link adopted a token from an application it was not built against; \
         every layer below the name agreed, which is exactly why the name has \
         to be checked"
    );
    assert_eq!(h.g.stats().epoch, 0, "an epoch after a refused ACK");
    assert_eq!(
        h.g.stats().name_rejects,
        1,
        "the refusal has to be countable or a mismatched companion is \
         indistinguishable from a dead one"
    );
    assert_eq!(
        h.g.stats().drops,
        1,
        "a refused frame is still a refused frame"
    );

    // THE RETRY CONTINUATION, and it is the half a wrong implementation breaks.
    // The rejected ACK must not CONSUME the outstanding HELLO: a companion that
    // restarts with the right identity while that HELLO is still in flight
    // answers the SAME corr, and clearing `hello_corr` on the reject would
    // leave this guest deaf to it until the next search.
    ack_named(0xBBBB_0002, corr, "app/1");
    h.step(1);
    assert!(
        h.g.stats().up && h.g.stats().epoch == 0xBBBB_0002,
        "a CORRECT ACK on the same outstanding HELLO was not adopted after a \
         rejected one: {:?}. The reject consumed the HELLO's retry state",
        h.g.stats()
    );
    assert_eq!(
        h.g.stats().name_rejects,
        1,
        "name_rejects moved on an ACCEPTED ack"
    );
}

/// A rejected ACK does not accelerate the search, and this is the arm that
/// keeps the refusal from being worse than the thing it refuses.
///
/// A mismatched companion answers EVERY hello, so "reject, then re-HELLO at
/// once" is one frame per tick in each direction for as long as the
/// misconfiguration lasts -- the livelock shape the source-port filter was
/// built to end, met from a new direction. The cadence must stay
/// `SEARCH_TICKS`, and the guest must still recover when the right companion
/// appears, which is the SECOND half of the retry continuation: a fresh HELLO's
/// corr is adopted too.
///
/// The mirror of Go's `TestARejectedHelloAckDoesNotChangeTheSearchCadence`.
#[test]
fn a_rejected_hello_ack_does_not_change_the_search_cadence() {
    let mut h = new_harness(Opts {
        name: "app/1",
        expect_peer: "app/1",
        ..Default::default()
    });
    h.step(1);
    let first = h.last_of(Type::HELLO).expect("no HELLO").0.corr;
    ack_named(1, first, "wrong/1");
    h.step(1);
    assert_eq!(
        h.sent(),
        1,
        "a reject that re-HELLOs immediately is a frame per tick against a \
         companion that answers every one"
    );

    // Nothing until the search timer, then exactly one more.
    h.step(fkipc::SEARCH_TICKS - 2);
    assert_eq!(h.sent(), 1, "a frame went out before SEARCH_TICKS elapsed");
    h.step(1);
    assert_eq!(
        h.sent(),
        2,
        "the link stopped searching after a reject, which is deafness rather \
         than refusal"
    );
    let second = h.last_of(Type::HELLO).expect("no second HELLO").0.corr;
    assert_ne!(second, first, "a fresh search mints a fresh corr");

    // And the recovery half: the right companion turns up and is adopted on the
    // NEW corr.
    ack_named(0xC0DE, second, "app/1");
    h.step(1);
    assert!(
        h.g.stats().up && h.g.stats().epoch == 0xC0DE,
        "the correct companion's ACK was not adopted on a fresh HELLO's corr: {:?}",
        h.g.stats()
    );
    assert_eq!(h.g.stats().name_rejects, 1);
}

/// ONE TOKEN NAMES THE CONTRACT, so a guest that says what it requires has by
/// that act said what it is. A guest setting only `expect_peer` would otherwise
/// send an empty `name` and be refused by the very companion it just described.
///
/// The mirror of Go's
/// `TestAGuestThatStatesOnlyWhatItExpectsAlsoStatesWhatItIs`.
#[test]
fn a_guest_that_states_only_what_it_expects_also_states_what_it_is() {
    let mut h = new_harness(Opts {
        expect_peer: "app/7",
        ..Default::default()
    });
    h.step(1);
    let (_, body) = h.last_of(Type::HELLO).expect("no HELLO");
    let hello = wire::control::decode_hello(&body).expect("decode hello");
    assert_eq!(
        hello.name, "app/7",
        "the HELLO's name is not the expected token"
    );
}
