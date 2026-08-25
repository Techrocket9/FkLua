// Command lasterror is the fixture for fk.LastError, the import that carries
// WHAT THE ENGINE SAID across the boundary beside a status.
//
// A host call returns an i32 and never raises into wasm, so a binding that
// fails can tell a guest the KIND of failure -- "the Factorio API raised" --
// and nothing else. fk_abi.lua has recorded the engine's own message since the
// ABI existed and no import carried it, so the sentence was reachable from this
// repo's own tests and from nowhere a mod could stand.
//
// What it is FOR is a tripwire. Factorio refuses a documented subset of
// script.raise_event outright, and a suite that asserts only ok=false cannot
// tell "refused because that event is not raiseable" from "refused for some
// other reason" -- so the day the engine starts allowing one, the run goes on
// passing over a path that has silently become testable for real. Asserting the
// exact text is what makes that day loud.
//
// Four legs, and each is a property that could be wrong on its own:
//
//   - a call that RAISES, whose exact text comes back;
//   - the EMPTY case, before anything has failed;
//   - a call that SUCCEEDS, which CLEARS the slot -- without that, this would
//     mean "whatever failed last, ever", and a guest reading it after an OK call
//     would get a stale sentence that reads exactly like a fresh one;
//   - TRUNCATION: a message longer than the wrapper's own buffer, which must
//     come back whole rather than short, because the import returns the FULL
//     length and the wrapper asks again.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

//go:wasmexport fk_on_init
func onInit() {
	// NOTHING HAS FAILED YET. The slot is empty rather than holding whatever a
	// previous session left, because nothing persists it.
	fk.Log("empty: [" + fk.LastError() + "]")

	// A CALL THAT RAISES. game.tick is an ordinary attribute read; the host
	// stub's __index raises for that key, which is exactly what Factorio's own
	// does for some accesses -- the reason the member read is inside the pcall
	// at all.
	if _, err := fkapi.Game.Tick(); err == nil {
		fk.Log("raised: game.tick did not fail, so this fixture proves nothing")
	} else {
		// THE STATUS AS A NUMBER, not as its language's sentence. Go's error
		// convention prefixes "fklua: " and Rust's Status::as_str does not, and
		// this fixture is two renderings held to ONE set of expectations -- so
		// what it prints is the ABI's own code, which is 5, ERR_CALL_FAILED. The
		// status is asserted BESIDE the message because a message with the wrong
		// status next to it would be a guest reading a stale slot.
		st := uint32(0)
		if s, ok := err.(fkapi.Status); ok {
			st = uint32(s)
		}
		m := fk.LastError()
		fk.Log("raised: st=" + strconv.FormatUint(uint64(st), 10) +
			" len=" + strconv.Itoa(len(m)) + " msg=[" + m + "]")
	}

	// A CALL THAT SUCCEEDS CLEARS IT. This is the leg with teeth: the slot is
	// cleared by M.call on the way IN, so an OK call leaves it empty rather than
	// leaving the previous failure standing.
	if _, err := fkapi.Game.Speed(); err != nil {
		fk.Log("after-ok: game.speed failed: " + err.Error())
	} else {
		fk.Log("after-ok: [" + fk.LastError() + "]")
	}

	// TRUNCATION. The stub raises a message longer than fk's 256-byte scratch,
	// so the first host call reports a length that did not fit and the wrapper
	// asks again with room. A wrapper that trusted its own buffer would report
	// 256 and a message ending mid-word.
	if _, err := fkapi.Game.TicksPlayed(); err == nil {
		fk.Log("long: game.ticks_played did not fail")
	} else {
		m := fk.LastError()
		head, tail := "", ""
		if len(m) >= 8 {
			head, tail = m[:8], m[len(m)-8:]
		}
		fk.Log("long: len=" + strconv.Itoa(len(m)) + " head=" + head +
			" tail=" + tail)
	}
}

func main() {}
