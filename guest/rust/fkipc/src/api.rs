//! The public value types: what a guest configures, what it is answered with,
//! and what its handlers are handed.

use crate::version::Version;
use crate::wire;

/// Which of the two driving shapes this guest is, and it sets three defaults
/// that invert each other.
///
/// THE SERVER PROFILE (Clusterio-shaped): a headless server, `for_player = 0`,
/// one authoritative peer injecting. Telemetry-dominated, and its binding
/// constraint is the ~6 kB/s inbound wall once even one player is connected --
/// so inbound is a control channel, never a data channel.
///
/// THE INTERACTIVE-AGENT PROFILE (Vibetorio-shaped): a graphical client, single
/// player, a model in a side process. The walls move: UDP receive is nearly
/// free because there is no replication fan-out, the latency floor is ~1 tick
/// rather than ~6, and PAUSE is the hazard -- the pump stops, the OS buffer
/// fills silently, and the guest must drain on resume.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub enum Profile {
    #[default]
    Server,
    Client,
}

/// What a guest tells [`crate::open`]. Every field has a working default except
/// the port.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Config {
    /// The PEER's port, and it must differ from `--enable-lua-udp`'s.
    ///
    /// That flag binds ONE socket, which is both the game's receive socket and
    /// the source port of everything it sends, so a companion on the same port
    /// is the game talking to itself.
    pub port: u16,

    pub profile: Profile,

    /// Selects whose copy of a send actually goes out:
    ///
    /// ```text
    ///  0  the server        -- Profile::Server's default, measured working headless
    /// -1  omit the argument -- Profile::Client's default
    ///  N  player N
    /// ```
    ///
    /// The polarity is measured rather than assumed, and it is not symmetric.
    /// `for_player = 0` works on a headless server and is a SILENT NO-OP under
    /// `--benchmark`, where no server exists; omitting it works under
    /// benchmark; `for_player = 1` with no such player is a silent no-op with
    /// no error. The same rule governs `write_file`.
    ///
    /// `Profile::Client` with this left at 0 omits the argument instead,
    /// because a graphical client has no server to send FOR and 0 there is the
    /// silent no-op. Set it explicitly to override.
    pub for_player: i32,

    /// What this guest will ACCEPT, carried in HELLO; 0 means
    /// [`wire::DEFAULT_MAX_FRAME`].
    ///
    /// It is a budget shared with the application: the host's string scratch
    /// region is reset once per OUTERMOST dispatch, so an inbound payload holds
    /// its own length for the whole handler, and every host call the handler
    /// makes takes its string returns from above that point. A guest that reads
    /// entity names from inside a message handler wants a smaller frame than
    /// one that only decodes.
    pub max_frame: u16,

    /// This guest's IDENTITY TOKEN, carried in HELLO. It is the peer's log
    /// line, and -- when the peer sets its own expected name -- the thing the
    /// peer checks before it will answer at all.
    ///
    /// THE RECOMMENDED CONVENTION IS `"<mod-name>/<schema-tag>"`, where the tag
    /// is the author's claim about CHANNEL-CONTRACT compatibility: a
    /// design-time UUID or a version string, bumped when the meaning of a
    /// channel changes. Deliberately NOT FkLua's per-build id, which moves on
    /// every rebuild and would turn a correctness check into a nuisance.
    ///
    /// `&'static str` rather than an owned String: a Config is built in
    /// `_initialize` from a literal, and an allocation there would be one more
    /// thing in the save for no reason. It also states for free the rule the Go
    /// half has to state in prose -- a token must be a BUILD-TIME CONSTANT,
    /// because anything computed at run time would have to be a deterministic
    /// function of guest state (the theorem that stops a guest minting its own
    /// epoch), and anything that was not deterministic would be a desync.
    pub name: &'static str,

    /// The identity token this guest requires of its COMPANION, and empty means
    /// NO CHECK -- which is what every guest written before this existed gets,
    /// unchanged.
    ///
    /// Set it and a HELLO_ACK whose name differs is refused: the token is not
    /// adopted, [`Stats::name_rejects`] counts it, and the link stays peerless
    /// and goes on searching at the ordinary cadence. That closes the one gap
    /// the source-port filter cannot, because the port filter answers "is this
    /// frame from the process I was pointed at" and this answers "is the process
    /// I was pointed at the one I was BUILT against" -- a swapped port config or
    /// a companion left running from last week is a SUCCESSFUL transport
    /// handshake between two ends that disagree about what the channels mean.
    ///
    /// THE CHECK IS LEGAL BECAUSE THE ACK IS REPLICATED. It arrives through
    /// `recv_udp`, which enters game state as an InputAction, so every peer sees
    /// the same ACK at the same tick; refusing it, counting the refusal and
    /// carrying on searching are therefore identical on every peer. What a check
    /// here may never do is branch on anything PEER-LOCAL -- see
    /// [`crate::Link::reload`].
    ///
    /// The usual shape is one token per pairing: set `name` and `expect_peer`
    /// to the same string here and `Name` and `ExpectedName` to that string on
    /// the SDK side, because the token names the CONTRACT rather than either
    /// party. As a convenience, a guest that sets only `expect_peer` sends it
    /// as its `name` too.
    ///
    /// IT IS A CORRECTNESS CHECK AND NOT AN AUTH BOUNDARY. The token is a
    /// constant in a mod zip anybody can read, and the transport is localhost.
    /// See `agents/ipc.md`.
    pub expect_peer: &'static str,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            port: 0,
            profile: Profile::Server,
            for_player: 0,
            max_frame: 0,
            name: "",
            expect_peer: "",
        }
    }
}

/// What the outbound half of this API answers with.
///
/// `Status::NoSession` is deliberately NOT an error to handle at every call
/// site: a guest whose peer is down must keep playing, so `send` becomes a
/// counted no-op rather than something the mod has to branch on. The counter is
/// in [`Stats`].
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub enum Status {
    #[default]
    Ok,
    NotOpen,
    NoSession,
    TooLarge,
    QueueFull,
    TooManyPending,
    BadConfig,
    NoTransport,
    /// The running engine is below [`crate::MIN_ENGINE_VERSION`], so this link
    /// is inert for the whole session. APPENDED rather than inserted, so every
    /// variant above keeps the discriminant it had.
    ///
    /// It is a DETERMINISTIC refusal like the rest of this enum -- the engine
    /// version is identical on every peer in a multiplayer game, because
    /// Factorio will not connect two different builds -- which is what makes it
    /// legal for a guest to branch on and store.
    ///
    /// NOT `NoSession` AND NOT `NotOpen`, and the choice is about what a mod
    /// author does next. `NoSession` is the QUIESCE shape and means "the peer
    /// is down, it may be back this second" -- transient, and here the session
    /// can never come up, because an engine cannot change under a running game.
    /// `NotOpen` means "you did not call `open`", a programming mistake fixed
    /// in source; here the author did everything right and the ENGINE is what
    /// is wrong. It keeps the counted-no-op property either way: a `Status` is
    /// not an error a mod must branch on.
    Disabled,
}

impl Status {
    pub fn as_str(self) -> &'static str {
        match self {
            Status::Ok => "ok",
            Status::NotOpen => "fkipc: open has not been called",
            Status::NoSession => "fkipc: no peer -- the send was counted and dropped",
            Status::TooLarge => "fkipc: larger than the negotiated message ceiling",
            Status::QueueFull => "fkipc: the send queue is full",
            Status::TooManyPending => "fkipc: too many requests in flight",
            Status::BadConfig => "fkipc: bad configuration",
            Status::NoTransport => "fkipc: no transport",
            Status::Disabled => "fkipc: disabled -- this engine is too old",
        }
    }
}

/// A property of a CHANNEL, deliberately not a wire field: the receiver never
/// needs it, and a field the receiver ignores is a field that eventually
/// disagrees with the sender's behaviour. It decides only which of this guest's
/// own queued frames leaves first when the per-tick send budget binds.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub enum Priority {
    #[default]
    Control,
    Bulk,
}

/// A correlation id: the key that ties a RESP to its REQ, and the key a
/// responder dedups on.
///
/// MINTED FROM A COUNTER, never from randomness. Determinism, and it also makes
/// the dedup window's arithmetic trivial.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Corr(pub u32);

/// What an `on_session` handler reports.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum SessionEvent {
    /// A HELLO_ACK arrived and its token is now the epoch.
    Up,
    /// Nothing has been heard for `LIVENESS_TICKS`, the guest's own clock went
    /// backwards, or the peer said BYE. Pending requests have already failed
    /// with [`ReplyError::SessionLost`] and the send queue has already been
    /// dropped. A fresh HELLO goes out on the next pump.
    Down,
    /// NEVER RAISED any more, and the variant is kept rather than deleted for
    /// two reasons: the discriminants behind it would move, and a downstream
    /// `match ev` that still names it must go on compiling.
    ///
    /// It meant "`reload` ran", and [`crate::reload`] now does nothing -- a
    /// load is not a session boundary, because `fk_after_load` fires on a
    /// joining multiplayer client and on no other peer. Everything it used to
    /// signal arrives as [`SessionEvent::Down`] now, from a replicated signal.
    Reset,
}

impl SessionEvent {
    pub fn as_str(self) -> &'static str {
        match self {
            SessionEvent::Up => "up",
            SessionEvent::Down => "down",
            SessionEvent::Reset => "reset",
        }
    }
}

/// One inbound MSG.
///
/// `payload` BORROWS this crate's receive buffer and the borrow ends with the
/// handler. Copy what you keep -- and here, unlike in the Go half, the compiler
/// says so.
#[derive(Clone, Copy, Debug)]
pub struct Message<'a> {
    pub channel: u16,
    pub seq: u32,
    pub snapshot: bool,
    pub payload: &'a [u8],
}

/// One inbound REQ.
///
/// `payload` has the same lifetime rule as [`Message::payload`]; the handler's
/// RETURN value is copied by this crate before it goes on the wire, so a
/// handler may return the payload it was given.
#[derive(Clone, Copy, Debug)]
pub struct Request<'a> {
    pub channel: u16,
    pub corr: Corr,
    pub retry: bool,
    pub payload: &'a [u8],
}

/// Why a [`Reply`] carries no result.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ReplyError {
    /// The retry budget was exhausted with no answer.
    Timeout,
    /// NOT "the request failed" -- "THE OUTCOME IS UNKNOWN", and the
    /// distinction is the whole reason this variant exists.
    ///
    /// A save may predate a response the peer already sent, or predate the peer
    /// EXECUTING the request and not yet replying. Retrying it into the new
    /// session would re-execute it outside the dedup window, which is precisely
    /// the guarantee corr-based dedup exists to provide. So it is never
    /// retried: the application re-derives or re-asks, and that is what
    /// "idempotent RPC" buys.
    SessionLost,
    /// The peer answered with an error record.
    Peer(PeerError),
}

impl ReplyError {
    pub fn as_str(self) -> &'static str {
        match self {
            ReplyError::Timeout => "fkipc: the peer did not answer",
            ReplyError::SessionLost => "fkipc: the session ended before the outcome was known",
            ReplyError::Peer(_) => "fkipc: the peer answered with an error record",
        }
    }
}

/// A RESP that carried the ERROR flag.
///
/// The Go half spells this `{Code uint16; Message string}`. Here the message
/// travels as the reply's own `payload` instead, still borrowing the receive
/// buffer -- because a `Reply` reaches a `fn` pointer and an owned `String`
/// would be an allocation on every failed request, in a heap that is in the
/// save. The pair is the same pair; only its ownership differs.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct PeerError {
    pub code: u16,
}

impl PeerError {
    /// The one error code an application must usually handle: the request
    /// EXECUTED and its result is no longer cached, because it was larger than
    /// `MAX_DEDUP_PAYLOAD`. Re-sending it would execute it twice.
    pub fn duplicate(self) -> bool {
        self.code == wire::CODE_DUPLICATE
    }
}

/// The completion of a request.
///
/// `payload` is a view with the usual lifetime: the RESP's result when `err` is
/// `None`, the peer's error MESSAGE when it is `Some(ReplyError::Peer(_))`, and
/// empty otherwise.
#[derive(Clone, Copy, Debug)]
pub struct Reply<'a> {
    pub corr: Corr,
    pub payload: &'a [u8],
    pub err: Option<ReplyError>,
}

/// The observability surface.
///
/// Everything here is a counter over the life of the guest's memory, so it
/// survives a save like the rest of it.
///
/// AND THEREFORE EVERY FIELD IS A DETERMINISTIC FUNCTION OF REPLICATED INPUT.
/// That is a constraint on what may be counted here, not an observation: guest
/// memory is CRC'd across every peer, so a counter that moved on one peer only
/// is a desync. What is countable is inbound (replicated), the tick
/// (replicated), build-time configuration (identical builds), and this link's
/// own decisions. What is NOT is whether an outbound host call worked -- see
/// `queue_drops` below, and the join-safety contract in the crate doc. A guest
/// that wants that answer logs it with `fk::log`, which goes to the game log,
/// which is not CRC'd.
///
/// THE TYPE IS `Stats` AND THE FUNCTION IS `stats()`, which is the one place
/// the Rust mirror does not follow the Go half's spelling: a Go package cannot
/// hold a function and a type of one name, so it says `Stats() LinkStats`; Rust
/// has no collision and keeps both.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Stats {
    pub epoch: u32,
    pub up: bool,

    pub tx_frames: u32,
    pub tx_bytes: u32,
    pub rx_frames: u32,
    pub rx_bytes: u32,

    /// Every frame this guest refused: bad magic, a version it does not speak,
    /// an unknown type, a length disagreeing with the datagram, an epoch it
    /// does not recognise, or a channel nobody registered.
    pub drops: u32,
    /// `bad_frames`, `epoch_drops` and `stale_drops` split [`Stats::drops`] by
    /// cause, because "junk on a shared local port" and "a peer still talking
    /// to the session before this one" are different problems with different
    /// fixes.
    pub bad_frames: u32,
    pub epoch_drops: u32,
    pub stale_drops: u32,

    /// Datagrams that arrived from a port that is not [`crate::Config::port`]
    /// -- almost always ANOTHER IPC MOD'S companion, because
    /// `--enable-lua-udp` binds one socket for the whole game and every mod is
    /// handed every mod's inbound traffic.
    ///
    /// A steady non-zero value is normal and healthy on a machine running two
    /// IPC mods: it is this filter doing its job. It is only a diagnosis when
    /// it rises while [`Stats::rx_frames`] stays at zero, which says the
    /// companion is sending from a port this guest was not configured for.
    pub foreign_drops: u32,

    /// HELLO_ACKs that came from the configured port, matched the outstanding
    /// HELLO's corr, decoded cleanly, and carried an identity token that is not
    /// [`crate::Config::expect_peer`].
    ///
    /// It is the counter for the failure the source-port filter cannot see: the
    /// transport handshake SUCCEEDED and the two ends disagree about what the
    /// channels mean. A rising value with [`Stats::up`] false is the whole
    /// diagnosis -- something IS answering on this port and it is not what this
    /// guest was built against -- where without it that state is
    /// indistinguishable from a companion nobody started.
    ///
    /// It is a function of REPLICATED inbound and of build-time configuration,
    /// so it holds the same value on every peer.
    pub name_rejects: u32,

    pub gaps: u32,
    pub retries: u32,
    pub timeouts: u32,
    pub dup_hits: u32,
    pub queue_depth: u32,
    /// About THIS LINK'S OWN DECISIONS -- a send with no session, a message
    /// that did not fit the queue -- and never about whether a datagram reached
    /// the socket. That distinction is a correctness property rather than
    /// tidiness: whether `send_udp` works depends on whether this peer was
    /// started with `--enable-lua-udp`, which a joining client is not, and a
    /// counter that moved on one peer only is a desync. See
    /// [`crate::Link::raw_send_slice`].
    pub queue_drops: u32,

    /// Inbound frames that CANNOT have fitted the host's 4 KiB string scratch
    /// region and therefore came through `fk_alloc`.
    ///
    /// It is a FLOOR and not a total, and saying so is the point: the region is
    /// shared with everything else the dispatch marshals, so a frame below the
    /// region size may still have fallen back if the dispatch had already
    /// consumed some. A non-zero value on a live session means the negotiated
    /// frame size is wrong for what the handler does.
    pub scratch_overflows: u32,

    /// Everything the ENGINE GATE turned away: one per pump that did nothing,
    /// and one per API call answered with [`Status::Disabled`]. A non-zero
    /// value is the whole diagnosis on its own -- this mod is running on an
    /// engine below [`crate::MIN_ENGINE_VERSION`] and is inert -- and pumps
    /// dominate it, so read it as "how long has it been like this" rather than
    /// as a call count.
    ///
    /// It replaces `recv_refused`, which counted pumps that skipped `recv_udp`
    /// while the rest of the link went on running. There is no such state any
    /// more.
    pub refusals: u32,
    /// What the engine gate read from `helpers.game_version`. Re-read once a
    /// second while the gate is SHUT and never once it is open, because a save
    /// can move to a newer engine and an engine cannot move under a running
    /// game.
    pub base_version: Version,
    /// The gate's verdict: `true` when this engine is at or above
    /// [`crate::MIN_ENGINE_VERSION`] and the link is live. `false` means inert
    /// -- no HELLO, no heartbeat, no poll, no datagram of any kind.
    pub enabled: bool,

    /// THE SESSION GENERATION: how many sessions this guest has ended. It goes
    /// up at a session boundary -- a BYE, or liveness -- and NOT on a load,
    /// which is the change the multiplayer-join fix made. A load is not a
    /// boundary, because the only signal a load has is peer-local.
    ///
    /// It rides in HELLO's `boot` field and the peer must still never compare
    /// it: it lives in guest memory, so it time-travels with the save and
    /// aliases across two loads of one save.
    pub boot: u32,
}

impl Stats {
    pub(crate) const fn new() -> Stats {
        Stats {
            epoch: 0,
            up: false,
            tx_frames: 0,
            tx_bytes: 0,
            rx_frames: 0,
            rx_bytes: 0,
            drops: 0,
            bad_frames: 0,
            epoch_drops: 0,
            stale_drops: 0,
            foreign_drops: 0,
            name_rejects: 0,
            gaps: 0,
            retries: 0,
            timeouts: 0,
            dup_hits: 0,
            queue_depth: 0,
            queue_drops: 0,
            scratch_overflows: 0,
            refusals: 0,
            base_version: Version::ZERO,
            enabled: false,
            boot: 0,
        }
    }
}
