package fkipc

import "github.com/Techrocket9/fklua/guest/go/fkipc/wire"

// The protocol's timers and budgets.
//
// ALL TIMERS ARE IN GAME TICKS. There is no wall clock in the sandbox and there
// is not going to be one; a tick is 16.67 ms of nominal game time and a
// variable amount of real time, which is exactly right for a peer whose pauses
// are the game's pauses. The external side keeps its own timers in real time,
// and the two are reconciled by the tick each HEARTBEAT carries.
const (
	// RetryTicks*: measured, not guessed. Round-trip latency on a headless
	// server through the InputAction path is median 31.5 ms (~1.89 ticks), min
	// 8.4 ms, p90 94.8 ms (~5.7 ticks) -- so a server-profile retry under about
	// ten ticks would be retransmitting frames that were merely in flight, and
	// the client value sits at that p90 on purpose because a single-player
	// client has no replication fan-out and its floor is ~1 tick.
	RetryTicksServer = 15
	RetryTicksClient = 6

	// RetryBackoff is x2 to this cap.
	RetryBackoffCap = 60

	// MaxRetries retransmissions before a request is declared timed out:
	// 15 + 30 + 60 + 60 = 165 ticks, about 2.8 s at the server default.
	MaxRetries = 4

	// DedupTicks is longer than the whole retry schedule, with margin.
	DedupTicks = 600
	// MaxDedup and MaxDedupPayload are both bounds on the SAVE. The dedup
	// table is guest memory, so a cached response is save weight.
	MaxDedup        = 256
	MaxDedupPayload = 512

	// HeartbeatTicks is one per second of game time, UNCONDITIONALLY.
	//
	// It used to be "only if nothing else was sent in the window", on the
	// grounds that any frame is a liveness signal -- which is true and is not
	// the whole job. The heartbeat is the ONLY frame that carries the guest's
	// TICK, and the peer is the side with a real clock and therefore the side
	// that has to notice the guest's clock going backwards (see
	// SDK RollbackTicks). Under the old rule a guest that streams telemetry
	// every tick never heartbeats at all, so the peer's reading of the guest
	// clock froze at the HELLO and stayed there for the whole session -- and
	// so did the rx/drops/gaps flow-control counters the heartbeat carries,
	// which agents/ipc.md already described as arriving "one frame per second,
	// for free" and which were in fact arriving never.
	//
	// The cost is one 40-byte datagram per second of game time in the FREE
	// direction: outbound is a local side effect that is never replicated,
	// never saved into the replay and never quantized to a tick.
	HeartbeatTicks = 60
	// LivenessTicks is three missed heartbeats.
	LivenessTicks = 180

	// ReassemblyTicks is blueprint-share's number, and it has held up.
	ReassemblyTicks = 120

	// SearchTicks is how often a peerless guest sends HELLO.
	SearchTicks = 60

	// SendBudget bounds the WORST TICK, not the bandwidth. The engine does not
	// coalesce and does not rate-limit: ten send_udp calls in one tick produced
	// ten datagrams, in order, on loopback.
	SendBudget = 8

	// DrainMax is a safety valve rather than a working loop bound. One
	// recv_udp per tick drained a 20-packet backlog blasted in 0.34 ms -- all
	// twenty arrived within that tick, in order, complete -- so one call per
	// tick is the shape, and this is the knob if a future build ever delivers
	// one packet per call instead.
	DrainMax = 1

	// MaxQueue frames per priority class. The queue is guest memory, so it is
	// in the save; a class that is never used allocates none of it.
	MaxQueue = 64

	// MaxPending requests in flight. Not in the spec: a retried request keeps
	// its whole message so it can be resent, and at the message ceiling that is
	// ~62 KB apiece, so an unbounded pending table is an unbounded save.
	MaxPending = 16

	// scratchBytes is the host's string scratch region. Only used to count
	// ScratchOverflows -- see the field's own comment for why the count is a
	// floor.
	scratchBytes = 4096
)

// Link is one fkipc session's whole state.
//
// A guest has exactly one, reached through the package-level Open/Pump/... The
// type is exported so the host-side conformance suite can drive several
// independent ones in one process; on the game target there is only ever the
// package's own.
type Link struct {
	cfg Config
	tr  Transport

	// The engine gate. False means the whole link is INERT -- see version.go
	// for why refusing is the default and why it is the whole link rather than
	// the receive path. gated says regate has run at least once, which is what
	// makes the log line a transition rather than a repeat.
	enabled bool
	gated   bool

	tick uint32

	// The session. up and epoch move together by construction: adopting a
	// token is what "up" means, and losing the session is what clears it.
	up    bool
	epoch uint32
	boot  uint32

	helloDue  bool
	helloCorr uint32
	lastHello uint32
	lastRx    uint32
	// lastHB is the tick the last HEARTBEAT went out on, and it is NOT
	// "the last tick anything went out on". See HeartbeatTicks.
	lastHB uint32

	// What the peer said it will ACCEPT. Until a HELLO_ACK arrives these are
	// this side's own defaults, which is what the HELLO itself is sized by.
	peerMaxFrame uint16
	peerMaxFrags uint16

	corrCtr uint32

	chans []*channel

	pend [MaxPending]pending

	// The dedup table as a ring in tick order, which is what makes expiry a
	// walk from the head rather than a scan.
	dedup     []dedupEntry
	dedupHead int
	dedupLen  int

	qCtl  frameQueue
	qBulk frameQueue

	// Reused buffers. enc is the frame being encoded, ctl the control payload
	// going into it, and asm the reassembled message being delivered -- three
	// rather than one because each is live while another is being written.
	enc []byte
	ctl []byte
	asm []byte

	onSession func(SessionEvent)

	// Heartbeat counters, reset every time one goes out.
	hbRx, hbDrops, hbGaps uint32

	stats LinkStats
}

type pending struct {
	used     bool
	ch       uint16
	corr     uint32
	msg      []byte
	tries    uint8
	interval uint32
	due      uint32
	onReply  func(Reply)
}

type dedupEntry struct {
	epoch  uint32
	corr   uint32
	tick   uint32
	ch     uint16
	cached bool
	resp   []byte
}

// pkg is the guest's own link, and it EXISTS BEFORE Open runs.
//
// That is not defensive: it is what makes the obvious guest source correct. Go
// initialises package-level variables BEFORE init() functions, so
//
//	var telemetry = fkipc.Chan(1, fkipc.PriBulk)
//	func init() { fkipc.Open(...) }
//
// names a channel before Open has been called -- and a Chan that answered that
// with a dead handle would take every handler registered on it with it,
// silently, which is exactly the class of failure this repo keeps finding. So
// Open CONFIGURES the link rather than creating it, and the channel table and
// its handlers are there from the first line of package initialisation.
//
// A wasm module is one instance, so a singleton is the honest model anyway, and
// the four wiring exports have nothing to carry a receiver in.
var pkg = &Link{}

// Open gives the link its configuration and its transport, and registers the
// event subscription. Call it from init().
//
// IT SENDS NOTHING. _initialize is control.lua's main chunk, and 2.1 documents
// a non-zero for_player on send_udp, recv_udp and write_file as silently
// skipped there. The first frame goes out from the first Pump, which is inside
// an event dispatch.
//
// Calling it twice reconfigures in place and KEEPS the channels and handlers,
// which is the only behaviour that makes the initialisation order above work.
//
// IT RETURNS StatusDisabled ON AN ENGINE BELOW [MinEngineVersion], having
// logged one line saying so, and the link then does nothing at all for the rest
// of the session. That is a report and not a failure: the guest is configured,
// [Stats] answers, every channel and handler is registered, and if a later load
// lands on a newer engine the link comes up by itself. See [MinEngineVersion].
func Open(cfg Config) Status {
	// THE TRANSPORT IS BUILT FROM cfg, SO THE NORMALISATION HAS TO HAPPEN FIRST.
	// configure() below applies the same rule, but to its own copy and after
	// newTransport has already read the raw ForPlayer -- so a ProfileClient
	// guest that never set ForPlayer sent every frame with for_player = 0,
	// which is "the server if present" and a SILENT NO-OP in graphical single
	// player. Measured 2026-08-07: the demo mods held no session at all in a
	// focused single-player game while the engine's own send_udp delivered 31
	// datagrams from a bare-Lua mod in the same environment.
	cfg = normaliseForPlayer(cfg)
	tr, st := newTransport(cfg)
	if st != StatusOK {
		return st
	}
	return pkg.configure(cfg, tr)
}

// normaliseForPlayer is the one place the ProfileClient default lives, because
// two places is how it came to be applied to the link and not to the transport.
func normaliseForPlayer(cfg Config) Config {
	// ProfileClient with an unset ForPlayer omits the argument rather than
	// asking for the server, because on a client there is no server and a
	// for_player naming one is the measured silent no-op.
	if cfg.Profile == ProfileClient && cfg.ForPlayer == 0 {
		cfg.ForPlayer = -1
	}
	return cfg
}

func newLink(cfg Config, tr Transport) (*Link, Status) {
	l := &Link{}
	st := l.configure(cfg, tr)
	// StatusDisabled IS NOT A CONSTRUCTION FAILURE, and the difference matters
	// here in a way it does not for Open: a bad config or a missing transport
	// leaves nothing worth handing back, but a disabled link is fully
	// configured -- Stats answers, channels register, every call refuses
	// deterministically, and a load onto a newer engine brings it up. Returning
	// nil for it would make the caller's own null check the thing that decides
	// whether a mod compiles, over a condition the mod cannot control.
	if st != StatusOK && st != StatusDisabled {
		return nil, st
	}
	return l, st
}

func (l *Link) configure(cfg Config, tr Transport) Status {
	if cfg.Port == 0 {
		return StatusBadConfig
	}
	if tr == nil {
		return StatusNoTransport
	}
	cfg = normaliseForPlayer(cfg)
	// ONE TOKEN NAMES THE CONTRACT, not either party, so a guest that states
	// what it requires has by that act also stated what it is. Without this a
	// guest setting only ExpectPeer would send an empty Name and be refused by
	// the very companion it just described, which is a footgun with no upside:
	// there is no useful configuration in which a guest checks its peer's
	// identity and wants to withhold its own.
	if cfg.Name == "" {
		cfg.Name = cfg.ExpectPeer
	}
	l.cfg, l.tr = cfg, tr
	l.peerMaxFrame = clampFrame(cfg.MaxFrame)
	l.peerMaxFrags = wire.MaxFragments
	l.helloDue = true
	l.regate()
	// The gate's verdict is Open's answer, so a guest that wants to know can ask
	// at the one call it already makes. Everything else about the link is
	// configured either way -- channels, handlers and Stats all work on a
	// disabled link, and a later load onto a newer engine brings it up with no
	// second Open.
	if !l.enabled {
		return StatusDisabled
	}
	return StatusOK
}

// clampFrame turns whatever a peer or a config said into something sendable.
//
// A zero is "no opinion". Below MinMaxFrame is a peer that is confused or
// hostile, and clamping up is kinder than fragmenting a heartbeat. Above the
// ceiling is refused whoever asked for it: the ceiling clears the inbound
// datagram wall, the host's string scratch and every OS's limit, and the
// transport reports none of those.
func clampFrame(v uint16) uint16 {
	if v == 0 {
		return wire.DefaultMaxFrame
	}
	if v < wire.MinMaxFrame {
		return wire.MinMaxFrame
	}
	if v > wire.MaxFrameCeiling {
		return wire.MaxFrameCeiling
	}
	return v
}

// regate re-reads the base-game version and decides whether the link may run at
// all.
//
// IT RUNS IN Open AND FROM THE PUMP, AND IT USED TO RUN FROM Reload. That move
// is the engine gate's half of the join fix: fk_after_load is a PEER-LOCAL
// signal, and nothing peer-local may write guest state. What replaces it is
// serviceGate below, which is driven by the replicated tick.
//
// Reading the version is legal from anywhere and on every peer, and that is not
// an accident of this design: Factorio refuses a multiplayer connection between
// two different builds, so helpers.game_version is the same string on every
// peer in the game. The gate is also MONOTONE -- a save carries its build and
// the engine refuses to load one written by a NEWER build, so a restored
// "the link may run" can only have come from an engine at or below this one --
// which is why serviceGate only re-reads while the gate is shut.
//
// THE LOG LINE FIRES ON A TRANSITION, so it is once per load at most and not
// once per re-read: serviceGate polls while shut, and a line every second would
// be a log nobody reads with the one interesting entry buried in it.
func (l *Link) regate() {
	was, first := l.enabled, !l.gated
	v, ok := l.tr.BaseVersion()
	l.stats.BaseVersion = v
	l.enabled = ok && !v.Less(MinEngineVersion)
	l.stats.Enabled = l.enabled
	l.gated = true
	if l.enabled == was && !first {
		return
	}
	if !l.enabled {
		l.tr.Log(disabledMessage(v))
		return
	}
	if !first {
		// Only reachable across a LOAD onto a newer engine, since the gate is
		// monotone within a session. Worth a line, because the previous session
		// logged the refusal and a reader deserves to see it withdrawn.
		l.tr.Log("fkipc: enabled -- this engine is " + v.String())
	}
}

// serviceGate re-opens the engine gate for a save that was written on an older
// engine, without ever branching on anything peer-local.
//
// A SAVE REALLY DOES MOVE BETWEEN ENGINES, which is the whole reason this
// exists: an engine cannot change under a running game, so within one session
// the answer is fixed -- but a map made on 2.0.77 and then loaded on 2.1.14 is
// an ordinary thing for a player to do, and a link that only asked at Open
// would stay dead for the life of that save. Open runs from _initialize on
// every load, so the common case is already covered; this covers the residual
// one where the transport's read failed the first time.
//
// The condition is guest state (enabled) and the REPLICATED tick, so every peer
// re-reads on the same tick and writes the same answer -- which is the whole
// property the load-reset design broke. The cost is one host call per second of
// game time, and only on a build where the link is refused anyway; once the
// gate is open it is never re-read, because it cannot shut again.
//
// Worst case after a load that changes the answer: SearchTicks of inertness,
// which is the same second a peerless guest already spends between HELLOs.
func (l *Link) serviceGate() {
	if l.enabled || l.tick%SearchTicks != 0 {
		return
	}
	l.regate()
}

// Reload is the fk_after_load route, and IT DOES NOTHING. That is the fix, not
// an oversight, and it is the single most important comment in this package.
//
// fk_mod.lua arms its fk_after_load one-shot from script.on_load, and Factorio
// runs script.on_load ON EVERY PEER THAT LOADS THE STATE -- including a client
// joining a game already in progress. The server ran it when it started and
// will not run it again; the joiner runs it on its first simulated tick. So
// fk_after_load is a PEER-LOCAL signal, and under the default --persist=table
// guest memory IS storage.fk_mem, which Factorio CRCs. This function used to
// bump boot, discard the epoch, drop the dedup table, fail every pending
// request and reset every channel's seq -- all of it on one peer only, one tick
// after a join. Measured on 2.1.14: "fkipc session reset" on the client,
// followed by "Multiplayer desynchronisation: crc test failed" on the very next
// tick, every tick, with a desync report generated.
//
// THE GENERAL RULE, WHICH IS NOT FKIPC'S: no peer-local signal may mutate guest
// state. fk_mod.lua says it one level up for the hook this one is armed from --
// "on_load is READ-ONLY with respect to storage ... a write here is a desync
// waiting to happen" -- and a one-shot armed from on_load is a write from
// on_load with one tick of delay.
//
// WHAT REPLACES IT, because a load really does invalidate a session and
// something has to notice:
//
//   - The companion restarted too. It does not recognise the epoch the restored
//     guest is still using, so it answers BYE. A BYE arrives through recv_udp,
//     which is an InputAction, which every peer sees at the same tick -- so
//     every peer resets identically and the guest re-HELLOs.
//   - The companion kept running across the guest's rollback. Then the epoch
//     still matches and the guest's seq counters have gone BACKWARDS, which
//     would leave every channel stale-dropped and deaf. The side with a real
//     clock is the side that can see a clock go backwards, so the SDK watches
//     the tick every HEARTBEAT carries and BYEs on a regression past its
//     tolerance -- see sdk/go/fkipc's RollbackTicks.
//   - Nobody is there at all. serviceSession's liveness test fires within
//     LivenessTicks and the guest quiesces and searches, exactly as it does
//     when a peer dies mid-session.
//
// Every one of those is driven by replicated state, which is why they are
// join-safe and the load-reset was not.
//
// It is kept -- rather than deleted -- because the wiring line
// `//go:wasmexport fk_after_load func afterLoad() { fkipc.Reload() }` is in
// every guest this project has ever documented, and removing the function it
// calls turns a correctness fix into a compile error in somebody else's mod.
// A guest that has no other use for fk_after_load may now drop the export.
func Reload() { pkg.Reload() }

// Reload does nothing. See the package-level [Reload].
func (l *Link) Reload() {}

// Pump is the fk_on_tick route.
//
// A PERMANENT fk_on_tick IS THE RIGHT SHAPE HERE, which is the one place this
// repo's standing "register nothing when idle" bias does not survive contact
// with the problem: a guest with no peer must still be able to NOTICE one
// appearing, and the only way to notice is to call recv_udp. A guest that
// registers nothing is a guest that can never be reached again. And a slow poll
// costs the same registration anyway, because fk.defer is a next-tick one-shot
// with no way to skip ticks.
//
// So what varies with session state is the work inside, not the registration:
// an IPC guest with no peer pays one dispatch and one integer compare per tick
// plus a recv_udp every SearchTicks-worth of searching, and one with a live peer
// pays one dispatch and one recv_udp per tick.
func Pump(tick uint32) { pkg.Pump(tick) }

func (l *Link) Pump(tick uint32) {
	if l == nil || l.tr == nil {
		return
	}
	l.tick = tick
	l.serviceGate()

	// BELOW THE FLOOR THE PUMP DOES NOTHING AND RETURNS, and "nothing" is
	// literal: no poll, no HELLO, no heartbeat, no flush, not one datagram of
	// any kind. Everything below this line either puts a frame on the wire or
	// services state that only a frame can change.
	//
	// It is not a fast path bolted on. A send-only link would still HELLO once a
	// second forever -- the ACK that would end the search arrives inbound, which
	// is the direction that is shut -- so "outbound is free" is true of the cost
	// and false of the usefulness. See MinEngineVersion.
	if !l.enabled {
		l.stats.Refusals++
		return
	}

	// Receive first, so a reply that arrived this tick cancels its own retry
	// before the retry timer runs.
	for i := 0; i < DrainMax; i++ {
		if !l.tr.Poll(l.deliver) {
			break
		}
	}

	l.expireReassembly()
	l.expireDedup()
	l.serviceSession()
	l.serviceRetries()
	l.flush()
}

// OnEvent is the fk_on_event route. It reports whether the event was fkipc's,
// so the guest's own switch runs on everything else.
func OnEvent(id, ptr uint32) bool { return pkg.OnEvent(id, ptr) }

func (l *Link) OnEvent(id, ptr uint32) bool {
	if l == nil || l.tr == nil {
		return false
	}
	// A DISABLED LINK CLAIMS NOTHING. Below the floor this package never calls
	// recv_udp, so it never causes an on_udp_packet_received -- but another IPC
	// mod in the same game might, and --enable-lua-udp binds ONE socket, so the
	// event reaches every subscribed mod. Returning true here would swallow it
	// from the guest's own switch on behalf of a link that is not going to look
	// at it. Returning false costs the guest one comparison and keeps the event
	// its own.
	if !l.enabled {
		return false
	}
	return l.tr.Event(id, ptr, l.deliver)
}

// OnSession registers the session-state handler.
func OnSession(h func(ev SessionEvent)) { pkg.OnSession(h) }

func (l *Link) OnSession(h func(ev SessionEvent)) {
	if l != nil {
		l.onSession = h
	}
}

// Stats returns the observability snapshot.
//
// The TYPE is LinkStats rather than Stats because a package cannot hold a
// function and a type of one name, and fkgc already settled which one gives
// way: it spells the pair Stats() MemStats. The Rust mirror has no collision
// and keeps `stats() -> Stats`.
func Stats() LinkStats { return pkg.Stats() }

// refused is what every outbound API entry point answers on a link the engine
// gate has shut, and it is ONE helper so the answer cannot differ by call site.
//
// StatusDisabled RATHER THAN StatusNoSession OR StatusNotOpen, and the choice
// is about what a mod author does next.
//
//   - StatusNoSession is the QUIESCE shape and it means "the peer is down, it
//     may be back this second". A guest is told to treat it as transient and
//     keep playing, which is right for a companion that crashed and wrong here:
//     an engine cannot change under a running game, so below the floor the
//     session can never come up and reporting a transient condition for a
//     permanent one invites a guest to spin on it.
//   - StatusNotOpen means "you did not call Open" -- a programming mistake the
//     author fixes in source. Here the author did everything right and the
//     ENGINE is what is wrong, which is not something their code can address.
//
// It keeps the counted-no-op property that made StatusNoSession worth having: a
// Status is not an error a mod must branch on, so a guest written against a
// 2.1 engine and run on 2.0 simply does nothing, once per call, counted.
func (l *Link) refused() Status {
	l.stats.Refusals++
	return StatusDisabled
}

func (l *Link) Stats() LinkStats {
	if l == nil {
		return LinkStats{}
	}
	s := l.stats
	s.Epoch, s.Up, s.Boot = l.epoch, l.up, l.boot
	s.QueueDepth = uint32(l.qCtl.n + l.qBulk.n)
	return s
}

// ---------------------------------------------------------------------------
// The session.

// serviceSession is the whole of what a tick owes the session, and both of its
// tests are functions of guest state and the REPLICATED tick -- which is what
// makes them legal to run on every peer, including one that just joined.
func (l *Link) serviceSession() {
	if l.up {
		// TWO CONDITIONS, AND THE SECOND ONE IS THE GUEST'S OWN HALF OF ROLLBACK
		// DETECTION rather than an arithmetic accident.
		//
		// d < 0 says the clock has gone BACKWARDS since the last frame this link
		// accepted -- a save restored to a point before it, i.e. the session in
		// memory belongs to a future that no longer happened. It used to fall
		// out of the unsigned subtraction underflowing past LivenessTicks, which
		// gave the right answer for the wrong stated reason; now that a load
		// resets nothing, it is load-bearing and is spelled out.
		//
		// It catches a rollback LARGER than the time since the last inbound
		// frame, which with a peer heartbeating once a second is most of them.
		// What it cannot catch is a save taken just after an inbound frame and
		// restored much later -- tick and lastRx move together, so the
		// difference is small and nothing here is wrong. That one is the peer's,
		// because only the peer has a clock that did not travel with the save.
		if d := int32(l.tick - l.lastRx); d < 0 || uint32(d) > LivenessTicks {
			l.resetSession(SessionDown)
		}
	}
	if !l.up {
		if l.helloDue || l.tick-l.lastHello >= SearchTicks {
			l.sendHello()
		}
		return
	}
	// UNCONDITIONAL, not "only if nothing else went out". See HeartbeatTicks:
	// this is the only frame carrying the guest's tick and its flow-control
	// counters, and a telemetry-heavy guest would otherwise never send one.
	if l.tick-l.lastHB >= HeartbeatTicks {
		l.queueHeartbeat()
	}
}

// resetSession is the quiesce, and everything it does is the same rule:
// nothing survives a session boundary except the application's own handlers.
//
// A guest whose peer is down KEEPS PLAYING. It does not retry harder, it does
// not buffer against the peer's return, and it never blocks -- so the send
// queue goes rather than growing, and Send becomes a counted no-op rather than
// an error the mod has to handle at every call site.
//
// EVERY CALLER IS A REPLICATED SIGNAL and that is now a rule rather than an
// observation: the liveness test above (guest state and the replicated tick)
// and a BYE (an InputAction, delivered to every peer at the same tick). Nothing
// peer-local may reach here -- which is exactly what Reload stopped doing.
func (l *Link) resetSession(ev SessionEvent) {
	l.up = false
	l.epoch = 0
	l.helloCorr = 0
	l.helloDue = true
	// THE SESSION GENERATION, and it is what boot means now. It used to be a
	// LOAD counter bumped by Reload, which is the one place it could not be
	// bumped; a session boundary is replicated, so this is. It still aliases
	// across two loads of one save -- the save carries it -- so the peer must
	// still never compare it, and the theorem that only the peer can mint a
	// unique session id is untouched.
	l.boot++
	l.stats.Boot = l.boot
	// The dedup table is dead the moment the epoch is: every entry is keyed by
	// the epoch it was recorded under, so nothing here can ever match again and
	// it is pure save weight until DedupTicks expires it.
	l.dedupHead, l.dedupLen = 0, 0
	l.qCtl.reset()
	l.qBulk.reset()

	for i := range l.pend {
		p := &l.pend[i]
		if !p.used {
			continue
		}
		cb, corr := p.onReply, p.corr
		l.freePending(p)
		if cb != nil {
			cb(Reply{Corr: Corr(corr), Err: ErrSessionLost})
		}
	}
	for _, c := range l.chans {
		c.txSeq, c.rxLast = 0, 0
		c.resyncSent = false
		c.abandon()
	}
	if l.onSession != nil {
		l.onSession(ev)
	}
}

func (l *Link) sendHello() {
	l.helloCorr = l.nextCorr()
	l.helloDue = false
	l.lastHello = l.tick

	var err error
	l.ctl, err = wire.AppendHello(l.ctl[:0], wire.Hello{
		ProtoMin: wire.Version, ProtoMax: wire.Version,
		MaxFrame: clampFrame(l.cfg.MaxFrame), MaxFragments: wire.MaxFragments,
		Boot: l.boot, Tick: l.tick,
		Profile: wire.Profile(l.cfg.Profile), Name: l.cfg.Name,
	})
	if err != nil {
		return
	}
	// HELLO bypasses the queue. It is the recovery path, and the queue is the
	// thing a quiesce just threw away.
	l.enc, err = wire.AppendFrame(l.enc[:0], wire.Header{
		Type: wire.TypeHello, Epoch: l.boot, Corr: l.helloCorr,
	}, l.ctl)
	if err != nil {
		return
	}
	l.rawSend(l.enc)
}

// onHelloAck is THE ONE EPOCH-TEST EXEMPTION, and it is stated here rather than
// left to be inferred because two implementations would otherwise disagree
// about it: HELLO_ACK carries an epoch the guest does not yet know, by
// definition, so it is matched on corr against the outstanding HELLO instead.
//
// Adopting a peer-chosen value into guest state is legal, and the reason is the
// cost model. The token arrives via recv_udp, which enters game state as an
// InputAction, which the engine replicates to every peer at the same tick. Every
// peer's guest adopts the same token at the same tick. This is the expensive
// direction paying for itself.
func (l *Link) onHelloAck(h wire.Header, p []byte) {
	if l.helloCorr == 0 || h.Corr != l.helloCorr {
		l.drop(&l.stats.EpochDrops)
		return
	}
	hello, err := wire.DecodeHello(p)
	if err != nil {
		l.drop(&l.stats.BadFrames)
		return
	}
	// THE NAME IS THE SCHEMA FILTER, and it is a fourth mechanism rather than a
	// refinement of any of the other three: the HELLO is the session boundary,
	// the epoch is the frame filter, the SOURCE PORT is the mod filter, and this
	// is the only one that can refuse a peer whose transport is entirely
	// correct. A swapped port config or a companion left running from last week
	// produces a handshake that succeeds at every layer below this one and two
	// ends that disagree about what channel 1 means.
	//
	// IT IS LEGAL FOR THE SAME REASON ADOPTING THE TOKEN IS. The ACK arrived
	// through recv_udp, which is an InputAction, which the engine replicates to
	// every peer at the same tick -- so refusing it, counting the refusal and
	// carrying on searching are identical on every peer, exactly as adopting it
	// would have been. The configuration it is compared against is a build-time
	// constant, which is identical on every peer by construction. Nothing here
	// touches a peer-local signal, which is what the load-reset design got
	// wrong -- see Reload.
	//
	// NOTHING ABOUT THE OUTSTANDING HELLO IS CONSUMED, and that is the whole of
	// the retry continuation:
	//
	//   - helloCorr is LEFT SET, so this HELLO is still answerable. A correct
	//     ACK on the same corr -- the companion restarted with the right
	//     identity while that HELLO was in flight, or two companions answered
	//     and the right one was second -- is still adopted, where zeroing it
	//     would have made the guest deaf until the next search.
	//   - lastHello and helloDue are LEFT ALONE, so the search cadence does not
	//     change. Re-HELLOing at once on a reject is the tempting move and it is
	//     a HELLO storm: a mismatched companion answers every HELLO, so "reject,
	//     then re-HELLO" is a frame per tick in both directions for as long as
	//     the misconfiguration lasts. That is the livelock shape the source-port
	//     filter was built to end, met from a new direction.
	//
	// So a rejected ACK costs exactly one counted drop and the link goes on
	// searching at SearchTicks, which is what it was already doing.
	if l.cfg.ExpectPeer != "" && hello.Name != l.cfg.ExpectPeer {
		// Counted like a foreign-port drop and for its reason: charged to Drops
		// so a total still accounts for every refused frame, and NOT to hbDrops,
		// because hbDrops is flow control -- the number this side asks its peer
		// to slow down over -- and a peer that is the wrong program is not
		// something any rate can fix.
		l.stats.Drops++
		l.stats.NameRejects++
		return
	}
	l.epoch = h.Epoch
	l.up = true
	l.helloCorr = 0
	l.lastRx = l.tick
	// So the first heartbeat of a session is exactly HeartbeatTicks after it
	// comes up rather than at whatever offset the previous one left behind.
	l.lastHB = l.tick
	l.peerMaxFrame = clampFrame(hello.MaxFrame)
	l.peerMaxFrags = hello.MaxFragments
	if l.peerMaxFrags == 0 || l.peerMaxFrags > wire.MaxFragments {
		l.peerMaxFrags = wire.MaxFragments
	}
	l.stats.RxFrames++
	l.hbRx++
	for _, c := range l.chans {
		c.txSeq, c.rxLast = 0, 0
		c.resyncSent = false
		c.abandon()
	}
	if l.onSession != nil {
		l.onSession(SessionUp)
	}
}

func (l *Link) queueHeartbeat() {
	l.ctl = wire.AppendHeartbeat(l.ctl[:0], wire.Heartbeat{
		Tick: l.tick, Rx: l.hbRx, Drops: l.hbDrops, Gaps: l.hbGaps,
	})
	if l.queueFrame(PriControl, wire.Header{
		Type: wire.TypeHeartbeat, Epoch: l.epoch,
	}, l.ctl) == StatusOK {
		l.hbRx, l.hbDrops, l.hbGaps = 0, 0, 0
		// Charged here rather than at the flush so a full control queue does
		// not turn into a heartbeat every tick.
		l.lastHB = l.tick
	}
}

func (l *Link) nextCorr() uint32 {
	l.corrCtr++
	if l.corrCtr == 0 {
		l.corrCtr = 1 // 0 means "no correlation"
	}
	return l.corrCtr
}

func (l *Link) retryTicks() uint32 {
	if l.cfg.Profile == ProfileClient {
		return RetryTicksClient
	}
	return RetryTicksServer
}

// ---------------------------------------------------------------------------
// Outbound.

// rawSend puts one frame on the socket and CANNOT SEE WHETHER IT WENT, which is
// the second half of the peer-local rule and cost a multiplayer join on its own.
//
// It used to ignore an outcome the seam offered; [Transport.Send] returns
// nothing now, so there is no outcome to ignore and no way for a later edit
// here to reintroduce the branch. The reasoning is unchanged and is worth
// keeping in front of whoever changes this function:
//
// Whether send_udp succeeds is a fact about THIS PEER'S COMMAND LINE:
// --enable-lua-udp is what binds the socket, a headless server in this project
// has it and a graphical client joining that server does not. So a guest that
// branched on the outcome wrote TxFrames and TxBytes on the server and
// QueueDrops on the client, every frame, into storage.fk_mem -- which Factorio
// CRCs. Measured on 2.1.14: a client joining a server running the demo mods
// desyncs on the first tick it simulates, with NO companion anywhere and no
// inbound datagram in the game, while the same client joining a server running
// a NON-IPC guest stays in sync for as long as you leave it.
//
// So these count what this link ATTEMPTED, which is a deterministic function of
// guest state and therefore identical on every peer. What is lost is the
// ability to see a failed send in Stats, and that is the right trade in a
// direction the cost model already calls FREE: an outbound frame is a local
// side effect that never enters game state, so its fate is not something guest
// state may have an opinion about. A peer that wants to know can look at the
// socket.
func (l *Link) rawSend(f []byte) {
	l.tr.Send(f)
	l.stats.TxFrames++
	l.stats.TxBytes += uint32(len(f))
}

func (l *Link) queueFrame(pri Priority, h wire.Header, payload []byte) Status {
	var err error
	l.enc, err = wire.AppendFrame(l.enc[:0], h, payload)
	if err != nil {
		return StatusTooLarge
	}
	q := &l.qBulk
	if pri == PriControl {
		q = &l.qCtl
	}
	if !q.push(l.enc) {
		l.stats.QueueDrops++
		return StatusQueueFull
	}
	return StatusOK
}

// sendMessage fragments one message onto one channel and queues every piece, or
// queues none of them.
//
// ALL OR NOTHING, because a partially queued message is a guaranteed gap at the
// far end plus a reassembly that can never complete -- strictly worse than the
// send the caller can see fail.
func (l *Link) sendMessage(c *channel, ty wire.Type, flags wire.Flags,
	corr uint32, payload []byte) Status {

	room := int(l.peerMaxFrame) - wire.HeaderBytes
	if room <= 0 {
		return StatusTooLarge
	}
	n := (len(payload) + room - 1) / room
	if n == 0 {
		n = 1
	}
	maxFrags := int(l.peerMaxFrags)
	if maxFrags > wire.MaxFragments {
		maxFrags = wire.MaxFragments
	}
	if n > maxFrags {
		return StatusTooLarge
	}
	q := &l.qBulk
	if c.pri == PriControl {
		q = &l.qCtl
	}
	if MaxQueue-q.n < n {
		l.stats.QueueDrops++
		return StatusQueueFull
	}
	// A multi-fragment message needs a correlation id to be reassembled by.
	if n > 1 && corr == 0 {
		corr = l.nextCorr()
	}
	for i := 0; i < n; i++ {
		lo := i * room
		hi := lo + room
		if hi > len(payload) {
			hi = len(payload)
		}
		st := l.queueFrame(c.pri, wire.Header{
			Type: ty, Flags: flags, Channel: c.id, Epoch: l.epoch,
			Seq: c.nextSeq(), Corr: corr, Frag: uint8(i), NFrag: uint8(n),
		}, payload[lo:hi])
		if st != StatusOK {
			return st
		}
	}
	return StatusOK
}

func (l *Link) flush() {
	for i := 0; i < SendBudget; i++ {
		q := &l.qCtl
		if q.n == 0 {
			q = &l.qBulk
		}
		if q.n == 0 {
			return
		}
		f := q.peek()
		q.drop()
		l.rawSend(f)
	}
}

func (l *Link) serviceRetries() {
	for i := range l.pend {
		p := &l.pend[i]
		if !p.used || int32(l.tick-p.due) < 0 {
			continue
		}
		if p.tries >= MaxRetries {
			cb, corr := p.onReply, p.corr
			l.freePending(p)
			l.stats.Timeouts++
			if cb != nil {
				cb(Reply{Corr: Corr(corr), Err: ErrTimeout})
			}
			continue
		}
		c := l.findChan(p.ch)
		if c == nil {
			l.freePending(p)
			continue
		}
		p.tries++
		l.stats.Retries++
		l.sendMessage(c, wire.TypeReq, wire.FlagRetry, p.corr, p.msg)
		p.interval *= 2
		if p.interval > RetryBackoffCap {
			p.interval = RetryBackoffCap
		}
		p.due = l.tick + p.interval
	}
}

func (l *Link) allocPending() *pending {
	for i := range l.pend {
		if !l.pend[i].used {
			l.pend[i].used = true
			return &l.pend[i]
		}
	}
	return nil
}

func (l *Link) freePending(p *pending) {
	p.used = false
	p.onReply = nil
	p.msg = p.msg[:0]
	p.tries = 0
}

func (l *Link) findPending(ch uint16, corr uint32) *pending {
	for i := range l.pend {
		p := &l.pend[i]
		if p.used && p.ch == ch && p.corr == corr {
			return p
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inbound.

func (l *Link) drop(cause *uint32) {
	l.stats.Drops++
	l.hbDrops++
	*cause++
}

// deliver takes one inbound datagram and the port it came FROM.
//
// THE SOURCE-PORT TEST IS FIRST, AND IT IS THE ONLY THING THAT MAKES TWO IPC
// MODS IN ONE GAME SAFE. --enable-lua-udp binds ONE socket for the whole game,
// so on_udp_packet_received fires in every mod for every mod's datagrams: mod
// A's link sees mod B's frames and vice versa. The epoch filter catches most of
// that, and there is exactly one hole in it, which is not hypothetical --
//
//	HELLO_ACK IS THE ONE FRAME MATCHED ON corr WITH THE EPOCH TEST SKIPPED,
//	because by definition it carries an epoch the guest does not yet know. corr
//	is minted from a COUNTER, and two freshly-loaded guests have identical
//	counter state, so both first HELLOs carry corr = 1. Two companions answer,
//	both ACKs reach both mods, and whichever lands first is adopted -- so mod A
//	can adopt mod B's token, then talk to A's companion under an epoch A's
//	companion has never heard of, which that side answers with BYE. A session
//	lost to a mod that is not even ours.
//
// A frame from any port but the configured peer's is therefore dropped before
// anything else looks at it -- before RxBytes, so a session's byte accounting
// describes its OWN conversation rather than the machine's traffic, and before
// the ScratchOverflows floor, which is a statement about this link's negotiated
// frame size.
//
// TWO DELIBERATE ASYMMETRIES WITH THE OTHER DROPS. It is counted in Drops and
// ForeignDrops but NOT in hbDrops: hbDrops is flow control, the number this
// side asks its peer to slow down over, and another mod's traffic is not
// something the peer can do anything about. And src == 0 is ACCEPTED: zero is
// not a valid UDP source port, so it means "the engine did not say", and
// refusing on silence would make a guest deaf on any build that stops
// reporting the field -- deafness is silent and total, cross-talk is loud and
// recoverable. The live gate is what proves the field is really populated:
// scripts/run-ipcdemo.sh's foreign-port leg sends a valid frame from a second
// socket and requires the guest to ignore it, which cannot pass if src is 0.
func (l *Link) deliver(src uint16, dg []byte) {
	if src != 0 && src != l.cfg.Port {
		l.stats.Drops++
		l.stats.ForeignDrops++
		return
	}
	l.stats.RxBytes += uint32(len(dg))
	if len(dg) >= scratchBytes {
		l.stats.ScratchOverflows++
	}
	h, p, err := wire.Decode(dg)
	if err != nil {
		l.drop(&l.stats.BadFrames)
		return
	}
	if h.Type == wire.TypeHelloAck {
		l.onHelloAck(h, p)
		return
	}
	if !l.up || h.Epoch != l.epoch {
		l.drop(&l.stats.EpochDrops)
		return
	}
	l.stats.RxFrames++
	l.hbRx++
	l.lastRx = l.tick

	switch h.Type {
	case wire.TypeHeartbeat:
		// Liveness only, already charged. The counters it carries are for the
		// PEER's flow control, not this side's.
	case wire.TypeBye:
		l.resetSession(SessionDown)
	case wire.TypeMsg, wire.TypeReq, wire.TypeResp, wire.TypeResync:
		l.channelFrame(h, p)
	case wire.TypeFileNotify:
		// Inbound only. A guest cannot read files -- there is no file-read API
		// -- so a notify aimed at one is meaningless and is counted rather than
		// delivered to a handler that could do nothing with it.
		l.drop(&l.stats.BadFrames)
	default:
		// HELLO: the guest is the side that sends those.
		l.drop(&l.stats.BadFrames)
	}
}

func (l *Link) channelFrame(h wire.Header, p []byte) {
	c := l.findChan(h.Channel)
	if c == nil {
		l.drop(&l.stats.BadFrames)
		return
	}
	// Channel 0 is the protocol's own and carries no seq: a lost heartbeat is
	// normal and must not read as a gap in application state.
	if h.Channel != 0 {
		// A SNAPSHOT RESETS last RATHER THAN ADVANCING IT, and it is exempt from
		// the staleness rule for the same reason: it is a COMPLETE state, so
		// accepting it can never deliver a world older than the one already
		// delivered, and it is the only frame that can rescue a channel whose
		// counter has got ahead of its sender. Without the exemption a receiver
		// whose rxLast ever jumped forward -- a corrupted seq, a peer that
		// restarted its counter without a new epoch -- is deaf on that channel
		// FOREVER: every later frame reads as old, so no gap is ever raised, so
		// no RESYNC is ever sent, and nothing anywhere says anything. Found by
		// the seeded fault soak.
		snapshot := h.Type == wire.TypeMsg && h.Flags.Has(wire.FlagSnapshot)
		d := wire.SerialDelta(h.Seq, c.rxLast)
		if d <= 0 && !snapshot {
			// DROPPING d <= 0 IS A SEMANTIC CHOICE: a channel carries STATE,
			// not a LOG. An out-of-order or duplicated frame describes an older
			// world than one already delivered, and stale game state is worse
			// than useless. An application that needs an append-only record
			// numbers its own entries inside the payload.
			l.drop(&l.stats.StaleDrops)
			return
		}
		if d > 1 && !snapshot {
			l.stats.Gaps++
			l.hbGaps++
			if !c.resyncSent {
				// Through sendMessage rather than queueFrame, because a RESYNC
				// names a channel and therefore consumes that channel's seq --
				// one sent with seq 0 would arrive as d <= 0 and be dropped as
				// stale by the very rule it exists to escape.
				c.resyncSent = true
				l.sendMessage(c, wire.TypeResync, 0, 0, nil)
			}
			if c.onGap != nil {
				c.onGap(uint32(d - 1))
			}
		}
		c.rxLast = h.Seq
	}
	if h.NFrag == 1 {
		l.dispatch(c, h, p)
		return
	}
	l.reassemble(c, h, p)
}

// reassemble holds AT MOST ONE open message per channel, which is what bounds
// the buffer and what imposes the rule that a peer must not interleave two
// fragmented messages on one channel.
func (l *Link) reassemble(c *channel, h wire.Header, p []byte) {
	if int(h.NFrag) > wire.MaxFragments {
		c.abandon()
		l.drop(&l.stats.BadFrames)
		return
	}
	if c.rasmActive && h.Corr != c.rasmCorr {
		// A new corr on a channel with one already open: the peer interleaved,
		// or the old one's remaining fragments are never coming. Either way the
		// old is dead.
		c.abandon()
	}
	if c.rasmActive && h.NFrag != c.rasmNFrag {
		// The same corr describing a different message. Nothing here can tell
		// which of the two is real, so neither is.
		c.abandon()
		l.drop(&l.stats.BadFrames)
		return
	}
	if !c.rasmActive {
		c.rasmActive = true
		c.rasmCorr = h.Corr
		c.rasmNFrag = h.NFrag
		c.rasmType = h.Type
		c.rasmFlags = h.Flags
		c.rasmGot = 0
		for i := range c.rasmSeen {
			c.rasmSeen[i] = false
		}
	}
	c.rasmDeadline = l.tick + ReassemblyTicks

	i := int(h.Frag)
	if !c.rasmSeen[i] {
		c.rasmSeen[i] = true
		c.rasmGot++
	}
	c.rasmPart[i] = append(c.rasmPart[i][:0], p...)
	if c.rasmGot < c.rasmNFrag {
		return
	}

	l.asm = l.asm[:0]
	for k := 0; k < int(c.rasmNFrag); k++ {
		l.asm = append(l.asm, c.rasmPart[k]...)
	}
	h.Type, h.Flags = c.rasmType, c.rasmFlags
	h.Corr = c.rasmCorr
	c.abandon()
	l.dispatch(c, h, l.asm)
}

func (l *Link) expireReassembly() {
	for _, c := range l.chans {
		if c.rasmActive && int32(l.tick-c.rasmDeadline) >= 0 {
			c.abandon()
		}
	}
}

func (l *Link) dispatch(c *channel, h wire.Header, payload []byte) {
	switch h.Type {
	case wire.TypeMsg:
		if h.Flags.Has(wire.FlagSnapshot) {
			c.resyncSent = false
		}
		if c.onMessage != nil {
			c.onMessage(Message{
				Channel: c.id, Seq: h.Seq,
				Snapshot: h.Flags.Has(wire.FlagSnapshot), Payload: payload,
			})
		}
	case wire.TypeResync:
		if c.onResync != nil {
			c.onResync()
		}
	case wire.TypeReq:
		l.onReq(c, h, payload)
	case wire.TypeResp:
		l.onResp(c, h, payload)
	}
}

func (l *Link) onReq(c *channel, h wire.Header, payload []byte) {
	if e := l.findDedup(h.Channel, h.Corr); e != nil {
		l.stats.DupHits++
		if e.cached {
			l.sendMessage(c, wire.TypeResp, wire.FlagRetry, h.Corr, e.resp)
			return
		}
		// EXECUTED, AND THE RESULT IS GONE. Strictly better than the two
		// alternatives -- silently re-executing, or growing the save without
		// bound -- and the application can tell the difference.
		l.respondError(c, h.Corr, wire.CodeDuplicate, "result was not cached")
		return
	}
	if c.onRequest == nil {
		l.respondError(c, h.Corr, wire.CodeNoHandler, "")
		return
	}
	out := c.onRequest(Request{
		Channel: c.id, Corr: Corr(h.Corr),
		Retry: h.Flags.Has(wire.FlagRetry), Payload: payload,
	})
	l.addDedup(h.Channel, h.Corr, out)
	l.sendMessage(c, wire.TypeResp, 0, h.Corr, out)
}

func (l *Link) onResp(c *channel, h wire.Header, payload []byte) {
	p := l.findPending(c.id, h.Corr)
	if p == nil {
		// A response to a request that already completed, timed out, or died
		// with the session. Not an error -- it is what a retry that crossed its
		// own answer looks like.
		l.drop(&l.stats.StaleDrops)
		return
	}
	r := Reply{Corr: Corr(h.Corr)}
	if h.Flags.Has(wire.FlagError) {
		rec, err := wire.DecodeErrorRecord(payload)
		if err != nil {
			l.drop(&l.stats.BadFrames)
			return
		}
		r.Err = &PeerError{Code: rec.Code, Message: rec.Message}
	} else {
		r.Payload = payload
	}
	cb := p.onReply
	l.freePending(p)
	if cb != nil {
		cb(r)
	}
}

func (l *Link) respondError(c *channel, corr uint32, code uint16, msg string) {
	var err error
	l.ctl, err = wire.AppendErrorRecord(l.ctl[:0], wire.ErrorRecord{Code: code, Message: msg})
	if err != nil {
		return
	}
	l.sendMessage(c, wire.TypeResp, wire.FlagError, corr, l.ctl)
}

// ---------------------------------------------------------------------------
// Dedup: a ring in tick order, so expiry is a walk from the head.

func (l *Link) findDedup(ch uint16, corr uint32) *dedupEntry {
	for i := 0; i < l.dedupLen; i++ {
		e := &l.dedup[(l.dedupHead+i)%len(l.dedup)]
		if e.epoch == l.epoch && e.ch == ch && e.corr == corr {
			return e
		}
	}
	return nil
}

func (l *Link) addDedup(ch uint16, corr uint32, resp []byte) {
	if l.dedupLen == MaxDedup {
		l.dedupHead = (l.dedupHead + 1) % len(l.dedup)
		l.dedupLen--
	}
	if l.dedupLen == len(l.dedup) {
		// Grow by REBUILDING in ring order rather than appending. Expiry moves
		// the head, so a full ring is not in general one whose head is 0, and
		// an appended slot would land where the arithmetic expects a different
		// entry -- silently answering one request's retry with another's reply.
		n := len(l.dedup)*2 + 8
		if n > MaxDedup {
			n = MaxDedup
		}
		grown := make([]dedupEntry, n)
		for i := 0; i < l.dedupLen; i++ {
			grown[i] = l.dedup[(l.dedupHead+i)%len(l.dedup)]
		}
		l.dedup, l.dedupHead = grown, 0
	}
	e := &l.dedup[(l.dedupHead+l.dedupLen)%len(l.dedup)]
	e.epoch, e.ch, e.corr, e.tick = l.epoch, ch, corr, l.tick
	e.cached = len(resp) <= MaxDedupPayload
	if e.cached {
		e.resp = append(e.resp[:0], resp...)
	} else {
		e.resp = e.resp[:0]
	}
	l.dedupLen++
}

func (l *Link) expireDedup() {
	for l.dedupLen > 0 {
		e := &l.dedup[l.dedupHead]
		if l.tick-e.tick < DedupTicks {
			return
		}
		e.resp = e.resp[:0]
		l.dedupHead = (l.dedupHead + 1) % len(l.dedup)
		l.dedupLen--
	}
}

// ---------------------------------------------------------------------------
// The frame queue: a ring of reused buffers, so a steady send rate allocates
// nothing after the first pass through it.

type frameQueue struct {
	slot [MaxQueue][]byte
	head int
	n    int
}

func (q *frameQueue) push(f []byte) bool {
	if q.n == MaxQueue {
		return false
	}
	i := (q.head + q.n) % MaxQueue
	q.slot[i] = append(q.slot[i][:0], f...)
	q.n++
	return true
}

func (q *frameQueue) peek() []byte { return q.slot[q.head] }

func (q *frameQueue) drop() {
	q.head = (q.head + 1) % MaxQueue
	q.n--
}

func (q *frameQueue) reset() { q.head, q.n = 0, 0 }
