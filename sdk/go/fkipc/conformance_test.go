package fkipc_test

import (
	"bytes"
	"math"
	"math/rand"
	"strings"
	"testing"

	guestipc "github.com/Techrocket9/fklua/guest/go/fkipc"
	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	sdkipc "github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// The handshake, the token, and THE ONE EPOCH-TEST EXEMPTION.
//
// HELLO_ACK carries an epoch the guest does not yet know, by definition, so it
// is matched on corr against the outstanding HELLO instead. That exemption is
// stated in the spec precisely because two implementations would otherwise
// disagree about it, and the disagreement would present as "the handshake never
// completes" with both sides looking correct in isolation.
func TestTheHandshakeAdoptsThePeersTokenAndOnlyHelloAckSkipsTheEpochTest(t *testing.T) {
	h := newHarness(t, opts{})
	var ups []guestipc.SessionEvent
	h.g.OnSession(func(e guestipc.SessionEvent) { ups = append(ups, e) })

	h.step(1)
	if n := h.count(toSDK, wire.TypeHello); n != 1 {
		t.Fatalf("the first pump sent %d HELLOs, want 1", n)
	}
	h.step(3)

	gs, ss := h.g.Stats(), h.s.Stats()
	if !gs.Up || gs.Epoch == 0 {
		t.Fatalf("guest never adopted a token: %+v", gs)
	}
	if gs.Epoch != ss.Epoch {
		t.Errorf("epoch disagreement: guest %d, sdk %d", gs.Epoch, ss.Epoch)
	}
	if len(ups) != 1 || ups[0] != guestipc.SessionUp {
		t.Errorf("session events: %v", ups)
	}

	// THE OTHER HALF, which is the one a wrong implementation passes: every
	// type EXCEPT HELLO_ACK is dropped when the epoch does not match.
	before := h.g.Stats()
	c := h.g.Chan(1, guestipc.PriControl)
	got := 0
	c.OnMessage(func(m guestipc.Message) { got++ })
	h.b.inject(craft(wire.Header{
		Type: wire.TypeMsg, Channel: 1, Epoch: gs.Epoch ^ 0xFFFF, Seq: 1,
	}, []byte("from a session nobody remembers")))
	h.step(1)
	if got != 0 {
		t.Error("a frame from a foreign epoch was delivered")
	}
	if h.g.Stats().EpochDrops != before.EpochDrops+1 {
		t.Errorf("the foreign-epoch frame was not counted as one: %d -> %d",
			before.EpochDrops, h.g.Stats().EpochDrops)
	}
}

// A SESSION BOUNDARY mid-flight fails pending requests with ErrSessionLost and
// NEVER retries them.
//
// ErrSessionLost is not "the request failed", it is "THE OUTCOME IS UNKNOWN":
// the boundary may predate a response the peer already sent, or predate the
// peer EXECUTING the request and not yet replying. Retrying into a new session
// would re-execute it outside the dedup window, which is exactly the guarantee
// corr-based dedup exists to provide.
//
// The boundary here is a BYE, and it is a BYE rather than a Reload BECAUSE OF
// THE JOIN FIX: every session boundary is now a REPLICATED signal, so the test
// has to arrive through the wire like the real thing does. A BYE reaches the
// guest through recv_udp, which is an InputAction, which every peer sees at the
// same tick.
func TestASessionBoundaryFailsPendingRequestsAndNeverRetriesThem(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()

	served := 0
	h.s.Handle(3, func(r sdkipc.Request) ([]byte, error) {
		served++
		return []byte("late"), nil
	})

	// A request whose answer is on the wire but not yet delivered: the SDK is
	// pumped only after the guest, so a REQ sent this tick is answered next.
	var got []guestipc.Reply
	c := h.g.Chan(3, guestipc.PriControl)
	if _, st := c.Request([]byte("q"), func(r guestipc.Reply) {
		got = append(got, r)
	}); st != guestipc.StatusOK {
		t.Fatal(st)
	}

	h.b.inject(craft(wire.Header{Type: wire.TypeBye, Epoch: h.g.Stats().Epoch}, nil))
	h.step(1)

	if len(got) != 1 {
		t.Fatalf("the BYE did not complete the pending request: %v", got)
	}
	if got[0].Err != guestipc.ErrSessionLost {
		t.Errorf("pending request failed with %v, want ErrSessionLost", got[0].Err)
	}

	// Nothing above the transport gets to retry it. The guest re-HELLOs and the
	// peer mints a new token, so REQs will flow again -- what must not happen is
	// THIS request going out again, under any epoch.
	reqsBefore := h.count(toSDK, wire.TypeReq)
	h.step(200) // well past the whole retry schedule
	if n := h.count(toSDK, wire.TypeReq) - reqsBefore; n != 0 {
		t.Errorf("%d REQ frames went out after the session was lost; a lost "+
			"request is never retried", n)
	}
	if len(got) != 1 {
		t.Errorf("the completion ran %d times", len(got))
	}
	if served != 0 {
		t.Errorf("the peer's handler ran %d times", served)
	}
}

// A LOAD IS NOT A SESSION BOUNDARY, and that is the multiplayer-join fix stated
// as a property rather than as a comment.
//
// fk_mod.lua arms its fk_after_load one-shot from script.on_load, and Factorio
// runs script.on_load on every peer that LOADS the state -- including a client
// joining a game in progress, one tick after it joins, and on no other peer. So
// Reload is a peer-local signal, guest memory is storage.fk_mem under the
// default --persist=table, and Factorio CRCs that. Anything Reload writes is a
// desync; the measured symptom on 2.1.14 was "fkipc session reset" on the
// joining client and "Multiplayer desynchronisation: crc test failed" from the
// tick after it.
//
// So Reload does nothing at all, and the session it was restarting simply
// carries on -- which is also the RIGHT answer for the joiner, whose downloaded
// state describes a session that is genuinely still live on every other peer.
//
// internal/guest's TestAJoiningPeerStaysByteIdenticalToTheServer is the same
// property one layer down, through the verbatim runtime, over real linear
// memory. This one is what the protocol suite can say without a toolchain.
func TestALoadDoesNotEndTheSession(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()

	c := h.g.Chan(3, guestipc.PriControl)
	h.s.Handle(3, func(r sdkipc.Request) ([]byte, error) { return []byte("pong"), nil })

	var got []guestipc.Reply
	if _, st := c.Request([]byte("q"), func(r guestipc.Reply) {
		got = append(got, r)
	}); st != guestipc.StatusOK {
		t.Fatal(st)
	}

	var events []guestipc.SessionEvent
	h.g.OnSession(func(e guestipc.SessionEvent) { events = append(events, e) })

	before := h.g.Stats()
	helloBefore := h.count(toSDK, wire.TypeHello)
	h.g.Reload() // fk_after_load, on the joining client and nowhere else

	after := h.g.Stats()
	if after.Epoch != before.Epoch || !after.Up {
		t.Errorf("a load moved the session: epoch %d -> %d, up %v -> %v",
			before.Epoch, after.Epoch, before.Up, after.Up)
	}
	if after.Boot != before.Boot {
		t.Errorf("a load moved boot %d -> %d; boot is the SESSION generation "+
			"now and a load is not a session boundary", before.Boot, after.Boot)
	}
	if len(events) != 0 {
		t.Errorf("a load raised %v; nothing peer-local may reach the "+
			"application either", events)
	}
	if len(got) != 0 {
		t.Errorf("a load failed a request in flight: %v", got)
	}

	// ...and the request in flight is answered, under the SAME epoch, which is
	// the part that says the session is genuinely still there rather than
	// merely not visibly reset.
	h.step(4)
	if n := h.count(toSDK, wire.TypeHello) - helloBefore; n != 0 {
		t.Errorf("%d HELLOs went out after a load; there is no session to open", n)
	}
	if len(got) != 1 || got[0].Err != nil || string(got[0].Payload) != "pong" {
		t.Fatalf("the request in flight across the load: %v", got)
	}
	if h.g.Stats().Epoch != before.Epoch {
		t.Errorf("the epoch moved after the load")
	}
	if n := h.s.Stats().Sessions; n != 1 {
		t.Errorf("the peer counted %d sessions across a load, want 1", n)
	}
}

// TWO LOADS OF ONE SAVE PRODUCE THE SAME boot AND DIFFERENT TOKENS, and the
// peer resyncs on the HELLO rather than on the epoch value.
//
// This is the theorem the whole epoch design rests on. Everything a guest can
// compute is a deterministic function of its own state, and its own state
// time-travels -- so there is no deterministic function of guest state that
// distinguishes two loads of one save, and any function that did would be a
// desync. The uniqueness has to come from the side with entropy.
//
// boot is now the SESSION generation rather than a load counter, and that makes
// the theorem sharper rather than weaker: it is not merely equal across two
// loads of one save modulo a bump, it is literally the number the save carries.
// A peer that compared it would be carrying state across a boundary the guest
// has already forgotten, in both designs.
func TestTwoLoadsOfOneSaveShareABootAndGetDifferentTokens(t *testing.T) {
	h := newHarness(t, opts{})
	const savedBoot = 7

	var epochs []uint32
	var boots []uint32
	h.s.OnSession(func(ev sdkipc.SessionEvent, ep uint32) {
		if ev == sdkipc.SessionUp {
			epochs = append(epochs, ep)
			boots = append(boots, h.s.Stats().GuestBoot)
		}
	})

	// A save taken with no session live -- the guest is searching, which is what
	// a HELLO on the first pump after a load means. Both loads restore the same
	// bytes, so both say the same thing about themselves.
	for load := 0; load < 2; load++ {
		h.g = h.newGuest()
		h.g.RestoreBoot(savedBoot)
		h.step(4)
		if !h.g.Stats().Up {
			t.Fatalf("load %d never came up", load)
		}
	}

	if len(boots) != 2 || boots[0] != savedBoot || boots[1] != savedBoot {
		t.Errorf("boot across two loads of one save: %v, want [%d %d]",
			boots, savedBoot, savedBoot)
	}
	if len(epochs) != 2 || epochs[0] == epochs[1] {
		t.Errorf("two loads got the same token: %v", epochs)
	}
	if n := h.s.Stats().Sessions; n != 2 {
		t.Errorf("the peer counted %d sessions for two HELLOs; the HELLO is the "+
			"session boundary and boot must not be compared", n)
	}
}

// THE COMPANION KEPT RUNNING ACROSS THE GUEST'S ROLLBACK, which is the one
// recovery path the load-reset used to provide and nothing else did.
//
// A save restored mid-session hands the guest back a link that still knows the
// epoch and whose per-channel seq counters have gone BACKWARDS. Every telemetry
// frame it sends then reads as d <= 0 here and is dropped as stale, forever,
// with heartbeats flowing and both sides believing the session is healthy --
// the wedge. The guest cannot see it, because everything the guest can compute
// travelled with the save; this side has a clock that did not.
//
// The regressed heartbeat is CRAFTED, for the reason the u32-wrap test crafts
// its frames: there is no way to roll a live Link back from outside it, and the
// frame a rolled-back guest emits is exactly this one. Everything after it --
// the BYE, the guest's teardown, the re-HELLO, the new token -- runs through
// both real state machines.
func TestTheCompanionTearsDownAGuestWhoseClockWentBackwards(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()
	h.step(200) // a session with some history behind it

	first := h.g.Stats().Epoch
	bootBefore := h.g.Stats().Boot

	var replies []guestipc.Reply
	c := h.g.Chan(3, guestipc.PriControl)
	if _, st := c.Request([]byte("q"), func(r guestipc.Reply) {
		replies = append(replies, r)
	}); st != guestipc.StatusOK {
		t.Fatal(st)
	}

	var evs []sdkipc.SessionEvent
	h.s.OnSession(func(ev sdkipc.SessionEvent, ep uint32) { evs = append(evs, ev) })

	// The guest, restored from a save taken 5,000 ticks ago, saying so.
	h.b.injectTo(toSDK, craft(wire.Header{Type: wire.TypeHeartbeat, Epoch: first},
		wire.AppendHeartbeat(nil, wire.Heartbeat{Tick: h.tick - 5000})))
	h.step(1)

	if h.s.Stats().Rollbacks != 1 {
		t.Fatalf("the companion did not notice the guest time-travelling: %+v",
			h.s.Stats())
	}
	if n := h.count(toGuest, wire.TypeBye); n != 1 {
		t.Fatalf("%d BYEs went out, want 1 -- the teardown has to be REPLICATED, "+
			"and a BYE is how it reaches every peer at the same tick", n)
	}
	if h.s.Stats().Up {
		t.Error("the companion kept the session after tearing it down")
	}
	if len(evs) == 0 || evs[len(evs)-1] != sdkipc.SessionDown {
		t.Errorf("session events %v", evs)
	}

	// The guest acts on the BYE in the pump that receives it, which is the next
	// one: the harness drives the guest and then the companion, so a frame the
	// companion emitted this tick is read on the following one.
	h.step(1)
	if len(replies) != 1 || replies[0].Err != guestipc.ErrSessionLost {
		t.Errorf("the request in flight: %v, want one ErrSessionLost", replies)
	}

	// ...and the recovery is the ordinary one: the guest re-HELLOs and the peer
	// mints a token it has never used.
	h.step(8)
	gs, ss := h.g.Stats(), h.s.Stats()
	if !gs.Up || gs.Epoch == 0 || gs.Epoch == first {
		t.Fatalf("no fresh session after the rollback: %+v", gs)
	}
	if gs.Epoch != ss.Epoch {
		t.Errorf("epoch disagreement after the rollback: guest %d sdk %d",
			gs.Epoch, ss.Epoch)
	}
	if ss.Sessions != 2 {
		t.Errorf("the peer counted %d sessions, want 2", ss.Sessions)
	}
	if gs.Boot != bootBefore+1 {
		t.Errorf("boot %d -> %d across one session boundary", bootBefore, gs.Boot)
	}
}

// THE NEGATIVE CONTROLS, and they are what makes the detector a discriminator
// rather than a hair trigger. Both arms run the identical setup and differ only
// in the tick the heartbeat carries.
func TestWhatIsNotAGuestRollback(t *testing.T) {
	// The u32 WRAP. At one tick per 16.67 ms a guest's clock wraps after about
	// 2.27 years of game time, and it is a step of +1 rather than a regression
	// of 2^32 -- which is true only because the comparison is RFC-1982 serial,
	// and is exactly the arithmetic the per-channel seq comparison uses.
	t.Run("the u32 wrap", func(t *testing.T) {
		h, hb := craftedClock(t)
		// Walk the high-water mark to just under the wrap in strides below 2^31,
		// because a stride of 2^31 or more IS the regression arm.
		for _, s := range []uint32{0x40000000, 0x80000000, 0xC0000000, math.MaxUint32 - 60} {
			hb(s)
		}
		hb(math.MaxUint32) // still forward
		hb(30)             // OVER the wrap: +31, not -4294967265

		if n := h.s.Stats().Rollbacks; n != 0 {
			t.Errorf("the wrap read as %d rollbacks", n)
		}
		if !h.s.Stats().Up {
			t.Error("the session died at the wrap")
		}
		if got := h.s.Stats().GuestHigh; got != 30 {
			t.Errorf("the high-water mark after the wrap is %d, want 30", got)
		}
	})

	// JITTER INSIDE THE TOLERANCE. Below DefaultRollbackTicks the wedge un-wedges
	// by itself -- the guest's seq climbs back past where it was in as many ticks
	// as it went back -- so waiting is cheaper than a re-handshake, which fails
	// everything in flight with ErrSessionLost.
	t.Run("a regression inside the tolerance", func(t *testing.T) {
		h, hb := craftedClock(t)
		const high = 100000
		hb(high)

		for _, back := range []uint32{1, 30, sdkipc.DefaultRollbackTicks} {
			hb(high - back)
			if n := h.s.Stats().Rollbacks; n != 0 {
				t.Fatalf("a regression of %d ticks tore the session down; the "+
					"tolerance is %d", back, sdkipc.DefaultRollbackTicks)
			}
		}
		// ...and ONE tick past it does. Same harness, same frame, one number
		// different, which is what makes the arm above evidence.
		hb(high - sdkipc.DefaultRollbackTicks - 1)
		if n := h.s.Stats().Rollbacks; n != 1 {
			t.Errorf("one tick past the tolerance gave %d rollbacks, want 1", n)
		}
	})
}

// craftedClock brings a session up and then puts the GUEST'S OWN FRAMES ON THE
// FLOOR, so the only clock the companion hears is the one the test is dictating.
//
// Without that the arms above measure the wrong thing: the guest is at tick ~10
// and heartbeating for real, so a crafted high-water mark somewhere in 2028
// makes the guest's next genuine heartbeat a four-billion-tick regression. That
// is the detector working correctly on a contradiction the harness invented,
// and it would drown the property under test. The companion keeps heartbeating
// TOWARD the guest, so nobody times anybody out.
func craftedClock(t *testing.T) (*harness, func(uint32)) {
	t.Helper()
	h := newHarness(t, opts{})
	h.up()
	ep := h.g.Stats().Epoch
	h.b.fault = func(d dir, f []byte) [][]byte {
		if d == toSDK {
			return nil
		}
		return [][]byte{f}
	}
	return h, func(tick uint32) {
		h.b.injectTo(toSDK, craft(wire.Header{Type: wire.TypeHeartbeat, Epoch: ep},
			wire.AppendHeartbeat(nil, wire.Heartbeat{Tick: tick})))
		h.step(1)
	}
}

// THE GUEST'S OWN HALF: a clock that has gone backwards past the last frame the
// link accepted is a session that belongs to a future which no longer happened.
//
// It catches every rollback LARGER than the time since the last inbound frame,
// which with a peer heartbeating once a second is most of them -- and it is
// legal on a joining client for the reason nothing else here is: it is a
// function of guest state and the REPLICATED tick, so every peer decides it
// identically on the same tick.
func TestTheGuestNoticesItsOwnClockGoingBackwards(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()
	h.step(500)

	var evs []guestipc.SessionEvent
	h.g.OnSession(func(e guestipc.SessionEvent) { evs = append(evs, e) })
	first := h.g.Stats().Epoch

	// The same guest, one tick later by its own reckoning, five hundred ticks
	// earlier by the clock: a save restored.
	h.tick = 20
	h.g.Pump(h.tick)

	if h.g.Stats().Up {
		t.Fatalf("the guest kept a session across its own rollback: %+v",
			h.g.Stats())
	}
	if len(evs) != 1 || evs[0] != guestipc.SessionDown {
		t.Errorf("session events %v, want one down", evs)
	}
	if n := h.count(toSDK, wire.TypeHello); n < 1 {
		t.Error("the guest did not go looking for a peer again")
	}
	h.step(6)
	if gs := h.g.Stats(); !gs.Up || gs.Epoch == first {
		t.Errorf("no fresh session: %+v", gs)
	}
}

// THE HEARTBEAT IS UNCONDITIONAL, and it has to be, because it is the only
// frame carrying the guest's clock and the rollback detector above is the only
// thing that can see a rollback the guest cannot.
//
// It used to be suppressed whenever anything else had gone out in the window,
// on the grounds that any frame is a liveness signal -- true, and it left a
// telemetry-heavy guest never heartbeating at all, so the peer's reading of the
// guest clock froze at the HELLO and so did the rx/drops/gaps counters that are
// the flow-control signal. This guest sends a MSG every single tick, which is
// the shape that used to produce zero heartbeats.
func TestABusyGuestStillHeartbeats(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()
	c := h.g.Chan(1, guestipc.PriBulk)
	h.s.Subscribe(1, func(m sdkipc.Message) {})

	before := h.count(toSDK, wire.TypeHeartbeat)
	for i := 0; i < 4*guestipc.HeartbeatTicks; i++ {
		c.Send([]byte("telemetry"))
		h.step(1)
	}
	if n := h.count(toSDK, wire.TypeHeartbeat) - before; n < 3 {
		t.Errorf("a guest sending every tick emitted %d heartbeats over four "+
			"heartbeat windows, want at least 3", n)
	}
	if got := h.s.Stats().GuestTick; got < h.tick-guestipc.HeartbeatTicks {
		t.Errorf("the peer's reading of the guest clock is %d at tick %d -- "+
			"stale by more than one heartbeat window", got, h.tick)
	}
}

// Dedup: a retried REQ replays the cached RESP and the handler runs ONCE.
func TestARetriedRequestReplaysTheCachedResponseAndRunsTheHandlerOnce(t *testing.T) {
	h := newHarness(t, opts{})

	runs := 0
	c := h.g.Chan(4, guestipc.PriControl)
	c.OnRequest(func(r guestipc.Request) []byte {
		runs++
		return []byte("answer")
	})
	h.up()

	// Every RESP toward the SDK is dropped, so the SDK retries on its own
	// schedule and the guest sees genuine retransmissions.
	dropResp := true
	h.b.fault = func(d dir, f []byte) [][]byte {
		hd, _, err := wire.Decode(f)
		if err == nil && dropResp && d == toSDK && hd.Type == wire.TypeResp {
			return nil
		}
		return [][]byte{f}
	}

	var answers [][]byte
	var errs []error
	h.s.RequestAsync(4, []byte("ask"), func(b []byte, err error) {
		answers = append(answers, b)
		errs = append(errs, err)
	})
	h.step(120) // two retries at 300 ms and 600 ms, on a 16.667 ms tick
	if runs != 1 {
		t.Errorf("the handler ran %d times across the retries; a dedup hit must "+
			"replay, not re-invoke", runs)
	}
	if h.g.Stats().DupHits == 0 {
		t.Error("no dedup hit was counted")
	}

	// Let one through and the cached answer -- not a fresh one -- arrives.
	dropResp = false
	h.step(200)
	if len(answers) != 1 || string(answers[0]) != "answer" || errs[0] != nil {
		t.Fatalf("answers %q errs %v", answers, errs)
	}
	if runs != 1 {
		t.Errorf("the handler ran %d times in total", runs)
	}
}

// A response above MaxDedupPayload answers DUPLICATE on retry instead of
// re-executing.
//
// The application learns that the operation EXECUTED and the result is gone,
// which is strictly better than the two alternatives: silently re-executing it,
// or growing the save without bound to hold every reply.
func TestAnUncacheableResponseAnswersDuplicateOnRetry(t *testing.T) {
	h := newHarness(t, opts{sdkMaxFrame: wire.MaxFrameCeiling})

	runs := 0
	big := bytes.Repeat([]byte("x"), guestipc.MaxDedupPayload+1)
	c := h.g.Chan(5, guestipc.PriControl)
	c.OnRequest(func(r guestipc.Request) []byte {
		runs++
		return big
	})
	h.up()

	dropResp := true
	h.b.fault = func(d dir, f []byte) [][]byte {
		hd, _, err := wire.Decode(f)
		if err == nil && dropResp && d == toSDK && hd.Type == wire.TypeResp {
			return nil
		}
		return [][]byte{f}
	}

	var gotErr error
	var gotOK []byte
	h.s.RequestAsync(5, []byte("ask"), func(b []byte, err error) {
		gotOK, gotErr = b, err
	})
	h.step(60)
	dropResp = false
	h.step(200)

	if runs != 1 {
		t.Errorf("the handler ran %d times; an uncached response must not "+
			"re-execute on retry", runs)
	}
	pe, ok := gotErr.(*sdkipc.PeerError)
	if !ok || !pe.Duplicate() {
		t.Fatalf("got (%q, %v), want a DUPLICATE peer error", gotOK, gotErr)
	}
}

// Serial arithmetic at the u32 wrap, BOTH ARMS.
//
// This is the one comparison two implementations silently disagree about, and a
// disagreement does not fail -- it delivers or drops the wrong frames forever.
// The frames are crafted rather than sent, because getting a real sender to a
// seq of 2^32-2 would take two thousand years at one frame per tick.
func TestSerialArithmeticAtTheWrapInBothArms(t *testing.T) {
	h := newHarness(t, opts{})
	var seen []uint32
	var gaps []uint32
	c := h.g.Chan(9, guestipc.PriControl)
	c.OnMessage(func(m guestipc.Message) { seen = append(seen, m.Seq) })
	c.OnGap(func(missed uint32) { gaps = append(gaps, missed) })
	h.up()
	ep := h.g.Stats().Epoch

	send := func(seq uint32) {
		h.b.inject(craft(wire.Header{
			Type: wire.TypeMsg, Channel: 9, Epoch: ep, Seq: seq,
		}, []byte{byte(seq)}))
		h.step(1)
	}

	// Walk rxLast up to the boundary in strides under 2^31, because that is the
	// only way there is: a delta of 2^31 or more IS the drop arm, so a receiver
	// cannot be jumped to the far side of the space in one frame. Three hops.
	for _, s := range []uint32{0x40000000, 0x80000000, 0xC0000000, math.MaxUint32 - 2} {
		send(s)
	}
	seen, gaps = nil, nil
	drops := h.g.Stats().StaleDrops

	// Right up to the wrap, over it, and then back into it.
	send(math.MaxUint32 - 1) // d = 1
	send(math.MaxUint32)     // d = 1
	send(0)                  // d = 1 ACROSS THE WRAP
	send(2)                  // d = 2, a gap of one
	send(math.MaxUint32)     // d = -3: old
	send(0)                  // d = -2: old

	want := []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 2}
	if len(seen) != len(want) {
		t.Fatalf("delivered %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("delivered %v, want %v", seen, want)
		}
	}
	// The wrap itself must not read as a gap; the deliberate skip must.
	if len(gaps) != 1 || gaps[0] != 1 {
		t.Errorf("gaps %v, want exactly one gap of 1", gaps)
	}
	if n := h.g.Stats().StaleDrops - drops; n != 2 {
		t.Errorf("stale drops %d, want 2", n)
	}
}

// A gap raises OnGap, sends a RESYNC, and a SNAPSHOT clears it.
//
// A gap is answered with a SNAPSHOT and never a replay, and the reason is not
// economy: the producer usually cannot replay, because the state it described
// no longer exists. A resend of "entity 4471 at 30% health" three seconds later
// is a lie, and a lie that arrives is worse than a gap that is noticed.
func TestAGapResyncsAndASnapshotClearsIt(t *testing.T) {
	h := newHarness(t, opts{})
	var gaps []uint32
	var snaps int
	c := h.g.Chan(2, guestipc.PriControl)
	c.OnGap(func(missed uint32) { gaps = append(gaps, missed) })
	c.OnMessage(func(m guestipc.Message) {
		if m.Snapshot {
			snaps++
		}
	})
	h.up()

	resyncs := 0
	h.s.Subscribe(2, func(m sdkipc.Message) {
		if m.Snapshot && m.Payload == nil {
			resyncs++
			h.s.Snapshot(2, []byte("whole world"))
		}
	})

	// Three frames, the middle two lost.
	h.s.Send(2, []byte("a"))
	h.step(1)
	h.b.fault = func(d dir, f []byte) [][]byte { return nil }
	h.s.Send(2, []byte("b"))
	h.s.Send(2, []byte("c"))
	h.b.fault = nil
	h.s.Send(2, []byte("d"))
	h.step(6)

	if len(gaps) != 1 || gaps[0] != 2 {
		t.Fatalf("gaps %v, want one gap of 2", gaps)
	}
	if resyncs != 1 {
		t.Fatalf("the peer saw %d resyncs, want 1", resyncs)
	}
	if snaps != 1 {
		t.Fatalf("the guest received %d snapshots, want 1", snaps)
	}

	// The gap is cleared: an ordinary frame after the snapshot raises nothing
	// new, and a SECOND gap raises exactly one more resync rather than being
	// suppressed forever by the first.
	h.s.Send(2, []byte("e"))
	h.step(3)
	if len(gaps) != 1 {
		t.Errorf("gaps after the snapshot: %v", gaps)
	}
	h.b.fault = func(d dir, f []byte) [][]byte { return nil }
	h.s.Send(2, []byte("f"))
	h.b.fault = nil
	h.s.Send(2, []byte("g"))
	h.step(6)
	if len(gaps) != 2 {
		t.Errorf("a second gap after a cleared one: gaps %v", gaps)
	}
}

// A fragmented MSG that loses a fragment is LOST ENTIRELY and shows up as a
// gap, not as a short message.
//
// That is correct and it is also the guidance: a sender that needs a large
// message DELIVERED uses REQ/RESP, not MSG.
func TestALostFragmentIsAGapAndNotAShortMessage(t *testing.T) {
	h := newHarness(t, opts{guestMaxFrame: 128})
	var got [][]byte
	var gaps []uint32
	c := h.g.Chan(6, guestipc.PriBulk)
	c.OnMessage(func(m guestipc.Message) {
		got = append(got, append([]byte(nil), m.Payload...))
	})
	c.OnGap(func(n uint32) { gaps = append(gaps, n) })
	h.up()

	body := bytes.Repeat([]byte("0123456789"), 40) // 400 B over a 104 B fragment
	dropped := false
	h.b.fault = func(d dir, f []byte) [][]byte {
		hd, _, err := wire.Decode(f)
		if err == nil && d == toGuest && hd.NFrag > 1 && hd.Frag == 1 && !dropped {
			dropped = true
			return nil
		}
		return [][]byte{f}
	}
	if err := h.s.Send(6, body); err != nil {
		t.Fatal(err)
	}
	h.step(4)

	if len(got) != 0 {
		t.Fatalf("a message was delivered with a fragment missing: %d bytes",
			len(got[0]))
	}
	if len(gaps) == 0 {
		t.Error("a lost fragment must show up as a gap")
	}

	// The whole message, intact, when nothing is lost -- so the failure above
	// is about the loss and not about fragmentation being broken.
	h.b.fault = nil
	h.s.Snapshot(6, body)
	h.step(4)
	if len(got) != 1 || !bytes.Equal(got[0], body) {
		t.Fatalf("the intact message did not reassemble: %d messages", len(got))
	}
}

// The three ways a reassembly is abandoned, each with its own consequence.
func TestReassemblyTimesOutDisagreesAndInterleaves(t *testing.T) {
	h := newHarness(t, opts{guestMaxFrame: 128})
	var got [][]byte
	c := h.g.Chan(7, guestipc.PriBulk)
	c.OnMessage(func(m guestipc.Message) {
		got = append(got, append([]byte(nil), m.Payload...))
	})
	h.up()
	ep := h.g.Stats().Epoch
	seq := uint32(0)
	frag := func(corr uint32, i, n uint8, body string) {
		seq++
		h.b.inject(craft(wire.Header{
			Type: wire.TypeMsg, Channel: 7, Epoch: ep, Seq: seq, Corr: corr,
			Frag: i, NFrag: n,
		}, []byte(body)))
	}

	// (1) TIMEOUT. Half a message, then silence past ReassemblyTicks; the rest
	// arrives and must not complete it.
	frag(100, 0, 2, "AAA")
	h.step(1)
	h.step(guestipc.ReassemblyTicks + 2)
	frag(100, 1, 2, "BBB")
	h.step(1)
	if len(got) != 0 {
		t.Fatalf("a reassembly completed across its own timeout: %q", got)
	}

	// (2) nfrag DISAGREEMENT. Nothing here can tell which of the two messages
	// is real, so neither is.
	frag(200, 0, 2, "CCC")
	h.step(1)
	frag(200, 1, 3, "DDD")
	h.step(1)
	if len(got) != 0 {
		t.Fatalf("a reassembly survived an nfrag disagreement: %q", got)
	}

	// (3) INTERLEAVE. At most one reassembly is open per channel, so a new corr
	// kills the old -- which is what bounds the buffer and what imposes the
	// rule that a peer must not interleave two fragmented messages on one
	// channel.
	frag(300, 0, 2, "EEE")
	h.step(1)
	frag(400, 0, 2, "FFF")
	frag(400, 1, 2, "GGG")
	h.step(1)
	if len(got) != 1 || string(got[0]) != "FFFGGG" {
		t.Fatalf("interleave: got %q, want only the second message", got)
	}
	frag(300, 1, 2, "HHH")
	h.step(1)
	if len(got) != 1 {
		t.Fatalf("the abandoned message came back: %q", got)
	}
}

// Retry budget exhaustion is ErrTimeout, and it takes the whole schedule to get
// there.
func TestRetryExhaustionTimesOut(t *testing.T) {
	h := newHarness(t, opts{})
	h.s.Handle(8, func(r sdkipc.Request) ([]byte, error) { return []byte("ok"), nil })
	h.up()

	// Only the answers are lost.
	h.b.fault = func(d dir, f []byte) [][]byte {
		hd, _, err := wire.Decode(f)
		if err == nil && d == toGuest && hd.Type == wire.TypeResp {
			return nil
		}
		return [][]byte{f}
	}

	var reply guestipc.Reply
	done := false
	c := h.g.Chan(8, guestipc.PriControl)
	c.Request([]byte("q"), func(r guestipc.Reply) { reply, done = r, true })

	// THE PEER MUST BE SEEN TO BE ALIVE, or this measures the wrong thing.
	// The guest's whole retry schedule is 15+30+60+60 ticks and it declares the
	// timeout one interval after the last retry, at 225 -- which is PAST
	// LivenessTicks (180). So a request whose answers are all lost AND whose
	// peer says nothing else dies with ErrSessionLost, correctly, and only a
	// peer that is demonstrably alive isolates the retry budget. The SDK's own
	// heartbeat is suppressed here because it believes it is sending RESPs, so
	// the harness supplies the liveness the wire is swallowing.
	ep := h.g.Stats().Epoch
	for i := 0; i < 240 && !done; i++ {
		if i%30 == 0 {
			h.b.inject(craft(wire.Header{Type: wire.TypeHeartbeat, Epoch: ep},
				wire.AppendHeartbeat(nil, wire.Heartbeat{Tick: h.tick})))
		}
		h.step(1)
	}
	if !done {
		t.Fatal("the request never completed")
	}
	if reply.Err != guestipc.ErrTimeout {
		t.Fatalf("completed with %v, want ErrTimeout", reply.Err)
	}
	if st := h.g.Stats(); st.Retries != guestipc.MaxRetries || st.Timeouts != 1 {
		t.Errorf("retries %d timeouts %d, want %d and 1",
			st.Retries, st.Timeouts, guestipc.MaxRetries)
	}
	if !h.g.Stats().Up {
		t.Error("the session died; this test is about the retry budget")
	}
}

// The quiesce: with no peer, Send is a COUNTED NO-OP rather than an error to
// handle at every call site.
//
// The mod's own behaviour must be defined with no peer, and the library makes
// that the easy path. It also stops sending everything except a HELLO every
// SearchTicks, because a guest that registers nothing can never be reached
// again.
func TestWithNoPeerSendIsACountedNoOpAndHelloKeepsSearching(t *testing.T) {
	h := newHarness(t, opts{})
	c := h.g.Chan(1, guestipc.PriBulk)
	h.up()

	var events []guestipc.SessionEvent
	h.g.OnSession(func(e guestipc.SessionEvent) { events = append(events, e) })

	// The peer vanishes: nothing reaches the guest any more.
	h.b.fault = func(d dir, f []byte) [][]byte {
		if d == toGuest {
			return nil
		}
		return [][]byte{f}
	}
	h.step(guestipc.LivenessTicks + 2)

	if h.g.Stats().Up {
		t.Fatal("the guest still thinks the peer is there")
	}
	if len(events) != 1 || events[0] != guestipc.SessionDown {
		t.Errorf("session events %v", events)
	}

	before := h.g.Stats()
	if st := c.Send([]byte("into the void")); st != guestipc.StatusNoSession {
		t.Errorf("Send returned %v, want StatusNoSession", st)
	}
	if h.g.Stats().QueueDrops != before.QueueDrops+1 {
		t.Error("the dropped send was not counted")
	}
	if _, st := c.Request([]byte("q"), nil); st != guestipc.StatusNoSession {
		t.Errorf("Request returned %v, want StatusNoSession", st)
	}

	// ...and it keeps looking, at SearchTicks, and sends nothing else.
	mark := len(h.b.log[toSDK])
	h.step(3 * guestipc.SearchTicks)
	for _, f := range h.b.log[toSDK][mark:] {
		hd, _, err := wire.Decode(f)
		if err != nil || hd.Type != wire.TypeHello {
			t.Fatalf("a quiesced guest sent %v", hd.Type)
		}
	}
	if n := len(h.b.log[toSDK]) - mark; n < 2 || n > 4 {
		t.Errorf("%d HELLOs over three SearchTicks windows", n)
	}
}

// Below the engine floor the link is INERT: not one datagram of any kind, every
// API call refused deterministically, and one line in the log saying why.
//
// PUMPING IS FATAL WHERE IT IS NOT USELESS: on 2.0.77 a headless server calling
// recv_udp with a packet queued aborts in C++ at TickClosure.cpp:91, which no
// pcall can catch. This ran SEND-ONLY down there until 2026-08-07, on the
// reasoning that outbound is free -- true of the datagrams and false of the
// protocol, because a session is established by an ACK and an ACK arrives
// INBOUND. So a send-only link HELLOed once a second forever, never came up,
// refused every Send for want of a session, and told its author nothing beyond
// a counter. Hard-disable replaces the trickle with silence and one sentence.
//
// The `sent` side of this is the assertion that matters: ZERO frames, not
// "only HELLOs".
func TestBelowTheEngineFloorTheLinkIsInert(t *testing.T) {
	old := guestipc.Version{Major: 2, Minor: 0, Patch: 77}
	h := newHarness(t, opts{baseVersion: old})
	c := h.g.Chan(1, guestipc.PriBulk)
	h.step(20)

	st := h.g.Stats()
	if st.Enabled {
		t.Fatal("the gate opened below the floor")
	}
	if n := len(h.b.log[toSDK]); n != 0 {
		t.Errorf("a disabled link put %d frame(s) on the wire; it must put none",
			n)
	}
	if st.TxFrames != 0 || st.RxFrames != 0 {
		t.Errorf("tx %d rx %d, want 0 and 0", st.TxFrames, st.RxFrames)
	}
	if st.Up {
		t.Error("a session cannot come up on a link that never says HELLO")
	}
	if st.BaseVersion != old {
		t.Errorf("BaseVersion %v", st.BaseVersion)
	}

	// EVERY OUTBOUND CALL ANSWERS THE SAME DETERMINISTIC REFUSAL, which is the
	// half a counter cannot give an author: StatusDisabled at the call site says
	// "this engine", where StatusNoSession would have said "your companion is
	// down" about a companion that is running fine.
	before := h.g.Stats().Refusals
	if got := c.Send([]byte("into the void")); got != guestipc.StatusDisabled {
		t.Errorf("Send returned %v, want StatusDisabled", got)
	}
	if got := c.Snapshot([]byte("state")); got != guestipc.StatusDisabled {
		t.Errorf("Snapshot returned %v, want StatusDisabled", got)
	}
	if _, got := c.Request([]byte("q"), nil); got != guestipc.StatusDisabled {
		t.Errorf("Request returned %v, want StatusDisabled", got)
	}
	if got := guestipc.WriteBulk(c, "bulk.bin", []byte("x")); got != guestipc.StatusDisabled {
		t.Errorf("WriteBulk returned %v, want StatusDisabled", got)
	}
	if got := guestipc.NotifyFile(c, "shot.png"); got != guestipc.StatusDisabled {
		t.Errorf("NotifyFile returned %v, want StatusDisabled", got)
	}
	if n := h.g.Stats().Refusals - before; n != 5 {
		t.Errorf("%d refusals counted over five refused calls", n)
	}
	// WriteBulk refuses BEFORE the write, so nothing lands on disk either: the
	// notify that announces the file can never be sent, and a file the peer will
	// never hear about is worse than no file.
	if len(h.ge.files) != 0 {
		t.Errorf("a disabled link wrote %v", h.ge.files)
	}
	if len(h.b.log[toSDK]) != 0 {
		t.Error("a refused call still reached the wire")
	}

	// AND IT SAYS SO, ONCE. The game log is not CRC'd and is per-peer by nature,
	// which is what makes it the only sanctioned sink for this -- and why the
	// line is the one thing here a mod author actually reads.
	if len(h.ge.logs) != 1 {
		t.Fatalf("the gate logged %d line(s), want exactly one: %v",
			len(h.ge.logs), h.ge.logs)
	}
	want := "fkipc: disabled -- requires Factorio >= " +
		guestipc.MinEngineVersion.String() + "; this engine is 2.0.77"
	if h.ge.logs[0] != want {
		t.Errorf("logged %q,\n   want %q", h.ge.logs[0], want)
	}

	// And at the floor it opens. Same harness shape, one constant different,
	// so the assertion above is about the gate and not about the wiring.
	h2 := newHarness(t, opts{baseVersion: guestipc.MinEngineVersion})
	h2.step(4)
	if !h2.g.Stats().Up || !h2.g.Stats().Enabled {
		t.Errorf("at the floor: %+v", h2.g.Stats())
	}
	if h2.count(toSDK, wire.TypeHello) == 0 {
		t.Error("at the floor the link must still say HELLO")
	}
	if len(h2.ge.logs) != 0 {
		t.Errorf("an enabled link logged %v; the line is for the refusal only",
			h2.ge.logs)
	}
}

// A SAVE MOVED ONTO A NEWER ENGINE COMES UP BY ITSELF, and this is the arm the
// engine gate's re-read exists for.
//
// An engine cannot change under a running game, so within one session the
// gate's answer is fixed and re-reading it would be waste. A SAVE is the other
// case and is an ordinary thing for a player to do: a map made on 2.0.77 and
// then loaded on 2.1.14 carries guest state that says "disabled" into a game
// where the library works. Under --persist=table that state IS storage.fk_mem
// and comes back from the save, so it is not enough for Open to have asked --
// serviceGate re-asks on the REPLICATED tick, at SearchTicks, and only while
// the gate is shut.
//
// Modelled by changing what the transport reports under a LIVE link rather than
// by rebuilding one, which is the honest shape: what a load does is carry guest
// state across into a different engine, and this is that link meeting that
// engine. Rebuilding would test Open, which is the case already covered.
//
// The re-read is legal for the same reason every other decision in here is:
// the condition is guest state plus the tick, so every peer re-reads on the
// same tick and writes the same answer. An fk_after_load one-shot would not be.
func TestASaveMovedOntoANewerEngineComesUpByItself(t *testing.T) {
	h := newHarness(t, opts{baseVersion: guestipc.Version{Major: 2, Minor: 0, Patch: 77}})
	h.step(20)
	if h.g.Stats().Enabled || len(h.b.log[toSDK]) != 0 {
		t.Fatalf("the link was not inert to begin with: %+v", h.g.Stats())
	}

	// The load. Same link, same guest state, new engine underneath it.
	h.ge.ver = guestipc.MinEngineVersion

	// Up to a SearchTicks boundary, which is the whole worst case: the gate is
	// polled once a second of game time and not once a tick, because below the
	// floor a host call per tick would be the one cost this mode is meant not
	// to have.
	h.step(2 * int(guestipc.SearchTicks))
	st := h.g.Stats()
	if !st.Enabled {
		t.Fatalf("the gate stayed shut over two SearchTicks windows: %+v", st)
	}
	if !st.Up {
		t.Errorf("the gate opened and the session did not follow: %+v", st)
	}
	if h.count(toSDK, wire.TypeHello) == 0 {
		t.Error("no HELLO went out after the gate opened")
	}
	// Both lines, in order: the refusal from the old engine and its withdrawal.
	// The second matters more than it looks -- a reader who saw the first one
	// deserves to see it taken back rather than being left to infer it from
	// traffic.
	if len(h.ge.logs) != 2 ||
		!strings.Contains(h.ge.logs[0], "disabled") ||
		!strings.Contains(h.ge.logs[1], "enabled -- this engine is 2.1.14") {
		t.Errorf("the gate logged %q; want a disabled line then an enabled one",
			h.ge.logs)
	}

	// ...and it is never re-read once open. The gate is MONOTONE within a
	// session -- Factorio refuses a save written by a newer build, so a restored
	// "the link may run" can only have come from an engine at or below this one
	// -- so a host call per second here would buy nothing at all.
	h.ge.ver = guestipc.Version{Major: 2, Minor: 0, Patch: 77}
	h.step(2 * int(guestipc.SearchTicks))
	if !h.g.Stats().Enabled {
		t.Error("the gate shut again; it must never re-read once open")
	}
	if len(h.ge.logs) != 2 {
		t.Errorf("the gate logged again: %q", h.ge.logs)
	}
}

// A guest whose peer speaks a protocol version it does not is deaf but not
// broken, and the frames are counted as bad rather than as an epoch mismatch.
func TestAFutureProtocolVersionIsDroppedAndCounted(t *testing.T) {
	h := newHarness(t, opts{})
	h.up()
	ep := h.g.Stats().Epoch
	got := 0
	h.g.Chan(1, guestipc.PriControl).OnMessage(func(m guestipc.Message) { got++ })

	f := craft(wire.Header{Type: wire.TypeMsg, Channel: 1, Epoch: ep, Seq: 1},
		[]byte("hi"))
	f[2] = wire.Version + 1
	h.b.inject(f)
	// ...and a truncated frame, which is what the length field is for.
	good := craft(wire.Header{Type: wire.TypeMsg, Channel: 1, Epoch: ep, Seq: 1},
		[]byte("hello"))
	h.b.inject(good[:len(good)-2])
	h.step(1)

	if got != 0 {
		t.Error("an undecodable frame was delivered")
	}
	if h.g.Stats().BadFrames != 2 {
		t.Errorf("BadFrames %d, want 2", h.g.Stats().BadFrames)
	}
}

// A frame larger than the negotiated ceiling is refused by the SENDER, because
// the transport will not report it: an oversized send_udp is accepted, raises
// nothing, and never arrives.
func TestAMessageOverTheCeilingIsRefusedBySender(t *testing.T) {
	h := newHarness(t, opts{guestMaxFrame: 128, sdkMaxFrame: 128})
	h.up()
	c := h.g.Chan(1, guestipc.PriBulk)

	room := 128 - wire.HeaderBytes
	if st := c.Send(bytes.Repeat([]byte("x"), room*wire.MaxFragments)); st != guestipc.StatusOK {
		t.Errorf("a message at exactly the ceiling was refused: %v", st)
	}
	if st := c.Send(bytes.Repeat([]byte("x"), room*wire.MaxFragments+1)); st != guestipc.StatusTooLarge {
		t.Errorf("one byte over the ceiling returned %v, want StatusTooLarge", st)
	}
}

// WriteBulk writes the file and notifies with a digest the peer can verify
// exactly; NotifyFile announces a file the guest never held and carries none.
func TestWriteBulkDigestsAndNotifyFileDoesNot(t *testing.T) {
	h := newHarness(t, opts{})
	c := h.g.Chan(1, guestipc.PriBulk)
	h.up()

	body := bytes.Repeat([]byte("payload"), 500)
	if st := guestipc.WriteBulk(c, "fkipc/dump.bin", body); st != guestipc.StatusOK {
		t.Fatal(st)
	}
	if got := h.ge.files["fkipc/dump.bin"]; !bytes.Equal(got, body) {
		t.Fatalf("the file is %d bytes, want %d", len(got), len(body))
	}
	if st := guestipc.NotifyFile(c, "shot.png"); st != guestipc.StatusOK {
		t.Fatal(st)
	}
	h.step(2)

	var withDigest, without int
	for i, f := range h.b.log[toSDK] {
		hd, p, err := wire.Decode(f)
		if err != nil || hd.Type != wire.TypeFileNotify {
			continue
		}
		fn, err := wire.DecodeFileNotify(p)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if hd.Flags.Has(wire.FlagHasDigest) {
			withDigest++
			if fn.Bytes != uint32(len(body)) || fn.FNV1a32 != wire.FNV1a32(body) {
				t.Errorf("digest %d/%#x, want %d/%#x", fn.Bytes, fn.FNV1a32,
					len(body), wire.FNV1a32(body))
			}
		} else {
			without++
			if fn.Name != "shot.png" {
				t.Errorf("name %q", fn.Name)
			}
		}
	}
	if withDigest != 1 || without != 1 {
		t.Errorf("%d digested and %d bare notifies, want 1 and 1", withDigest, without)
	}
}

// THE SOAK: a seeded fault storm, then quiet, and the session must converge.
//
// It asserts two things and no more, because that is what a randomised test can
// honestly assert. First, that nothing panics -- an index off the end of a
// reassembly table, a nil handler, a ring that wrapped wrong. Second, that once
// the faults stop the two sides AGREE again: same epoch, session up, and a
// fresh request answered. A protocol that only converges when nothing goes
// wrong has not been tested.
func TestASeededFaultSoakConverges(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233} {
		rng := rand.New(rand.NewSource(seed))
		h := newHarness(t, opts{guestMaxFrame: 160, sdkMaxFrame: 160})

		c := h.g.Chan(11, guestipc.PriBulk)
		ctrl := h.g.Chan(12, guestipc.PriControl)
		ctrl.OnRequest(func(r guestipc.Request) []byte { return []byte("pong") })
		c.OnMessage(func(m guestipc.Message) {})
		c.OnResync(func() { c.Snapshot([]byte("snap")) })
		h.s.Subscribe(11, func(m sdkipc.Message) {})
		h.s.Handle(11, func(r sdkipc.Request) ([]byte, error) { return []byte("ok"), nil })

		// Drop, duplicate, truncate and reorder -- the four things a datagram
		// transport does. NOT bit corruption: the header carries no checksum by
		// design (the acceptance test is magic, version, type, length and
		// epoch), the wire is loopback UDP which has one, and a soak that
		// injected it would be scoring the protocol against a threat it
		// explicitly does not defend against. What it costs is written down in
		// the SNAPSHOT exemption in channelFrame, which this soak is what found.
		var held [2][]byte
		h.b.fault = func(d dir, f []byte) [][]byte {
			out := [][]byte{f}
			switch n := rng.Intn(100); {
			case n < 12: // drop
				out = nil
			case n < 18: // duplicate
				out = [][]byte{f, append([]byte(nil), f...)}
			case n < 22: // truncate -- the length field must catch it
				if len(f) > wire.HeaderBytes {
					out = [][]byte{f[:wire.HeaderBytes+rng.Intn(len(f)-wire.HeaderBytes)]}
				}
			case n < 28: // reorder: hold this one back behind the next
				if held[d] == nil {
					held[d] = f
					return nil
				}
			}
			if held[d] != nil {
				out = append(out, held[d])
				held[d] = nil
			}
			return out
		}

		for i := 0; i < 400; i++ {
			switch rng.Intn(4) {
			case 0:
				c.Send(bytes.Repeat([]byte{byte(i)}, rng.Intn(400)))
			case 1:
				ctrl.Request([]byte("ping"), func(guestipc.Reply) {})
			case 2:
				h.s.Send(11, bytes.Repeat([]byte{byte(i)}, rng.Intn(400)))
			case 3:
				h.s.RequestAsync(11, []byte("ask"), func([]byte, error) {})
			}
			h.step(1)
		}

		// Quiet, and long enough for a fresh handshake if the storm cost them
		// the session.
		h.b.fault = nil
		h.step(400)

		gs, ss := h.g.Stats(), h.s.Stats()
		if !gs.Up {
			t.Errorf("seed %d: the guest never recovered: %+v", seed, gs)
			continue
		}
		if gs.Epoch != ss.Epoch {
			t.Errorf("seed %d: epochs diverged, guest %d sdk %d", seed, gs.Epoch, ss.Epoch)
			continue
		}
		var got []byte
		var gotErr error
		done := false
		h.s.RequestAsync(12, []byte("ping"), func(b []byte, err error) {
			got, gotErr, done = b, err, true
		})
		h.step(60)
		if !done || gotErr != nil || string(got) != "pong" {
			t.Errorf("seed %d: after the storm, a request answered (%q, %v, done=%v)",
				seed, got, gotErr, done)
		}
	}
}

// THE IDENTITY CHECK, DRIVEN THROUGH BOTH REAL STATE MACHINES: the whole
// mismatch scenario, in one harness, with the positive control beside it.
//
// It is the fourth filter and the only one that can refuse a peer whose
// TRANSPORT is entirely correct. The HELLO here reaches the companion over the
// configured link and decodes cleanly; the ACK, in the second half, comes back
// from the configured source port on the outstanding corr. Every layer below the
// name is satisfied in both directions -- which is precisely the failure the
// source-port filter cannot see, because a swapped port config or a stale
// companion is a handshake that SUCCEEDS between two ends that disagree about
// what channel 1 means.
//
// Three arms, and the third is what makes the first two mean anything:
//
//  1. the COMPANION refuses the guest    (ExpectedName set, guest name wrong)
//  2. the GUEST refuses the companion    (ExpectPeer set, ACK name wrong)
//  3. the matched pair comes up          -- so the refusals are about the TOKEN
func TestSchemaMismatchIsRefusedAtBothEndsAndAMatchedPairIsNot(t *testing.T) {
	// 1. THE COMPANION REFUSES. The guest offers "other-mod/1"; this companion
	//    was built for "app/1", so no session is minted at all.
	h := newHarness(t, opts{guestName: "other-mod/1", sdkExpectedName: "app/1"})
	var evs []sdkipc.SessionEvent
	h.s.OnSession(func(e sdkipc.SessionEvent, epoch uint32) { evs = append(evs, e) })
	// Past two SearchTicks boundaries, so the guest has re-HELLOed and the rate
	// limiter below has something to limit.
	h.step(2*guestipc.SearchTicks + 10)

	ss, gs := h.s.Stats(), h.g.Stats()
	if ss.Up || ss.Sessions != 0 || ss.Epoch != 0 {
		t.Fatalf("the companion minted a session for a guest it was not built "+
			"against: %+v", ss)
	}
	if gs.Up {
		t.Fatalf("the guest came up against a companion that refused it: %+v", gs)
	}
	if ss.NameRejects == 0 {
		t.Error("NameRejects is 0 -- a refusal nobody counts is indistinguishable " +
			"from a mod that is not running, which is the state this feature exists " +
			"to tell apart")
	}
	if ss.RejectedName != "other-mod/1" {
		t.Errorf("RejectedName is %q, want the token that was OFFERED: a GUI that "+
			"cannot name what is on the port can only show a spinner",
			ss.RejectedName)
	}
	if len(evs) == 0 || evs[0] != sdkipc.SessionRejected {
		t.Errorf("session events %v, want SessionRejected first -- a DISTINCT "+
			"event, because 'never connects' and 'the wrong mod is here' need "+
			"different words", evs)
	}
	for _, e := range evs {
		if e == sdkipc.SessionUp {
			t.Fatal("a refused HELLO raised SessionUp")
		}
	}
	// THE BYE IS RATE-LIMITED, and this is the arm that keeps the refusal from
	// being worse than what it refuses: a mismatched guest re-HELLOs every
	// SearchTicks forever, so an unthrottled answer is a frame per HELLO for as
	// long as the misconfiguration lasts. The window is QuietAfter (3 s of the
	// harness's simulated clock), and the guest has sent several HELLOs by now.
	hellos := h.count(toSDK, wire.TypeHello)
	byes := h.count(toGuest, wire.TypeBye)
	if hellos < 2 {
		t.Fatalf("%d HELLOs, so the rate limit is not exercised", hellos)
	}
	if byes >= hellos {
		t.Errorf("%d BYEs for %d HELLOs: the refusal answers every HELLO, which "+
			"is the amplification the shared rate limiter exists to prevent",
			byes, hellos)
	}
	if byes == 0 {
		t.Error("no BYE at all: a refusal that puts nothing on the wire is " +
			"indistinguishable, in a capture, from a companion nobody started")
	}

	// 2. THE GUEST REFUSES. The companion answers every HELLO -- it states no
	//    expectation -- but its ACK carries a token this guest was not built
	//    against, so the token is not adopted and the guest goes on searching.
	h2 := newHarness(t, opts{guestExpectPeer: "app/1", sdkName: "other-companion/1"})
	h2.step(2*guestipc.SearchTicks + 10)
	gs2, ss2 := h2.g.Stats(), h2.s.Stats()
	if gs2.Up || gs2.Epoch != 0 {
		t.Fatalf("the guest adopted a token from an application it was not built "+
			"against: %+v", gs2)
	}
	if gs2.NameRejects == 0 {
		t.Error("the guest counted no NameRejects")
	}
	if ss2.Sessions < 2 {
		t.Errorf("the companion minted %d sessions: a guest that refuses an ACK "+
			"keeps searching, and each HELLO is unconditionally a new session at "+
			"the peer -- which is the shape an operator sees", ss2.Sessions)
	}

	// 3. THE POSITIVE CONTROL. The same machinery, one token, both ends stating
	//    it: the session comes up. Without this the two refusals above would
	//    pass just as well against a link that never connects for any reason.
	h3 := newHarness(t, opts{
		guestName: "app/1", guestExpectPeer: "app/1",
		sdkName: "app/1", sdkExpectedName: "app/1",
	})
	h3.up()
	if got := h3.s.Stats().NameRejects; got != 0 {
		t.Errorf("the matched pair rejected %d HELLOs", got)
	}
	if got := h3.g.Stats().NameRejects; got != 0 {
		t.Errorf("the matched guest rejected %d ACKs", got)
	}
	if h3.g.Stats().Epoch != h3.s.Stats().Epoch {
		t.Errorf("epoch disagreement in the matched pair: guest %#x sdk %#x",
			h3.g.Stats().Epoch, h3.s.Stats().Epoch)
	}
}

// A REFUSED HELLO MUST NOT DISTURB A LIVE SESSION, which is why the name is
// tested BEFORE "a HELLO is always a new session" rather than after it.
//
// That rule is right for a guest this companion is FOR. A HELLO from a mod it is
// not for arrives on the same socket in exactly one situation -- somebody
// pointed a second mod at this port -- and below the check, one stray datagram
// would fail every request in flight with ErrSessionLost and mint a token the
// real guest has never heard of.
func TestARefusedHelloLeavesALiveSessionAlone(t *testing.T) {
	h := newHarness(t, opts{
		guestName: "app/1", guestExpectPeer: "app/1",
		sdkName: "app/1", sdkExpectedName: "app/1",
	})
	h.up()
	before := h.s.Stats()

	// A well-formed HELLO from a DIFFERENT application, on the same wire.
	other, err := wire.AppendHello(nil, wire.Hello{
		ProtoMin: wire.Version, ProtoMax: wire.Version,
		MaxFrame: wire.DefaultMaxFrame, MaxFragments: wire.MaxFragments,
		Boot: 1, Name: "somebody-else/9",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.b.injectTo(toSDK, craft(wire.Header{Type: wire.TypeHello, Epoch: 1, Corr: 4242}, other))
	h.step(2)

	after := h.s.Stats()
	if !after.Up || after.Epoch != before.Epoch || after.Sessions != before.Sessions {
		t.Fatalf("a HELLO from another application tore down a live session: "+
			"epoch %#x -> %#x, sessions %d -> %d", before.Epoch, after.Epoch,
			before.Sessions, after.Sessions)
	}
	if after.NameRejects != before.NameRejects+1 {
		t.Errorf("NameRejects %d -> %d, want +1", before.NameRejects, after.NameRejects)
	}
	if !h.g.Stats().Up || h.g.Stats().Epoch != before.Epoch {
		t.Errorf("the real guest's session moved: %+v", h.g.Stats())
	}
}
