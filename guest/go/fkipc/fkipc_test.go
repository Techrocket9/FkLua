package fkipc_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Techrocket9/fklua/guest/go/fkipc"
	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

// The protocol behaviour lives in sdk/go/fkipc's conformance suite, which drives
// this state machine against the real SDK. What is here is the half that has no
// peer: the version parse the gate is built on, and the two bounds that protect
// guest memory.

func TestParseVersion(t *testing.T) {
	ok := []struct {
		in   string
		want fkipc.Version
	}{
		{"2.1.14", fkipc.Version{Major: 2, Minor: 1, Patch: 14}},
		{"0.0.0", fkipc.Version{}},
		{"2.0.77", fkipc.Version{Major: 2, Minor: 0, Patch: 77}},
		// helpers.game_version is documented as the version and nothing more,
		// but a trailing build tag has appeared in Factorio's version strings
		// before and must not make the whole read fail closed.
		{"2.1.14 (build 84539)", fkipc.Version{Major: 2, Minor: 1, Patch: 14}},
		{"10.20.30", fkipc.Version{Major: 10, Minor: 20, Patch: 30}},
	}
	for _, c := range ok {
		got, good := fkipc.ParseVersion(c.in)
		if !good || got != c.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, true", c.in, got, good, c.want)
		}
	}
	for _, bad := range []string{"", "2", "2.1", "2..1", "x.y.z", "2.1.x", "-1.0.0",
		"999999.0.0", "2.1."} {
		if got, good := fkipc.ParseVersion(bad); good {
			t.Errorf("ParseVersion(%q) accepted it as %v", bad, got)
		}
	}
}

// Less is the gate's whole decision, so both directions and the equal case are
// pinned rather than assumed. At the floor the gate OPENS -- MinEngineVersion
// is the version that was measured working, not the one after it.
func TestVersionOrdering(t *testing.T) {
	f := fkipc.MinEngineVersion
	below := []fkipc.Version{
		{Major: 2, Minor: 0, Patch: 77},
		{Major: 2, Minor: 1, Patch: 13},
		{Major: 1, Minor: 9, Patch: 99},
		{},
	}
	for _, v := range below {
		if !v.Less(f) {
			t.Errorf("%v is not below the floor %v", v, f)
		}
	}
	for _, v := range []fkipc.Version{f, {Major: 2, Minor: 1, Patch: 15},
		{Major: 2, Minor: 2, Patch: 0}, {Major: 3, Minor: 0, Patch: 0}} {
		if v.Less(f) {
			t.Errorf("%v reads as below the floor %v", v, f)
		}
	}
	if f.String() != "2.1.14" {
		t.Errorf("MinEngineVersion is %s -- the floor is the version the probe "+
			"MEASURED working, so moving it wants a probe run and not an "+
			"argument from a changelog", f)
	}
}

// A transport that says nothing and accepts everything, so the bounds can be
// exercised without a peer.
type nullTransport struct {
	ver   fkipc.Version
	sent  int
	fail  bool
	last  []byte
	inbox []datagram
	logs  []string
}

// Send RECORDS but does not REPORT, which is the whole shape of a test double
// under a void seam: `sent` and `last` are fields of a host-side struct that no
// library code can reach, so a test may assert on them from outside the state
// machine without any of it becoming guest state. `fail` still models a peer
// whose socket is not bound -- the frame simply does not land -- and the point
// of TestAFailedSendIsInvisibleToGuestState is that the link cannot tell.
func (n *nullTransport) Send(frame []byte) {
	if n.fail {
		return
	}
	n.sent++
	n.last = append(n.last[:0], frame...)
}

// inbox is what a Poll delivers, each entry with the port it came FROM, which
// is the whole subject of TestAFrameFromAnotherModsCompanionIsRefused.
func (n *nullTransport) Poll(deliver func(uint16, []byte)) bool {
	if len(n.inbox) == 0 {
		return false
	}
	batch := n.inbox
	n.inbox = nil
	for _, d := range batch {
		deliver(d.src, d.dg)
	}
	return false
}

func (n *nullTransport) Event(uint32, uint32, func(uint16, []byte)) bool { return false }

func (n *nullTransport) WriteFile(string, []byte) {}

func (n *nullTransport) BaseVersion() (fkipc.Version, bool) { return n.ver, true }

// logs is where the engine gate's one line lands. It is a host-side field no
// library code can reach, exactly like `sent`: the game log is per-peer and not
// CRC'd, so recording it here is the test looking at something guest state may
// never look at.
func (n *nullTransport) Log(msg string) { n.logs = append(n.logs, msg) }

type datagram struct {
	src uint16
	dg  []byte
}

// TWO IPC MODS IN ONE GAME SHARE ONE SOCKET, and this is the property that
// makes that safe. --enable-lua-udp binds a single socket for the whole game,
// so on_udp_packet_received fires in EVERY mod for EVERY mod's datagrams, and
// the only thing in the event that distinguishes them is the sender's port.
//
// The frame this test injects is not junk: it is a well-formed HELLO_ACK
// carrying the corr of the HELLO this guest just sent. HELLO_ACK is the ONE
// frame matched on corr with the epoch test skipped -- it must be, because it
// carries an epoch the guest cannot yet know -- and corr is minted from a
// counter, so a second freshly-loaded guest's first HELLO carries the same
// corr = 1. Without the source-port test this link adopts the OTHER mod's
// session token and then talks to its own companion under an epoch that
// companion has never heard of.
func TestAFrameFromAnotherModsCompanionIsRefused(t *testing.T) {
	const ourPeer, otherPeer = 29434, 29437

	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{Port: ourPeer}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l.Pump(1) // sends the HELLO
	if tr.sent != 1 {
		t.Fatalf("%d frames sent on the first pump, want the HELLO", tr.sent)
	}
	hello := tr.last
	h, _, err := wire.Decode(hello)
	if err != nil || h.Type != wire.TypeHello {
		t.Fatalf("the first frame is not a HELLO: %v %v", h.Type, err)
	}

	ack := mustAck(t, h.Corr, 0xC0FFEE01)

	// The other mod's companion, on the same corr.
	tr.inbox = []datagram{{src: otherPeer, dg: ack}}
	l.Pump(2)
	if l.Stats().Up {
		t.Fatal("the link adopted a session from a port that is not its peer's " +
			"-- this is the corr collision two IPC mods in one game produce, " +
			"and the epoch filter cannot catch it because HELLO_ACK is the one " +
			"frame exempt from the epoch test")
	}
	if got := l.Stats().ForeignDrops; got != 1 {
		t.Errorf("ForeignDrops %d, want 1 -- the refusal has to be countable or "+
			"a misconfigured port is indistinguishable from a dead companion", got)
	}
	if got := l.Stats().RxBytes; got != 0 {
		t.Errorf("RxBytes %d: a foreign datagram was charged to this session's "+
			"own byte accounting", got)
	}

	// And the real companion, on the same corr, still works.
	tr.inbox = []datagram{{src: ourPeer, dg: mustAck(t, h.Corr, 0xC0FFEE02)}}
	l.Pump(3)
	if !l.Stats().Up || l.Stats().Epoch != 0xC0FFEE02 {
		t.Fatalf("the configured peer's ACK was not adopted: up=%v epoch=%#x",
			l.Stats().Up, l.Stats().Epoch)
	}

	// A BYE AT THE LIVE EPOCH FROM THE WRONG PORT, which is the exact frame
	// scripts/run-ipcdemo.sh's foreign-port leg puts on the real wire. It is
	// the loudest thing a stray sender can do -- one datagram ends the session
	// -- and it passes the epoch test by construction, so the source port is
	// the ONLY thing standing between another mod's companion and this mod's
	// session. The positive control is the line below it.
	bye, err := wire.AppendFrame(nil, wire.Header{
		Type: wire.TypeBye, Epoch: l.Stats().Epoch,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr.inbox = []datagram{{src: otherPeer, dg: bye}}
	l.Pump(4)
	if !l.Stats().Up {
		t.Fatal("a BYE from another mod's companion ended the session")
	}
	tr.inbox = []datagram{{src: ourPeer, dg: bye}}
	l.Pump(5)
	if l.Stats().Up {
		t.Fatal("the SAME BYE from the configured port did NOT end the session, " +
			"so the assertion above passes for the wrong reason")
	}
}

// A source port of ZERO is accepted, deliberately: zero is not a valid UDP
// source port, so it means "the engine did not say", and refusing on silence
// would make a guest deaf on any build that stops reporting the field.
// Deafness is silent and total; cross-talk is loud and recoverable.
func TestAnUnreportedSourcePortIsAccepted(t *testing.T) {
	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{Port: 29434}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l.Pump(1)
	h, _, err := wire.Decode(tr.last)
	if err != nil {
		t.Fatal(err)
	}
	tr.inbox = []datagram{{src: 0, dg: mustAck(t, h.Corr, 0xABCD1234)}}
	l.Pump(2)
	if !l.Stats().Up {
		t.Error("a datagram whose source port the engine did not report was " +
			"refused; a build that stops filling the field must not make " +
			"every IPC guest deaf")
	}
}

func mustAck(t *testing.T, corr, epoch uint32) []byte {
	t.Helper()
	return mustAckNamed(t, corr, epoch, "other")
}

// mustAckNamed is mustAck with the peer's IDENTITY TOKEN spelled out, which is
// what Config.ExpectPeer is tested against.
func mustAckNamed(t *testing.T, corr, epoch uint32, name string) []byte {
	t.Helper()
	ctl, err := wire.AppendHello(nil, wire.Hello{
		ProtoMin: wire.Version, ProtoMax: wire.Version,
		MaxFrame: wire.DefaultMaxFrame, MaxFragments: wire.MaxFragments,
		Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := wire.AppendFrame(nil, wire.Header{
		Type: wire.TypeHelloAck, Epoch: epoch, Corr: corr,
	}, ctl)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// THE NAME IS THE SCHEMA FILTER, and it is the only one of the four mechanisms
// that can refuse a peer whose TRANSPORT is entirely correct.
//
// The frame here comes from the configured port, carries the corr of the HELLO
// this guest just sent, and decodes cleanly -- it passes the mod filter, it
// passes the one epoch-test exemption, and every layer below this one is
// satisfied. What is wrong with it is the only thing left: it is a different
// application. That is a swapped port config or a companion left running from
// last week, and without this check it is a session that comes up and then
// disagrees with itself about what channel 1 means.
func TestAHelloAckFromTheWrongApplicationIsNotAdopted(t *testing.T) {
	const ourPeer = 29434

	// THE CONTROL FIRST, because it is what makes the refusal below a fact
	// about the TOKEN rather than about the frame: the identical ACK, at a link
	// that states no expectation, is adopted.
	loose := &nullTransport{ver: fkipc.MinEngineVersion}
	l0, st := fkipc.Attach(fkipc.Config{Port: ourPeer, Name: "app/1"}, loose)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l0.Pump(1)
	h0, _, err := wire.Decode(loose.last)
	if err != nil {
		t.Fatal(err)
	}
	loose.inbox = []datagram{{src: ourPeer,
		dg: mustAckNamed(t, h0.Corr, 0xAAAA0001, "somebody-else/9")}}
	l0.Pump(2)
	if !l0.Stats().Up {
		t.Fatal("a link with no ExpectPeer refused an ACK: the check is running " +
			"when nobody asked for it, and every guest written before it existed " +
			"would stop connecting")
	}

	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{
		Port: ourPeer, Name: "app/1", ExpectPeer: "app/1",
	}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l.Pump(1)
	h, _, err := wire.Decode(tr.last)
	if err != nil || h.Type != wire.TypeHello {
		t.Fatalf("the first frame is not a HELLO: %v %v", h.Type, err)
	}

	tr.inbox = []datagram{{src: ourPeer,
		dg: mustAckNamed(t, h.Corr, 0xBBBB0001, "somebody-else/9")}}
	l.Pump(2)
	if l.Stats().Up {
		t.Fatal("the link adopted a token from an application it was not built " +
			"against; every layer below the name agreed, which is exactly why " +
			"the name has to be checked")
	}
	if got := l.Stats().Epoch; got != 0 {
		t.Errorf("Epoch %#x after a refused ACK, want 0", got)
	}
	if got := l.Stats().NameRejects; got != 1 {
		t.Errorf("NameRejects %d, want 1 -- the refusal has to be countable or a "+
			"mismatched companion is indistinguishable from a dead one", got)
	}
	if got := l.Stats().Drops; got != 1 {
		t.Errorf("Drops %d, want 1: a refused frame is still a refused frame", got)
	}

	// THE RETRY CONTINUATION, and it is the half a wrong implementation breaks.
	// The rejected ACK must not CONSUME the outstanding HELLO: a companion that
	// restarts with the right identity while that HELLO is still in flight
	// answers the SAME corr, and clearing helloCorr on the reject would leave
	// this guest deaf to it until the next search.
	tr.inbox = []datagram{{src: ourPeer, dg: mustAckNamed(t, h.Corr, 0xBBBB0002, "app/1")}}
	l.Pump(3)
	if !l.Stats().Up || l.Stats().Epoch != 0xBBBB0002 {
		t.Fatalf("a CORRECT ACK on the same outstanding HELLO was not adopted after "+
			"a rejected one: up=%v epoch=%#x. The reject consumed the HELLO's "+
			"retry state", l.Stats().Up, l.Stats().Epoch)
	}
	if got := l.Stats().NameRejects; got != 1 {
		t.Errorf("NameRejects moved to %d on an ACCEPTED ack", got)
	}
}

// A rejected ACK does not accelerate the search, and this is the arm that keeps
// the refusal from being worse than the thing it refuses.
//
// A mismatched companion answers EVERY hello, so "reject, then re-HELLO at
// once" is one frame per tick in each direction for as long as the
// misconfiguration lasts -- the livelock shape the source-port filter was built
// to end, met from a new direction. The cadence must stay SearchTicks, and the
// guest must still recover when the right companion appears, which is the
// SECOND half of the retry continuation: a fresh HELLO's corr is adopted too.
func TestARejectedHelloAckDoesNotChangeTheSearchCadence(t *testing.T) {
	const ourPeer = 29434
	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{
		Port: ourPeer, Name: "app/1", ExpectPeer: "app/1",
	}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l.Pump(1)
	first, _, err := wire.Decode(tr.last)
	if err != nil {
		t.Fatal(err)
	}
	tr.inbox = []datagram{{src: ourPeer, dg: mustAckNamed(t, first.Corr, 1, "wrong/1")}}
	l.Pump(2)
	if tr.sent != 1 {
		t.Fatalf("%d frames sent by tick 2, want just the first HELLO -- a reject "+
			"that re-HELLOs immediately is a frame per tick against a companion "+
			"that answers every one", tr.sent)
	}

	// Nothing until the search timer, then exactly one more.
	for tick := uint32(3); tick < 1+fkipc.SearchTicks; tick++ {
		l.Pump(tick)
	}
	if tr.sent != 1 {
		t.Fatalf("%d frames sent before SearchTicks elapsed, want 1", tr.sent)
	}
	l.Pump(1 + fkipc.SearchTicks)
	if tr.sent != 2 {
		t.Fatalf("%d frames sent at the search boundary, want 2: the link stopped "+
			"searching after a reject, which is deafness rather than refusal",
			tr.sent)
	}
	second, _, err := wire.Decode(tr.last)
	if err != nil {
		t.Fatal(err)
	}
	if second.Type != wire.TypeHello || second.Corr == first.Corr {
		t.Fatalf("the second frame is %v corr %d (the first was corr %d): a fresh "+
			"search mints a fresh corr", second.Type, second.Corr, first.Corr)
	}

	// And the recovery half: the right companion turns up and is adopted on the
	// NEW corr.
	tr.inbox = []datagram{{src: ourPeer, dg: mustAckNamed(t, second.Corr, 0xC0DE, "app/1")}}
	l.Pump(2 + fkipc.SearchTicks)
	if !l.Stats().Up || l.Stats().Epoch != 0xC0DE {
		t.Fatalf("the correct companion's ACK was not adopted on a fresh HELLO's "+
			"corr: up=%v epoch=%#x", l.Stats().Up, l.Stats().Epoch)
	}
	if got := l.Stats().NameRejects; got != 1 {
		t.Errorf("NameRejects %d, want 1", got)
	}
}

// ONE TOKEN NAMES THE CONTRACT, so a guest that says what it requires has by
// that act said what it is. A guest setting only ExpectPeer would otherwise
// send an empty Name and be refused by the very companion it just described.
func TestAGuestThatStatesOnlyWhatItExpectsAlsoStatesWhatItIs(t *testing.T) {
	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{Port: 29434, ExpectPeer: "app/7"}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	l.Pump(1)
	_, p, err := wire.Decode(tr.last)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := wire.DecodeHello(p)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Name != "app/7" {
		t.Errorf("the HELLO's name is %q, want the expected token %q", hello.Name, "app/7")
	}
}

// Open refuses a configuration it cannot act on rather than producing a link
// that silently never speaks.
func TestOpenRefusesABadConfig(t *testing.T) {
	if _, st := fkipc.Attach(fkipc.Config{}, &nullTransport{}); st != fkipc.StatusBadConfig {
		t.Errorf("a zero port was accepted: %v", st)
	}
	if _, st := fkipc.Attach(fkipc.Config{Port: 1}, nil); st != fkipc.StatusNoTransport {
		t.Errorf("a nil transport was accepted: %v", st)
	}
}

// Before a session exists, everything outbound is a counted no-op. A guest that
// pumps for an hour with nobody listening must not accumulate anything.
func TestWithNoSessionEverythingOutboundIsACountedNoOp(t *testing.T) {
	l, st := fkipc.Attach(fkipc.Config{Port: 29434}, &nullTransport{ver: fkipc.MinEngineVersion})
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	c := l.Chan(1, fkipc.PriBulk)
	l.Pump(1)

	for i, got := range []fkipc.Status{
		c.Send([]byte("x")),
		c.Snapshot([]byte("x")),
		fkipc.WriteBulk(c, "f", []byte("x")),
		fkipc.NotifyFile(c, "f"),
	} {
		if got != fkipc.StatusNoSession {
			t.Errorf("call %d returned %v, want StatusNoSession", i, got)
		}
	}
	if _, got := c.Request([]byte("x"), nil); got != fkipc.StatusNoSession {
		t.Errorf("Request returned %v", got)
	}
	if n := l.Stats().QueueDrops; n != 5 {
		t.Errorf("QueueDrops %d, want 5 -- every refused call is counted, which "+
			"is what makes 'a counted no-op' a diagnosable state rather than "+
			"silence", n)
	}
	if l.Stats().QueueDepth != 0 {
		t.Error("a peerless guest queued something")
	}
}

// A channel named before Open still works, because Go initialises package-level
// variables BEFORE init() functions and `var c = fkipc.Chan(1, ...)` is the
// obvious way to write it.
//
// A Chan that answered that with a dead handle would take every handler
// registered on it with it, silently.
func TestAChannelNamedBeforeOpenKeepsItsHandlers(t *testing.T) {
	c := fkipc.Chan(3, fkipc.PriControl) // as a package-level var would
	got := 0
	c.OnMessage(func(m fkipc.Message) { got++ })

	if st := fkipc.Open(fkipc.Config{Port: 29434}); st != fkipc.StatusNoTransport {
		// Off-target there is no transport to build, which is exactly what
		// StatusNoTransport means. What matters is what Open did NOT do.
		t.Logf("Open off-target: %v", st)
	}
	if fkipc.Chan(3, fkipc.PriControl).ID() != 3 {
		t.Error("the channel did not survive Open")
	}
	if got != 0 {
		t.Error("nothing should have been delivered")
	}
}

// The message ceiling is enforced by the SENDER, because the transport will not
// report it: an oversized send_udp is accepted, raises nothing, and never
// arrives. Nothing downstream of this check can notice.
func TestTheMessageCeilingIsEnforcedBeforeTheWire(t *testing.T) {
	tr := &nullTransport{ver: fkipc.MinEngineVersion}
	l, st := fkipc.Attach(fkipc.Config{Port: 29434, MaxFrame: wire.MaxFrameCeiling}, tr)
	if st != fkipc.StatusOK {
		t.Fatal(st)
	}
	// Nothing sends without a session, so this asserts on the ordering of the
	// two refusals: too-large must be indistinguishable from no-session only in
	// the direction that matters, which is that neither reaches the transport.
	c := l.Chan(1, fkipc.PriBulk)
	huge := bytes.Repeat([]byte("x"), wire.MaxFrameCeiling*wire.MaxFragments*2)
	if got := c.Send(huge); got == fkipc.StatusOK {
		t.Error("a message far over the ceiling was accepted")
	}
	if tr.sent != 0 {
		t.Errorf("%d frames reached the transport", tr.sent)
	}
}

// WHETHER AN OUTBOUND FRAME ACTUALLY WENT IS INVISIBLE TO GUEST STATE, and this
// is the second half of the multiplayer-join fix.
//
// send_udp works only if the peer running the guest was started with
// --enable-lua-udp. In this project's own topology a headless server has it and
// the graphical client joining that server does NOT -- and both peers run the
// same guest, in lockstep, over one CRC'd copy of guest memory. So a link that
// counted TxFrames on success and QueueDrops on failure wrote a different word
// on each peer, every frame, and the client desynced on the first tick it
// simulated. Measured on 2.1.14 with no companion running at all, which is what
// says it is the SEND and not anything inbound; the same client joining a
// server running a non-IPC guest stayed in sync indefinitely.
//
// The two links here differ in nothing but whether their transport works. Every
// number either of them can ever show a peer has to match.
func TestAFailedSendIsInvisibleToGuestState(t *testing.T) {
	drive := func(fail bool) fkipc.LinkStats {
		tr := &nullTransport{ver: fkipc.MinEngineVersion, fail: fail}
		l, st := fkipc.Attach(fkipc.Config{Port: 29434, Name: "g"}, tr)
		if st != fkipc.StatusOK {
			t.Fatal(st)
		}
		c := l.Chan(1, fkipc.PriBulk)
		ctl := l.Chan(2, fkipc.PriControl)

		// Searching, with nobody listening: a HELLO every SearchTicks.
		for tick := uint32(1); tick <= 3; tick++ {
			l.Pump(tick)
		}
		// ...then a session, which arrives INBOUND and therefore identically on
		// every peer whatever the send did. The corr is read from the frame the
		// working transport captured; the failing one never captured a frame,
		// which is exactly the asymmetry under test, so both are ACKed with the
		// corr a link at this point must have minted.
		l.Pump(4)
		tr.inbox = []datagram{{src: 29434, dg: mustAck(t, 1, 0xC0FFEE01)}}
		l.Pump(5)
		if !l.Stats().Up {
			t.Fatalf("fail=%v: the session never came up, so this compares nothing",
				fail)
		}
		// Everything outbound, including the one that used to return early on a
		// failed write and skip the notify -- which would have desynced the
		// channel's seq as well as the counters.
		c.Send([]byte("telemetry"))
		ctl.Request([]byte("q"), nil)
		fkipc.WriteBulk(c, "bulk.bin", bytes.Repeat([]byte("x"), 300))
		fkipc.NotifyFile(c, "shot.png")
		for tick := uint32(6); tick <= 200; tick++ {
			l.Pump(tick)
		}
		return l.Stats()
	}

	ok, broken := drive(false), drive(true)
	if ok != broken {
		t.Errorf("a link whose sends FAILED holds different state than one whose "+
			"sends worked, and both are the same guest on two peers of one "+
			"lockstep game:\n  sends worked: %+v\n  sends failed: %+v", ok, broken)
	}
	if ok.TxFrames == 0 {
		t.Error("neither link sent anything, so the comparison is vacuous")
	}
}

// THE OUTBOUND SEAM CARRIES NO RETURN VALUE, AND THAT IS A TEXT PROPERTY OR IT
// IS NOTHING.
//
// TestAFailedSendIsInvisibleToGuestState above proves the state machine as it
// stands does not branch on a send's outcome. It cannot prove the NEXT edit
// will not: the sentence "so these count what this link attempted" is a
// comment, and this repo has watched a comment lose to a plausible-looking
// change more than once (the dead loop-guard seed, the missed page mark, the
// send-status counters that desynced a join in the first place).
//
// So the guard is on the DECLARATION. Transport.Send, Transport.WriteFile and
// Transport.Log return nothing, and the udpTransport methods that implement
// them on the game target return nothing -- which means no future edit ANYWHERE
// in this package can write `if l.tr.Send(f) == StatusOK`, because there is no
// value to compare. The compiler holds the rule, and this test holds the
// compiler to it.
//
// LOG IS IN THE LIST AND IS THE MOST TEMPTING OF THE THREE. It is the one
// sanctioned per-peer sink -- the game log is not CRC'd, which is the whole
// reason it may carry a fact about how this peer was launched -- so a value
// coming BACK out of it would be that same per-peer fact re-entering guest
// state through the door built to keep it out. It was added with the engine
// gate's one refusal line; it is void from the day it existed and it stays
// void.
//
// Whether send_udp or write_file succeeds is a fact about THIS PEER'S COMMAND
// LINE (--enable-lua-udp binds the socket; a joining graphical client has no
// such flag), and under --persist=table guest memory IS storage.fk_mem, which
// Factorio CRCs across every peer. See agents/ipc.md, "The rule the cost model
// implies", and CLAUDE.md's determinism rule.
//
// Status is deliberately NOT swept away with them: it still answers the
// DETERMINISTIC refusals -- StatusQueueFull, StatusTooLarge, StatusNotOpen,
// StatusNoSession, StatusNoTransport -- each of which is a function of guest
// state alone and therefore the same answer on every peer. That is the whole
// classification, and it is why this test names two methods rather than a file.
//
// The two halves below are enforced by different things, which is why both are
// here. Putting a result back on the INTERFACE does not reach this assertion at
// all -- it fails to compile, because every test double in this package and in
// sdk/go implements it -- and that is the stronger guard. The wasm arm is the
// weak one: transport_guest.go is behind //go:build tinygo.wasm, so `go test`
// never type-checks it against the interface, and a result put back there
// compiles clean and is caught by nothing else. Confirmed by mutation: adding
// `Status` to udpTransport.Send fails this test and nothing else in the repo.
func TestTheOutboundTransportSeamHasNoReturnValue(t *testing.T) {
	fset := token.NewFileSet()

	// The seam itself. A method here with a result type is a value the link is
	// free to read, whatever the implementations do.
	iface := map[string]*ast.FuncType{}
	f, err := parser.ParseFile(fset, "transport.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing transport.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Transport" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return false
		}
		for _, m := range it.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok || len(m.Names) != 1 {
				continue
			}
			iface[m.Names[0].Name] = ft
		}
		return false
	})
	if len(iface) == 0 {
		t.Fatal("the Transport interface was not found in transport.go -- this " +
			"test is looking at the wrong thing, which is worse than it failing")
	}
	for _, name := range []string{"Send", "WriteFile", "Log"} {
		ft, ok := iface[name]
		if !ok {
			t.Errorf("Transport has no %s; if it was renamed, rename it here too "+
				"-- the property is about the outbound half, not about a spelling",
				name)
			continue
		}
		if ft.Results != nil {
			t.Errorf("Transport.%s declares a result. AN OUTBOUND CALL'S OUTCOME "+
				"IS PER-PEER, so a value here is a desync one `if` away: it is "+
				"what `if l.tr.Send(f) == StatusOK { TxFrames++ } else "+
				"{ QueueDrops++ }` was, and that shipped and desynced a joining "+
				"client on the first tick it simulated. Keep the deterministic "+
				"refusals in Status and leave this void.", name)
		}
	}

	// ...and the implementation that actually runs in the game. It is behind
	// //go:build tinygo.wasm, so `go test` never type-checks it against the
	// interface above and a stale signature there would be found by nothing
	// short of an in-game run.
	g, err := parser.ParseFile(fset, "transport_guest.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing transport_guest.go: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range g.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil {
			continue
		}
		if fd.Name.Name != "Send" && fd.Name.Name != "WriteFile" &&
			fd.Name.Name != "Log" {
			continue
		}
		seen[fd.Name.Name] = true
		if fd.Type.Results != nil {
			t.Errorf("the wasm transport's %s declares a result. This is the ONE "+
				"implementation that runs inside a lockstep game, so it is the "+
				"one that must not be able to tell the library how a host call "+
				"went.", fd.Name.Name)
		}
	}
	for _, name := range []string{"Send", "WriteFile", "Log"} {
		if !seen[name] {
			t.Errorf("transport_guest.go declares no %s -- the wasm arm is what "+
				"this property is about, so not finding it is a failure and not "+
				"a skip", name)
		}
	}
}
