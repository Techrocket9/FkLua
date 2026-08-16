package luagen

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// memory.copy and memory.fill.
//
// These are runtime helpers rather than inline loops for one reason: binaryen's
// --llvm-memory-copy-fill-lowering, which is what a guest gets without them,
// emits a BYTE-at-a-time loop, and in a word-table memory every byte store is a
// read-modify-write of a whole word plus a dirty-page test. Measured on
// a 64 KiB aligned copy under lua52f:
//
//	byte loop, compiled     173.22 ns/byte
//	native memory.copy        4.11 ns/byte
//	native memory.fill        2.24 ns/byte
//
// AND THE RAGGED PATH MATTERS AS MUCH AS THE FAST ONE. It was a byte loop
// until a real TinyGo guest showed why that is not good enough: its allocator
// handed out a destination at 1 mod 4 against an aligned source, so 64 of 66
// copies missed the fast path entirely and ran at ~100 ns/byte -- slower than
// TinyGo's own memmove, which is how enabling bulk-memory first measured 3.5x
// SLOWER. Aligning the destination and assembling each destination word from
// two source words takes that to 8.01, and the same guest to 5.78x FASTER.
//
// The correctness cases below are the ones the fast path can plausibly get
// wrong, which is why each is here rather than a single happy-path copy.

const bulkWAT = `(module
	(memory (export "memory") 4)
	(func (export "cp") (param i32 i32 i32)
		(memory.copy (local.get 0) (local.get 1) (local.get 2)))
	(func (export "fl") (param i32 i32 i32)
		(memory.fill (local.get 0) (local.get 1) (local.get 2)))
	(func (export "st") (param i32 i32) (i32.store8 (local.get 0) (local.get 1)))
	(func (export "ld") (param i32) (result i32) (i32.load8_u (local.get 0))))`

// bulkExpr builds a Lua expression that seeds memory, runs ops, and reports
// bytes back as a comma-joined string.
func bulkExpr(setup, read string) string {
	return `(function()
	local cp, fl = M.exports["cp"], M.exports["fl"]
	local st, ld = M.exports["st"], M.exports["ld"]
	local function seed(at, n) for i = 0, n-1 do st(at + i, (i + 1) % 251) end end
	local function bytes(at, n)
		local t = {}
		for i = 0, n-1 do t[#t+1] = ld(at + i) end
		return table.concat(t, ",")
	end
	` + setup + `
	return ` + read + `
end)()`
}

// The aligned, word-multiple fast path -- and the ragged one beside it, since a
// copy that only ever ran aligned would leave the byte edges untested.
func TestBulkCopyAlignedAndRagged(t *testing.T) {
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`seed(0, 32) fl(128, 0, 32) cp(129, 3, 7)`,
			`bytes(129, 7) .. "|" .. bytes(3, 7)`),
		"4,5,6,7,8,9,10|4,5,6,7,8,9,10")
}

// memory.copy is MEMMOVE, not memcpy: the ranges may overlap and the result
// must read as if the source were consumed first. Both directions, because the
// fast path picks its loop direction and picking wrong only shows up here.
func TestBulkCopyOverlapsBothWays(t *testing.T) {
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`seed(200, 8) cp(202, 200, 6)`, `bytes(200, 8)`),
		"1,2,1,2,3,4,5,6")
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`seed(300, 8) cp(300, 302, 6)`, `bytes(300, 8)`),
		"3,4,5,6,7,8,7,8")
}

// The fill value is a byte, so it is truncated -- and the aligned path builds a
// whole word out of it, which is where a missing truncation would show up as
// three wrong bytes out of four.
func TestBulkFillTruncatesToAByte(t *testing.T) {
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`fl(400, 0, 8) fl(400, 43981, 8)`, `bytes(400, 8)`),
		"205,205,205,205,205,205,205,205")
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`fl(500, 0, 8) fl(501, 7, 3)`, `bytes(500, 8)`),
		"0,7,7,7,0,0,0,0")
}

// A zero length is a no-op rather than a trap, even where the address alone
// would be out of range -- the spec checks the RANGE.
func TestBulkZeroLengthDoesNothing(t *testing.T) {
	sameAtEveryLevel(t, bulkWAT,
		bulkExpr(`fl(600, 9, 4) cp(600, 0, 0) fl(600, 1, 0)`, `bytes(600, 4)`),
		"9,9,9,9")
}

// Out of range traps, and it traps BEFORE moving anything. A helper that
// checked bounds per element would leave a partially-completed copy behind,
// which the spec does not allow and which is far worse than a clean trap.
func TestBulkOutOfRangeTrapsBeforeMoving(t *testing.T) {
	got := runAt(t, bulkWAT, bulkExpr(
		`seed(700, 4)
		local before = bytes(700, 4)
		local ok = pcall(function() cp(262140, 700, 16) end)`,
		`tostring(ok) .. "|" .. before .. "|" .. bytes(700, 4)`), analysis.O3)
	if got != "false|1,2,3,4|1,2,3,4" {
		t.Errorf("got %q, want %q (trapped, and the source untouched)",
			got, "false|1,2,3,4|1,2,3,4")
	}
}

// The whole point is that these are CALLS, not inlined loops. An emitter that
// unrolled them would lose the single bounds check and the single page mark
// update, which is where the 48x lives.
func TestBulkOpsCallTheRuntimeHelpers(t *testing.T) {
	src := emitBody(t, bulkWAT, analysis.O3)
	for _, want := range []string{"mem_copy(MEM, MEMSIZE,", "mem_fill(MEM, MEMSIZE,"} {
		if !strings.Contains(src, want) {
			t.Errorf("expected a call to %q in:\n%s", want, src)
		}
	}
}

// Every alignment pair, every short length, and overlap in both directions.
//
// The ragged path stopped being a byte loop and became "align the destination,
// then assemble each destination word from two source words with a shift" --
// which is 19x faster and exactly the kind of code that is subtly wrong at one
// offset. A spot check would not have found that; this walks all 16 alignment
// combinations and reads back every byte, including the ones AROUND the copy
// that must not have moved.
//
// Writing it found a bug in the test rather than the code: the overlap model
// read back its own writes, where memmove behaves as if the source were read
// entirely first.
func TestBulkCopyIsCorrectAtEveryAlignment(t *testing.T) {
	const src, dst = 4096, 8192
	// One driver rather than 16*41 round trips through lua52f: the harness
	// cost would dominate and the test would be too slow to keep.
	got := runAt(t, bulkWAT, bulkExpr(`
	local bad = 0
	for sa = 0, 3 do
	 for da = 0, 3 do
	  for n = 0, 40 do
	    for i = 0, 80 do st(`+itoa(src)+` + i, (i * 7 + 3) % 256) end
	    for i = 0, 80 do st(`+itoa(dst)+` + i, 238) end
	    local want = {}
	    for i = 0, 80 do want[i] = 238 end
	    for i = 0, n - 1 do want[da + i] = ((sa + i) * 7 + 3) % 256 end
	    cp(`+itoa(dst)+` + da, `+itoa(src)+` + sa, n)
	    for i = 0, 80 do
	      if ld(`+itoa(dst)+` + i) ~= want[i] then bad = bad + 1 end
	    end
	  end
	 end
	end
	-- Overlap both ways. The model SNAPSHOTS first, because memmove behaves as
	-- if the whole source were read before any write.
	for off = -8, 8 do
	  for n = 1, 24 do
	    if off ~= 0 then
	      for i = 0, 60 do st(20000 + i, (i * 5 + 1) % 256) end
	      local want = {}
	      for i = 0, 60 do want[i] = (i * 5 + 1) % 256 end
	      local snap = {}
	      for i = 0, n - 1 do snap[i] = want[10 + i] end
	      for i = 0, n - 1 do want[10 + off + i] = snap[i] end
	      cp(20000 + 10 + off, 20000 + 10, n)
	      for i = 0, 60 do
	        if ld(20000 + i) ~= want[i] then bad = bad + 1 end
	      end
	    end
	  end
	end
	-- FILL at every alignment and length, same shape. It had the same byte-loop
	-- ragged path as the copy, and the same TinyGo destinations reach it.
	for da = 0, 3 do
	  for n = 0, 40 do
	    for i = 0, 80 do st(30000 + i, 238) end
	    local want = {}
	    for i = 0, 80 do want[i] = 238 end
	    for i = 0, n - 1 do want[da + i] = 171 end
	    fl(30000 + da, 171, n)
	    for i = 0, 80 do
	      if ld(30000 + i) ~= want[i] then bad = bad + 1 end
	    end
	  end
	end`, `"wrong=" .. bad`), analysis.O3)
	if got != "wrong=0" {
		t.Errorf("got %q, want %q", got, "wrong=0")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// fk_wstr at every alignment and length.
//
// The length sweep runs to 80 -- twenty words -- because the body writes FOUR
// words per string.unpack and then finishes the odd ones singly, so the shape
// that breaks is a length whose word count is not a multiple of four. Stopping
// at ten words would exercise only two full batches and could miss an
// off-by-one where the batched loop and the remainder loop meet.
//
// It is the path every string takes into guest memory on the host-call ABI's
// return side, and it had the same per-byte shape mem_copy and mem_fill had.
// The ABI's allocator makes no alignment promise, so the misaligned case is the
// normal one rather than the exception.
func TestStringIntoMemoryIsCorrectAtEveryAlignment(t *testing.T) {
	got := runAt(t, bulkWAT, `(function()
	local ld = M.exports["ld"]
	local wstr = M.memio.wstr
	local bad = 0
	for a = 0, 3 do
	  for n = 0, 80 do
	    -- A string with bytes spanning the whole range, including 0 and 255,
	    -- since a word is assembled by multiplication and a high byte is where
	    -- a sign or overflow mistake would show.
	    local t = {}
	    for i = 1, n do t[i] = string.char((i * 37 + 11) % 256) end
	    local s = table.concat(t)
	    for i = 0, 100 do M.memio.st8(50000 + i, 238) end
	    wstr(50000 + a, s)
	    for i = 0, 100 do
	      local want = 238
	      if i >= a and i < a + n then want = ((i - a + 1) * 37 + 11) % 256 end
	      if ld(50000 + i) ~= want then bad = bad + 1 end
	    end
	  end
	end
	return "wrong=" .. bad
end)()`, analysis.O3)
	if got != "wrong=0" {
		t.Errorf("got %q, want %q", got, "wrong=0")
	}
}
