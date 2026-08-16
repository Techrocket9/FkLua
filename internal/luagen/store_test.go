package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// The inlined store is actually inlined at -opt=3 and not below.
//
// The twin of TestTheInlinedLoadFiresAtOpt3AndNotBelow, and it exists for the
// same reason: the pass could silently stop firing, the emitted Lua would still
// be correct, the suite would still be green, and the win would just be gone.
func TestTheInlinedStoreFiresAtOpt3AndNotBelow(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "f") (param i32) (param i32)
			(i32.store (local.get 0) (local.get 1))))`
	for _, tc := range []struct {
		lvl    analysis.Level
		inline bool
	}{{analysis.O0, false}, {analysis.O1, false}, {analysis.O2, false}, {analysis.O3, true}} {
		src := emitAt(t, wat, tc.lvl)
		got := strings.Contains(src, "S1[t0 / 4 + 1] = ")
		if got != tc.inline {
			verb := "did not inline"
			if got {
				verb = "inlined"
			}
			t.Errorf("-opt=%d %s the store; want inline=%v", tc.lvl, verb, tc.inline)
		}
		// Whatever it does, a call to st32 must remain reachable for the
		// unaligned case rather than being dropped: align= in a memarg is a
		// HINT, and an unaligned store still has to give the right answer.
		if tc.inline && !strings.Contains(src, "st32(MEM, MEMSIZE, t0, ") {
			t.Errorf("-opt=%d inlined the store but left no unaligned fallback", tc.lvl)
		}
		// -opt=0 is the M4 reference and must still emit the plain call.
		if !tc.inline && !strings.Contains(src, "st32(MEM, MEMSIZE, v0, v1)") {
			t.Errorf("-opt=%d should still emit the st32 call:\n%s",
				tc.lvl, emitBody(t, wat, tc.lvl))
		}
	}
}

// THE HAZARD THIS WHOLE CHANGE HAS TO CLEAR.
//
// Under --persist=packed the save carries `string.pack` pages, and a flush
// repacks only the pages the DIRTY-PAGE SET says were written. That marking
// lives inside st8b/st16/st32, and every store in the system funnels into those
// three precisely so that no store can miss it. Inlining st32 walks around the
// funnel, so the inlined form has to mark its own page.
//
// Getting it wrong produces no error anywhere. The store lands in the live word
// table, every read in the session sees it, and the save simply does not carry
// it -- the guest comes back with stale memory after a reload, which in lockstep
// multiplayer is a desync rather than a message. So this asserts the observable
// consequence twice over: flush has to report the page as rewritten, AND a
// second instance restoring from those pages has to see the word.
//
// Delete the `if MEMDIRTY then ... end` line from emitInlineStore32 and this
// fails at -opt=3 with "dirty 0" and "word 0", while every other gate in the
// repo stays green.
func TestTheInlinedStoreStillDirtiesItsPage(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at))))`
	// The protocol control.lua actually runs, against a stand-in for `storage`:
	// a full pack at state_init, a flush after the guest call, a restore into a
	// fresh instance at state_load. Handing the live table over instead would
	// prove nothing -- the marking only shows up in what the SAVE carries.
	const script = `
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()      -- state_init: an all-zero heap
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["poke"](40000, 305419896)      -- aligned: the inlined fast path
print("dirty " .. a.persist.flush(storage.fk_pages))

local b = mk({})                         -- the next load
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("word " .. tostring(b.exports["peek"](40000)))
`
	want := "dirty 1\nword 305419896"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q -- the store never dirtied its page, "+
				"so the save does not carry it", lvl, got, want)
		}
	}
}

// The unaligned arm delegates to st32, which owns the page mark itself. Pinned
// separately because the two arms of the inlined store are two different pieces
// of code and one being right says nothing about the other.
func TestTheUnalignedArmOfTheInlinedStoreAlsoDirties(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at))))`
	const script = `
local storage = {}
local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["poke"](40002, 305419896)      -- 40002 % 4 == 2: the st32 fallback
print("dirty " .. a.persist.flush(storage.fk_pages))

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("word " .. tostring(b.exports["peek"](40002)))
`
	want := "dirty 1\nword 305419896"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// A store dirties the pages it SPANS, not just the one its address is in, which
// is why the mark covers a + 3 rather than only a.
//
// Only an UNALIGNED store can straddle a 4 KiB page, so this necessarily lands
// on the st32 fallback arm rather than the inlined one -- 4096 is a multiple of
// 4, so an aligned word never crosses. It is here because that arm is the half
// of the inlined store which delegates, and delegation being right is a separate
// fact from the fast path being right.
func TestAStoreStraddlingAPageBoundaryDirtiesBothPages(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at))))`
	const script = `
local storage = {}
local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["poke"](4094, 305419896)       -- bytes 4094..4097: pages 0 and 1
print("dirty " .. a.persist.flush(storage.fk_pages))

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("word " .. tostring(b.exports["peek"](4094)))
`
	want := "dirty 2\nword 305419896"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// What a store computes must not depend on how hard the optimizer worked.
//
// The suite asserts loads and stores through ordinary aligned access, so the
// cases below are the ones it is thin on and the ones the inlined form actually
// changes: the unaligned arm, a static offset folded into the address, a value
// at the top of the u32 range, and a value the wrap is what makes correct.
func TestTheInlinedStoreAgreesWithTheCallAtEveryLevel(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "pokeoff") (param $at i32) (param $v i32)
			(i32.store offset=12 (local.get $at) (local.get $v)))
		(func (export "bump") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (i32.add (local.get $v) (i32.const 1))))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at))))`

	for _, tc := range []struct{ name, expr, want string }{
		{"aligned", `(function()
			M.exports["poke"](64, 305419896) return M.exports["peek"](64) end)()`,
			"305419896"},
		{"unaligned", `(function()
			M.exports["poke"](66, 305419896) return M.exports["peek"](66) end)()`,
			"305419896"},
		// An unaligned store must not disturb the words on either side of it.
		{"unaligned neighbours", `(function()
			M.exports["poke"](128, 4294967295)
			M.exports["poke"](132, 4294967295)
			M.exports["poke"](130, 0)
			return M.exports["peek"](128) .. "/" .. M.exports["peek"](132) end)()`,
			"65535/4294901760"},
		{"static offset", `(function()
			M.exports["pokeoff"](64, 7) return M.exports["peek"](76) end)()`, "7"},
		{"top of range", `(function()
			M.exports["poke"](64, 4294967295) return M.exports["peek"](64) end)()`,
			"4294967295"},
		// The value operand wraps: 0xFFFFFFFF + 1 is 0, and the store has to
		// see the wasm value rather than 2^32.
		{"value wraps", `(function()
			M.exports["bump"](64, 4294967295) return M.exports["peek"](64) end)()`, "0"},
		// Out of range traps, and the spec says a trapping store changes
		// NOTHING -- so the word below the limit must survive the attempt.
		{"oob traps", `M.exports["poke"](65534, 1)`, "TRAP\tout of bounds memory access"},
		{"oob leaves memory alone", `(function()
			M.exports["poke"](65532, 42)
			pcall(function() M.exports["poke"](65534, 1) end)
			return M.exports["peek"](65532) end)()`, "42"},
		{"negative-looking address traps", `M.exports["poke"](4294967292, 1)`,
			"TRAP\tout of bounds memory access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sameAtEveryLevel(t, wat, tc.expr, tc.want)
		})
	}
}

// Each operand is evaluated exactly once and in wasm's order.
//
// Once the store stops being a single call it names its value in two places,
// and printing a composite expression into both arms would evaluate it twice --
// twice the work, and twice the chance to trap from a program point the wasm
// gave one operation. The rule is that only a bare name or numeral is left in
// place; anything else goes through t1.
func TestTheInlinedStoreEvaluatesACompositeValueOnce(t *testing.T) {
	src := emitBody(t, `(module (memory 1)
		(func (export "f") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (i32.mul (local.get $v) (local.get $v)))))`,
		analysis.O3)
	if !strings.Contains(src, "t1 = ") {
		t.Errorf("a composite value must be materialised in t1, not printed twice:\n%s", src)
	}
	if strings.Count(src, "i32_mul") > 1 {
		t.Errorf("the value expression appears more than once:\n%s", src)
	}
	// A bare name costs nothing to name twice, so it stays in place rather than
	// paying for a move.
	plain := emitBody(t, `(module (memory 1)
		(func (export "f") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v))))`, analysis.O3)
	if strings.Contains(plain, "t1 = ") {
		t.Errorf("a bare name needs no scratch move:\n%s", plain)
	}
}

// The inlined store still reduces its value modulo 2^32 on the aligned path.
//
// MEM is required to hold genuine u32 words -- packed mode feeds them straight
// to string.pack("<I4"), which raises on anything else -- and st32 is what
// guaranteed that before. Dropping the reduction would also quietly make the
// inlined form a NON-absorbing consumer where the call was an absorbing one,
// which is the shape of the deferral miscompile the audit found.
func TestTheInlinedStoreStillReducesItsValue(t *testing.T) {
	src := emitBody(t, `(module (memory 1)
		(func (export "f") (param i32) (param i32)
			(i32.store (local.get 0) (local.get 1))))`, analysis.O3)
	if !strings.Contains(src, "S1[t0 / 4 + 1] = v1 % 4294967296.0") {
		t.Errorf("the aligned path must reduce like st32 does:\n%s", src)
	}
}
