package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// A guest's own memcpy is a byte loop the runtime already does better, so its
// body is replaced. These assert the substitution is behaviourally invisible --
// which matters more here than anywhere else in the emitter, because the whole
// function body is thrown away on the strength of a name plus a shape check.

// byteCopy is a minimal, honest memcpy: a byte loop with the C signature.
const byteCopy = `(module (memory 1)
	(func $memcpy (param $d i32) (param $s i32) (param $n i32) (result i32)
		(local $i i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(i32.store8 (i32.add (local.get $d) (local.get $i))
					(i32.load8_u (i32.add (local.get $s) (local.get $i))))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $d))
	(func (export "f") (param $d i32) (param $s i32) (param $n i32) (result i32)
		(call $memcpy (local.get $d) (local.get $s) (local.get $n)))
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store8 (local.get $a) (local.get $v)))
	(func (export "peek") (param $a i32) (result i32)
		(i32.load8_u (local.get $a))))`

func TestAGuestMemcpyBecomesTheRuntimesOwn(t *testing.T) {
	src := emitBody(t, byteCopy, analysis.O1)
	if !strings.Contains(src, "mem_copy(MEM, MEMSIZE, v0, v1, v2) return v0") {
		t.Errorf("memcpy should be replaced by mem_copy:\n%s", src)
	}
	// -opt=0 is the bisect reference and compiles the body as written.
	if zero := emitBody(t, byteCopy, analysis.O0); strings.Contains(zero, "mem_copy(MEM") {
		t.Errorf("-opt=0 must compile the guest's own memcpy:\n%s", zero)
	}
}

// The substitution has to move the same bytes, at every level.
func TestTheSubstitutedCopyMovesTheSameBytes(t *testing.T) {
	expr := `(function()
		local p, f = M.exports["poke"], M.exports["f"]
		for i = 0, 7 do p(100 + i, i * 11 + 1) end
		f(200, 100, 8)
		local s = ""
		for i = 0, 7 do s = s .. M.exports["peek"](200 + i) .. "," end
		return s end)()`
	sameAtEveryLevel(t, byteCopy, expr, "1,12,23,34,45,56,67,78,")
}

// Overlap is where memcpy and mem_copy could genuinely differ: C leaves it
// undefined, mem_copy has memory.copy's memmove semantics. The substitution is
// only sound because that is strictly MORE defined -- and a forward-overlapping
// copy is exactly where a naive byte loop and a memmove disagree, so this pins
// which one the emitted code now behaves like.
func TestAnOverlappingCopyIsMemmoveSemantics(t *testing.T) {
	expr := `(function()
		local p, f = M.exports["poke"], M.exports["f"]
		for i = 0, 5 do p(300 + i, i + 1) end
		f(302, 300, 4)
		local s = ""
		for i = 0, 5 do s = s .. M.exports["peek"](300 + i) .. "," end
		return s end)()`
	// memmove: dst overlaps src ahead of it, so the source is read before it is
	// overwritten -- 1,2,1,2,3,4 rather than the 1,2,1,2,1,2 a forward byte loop
	// would smear.
	got := runAt(t, byteCopy, expr, analysis.O1)
	if got != "1,2,1,2,3,4," {
		t.Errorf("overlapping copy = %q, want memmove semantics %q", got, "1,2,1,2,3,4,")
	}
}

// The name section carries no semantics, so a name alone must never change what
// a program computes. Each of these is named memcpy and must still be compiled.
func TestASameNamedFunctionThatIsNotAMemoryShuffleIsCompiled(t *testing.T) {
	cases := []struct{ name, wat string }{
		{"it calls something", `(module (memory 1)
			(func $helper (result i32) (i32.const 3))
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (call $helper))
				(local.get 0))
			(func (export "f") (result i32) (call $memcpy (i32.const 0) (i32.const 1) (i32.const 2))))`},
		{"it reads a global", `(module (memory 1) (global $g i32 (i32.const 7))
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (global.get $g))
				(local.get 0))
			(func (export "f") (result i32) (call $memcpy (i32.const 0) (i32.const 1) (i32.const 2))))`},
		{"it writes no memory at all", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.add (local.get 0) (local.get 2)))
			(func (export "f") (result i32) (call $memcpy (i32.const 0) (i32.const 1) (i32.const 2))))`},
		{"its signature is not C's", `(module (memory 1)
			(func $memcpy (param i32 i32) (result i32)
				(i32.store8 (local.get 0) (local.get 1)) (local.get 0))
			(func (export "f") (result i32) (call $memcpy (i32.const 0) (i32.const 1))))`},
		{"it does float arithmetic", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (i32.trunc_f64_u (f64.add (f64.const 1) (f64.const 2))))
				(local.get 0))
			(func (export "f") (result i32) (call $memcpy (i32.const 0) (i32.const 1) (i32.const 2))))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, lvl := range allLevels {
				if src := emitBody(t, tc.wat, lvl); strings.Contains(src, "mem_copy(MEM") {
					t.Errorf("-opt=%s replaced a function that is not a memory shuffle:\n%s", lvl, src)
				}
			}
		})
	}
}

// memset and memmove take the same route.
func TestMemsetAndMemmoveAreRecognisedToo(t *testing.T) {
	const fill = `(module (memory 1)
		(func $memset (param $d i32) (param $c i32) (param $n i32) (result i32)
			(local $i i32)
			(block $done
				(loop $top
					(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
					(i32.store8 (i32.add (local.get $d) (local.get $i)) (local.get $c))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top)))
			(local.get $d))
		(func (export "f") (param $d i32) (param $c i32) (param $n i32) (result i32)
			(call $memset (local.get $d) (local.get $c) (local.get $n)))
		(func (export "peek") (param $a i32) (result i32) (i32.load8_u (local.get $a))))`
	if src := emitBody(t, fill, analysis.O1); !strings.Contains(src, "mem_fill(MEM, MEMSIZE, v0, v1, v2)") {
		t.Errorf("memset should be replaced by mem_fill:\n%s", src)
	}
	expr := `(function()
		M.exports["f"](400, 0xAB, 5)
		local s = "" for i = 0, 5 do s = s .. M.exports["peek"](400 + i) .. "," end
		return s end)()`
	// Five bytes set, the sixth untouched. mem_fill takes c mod 256, as C does.
	sameAtEveryLevel(t, fill, expr, "171,171,171,171,171,0,")
}

// A substituted copy or fill still has to reach the SAVE in --persist=packed.
//
// This is the hazard class the audit found twice, most recently in fk_wstr: a
// writer that bypasses the store funnel and marks nothing lands its bytes in the
// live table, reads back correctly all session, and is simply absent from the
// save -- a desync one load cycle later, a long way from the code that wrote it.
// mem_copy and mem_fill each mark their whole span in one call, so the
// substitution is safe; that is asserted here rather than assumed, for BOTH --
// the comment claimed both and the test exercised only the copy.
//
// The three writes share the flush run, which the old byte range could not have
// been asked to do: DLO..DHI over pages 1 and 9 reported all nine in between, so
// the copy's own page was indistinguishable from the span the seed alone would
// have produced, and the test had to move the seed before the baseline pack to
// say anything. Against a page SET the answer is three, and three is only
// reachable if each of the three writers marked its own destination page.
func TestASubstitutedCopyReachesTheSave(t *testing.T) {
	const wat = `(module (memory 1)
		(func $memcpy (param $d i32) (param $s i32) (param $n i32) (result i32)
			(local $i i32)
			(block $done
				(loop $top
					(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
					(i32.store8 (i32.add (local.get $d) (local.get $i))
						(i32.load8_u (i32.add (local.get $s) (local.get $i))))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top)))
			(local.get $d))
		(func $memset (param $d i32) (param $c i32) (param $n i32) (result i32)
			(local $i i32)
			(block $done
				(loop $top
					(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
					(i32.store8 (i32.add (local.get $d) (local.get $i)) (local.get $c))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top)))
			(local.get $d))
		(func (export "seed") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "copy") (param $d i32) (param $s i32) (param $n i32) (result i32)
			(call $memcpy (local.get $d) (local.get $s) (local.get $n)))
		(func (export "fill") (param $d i32) (param $c i32) (param $n i32) (result i32)
			(call $memset (local.get $d) (local.get $c) (local.get $n)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at)))
		(func (export "peek8") (param $at i32) (result i32)
			(i32.load8_u (local.get $at))))`
	const script = `
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["seed"](8000, 305419896)       -- page 1, through the store funnel
a.exports["copy"](40000, 8000, 4)        -- page 9, through the substituted memcpy
a.exports["fill"](50000, 171, 5)         -- page 12, through the substituted memset
print("dirty " .. a.persist.flush(storage.fk_pages))

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("word " .. tostring(b.exports["peek"](40000)))
print("byte " .. tostring(b.exports["peek8"](50000)))
`
	want := "dirty 3\nword 305419896\nbyte 171"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q -- a bulk writer never dirtied its "+
				"page, so the save does not carry it", lvl, got, want)
		}
	}
}
