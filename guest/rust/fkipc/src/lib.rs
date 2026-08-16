//! A message-oriented IPC link between a FkLua **Rust** guest and a companion
//! process on the same machine, over Factorio's UDP surface.
//!
//! It is the mirror of `guest/go/fkipc`: the same protocol, the same
//! constants, the same state machine, the same deviations from the spec draft.
//! Where the two differ, the difference is a fact about Rust and is written
//! down where it lives -- there are three, and they are named in
//! [`link::MessageFn`], [`transport_guest`]'s `send`, and [`channel::Channel`].
//!
//! It is a hand-written crate beside `fkapi`, modelled on `fkgc`: not
//! generated, not part of the bindings, no census row. It calls the generated
//! bindings and never names a member or an event id itself.
//!
//! ```ignore
//! const STATE: fkipc::Channel = fkipc::Channel::new(1);
//!
//! #[no_mangle]
//! pub extern "C" fn _initialize() {
//!     fkipc::open(fkipc::Config { port: 29434, name: "my-mod", ..Default::default() });
//!     STATE.open(fkipc::Priority::Bulk);
//! }
//!
//! #[no_mangle]
//! pub extern "C" fn fk_on_tick(tick: u32) { fkipc::pump(tick); }
//!
//! #[no_mangle]
//! pub extern "C" fn fk_on_event(id: u32, ptr: u32) {
//!     if fkipc::on_event(id, ptr) { return; }
//!     // ... your own events
//! }
//! ```
//!
//! Three exports, and each is unavoidable for a different reason. A wasm module
//! has ONE export per name, so this crate cannot own `fk_on_tick` or
//! `fk_on_event` -- the guest author's program owns them and routes in.
//!
//! - **[`open`] from `_initialize`, never from `fk_on_init`.** control.lua
//!   calls `_initialize` on EVERY load, and event registrations are not saved.
//!   `fk_on_init` fires on a new map only. (A Go guest gets this from a package
//!   initialiser; Rust has no pre-main phase in a cdylib reactor, so the guest
//!   exports the hook -- which is also why this side needs none of the Go
//!   half's "the singleton must be non-nil before `init()`" care. See
//!   [`Link::new`].)
//! - **[`open`] SENDS NOTHING.** `_initialize` is control.lua's main chunk,
//!   where 2.1 documents a non-zero `for_player` on `send_udp` as silently
//!   skipped. The first frame goes out from the first [`pump`], which is inside
//!   a dispatch.
//! - **[`on_event`] returning `bool`** so the event-id constant stays in here.
//!   `fklua mod` prunes the event table by scanning for an `i32.const` reaching
//!   `fk.subscribe`, and an id it cannot prove constant ships every descriptor
//!   there is -- about 55 KB of Lua per load at the 2.1.14 pin, silently.
//!
//! THERE USED TO BE A FOURTH EXPORT, `fk_after_load` routing to [`reload`], and
//! it is now optional because [`reload`] does nothing. Keeping it costs nothing
//! and breaks nothing; a guest with no other use for `fk_after_load` may drop
//! it. WHY it does nothing is the most important thing in this crate to
//! understand before changing any of it -- see [`Link::reload`].
//!
//! # The cost model, which decides everything by DIRECTION
//!
//! OUTBOUND IS FREE. `send_udp` and `write_file` are local side effects. Every
//! peer in a lockstep game executes the same guest code and would perform the
//! same send; `for_player` is the knob that says which peer's copy actually
//! goes out. Nothing about an outbound frame enters game state.
//!
//! INBOUND IS EXPENSIVE. A received datagram becomes an InputAction: it is
//! replicated to every peer through the multiplayer server, it lands in the
//! replay, and it is quantized to a tick. On a populated server the whole
//! inbound budget is about one full frame every forty ticks.
//!
//! So the design brief is TALK A LOT, LISTEN A LITTLE, AND MAKE THE LISTENING
//! IDEMPOTENT. It also buys the one thing that makes this protocol legal at
//! all: inbound data arrives at every peer identically, at the same tick,
//! through the engine's own replication, so a guest may branch on it without
//! desyncing. That is what lets the peer mint the session epoch.
//!
//! # The rule that falls out of it, and it is the one this crate got wrong
//!
//! NO PEER-LOCAL SIGNAL MAY MUTATE GUEST STATE. Under the default
//! `--persist=table`, guest memory IS `storage.fk_mem`, and Factorio CRCs that
//! across every peer in a multiplayer game. So the only things a guest may
//! branch on when it writes are its own state, the tick, and what arrived
//! through the replicated inbound path. `fk_after_load` is none of those -- it
//! fires on a joining client and on no other peer -- which is why [`reload`] is
//! a no-op and why every session boundary in here is driven by a BYE, by the
//! liveness test, or by the guest's own clock. See [`Link::reload`].
//!
//! # The join-safety contract
//!
//! This is the whole rule, in the form a mod author needs it. A multiplayer
//! client joining a running game downloads guest memory and then simulates
//! alongside every other peer; Factorio CRCs that memory every tick. So:
//!
//! YOU MAY BRANCH ON, AND STORE WHAT YOU DECIDED:
//!
//!   - a [`Request`] or [`Message`] payload, or a [`Reply`] -- inbound is
//!     replicated, which is what makes it the one expensive direction worth
//!     paying for;
//!   - a [`SessionEvent`], and anything derived from one;
//!   - the tick handed to [`pump`];
//!   - [`stats`] -- every counter in it is a function of the three above, of
//!     build-time configuration, or of this link's own decisions;
//!   - your own guest state, and the world you read through `fkapi`.
//!
//! YOU MUST NEVER STORE:
//!
//!   - WHETHER AN OUTBOUND HOST CALL SUCCEEDED. `send_udp`, `write_file` and
//!     `rcon.print` are local side effects and their outcome is a fact about how
//!     THIS peer was launched -- `--enable-lua-udp` binds the socket and a
//!     joining graphical client has no such flag. Attempt it and drop the
//!     answer. This crate no longer offers you one: its transport seam returns
//!     `()`, pinned by `tests/seam.rs`.
//!   - ANYTHING COMPUTED IN `fk_after_load`. It fires on the joining peer and on
//!     no other. See [`Link::reload`].
//!
//! If you want to know whether your own write landed, say so with `fk::log`.
//! The game log is not CRC'd and is per-peer by nature, which is exactly where a
//! per-peer fact belongs -- and it is the ONLY sanctioned sink for one.
//! [`Link::write_bulk`] is the worked example of the pattern: it attempts the
//! write, ignores the outcome by construction, and sends the FILE_NOTIFY that
//! consumes the channel's seq unconditionally, because a peer that skipped the
//! notify would advance that counter differently from its neighbours.
//!
//! The crate's own half of this is enforced by
//! `a_failed_send_is_invisible_to_guest_state` and, end to end through the real
//! runtime with a joiner whose socket is not bound, by `internal/guest`'s
//! `TestAJoiningPeerStaysByteIdenticalToTheServer`. Your half is yours.
//!
//! # Determinism is a correctness property here, not a style
//!
//! Nothing in this crate iterates a hash map into wire bytes, mints a
//! correlation id from randomness, or reads a clock. Every timer is in GAME
//! TICKS, because there is no wall clock in the sandbox and a tick is exactly
//! the unit whose pauses are the game's pauses.
//!
//! # What the probe measured, and the one law it imposes
//!
//! PUMPING IS FATAL WHERE IT IS NOT USELESS, BELOW AN ENGINE FLOOR. On Factorio
//! 2.0.77 a headless server calling `recv_udp` with a packet queued dies at
//! `TickClosure.cpp:91` -- a C++ abort no pcall can catch, reproduced five
//! times in five runs. It needs BOTH the pump call and a queued packet. On
//! 2.1.14 the same arm survives and delivers. So below
//! [`MIN_ENGINE_VERSION`] this crate is INERT: [`open`] answers
//! [`Status::Disabled`] and logs one line saying so, [`pump`] does nothing at
//! all, every send and request answers the same refusal, and [`stats`] counts
//! them. Not send-only -- a session is established by an ACK and an ACK
//! arrives INBOUND, so a link that can only talk searches forever.
//!
//! THE FLOOR IS ABOUT THE ENGINE AND NOT ABOUT THE API PIN. The pin is a
//! build-time choice of description; the engine is what `helpers.game_version`
//! reports at run time. Every member this crate calls exists in the 2.0.77
//! description, so a mod pinned to the general-availability release gets the
//! whole library on a newer engine with no rebuild.
//!
//! # Four filters, and one of them is yours to configure
//!
//! A frame reaches a handler only after four independent questions, and it is
//! worth knowing which is which because they fail differently:
//!
//!   - THE HELLO IS THE SESSION BOUNDARY. Everything about the old session goes.
//!   - THE EPOCH IS THE FRAME FILTER. A frame under any other session is
//!     dropped.
//!   - THE SOURCE PORT IS THE MOD FILTER. `--enable-lua-udp` binds ONE socket
//!     for the whole game, so every mod is handed every mod's datagrams.
//!   - THE NAME IS THE SCHEMA FILTER, and it is the only one that can refuse a
//!     peer whose transport is entirely correct.
//!
//! The first three are automatic. The fourth is [`Config::expect_peer`], which
//! is empty by default and therefore off: set it, and a HELLO_ACK from a
//! companion calling itself anything else is refused rather than adopted. That
//! is what turns a swapped port config or a companion left running from last
//! week into a session that never comes up, instead of two ends that agree
//! about every byte of the transport and disagree about what channel 1 means.
//!
//! It is a CORRECTNESS check, not an authentication boundary: the token is a
//! constant in a mod zip anybody can read. See `agents/ipc.md`.
//!
//! # What a handler may keep
//!
//! NOTHING IT DOES NOT COPY. Every payload handed to a handler BORROWS this
//! crate's receive buffer, and here -- unlike in the Go half, where it is a
//! comment -- the compiler enforces it.

#![no_std]

extern crate alloc;

use core::cell::{Cell, UnsafeCell};

pub mod api;
pub mod channel;
pub mod link;
pub mod transport;
pub mod version;
pub mod wire;

#[cfg(target_family = "wasm")]
mod transport_guest;

pub use api::{
    Config, Corr, Message, PeerError, Priority, Profile, Reply, ReplyError, Request, SessionEvent,
    Stats, Status,
};
pub use channel::Channel;
pub use link::{
    GapFn, Link, MessageFn, ReplyFn, RequestFn, ResyncFn, SessionFn, DEDUP_TICKS, DRAIN_MAX,
    HEARTBEAT_TICKS, LIVENESS_TICKS, MAX_DEDUP, MAX_DEDUP_PAYLOAD, MAX_PENDING, MAX_QUEUE,
    MAX_RETRIES, REASSEMBLY_TICKS, RETRY_BACKOFF_CAP, RETRY_TICKS_CLIENT, RETRY_TICKS_SERVER,
    SEARCH_TICKS, SEND_BUDGET,
};
pub use transport::Transport;
pub use version::{parse_version, Version, MIN_ENGINE_VERSION};

/// The guest's own link.
///
/// A wasm module is one instance, so a singleton is the honest model, and the
/// four wiring exports have nothing to carry a receiver in.
///
/// `busy` IS NOT DEFENSIVE PROGRAMMING, it is the borrow rule made checkable. A
/// handler runs with the link already mutably borrowed, so a handler that
/// reached this singleton again would produce two live `&mut Link` to one
/// object -- undefined behaviour rather than a style question. The handler's
/// own `&mut Link` argument is the supported route; this flag makes the
/// unsupported one a counted refusal instead of a silent miscompilation.
struct LinkCell {
    link: UnsafeCell<Link>,
    busy: Cell<bool>,
}

// Sound because a Factorio mod is single-threaded by construction: wasm without
// the threads proposal has one thread, and the target does not enable it. The
// same reasoning `guest/rust/fk`'s allocator is built on.
unsafe impl Sync for LinkCell {}

static LINK: LinkCell = LinkCell {
    link: UnsafeCell::new(Link::new()),
    busy: Cell::new(false),
};

/// Runs `f` against the guest's link, or answers [`Status::NotOpen`] if the
/// link is already borrowed -- which means `f` was reached from inside a
/// handler, where the handler's own `&mut Link` is the route.
pub(crate) fn with_link<F: FnOnce(&mut Link) -> Status>(f: F) -> Status {
    with_link_r(f, Status::NotOpen)
}

pub(crate) fn with_link_r<R, F: FnOnce(&mut Link) -> R>(f: F, busy: R) -> R {
    if LINK.busy.get() {
        return busy;
    }
    LINK.busy.set(true);
    let out = f(unsafe { &mut *LINK.link.get() });
    LINK.busy.set(false);
    out
}

/// Gives the link its configuration and its transport, and registers the event
/// subscription. Call it from `_initialize`.
///
/// IT SENDS NOTHING. `_initialize` is control.lua's main chunk, and 2.1
/// documents a non-zero `for_player` on `send_udp`, `recv_udp` and `write_file`
/// as silently skipped there. The first frame goes out from the first [`pump`],
/// which is inside an event dispatch.
///
/// Calling it twice reconfigures in place and KEEPS the channels and handlers.
pub fn open(cfg: Config) -> Status {
    // THE TRANSPORT IS BUILT FROM `cfg`, SO THE NORMALISATION HAS TO HAPPEN
    // FIRST. `configure` below applies the same rule, but to its own copy and
    // after `new_transport` has already read the raw `for_player` -- so a
    // Profile::Client guest that never set `for_player` sent every frame with
    // `for_player = 0`, which is "the server if present" and a SILENT NO-OP in
    // graphical single player. Same defect, same two lines, as the Go half.
    let cfg = link::normalise_for_player(cfg);
    #[cfg(target_family = "wasm")]
    {
        let (tr, st) = transport_guest::new_transport(cfg);
        if st != Status::Ok {
            return st;
        }
        match tr {
            Some(tr) => with_link(|l| l.configure(cfg, tr)),
            None => Status::NoTransport,
        }
    }
    // Off-target there is no `send_udp` and no event dispatcher, so there is no
    // transport this crate can build for itself. What there is instead is
    // [`attach`], which is the seam that makes the test suite a test OF THE
    // SHIPPING STATE MACHINE rather than of a second implementation somebody
    // has to keep in sync with it.
    #[cfg(not(target_family = "wasm"))]
    {
        let _ = cfg;
        Status::NoTransport
    }
}

/// Builds an independent link over a caller-supplied transport.
///
/// It exists for host-side tests and is not compiled into a guest. A guest has
/// one link and reaches it through the crate-level
/// `open`/`pump`/`reload`/`on_event`; a test wants several, in one process,
/// without a singleton making them serial.
#[cfg(not(target_family = "wasm"))]
pub fn attach(cfg: Config, tr: alloc::boxed::Box<dyn Transport>) -> Result<Link, Status> {
    let mut l = Link::new();
    match l.configure(cfg, tr) {
        // Status::Disabled IS NOT A CONSTRUCTION FAILURE, and the difference
        // matters here in a way it does not for [`open`]: a bad config leaves
        // nothing worth handing back, but a link the engine gate shut is fully
        // configured -- `stats` answers, channels register, every call refuses
        // deterministically, and a load onto a newer engine brings it up. The
        // verdict is on the link, as [`Link::enabled`], because a `Result` has
        // only one slot and the link is the more useful thing to put in it.
        Status::Ok | Status::Disabled => Ok(l),
        st => Err(st),
    }
}

/// Configures the crate's SINGLETON over a caller-supplied transport.
///
/// [`attach`] builds an INDEPENDENT link, which is what most host-side tests
/// want. This is for the one property that is about the singleton itself: that
/// a handler reaching it while it is already borrowed is refused rather than
/// aliased. Not compiled into a guest.
#[cfg(not(target_family = "wasm"))]
pub fn open_with(cfg: Config, tr: alloc::boxed::Box<dyn Transport>) -> Status {
    with_link(|l| l.configure(cfg, tr))
}

/// The `fk_after_load` route, and IT DOES NOTHING. See [`Link::reload`] for
/// why, which is the multiplayer-join fix and not an oversight. Kept so that
/// the wiring line every documented Rust guest carries goes on compiling.
pub fn reload() {
    with_link(|l| {
        l.reload();
        Status::Ok
    });
}

/// The `fk_on_tick` route. See [`Link::pump`].
///
/// THE LINK IS RELEASED ACROSS THE POLL, AND THAT IS THE WHOLE REASON THIS IS
/// NOT ONE `with_link`. `recv_udp` dispatches every queued datagram as an
/// `on_udp_packet_received` event from inside the call, and each of those
/// re-enters the module through its own `fk_on_event` export -- which reaches
/// this singleton. Holding the borrow around the poll would be two live
/// `&mut Link` to one object; [`Link::pump_begin`] and [`Link::pump_end`] exist
/// so the borrow can be dropped in between. The Go half needs none of this
/// because Go does not mind a second reference to its package's link.
///
/// A RE-ENTRANT `pump` CANNOT LOSE THE TRANSPORT, and it is worth saying why
/// because the shape looks like it could: `pump_end` moves the transport back
/// through a `with_link_r` that would DROP it if the link were busy, leaving a
/// mute guest with nothing said. It is unreachable. The only code that runs
/// while the transport is out is a handler, every handler runs inside a
/// `with_link`, and `pump_begin` takes nothing when it finds the link busy --
/// so a nested `pump` returns at the `None` arm below without ever holding one.
pub fn pump(tick: u32) {
    let (tr, poll) = with_link_r(|l| l.pump_begin(tick), (None, false));
    let mut tr = match tr {
        Some(t) => t,
        None => return,
    };
    if poll {
        for _ in 0..link::DRAIN_MAX {
            // NO BORROW IS HELD HERE. Every datagram reaches the link through
            // `deliver`, one fresh borrow at a time.
            if !tr.poll(&mut deliver) {
                break;
            }
        }
    }
    with_link_r(move |l| l.pump_end(tr), ());
}

fn deliver(src: u16, dg: &[u8]) {
    with_link(|l| {
        l.deliver_datagram(src, dg);
        Status::Ok
    });
}

/// The `fk_on_event` route. It reports whether the event was fkipc's, so the
/// guest's own match runs on everything else.
///
/// It runs on a NESTED call stack -- the engine raises the event from inside
/// the `recv_udp` that [`pump`] made -- so it takes its own fresh borrow of the
/// link. That is sound because `pump` released one, and it is why the event
/// decode is a free function in the wasm-only module rather than a method on
/// [`Transport`]: the transport is out of the link for exactly this window.
pub fn on_event(id: u32, ptr: u32) -> bool {
    #[cfg(target_family = "wasm")]
    {
        transport_guest::inbound(id, ptr, &mut deliver)
    }
    #[cfg(not(target_family = "wasm"))]
    {
        let _ = (id, ptr);
        false
    }
}

/// Registers the session-state handler.
pub fn on_session(h: SessionFn) -> Status {
    with_link(|l| {
        l.set_on_session(h);
        Status::Ok
    })
}

/// The observability snapshot.
///
/// THE TYPE IS `Stats` AND SO IS THE RETURN, which is the one spelling the Go
/// half cannot have: a Go package cannot hold a function and a type of one
/// name, so it says `Stats() LinkStats`. The fields are the same fields.
pub fn stats() -> Stats {
    with_link_r(|l| l.stats(), Stats::default())
}
