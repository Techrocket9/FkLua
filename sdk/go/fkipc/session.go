package fkipc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

// The external side's timers, in real time. Their guest counterparts are in
// ticks, and the two are reconciled by the tick every HEARTBEAT carries.
const (
	// DefaultRetryInterval doubles to DefaultRetryCap. The measured round trip
	// through the InputAction path is median 31.5 ms and p90 94.8 ms on a
	// headless server, so this is roughly three times the p90 -- a retry under
	// it would be retransmitting frames that were merely in flight.
	DefaultRetryInterval = 300 * time.Millisecond
	DefaultRetryCap      = 2 * time.Second
	DefaultMaxRetries    = 4

	// DefaultHeartbeat must be comfortably under the guest's LivenessTicks
	// (180 ticks, 3 s at 60 UPS) or the guest declares this side down.
	DefaultHeartbeat = time.Second

	// DefaultQuietAfter is the flow-control threshold, and it is the important
	// half of the pause mitigation. The guest's heartbeats stop when the game
	// does -- paused, saving, or simply slow -- while the OS receive buffer is
	// 256 KB and overflows SILENTLY. So this side goes quiet on the guest's
	// silence rather than trusting a bigger buffer.
	DefaultQuietAfter = 3 * time.Second

	// DefaultReassembly is the guest's ReassemblyTicks (120) in real time.
	DefaultReassembly = 2 * time.Second

	DefaultDedupWindow = 30 * time.Second

	// DefaultRollbackTicks is how far the GUEST'S CLOCK may run backwards
	// before this side calls the session dead. It is in GAME TICKS, because
	// that is the clock being watched; everything else here is in real time.
	//
	// THIS SIDE IS THE ONLY SIDE THAT CAN SEE THIS. A save restored mid-session
	// hands the guest back a link that still knows the epoch and whose per-
	// channel seq counters have gone BACKWARDS, so every telemetry frame it
	// sends reads as d <= 0 here and is dropped as stale -- forever, with
	// heartbeats still flowing and both sides believing the session is healthy.
	// The guest cannot notice: everything it can compute travelled with the
	// save. This side has a clock that did not.
	//
	// SIXTY TICKS -- one second of game time -- and the number is the
	// SELF-HEAL BUDGET rather than a fudge factor. A rollback of R ticks
	// rewinds the guest's seq by whatever it sends in R ticks, and the channel
	// un-wedges by itself once the counter climbs back past where it was: R
	// ticks later, whatever the frame rate. Below the tolerance, waiting is
	// cheaper than a re-handshake, which costs a HELLO round trip and fails
	// every request in flight with ErrSessionLost. Above it -- an autosave
	// restored twenty minutes on -- the channel is deaf for the whole rollback
	// and no amount of waiting fixes it.
	//
	// It is not paying for reordering. The comparison is RFC-1982 serial, so
	// the u32 wrap at ~2.27 years of game time is a step of +1 rather than a
	// regression of 2^32, and the transport is localhost datagrams, which do
	// not reorder. If it ever had to pay for reordering the number would have
	// to clear the heartbeat interval -- which is also 60.
	DefaultRollbackTicks = 60

	// DefaultDedupPayload is far larger than the guest's 512 B, and the
	// asymmetry is the point: the guest's limit is a bound on the SAVE and this
	// side has no save.
	DefaultDedupPayload = 64 << 10

	timerInterval = 25 * time.Millisecond
)

// The errors an application sees.
var (
	// ErrPeerQuiet: the guest has said nothing for QuietAfter, so it is paused,
	// saving, or gone. Sending into that fills an OS buffer that drops
	// silently, so this side stops.
	ErrPeerQuiet = errors.New("fkipc: the guest is quiet -- paused, saving, or gone")
	// ErrNoSession: no HELLO has been seen yet, or the last one was superseded.
	ErrNoSession = errors.New("fkipc: no session")
	// ErrSessionLost: the guest reloaded or reconnected while this was in
	// flight. THE OUTCOME IS UNKNOWN, not "it failed".
	ErrSessionLost = errors.New("fkipc: the session ended before the outcome was known")
	ErrTimeout     = errors.New("fkipc: the guest did not answer")
	ErrTooLarge    = errors.New("fkipc: larger than the negotiated message ceiling")
	ErrSessionShut = errors.New("fkipc: session closed")
)

// PeerError is a RESP that carried the ERROR flag.
type PeerError struct {
	Code    uint16
	Message string
}

func (e *PeerError) Error() string { return "fkipc: peer error: " + e.Message }

// Duplicate reports the code an application must usually handle: the request
// EXECUTED and its result is no longer cached. Re-sending it would execute it
// twice.
func (e *PeerError) Duplicate() bool { return e.Code == wire.CodeDuplicate }

type SessionEvent uint8

const (
	SessionUp SessionEvent = iota
	SessionDown
	// SessionRejected: a HELLO arrived whose identity token is not
	// [Options.ExpectedName], so no session was minted and the guest was told
	// BYE.
	//
	// It is a DISTINCT event rather than silence for one reason: a GUI or a log
	// has to be able to say "the wrong mod is on this port" instead of showing a
	// spinner forever, which is what "never connects" looks like from out here.
	// [Stats.NameRejects] counts them and [Stats.RejectedName] carries the token
	// that was offered; the epoch handed to the callback is 0, because there is
	// no session for it to name.
	//
	// A handler that only cares about up/down needs no change: it is a new
	// constant after the two that existed, so nothing renumbers, and an
	// `if ev == SessionUp` reads it as not-up exactly as it read SessionDown.
	SessionRejected
)

func (e SessionEvent) String() string {
	switch e {
	case SessionUp:
		return "up"
	case SessionRejected:
		return "rejected"
	}
	return "down"
}

// Message is one inbound MSG. Payload belongs to the callback and is not
// reused, so unlike the guest side it may be kept.
type Message struct {
	Channel  uint16
	Seq      uint32
	Snapshot bool
	Payload  []byte
}

// Request is one inbound REQ from the guest.
type Request struct {
	Channel uint16
	Corr    uint32
	Retry   bool
	Payload []byte
}

// FileNotify is what the guest said about a file it wrote or the engine wrote.
type FileNotify struct {
	Channel   uint16
	Name      string
	Bytes     uint32
	Digest    uint32
	HasDigest bool
}

// Stats is the observability snapshot.
type Stats struct {
	Epoch                      uint32
	Up, Quiet                  bool
	Sessions                   uint32
	TxFrames, TxBytes          uint32
	RxFrames, RxBytes          uint32
	Drops                      uint32
	BadFrames, EpochDrops      uint32
	StaleDrops                 uint32
	Gaps                       uint32
	Retries, Timeouts, DupHits uint32
	GuestTick                  uint32
	GuestBoot                  uint32
	// GuestHigh is the highest guest tick this session has seen, which is what
	// a regression is measured against.
	GuestHigh uint32
	// Rollbacks counts sessions this side tore down because the guest's clock
	// went backwards -- a save restored under a session this side never lost.
	// A non-zero value is not an error: it is the recovery working.
	Rollbacks uint32

	// NameRejects counts HELLOs refused because their identity token is not
	// [Options.ExpectedName]. A rising value with Up false is the whole
	// diagnosis: a mod IS talking to this port and it is not the one this
	// companion was built against.
	NameRejects uint32
	// RejectedName is the token the most recent refused HELLO offered.
	//
	// It is a DIAGNOSTIC and last-writer-wins is the right semantic for one: it
	// answers "what is on this port", which is a question about the present. It
	// lives here rather than in the callback's arguments because Stats is
	// already the channel through which everything a GUI displays arrives --
	// epoch, session count, guest tick -- so a card that redraws on a session
	// event picks it up with no new plumbing.
	RejectedName string
}

// Options configures Dial.
type Options struct {
	// GamePort is --enable-lua-udp <port>: the game's ONE socket.
	GamePort uint16
	// ListenPort is ours, and MUST differ from GamePort.
	ListenPort uint16
	// ScriptOutput is where the guest's files land; DefaultScriptOutput() when
	// empty.
	ScriptOutput string
	// MaxFrame is what this side will ACCEPT; 0 means the protocol default.
	MaxFrame uint16
	Logger   *slog.Logger

	// Name is this companion's IDENTITY TOKEN, carried in HELLO_ACK. It is the
	// guest's log line, and -- when the guest sets its own ExpectPeer -- the
	// thing the guest checks before it will adopt the session at all.
	//
	// THE RECOMMENDED CONVENTION IS "<mod-name>/<schema-tag>", where the tag is
	// the author's claim about CHANNEL-CONTRACT compatibility. Setting
	// ExpectedName and leaving this empty uses ExpectedName, because the usual
	// shape is ONE token per pairing: the token names the CONTRACT rather than
	// either party.
	Name string

	// ExpectedName is the identity token this companion requires of the GUEST,
	// and empty means NO CHECK -- which is what every program written before
	// this existed gets, unchanged.
	//
	// Set it and a HELLO whose name differs is refused: no session is minted,
	// nothing about an existing one is disturbed, the guest is told BYE (under
	// the same rate limiter every other unsolicited BYE uses), [Stats.NameRejects]
	// counts it, and OnSession is told [SessionRejected] with the offered token
	// in [Stats.RejectedName].
	//
	// IT CLOSES THE GAP THE PORTS CANNOT. Destination-port routing already keeps
	// two mods' conversations apart, and it answers "did this frame come from
	// the process I bound against". It cannot answer "is that process the one I
	// was BUILT against" -- a swapped port config or a companion left running
	// from last week is a transport handshake that succeeds at every layer and
	// two ends that disagree about what channel 1 means.
	//
	// IT IS A CORRECTNESS CHECK AND NOT AN AUTH BOUNDARY. The token is a
	// constant in a mod zip anybody can read, and the transport is localhost.
	// See agents/ipc.md.
	ExpectedName string

	RetryInterval time.Duration
	MaxRetries    int
	Heartbeat     time.Duration
	QuietAfter    time.Duration
	Reassembly    time.Duration
	DedupWindow   time.Duration

	// PeerTimeout declares the session down after this much silence. ZERO
	// MEANS NEVER, and that is the default on purpose: a paused or saving game
	// is silent for as long as the player likes, and the guest's own liveness
	// is measured in TICKS, which do not advance while it is paused. A session
	// this side gave up on while the guest still had it would drop pending
	// requests the guest is still holding an answer for.
	PeerTimeout time.Duration

	// RollbackTicks is the guest-clock regression, in GAME TICKS, that means
	// the guest was restored from a save under a session this side never lost.
	// Zero means DefaultRollbackTicks; a very large value effectively disables
	// the detector, which is only ever what a test wants.
	RollbackTicks uint32

	// Transport replaces the UDP socket. The conformance suite drives this
	// state machine and the guest's against each other over an in-memory link
	// with an injectable fault model; nothing above the seam knows.
	Transport Transport

	// Manual runs with NO GOROUTINES: the caller drives everything from
	// Session.Pump. It is what makes a fault-injected conformance run
	// deterministic.
	Manual bool

	// Now and Rand are the two impurities, injectable for the same reason.
	Now  func() time.Time
	Rand func() uint32
}

type sdkChannel struct {
	id     uint16
	txSeq  uint32
	rxLast uint32

	// No "gap" flag beside resyncSent: nothing reads one. What a gap means to a
	// channel is that a RESYNC is outstanding, which is what this gates.
	resyncSent bool

	onMessage func(Message)
	onRequest func(Request) ([]byte, error)

	rasmActive   bool
	rasmCorr     uint32
	rasmNFrag    uint8
	rasmType     wire.Type
	rasmFlags    wire.Flags
	rasmDeadline time.Time
	rasmSeen     [wire.MaxFragments]bool
	rasmPart     [wire.MaxFragments][]byte
	rasmGot      uint8
}

type sdkPending struct {
	ch       uint16
	corr     uint32
	msg      []byte
	tries    int
	interval time.Duration
	due      time.Time
	done     func([]byte, error)
}

type dedupKey struct {
	epoch uint32
	ch    uint16
	corr  uint32
}

type dedupVal struct {
	at time.Time
	// inflight marks a request whose handler is RUNNING. Handlers run outside
	// the session mutex -- a Handle that issues its own Request is an ordinary
	// thing to write and would otherwise deadlock -- so without this marker a
	// retry arriving while the first attempt was still executing would execute
	// it a SECOND time, which is the one thing corr-based dedup exists to
	// prevent. It answers BUSY, which is what that code is for.
	inflight bool
	cached   bool
	resp     []byte
}

// Session is one link to one guest.
//
// Every exported method is safe for concurrent use. The state machine itself
// runs under one mutex and never calls an application handler while holding
// it -- a handler that called back into the session would otherwise deadlock,
// and a Handle that issues its own Request is an ordinary thing to write.
type Session struct {
	opt Options
	tr  Transport
	log *slog.Logger
	now func() time.Time
	rnd func() uint32

	mu       sync.Mutex
	epoch    uint32
	up       bool
	quiet    bool
	lastRx   time.Time
	lastTx   time.Time
	lastBye  time.Time
	closed   bool
	guestTk  uint32
	guestHi  uint32
	guestBt  uint32
	maxFrame uint16
	maxFrags uint16

	corrCtr uint32
	chans   map[uint16]*sdkChannel
	pend    map[uint32]*sdkPending
	dedup   map[dedupKey]*dedupVal

	onSession func(SessionEvent, uint32)
	onFile    func(FileNotify, io.ReadCloser)

	stats Stats

	enc  []byte
	ctl  []byte
	done chan struct{}
	wg   sync.WaitGroup
}

// Dial opens the companion side of the link.
//
// It refuses ListenPort == GamePort, which is not a subtle bug: --enable-lua-udp
// binds one socket and it is both the game's receive socket and the source port
// of everything the game sends, so a companion sharing it is the game talking to
// itself. The failure without this check is a session that never receives
// anything and says nothing about why.
func Dial(o Options) (*Session, error) {
	if o.Transport == nil {
		if o.GamePort == 0 || o.ListenPort == 0 {
			return nil, errors.New("fkipc: GamePort and ListenPort are both required")
		}
		if o.ListenPort == o.GamePort {
			return nil, errors.New(
				"fkipc: ListenPort must differ from GamePort -- --enable-lua-udp " +
					"binds ONE socket and it is both the game's receive socket and " +
					"the source port of everything it sends")
		}
	}
	fill(&o)
	s := &Session{
		opt: o, tr: o.Transport, log: o.Logger, now: o.Now, rnd: o.Rand,
		chans: map[uint16]*sdkChannel{},
		pend:  map[uint32]*sdkPending{},
		dedup: map[dedupKey]*dedupVal{},
		done:  make(chan struct{}),
	}
	s.maxFrame = clampFrame(o.MaxFrame)
	s.maxFrags = wire.MaxFragments
	if s.tr == nil {
		t, err := dialUDP(o.ListenPort, o.GamePort)
		if err != nil {
			return nil, err
		}
		s.tr = t
	}
	if !o.Manual {
		s.wg.Add(2)
		go s.recvLoop()
		go s.timerLoop()
	}
	return s, nil
}

func fill(o *Options) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = randU32
	}
	if o.RetryInterval == 0 {
		o.RetryInterval = DefaultRetryInterval
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = DefaultMaxRetries
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = DefaultHeartbeat
	}
	if o.QuietAfter == 0 {
		o.QuietAfter = DefaultQuietAfter
	}
	if o.Reassembly == 0 {
		o.Reassembly = DefaultReassembly
	}
	if o.DedupWindow == 0 {
		o.DedupWindow = DefaultDedupWindow
	}
	if o.RollbackTicks == 0 {
		o.RollbackTicks = DefaultRollbackTicks
	}
	if o.Name == "" {
		// ONE TOKEN NAMES THE CONTRACT, not either party, so a program that
		// states what it requires has by that act also stated what it is.
		// Without this an ExpectedName-only configuration would answer with
		// "fkipc-sdk" and be refused by the very guest it just described.
		o.Name = o.ExpectedName
	}
	if o.Name == "" {
		o.Name = "fkipc-sdk"
	}
}

func randU32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a
		// predictable epoch is a correctness problem rather than a security
		// one -- two sessions must not collide. Panicking is louder than
		// silently minting zero.
		panic("fkipc: no entropy for a session token: " + err.Error())
	}
	return binary.LittleEndian.Uint32(b[:])
}

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

// ---------------------------------------------------------------------------
// The application surface.

func (s *Session) OnSession(h func(ev SessionEvent, epoch uint32)) {
	s.mu.Lock()
	s.onSession = h
	s.mu.Unlock()
}

func (s *Session) Subscribe(channel uint16, h func(Message)) {
	s.mu.Lock()
	s.chan_(channel).onMessage = h
	s.mu.Unlock()
}

// Handle registers a REQ handler. A non-nil error becomes a RESP carrying the
// ERROR flag with code APP and the error's text; the application's own error
// detail belongs inside the payload, where it already has an encoding.
func (s *Session) Handle(channel uint16, h func(Request) ([]byte, error)) {
	s.mu.Lock()
	s.chan_(channel).onRequest = h
	s.mu.Unlock()
}

// OnFile registers the file-pickup handler. See file.go for what "the file is
// ready" means, which is different depending on whether the guest or the engine
// wrote it.
func (s *Session) OnFile(h func(FileNotify, io.ReadCloser)) {
	s.mu.Lock()
	s.onFile = h
	s.mu.Unlock()
}

func (s *Session) Send(channel uint16, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sendable(); err != nil {
		return err
	}
	return s.sendMessage(s.chan_(channel), wire.TypeMsg, 0, 0, p)
}

// Snapshot is Send with the SNAPSHOT flag: a complete state, which clears the
// guest's gap condition on that channel.
func (s *Session) Snapshot(channel uint16, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sendable(); err != nil {
		return err
	}
	return s.sendMessage(s.chan_(channel), wire.TypeMsg, wire.FlagSnapshot, 0, p)
}

// Resync asks the guest for a snapshot of a channel.
func (s *Session) Resync(channel uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sendable(); err != nil {
		return err
	}
	return s.sendMessage(s.chan_(channel), wire.TypeResync, 0, 0, nil)
}

// Request sends a REQ and blocks for the answer.
//
// The same corr is retried on the schedule; the guest keys its dedup table on
// (epoch, channel, corr) and replays rather than re-invoking its handler. So a
// request must be IDEMPOTENT in the sense that asking twice is safe.
func (s *Session) Request(ctx context.Context, channel uint16, p []byte) ([]byte, error) {
	type res struct {
		p   []byte
		err error
	}
	ch := make(chan res, 1)
	if err := s.RequestAsync(channel, p, func(b []byte, err error) {
		ch <- res{b, err}
	}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.p, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RequestAsync is Request without the block, and it is what a Manual session
// uses: a blocking call in a session nobody is pumping would wait forever.
//
// done runs on the session's own goroutine (or inside Pump), with the mutex
// released, so it may call back into the session.
func (s *Session) RequestAsync(channel uint16, p []byte, done func([]byte, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sendable(); err != nil {
		return err
	}
	c := s.chan_(channel)
	corr := s.nextCorr()
	pd := &sdkPending{ch: channel, corr: corr, tries: 0, done: done,
		interval: s.opt.RetryInterval}
	pd.msg = append(pd.msg[:0], p...)
	if err := s.sendMessage(c, wire.TypeReq, 0, corr, p); err != nil {
		return err
	}
	pd.due = s.now().Add(pd.interval)
	s.pend[corr] = pd
	return nil
}

func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Epoch, st.Up, st.Quiet = s.epoch, s.up, s.quiet
	st.GuestTick, st.GuestBoot, st.GuestHigh = s.guestTk, s.guestBt, s.guestHi
	return st
}

// Close says BYE and stops. The BYE is advisory -- the guest recovers from this
// side simply vanishing, by liveness -- but it turns a three-second timeout
// into an immediate one, which matters when the companion is a tool somebody
// restarts often.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.up {
		s.rawSend(wire.Header{Type: wire.TypeBye, Epoch: s.epoch}, nil)
	}
	fails := s.failAllLocked(ErrSessionShut)
	s.mu.Unlock()

	close(s.done)
	err := s.tr.Close()
	s.wg.Wait()
	for _, f := range fails {
		f()
	}
	return err
}

// ---------------------------------------------------------------------------
// Driving.

// Pump is the Manual-mode drive: drain whatever the transport has and run the
// timers once. A pumped session does exactly what a goroutine-driven one does,
// in the caller's own order.
func (s *Session) Pump() {
	for {
		p, ok := s.tr.Poll()
		if !ok {
			break
		}
		s.deliver(p)
	}
	s.runTimers()
}

func (s *Session) recvLoop() {
	defer s.wg.Done()
	for {
		p, err := s.tr.Recv()
		if err != nil {
			return
		}
		s.deliver(p)
	}
}

func (s *Session) timerLoop() {
	defer s.wg.Done()
	t := time.NewTicker(timerInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.runTimers()
		}
	}
}

// after collects the callbacks a locked section wants to run once it is not.
type after []func()

func (s *Session) runTimers() {
	now := s.now()
	s.mu.Lock()
	var post after

	// Quiet, and its release. The guest's silence is the flow-control signal.
	if s.up && !s.lastRx.IsZero() && now.Sub(s.lastRx) > s.opt.QuietAfter {
		if !s.quiet {
			s.quiet = true
			s.log.Info("fkipc: guest quiet, throttling to heartbeats")
		}
	}
	if s.opt.PeerTimeout > 0 && s.up && !s.lastRx.IsZero() &&
		now.Sub(s.lastRx) > s.opt.PeerTimeout {
		post = append(post, s.downLocked()...)
	}

	// Reassembly expiry.
	for _, c := range s.chans {
		if c.rasmActive && now.After(c.rasmDeadline) {
			c.abandon()
		}
	}
	// Dedup expiry.
	for k, v := range s.dedup {
		if now.Sub(v.at) > s.opt.DedupWindow {
			delete(s.dedup, k)
		}
	}

	// Retries. SUSPENDED WHILE QUIET rather than counted: a paused game would
	// otherwise burn the whole retry budget without a frame ever having been
	// lost, and the request would fail the moment play resumed.
	for corr, p := range s.pend {
		if now.Before(p.due) {
			continue
		}
		if s.quiet {
			p.due = now.Add(p.interval)
			continue
		}
		if p.tries >= s.opt.MaxRetries {
			delete(s.pend, corr)
			s.stats.Timeouts++
			d := p.done
			post = append(post, func() { d(nil, ErrTimeout) })
			continue
		}
		p.tries++
		s.stats.Retries++
		s.sendMessage(s.chan_(p.ch), wire.TypeReq, wire.FlagRetry, p.corr, p.msg)
		p.interval *= 2
		if p.interval > DefaultRetryCap {
			p.interval = DefaultRetryCap
		}
		p.due = now.Add(p.interval)
	}

	if s.up && now.Sub(s.lastTx) >= s.opt.Heartbeat {
		s.ctl = wire.AppendHeartbeat(s.ctl[:0], wire.Heartbeat{
			Tick: s.guestTk, Rx: s.stats.RxFrames, Drops: s.stats.Drops,
			Gaps: s.stats.Gaps,
		})
		s.rawSend(wire.Header{Type: wire.TypeHeartbeat, Epoch: s.epoch}, s.ctl)
	}
	s.mu.Unlock()
	post.run()
}

func (a after) run() {
	for _, f := range a {
		f()
	}
}

// ---------------------------------------------------------------------------
// Inbound.

func (s *Session) deliver(dg []byte) {
	s.mu.Lock()
	var post after
	defer func() {
		s.mu.Unlock()
		post.run()
	}()

	s.stats.RxBytes += uint32(len(dg))
	h, p, err := wire.Decode(dg)
	if err != nil {
		s.stats.Drops++
		s.stats.BadFrames++
		if err == wire.ErrVersion {
			s.log.Warn("fkipc: a frame from a protocol version this build does not speak")
		}
		return
	}
	if h.Type == wire.TypeHello {
		post = append(post, s.onHello(h, p)...)
		return
	}
	if !s.up || h.Epoch != s.epoch {
		s.stats.Drops++
		s.stats.EpochDrops++
		// A guest retransmitting into a session nobody remembers is the common
		// shape after a companion restart, and a BYE turns its three-second
		// liveness timeout into an immediate re-HELLO. Rate-limited, because
		// the guest may be mid-retry-storm and this must not amplify it.
		now := s.now()
		if now.Sub(s.lastBye) > s.opt.QuietAfter {
			s.lastBye = now
			s.rawSend(wire.Header{Type: wire.TypeBye, Epoch: h.Epoch}, nil)
		}
		return
	}
	s.stats.RxFrames++
	s.lastRx = s.now()
	if s.quiet {
		s.quiet = false
		s.log.Info("fkipc: guest is back")
	}

	switch h.Type {
	case wire.TypeHeartbeat:
		// THE HEARTBEAT IS THE GUEST'S CLOCK, and this is the only place that
		// clock is read. It is also why the guest heartbeats unconditionally
		// rather than only when it has been quiet: a telemetry-heavy guest that
		// suppressed its heartbeats would leave this reading frozen at the
		// HELLO and the detector below with nothing to detect.
		if hb, err := wire.DecodeHeartbeat(p); err == nil {
			s.guestTk = hb.Tick
			if s.timeTravelled(hb.Tick) {
				post = append(post, s.rollbackLocked(hb.Tick)...)
			}
		}
	case wire.TypeBye:
		post = append(post, s.downLocked()...)
	case wire.TypeMsg, wire.TypeReq, wire.TypeResp, wire.TypeResync, wire.TypeFileNotify:
		post = append(post, s.channelFrame(h, p)...)
	default:
		s.stats.Drops++
		s.stats.BadFrames++
	}
}

// onHello: A HELLO IS ALWAYS A NEW SESSION.
//
// It is not compared against the previous one, and boot is not compared at all:
// boot aliases across two loads of one save, so a peer that trusted it would
// carry state across a boundary the guest has already forgotten. Everything
// about the old session goes, a token is minted from real entropy, and the ACK
// carries it back matched on the HELLO's corr -- which is the one frame the
// guest accepts without an epoch it recognises, because by definition it cannot
// yet.
func (s *Session) onHello(h wire.Header, p []byte) after {
	hello, err := wire.DecodeHello(p)
	if err != nil {
		s.stats.Drops++
		s.stats.BadFrames++
		return nil
	}
	// THE NAME IS THE SCHEMA FILTER, and it is tested HERE -- before
	// resetLocked, before a token is minted, before anything about this session
	// moves. THE ORDERING IS THE ASSERTION: "a HELLO is always a new session" is
	// the rule for a guest this companion is FOR, and a HELLO from a mod it is
	// not for must not be able to tear down a live conversation with the mod it
	// is. Below the check, one stray datagram from a swapped port config would
	// do exactly that.
	//
	// The BYE is advisory and an fkipc guest will DROP it -- it carries the
	// HELLO's own epoch field, which is the guest's boot counter and not a
	// session it has adopted, so it fails the guest's epoch test. It is sent
	// anyway because a refusal that puts NOTHING on the wire is
	// indistinguishable, in a packet capture or in a log, from a companion
	// nobody started, and telling those two apart is the entire point of this
	// feature. It is charged against the SAME rate limiter as the unknown-epoch
	// BYE and the rollback one, because it is the same frame arriving by a third
	// route and a mismatched guest re-HELLOs once a second forever.
	if s.opt.ExpectedName != "" && hello.Name != s.opt.ExpectedName {
		s.stats.Drops++
		s.stats.NameRejects++
		s.stats.RejectedName = hello.Name
		now := s.now()
		if now.Sub(s.lastBye) > s.opt.QuietAfter {
			s.lastBye = now
			s.rawSend(wire.Header{Type: wire.TypeBye, Epoch: h.Epoch}, nil)
		}
		s.log.Warn("fkipc: refused a HELLO whose identity does not match",
			"offered", hello.Name, "expected", s.opt.ExpectedName, "boot", hello.Boot)
		if cb := s.onSession; cb != nil {
			return after{func() { cb(SessionRejected, 0) }}
		}
		return nil
	}
	post := s.resetLocked(ErrSessionLost)

	// A NEW TOKEN EVERY TIME, and the loop is the assertion rather than a
	// retry: reusing the previous session's value would make two loads of one
	// save indistinguishable on the wire, which is the exact property the token
	// exists to provide. 0 is excluded because it means "no epoch", and the
	// guest's boot is excluded because a HELLO carries it in the epoch field.
	for {
		e := s.rnd()
		if e != 0 && e != h.Epoch && e != s.epoch {
			s.epoch = e
			break
		}
	}
	s.up = true
	s.quiet = false
	s.lastRx = s.now()
	s.guestBt, s.guestTk, s.guestHi = hello.Boot, hello.Tick, hello.Tick
	s.maxFrame = clampFrame(hello.MaxFrame)
	s.maxFrags = hello.MaxFragments
	if s.maxFrags == 0 || s.maxFrags > wire.MaxFragments {
		s.maxFrags = wire.MaxFragments
	}
	s.stats.Sessions++
	s.stats.RxFrames++

	s.ctl, err = wire.AppendHello(s.ctl[:0], wire.Hello{
		ProtoMin: wire.Version, ProtoMax: wire.Version,
		MaxFrame: clampFrame(s.opt.MaxFrame), MaxFragments: wire.MaxFragments,
		Boot: 0, Tick: hello.Tick, Profile: hello.Profile, Name: s.opt.Name,
	})
	if err != nil {
		return post
	}
	s.rawSend(wire.Header{Type: wire.TypeHelloAck, Epoch: s.epoch, Corr: h.Corr}, s.ctl)

	ep, cb := s.epoch, s.onSession
	if cb != nil {
		post = append(post, func() { cb(SessionUp, ep) })
	}
	s.log.Info("fkipc: session up", "epoch", s.epoch, "guest", hello.Name,
		"boot", hello.Boot, "profile", hello.Profile.String())
	return post
}

// timeTravelled reports whether the guest's clock has gone backwards far enough
// to mean a save was restored under a session this side never lost, and
// advances the high-water mark when it has not.
//
// A MAXIMUM, NOT THE LAST READING, and the difference matters: measuring
// against the previous heartbeat would make one stale datagram look like a
// rollback and then make the real rollback look like a recovery. The comparison
// is RFC-1982 serial (SerialDelta is int32(a-b)), so the u32 wrap is a forward
// step of +1 and needs no special case -- which is the same arithmetic, and the
// same reasoning, as the per-channel seq comparison two functions down. The
// widening to int64 is so that a caller who sets RollbackTicks to something
// enormous to switch the detector off gets that rather than an overflow.
func (s *Session) timeTravelled(tick uint32) bool {
	d := int64(wire.SerialDelta(tick, s.guestHi))
	if d > 0 {
		s.guestHi = tick
		return false
	}
	return -d > int64(s.opt.RollbackTicks)
}

// rollbackLocked tears the session down and tells the guest, which is the whole
// recovery: a BYE arrives at the guest through recv_udp, which is an
// InputAction, which the engine delivers to EVERY peer at the same tick -- so a
// multiplayer game resets identically everywhere and re-HELLOs, where a guest
// that decided this for itself out of local knowledge would desync.
//
// The BYE goes out BEFORE downLocked, because downLocked clears the epoch and a
// BYE at epoch 0 is a frame the guest drops.
func (s *Session) rollbackLocked(tick uint32) after {
	s.stats.Rollbacks++
	s.log.Warn("fkipc: the guest's clock went backwards -- a save was restored "+
		"under a session this side never lost; tearing it down so both sides "+
		"re-handshake", "epoch", s.epoch, "high", s.guestHi, "now", tick,
		"tolerance", s.opt.RollbackTicks)
	s.rawSend(wire.Header{Type: wire.TypeBye, Epoch: s.epoch}, nil)
	// Charged against the same rate limiter the unknown-epoch BYE uses, because
	// it is the same BYE arriving by a different route: the guest is about to
	// keep talking under an epoch this side has just forgotten, and answering
	// each of those with another BYE would amplify a retry storm the guest is
	// already in.
	s.lastBye = s.now()
	return s.downLocked()
}

func (s *Session) channelFrame(h wire.Header, p []byte) after {
	c := s.chan_(h.Channel)
	if h.Channel != 0 {
		// A SNAPSHOT resets last rather than advancing it, and is exempt from
		// the staleness rule. See the guest half for the argument -- the two
		// sides must agree on this or a rescued channel is rescued at one end
		// only.
		snapshot := h.Type == wire.TypeMsg && h.Flags.Has(wire.FlagSnapshot)
		d := wire.SerialDelta(h.Seq, c.rxLast)
		if d <= 0 && !snapshot {
			s.stats.Drops++
			s.stats.StaleDrops++
			return nil
		}
		if d > 1 && !snapshot {
			s.stats.Gaps++
			if !c.resyncSent {
				c.resyncSent = true
				s.sendMessage(c, wire.TypeResync, 0, 0, nil)
			}
		}
		c.rxLast = h.Seq
	}
	if h.NFrag == 1 {
		return s.dispatch(c, h, p)
	}
	return s.reassemble(c, h, p)
}

func (c *sdkChannel) abandon() {
	c.rasmActive = false
	c.rasmGot, c.rasmCorr, c.rasmNFrag = 0, 0, 0
	for i := range c.rasmSeen {
		c.rasmSeen[i] = false
		c.rasmPart[i] = c.rasmPart[i][:0]
	}
}

func (s *Session) reassemble(c *sdkChannel, h wire.Header, p []byte) after {
	if int(h.NFrag) > wire.MaxFragments {
		c.abandon()
		s.stats.Drops++
		s.stats.BadFrames++
		return nil
	}
	if c.rasmActive && h.Corr != c.rasmCorr {
		c.abandon()
	}
	if c.rasmActive && h.NFrag != c.rasmNFrag {
		c.abandon()
		s.stats.Drops++
		s.stats.BadFrames++
		return nil
	}
	if !c.rasmActive {
		c.rasmActive = true
		c.rasmCorr, c.rasmNFrag = h.Corr, h.NFrag
		c.rasmType, c.rasmFlags = h.Type, h.Flags
		c.rasmGot = 0
		for i := range c.rasmSeen {
			c.rasmSeen[i] = false
		}
	}
	c.rasmDeadline = s.now().Add(s.opt.Reassembly)
	if !c.rasmSeen[h.Frag] {
		c.rasmSeen[h.Frag] = true
		c.rasmGot++
	}
	c.rasmPart[h.Frag] = append(c.rasmPart[h.Frag][:0], p...)
	if c.rasmGot < c.rasmNFrag {
		return nil
	}
	var msg []byte
	for k := 0; k < int(c.rasmNFrag); k++ {
		msg = append(msg, c.rasmPart[k]...)
	}
	h.Type, h.Flags, h.Corr = c.rasmType, c.rasmFlags, c.rasmCorr
	c.abandon()
	return s.dispatch(c, h, msg)
}

func (s *Session) dispatch(c *sdkChannel, h wire.Header, payload []byte) after {
	switch h.Type {
	case wire.TypeMsg:
		if h.Flags.Has(wire.FlagSnapshot) {
			c.resyncSent = false
		}
		if c.onMessage == nil {
			return nil
		}
		m := Message{Channel: c.id, Seq: h.Seq,
			Snapshot: h.Flags.Has(wire.FlagSnapshot),
			Payload:  append([]byte(nil), payload...)}
		cb := c.onMessage
		return after{func() { cb(m) }}

	case wire.TypeFileNotify:
		fn, err := wire.DecodeFileNotify(payload)
		if err != nil {
			s.stats.Drops++
			s.stats.BadFrames++
			return nil
		}
		if s.onFile == nil {
			return nil
		}
		n := FileNotify{Channel: c.id, Name: fn.Name, Bytes: fn.Bytes,
			Digest: fn.FNV1a32, HasDigest: h.Flags.Has(wire.FlagHasDigest)}
		cb, dir := s.onFile, s.opt.ScriptOutput
		return after{func() { pickUp(dir, n, cb, s.log) }}

	case wire.TypeResync:
		// The application answers a resync with Snapshot on that channel; there
		// is no separate handler because there is nothing else it could do.
		if c.onMessage == nil {
			return nil
		}
		cb := c.onMessage
		return after{func() {
			cb(Message{Channel: c.id, Seq: h.Seq, Snapshot: true, Payload: nil})
		}}

	case wire.TypeReq:
		return s.onReq(c, h, payload)

	case wire.TypeResp:
		return s.onResp(h, payload)
	}
	return nil
}

func (s *Session) onReq(c *sdkChannel, h wire.Header, payload []byte) after {
	k := dedupKey{s.epoch, c.id, h.Corr}
	if v, ok := s.dedup[k]; ok {
		s.stats.DupHits++
		switch {
		case v.inflight:
			s.respondError(c, h.Corr, wire.CodeBusy, "still running")
		case v.cached:
			s.sendMessage(c, wire.TypeResp, wire.FlagRetry, h.Corr, v.resp)
		default:
			s.respondError(c, h.Corr, wire.CodeDuplicate, "result was not cached")
		}
		return nil
	}
	if c.onRequest == nil {
		s.respondError(c, h.Corr, wire.CodeNoHandler, "")
		return nil
	}
	// The handler runs OUTSIDE the lock, because a handler issuing its own
	// Request is an ordinary thing to write and would otherwise deadlock.
	cb := c.onRequest
	req := Request{Channel: c.id, Corr: h.Corr, Retry: h.Flags.Has(wire.FlagRetry),
		Payload: append([]byte(nil), payload...)}
	corr := h.Corr
	s.dedup[k] = &dedupVal{at: s.now(), inflight: true}
	return after{func() {
		out, err := cb(req)
		s.mu.Lock()
		defer s.mu.Unlock()
		ch := s.chan_(req.Channel)
		if err != nil {
			delete(s.dedup, dedupKey{s.epoch, req.Channel, corr})
			s.respondError(ch, corr, wire.CodeApp, err.Error())
			return
		}
		e := &dedupVal{at: s.now(), cached: len(out) <= DefaultDedupPayload}
		if e.cached {
			e.resp = append([]byte(nil), out...)
		}
		s.dedup[dedupKey{s.epoch, req.Channel, corr}] = e
		s.sendMessage(ch, wire.TypeResp, 0, corr, out)
	}}
}

func (s *Session) onResp(h wire.Header, payload []byte) after {
	p, ok := s.pend[h.Corr]
	if !ok {
		s.stats.Drops++
		s.stats.StaleDrops++
		return nil
	}
	delete(s.pend, h.Corr)
	d := p.done
	if h.Flags.Has(wire.FlagError) {
		rec, err := wire.DecodeErrorRecord(payload)
		if err != nil {
			s.stats.Drops++
			s.stats.BadFrames++
			return nil
		}
		e := &PeerError{Code: rec.Code, Message: rec.Message}
		return after{func() { d(nil, e) }}
	}
	out := append([]byte(nil), payload...)
	return after{func() { d(out, nil) }}
}

func (s *Session) respondError(c *sdkChannel, corr uint32, code uint16, msg string) {
	var err error
	s.ctl, err = wire.AppendErrorRecord(s.ctl[:0], wire.ErrorRecord{Code: code, Message: msg})
	if err != nil {
		return
	}
	s.sendMessage(c, wire.TypeResp, wire.FlagError, corr, s.ctl)
}

// ---------------------------------------------------------------------------
// Outbound and session bookkeeping.

func (s *Session) sendable() error {
	if s.closed {
		return ErrSessionShut
	}
	if !s.up {
		return ErrNoSession
	}
	if s.quiet {
		return ErrPeerQuiet
	}
	return nil
}

func (s *Session) chan_(id uint16) *sdkChannel {
	c, ok := s.chans[id]
	if !ok {
		c = &sdkChannel{id: id}
		s.chans[id] = c
	}
	return c
}

func (s *Session) nextCorr() uint32 {
	s.corrCtr++
	if s.corrCtr == 0 {
		s.corrCtr = 1
	}
	return s.corrCtr
}

func (s *Session) rawSend(h wire.Header, payload []byte) {
	var err error
	s.enc, err = wire.AppendFrame(s.enc[:0], h, payload)
	if err != nil {
		return
	}
	if err := s.tr.Send(s.enc); err != nil {
		s.log.Warn("fkipc: send failed", "err", err)
		return
	}
	s.stats.TxFrames++
	s.stats.TxBytes += uint32(len(s.enc))
	s.lastTx = s.now()
}

func (s *Session) sendMessage(c *sdkChannel, ty wire.Type, flags wire.Flags,
	corr uint32, payload []byte) error {

	room := int(s.maxFrame) - wire.HeaderBytes
	if room <= 0 {
		return ErrTooLarge
	}
	n := (len(payload) + room - 1) / room
	if n == 0 {
		n = 1
	}
	if n > int(s.maxFrags) {
		return ErrTooLarge
	}
	if n > 1 && corr == 0 {
		corr = s.nextCorr()
	}
	for i := 0; i < n; i++ {
		lo, hi := i*room, (i+1)*room
		if hi > len(payload) {
			hi = len(payload)
		}
		seq := uint32(0)
		if c.id != 0 {
			c.txSeq++
			seq = c.txSeq
		}
		s.rawSend(wire.Header{
			Type: ty, Flags: flags, Channel: c.id, Epoch: s.epoch,
			Seq: seq, Corr: corr, Frag: uint8(i), NFrag: uint8(n),
		}, payload[lo:hi])
	}
	return nil
}

// resetLocked drops everything about the current session. Returns the callbacks
// to run once the lock is released.
func (s *Session) resetLocked(cause error) after {
	post := s.failAllLocked(cause)
	for _, c := range s.chans {
		c.txSeq, c.rxLast = 0, 0
		c.resyncSent = false
		c.abandon()
	}
	s.dedup = map[dedupKey]*dedupVal{}
	return post
}

func (s *Session) downLocked() after {
	if !s.up {
		return nil
	}
	post := s.resetLocked(ErrSessionLost)
	s.up = false
	ep, cb := s.epoch, s.onSession
	s.epoch = 0
	if cb != nil {
		post = append(post, func() { cb(SessionDown, ep) })
	}
	return post
}

func (s *Session) failAllLocked(cause error) after {
	var post after
	for corr, p := range s.pend {
		d := p.done
		delete(s.pend, corr)
		post = append(post, func() { d(nil, cause) })
	}
	return post
}
