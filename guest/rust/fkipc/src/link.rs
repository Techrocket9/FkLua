//! One fkipc session's whole state, and the protocol's timers and budgets.

use alloc::boxed::Box;
use alloc::vec::Vec;

use crate::api::{
    Config, Corr, Message, PeerError, Priority, Profile, Reply, ReplyError, Request, SessionEvent,
    Stats, Status,
};
use crate::channel::ChannelState;
use crate::transport::Transport;
use crate::version::MIN_ENGINE_VERSION;
use crate::wire::{self, Flags, Header, Type};

// The protocol's timers and budgets.
//
// ALL TIMERS ARE IN GAME TICKS. There is no wall clock in the sandbox and there
// is not going to be one; a tick is 16.67 ms of nominal game time and a
// variable amount of real time, which is exactly right for a peer whose pauses
// are the game's pauses. The external side keeps its own timers in real time,
// and the two are reconciled by the tick each HEARTBEAT carries.

/// Measured, not guessed. Round-trip latency on a headless server through the
/// InputAction path is median 31.5 ms (~1.89 ticks), min 8.4 ms, p90 94.8 ms
/// (~5.7 ticks) -- so a server-profile retry under about ten ticks would be
/// retransmitting frames that were merely in flight.
pub const RETRY_TICKS_SERVER: u32 = 15;
/// The client value sits at that p90 on purpose: a single-player client has no
/// replication fan-out and its floor is ~1 tick.
pub const RETRY_TICKS_CLIENT: u32 = 6;

/// Retry backoff is x2 to this cap.
pub const RETRY_BACKOFF_CAP: u32 = 60;

/// Retransmissions before a request is declared timed out:
/// 15 + 30 + 60 + 60 = 165 ticks, about 2.8 s at the server default.
pub const MAX_RETRIES: u8 = 4;

/// Longer than the whole retry schedule, with margin.
pub const DEDUP_TICKS: u32 = 600;
/// A bound on the SAVE. The dedup table is guest memory.
pub const MAX_DEDUP: usize = 256;
/// Ditto -- a cached response is save weight.
pub const MAX_DEDUP_PAYLOAD: usize = 512;

/// One per second of game time, UNCONDITIONALLY.
///
/// It used to be "only if nothing else was sent in the window", on the grounds
/// that any frame is a liveness signal -- which is true and is not the whole
/// job. The heartbeat is the ONLY frame that carries the guest's TICK, and the
/// peer is the side with a real clock and therefore the side that has to notice
/// the guest's clock going backwards (see `sdk/go/fkipc`'s `RollbackTicks`).
/// Under the old rule a guest that streams telemetry every tick never
/// heartbeats at all, so the peer's reading of the guest clock froze at the
/// HELLO and stayed there for the whole session -- and so did the
/// rx/drops/gaps flow-control counters the heartbeat carries.
///
/// The cost is one 40-byte datagram per second of game time in the FREE
/// direction: outbound is a local side effect that is never replicated, never
/// saved into the replay and never quantized to a tick.
pub const HEARTBEAT_TICKS: u32 = 60;
/// Three missed heartbeats.
pub const LIVENESS_TICKS: u32 = 180;

/// blueprint-share's number, and it has held up.
pub const REASSEMBLY_TICKS: u32 = 120;

/// How often a peerless guest sends HELLO.
pub const SEARCH_TICKS: u32 = 60;

/// Bounds the WORST TICK, not the bandwidth. The engine does not coalesce and
/// does not rate-limit: ten `send_udp` calls in one tick produced ten
/// datagrams, in order, on loopback.
pub const SEND_BUDGET: usize = 8;

/// A safety valve rather than a working loop bound. One `recv_udp` per tick
/// drained a 20-packet backlog blasted in 0.34 ms -- all twenty arrived within
/// that tick, in order, complete -- so one call per tick is the shape, and this
/// is the knob if a future build ever delivers one packet per call instead.
pub const DRAIN_MAX: usize = 1;

/// Frames per priority class. The queue is guest memory, so it is in the save.
pub const MAX_QUEUE: usize = 64;

/// Requests in flight. A retried request keeps its whole message so it can be
/// resent, and at the message ceiling that is ~62 KB apiece, so an unbounded
/// pending table is an unbounded save.
pub const MAX_PENDING: usize = 16;

/// The host's string scratch region. Only used to count
/// [`Stats::scratch_overflows`] -- see that field for why the count is a floor.
const SCRATCH_BYTES: usize = 4096;

// The handler signatures.
//
// `fn` POINTERS AND NOT CLOSURES, which the spec settles for the allocator's
// sake: a `Box<dyn Fn>` lives in the `#[global_allocator]`-owned heap and is a
// retention the collector would have to be told about.
//
// EACH ONE TAKES `&mut Link`, AND THAT IS THIS MIRROR'S ONE STRUCTURAL
// DEVIATION FROM THE GO HALF. Go's handlers take only their payload and reach
// the link back through the package singleton -- `state.Snapshot(...)` from
// inside `OnResync` is the documented shape and the example does exactly that.
// Reaching a `static mut`-style singleton from inside a call that already holds
// `&mut` to it is two live `&mut` to one object, which is undefined behaviour
// here rather than a style question. Handing the borrow to the handler is the
// same capability, costs nothing, and the compiler checks it.
pub type MessageFn = for<'a> fn(&mut Link, Message<'a>);
/// The return value is the RESP payload, and it may borrow the request's own
/// payload -- so `fn echo(l: &mut Link, r: Request) -> &[u8] { r.payload }`
/// compiles, which is what the Go half's `return r.Payload` does.
///
/// A nil return is an empty response, not an error: an error is something the
/// handler cannot express here on purpose -- an application error belongs
/// inside the payload, where the application already has an encoding for it,
/// and the protocol's own error codes are about the PROTOCOL.
pub type RequestFn = for<'a> fn(&mut Link, Request<'a>) -> &'a [u8];
pub type ResyncFn = fn(&mut Link);
pub type GapFn = fn(&mut Link, u32);
pub type ReplyFn = for<'a> fn(&mut Link, Reply<'a>);
pub type SessionFn = fn(&mut Link, SessionEvent);

pub(crate) struct Pending {
    pub used: bool,
    pub ch: u16,
    pub corr: u32,
    pub msg: Vec<u8>,
    pub tries: u8,
    pub interval: u32,
    pub due: u32,
    pub on_reply: Option<ReplyFn>,
}

impl Pending {
    const fn new() -> Pending {
        Pending {
            used: false,
            ch: 0,
            corr: 0,
            msg: Vec::new(),
            tries: 0,
            interval: 0,
            due: 0,
            on_reply: None,
        }
    }
}

pub(crate) struct DedupEntry {
    epoch: u32,
    corr: u32,
    tick: u32,
    ch: u16,
    cached: bool,
    resp: Vec<u8>,
}

impl DedupEntry {
    const fn new() -> DedupEntry {
        DedupEntry {
            epoch: 0,
            corr: 0,
            tick: 0,
            ch: 0,
            cached: false,
            resp: Vec::new(),
        }
    }
}

/// The frame queue: a ring of reused buffers, so a steady send rate allocates
/// nothing after the first pass through it.
pub(crate) struct FrameQueue {
    slot: [Vec<u8>; MAX_QUEUE],
    head: usize,
    n: usize,
}

impl FrameQueue {
    const fn new() -> FrameQueue {
        const EMPTY: Vec<u8> = Vec::new();
        FrameQueue {
            slot: [EMPTY; MAX_QUEUE],
            head: 0,
            n: 0,
        }
    }

    fn push(&mut self, f: &[u8]) -> bool {
        if self.n == MAX_QUEUE {
            return false;
        }
        let i = (self.head + self.n) % MAX_QUEUE;
        self.slot[i].clear();
        self.slot[i].extend_from_slice(f);
        self.n += 1;
        true
    }

    fn drop_head(&mut self) {
        self.head = (self.head + 1) % MAX_QUEUE;
        self.n -= 1;
    }

    fn reset(&mut self) {
        self.head = 0;
        self.n = 0;
    }
}

/// The one place the `Profile::Client` `for_player` default lives, because two
/// places is how it came to be applied to the link and not to the transport.
///
/// `Profile::Client` with an unset `for_player` omits the argument rather than
/// asking for the server, because on a client there is no server and a
/// `for_player` naming one is the measured silent no-op -- which is what kept
/// every fkipc guest from holding a session in graphical single player.
pub(crate) fn normalise_for_player(mut cfg: Config) -> Config {
    if cfg.profile == Profile::Client && cfg.for_player == 0 {
        cfg.for_player = -1;
    }
    cfg
}

/// One fkipc session's whole state.
///
/// A guest has exactly one, reached through the crate-level
/// `open`/`pump`/`reload`/`on_event`. The type is public so the test suite can
/// drive several independent ones in one process, and because every handler is
/// handed one; on the game target there is only ever the crate's own.
pub struct Link {
    cfg: Config,
    tr: Option<Box<dyn Transport>>,

    /// The version gate. See [`crate::version`] for why refusing is the
    /// default.
    /// The engine gate. `false` means the whole link is INERT -- see
    /// `version.rs` for why refusing is the default and why it is the whole
    /// link rather than the receive path. `gated` says `regate` has run at
    /// least once, which is what makes the log line a transition rather than a
    /// repeat.
    enabled: bool,
    gated: bool,

    tick: u32,

    // The session. `up` and `epoch` move together by construction: adopting a
    // token is what "up" means, and losing the session is what clears it.
    up: bool,
    epoch: u32,
    boot: u32,

    hello_due: bool,
    hello_corr: u32,
    last_hello: u32,
    last_rx: u32,
    /// The tick the last HEARTBEAT went out on, and NOT "the last tick anything
    /// went out on". See [`HEARTBEAT_TICKS`].
    last_hb: u32,

    // What the peer said it will ACCEPT. Until a HELLO_ACK arrives these are
    // this side's own defaults, which is what the HELLO itself is sized by.
    peer_max_frame: u16,
    peer_max_frags: u16,

    corr_ctr: u32,

    chans: Vec<ChannelState>,

    pend: [Pending; MAX_PENDING],

    // The dedup table as a ring in tick order, which is what makes expiry a
    // walk from the head rather than a scan.
    dedup: Vec<DedupEntry>,
    dedup_head: usize,
    dedup_len: usize,

    q_ctl: FrameQueue,
    q_bulk: FrameQueue,

    // Reused buffers. `enc` is the frame being encoded, `ctl` the control
    // payload going into it, and `asm` the reassembled message being delivered
    // -- three rather than one because each is live while another is written.
    enc: Vec<u8>,
    ctl: Vec<u8>,
    asm: Vec<u8>,

    on_session: Option<SessionFn>,

    // Heartbeat counters, reset every time one goes out.
    hb_rx: u32,
    hb_drops: u32,
    hb_gaps: u32,

    stats: Stats,
}

impl Link {
    /// A link that exists and is not configured.
    ///
    /// `const` because the crate's singleton is a `static`, and because that is
    /// the whole of what the Go half's "the package singleton is a non-nil
    /// `&Link{}`" deviation was about. THERE IS NO PRE-MAIN INITIALISER TO RACE
    /// HERE: Go runs package-level `var` initialisers before `init()`, so
    /// `var ch = fkipc.Chan(1, ...)` names a channel before `Open` has run and
    /// a `Chan` that answered with a dead handle would silently drop every
    /// handler on it. Rust has no such phase in a cdylib reactor -- a guest
    /// exports `_initialize` and calls [`crate::open`] from it, in an order it
    /// wrote -- so the hazard does not exist and this is a `const fn` because a
    /// `static` needs one, not as a workaround for anything.
    pub const fn new() -> Link {
        const P: Pending = Pending::new();
        Link {
            cfg: Config {
                port: 0,
                profile: Profile::Server,
                for_player: 0,
                max_frame: 0,
                name: "",
                expect_peer: "",
            },
            tr: None,
            enabled: false,
            gated: false,
            tick: 0,
            up: false,
            epoch: 0,
            boot: 0,
            hello_due: false,
            hello_corr: 0,
            last_hello: 0,
            last_rx: 0,
            last_hb: 0,
            peer_max_frame: wire::DEFAULT_MAX_FRAME,
            peer_max_frags: wire::MAX_FRAGMENTS as u16,
            corr_ctr: 0,
            chans: Vec::new(),
            pend: [P; MAX_PENDING],
            dedup: Vec::new(),
            dedup_head: 0,
            dedup_len: 0,
            q_ctl: FrameQueue::new(),
            q_bulk: FrameQueue::new(),
            enc: Vec::new(),
            ctl: Vec::new(),
            asm: Vec::new(),
            on_session: None,
            hb_rx: 0,
            hb_drops: 0,
            hb_gaps: 0,
            stats: Stats::new(),
        }
    }

    /// Gives the link its configuration and its transport.
    ///
    /// Calling it twice reconfigures in place and KEEPS the channels and their
    /// handlers, which mirrors the Go half exactly.
    pub fn configure(&mut self, cfg: Config, tr: Box<dyn Transport>) -> Status {
        if cfg.port == 0 {
            return Status::BadConfig;
        }
        let mut cfg = normalise_for_player(cfg);
        // ONE TOKEN NAMES THE CONTRACT, not either party, so a guest that
        // states what it requires has by that act also stated what it is.
        // Without this a guest setting only `expect_peer` would send an empty
        // `name` and be refused by the very companion it just described, which
        // is a footgun with no upside: there is no useful configuration in
        // which a guest checks its peer's identity and wants to withhold its
        // own.
        if cfg.name.is_empty() {
            cfg.name = cfg.expect_peer;
        }
        self.cfg = cfg;
        self.tr = Some(tr);
        self.peer_max_frame = clamp_frame(cfg.max_frame);
        self.peer_max_frags = wire::MAX_FRAGMENTS as u16;
        self.hello_due = true;
        self.regate();
        // The gate's verdict is `open`'s answer, so a guest that wants to know
        // can ask at the one call it already makes. Everything else about the
        // link is configured either way -- channels, handlers and `stats` all
        // work on a disabled link, and a later load onto a newer engine brings
        // it up with no second `open`.
        if !self.enabled {
            return Status::Disabled;
        }
        Status::Ok
    }

    /// Re-reads the base-game version and decides whether the link may run at
    /// all.
    ///
    /// IT RUNS IN `configure` AND FROM THE PUMP, AND IT USED TO RUN FROM
    /// [`Link::reload`]. That move is the engine gate's half of the join fix:
    /// `fk_after_load` is a PEER-LOCAL signal, and nothing peer-local may write
    /// guest state. What replaces it is [`Link::service_gate`], which is driven
    /// by the replicated tick.
    ///
    /// Reading the version is legal from anywhere and on every peer, and that
    /// is not an accident of this design: Factorio refuses a multiplayer
    /// connection between two different builds, so `helpers.game_version` is
    /// the same string on every peer in the game. The gate is also MONOTONE --
    /// a save carries its build and the engine refuses to load one written by a
    /// NEWER build, so a restored "the link may run" can only have come from an
    /// engine at or below this one -- which is why `service_gate` only re-reads
    /// while the gate is shut.
    ///
    /// THE LOG LINE FIRES ON A TRANSITION, so it is once per load at most and
    /// not once per re-read: `service_gate` polls while shut, and a line every
    /// second would be a log nobody reads with the one interesting entry buried
    /// in it.
    fn regate(&mut self) {
        let was = self.enabled;
        let first = !self.gated;
        let v = match self.tr.as_mut() {
            Some(tr) => tr.base_version(),
            None => None,
        };
        let read = v.unwrap_or(crate::Version::ZERO);
        self.stats.base_version = read;
        self.enabled = v.map(|v| !v.less(MIN_ENGINE_VERSION)).unwrap_or(false);
        self.stats.enabled = self.enabled;
        self.gated = true;
        if self.enabled == was && !first {
            return;
        }
        let msg = if !self.enabled {
            crate::version::disabled_message(read)
        } else if !first {
            // Only reachable across a LOAD onto a newer engine, since the gate
            // is monotone within a session. Worth a line, because the previous
            // session logged the refusal and a reader deserves to see it
            // withdrawn.
            let mut s = alloc::string::String::from("fkipc: enabled -- this engine is ");
            crate::version::append_version(&mut s, read);
            s
        } else {
            return;
        };
        if let Some(tr) = self.tr.as_mut() {
            tr.log(&msg);
        }
    }

    /// Re-opens the engine gate for a save written on an older engine, without
    /// ever branching on anything peer-local.
    ///
    /// A SAVE REALLY DOES MOVE BETWEEN ENGINES, which is the whole reason this
    /// exists: an engine cannot change under a running game, so within one
    /// session the answer is fixed -- but a map made on 2.0.77 and then loaded
    /// on 2.1.14 is an ordinary thing for a player to do, and a link that only
    /// asked at `open` would stay dead for the life of that save. `open` runs
    /// from `_initialize` on every load, so the common case is already covered;
    /// this covers the residual one where the transport's read failed the first
    /// time.
    ///
    /// The condition is guest state (`enabled`) and the REPLICATED tick, so
    /// every peer re-reads on the same tick and writes the same answer -- which
    /// is the whole property the load-reset design broke. The cost is one host
    /// call per game second, and only on a build where the link is refused
    /// anyway; once the gate is open it is never re-read, because it cannot
    /// shut again.
    fn service_gate(&mut self) {
        if self.enabled || self.tick % SEARCH_TICKS != 0 {
            return;
        }
        self.regate();
    }
    /// The `fk_after_load` route, and IT DOES NOTHING. That is the fix, not an
    /// oversight, and it is the single most important doc comment in this
    /// crate.
    ///
    /// `fk_mod.lua` arms its `fk_after_load` one-shot from `script.on_load`,
    /// and Factorio runs `script.on_load` ON EVERY PEER THAT LOADS THE STATE --
    /// including a client joining a game already in progress. The server ran it
    /// when it started and will not run it again; the joiner runs it on its
    /// first simulated tick. So `fk_after_load` is a PEER-LOCAL signal, and
    /// under the default `--persist=table` guest memory IS `storage.fk_mem`,
    /// which Factorio CRCs. This function used to bump `boot`, discard the
    /// epoch, drop the dedup table, fail every pending request and reset every
    /// channel's seq -- all of it on one peer only, one tick after a join.
    /// Measured on 2.1.14: `fkipc session reset` on the client, followed by
    /// `Multiplayer desynchronisation: crc test failed` on the very next tick.
    ///
    /// THE GENERAL RULE, WHICH IS NOT FKIPC'S: no peer-local signal may mutate
    /// guest state. `fk_mod.lua` says it one level up for the hook this one is
    /// armed from -- "on_load is READ-ONLY with respect to storage ... a write
    /// here is a desync waiting to happen" -- and a one-shot armed from on_load
    /// is a write from on_load with one tick of delay.
    ///
    /// WHAT REPLACES IT, because a load really does invalidate a session and
    /// something has to notice: a companion that restarted answers BYE, which
    /// is an InputAction and therefore reaches every peer at the same tick; a
    /// companion that kept running across the guest's rollback sees the guest's
    /// clock go backwards in its HEARTBEATs and BYEs; and a companion that is
    /// simply gone is caught by [`Link::service_session`]'s liveness test
    /// within [`LIVENESS_TICKS`]. Every one of those is driven by replicated
    /// state, which is why they are join-safe and the load-reset was not.
    ///
    /// It is kept -- rather than deleted -- because the wiring line
    /// `#[no_mangle] pub extern "C" fn fk_after_load() { fkipc::reload() }` is
    /// in every guest this project has ever documented, and removing the
    /// function it calls turns a correctness fix into a compile error in
    /// somebody else's mod. A guest with no other use for `fk_after_load` may
    /// now drop the export.
    pub fn reload(&mut self) {}

    /// The `fk_on_tick` route.
    ///
    /// A PERMANENT `fk_on_tick` IS THE RIGHT SHAPE HERE, which is the one place
    /// this repo's standing "register nothing when idle" bias does not survive
    /// contact with the problem: a guest with no peer must still be able to
    /// NOTICE one appearing, and the only way to notice is to call `recv_udp`.
    /// A guest that registers nothing is a guest that can never be reached
    /// again. And a slow poll costs the same registration anyway, because
    /// `fk.defer` is a next-tick one-shot with no way to skip ticks.
    ///
    /// So what varies with session state is the work inside, not the
    /// registration: an IPC guest with no peer pays one dispatch and one
    /// integer compare per tick plus a `recv_udp` every `SEARCH_TICKS`-worth of
    /// searching, and one with a live peer pays one dispatch and one `recv_udp`
    /// per tick.
    pub fn pump(&mut self, tick: u32) {
        let (tr, poll) = self.pump_begin(tick);
        // `None` is either "no transport" or "the engine gate is shut"; both
        // mean the transport was never taken and there is nothing to put back.
        let mut tr = match tr {
            Some(t) => t,
            None => return,
        };
        if poll {
            for _ in 0..DRAIN_MAX {
                let any = {
                    let me = &mut *self;
                    tr.poll(&mut |src: u16, dg: &[u8]| me.deliver_datagram(src, dg))
                };
                if !any {
                    break;
                }
            }
        }
        self.pump_end(tr);
    }

    /// The first half of a pump: set the tick, lift the transport out, and say
    /// whether the version gate allows a `recv_udp`.
    ///
    /// IT IS SPLIT BECAUSE THE POLL RE-ENTERS THE GUEST. On the game target
    /// `recv_udp` dispatches every queued datagram as an
    /// `on_udp_packet_received` event from INSIDE the call, and each of those
    /// re-enters the module through its own `fk_on_event` export, which reaches
    /// the crate singleton -- so the singleton must not be borrowed while
    /// `poll` runs, or that is two live `&mut Link` to one object. See
    /// [`crate::pump`], which is the caller that needs the two halves apart;
    /// [`Link::pump`] above is the same sequence for a link that is not the
    /// singleton and can hold its own borrow throughout.
    ///
    /// One consequence, stated because it is real and the Go half does not have
    /// it: for the duration of the poll the link holds NO transport, so
    /// [`Link::write_bulk`] and [`Link::notify_file`] called from inside an
    /// inbound handler answer [`Status::NoTransport`]. A guest that wants to
    /// write a file in response to a message sets a flag and does it from
    /// `fk_on_tick`, which is the better shape anyway -- a `write_file` from
    /// inside an event dispatch is a host call nested inside a host call.
    pub fn pump_begin(&mut self, tick: u32) -> (Option<Box<dyn Transport>>, bool) {
        if self.tr.is_none() {
            return (None, false);
        }
        self.tick = tick;
        self.service_gate();
        // BELOW THE FLOOR THE PUMP DOES NOTHING, and "nothing" is literal: no
        // poll, no HELLO, no heartbeat, no flush, not one datagram of any kind.
        // The transport is NOT lifted out, so `pump` returns before
        // `pump_end` -- which is safe precisely because nothing was taken.
        //
        // It is not a fast path bolted on. A send-only link would still HELLO
        // once a second forever -- the ACK that would end the search arrives
        // inbound, which is the direction that is shut -- so "outbound is free"
        // is true of the cost and false of the usefulness. See
        // [`crate::MIN_ENGINE_VERSION`].
        if !self.enabled {
            self.stats.refusals += 1;
            return (None, false);
        }
        // Receive first, so a reply that arrived this tick cancels its own
        // retry before the retry timer runs.
        (self.tr.take(), true)
    }

    /// The second half: put the transport back and run everything the pump owes
    /// whether or not anything arrived.
    pub fn pump_end(&mut self, tr: Box<dyn Transport>) {
        self.tr = Some(tr);
        self.expire_reassembly();
        self.expire_dedup();
        self.service_session();
        self.service_retries();
        self.flush();
    }

    /// One inbound datagram and the port it came FROM.
    ///
    /// Public because on the game target it arrives from `fk_on_event` rather
    /// than from the poll callback -- see [`Link::pump_begin`].
    ///
    /// THE SOURCE-PORT TEST IS FIRST, AND IT IS THE ONLY THING THAT MAKES TWO
    /// IPC MODS IN ONE GAME SAFE. `--enable-lua-udp` binds ONE socket for the
    /// whole game, so `on_udp_packet_received` fires in every mod for every
    /// mod's datagrams: mod A's link sees mod B's frames and vice versa. The
    /// epoch filter catches most of that, and there is exactly one hole in it,
    /// which is not hypothetical --
    ///
    /// > HELLO_ACK IS THE ONE FRAME MATCHED ON `corr` WITH THE EPOCH TEST
    /// > SKIPPED, because by definition it carries an epoch the guest does not
    /// > yet know. `corr` is minted from a COUNTER, and two freshly-loaded
    /// > guests have identical counter state, so both first HELLOs carry
    /// > `corr = 1`. Two companions answer, both ACKs reach both mods, and
    /// > whichever lands first is adopted -- so mod A can adopt mod B's token,
    /// > then talk to A's companion under an epoch A's companion has never
    /// > heard of, which that side answers with BYE.
    ///
    /// A frame from any port but the configured peer's is dropped before
    /// anything else looks at it -- before `rx_bytes`, so a session's byte
    /// accounting describes its OWN conversation, and before the
    /// `scratch_overflows` floor, which is a statement about this link's
    /// negotiated frame size.
    ///
    /// TWO DELIBERATE ASYMMETRIES WITH THE OTHER DROPS. It is counted in
    /// `drops` and `foreign_drops` but NOT in `hb_drops`: that is flow control,
    /// the number this side asks its peer to slow down over, and another mod's
    /// traffic is not something the peer can do anything about. And `src == 0`
    /// is ACCEPTED: zero is not a valid UDP source port, so it means "the
    /// engine did not say", and refusing on silence would make a guest deaf on
    /// any build that stops reporting the field -- deafness is silent and
    /// total, cross-talk is loud and recoverable.
    pub fn deliver_datagram(&mut self, src: u16, dg: &[u8]) {
        if src != 0 && src != self.cfg.port {
            self.stats.drops += 1;
            self.stats.foreign_drops += 1;
            return;
        }
        self.deliver(dg)
    }

    /// Whether the engine gate opened. See [`crate::version`].
    pub fn enabled(&self) -> bool {
        self.enabled
    }

    /// What every outbound API entry point answers on a link the engine gate
    /// has shut, and it is ONE helper so the answer cannot differ by call site.
    ///
    /// `Status::Disabled` rather than `NoSession` or `NotOpen` -- the reasoning
    /// is on the variant itself, and it comes down to a permanent condition not
    /// being reported with a transient one's name.
    fn refused(&mut self) -> Status {
        self.stats.refusals += 1;
        Status::Disabled
    }

    /// Registers the session-state handler.
    pub fn set_on_session(&mut self, h: SessionFn) {
        self.on_session = Some(h);
    }

    /// The observability snapshot.
    pub fn stats(&self) -> Stats {
        let mut s = self.stats;
        s.epoch = self.epoch;
        s.up = self.up;
        s.boot = self.boot;
        s.queue_depth = (self.q_ctl.n + self.q_bulk.n) as u32;
        s
    }

    /// The last tick handed to [`Link::pump`], so a test can assert on the
    /// library's own notion of time rather than on its own loop counter.
    pub fn tick(&self) -> u32 {
        self.tick
    }

    /// Models what a save carries.
    ///
    /// `boot` lives in guest memory, which is persisted, and a SESSION BOUNDARY
    /// bumps it -- a load does not, since the join fix. TWO LOADS OF ONE SAVE
    /// THEREFORE PRODUCE THE SAME `boot`, literally the number the save
    /// carries: that is the theorem the whole epoch design rests on, and it is
    /// what a test asserting "the peer must not trust boot" needs to be able to
    /// set up. Off-target there is no linear memory to restore, so the value is
    /// handed over instead.
    pub fn restore_boot(&mut self, b: u32) {
        self.boot = b;
        self.stats.boot = b;
    }

    // -----------------------------------------------------------------------
    // The session.

    /// The whole of what a tick owes the session, and both of its tests are
    /// functions of guest state and the REPLICATED tick -- which is what makes
    /// them legal to run on every peer, including one that just joined.
    fn service_session(&mut self) {
        if self.up {
            // TWO CONDITIONS, AND THE SECOND ONE IS THE GUEST'S OWN HALF OF
            // ROLLBACK DETECTION rather than an arithmetic accident.
            //
            // d < 0 says the clock has gone BACKWARDS since the last frame this
            // link accepted -- a save restored to a point before it, i.e. the
            // session in memory belongs to a future that no longer happened. It
            // used to fall out of the wrapping subtraction overshooting
            // LIVENESS_TICKS, which gave the right answer for the wrong stated
            // reason; now that a load resets nothing, it is load-bearing and is
            // spelled out.
            //
            // It catches a rollback LARGER than the time since the last inbound
            // frame, which with a peer heartbeating once a second is most of
            // them. What it cannot catch is a save taken just after an inbound
            // frame and restored much later -- tick and last_rx move together,
            // so the difference is small and nothing here is wrong. That one is
            // the peer's, because only the peer has a clock that did not travel
            // with the save.
            let d = self.tick.wrapping_sub(self.last_rx) as i32;
            if d < 0 || d as u32 > LIVENESS_TICKS {
                self.reset_session(SessionEvent::Down);
            }
        }
        if !self.up {
            if self.hello_due || self.tick.wrapping_sub(self.last_hello) >= SEARCH_TICKS {
                self.send_hello();
            }
            return;
        }
        // UNCONDITIONAL, not "only if nothing else went out". See
        // HEARTBEAT_TICKS: this is the only frame carrying the guest's tick and
        // its flow-control counters, and a telemetry-heavy guest would
        // otherwise never send one.
        if self.tick.wrapping_sub(self.last_hb) >= HEARTBEAT_TICKS {
            self.queue_heartbeat();
        }
    }

    /// The quiesce, and everything it does is the same rule: nothing survives a
    /// session boundary except the application's own handlers.
    ///
    /// A guest whose peer is down KEEPS PLAYING. It does not retry harder, it
    /// does not buffer against the peer's return, and it never blocks -- so the
    /// send queue goes rather than growing, and `send` becomes a counted no-op
    /// rather than an error the mod has to handle at every call site.
    ///
    /// EVERY CALLER IS A REPLICATED SIGNAL and that is now a rule rather than
    /// an observation: the liveness test above (guest state and the replicated
    /// tick) and a BYE (an InputAction, delivered to every peer at the same
    /// tick). Nothing peer-local may reach here -- which is exactly what
    /// [`Link::reload`] stopped doing.
    fn reset_session(&mut self, ev: SessionEvent) {
        self.up = false;
        self.epoch = 0;
        self.hello_corr = 0;
        self.hello_due = true;
        // THE SESSION GENERATION, and it is what `boot` means now. It used to
        // be a LOAD counter bumped by `reload`, which is the one place it could
        // not be bumped; a session boundary is replicated, so this is. It still
        // aliases across two loads of one save -- the save carries it -- so the
        // peer must still never compare it.
        self.boot = self.boot.wrapping_add(1);
        self.stats.boot = self.boot;
        // The dedup table is dead the moment the epoch is: every entry is keyed
        // by the epoch it was recorded under, so nothing here can ever match
        // again and it is pure save weight until DEDUP_TICKS expires it.
        self.dedup_head = 0;
        self.dedup_len = 0;
        self.q_ctl.reset();
        self.q_bulk.reset();

        for i in 0..MAX_PENDING {
            if !self.pend[i].used {
                continue;
            }
            let cb = self.pend[i].on_reply;
            let corr = self.pend[i].corr;
            self.free_pending(i);
            if let Some(f) = cb {
                f(
                    self,
                    Reply {
                        corr: Corr(corr),
                        payload: &[],
                        err: Some(ReplyError::SessionLost),
                    },
                );
            }
        }
        for c in self.chans.iter_mut() {
            c.tx_seq = 0;
            c.rx_last = 0;
            c.resync_sent = false;
            c.abandon();
        }
        if let Some(h) = self.on_session {
            h(self, ev);
        }
    }

    fn send_hello(&mut self) {
        self.hello_corr = self.next_corr();
        self.hello_due = false;
        self.last_hello = self.tick;

        let mut ctl = core::mem::take(&mut self.ctl);
        ctl.clear();
        let built = wire::control::append_hello(
            &mut ctl,
            &wire::Hello {
                proto_min: wire::VERSION,
                proto_max: wire::VERSION,
                max_frame: clamp_frame(self.cfg.max_frame),
                max_fragments: wire::MAX_FRAGMENTS as u16,
                boot: self.boot,
                tick: self.tick,
                profile: wire::Profile(self.cfg.profile as u8),
                name: alloc::string::String::from(self.cfg.name),
            },
        );
        if built.is_ok() {
            // HELLO bypasses the queue. It is the recovery path, and the queue
            // is the thing a quiesce just threw away.
            let mut enc = core::mem::take(&mut self.enc);
            enc.clear();
            let framed = wire::append_frame(
                &mut enc,
                Header {
                    ty: Type::HELLO,
                    epoch: self.boot,
                    corr: self.hello_corr,
                    ..Default::default()
                },
                &ctl,
            );
            if framed.is_ok() {
                self.raw_send_slice(&enc);
            }
            self.enc = enc;
        }
        self.ctl = ctl;
    }

    /// THE ONE EPOCH-TEST EXEMPTION, and it is stated here rather than left to
    /// be inferred because two implementations would otherwise disagree about
    /// it: HELLO_ACK carries an epoch the guest does not yet know, by
    /// definition, so it is matched on `corr` against the outstanding HELLO
    /// instead.
    ///
    /// Adopting a peer-chosen value into guest state is legal, and the reason
    /// is the cost model. The token arrives via `recv_udp`, which enters game
    /// state as an InputAction, which the engine replicates to every peer at
    /// the same tick. Every peer's guest adopts the same token at the same
    /// tick. This is the expensive direction paying for itself.
    fn on_hello_ack(&mut self, h: Header, p: &[u8]) {
        if self.hello_corr == 0 || h.corr != self.hello_corr {
            self.drop_epoch();
            return;
        }
        let hello = match wire::control::decode_hello(p) {
            Ok(v) => v,
            Err(_) => {
                self.drop_bad();
                return;
            }
        };
        // THE NAME IS THE SCHEMA FILTER, and it is a fourth mechanism rather
        // than a refinement of any of the other three: the HELLO is the session
        // boundary, the epoch is the frame filter, the SOURCE PORT is the mod
        // filter, and this is the only one that can refuse a peer whose
        // transport is entirely correct. A swapped port config or a companion
        // left running from last week produces a handshake that succeeds at
        // every layer below this one and two ends that disagree about what
        // channel 1 means.
        //
        // IT IS LEGAL FOR THE SAME REASON ADOPTING THE TOKEN IS. The ACK
        // arrived through `recv_udp`, which is an InputAction, which the engine
        // replicates to every peer at the same tick -- so refusing it, counting
        // the refusal and carrying on searching are identical on every peer,
        // exactly as adopting it would have been. The configuration it is
        // compared against is a build-time constant, identical on every peer by
        // construction. Nothing here touches a peer-local signal, which is what
        // the load-reset design got wrong -- see [`Link::reload`].
        //
        // NOTHING ABOUT THE OUTSTANDING HELLO IS CONSUMED, and that is the
        // whole of the retry continuation:
        //
        //   - `hello_corr` is LEFT SET, so this HELLO is still answerable. A
        //     correct ACK on the same corr -- the companion restarted with the
        //     right identity while that HELLO was in flight, or two companions
        //     answered and the right one was second -- is still adopted, where
        //     zeroing it would have made the guest deaf until the next search.
        //   - `last_hello` and `hello_due` are LEFT ALONE, so the search
        //     cadence does not change. Re-HELLOing at once on a reject is the
        //     tempting move and it is a HELLO storm: a mismatched companion
        //     answers every HELLO, so "reject, then re-HELLO" is a frame per
        //     tick in both directions for as long as the misconfiguration
        //     lasts. That is the livelock shape the source-port filter was
        //     built to end, met from a new direction.
        //
        // So a rejected ACK costs exactly one counted drop and the link goes on
        // searching at SEARCH_TICKS, which is what it was already doing.
        if !self.cfg.expect_peer.is_empty() && hello.name != self.cfg.expect_peer {
            // Counted like a foreign-port drop and for its reason: charged to
            // `drops` so a total still accounts for every refused frame, and
            // NOT to `hb_drops`, because that is flow control -- the number
            // this side asks its peer to slow down over -- and a peer that is
            // the wrong program is not something any rate can fix.
            self.stats.drops += 1;
            self.stats.name_rejects += 1;
            return;
        }
        self.epoch = h.epoch;
        self.up = true;
        self.hello_corr = 0;
        self.last_rx = self.tick;
        // So the first heartbeat of a session is exactly HEARTBEAT_TICKS after
        // it comes up rather than at whatever offset the previous one left.
        self.last_hb = self.tick;
        self.peer_max_frame = clamp_frame(hello.max_frame);
        self.peer_max_frags = hello.max_fragments;
        if self.peer_max_frags == 0 || self.peer_max_frags > wire::MAX_FRAGMENTS as u16 {
            self.peer_max_frags = wire::MAX_FRAGMENTS as u16;
        }
        self.stats.rx_frames += 1;
        self.hb_rx += 1;
        for c in self.chans.iter_mut() {
            c.tx_seq = 0;
            c.rx_last = 0;
            c.resync_sent = false;
            c.abandon();
        }
        if let Some(f) = self.on_session {
            f(self, SessionEvent::Up);
        }
    }

    fn queue_heartbeat(&mut self) {
        let mut ctl = core::mem::take(&mut self.ctl);
        ctl.clear();
        wire::control::append_heartbeat(
            &mut ctl,
            wire::Heartbeat {
                tick: self.tick,
                rx: self.hb_rx,
                drops: self.hb_drops,
                gaps: self.hb_gaps,
            },
        );
        let st = self.queue_frame(
            Priority::Control,
            Header {
                ty: Type::HEARTBEAT,
                epoch: self.epoch,
                ..Default::default()
            },
            &ctl,
        );
        self.ctl = ctl;
        if st == Status::Ok {
            self.hb_rx = 0;
            self.hb_drops = 0;
            self.hb_gaps = 0;
            // Charged here rather than at the flush so a full control queue
            // does not turn into a heartbeat every tick.
            self.last_hb = self.tick;
        }
    }

    fn next_corr(&mut self) -> u32 {
        self.corr_ctr = self.corr_ctr.wrapping_add(1);
        if self.corr_ctr == 0 {
            self.corr_ctr = 1; // 0 means "no correlation"
        }
        self.corr_ctr
    }

    fn retry_ticks(&self) -> u32 {
        if self.cfg.profile == Profile::Client {
            RETRY_TICKS_CLIENT
        } else {
            RETRY_TICKS_SERVER
        }
    }

    // -----------------------------------------------------------------------
    // Outbound.

    /// Puts one frame on the socket and DELIBERATELY IGNORES WHETHER IT WENT,
    /// which is the second half of the peer-local rule and cost a multiplayer
    /// join on its own.
    ///
    /// Whether `send_udp` succeeds is a fact about THIS PEER'S COMMAND LINE:
    /// `--enable-lua-udp` is what binds the socket, a headless server in this
    /// project has it and a graphical client joining that server does not. So a
    /// guest that branched on the outcome wrote `tx_frames`/`tx_bytes` on the
    /// server and `queue_drops` on the client, every frame, into
    /// `storage.fk_mem` -- which Factorio CRCs. Measured on 2.1.14: a client
    /// joining a server running the demo mods desyncs on the first tick it
    /// simulates, with no companion anywhere, while the same client joining a
    /// server running a NON-IPC guest stays in sync indefinitely.
    ///
    /// So these count what this link ATTEMPTED, which is a deterministic
    /// function of guest state and therefore identical on every peer. Losing
    /// the ability to see a failed send in `stats` is the right trade in a
    /// direction the cost model already calls FREE.
    ///
    /// It used to ignore an outcome the seam offered; [`Transport::send`]
    /// returns `()` now, so there is no outcome to ignore and no way for a
    /// later edit here to reintroduce the branch. The reasoning above is kept
    /// because it is what the void return is FOR.
    fn raw_send_slice(&mut self, f: &[u8]) {
        let mut tr = match self.tr.take() {
            Some(t) => t,
            None => return,
        };
        tr.send(f);
        self.tr = Some(tr);
        self.stats.tx_frames += 1;
        self.stats.tx_bytes += f.len() as u32;
    }

    fn queue_frame(&mut self, pri: Priority, h: Header, payload: &[u8]) -> Status {
        let mut enc = core::mem::take(&mut self.enc);
        enc.clear();
        let built = wire::append_frame(&mut enc, h, payload);
        let out = if built.is_err() {
            Status::TooLarge
        } else {
            let q = if pri == Priority::Control {
                &mut self.q_ctl
            } else {
                &mut self.q_bulk
            };
            if q.push(&enc) {
                Status::Ok
            } else {
                self.stats.queue_drops += 1;
                Status::QueueFull
            }
        };
        self.enc = enc;
        out
    }

    /// Fragments one message onto one channel and queues every piece, or queues
    /// none of them.
    ///
    /// ALL OR NOTHING, because a partially queued message is a guaranteed gap
    /// at the far end plus a reassembly that can never complete -- strictly
    /// worse than the send the caller can see fail.
    fn send_message(
        &mut self,
        ci: usize,
        ty: Type,
        flags: Flags,
        mut corr: u32,
        payload: &[u8],
    ) -> Status {
        let room = self.peer_max_frame as isize - wire::HEADER_BYTES as isize;
        if room <= 0 {
            return Status::TooLarge;
        }
        let room = room as usize;
        let mut n = (payload.len() + room - 1) / room;
        if n == 0 {
            n = 1;
        }
        let mut max_frags = self.peer_max_frags as usize;
        if max_frags > wire::MAX_FRAGMENTS as usize {
            max_frags = wire::MAX_FRAGMENTS as usize;
        }
        if n > max_frags {
            return Status::TooLarge;
        }
        let pri = self.chans[ci].pri;
        let free = MAX_QUEUE
            - if pri == Priority::Control {
                self.q_ctl.n
            } else {
                self.q_bulk.n
            };
        if free < n {
            self.stats.queue_drops += 1;
            return Status::QueueFull;
        }
        // A multi-fragment message needs a correlation id to be reassembled by.
        if n > 1 && corr == 0 {
            corr = self.next_corr();
        }
        let id = self.chans[ci].id;
        for i in 0..n {
            let lo = i * room;
            let hi = core::cmp::min(lo + room, payload.len());
            let seq = self.chans[ci].next_seq();
            let epoch = self.epoch;
            let st = self.queue_frame(
                pri,
                Header {
                    ty,
                    flags,
                    channel: id,
                    epoch,
                    seq,
                    corr,
                    length: 0,
                    frag: i as u8,
                    nfrag: n as u8,
                },
                &payload[lo..hi],
            );
            if st != Status::Ok {
                return st;
            }
        }
        Status::Ok
    }

    /// [`Link::send_message`] over the reusable control buffer, which the
    /// caller has just filled. The buffer comes out of the link because
    /// `send_message` needs the link -- the same move `pump` makes with the
    /// transport, for the same reason.
    fn send_ctl(&mut self, ci: usize, ty: Type, flags: Flags, corr: u32) -> Status {
        let ctl = core::mem::take(&mut self.ctl);
        let st = self.send_message(ci, ty, flags, corr, &ctl);
        self.ctl = ctl;
        st
    }

    fn flush(&mut self) {
        for _ in 0..SEND_BUDGET {
            let from_ctl = self.q_ctl.n > 0;
            if !from_ctl && self.q_bulk.n == 0 {
                return;
            }
            let f = {
                let q = if from_ctl {
                    &mut self.q_ctl
                } else {
                    &mut self.q_bulk
                };
                let head = q.head;
                q.drop_head();
                core::mem::take(&mut q.slot[head])
            };
            self.raw_send_slice(&f);
            // Back into the ring, so the buffer keeps its capacity.
            let q = if from_ctl {
                &mut self.q_ctl
            } else {
                &mut self.q_bulk
            };
            let back = (q.head + q.n) % MAX_QUEUE;
            q.slot[back] = f;
        }
    }

    fn service_retries(&mut self) {
        for i in 0..MAX_PENDING {
            if !self.pend[i].used || (self.tick.wrapping_sub(self.pend[i].due) as i32) < 0 {
                continue;
            }
            if self.pend[i].tries >= MAX_RETRIES {
                let cb = self.pend[i].on_reply;
                let corr = self.pend[i].corr;
                self.free_pending(i);
                self.stats.timeouts += 1;
                if let Some(f) = cb {
                    f(
                        self,
                        Reply {
                            corr: Corr(corr),
                            payload: &[],
                            err: Some(ReplyError::Timeout),
                        },
                    );
                }
                continue;
            }
            let ci = match self.find_chan(self.pend[i].ch) {
                Some(ci) => ci,
                None => {
                    self.free_pending(i);
                    continue;
                }
            };
            self.pend[i].tries += 1;
            self.stats.retries += 1;
            let corr = self.pend[i].corr;
            let msg = core::mem::take(&mut self.pend[i].msg);
            self.send_message(ci, Type::REQ, Flags::RETRY, corr, &msg);
            self.pend[i].msg = msg;
            let mut iv = self.pend[i].interval * 2;
            if iv > RETRY_BACKOFF_CAP {
                iv = RETRY_BACKOFF_CAP;
            }
            self.pend[i].interval = iv;
            self.pend[i].due = self.tick.wrapping_add(iv);
        }
    }

    pub(crate) fn alloc_pending(&mut self) -> Option<usize> {
        for i in 0..MAX_PENDING {
            if !self.pend[i].used {
                self.pend[i].used = true;
                return Some(i);
            }
        }
        None
    }

    pub(crate) fn free_pending(&mut self, i: usize) {
        let p = &mut self.pend[i];
        p.used = false;
        p.on_reply = None;
        p.msg.clear();
        p.tries = 0;
    }

    fn find_pending(&self, ch: u16, corr: u32) -> Option<usize> {
        (0..MAX_PENDING).find(|&i| {
            let p = &self.pend[i];
            p.used && p.ch == ch && p.corr == corr
        })
    }

    // -----------------------------------------------------------------------
    // Inbound.

    fn drop_bad(&mut self) {
        self.stats.drops += 1;
        self.hb_drops += 1;
        self.stats.bad_frames += 1;
    }

    fn drop_epoch(&mut self) {
        self.stats.drops += 1;
        self.hb_drops += 1;
        self.stats.epoch_drops += 1;
    }

    fn drop_stale(&mut self) {
        self.stats.drops += 1;
        self.hb_drops += 1;
        self.stats.stale_drops += 1;
    }

    fn deliver(&mut self, dg: &[u8]) {
        self.stats.rx_bytes += dg.len() as u32;
        if dg.len() >= SCRATCH_BYTES {
            self.stats.scratch_overflows += 1;
        }
        let (h, p) = match wire::decode(dg) {
            Ok(v) => v,
            Err(_) => {
                self.drop_bad();
                return;
            }
        };
        if h.ty == Type::HELLO_ACK {
            self.on_hello_ack(h, p);
            return;
        }
        if !self.up || h.epoch != self.epoch {
            self.drop_epoch();
            return;
        }
        self.stats.rx_frames += 1;
        self.hb_rx += 1;
        self.last_rx = self.tick;

        match h.ty {
            // Liveness only, already charged. The counters it carries are for
            // the PEER's flow control, not this side's.
            Type::HEARTBEAT => {}
            Type::BYE => self.reset_session(SessionEvent::Down),
            Type::MSG | Type::REQ | Type::RESP | Type::RESYNC => self.channel_frame(h, p),
            // Inbound only. A guest cannot read files -- there is no file-read
            // API -- so a notify aimed at one is meaningless and is counted
            // rather than delivered to a handler that could do nothing with it.
            Type::FILE_NOTIFY => self.drop_bad(),
            // HELLO: the guest is the side that sends those.
            _ => self.drop_bad(),
        }
    }

    fn channel_frame(&mut self, h: Header, p: &[u8]) {
        let ci = match self.find_chan(h.channel) {
            Some(ci) => ci,
            None => {
                self.drop_bad();
                return;
            }
        };
        // Channel 0 is the protocol's own and carries no seq: a lost heartbeat
        // is normal and must not read as a gap in application state.
        if h.channel != 0 {
            // A SNAPSHOT RESETS `rx_last` RATHER THAN ADVANCING IT, and it is
            // exempt from the staleness rule for the same reason: it is a
            // COMPLETE state, so accepting it can never deliver a world older
            // than the one already delivered, and it is the only frame that can
            // rescue a channel whose counter has got ahead of its sender.
            // Without the exemption a receiver whose `rx_last` ever jumped
            // forward -- a corrupted seq, a peer that restarted its counter
            // without a new epoch -- is deaf on that channel FOREVER: every
            // later frame reads as old, so no gap is ever raised, so no RESYNC
            // is ever sent, and nothing anywhere says anything. Found by the
            // Go half's seeded fault soak.
            let snapshot = h.ty == Type::MSG && h.flags.has(Flags::SNAPSHOT);
            let d = wire::serial_delta(h.seq, self.chans[ci].rx_last);
            if d <= 0 && !snapshot {
                // DROPPING d <= 0 IS A SEMANTIC CHOICE: a channel carries
                // STATE, not a LOG. An out-of-order or duplicated frame
                // describes an older world than one already delivered, and
                // stale game state is worse than useless. An application that
                // needs an append-only record numbers its own entries inside
                // the payload.
                self.drop_stale();
                return;
            }
            if d > 1 && !snapshot {
                self.stats.gaps += 1;
                self.hb_gaps += 1;
                if !self.chans[ci].resync_sent {
                    // Through send_message rather than queue_frame, because a
                    // RESYNC names a channel and therefore consumes that
                    // channel's seq -- one sent with seq 0 would arrive as
                    // d <= 0 and be dropped as stale by the very rule it exists
                    // to escape.
                    self.chans[ci].resync_sent = true;
                    self.send_message(ci, Type::RESYNC, Flags::NONE, 0, &[]);
                }
                if let Some(f) = self.chans[ci].on_gap {
                    f(self, (d - 1) as u32);
                }
            }
            self.chans[ci].rx_last = h.seq;
        }
        if h.nfrag == 1 {
            self.dispatch(ci, h, p);
            return;
        }
        self.reassemble(ci, h, p);
    }

    /// Holds AT MOST ONE open message per channel, which is what bounds the
    /// buffer and what imposes the rule that a peer must not interleave two
    /// fragmented messages on one channel.
    fn reassemble(&mut self, ci: usize, mut h: Header, p: &[u8]) {
        if h.nfrag > wire::MAX_FRAGMENTS {
            self.chans[ci].abandon();
            self.drop_bad();
            return;
        }
        if self.chans[ci].rasm_active && h.corr != self.chans[ci].rasm_corr {
            // A new corr on a channel with one already open: the peer
            // interleaved, or the old one's remaining fragments are never
            // coming. Either way the old is dead.
            self.chans[ci].abandon();
        }
        if self.chans[ci].rasm_active && h.nfrag != self.chans[ci].rasm_nfrag {
            // The same corr describing a different message. Nothing here can
            // tell which of the two is real, so neither is.
            self.chans[ci].abandon();
            self.drop_bad();
            return;
        }
        {
            let c = &mut self.chans[ci];
            if !c.rasm_active {
                c.rasm_active = true;
                c.rasm_corr = h.corr;
                c.rasm_nfrag = h.nfrag;
                c.rasm_ty = h.ty;
                c.rasm_flags = h.flags;
                c.rasm_got = 0;
                for s in c.rasm_seen.iter_mut() {
                    *s = false;
                }
            }
            c.rasm_deadline = self.tick.wrapping_add(REASSEMBLY_TICKS);

            let i = h.frag as usize;
            if !c.rasm_seen[i] {
                c.rasm_seen[i] = true;
                c.rasm_got += 1;
            }
            c.rasm_part[i].clear();
            c.rasm_part[i].extend_from_slice(p);
            if c.rasm_got < c.rasm_nfrag {
                return;
            }
        }

        let mut asm = core::mem::take(&mut self.asm);
        asm.clear();
        for k in 0..self.chans[ci].rasm_nfrag as usize {
            let part = core::mem::take(&mut self.chans[ci].rasm_part[k]);
            asm.extend_from_slice(&part);
            self.chans[ci].rasm_part[k] = part;
        }
        h.ty = self.chans[ci].rasm_ty;
        h.flags = self.chans[ci].rasm_flags;
        h.corr = self.chans[ci].rasm_corr;
        self.chans[ci].abandon();
        self.dispatch(ci, h, &asm);
        self.asm = asm;
    }

    fn expire_reassembly(&mut self) {
        let tick = self.tick;
        for c in self.chans.iter_mut() {
            if c.rasm_active && (tick.wrapping_sub(c.rasm_deadline) as i32) >= 0 {
                c.abandon();
            }
        }
    }

    fn dispatch(&mut self, ci: usize, h: Header, payload: &[u8]) {
        match h.ty {
            Type::MSG => {
                if h.flags.has(Flags::SNAPSHOT) {
                    self.chans[ci].resync_sent = false;
                }
                if let Some(f) = self.chans[ci].on_message {
                    let id = self.chans[ci].id;
                    f(
                        self,
                        Message {
                            channel: id,
                            seq: h.seq,
                            snapshot: h.flags.has(Flags::SNAPSHOT),
                            payload,
                        },
                    );
                }
            }
            Type::RESYNC => {
                if let Some(f) = self.chans[ci].on_resync {
                    f(self);
                }
            }
            Type::REQ => self.on_req(ci, h, payload),
            Type::RESP => self.on_resp(ci, h, payload),
            _ => {}
        }
    }

    fn on_req(&mut self, ci: usize, h: Header, payload: &[u8]) {
        if let Some(e) = self.find_dedup(h.channel, h.corr) {
            self.stats.dup_hits += 1;
            if self.dedup[e].cached {
                let resp = core::mem::take(&mut self.dedup[e].resp);
                self.send_message(ci, Type::RESP, Flags::RETRY, h.corr, &resp);
                self.dedup[e].resp = resp;
                return;
            }
            // EXECUTED, AND THE RESULT IS GONE. Strictly better than the two
            // alternatives -- silently re-executing, or growing the save
            // without bound -- and the application can tell the difference.
            self.respond_error(ci, h.corr, wire::CODE_DUPLICATE, "result was not cached");
            return;
        }
        let handler = match self.chans[ci].on_request {
            Some(f) => f,
            None => {
                self.respond_error(ci, h.corr, wire::CODE_NO_HANDLER, "");
                return;
            }
        };
        let id = self.chans[ci].id;
        let out = handler(
            self,
            Request {
                channel: id,
                corr: Corr(h.corr),
                retry: h.flags.has(Flags::RETRY),
                payload,
            },
        );
        self.add_dedup(h.channel, h.corr, out);
        self.send_message(ci, Type::RESP, Flags::NONE, h.corr, out);
    }

    fn on_resp(&mut self, ci: usize, h: Header, payload: &[u8]) {
        let id = self.chans[ci].id;
        let pi = match self.find_pending(id, h.corr) {
            Some(pi) => pi,
            None => {
                // A response to a request that already completed, timed out, or
                // died with the session. Not an error -- it is what a retry
                // that crossed its own answer looks like.
                self.drop_stale();
                return;
            }
        };
        let mut body: &[u8] = payload;
        let mut err = None;
        if h.flags.has(Flags::ERROR) {
            match wire::control::decode_error_record_ref(payload) {
                Ok((code, msg)) => {
                    err = Some(ReplyError::Peer(PeerError { code }));
                    body = msg;
                }
                Err(_) => {
                    self.drop_bad();
                    return;
                }
            }
        }
        let cb = self.pend[pi].on_reply;
        self.free_pending(pi);
        if let Some(f) = cb {
            f(
                self,
                Reply {
                    corr: Corr(h.corr),
                    payload: body,
                    err,
                },
            );
        }
    }

    fn respond_error(&mut self, ci: usize, corr: u32, code: u16, msg: &str) {
        let mut ctl = core::mem::take(&mut self.ctl);
        ctl.clear();
        let built = wire::control::append_error_record(
            &mut ctl,
            &wire::ErrorRecord {
                code,
                message: alloc::string::String::from(msg),
            },
        );
        self.ctl = ctl;
        if built.is_err() {
            return;
        }
        self.send_ctl(ci, Type::RESP, Flags::ERROR, corr);
    }

    // -----------------------------------------------------------------------
    // Dedup: a ring in tick order, so expiry is a walk from the head.

    fn find_dedup(&self, ch: u16, corr: u32) -> Option<usize> {
        for i in 0..self.dedup_len {
            let at = (self.dedup_head + i) % self.dedup.len();
            let e = &self.dedup[at];
            if e.epoch == self.epoch && e.ch == ch && e.corr == corr {
                return Some(at);
            }
        }
        None
    }

    fn add_dedup(&mut self, ch: u16, corr: u32, resp: &[u8]) {
        if self.dedup_len == MAX_DEDUP {
            self.dedup_head = (self.dedup_head + 1) % self.dedup.len();
            self.dedup_len -= 1;
        }
        if self.dedup_len == self.dedup.len() {
            // Grow by REBUILDING in ring order rather than appending. Expiry
            // moves the head, so a full ring is not in general one whose head
            // is 0, and an appended slot would land where the arithmetic
            // expects a different entry -- silently answering one request's
            // retry with another's reply.
            let mut n = self.dedup.len() * 2 + 8;
            if n > MAX_DEDUP {
                n = MAX_DEDUP;
            }
            let mut grown = Vec::with_capacity(n);
            for i in 0..self.dedup_len {
                let at = (self.dedup_head + i) % self.dedup.len();
                grown.push(core::mem::replace(&mut self.dedup[at], DedupEntry::new()));
            }
            while grown.len() < n {
                grown.push(DedupEntry::new());
            }
            self.dedup = grown;
            self.dedup_head = 0;
        }
        let at = (self.dedup_head + self.dedup_len) % self.dedup.len();
        let epoch = self.epoch;
        let tick = self.tick;
        let e = &mut self.dedup[at];
        e.epoch = epoch;
        e.ch = ch;
        e.corr = corr;
        e.tick = tick;
        e.cached = resp.len() <= MAX_DEDUP_PAYLOAD;
        e.resp.clear();
        if e.cached {
            e.resp.extend_from_slice(resp);
        }
        self.dedup_len += 1;
    }

    fn expire_dedup(&mut self) {
        while self.dedup_len > 0 {
            let head = self.dedup_head;
            if self.tick.wrapping_sub(self.dedup[head].tick) < DEDUP_TICKS {
                return;
            }
            self.dedup[head].resp.clear();
            self.dedup_head = (self.dedup_head + 1) % self.dedup.len();
            self.dedup_len -= 1;
        }
    }

    // -----------------------------------------------------------------------
    // Channels. Every one of these is keyed by the wire id and reached through
    // an index, because a `&mut ChannelState` and a `&mut Link` cannot both be
    // live -- which is the Rust half of "the state machine is one object".

    pub(crate) fn find_chan(&self, id: u16) -> Option<usize> {
        for (i, c) in self.chans.iter().enumerate() {
            if c.id == id {
                return Some(i);
            }
            if c.id > id {
                return None;
            }
        }
        None
    }

    /// Names a channel, creating it the first time.
    ///
    /// 0 is the protocol's own; 1-65535 are the application's. Naming one twice
    /// returns the same channel and updates its priority, so registration order
    /// -- which is deterministic, being the same code on every peer -- decides
    /// nothing that reaches the wire.
    pub fn open_channel(&mut self, id: u16, pri: Priority) -> usize {
        if let Some(i) = self.find_chan(id) {
            self.chans[i].pri = pri;
            return i;
        }
        // Sorted insert. The order is deterministic either way -- a guest's
        // registrations are the same on every peer -- but sorted means a lookup
        // can stop early and a dump of the table reads the same as the wire.
        let mut at = self.chans.len();
        for (i, c) in self.chans.iter().enumerate() {
            if c.id > id {
                at = i;
                break;
            }
        }
        self.chans.insert(at, ChannelState::new(id, pri));
        at
    }

    pub fn set_on_message(&mut self, id: u16, h: MessageFn) {
        let i = self.open_channel_default(id);
        self.chans[i].on_message = Some(h);
    }

    pub fn set_on_request(&mut self, id: u16, h: RequestFn) {
        let i = self.open_channel_default(id);
        self.chans[i].on_request = Some(h);
    }

    pub fn set_on_resync(&mut self, id: u16, h: ResyncFn) {
        let i = self.open_channel_default(id);
        self.chans[i].on_resync = Some(h);
    }

    pub fn set_on_gap(&mut self, id: u16, h: GapFn) {
        let i = self.open_channel_default(id);
        self.chans[i].on_gap = Some(h);
    }

    fn open_channel_default(&mut self, id: u16) -> usize {
        match self.find_chan(id) {
            Some(i) => i,
            None => self.open_channel(id, Priority::Control),
        }
    }

    /// Queues one MSG.
    ///
    /// The payload is COPIED into the library's frame buffer before returning,
    /// so the caller may reuse its slice -- which is the point, because the
    /// shape a guest wants is one scratch buffer refilled every tick rather
    /// than an allocation per message in a heap that is in the save.
    ///
    /// With no peer this is a COUNTED NO-OP rather than an error to handle at
    /// every call site. The mod's own behaviour must be defined with no peer,
    /// and this is the library making that the easy path.
    pub fn send(&mut self, id: u16, payload: &[u8]) -> Status {
        self.send_flagged(id, payload, Flags::NONE)
    }

    /// [`Link::send`] with the SNAPSHOT flag: a complete state rather than a
    /// delta, which is the ONLY answer to a RESYNC.
    ///
    /// A gap is never answered with a replay, and the reason is not economy:
    /// the producer usually CANNOT replay, because the state it described no
    /// longer exists. A resend of "entity 4471 at 30% health" three seconds
    /// later is a lie, and a lie that arrives is worse than a gap that is
    /// noticed. There is no retransmit queue anywhere in this design.
    pub fn snapshot(&mut self, id: u16, payload: &[u8]) -> Status {
        self.send_flagged(id, payload, Flags::SNAPSHOT)
    }

    fn send_flagged(&mut self, id: u16, payload: &[u8], flags: Flags) -> Status {
        if !self.enabled {
            return self.refused();
        }
        if !self.up {
            self.stats.queue_drops += 1;
            return Status::NoSession;
        }
        let ci = match self.find_chan(id) {
            Some(ci) => ci,
            None => return Status::NotOpen,
        };
        self.send_message(ci, Type::MSG, flags, 0, payload)
    }

    /// Queues a REQ and registers the completion.
    ///
    /// THERE ARE NO THREADS AND NO FUTURES ON THIS TARGET, so every
    /// asynchronous result in FkLua arrives as a callback from a dispatch and
    /// this is no different. `on_reply` is called from inside a later
    /// [`Link::pump`] -- with the answer, with [`ReplyError::Timeout`] when the
    /// retry budget runs out, or with [`ReplyError::SessionLost`] if the
    /// session ends first, which means THE OUTCOME IS UNKNOWN rather than "it
    /// failed".
    ///
    /// The same corr is retried on the schedule; the responder keys its dedup
    /// table on (epoch, channel, corr) and replays rather than re-invoking its
    /// handler. So a request must be IDEMPOTENT in the sense that asking twice
    /// is safe, which is the whole bargain this protocol asks for.
    pub fn request(
        &mut self,
        id: u16,
        payload: &[u8],
        on_reply: Option<ReplyFn>,
    ) -> Result<Corr, Status> {
        if !self.enabled {
            return Err(self.refused());
        }
        if !self.up {
            self.stats.queue_drops += 1;
            return Err(Status::NoSession);
        }
        let ci = match self.find_chan(id) {
            Some(ci) => ci,
            None => return Err(Status::NotOpen),
        };
        let pi = match self.alloc_pending() {
            Some(pi) => pi,
            None => return Err(Status::TooManyPending),
        };
        let corr = self.next_corr();
        {
            let p = &mut self.pend[pi];
            p.ch = id;
            p.corr = corr;
            p.on_reply = on_reply;
            p.msg.clear();
            p.msg.extend_from_slice(payload);
            p.tries = 0;
        }
        let st = self.send_message(ci, Type::REQ, Flags::NONE, corr, payload);
        if st != Status::Ok {
            self.free_pending(pi);
            return Err(st);
        }
        let iv = self.retry_ticks();
        self.pend[pi].interval = iv;
        self.pend[pi].due = self.tick.wrapping_add(iv);
        Ok(Corr(corr))
    }

    /// Writes `data` to `script-output/<name>` and sends a FILE_NOTIFY on the
    /// channel, with a length and an FNV-1a-32 the peer can verify exactly.
    ///
    /// PREFER IT TO A FRAGMENTED MESSAGE FOR ANYTHING ABOVE ONE FRAME. It is
    /// one datagram instead of sixteen, and the transport is localhost-only, so
    /// the peer is always on this filesystem -- the file path is ALWAYS
    /// available outbound. It is also the only path for a screenshot, which the
    /// engine writes to script-output and raises no completion event for.
    ///
    /// The notify is a MSG-class frame: seq'd, gap-detectable, and NOT retried.
    /// The file is durable, so a lost notify is recoverable by a RESYNC or by
    /// the peer scanning the directory; retrying it would be retrying a claim
    /// about a file that may since have been overwritten.
    pub fn write_bulk(&mut self, id: u16, name: &str, data: &[u8]) -> Status {
        // BEFORE THE WRITE, WHICH IS THE ONE PLACE THIS ORDER IS INTERESTING.
        // The file write is a per-instance side effect a guest may make
        // anywhere and the notify is replicated bookkeeping, so the rule is
        // normally "do both, never branch between them". A disabled link does
        // NEITHER: the notify can never be sent, and a file whose announcement
        // is impossible is a file the peer -- which is not running, on an
        // engine with no working IPC -- will never hear about. Refusing both
        // keeps the pair together, which is the invariant the rule is really
        // about.
        if !self.enabled {
            return self.refused();
        }
        if !self.up {
            self.stats.queue_drops += 1;
            return Status::NoSession;
        }
        // NoTransport rather than NotOpen: during a poll the transport is out
        // of the link (see pump_begin), and a handler calling this from inside
        // an inbound dispatch lands here. It is a real limitation and it is
        // loud rather than silent.
        let mut tr = match self.tr.take() {
            Some(t) => t,
            None => return Status::NoTransport,
        };
        // ATTEMPT, THEN NOTIFY, UNCONDITIONALLY -- and the seam makes that the
        // only thing this can do, because `Transport::write_file` returns `()`.
        //
        // `write_bulk` IS THE PATTERN FOR A PER-INSTANCE SIDE EFFECT, and it is
        // worth copying rather than only reading. Whether write_file works is a
        // fact about this peer: 2.1 documents a non-zero `for_player` as
        // silently skipped from some stages, and a client is not the server.
        // Branching on it would not merely miscount -- returning early here
        // would SKIP the notify, which consumes this channel's seq, so one peer
        // would advance the counter and the other would not. That is guest
        // state diverging AND a permanent gap at the far end, from one `?`.
        //
        // So the shape is: do the local thing, then do the replicated
        // bookkeeping with no edge between them. A guest that wants to know
        // whether its own write landed asks `fk::log`, which writes to the game
        // log -- which is not CRC'd, is per-peer by nature, and is where a
        // per-peer fact belongs.
        tr.write_file(name, data);
        self.tr = Some(tr);
        self.notify(
            id,
            wire::FileNotify {
                bytes: data.len() as u32,
                fnv1a32: wire::fnv1a32(data),
                name: alloc::string::String::from(name),
            },
            Flags::HAS_DIGEST,
        )
    }

    /// Announces a file this guest did NOT write -- a screenshot.
    ///
    /// No digest, because the guest has never held the bytes and cannot
    /// describe them, so the peer must stabilize-poll: size unchanged across
    /// two polls. Nothing documents a flush guarantee for the engine's own
    /// writes either, which is why the peer's test is a test rather than a
    /// promise.
    ///
    /// IT IS THE SAME PATTERN AS [`Link::write_bulk`] AND IT IS WORTH SEEING
    /// WHY. The thing that produced the file -- `take_screenshot`, say -- is a
    /// per-instance side effect whose success is a fact about this peer, and it
    /// is made from wherever the guest wants; the notify is replicated
    /// bookkeeping and consumes this channel's seq. Nothing may sit between
    /// them, so the guest calls the one and then unconditionally calls the
    /// other. Whether the screenshot happened is a question for `fk::log` and
    /// the game's own log, which is not CRC'd.
    pub fn notify_file(&mut self, id: u16, name: &str) -> Status {
        if !self.enabled {
            return self.refused();
        }
        if !self.up {
            self.stats.queue_drops += 1;
            return Status::NoSession;
        }
        self.notify(
            id,
            wire::FileNotify {
                bytes: 0,
                fnv1a32: 0,
                name: alloc::string::String::from(name),
            },
            Flags::NONE,
        )
    }

    fn notify(&mut self, id: u16, fnote: wire::FileNotify, flags: Flags) -> Status {
        let ci = match self.find_chan(id) {
            Some(ci) => ci,
            None => return Status::NotOpen,
        };
        let mut ctl = core::mem::take(&mut self.ctl);
        ctl.clear();
        let built = wire::control::append_file_notify(&mut ctl, &fnote);
        self.ctl = ctl;
        if built.is_err() {
            return Status::TooLarge;
        }
        self.send_ctl(ci, Type::FILE_NOTIFY, flags, 0)
    }
}

impl Default for Link {
    fn default() -> Self {
        Link::new()
    }
}

/// Turns whatever a peer or a config said into something sendable.
///
/// A zero is "no opinion". Below [`wire::MIN_MAX_FRAME`] is a peer that is
/// confused or hostile, and clamping up is kinder than fragmenting a heartbeat.
/// Above the ceiling is refused whoever asked for it: the ceiling clears the
/// inbound datagram wall, the host's string scratch and every OS's limit, and
/// the transport reports none of those.
pub(crate) fn clamp_frame(v: u16) -> u16 {
    if v == 0 {
        return wire::DEFAULT_MAX_FRAME;
    }
    if v < wire::MIN_MAX_FRAME {
        return wire::MIN_MAX_FRAME;
    }
    if v > wire::MAX_FRAME_CEILING {
        return wire::MAX_FRAME_CEILING;
    }
    v
}
