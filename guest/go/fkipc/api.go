package fkipc

import "github.com/Techrocket9/fklua/guest/go/fkipc/wire"

// Profile is which of the two driving shapes this guest is, and it sets three
// defaults that invert each other.
//
// THE SERVER PROFILE (Clusterio-shaped): a headless server, for_player = 0, one
// authoritative peer injecting. Telemetry-dominated, and its binding constraint
// is the ~6 kB/s inbound wall once even one player is connected -- so inbound
// is a control channel, never a data channel.
//
// THE INTERACTIVE-AGENT PROFILE (Vibetorio-shaped): a graphical client, single
// player, a model in a side process. The walls move: UDP receive is nearly free
// because there is no replication fan-out, the latency floor is ~1 tick rather
// than ~6, and PAUSE is the hazard -- the pump stops, the OS buffer fills
// silently, and the guest must drain on resume.
type Profile uint8

const (
	ProfileServer Profile = 0
	ProfileClient Profile = 1
)

// Config is what a guest tells Open. Every field has a working default except
// the port.
type Config struct {
	// Port is the PEER's port, and it must differ from --enable-lua-udp's.
	// That flag binds ONE socket, which is both the game's receive socket and
	// the source port of everything it sends, so a companion on the same port
	// is the game talking to itself.
	Port uint16

	Profile Profile

	// ForPlayer selects whose copy of a send actually goes out:
	//
	//	 0  the server        -- ProfileServer's default, measured working headless
	//	-1  omit the argument -- ProfileClient's default
	//	 N  player N
	//
	// The polarity is measured rather than assumed, and it is not symmetric.
	// for_player = 0 works on a headless server and is a SILENT NO-OP under
	// --benchmark, where no server exists; omitting it works under benchmark;
	// for_player = 1 with no such player is a silent no-op with no error. The
	// same rule governs write_file.
	//
	// ProfileClient with this left at 0 omits the argument instead, because a
	// graphical client has no server to send FOR and 0 there is the silent
	// no-op. Set it explicitly to override.
	ForPlayer int32

	// MaxFrame is what this guest will ACCEPT, carried in HELLO; 0 means
	// DefaultMaxFrame. It is a budget shared with the application: the host's
	// string scratch region is reset once per OUTERMOST dispatch, so an inbound
	// payload holds its own length for the whole handler, and every host call
	// the handler makes takes its string returns from above that point. A guest
	// that reads entity names from inside a message handler wants a smaller
	// frame than one that only decodes.
	MaxFrame uint16

	// Name is this guest's IDENTITY TOKEN, carried in HELLO. It is the peer's
	// log line, and -- when the peer sets its own ExpectedName -- the thing the
	// peer checks before it will answer at all.
	//
	// THE RECOMMENDED CONVENTION IS "<mod-name>/<schema-tag>", where the tag is
	// the author's claim about CHANNEL-CONTRACT compatibility: a design-time
	// UUID or a version string, bumped when the meaning of a channel changes.
	// Deliberately NOT FkLua's per-build id, which moves on every rebuild and
	// would turn a correctness check into a nuisance -- the question is whether
	// the two ends agree about what channel 1 means, not whether they were
	// compiled on the same afternoon.
	//
	// IT MUST BE A BUILD-TIME CONSTANT. Anything computed at run time would
	// have to be a deterministic function of guest state, which is the same
	// theorem that stops a guest minting its own epoch -- and anything that was
	// not deterministic would be a desync.
	Name string

	// ExpectPeer is the identity token this guest requires of its COMPANION,
	// and empty means NO CHECK -- which is what every guest written before this
	// existed gets, unchanged.
	//
	// Set it and a HELLO_ACK whose name differs is refused: the token is not
	// adopted, [LinkStats.NameRejects] counts it, and the link stays peerless
	// and goes on searching at the ordinary cadence. That closes the one gap
	// the source-port filter cannot, because the port filter answers "is this
	// frame from the process I was pointed at" and this answers "is the process
	// I was pointed at the one I was BUILT against" -- a swapped port config or
	// a companion left running from last week is a SUCCESSFUL transport
	// handshake between two ends that disagree about what the channels mean.
	//
	// THE CHECK IS LEGAL BECAUSE THE ACK IS REPLICATED. It arrives through
	// recv_udp, which enters game state as an InputAction, so every peer sees
	// the same ACK at the same tick; refusing it, counting the refusal and
	// carrying on searching are therefore identical on every peer. What a check
	// here may never do is branch on anything PEER-LOCAL -- see [Reload].
	//
	// The usual shape is one token per pairing: set Name and ExpectPeer to the
	// same string here and Name and ExpectedName to that string on the SDK
	// side, because the token names the CONTRACT rather than either party. As a
	// convenience, a guest that sets only ExpectPeer sends it as its Name too.
	//
	// IT IS A CORRECTNESS CHECK AND NOT AN AUTH BOUNDARY. The token is a
	// constant in a mod zip anybody can read, and the transport is localhost.
	// See agents/ipc.md.
	ExpectPeer string
}

// Status is what the outbound half of this API answers with.
//
// StatusNoSession is deliberately NOT an error to handle at every call site: a
// guest whose peer is down must keep playing, so Send becomes a counted no-op
// rather than something the mod has to branch on. The counter is in Stats.
type Status uint8

const (
	StatusOK Status = iota
	StatusNotOpen
	StatusNoSession
	StatusTooLarge
	StatusQueueFull
	StatusTooManyPending
	StatusBadConfig
	StatusNoTransport
	// StatusDisabled: the running engine is below [MinEngineVersion], so this
	// link is inert for the whole session. APPENDED rather than inserted, so
	// every value above keeps the number it had.
	//
	// It is a DETERMINISTIC refusal like the rest of this list -- the engine
	// version is identical on every peer in a multiplayer game, because
	// Factorio will not connect two different builds -- which is what makes it
	// legal for a guest to branch on and store. See [Link.refused] for why it
	// is not StatusNoSession.
	StatusDisabled
)

func (s Status) Error() string {
	switch s {
	case StatusOK:
		return ""
	case StatusNotOpen:
		return "fkipc: Open has not been called"
	case StatusNoSession:
		return "fkipc: no peer -- the send was counted and dropped"
	case StatusTooLarge:
		return "fkipc: larger than the negotiated message ceiling"
	case StatusQueueFull:
		return "fkipc: the send queue is full"
	case StatusTooManyPending:
		return "fkipc: too many requests in flight"
	case StatusBadConfig:
		return "fkipc: bad configuration"
	case StatusNoTransport:
		return "fkipc: no transport"
	case StatusDisabled:
		return "fkipc: disabled -- this engine is older than " +
			MinEngineVersion.String()
	}
	return "fkipc: unknown status"
}

func (s Status) String() string {
	if s == StatusOK {
		return "ok"
	}
	return s.Error()
}

// Priority is a property of a CHANNEL and is deliberately not a wire field: the
// receiver never needs it, and a field the receiver ignores is a field that
// eventually disagrees with the sender's behaviour. It decides only which of
// this guest's own queued frames leaves first when the per-tick send budget
// binds.
type Priority uint8

const (
	PriControl Priority = 0
	PriBulk    Priority = 1
)

// Corr is a correlation id: the key that ties a RESP to its REQ, and the key a
// responder dedups on.
//
// MINTED FROM A COUNTER, never from randomness. Determinism, and it also makes
// the dedup window's arithmetic trivial.
type Corr uint32

// SessionEvent is what OnSession reports.
type SessionEvent uint8

const (
	// SessionUp: a HELLO_ACK arrived and its token is now the epoch.
	SessionUp SessionEvent = iota
	// SessionDown: nothing has been heard for LivenessTicks, the guest's own
	// clock went backwards, or the peer said BYE. Pending requests have already
	// failed with ErrSessionLost and the send queue has already been dropped.
	// A fresh HELLO goes out on the next pump.
	SessionDown
	// SessionReset is NEVER RAISED any more, and the constant is kept rather
	// than deleted for two reasons: the numbering behind it would move, and a
	// downstream `switch ev` that still names it must go on compiling.
	//
	// It meant "Reload ran", and Reload now does nothing -- a load is not a
	// session boundary, because fk_after_load fires on a joining multiplayer
	// client and on no other peer. Everything it used to signal arrives as
	// SessionDown now, from a replicated signal. See [Reload].
	SessionReset
)

func (e SessionEvent) String() string {
	switch e {
	case SessionUp:
		return "up"
	case SessionDown:
		return "down"
	case SessionReset:
		return "reset"
	}
	return "?"
}

// Message is one inbound MSG.
//
// Payload is A VIEW into this package's receive buffer and is invalid the
// moment the handler returns. Copy what you keep.
type Message struct {
	Channel  uint16
	Seq      uint32
	Snapshot bool
	Payload  []byte
}

// Request is one inbound REQ. Payload has the same lifetime rule as
// Message.Payload; the handler's RETURN value is copied by this package before
// it goes on the wire, so a handler may return a slice it is about to reuse.
type Request struct {
	Channel uint16
	Corr    Corr
	Retry   bool
	Payload []byte
}

// Reply is the completion of a Request.
//
// Err is nil, [ErrTimeout], [ErrSessionLost], or a *[PeerError] carrying the
// peer's own error record. Payload is a view with the usual lifetime.
type Reply struct {
	Corr    Corr
	Payload []byte
	Err     error
}

type errorString string

func (e errorString) Error() string { return string(e) }

const (
	// ErrTimeout: the retry budget was exhausted with no answer.
	ErrTimeout = errorString("fkipc: the peer did not answer")

	// ErrSessionLost is NOT "the request failed" -- it is "THE OUTCOME IS
	// UNKNOWN", and the distinction is the whole reason this error exists.
	//
	// A save may predate a response the peer already sent, or predate the peer
	// EXECUTING the request and not yet replying. Retrying it into the new
	// session would re-execute it outside the dedup window, which is precisely
	// the guarantee corr-based dedup exists to provide. So it is never retried:
	// the application re-derives or re-asks, and that is what "idempotent RPC"
	// buys.
	ErrSessionLost = errorString("fkipc: the session ended before the outcome was known")
)

// PeerError is a RESP that carried the ERROR flag.
type PeerError struct {
	Code    uint16
	Message string
}

func (e *PeerError) Error() string {
	return "fkipc: peer error " + itoa(uint32(e.Code)) + ": " + e.Message
}

// Duplicate reports the one error code an application must usually handle: the
// request EXECUTED and its result is no longer cached, because it was larger
// than MaxDedupPayload. Re-sending it would execute it twice.
func (e *PeerError) Duplicate() bool { return e.Code == wire.CodeDuplicate }

// LinkStats is the observability surface. Everything here is a counter over the
// life of the guest's memory, so it survives a save like the rest of it.
//
// AND THEREFORE EVERY FIELD IS A DETERMINISTIC FUNCTION OF REPLICATED INPUT.
// That is a constraint on what may be counted here, not an observation: guest
// memory is CRC'd across every peer, so a counter that moved on one peer only
// is a desync. What is countable is inbound (replicated), the tick
// (replicated), build-time configuration (identical builds), and this link's
// own decisions. What is NOT is whether an outbound host call worked -- see
// QueueDrops below, and the join-safety contract in the package doc. A guest
// that wants that answer logs it with fk.Log, which goes to the game log, which
// is not CRC'd.
type LinkStats struct {
	Epoch uint32
	Up    bool

	TxFrames, TxBytes uint32
	RxFrames, RxBytes uint32

	// Drops is every frame this guest refused: bad magic, a version it does
	// not speak, an unknown type, a length disagreeing with the datagram, an
	// epoch it does not recognise, or a channel nobody registered.
	Drops uint32
	// BadFrames, EpochDrops and StaleDrops split Drops by cause, because "junk
	// on a shared local port" and "a peer still talking to the session before
	// this one" are different problems with different fixes.
	BadFrames, EpochDrops, StaleDrops uint32

	// ForeignDrops counts datagrams that arrived from a port that is not
	// Config.Port -- almost always ANOTHER IPC MOD'S companion, because
	// --enable-lua-udp binds one socket for the whole game and every mod is
	// handed every mod's inbound traffic.
	//
	// A steady non-zero value is normal and healthy on a machine running two
	// IPC mods: it is this filter doing its job. It is only a diagnosis when
	// it rises while RxFrames stays at zero, which says the companion is
	// sending from a port this guest was not configured for.
	ForeignDrops uint32

	// NameRejects counts HELLO_ACKs that came from the configured port, matched
	// the outstanding HELLO's corr, decoded cleanly, and carried an identity
	// token that is not [Config.ExpectPeer].
	//
	// It is the counter for the failure the source-port filter cannot see: the
	// transport handshake SUCCEEDED and the two ends disagree about what the
	// channels mean. A rising value with Up false is the whole diagnosis --
	// something IS answering on this port and it is not what this guest was
	// built against -- where without it that state is indistinguishable from a
	// companion nobody started.
	//
	// It is a function of REPLICATED inbound and of build-time configuration,
	// so it holds the same value on every peer. See [Config.ExpectPeer].
	NameRejects uint32

	Gaps                       uint32
	Retries, Timeouts, DupHits uint32
	// QueueDepth and QueueDrops are about THIS LINK'S OWN DECISIONS -- a send
	// with no session, a message that did not fit the queue -- and never about
	// whether a datagram reached the socket. That distinction is a correctness
	// property rather than tidiness: whether send_udp works depends on whether
	// this peer was started with --enable-lua-udp, which a joining client is
	// not, and a counter that moved on one peer only is a desync. See
	// Link.rawSend.
	QueueDepth, QueueDrops uint32

	// ScratchOverflows counts inbound frames that CANNOT have fitted the
	// host's 4 KiB string scratch region and therefore came through fk_alloc.
	//
	// It is a FLOOR and not a total, and saying so is the point: the region is
	// shared with everything else the dispatch marshals, so a frame below the
	// region size may still have fallen back if the dispatch had already
	// consumed some. A non-zero value on a live session means the negotiated
	// frame size is wrong for what the handler does.
	ScratchOverflows uint32

	// Refusals counts everything the ENGINE GATE turned away: one per pump that
	// did nothing, and one per API call answered with [StatusDisabled]. A
	// non-zero value is the whole diagnosis on its own -- this mod is running on
	// an engine below [MinEngineVersion] and is inert -- and pumps dominate it,
	// so read it as "how long has it been like this" rather than as a call
	// count.
	//
	// It replaces RecvRefused, which counted pumps that skipped recv_udp while
	// the rest of the link went on running. There is no such state any more.
	Refusals uint32
	// BaseVersion is what the engine gate read from helpers.game_version. It is
	// re-read once a second while the gate is SHUT and never once it is open,
	// because a save can move to a newer engine and an engine cannot move under
	// a running game. See Link.serviceGate.
	BaseVersion Version
	// Enabled is the gate's verdict: true when this engine is at or above
	// [MinEngineVersion] and the link is live. False means inert -- no HELLO, no
	// heartbeat, no poll, no datagram of any kind.
	Enabled bool

	// Boot is THE SESSION GENERATION: how many sessions this guest has ended.
	// It goes up at a session boundary -- a BYE, or liveness -- and NOT on a
	// load, which is the change the multiplayer-join fix made. A load is not a
	// boundary, because the only signal a load has is peer-local.
	//
	// It rides in HELLO's `boot` field and the peer must still never compare
	// it: it lives in guest memory, so it time-travels with the save and
	// aliases across two loads of one save. That is the theorem the epoch
	// exists to answer. What it is good for is a human reading a log and asking
	// whether the flapping is this guest re-sessioning or the companion
	// restarting.
	Boot uint32
}

// itoa without fmt: importing fmt into a guest pulls in reflection and
// formatting for a package whose only use of it would be two error strings.
func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
