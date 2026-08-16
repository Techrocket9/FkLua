package fkipc_test

import (
	"testing"
	"time"

	guestipc "github.com/Techrocket9/fklua/guest/go/fkipc"
	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	sdkipc "github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// THE CONFORMANCE HARNESS: the shipping guest state machine and the shipping
// SDK, in one process, over an in-memory link with an injectable fault model
// and a fake clock.
//
// The reference implementation of the guest half IS guest/go/fkipc compiled for
// the host -- that is what its transport seam is for, and it is what makes this
// layer worth having rather than a second thing to keep in sync. Neither wasm
// nor Factorio nor a toolchain is involved, which matters because CI has none
// of them.
//
// What it cannot say is anything about the real transport: that a datagram
// really carries NUL bytes, that recv_udp really drains a backlog in one tick,
// that an oversized send really fails silently. Those are the probe's, and the
// constants they produced are what this suite runs against.

const tickDur = 16667 * time.Microsecond

type dir int

const (
	toSDK dir = iota
	toGuest
)

// bus is the wire. A fault function sees every frame and returns what actually
// lands -- none of it to drop, two copies to duplicate, a shortened copy to
// truncate -- which is enough to express every failure this protocol claims to
// survive without any of them being a special case in the harness.
type bus struct {
	q     [2][][]byte
	fault func(d dir, frame []byte) [][]byte
	log   [2][][]byte // everything ever offered, faults not applied
}

func (b *bus) push(d dir, frame []byte) {
	cp := append([]byte(nil), frame...)
	b.log[d] = append(b.log[d], cp)
	out := [][]byte{cp}
	if b.fault != nil {
		out = b.fault(d, cp)
	}
	b.q[d] = append(b.q[d], out...)
}

func (b *bus) pop(d dir) ([]byte, bool) {
	if len(b.q[d]) == 0 {
		return nil, false
	}
	p := b.q[d][0]
	b.q[d] = b.q[d][1:]
	return p, true
}

// inject puts a frame in front of the guest with no fault function and no
// sender, which is how a test crafts a header it could not otherwise produce --
// a seq at the u32 wrap, an nfrag that disagrees with itself.
func (b *bus) inject(frame []byte) { b.injectTo(toGuest, frame) }

// injectTo is inject in either direction. Toward the SDK it is how a test says
// "the guest was restored from a save": a heartbeat whose tick has gone
// backwards is a frame a real guest emits only after a rollback, and there is
// no way to roll a live Link back from outside it.
func (b *bus) injectTo(d dir, frame []byte) {
	b.q[d] = append(b.q[d], append([]byte(nil), frame...))
}

// ---------------------------------------------------------------------------

type guestEnd struct {
	b   *bus
	ver guestipc.Version
	// src is the port the SDK end's datagrams arrive FROM, which the guest
	// link tests against its configured peer port. It is guestPeerPort here:
	// this harness is one game running one ipc mod.
	src   uint16
	files map[string][]byte
	logs  []string
}

// guestPeerPort is the guest Config.Port every harness uses, and therefore the
// port guestEnd's datagrams must appear to come from.
const guestPeerPort = 29434

// Send takes no status back, because the guest seam has none to take: whether
// send_udp works is a fact about how a peer was launched, and a value there is
// a word in storage.fk_mem that differs between two peers of one game. A test
// double may still RECORD -- the bus is host-side and no guest code can reach
// it -- which is what every assertion in this file reads.
func (g *guestEnd) Send(frame []byte) { g.b.push(toSDK, frame) }

// Poll drains everything queued and reports false, which is the measured shape:
// twenty packets blasted in 0.34 ms all arrived within one tick, in order,
// complete, from one recv_udp.
func (g *guestEnd) Poll(deliver func(uint16, []byte)) bool {
	for {
		p, ok := g.b.pop(toGuest)
		if !ok {
			return false
		}
		// The SDK end is the guest's configured peer, so its datagrams arrive
		// from the configured port -- which is what makes this harness model a
		// game running ONE ipc mod. The cross-mod case is
		// guest/go/fkipc's TestAFrameFromAnotherModsCompanionIsRefused.
		deliver(g.src, p)
	}
}

func (g *guestEnd) Event(id, ptr uint32, deliver func(uint16, []byte)) bool { return false }

func (g *guestEnd) WriteFile(name string, data []byte) {
	if g.files == nil {
		g.files = map[string][]byte{}
	}
	g.files[name] = append([]byte(nil), data...)
}

func (g *guestEnd) BaseVersion() (guestipc.Version, bool) { return g.ver, true }

// Log records the engine gate's one line. Host-side, unreachable from guest
// code, and per-peer by nature: the game log is not CRC'd, which is exactly
// what makes it the sanctioned sink for a fact about how this peer was
// launched -- and exactly why nothing in the library may read it back.
func (g *guestEnd) Log(msg string) { g.logs = append(g.logs, msg) }

type sdkEnd struct{ b *bus }

func (s *sdkEnd) Send(p []byte) error {
	s.b.push(toGuest, p)
	return nil
}

func (s *sdkEnd) Recv() ([]byte, error) { return nil, sdkipc.ErrClosed }

func (s *sdkEnd) Poll() ([]byte, bool) { return s.b.pop(toSDK) }

func (s *sdkEnd) Close() error { return nil }

// ---------------------------------------------------------------------------

type harness struct {
	t    *testing.T
	b    *bus
	ge   *guestEnd
	g    *guestipc.Link
	s    *sdkipc.Session
	tick uint32
	now  time.Time

	gcfg guestipc.Config
}

type opts struct {
	guestMaxFrame uint16
	sdkMaxFrame   uint16
	baseVersion   guestipc.Version
	profile       guestipc.Profile
	rnd           func() uint32
	scriptOutput  string

	// The four identity fields, all empty by default so every test written
	// before identities existed runs against a pair that checks nothing.
	//
	// A name is left EMPTY rather than defaulted here whenever an expectation is
	// set beside it, so what such a test observes is the LIBRARY's own "one
	// token names the contract" defaulting rather than the harness's idea of it.
	guestName       string
	guestExpectPeer string
	sdkName         string
	sdkExpectedName string
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	if o.baseVersion == (guestipc.Version{}) {
		o.baseVersion = guestipc.MinEngineVersion
	}
	h := &harness{
		t:   t,
		b:   &bus{},
		now: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
	}
	h.ge = &guestEnd{b: h.b, ver: o.baseVersion, src: guestPeerPort}

	// A deterministic token source by default: the tests that care about
	// uniqueness assert on the values, and a real crypto/rand would make a
	// failure unreproducible.
	tok := uint32(0x1000)
	rnd := o.rnd
	if rnd == nil {
		rnd = func() uint32 { tok += 0x1111; return tok }
	}

	sdkName := o.sdkName
	if sdkName == "" && o.sdkExpectedName == "" {
		sdkName = "harness"
	}
	s, err := sdkipc.Dial(sdkipc.Options{
		Transport:    &sdkEnd{b: h.b},
		Manual:       true,
		MaxFrame:     o.sdkMaxFrame,
		ScriptOutput: o.scriptOutput,
		Now:          func() time.Time { return h.now },
		Rand:         rnd,
		Name:         sdkName,
		ExpectedName: o.sdkExpectedName,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	guestName := o.guestName
	if guestName == "" && o.guestExpectPeer == "" {
		guestName = "guest"
	}
	h.gcfg = guestipc.Config{
		Port: guestPeerPort, Profile: o.profile, MaxFrame: o.guestMaxFrame,
		Name: guestName, ExpectPeer: o.guestExpectPeer,
	}
	h.g = h.newGuest()
	return h
}

// newGuest attaches a fresh link over the same wire. It models what a LOAD does
// to guest memory -- _initialize rebuilds the package state and the saved bytes
// then replace it -- which is why the boot counter is handed over separately.
func (h *harness) newGuest() *guestipc.Link {
	g, st := guestipc.Attach(h.gcfg, h.ge)
	// StatusDisabled is a verdict about the engine this harness told the
	// transport to report, not a failure to attach: the sub-floor test wants a
	// live object it can call and count refusals on.
	if st != guestipc.StatusOK && st != guestipc.StatusDisabled {
		h.t.Fatalf("Attach: %v", st)
	}
	if g == nil {
		h.t.Fatalf("Attach returned no link (%v)", st)
	}
	return g
}

func (h *harness) step(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.tick++
		h.now = h.now.Add(tickDur)
		h.g.Pump(h.tick)
		h.s.Pump()
	}
}

// up brings the session all the way to SessionUp and asserts it got there.
func (h *harness) up() {
	h.t.Helper()
	h.step(4)
	if !h.g.Stats().Up {
		h.t.Fatalf("no session after four ticks: guest %+v sdk %+v",
			h.g.Stats(), h.s.Stats())
	}
}

// frames decodes everything that has ever been offered in one direction, faults
// not applied -- so a test can assert on what a side TRIED to send even when
// the harness dropped it.
func (h *harness) frames(d dir) []wire.Header {
	var out []wire.Header
	for _, f := range h.b.log[d] {
		hd, _, err := wire.Decode(f)
		if err != nil {
			continue
		}
		out = append(out, hd)
	}
	return out
}

func (h *harness) count(d dir, ty wire.Type) int {
	n := 0
	for _, hd := range h.frames(d) {
		if hd.Type == ty {
			n++
		}
	}
	return n
}

// craft builds a frame as if the SDK had sent it, so a test can put a header on
// the wire that neither implementation would produce.
func craft(h wire.Header, payload []byte) []byte {
	f, err := wire.AppendFrame(nil, h, payload)
	if err != nil {
		panic(err)
	}
	return f
}
