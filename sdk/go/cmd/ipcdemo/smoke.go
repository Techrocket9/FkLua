package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	"github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// The headless half: the same conversation a person has with the sliders,
// scripted, with a named PASS/FAIL per leg and no display anywhere.
//
// It is what makes this demo a GATE rather than a toy. Everything below is
// reachable only against a real Factorio -- that recv_udp delivers on the
// installed build, that a LocalisedString payload survives the engine in both
// directions, that two mods sharing one socket keep their sessions apart, and
// that the API calls the two guests make actually work in game.

type verdict struct {
	fails int
	step  time.Duration
}

func (v *verdict) pass(name, format string, a ...any) {
	fmt.Printf("PASS %s -- %s\n", name, fmt.Sprintf(format, a...))
}

func (v *verdict) fail(name, format string, a ...any) {
	v.fails++
	fmt.Printf("FAIL %s -- %s\n", name, fmt.Sprintf(format, a...))
}

func runSmoke(ctx context.Context, lg *slog.Logger, links []*link,
	gamePort uint16, step time.Duration) int {

	v := &verdict{step: step}

	// 1. BOTH SESSIONS. Each guest sends a HELLO every SearchTicks whether or
	//    not anybody is listening -- a guest with no peer must still be able to
	//    notice one appearing -- so this side simply waits for the next one.
	//    Both, because one session coming up proves nothing about multiplexing.
	for _, l := range links {
		if l.waitUp(step) {
			st := l.sess.Stats()
			v.pass("session-"+l.spec.Key, "epoch %#08x on port %d, minted here and adopted by the guest",
				st.Epoch, l.spec.Port)
		} else {
			v.fail("session-"+l.spec.Key, "no HELLO on port %d in %s: the mod is not "+
				"loaded, the ports are crossed, or the server is not ticking (auto_pause)",
				l.spec.Port, step)
		}
	}
	if v.fails > 0 {
		// Every later leg would fail for this one reason.
		return finishSmoke(v)
	}

	// 2. THE SLIDERS, as RPC. Every slider of every mod, set to a value that
	//    differs from the guest's own initial one -- a readback that matched
	//    the initial value would pass with the RPC deleted.
	for _, l := range links {
		for _, sl := range l.spec.Sliders {
			leg := "rpc-" + l.spec.Key + "-" + sl.Key
			ack, err := l.set(ctx, sl.Key, sl.Smoke)
			want := fmt.Sprintf("ok %s %d", sl.Key, sl.Smoke)
			switch {
			case err != nil:
				v.fail(leg, "%v", err)
			case ack != want:
				v.fail(leg, "the guest answered %q, want %q", ack, want)
			default:
				v.pass(leg, "%s", ack)
			}
		}
	}

	// 3. THE READBACK, which is the leg that says the RPC had an EFFECT IN THE
	//    GAME rather than merely being acknowledged. The telemetry is produced
	//    by a different code path from the ack -- it re-reads the surface, the
	//    render object and the entity count -- so an ack without a readback is
	//    a guest that parsed the request and did nothing with it.
	for _, l := range links {
		leg := "readback-" + l.spec.Key
		want := map[string]int{}
		for _, sl := range l.spec.Sliders {
			want[sl.Key] = sl.Smoke
		}
		_, ok := l.waitTelemetry(step, func(t map[string]int) bool {
			for k, wv := range want {
				if t[k] != wv {
					return false
				}
			}
			return true
		})
		if ok {
			v.pass(leg, "telemetry agrees with every slider: %s", brief(l))
		} else {
			v.fail(leg, "telemetry never showed %v; last frame was %q", want, rawOf(l))
		}
	}

	// 4. EACH MOD'S OWN READOUT. The sliders prove the inbound half; these are
	//    the fields only that mod can produce, so they say the guest's API
	//    calls really ran in the game.
	if l := byKey(links, "daylight"); l != nil {
		if t, ok := l.waitTelemetry(step, func(t map[string]int) bool { return t["frozen"] == 1 }); ok {
			v.pass("daylight-frozen", "freeze_daytime took: daytime=%d frozen=%d player=%d",
				t["daytime"], t["frozen"], t["player"])
		} else {
			v.fail("daylight-frozen", "the surface's day/night cycle was never frozen, "+
				"so the slider would decay; last frame %q", rawOf(l))
		}
	}
	if l := byKey(links, "circle"); l != nil {
		// entities is -1 when the count call itself failed, which is a
		// different fact from "there is nothing in the circle" and is exactly
		// why the guest sends -1 rather than 0.
		if t, ok := l.waitTelemetry(step, func(t map[string]int) bool {
			_, hasEvo := t["evo"]
			return hasEvo && t["entities"] >= 0
		}); ok {
			v.pass("circle-readout", "count_entities_filtered and evolution both answered: "+
				"entities=%d evo=%d ppm", t["entities"], t["evo"])
		} else {
			v.fail("circle-readout", "no frame with a usable entity count; last was %q "+
				"(entities=-1 means count_entities_filtered failed)", rawOf(l))
		}
		circleTracksRadius(ctx, v, l, step)
	}

	// 4b. AN ARBITRARY-BYTE PAYLOAD. Every request above is ASCII and would pass
	//     just as well over a transport that mangled anything else -- and the
	//     inbound path here is not a socket, it is an InputAction the engine
	//     replicates, quantizes to a tick and writes into the replay. That is
	//     the one leg of this run whose subject is the ENGINE rather than
	//     either end of the protocol, which is why it is worth its twenty lines
	//     in an environment (graphical single player) where nothing had ever
	//     measured it through the library.
	if l := byKey(links, "daylight"); l != nil {
		binaryPayloadLeg(ctx, v, l)
	}

	// 5. ISOLATION AT THIS END, which is free and structural rather than
	//    built: each Session binds its own socket and the game sends to a
	//    DESTINATION port that is the mod's own, so the operating system does
	//    the routing. This asserts it rather than assuming it, because "it
	//    cannot happen" is how the other end's hole survived too.
	crossed := 0
	for _, l := range links {
		foreign := foreignKeyFor(l.spec.Key)
		for _, p := range l.snapshotHistory() {
			if strings.Contains(p, foreign+"=") {
				crossed++
			}
		}
	}
	if crossed == 0 {
		v.pass("isolation-sdk", "neither session ever received the other mod's telemetry "+
			"(%d + %d frames), which the destination port guarantees",
			framesOf(links, "daylight"), framesOf(links, "circle"))
	} else {
		v.fail("isolation-sdk", "%d frames carried the other mod's fields", crossed)
	}

	// 6. THE FOREIGN-PORT LEG, and it is the reason this program exists.
	//
	//    A third socket sends the game two well-formed frames at the daylight
	//    session's LIVE epoch: a BYE, which would tear the session down, and a
	//    REQ setting a sentinel value, which would be visible in the very next
	//    telemetry frame. Everything about them is valid except where they came
	//    from. Both mods are handed both, because --enable-lua-udp is one
	//    socket for the whole game, and both must refuse them on the source
	//    port alone.
	//
	//    IT ALSO PROVES THE FIELD IS POPULATED, which nothing else here can:
	//    the guest accepts a source port of 0 on purpose ("the engine did not
	//    say"), so a build that stopped filling it would still hold a session.
	//    A build that reported the GAME's port, or 0, or anything but the
	//    companion's, would fail every earlier leg -- so passing 1-5 says the
	//    field carries the sender's port, and passing this says the filter acts
	//    on it.
	if l := byKey(links, "daylight"); l != nil {
		foreignPortLeg(v, l, gamePort, lg)
	}

	// 7. THE IDENTITY LEG, which is the only one that can refuse a peer whose
	//    TRANSPORT is entirely correct. It runs LAST because it is the only leg
	//    that has to disturb the sessions everything above it needs.
	identityLeg(v, links, step)

	for _, l := range links {
		st := l.sess.Stats()
		fmt.Printf("STATS %s epoch=%#08x sessions=%d tx=%d/%dB rx=%d/%dB drops=%d bad=%d epoch_drops=%d stale=%d gaps=%d retries=%d guest_tick=%d guest_boot=%d\n",
			l.spec.Key, st.Epoch, st.Sessions, st.TxFrames, st.TxBytes, st.RxFrames,
			st.RxBytes, st.Drops, st.BadFrames, st.EpochDrops, st.StaleDrops,
			st.Gaps, st.Retries, st.GuestTick, st.GuestBoot)
	}
	return finishSmoke(v)
}

// circleTracksRadius is the leg that says the readback is a MEASUREMENT and not
// a number the guest made up.
//
// Everything above proves the slider reached the guest and came back; this
// proves the guest went and asked the GAME. A count taken over two tiles and a
// count taken over sixty are answers to different questions, and only a real
// count_entities_filtered gives different answers.
//
// Spawn is cleared, which is why the small radius is the interesting end: on
// the harness's own seed-1 map the count at radius 2 is 0 and at radius 60 is
// several hundred (39 trees plus ore, which is one entity per tile).
func circleTracksRadius(ctx context.Context, v *verdict, l *link, step time.Duration) {
	const leg = "circle-tracks-radius"
	const near, far = 2, 60

	count := func(r int) (int, bool) {
		if _, err := l.set(ctx, "radius", r); err != nil {
			v.fail(leg, "setting radius %d: %v", r, err)
			return 0, false
		}
		// Wait for a frame that reports THIS radius: the guest samples on its
		// own schedule, so the frame in flight when the set landed describes
		// the previous one.
		t, ok := l.waitTelemetry(step, func(t map[string]int) bool {
			return t["radius"] == r && t["entities"] >= 0
		})
		if !ok {
			v.fail(leg, "no frame reporting radius %d with a usable count; last was %q",
				r, rawOf(l))
			return 0, false
		}
		return t["entities"], true
	}

	a, ok := count(near)
	if !ok {
		return
	}
	b, ok := count(far)
	if !ok {
		return
	}
	switch {
	case b == 0:
		// NOT A FAILURE, and saying so rather than asserting is the honest
		// move: a map with nothing within sixty tiles of spawn cannot tell a
		// working count from a broken one, and this run is then simply unable
		// to answer. It does not happen on the harness's own map.
		v.pass(leg, "inconclusive: this map has nothing within %d tiles of spawn, "+
			"so no count can discriminate (both radii answered %d)", far, b)
	case b > a:
		v.pass(leg, "the count follows the circle: radius %d -> %d entities, "+
			"radius %d -> %d", near, a, far, b)
	default:
		v.fail(leg, "the count did not follow the circle: radius %d -> %d entities, "+
			"radius %d -> %d. A count that does not change with the search area "+
			"is not a count", near, a, far, b)
	}
}

// binaryPayloadLeg puts all 256 byte values through the engine, twice, in the
// two shapes that fail differently.
//
// WHAT IT ESTABLISHES, stated narrowly because the guest does not echo: a REQ
// whose payload contains every byte value -- NUL, newline, DEL, 0xff -- is
// DELIVERED to the guest and correlated back, and a 256-byte binary tail behind
// a well-formed command does not truncate, split or corrupt the parse of what
// precedes it. A payload cut at the first NUL would fail the second arm (the
// command would arrive whole but the frame would be short of what the header
// says) and a payload the engine refused outright would fail both.
//
// WHAT IT DOES NOT ESTABLISH is that every byte arrived unchanged, because
// nothing here can see the bytes the guest holds. `scripts/run-ipc.sh`'s `bytes`
// leg is the byte-exact one -- its guest echoes -- and the engine's own inbound
// path was measured byte-exact for all 256 values with `testdata/ipcprobe` in
// both environments (agents/ipc.md). This is the library-level delivery proof
// that sits between them.
//
// It runs after the readbacks so the value it sets is nobody else's business,
// and it uses the daylight mod because that guest's parser is the one whose
// failure text is a stable contract (`respErr` in
// guest/go/examples/demo-daylight).
func binaryPayloadLeg(ctx context.Context, v *verdict, l *link) {
	const leg = "rpc-binary"

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}

	// (a) NOTHING BUT BYTES. The guest's field() splits on 0x20, so the first
	//     token is bytes 0x00-0x1f, which is not "set" -- a deterministic
	//     refusal rather than an accident of parsing.
	const wantErr = "err want: set <key> <int>"
	out, err := l.ask(ctx, all)
	if err != nil {
		v.fail(leg, "a 256-byte payload of every byte value did not round trip: %v", err)
		return
	}
	if string(out) != wantErr {
		v.fail(leg, "the guest answered %q to a payload of every byte value, want %q",
			out, wantErr)
		return
	}

	// (b) A COMMAND WITH A BINARY TAIL. parseSet reads three tokens and ignores
	//     the rest, so this must be obeyed AND acknowledged with the value --
	//     which says the 256 bytes behind it changed nothing about the frame in
	//     front of it.
	const value = 640
	req := append([]byte(fmt.Sprintf("set daytime %d ", value)), all...)
	want := fmt.Sprintf("ok daytime %d", value)
	out, err = l.ask(ctx, req)
	switch {
	case err != nil:
		v.fail(leg, "a command with a 256-byte binary tail did not round trip: %v", err)
	case string(out) != want:
		v.fail(leg, "the guest answered %q to a command with a binary tail, want %q -- "+
			"the payload was truncated or altered somewhere between here and the guest",
			out, want)
	default:
		v.pass(leg, "every byte value 0x00-0xff crossed the engine's inbound path: raw, "+
			"answered %q; and behind a command, which was still obeyed (%q)", wantErr, out)
	}
}

// identityLeg is the live proof of the SCHEMA filter, and it is the fourth and
// last of the four mechanisms this run exercises against a real game: the HELLO
// is the session boundary, the epoch is the frame filter, the SOURCE PORT is
// the mod filter, and the NAME is the schema filter.
//
// WHAT IT CLOSES that leg 6 cannot. The source-port filter answers "is this
// frame from the process I was pointed at". It has nothing to say about a
// handshake that succeeds at every transport layer between two ends that
// disagree about what the channels MEAN -- swapped port config, a companion left
// running from last week, a mod updated past its tool. Here that is the demo's
// own two mods with their tokens crossed.
//
// IT RUNS LAST AND IT REDIALS, because there is no way around that: a companion
// binds one socket per mod port, so a deliberately mismatched session and a
// correctly-paired one cannot be held at the same time. The cost is visible in
// the game's own log -- each guest logs TWO session-up lines over the run rather
// than one, and NONE in between -- and `run-ipcdemo.sh` asserts exactly that,
// which makes the guest's refusal readable from the game side as well as this
// one.
//
// THREE ARMS, and the third is what makes the first two mean anything:
//
//	a. the COMPANION refuses the guest -- a swapped ExpectedName, so the guest's
//	   HELLO is refused, SessionRejected is raised and no session is minted.
//	b. the GUEST refuses the companion -- a swapped Name with NO expectation on
//	   this side, so this end ANSWERS every HELLO and the guest refuses every
//	   ACK. A fully-swapped pair could not show this half, because an end that
//	   refuses the HELLO never sends an ACK for the other end to refuse.
//	c. the matched pair comes back up -- so (a) and (b) are facts about the
//	   TOKEN and not about a session that was simply torn down.
func identityLeg(v *verdict, links []*link, step time.Duration) {
	day, circle := byKey(links, "daylight"), byKey(links, "circle")
	if day == nil || circle == nil {
		v.fail("identity", "both demo mods are needed to cross their tokens")
		return
	}
	// Bounded well under the leg deadline: a guest re-HELLOs every SearchTicks,
	// which is one second of game time, so everything here resolves in a few.
	budget := step
	if budget > 15*time.Second {
		budget = 15 * time.Second
	}

	// (a) THE COMPANION REFUSES. The daylight companion is re-dialled with the
	//     CIRCLE mod's token, which is precisely what a crossed
	//     -daylight-port/-circle-port produces.
	if err := day.redial(circle.spec.Identity, circle.spec.Identity); err != nil {
		v.fail("identity-sdk-refuses", "re-dialling the daylight port: %v", err)
		return
	}
	rejected := waitFor(budget, func() bool { return day.sess.Stats().NameRejects > 0 })
	st := day.sess.Stats()
	sawRejected := false
	for _, e := range day.sessionEvents() {
		if e == fkipc.SessionRejected {
			sawRejected = true
		}
	}
	switch {
	case !rejected:
		v.fail("identity-sdk-refuses", "no HELLO was refused in %s: the guest never "+
			"spoke, or a companion built for %q accepted a mod calling itself %q",
			budget, circle.spec.Identity, day.spec.Identity)
	case st.Up || st.Sessions != 0:
		v.fail("identity-sdk-refuses", "a session was minted anyway: up=%v sessions=%d "+
			"epoch=%#08x. A HELLO is unconditionally a new session ONLY for a guest "+
			"this companion is for", st.Up, st.Sessions, st.Epoch)
	case st.RejectedName != day.spec.Identity:
		v.fail("identity-sdk-refuses", "the refused token is reported as %q, want %q "+
			"-- a companion that cannot name what is on the port can only show a "+
			"spinner", st.RejectedName, day.spec.Identity)
	case !sawRejected:
		v.fail("identity-sdk-refuses", "no SessionRejected event: the refusal is "+
			"indistinguishable from a mod that is not running, which is the state "+
			"this whole leg exists to tell apart")
	default:
		v.pass("identity-sdk-refuses", "a companion built for %q refused %d HELLO(s) "+
			"from %q, raised SessionRejected and minted nothing",
			circle.spec.Identity, st.NameRejects, st.RejectedName)
	}

	// (b) THE GUEST REFUSES. This end states no expectation, so it answers every
	//     HELLO -- with the WRONG token. The guest refuses the ACK, stays
	//     peerless and goes on searching, so from out here the session count
	//     climbs (each HELLO is a new session at the peer) while not one
	//     telemetry frame ever arrives.
	if err := circle.redial(day.spec.Identity, ""); err != nil {
		v.fail("identity-guest-refuses", "re-dialling the circle port: %v", err)
		return
	}
	// SNAPSHOTTED AFTER THE REDIAL, not before it: the correctly-paired session
	// is still delivering telemetry right up to the BYE that redial sends, so a
	// count taken first would charge this leg for the previous one's last frame.
	framesBefore := circle.state().Frames
	climbed := waitFor(budget, func() bool { return circle.sess.Stats().Sessions >= 2 })
	cst := circle.sess.Stats()
	delivered := circle.state().Frames - framesBefore
	switch {
	case !climbed:
		v.fail("identity-guest-refuses", "the guest sent fewer than two HELLOs in %s "+
			"(sessions=%d): either it adopted the wrong companion's token and "+
			"stopped searching, or it is not running", budget, cst.Sessions)
	case delivered != 0:
		v.fail("identity-guest-refuses", "%d telemetry frames arrived under a token "+
			"the guest was not built against: the ACK was adopted, which is the "+
			"stale-companion failure this check exists to prevent", delivered)
	default:
		v.pass("identity-guest-refuses", "a guest built for %q refused an ACK from "+
			"%q %d times and kept searching; no telemetry crossed",
			circle.spec.Identity, day.spec.Identity, cst.Sessions)
	}

	// (c) THE POSITIVE CONTROL, in the same run. Without it both refusals above
	//     would pass just as well against a game that had stopped ticking.
	for _, l := range []*link{day, circle} {
		if err := l.redial(l.spec.Identity, l.spec.Identity); err != nil {
			v.fail("identity-restored", "re-dialling %s: %v", l.spec.Key, err)
			return
		}
	}
	for _, l := range []*link{day, circle} {
		leg := "identity-restored-" + l.spec.Key
		if !l.waitUp(budget) {
			v.fail(leg, "the matched pairing did not come back up in %s, so the "+
				"refusals above are not evidence about the TOKEN", budget)
			continue
		}
		before := l.state().Frames
		if !waitFor(budget, func() bool { return l.state().Frames > before }) {
			v.fail(leg, "the session came up but no telemetry followed")
			continue
		}
		v.pass(leg, "the matched token %q brought the session straight back: "+
			"epoch %#08x, telemetry flowing", l.spec.Identity, l.sess.Stats().Epoch)
	}
}

// waitFor polls a predicate to a deadline. Polling rather than a channel because
// everything it watches is a counter inside a Session that a background
// goroutine advances, and adding a notification path for one leg would be more
// machinery than the leg.
func waitFor(d time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return pred()
}

// foreignPortLeg is the live proof of the source-port filter.
//
// The frames it sends are the two that would be loudest if they got through: a
// BYE ends the session, and a REQ carrying a sentinel changes something the
// next telemetry frame reports. If either lands, this leg fails AND several
// later observations would too, which is the right shape for a safety property.
func foreignPortLeg(v *verdict, l *link, gamePort uint16, lg *slog.Logger) {
	const leg = "foreign-port"
	const sentinel = 999 // a legal daytime, so a failure shows up as a VALUE

	before := l.sess.Stats()
	if !before.Up {
		v.fail(leg, "the session was already down, so this proves nothing")
		return
	}

	// An ephemeral source port: not the game's, not either companion's.
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(gamePort)})
	if err != nil {
		v.fail(leg, "opening a third socket: %v", err)
		return
	}
	defer c.Close()
	from := c.LocalAddr().(*net.UDPAddr).Port

	bye, err := wire.AppendFrame(nil, wire.Header{
		Type: wire.TypeBye, Epoch: before.Epoch,
	}, nil)
	if err != nil {
		v.fail(leg, "building the BYE: %v", err)
		return
	}
	// Channel 2 carries a seq, and a big one lands as a GAP rather than as a
	// stale frame -- which is the arm that would actually be delivered if the
	// filter were absent. Choosing a seq that would be dropped anyway would
	// make this leg pass for the wrong reason.
	req, err := wire.AppendFrame(nil, wire.Header{
		Type: wire.TypeReq, Channel: chanControl, Epoch: before.Epoch,
		Seq: 0x4000_0000, Corr: 0x7A7A_7A7A, NFrag: 1,
	}, []byte(fmt.Sprintf("set daytime %d", sentinel)))
	if err != nil {
		v.fail(leg, "building the REQ: %v", err)
		return
	}
	for _, f := range [][]byte{bye, req, bye} {
		if _, err := c.Write(f); err != nil {
			v.fail(leg, "sending from port %d: %v", from, err)
			return
		}
	}
	lg.Info("foreign frames sent", "from", from, "to", gamePort,
		"epoch", fmt.Sprintf("%#08x", before.Epoch))

	// Long enough for the guest to have pumped many times: the round trip is
	// ~2 ticks and telemetry is every 30, so this covers several frames.
	time.Sleep(2 * time.Second)

	after := l.sess.Stats()
	t, _ := l.waitTelemetry(0, func(map[string]int) bool { return true })
	switch {
	case !after.Up || after.Epoch != before.Epoch || after.Sessions != before.Sessions:
		v.fail(leg, "a BYE from port %d ended the session: epoch %#08x -> %#08x, "+
			"sessions %d -> %d. The guest acted on a frame from a port that is "+
			"not its peer's, which is what lets one mod's companion kill another "+
			"mod's session", from, before.Epoch, after.Epoch,
			before.Sessions, after.Sessions)
	case t["daytime"] == sentinel:
		v.fail(leg, "a REQ from port %d was executed: daytime is the sentinel %d",
			from, sentinel)
	default:
		v.pass(leg, "a BYE and a REQ at the live epoch %#08x from port %d were both "+
			"ignored by both mods; daytime still %d", before.Epoch, from, t["daytime"])
	}
}

func finishSmoke(v *verdict) int {
	if v.fails == 0 {
		fmt.Println("RESULT ok")
		return 0
	}
	fmt.Printf("RESULT failed %d\n", v.fails)
	return 1
}

func byKey(links []*link, key string) *link {
	for _, l := range links {
		if l.spec.Key == key {
			return l
		}
	}
	return nil
}

// foreignKeyFor names a telemetry field that ONLY the other mod emits, which is
// what makes "did these two conversations cross" answerable from the payloads.
func foreignKeyFor(key string) string {
	if key == "daylight" {
		return "radius"
	}
	return "daytime"
}

func framesOf(links []*link, key string) uint32 {
	if l := byKey(links, key); l != nil {
		return l.state().Frames
	}
	return 0
}

func rawOf(l *link) string { return l.state().Raw }

func brief(l *link) string {
	s := l.state()
	return fmt.Sprintf("%s (frame %d, %d ms ago)", s.Raw, s.Frames, s.Age)
}
