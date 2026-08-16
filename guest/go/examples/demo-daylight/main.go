// Command demo-daylight is half of the two-mod fkipc demo: a slider in a web
// page drags the sun across a running Factorio.
//
// It is the Go arm. Its sibling, guest/rust/examples/demo-circle, is the Rust
// arm, and the two are meant to run IN THE SAME GAME AT THE SAME TIME -- which
// is the whole point of the pair. --enable-lua-udp binds ONE socket for the
// whole game, so every inbound datagram raises on_udp_packet_received in BOTH
// mods and each library's source-port filter is what keeps the two
// conversations apart (guest/go/fkipc's Link.deliver, and its mirror in
// guest/rust/fkipc). Run one mod and that machinery is never exercised; run
// two and it is exercised on every frame.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o demo-daylight.wasm ./examples/demo-daylight
//	fklua mod demo-daylight.wasm --name fk-demo-daylight --version 0.1.0 --author you
//
// Driven by sdk/go/cmd/ipcdemo, and packaged, launched and proved end to end by
// scripts/run-ipcdemo.sh.
//
// # The application protocol, and why it carries no floats
//
// Two channels -- 1 telemetry (MSG out), 2 control (REQ in) -- because a
// channel's seq is shared by everything on it, so a lost REQ on a mixed channel
// would raise a gap and therefore a spurious RESYNC on the telemetry.
//
// Every number on the wire is a DECIMAL INTEGER in a fixed unit: daytime is
// milli-units (0..1000), a position is centi-tiles. Formatting an f64 in a
// guest means either fmt (reflection, in a heap that is in the save) or a
// hand-written dtoa that two implementations would have to agree on digit for
// digit; a fixed-point integer is exact, allocation-free, and identical in both
// languages. The companion divides.
//
//	REQ   "set daytime 640"
//	RESP  "ok daytime 640"   -- the value ACTUALLY APPLIED, after clamping
//	RESP  "err <reason>"
//	MSG   "tick=1234 daytime=640 frozen=1 player=1 px=-350 py=1200"
//	      ...or "player=0" with no px/py when nobody is in the game.
//
// A set is a set, so the RPC is idempotent by construction -- which is what
// this protocol asks of every request, because a retried REQ may be executed
// again outside the dedup window.
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
	"github.com/Techrocket9/fklua/guest/go/fkipc"
)

const (
	// peerPort is the COMPANION's port and it is compiled in, because a guest
	// has no configuration file. ipcdemo's -daylight-port default matches it,
	// and the circle mod uses the next one along.
	peerPort = 29434

	chanTelemetry = 1
	chanControl   = 2

	// One telemetry frame every half second of game time. Outbound is free --
	// send_udp is a local side effect that never enters game state -- so the
	// only cost here is the host call, and half a second is what a slider
	// readout wants.
	telemetryTicks = 30
)

var (
	telemetry = fkipc.Chan(chanTelemetry, fkipc.PriBulk)
	control   = fkipc.Chan(chanControl, fkipc.PriControl)
)

var (
	ticks uint32

	// daytimeMilli is the last value ASKED FOR; the telemetry reports what the
	// surface actually has, which is the readback the UI shows.
	daytimeMilli uint32 = 500
	applyPending bool

	// frozen is one-shot: freeze_daytime is what makes the slider STICK. Without
	// it the engine advances daytime every tick and a set is a nudge that decays.
	frozen bool

	respBuf []byte
	telBuf  []byte
)

// Open from init(), never from fk_on_init.
//
// Package initialisers run inside _initialize, which control.lua calls on EVERY
// load, and event registrations are not saved. fk_on_init fires on a new map
// only, so a subscription made there would exist in the session that created
// the map and in no other.
func init() {
	// ProfileClient, AND IT IS MEASURED RATHER THAN CHOSEN FOR THE OBVIOUS
	// REASON. This mod is meant to run in a graphical single-player game, which
	// is what ProfileClient is for -- it omits for_player on send_udp and pumps
	// with a bare recv_udp(), which is what every graphical-client mod in the
	// ecosystem does. What was NOT known is whether that arm also works on a
	// HEADLESS server: the probe measured the omitted-for_player SEND working
	// headless on 2.0.77, but bare recv_udp() was one of the two arms that
	// CRASHED that build, and the 2.1.14 re-run only re-measured recv_udp(0).
	//
	// Measured on 2.1.14 by scripts/run-ipcdemo.sh --smoke on 2026-08-06: the
	// ProfileClient arm holds a full session on a headless --start-server, with
	// every leg green and zero drops. So ONE PROFILE SERVES BOTH the automated
	// gate and a person's graphical session, and there is no build-time switch
	// to explain to anybody.
	//
	// Its one cost, stated: RetryTicksClient is 6 and a headless server's p90
	// round trip is ~5.7 ticks, so a slow reply can be retransmitted where the
	// server profile's 15 would have waited. That is what the dedup table is
	// for, and the smoke run measures retries=0 in practice.
	// THE IDENTITY TOKEN, on both sides of the pairing, and it is the fourth
	// filter: the HELLO is the session boundary, the epoch is the frame filter,
	// the SOURCE PORT is the mod filter, and the NAME is the schema filter --
	// the only one that can refuse a peer whose transport is entirely correct.
	// Cross the two demo mods' -daylight-port/-circle-port and every layer below
	// this one is satisfied while the two ends disagree about what channel 1
	// means; here that is a session that never comes up rather than a slider
	// that does nothing.
	//
	// "/1" is the SCHEMA TAG: the author's claim about channel-contract
	// compatibility, bumped when the meaning of a channel changes, and
	// deliberately not a build id -- this guest is rebuilt constantly and the
	// pairing must survive that. run-ipcdemo.sh --smoke's identity leg swaps
	// these against a live game.
	fkipc.Open(fkipc.Config{
		Port:       peerPort,
		Profile:    fkipc.ProfileClient,
		Name:       "fk-demo-daylight/1",
		ExpectPeer: "fk-demo-daylight/1",
	})

	control.OnRequest(onRequest)
	// A gap is answered with a SNAPSHOT and never a replay: the producer
	// usually cannot replay, because the state it described no longer exists.
	telemetry.OnResync(func() { telemetry.Snapshot(sample()) })
	fkipc.OnSession(func(ev fkipc.SessionEvent) {
		fk.Log("fkipc session " + ev.String())
	})
}

// Optional now, and kept because this mod is what run-ipcdemo.sh --play joins a
// live client to: fkipc.Reload does nothing, and the export being present is
// half of what that run proves. See fkipc.Reload.
//
//go:wasmexport fk_after_load
func afterLoad() { fkipc.Reload() }

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	ticks = tick
	fkipc.Pump(tick)

	// THE EFFECT IS APPLIED HERE AND NOT IN THE HANDLER, deliberately. The
	// handler runs inside a dispatch that the engine raised from inside
	// recv_udp, so a host call there is a host call nested inside a host call;
	// and the Rust mirror refuses a write_file outright in that window because
	// its transport is out of the link. A flag plus an apply from fk_on_tick is
	// right in both languages, and it also collapses several sets in one tick
	// into one host call.
	if applyPending || !frozen {
		if s, ok := surface(); ok {
			if !frozen {
				// frozen records the ATTEMPT, not the outcome. The outcome here
				// happens to be replicated (a game-state member called at one
				// tick against one world fails identically on every peer), but
				// the join-safety contract says "never store an outbound call's
				// outcome" WITHOUT a carve-out, and this file is the exemplar
				// people copy — an exception here invites generalising it to
				// the calls where the outcome is peer-local and the store is a
				// desync. fkipc's own rawSend follows the same shape.
				s.SetFreezeDaytime(true)
				frozen = true
			}
			if applyPending {
				applyPending = false
				s.SetDaytime(float64(daytimeMilli) / 1000)
			}
		}
	}

	if tick%telemetryTicks == 0 {
		telemetry.Send(sample())
	}
}

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	if fkipc.OnEvent(id, ptr) {
		return
	}
	// ... a guest's own events would be switched on here.
}

// onRequest is the slider.
//
// THE PAYLOAD IS A VIEW into the library's own buffer and is invalid the moment
// this returns; the return value is copied by the library before it goes on the
// wire, so answering out of a reused buffer is safe.
func onRequest(r fkipc.Request) []byte {
	key, val, ok := parseSet(r.Payload)
	if !ok {
		return respErr("want: set <key> <int>")
	}
	if !eq(key, "daytime") {
		return respErr("unknown key")
	}
	// Clamped rather than refused, and the RESP carries what was APPLIED: a UI
	// that shows the ack is then showing the truth rather than its own request.
	if val < 0 {
		val = 0
	}
	if val > 1000 {
		val = 1000
	}
	daytimeMilli = uint32(val)
	applyPending = true
	return respOK("daytime", int32(daytimeMilli))
}

// sample builds one telemetry frame into a reused buffer, because a guest heap
// is in the save.
func sample() []byte {
	telBuf = append(telBuf[:0], "tick="...)
	telBuf = appendU32(telBuf, ticks)

	// The READBACK, not the request: if something else in the game moved the
	// sun the UI should show that rather than what it last asked for.
	shown := daytimeMilli
	if s, ok := surface(); ok {
		if v, err := s.Daytime(); err == nil {
			shown = uint32(v*1000 + 0.5)
		}
	}
	telBuf = append(telBuf, " daytime="...)
	telBuf = appendU32(telBuf, shown)
	telBuf = append(telBuf, " frozen="...)
	telBuf = appendU32(telBuf, btoi(frozen))

	// AN ABSENT PLAYER IS NOT AN ERROR. A headless server has none until
	// somebody connects, which is exactly how the automated smoke run works, so
	// the field is omitted rather than zeroed -- a position of (0,0) is a real
	// place and would read as a player standing at spawn.
	if p, err := fkapi.Game.GetPlayer(fkapi.OfNumber(1)); err == nil && p != nil {
		if pos, err := (fkapi.LuaPlayer{Object: *p}).Position(); err == nil {
			telBuf = append(telBuf, " player=1 px="...)
			telBuf = appendI32(telBuf, int32(pos.X*100))
			telBuf = append(telBuf, " py="...)
			telBuf = appendI32(telBuf, int32(pos.Y*100))
			return telBuf
		}
	}
	return append(telBuf, " player=0"...)
}

// surface re-reads the handle every time rather than keeping one.
//
// A HANDLE IS TRANSIENT: it is valid for the dispatch that produced it and is
// released when that dispatch returns, so a stored one is ERR_BAD_HANDLE on the
// next tick. fk.retain exists for the other case; here the re-read is one host
// call on the two ticks a second that need it.
func surface() (fkapi.LuaSurface, bool) {
	o, err := fkapi.Game.GetSurface(fkapi.OfNumber(1))
	if err != nil || o == nil {
		return fkapi.LuaSurface{}, false
	}
	return fkapi.LuaSurface{Object: *o}, true
}

func respOK(key string, val int32) []byte {
	respBuf = append(respBuf[:0], "ok "...)
	respBuf = append(respBuf, key...)
	respBuf = append(respBuf, ' ')
	return appendI32(respBuf, val)
}

func respErr(why string) []byte {
	respBuf = append(respBuf[:0], "err "...)
	return append(respBuf, why...)
}

// parseSet reads `set <key> <int>` out of a payload without allocating.
func parseSet(p []byte) (key []byte, val int32, ok bool) {
	verb, rest := field(p)
	if !eq(verb, "set") {
		return nil, 0, false
	}
	key, rest = field(rest)
	if len(key) == 0 {
		return nil, 0, false
	}
	num, _ := field(rest)
	val, ok = atoi(num)
	return key, val, ok
}

// field splits off the first space-delimited token.
func field(p []byte) (tok, rest []byte) {
	i := 0
	for i < len(p) && p[i] == ' ' {
		i++
	}
	j := i
	for j < len(p) && p[j] != ' ' {
		j++
	}
	return p[i:j], p[j:]
}

func atoi(b []byte) (int32, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	if b[0] == '-' {
		neg, b = true, b[1:]
	}
	if len(b) == 0 || len(b) > 9 {
		return 0, false
	}
	var v int32
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int32(c-'0')
	}
	if neg {
		v = -v
	}
	return v, true
}

// eq compares without materialising a string, which under -gc=leaking would be
// a permanent allocation per comparison.
func eq(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func btoi(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func appendI32(b []byte, v int32) []byte {
	if v < 0 {
		return appendU32(append(b, '-'), uint32(-v))
	}
	return appendU32(b, uint32(v))
}

func appendU32(b []byte, v uint32) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var d [10]byte
	i := len(d)
	for v > 0 {
		i--
		d[i] = byte('0' + v%10)
		v /= 10
	}
	return append(b, d[i:]...)
}
