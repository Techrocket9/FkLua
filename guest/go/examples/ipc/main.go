// Command ipc is the fkipc wiring fixture: the four lines a guest author writes
// to get an IPC link, and nothing else.
//
// It is deliberately minimal, because what it proves is not that the protocol
// works -- the host-side conformance suite proves that against the same state
// machine -- but the two things only a real TinyGo build can say. First, that
// the whole package COMPILES for wasm-unknown at all, which is where a
// host-buildable state machine could quietly have picked up something the
// target does not have. Second, that the event id survives inlining into
// fk.subscribe as an i32.const, so `fklua mod` ships ONE event descriptor
// instead of every descriptor the pinned description declares.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o ipc.wasm ./examples/ipc
//	fklua mod ipc.wasm --name fk-ipc --version 0.1.0 --author you
//
// Run it against a companion built from sdk/go, with the game started as
// `factorio --enable-lua-udp 25409` and the companion listening on 25411.
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkipc"
)

// Two channels: one for state going out and one for control coming back.
//
// Splitting them is the advice rather than decoration. A channel's seq is
// shared by everything on it, so a lost REQ raises a gap -- and therefore a
// RESYNC and a snapshot -- on whatever telemetry shares the channel.
var (
	state   = fkipc.Chan(1, fkipc.PriBulk)
	control = fkipc.Chan(2, fkipc.PriControl)
)

var (
	ticks   uint32
	echo    []byte
	lastCmd []byte

	// The bulk leg. wantBulk is set by a handler and acted on from fk_on_tick,
	// which is the whole point of it being a flag -- see the OnRequest comment.
	wantBulk bool
	bulk     []byte
	queued   = []byte("queued")
)

// bulkName is the file this guest writes into script-output when asked.
const bulkName = "fkipc-gate.bin"

// Open from init(), never from fk_on_init.
//
// Package initialisers run inside _initialize, which runs on EVERY load, and
// event registrations are not saved. fk_on_init fires on a new map only, so a
// subscription made there would exist in the session that created the map and
// in no other.
func init() {
	// THE IDENTITY TOKEN, on both sides of the pairing. It is the fourth filter
	// -- the HELLO is the session boundary, the epoch is the frame filter, the
	// SOURCE PORT is the mod filter, and the NAME is the schema filter, the only
	// one that can refuse a peer whose transport is entirely correct. Setting it
	// here is what makes scripts/run-ipc.sh's matched pairing a POSITIVE CONTROL
	// for the check rather than a run in which it is merely switched off.
	//
	// "/1" is the SCHEMA TAG: the author's claim about channel-contract
	// compatibility, bumped when the meaning of a channel changes. Deliberately
	// not a build id -- this fixture is rebuilt on every run of the gate.
	fkipc.Open(fkipc.Config{
		Port:       25411,
		Profile:    fkipc.ProfileServer,
		Name:       "fk-ipc/1",
		ExpectPeer: "fk-ipc/1",
	})

	control.OnMessage(func(m fkipc.Message) {
		// THE PAYLOAD IS A VIEW and is invalid the moment this returns.
		lastCmd = append(lastCmd[:0], m.Payload...)
	})
	// An echo, into a reused buffer. Returning r.Payload directly would also
	// work today -- the RESP is encoded before the view expires -- but a
	// handler that keeps anything must copy, and the fixture should show the
	// shape that is always right rather than the one that happens to be.
	control.OnRequest(func(r fkipc.Request) []byte {
		// "bulk" asks for a FILE rather than an echo, and SETTING A FLAG rather
		// than writing here is the shape worth copying. A WriteBulk from inside
		// an inbound handler is a host call nested inside a host call -- and in
		// the Rust mirror it is refused outright, because the transport is out
		// of the link for the duration of the poll (fkipc::Link::pump_begin).
		// A flag plus a write from fk_on_tick works in both languages.
		if string(r.Payload) == "bulk" {
			wantBulk = true
			return queued
		}
		echo = append(echo[:0], r.Payload...)
		return echo
	})
	state.OnResync(func() {
		state.Snapshot(snapshot())
	})
	fkipc.OnSession(func(ev fkipc.SessionEvent) {
		fk.Log("fkipc session " + ev.String())
	})
}

// THE EXPORT IS OPTIONAL NOW AND IS KEPT ON PURPOSE, because it is the shape
// every guest written against the old four-line wiring still has, and
// internal/guest's TestAJoiningPeerStaysByteIdenticalToTheServer drives THIS
// guest: keeping it is what makes that test prove the whole wiring is
// join-safe, export and all, rather than only the library behind it.
//
// fkipc.Reload does nothing. A load is not a session boundary, because
// fk_after_load fires on a joining multiplayer client and on no other peer --
// see the comment on fkipc.Reload, which is the one to read before changing
// anything about session lifetime.
//
//go:wasmexport fk_after_load
func afterLoad() { fkipc.Reload() }

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	ticks = tick
	fkipc.Pump(tick)
	if wantBulk {
		wantBulk = false
		// One datagram instead of sixteen, and the peer gets a length and an
		// FNV-1a-32 it can verify exactly rather than having to guess when the
		// file is finished. Prefer this to a fragmented message for anything
		// above one frame: the transport is localhost-only, so the peer is
		// always on this filesystem.
		fkipc.WriteBulk(state, bulkName, bulkPayload())
	}
	if tick%60 == 0 {
		state.Send(snapshot())
	}
}

// bulkPayload is a deterministic kilobyte: the 256 byte values, four times.
//
// Built once into a retained buffer rather than per call, because a guest heap
// is in the save. Every byte value is in it because that is the property the
// probe measured on the real transport and the one a file path has no reason to
// preserve for free.
func bulkPayload() []byte {
	if len(bulk) == 0 {
		bulk = make([]byte, 1024)
		for i := range bulk {
			bulk[i] = byte(i)
		}
	}
	return bulk
}

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	if fkipc.OnEvent(id, ptr) {
		return
	}
	// ... a guest's own events would be switched on here.
}

var snapBuf []byte

// snapshot is a stand-in for whatever the mod actually streams. It is built
// into a reused buffer because a guest heap is in the save, which is the same
// reason the library copies a payload rather than keeping the caller's slice.
func snapshot() []byte {
	snapBuf = append(snapBuf[:0], "tick="...)
	return appendU32(snapBuf, ticks)
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
