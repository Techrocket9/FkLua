//! The fkipc wiring fixture: the four exports a guest author writes to get an
//! IPC link, and nothing else.
//!
//! A line-for-line mirror of `guest/go/examples/ipc`, so the SAME host stub
//! drives both and their frames are compared byte for byte --
//! `TestBothGuestLibrariesSpeakTheSameWire`. That is the whole instrument for
//! keeping the two backends level here: nothing generates these two libraries,
//! so there is no census row to diff, and "the Rust generator was four
//! milestones behind" plus AD5 are what happens when two renderings of one
//! design are only checked one at a time.
//!
//! What it proves that the host-side suite cannot: that the crate COMPILES for
//! wasm32-unknown-unknown at all, which is where a host-buildable state machine
//! could quietly have picked up something the target does not have; and that
//! the event id survives inlining into `fk.subscribe` as an `i32.const`, so
//! `fklua mod` ships ONE event descriptor instead of all 224.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p ipc
//! fklua mod ipc.wasm --name fk-ipc --version 0.1.0 --author you
//! ```
//!
//! Run it against a companion built from `sdk/go`, with the game started as
//! `factorio --enable-lua-udp 25409` and the companion listening on 25411.

#![no_std]

extern crate alloc;

use alloc::vec::Vec;

use fkipc::{Channel, Config, Link, Message, Priority, Profile, Request, SessionEvent};

// Two channels: one for state going out and one for control coming back.
//
// Splitting them is the advice rather than decoration. A channel's seq is
// shared by everything on it, so a lost REQ raises a gap -- and therefore a
// RESYNC and a snapshot -- on whatever telemetry shares the channel.
//
// `const` and not `static`, because a Channel IS its id: naming one costs
// nothing and registers nothing, and registration happens in `_initialize`
// where a Rust guest does everything else.
const STATE: Channel = Channel::new(1);
const CONTROL: Channel = Channel::new(2);

/// The file this guest writes into script-output when asked.
const BULK_NAME: &str = "fkipc-gate.bin";

static QUEUED: &[u8] = b"queued";

/// The guest's own scratch. Single-threaded by construction -- wasm without the
/// threads proposal has one thread -- which is the same ground `fk`'s allocator
/// and `fkipc`'s singleton stand on.
struct Scratch {
    ticks: u32,
    echo: Vec<u8>,
    last_cmd: Vec<u8>,
    snap: Vec<u8>,
    /// Set by a handler, acted on from `fk_on_tick` -- see [`echo`].
    want_bulk: bool,
    bulk: Vec<u8>,
}

static mut SCRATCH: Scratch = Scratch {
    ticks: 0,
    echo: Vec::new(),
    last_cmd: Vec::new(),
    snap: Vec::new(),
    want_bulk: false,
    bulk: Vec::new(),
};

#[allow(static_mut_refs)]
fn scratch() -> &'static mut Scratch {
    unsafe { &mut SCRATCH }
}

/// Where a Rust guest's subscriptions go.
///
/// NOT `fk_on_init`, and the difference is load-bearing. `script.on_init` fires
/// once, when a save is CREATED; `_initialize` is called by control.lua on
/// every load, and event registrations are not saved. A subscription made in
/// `fk_on_init` therefore vanishes the first time the save is reloaded.
#[no_mangle]
pub extern "C" fn _initialize() {
    // THE IDENTITY TOKEN, on both sides of the pairing. It is the fourth filter
    // -- the HELLO is the session boundary, the epoch is the frame filter, the
    // SOURCE PORT is the mod filter, and the NAME is the schema filter, the only
    // one that can refuse a peer whose transport is entirely correct. Setting it
    // here is what makes `scripts/run-ipc.sh`'s matched pairing a POSITIVE
    // CONTROL for the check rather than a run in which it is merely off.
    //
    // `/1` is the SCHEMA TAG: the author's claim about channel-contract
    // compatibility, bumped when the meaning of a channel changes. Deliberately
    // not a build id -- this fixture is rebuilt on every run of the gate.
    fkipc::open(Config {
        port: 25411,
        profile: Profile::Server,
        name: "fk-ipc/1",
        expect_peer: "fk-ipc/1",
        ..Default::default()
    });

    STATE.open(Priority::Bulk);
    CONTROL.open(Priority::Control);

    CONTROL.on_message(on_command);
    CONTROL.on_request(echo);
    STATE.on_resync(resync);
    fkipc::on_session(session);
}

fn on_command(_l: &mut Link, m: Message) {
    // THE PAYLOAD BORROWS the library's buffer and the borrow ends here.
    let s = scratch();
    s.last_cmd.clear();
    s.last_cmd.extend_from_slice(m.payload);
}

/// An echo.
///
/// It copies into a reused buffer rather than returning `r.payload` -- which
/// would also compile, because the handler's return may borrow the request --
/// so the fixture shows the shape that is always right rather than the one that
/// happens to be.
fn echo(_l: &mut Link, r: Request) -> &'static [u8] {
    let s = scratch();
    // "bulk" asks for a FILE rather than an echo, and SETTING A FLAG rather
    // than writing here is not a stylistic preference on this side: during a
    // poll the transport is out of the link (`Link::pump_begin`), so a
    // `write_bulk` from inside an inbound handler answers `NoTransport`. The Go
    // mirror would let it through and it would still be a host call nested
    // inside a host call. A flag plus a write from `fk_on_tick` is right in
    // both languages.
    if r.payload == b"bulk" {
        s.want_bulk = true;
        return QUEUED;
    }
    s.echo.clear();
    s.echo.extend_from_slice(r.payload);
    &s.echo
}

/// A gap is answered with a SNAPSHOT and never a replay.
///
/// It reaches the link through the `&mut Link` it was handed, not through the
/// crate singleton -- which is the one place this mirror's API differs from the
/// Go half's, and the reason is that the singleton is already borrowed here.
fn resync(l: &mut Link) {
    let snap = snapshot();
    l.snapshot(STATE.id(), snap);
}

fn session(_l: &mut Link, ev: SessionEvent) {
    fk::log(match ev {
        SessionEvent::Up => "fkipc session up",
        SessionEvent::Down => "fkipc session down",
        SessionEvent::Reset => "fkipc session reset",
    });
}

/// THE EXPORT IS OPTIONAL NOW AND IS KEPT ON PURPOSE, because it is the shape
/// every guest written against the old four-line wiring still has, and
/// `internal/guest`'s `TestAJoiningPeerStaysByteIdenticalToTheServer` drives
/// THIS guest as its Rust arm: keeping it is what makes that test prove the
/// whole wiring is join-safe, export and all, rather than only the library
/// behind it.
///
/// `fkipc::reload` does nothing. A load is not a session boundary, because
/// `fk_after_load` fires on a joining multiplayer client and on no other peer.
#[no_mangle]
pub extern "C" fn fk_after_load() {
    fkipc::reload();
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    scratch().ticks = tick;
    fkipc::pump(tick);
    if scratch().want_bulk {
        scratch().want_bulk = false;
        // One datagram instead of sixteen, and the peer gets a length and an
        // FNV-1a-32 it can verify exactly rather than having to guess when the
        // file is finished. Prefer this to a fragmented message for anything
        // above one frame: the transport is localhost-only, so the peer is
        // always on this filesystem.
        STATE.write_bulk(BULK_NAME, bulk_payload());
    }
    if tick % 60 == 0 {
        let snap = snapshot();
        STATE.send(snap);
    }
}

/// A deterministic kilobyte: the 256 byte values, four times.
///
/// Built once into a retained buffer rather than per call, because a guest heap
/// is in the save. Every byte value is in it because that is the property the
/// probe measured on the real transport and the one a file path has no reason
/// to preserve for free.
fn bulk_payload() -> &'static [u8] {
    let s = scratch();
    if s.bulk.is_empty() {
        s.bulk.reserve(1024);
        for i in 0..1024u32 {
            s.bulk.push(i as u8);
        }
    }
    &s.bulk
}

#[no_mangle]
pub extern "C" fn fk_on_event(id: u32, ptr: u32) {
    if fkipc::on_event(id, ptr) {
        return;
    }
    // ... a guest's own events would be matched on here.
}

/// A stand-in for whatever the mod actually streams.
///
/// Built into a reused buffer because a guest heap is in the save, which is the
/// same reason the library copies a payload rather than keeping the caller's
/// slice. `format!` would allocate a fresh String every tick, which is the
/// exact shape a downstream mod measured as its entire guest heap.
fn snapshot() -> &'static [u8] {
    let s = scratch();
    s.snap.clear();
    s.snap.extend_from_slice(b"tick=");
    append_u32(&mut s.snap, s.ticks);
    &s.snap
}

fn append_u32(b: &mut Vec<u8>, v: u32) {
    if v == 0 {
        b.push(b'0');
        return;
    }
    let mut d = [0u8; 10];
    let mut i = d.len();
    let mut v = v;
    while v > 0 {
        i -= 1;
        d[i] = b'0' + (v % 10) as u8;
        v /= 10;
    }
    b.extend_from_slice(&d[i..]);
}
