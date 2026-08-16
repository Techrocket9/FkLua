package luagen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// emitWith is emitAt in a chosen persistence mode. The mode is not cosmetic
// here: packed arms a dirty-page set whose marking lives inside the store
// helpers, and what the emitter is allowed to inline depends on it.
func emitWith(t *testing.T, wat string, lvl analysis.Level, mode PersistMode) string {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: lvl, Persist: mode})
	if err != nil {
		t.Fatalf("emit at -opt=%s: %v", lvl, err)
	}
	return src
}

// An 8-byte access is ONE access, and it now reads or writes both words
// directly instead of delegating to two 32-bit ones.
//
// The delegation was doing the bounds check three times, the alignment test
// twice and the page mark twice, for a pair of adjacent words at i and
// i+1. Removing it is worth 1.48x on `dot`. What it puts at risk is the ragged
// path, which is now a genuinely different branch rather than the same code
// running twice -- so every alignment gets walked below.
const f64WAT = `(module
	(memory (export "memory") 4)
	(func (export "stf") (param i32 f64) (f64.store (local.get 0) (local.get 1)))
	(func (export "ldf") (param i32) (result f64) (f64.load (local.get 0)))
	(func (export "sti") (param i32 i64) (i64.store (local.get 0) (local.get 1)))
	(func (export "ldi") (param i32) (result i64) (i64.load (local.get 0)))
	(func (export "st8") (param i32 i32) (i32.store8 (local.get 0) (local.get 1)))
	(func (export "ld8") (param i32) (result i32) (i32.load8_u (local.get 0))))`

// Every alignment, and values chosen so a lost or swapped word shows up.
//
// Run at EVERY level since -opt=3 inlines the aligned fast path: below it the
// eight bytes go through ld_f64/st_f64, at it they go through generated Lua
// that reassembles the double itself. Those are two different pieces of code
// and one being right says nothing about the other.
func TestEightByteAccessRoundTripsAtEveryAlignment(t *testing.T) {
	for _, lvl := range allLevels {
		eightByteRoundTrip(t, lvl)
	}
}

func eightByteRoundTrip(t *testing.T, lvl analysis.Level) {
	t.Helper()
	got := runAt(t, f64WAT, `(function()
	local stf, ldf = M.exports["stf"], M.exports["ldf"]
	local bad = 0
	-- Values that exercise the sign, the exponent and both halves of the
	-- mantissa. A swapped word pair survives a symmetric value and nothing
	-- else, which is why none of these are symmetric.
	local vals = {0.0, -0.0, 1.0, -1.0, 0.1, -12345.6789,
	              1e-308, 1e308, 4.9406564584124654e-324, 1/0, -1/0,
	              3.141592653589793, 2.2250738585072014e-308}
	for a = 0, 7 do
	  for _, v in ipairs(vals) do
	    stf(1024 + a, v)
	    local r = ldf(1024 + a)
	    -- Compare the BITS, not the value: -0.0 == 0.0 in Lua, so a sign bit
	    -- lost in transit would compare equal and pass.
	    if r ~= v or (v == 0.0 and (1/r) ~= (1/v)) then bad = bad + 1 end
	  end
	end
	return "wrong=" .. bad
end)()`, lvl)
	if got != "wrong=0" {
		t.Errorf("-opt=%s: got %q, want %q", lvl, got, "wrong=0")
	}
}

// The neighbouring bytes must be untouched, and both words must actually land.
// A store that wrote only the low word would pass a round trip through the
// matching load if the load also read only the low word.
func TestAnEightByteStoreWritesExactlyEightBytes(t *testing.T) {
	for _, lvl := range allLevels {
		eightByteStoreWidth(t, lvl)
	}
}

func eightByteStoreWidth(t *testing.T, lvl analysis.Level) {
	t.Helper()
	got := runAt(t, f64WAT, `(function()
	local sti, st8, ld8 = M.exports["sti"], M.exports["st8"], M.exports["ld8"]
	local bad = 0
	for a = 0, 7 do
	  for i = 0, 24 do st8(2048 + i, 238) end
	  -- 0x0807060504030201: every byte distinct, so a swapped or duplicated
	  -- word is visible byte by byte. An i64 crosses the boundary as a
	  -- (lo, hi) PAIR, not one Lua number -- passing it as one is how the
	  -- first version of this test failed, on its own mistake rather than
	  -- the emitter's.
	  sti(2048 + a, 67305985, 134678021)
	  for i = 0, 24 do
	    local want = 238
	    if i >= a and i < a + 8 then want = i - a + 1 end
	    if ld8(2048 + i) ~= want then bad = bad + 1 end
	  end
	end
	return "wrong=" .. bad
end)()`, lvl)
	if got != "wrong=0" {
		t.Errorf("-opt=%s: got %q, want %q", lvl, got, "wrong=0")
	}
}

// An out-of-range 8-byte store leaves memory UNTOUCHED.
//
// This is the property the single leading bounds check exists for, and it is
// the one the aligned fast path could quietly break: for an address four bytes
// below the end, writing the low word before discovering the high word does
// not fit leaves a half-written value behind after a trap the spec says
// changed nothing.
func TestAnOutOfRangeEightByteStoreWritesNothing(t *testing.T) {
	for _, lvl := range allLevels {
		outOfRangeEightByteStore(t, lvl)
	}
}

func outOfRangeEightByteStore(t *testing.T, lvl analysis.Level) {
	t.Helper()
	got := runAt(t, f64WAT, `(function()
	local sti, st8, ld8 = M.exports["sti"], M.exports["st8"], M.exports["ld8"]
	local size = 4 * 65536
	local at = size - 4          -- the low word fits; the high word does not
	for i = 0, 3 do st8(at + i, 238) end
	local ok = pcall(function() sti(at, 67305985, 134678021) end)
	local seen = {}
	for i = 0, 3 do seen[#seen+1] = ld8(at + i) end
	return tostring(ok) .. "|" .. table.concat(seen, ",")
end)()`, lvl)
	if got != "false|238,238,238,238" {
		t.Errorf("-opt=%s: got %q, want %q (trapped, and not one byte written)",
			lvl, got, "false|238,238,238,238")
	}
}

// ---------------------------------------------------------------------------
// The INLINED 8-byte access (-opt=3).
//
// ld_f64 and st_f64 were still function CALLS after the pair-access fix, and
// the load-cost breakdown puts the call alone at 34% of an access. -opt=3 now
// expands the aligned fast path at the use site, the same trade the i32 load
// already makes: the access stops being an expression, so forwarding can no
// longer fold it into a larger one.
//
// Everything below either pins the expansion in place or checks the arithmetic
// it duplicates out of the runtime helper.
// ---------------------------------------------------------------------------

// inlineMarkers names, per export, the text only the inlined form emits and the
// fallback call that must survive beside it.
var inlineMarkers = []struct {
	export, inlined, fallback string
}{
	// S1 is shard 0. An 8-byte access reaches it only under the merged test,
	// which proves BOTH words inside the first shard -- so the inlined form is
	// the one shape that provably cannot straddle a shard boundary, and the
	// fallback below carries the straddle along with the unaligned case.
	{"ldf", "+ 4503599627370496.0) * PE[t3]", "ld_f64(MEM, MEMSIZE, t0)"},
	{"stf", "t2, t3 = f64_to_bits(t1)", "st_f64(MEM, MEMSIZE, t0, t1)"},
	{"ldi", "= S1[t1], S1[t1 + 1]", "ld32(MEM, MEMSIZE, t0)"},
	{"sti", "S1[t1 + 1] = ", "st64(MEM, MEMSIZE, t0,"},
}

// The expansion fires at -opt=3 and at no level below it.
//
// Without this the pass could silently stop firing: the emitted Lua would still
// be correct, every other gate would still be green, and the only trace would
// be a benchmark nobody re-ran.
func TestTheInlinedEightByteAccessFiresAtOpt3AndNotBelow(t *testing.T) {
	for _, lvl := range allLevels {
		src := emitBody(t, f64WAT, lvl)
		want := lvl >= analysis.O3
		for _, m := range inlineMarkers {
			body := functionBody(src, m.export)
			if got := strings.Contains(body, m.inlined); got != want {
				t.Errorf("-opt=%s %s: inlined=%v, want %v:\n%s",
					lvl, m.export, got, want, body)
			}
			// However it lowers, the ragged path has to stay reachable: LLVM
			// aligns what it can, but "almost never" is not "never", and the
			// unaligned case is the one the fast path cannot serve.
			if want && !strings.Contains(body, m.fallback) {
				t.Errorf("-opt=%s %s inlined but left no unaligned fallback:\n%s",
					lvl, m.export, body)
			}
		}
	}
}

// Every scratch the inlined 8-byte access reaches for is DECLARED.
//
// scratch_test.go makes this demand of t0 and t1, which is all the emitter had
// when it was written. The f64 forms need four, and a bare `t2 = ...` is a
// write to a GLOBAL exactly as a bare `t0 = ...` was: it parses, it runs, it
// computes the right answer, and the whole spec suite stays green while every
// scratch access becomes an _ENV lookup.
func TestTheInlinedEightByteAccessDeclaresEveryScratchItUses(t *testing.T) {
	body := regexp.MustCompile(`(?s)F\[\d+\] = function\([^)]*\)(.*?)\nend\n`)
	for _, lvl := range allLevels {
		src := emitAt(t, f64WAT, lvl)
		for _, m := range body.FindAllStringSubmatch(src, -1) {
			fn := m[1]
			decl := ""
			if i := strings.Index(fn, "local t0"); i >= 0 {
				decl, _, _ = strings.Cut(fn[i:], "\n")
			}
			for _, name := range []string{"t0", "t1", "t2", "t3"} {
				used := regexp.MustCompile(`\b` + name + `\b`).MatchString(fn)
				if used && !strings.Contains(decl, name) {
					t.Errorf("-opt=%s: %s is used but not declared (%q), so it "+
						"is a GLOBAL:\n%s", lvl, name, decl, fn)
				}
			}
		}
	}
}

// The inlined load handles NORMAL doubles only and hands everything else back
// to ld_f64. This walks the seam.
//
// Getting the exponent test off by one either way is invisible to a round trip
// through most numbers, and the boundaries are where every IEEE-754 reassembly
// bug lives: the largest subnormal and the smallest normal differ by one ulp
// and sit on opposite sides of the branch.
const boundaryWAT = `(module
	(memory (export "memory") 1)
	(func (export "ldf") (param i32) (result f64) (f64.load (local.get 0)))
	(func (export "st32") (param i32 i32) (i32.store (local.get 0) (local.get 1))))`

// Written as BITS and stored word by word, so the test names the pattern it
// means rather than a decimal literal that has to survive Lua's parser first.
const boundaryScript = `(function()
	local st32, ldf = M.exports["st32"], M.exports["ldf"]
	local cases = {
	  {0, 0},                        -- +0
	  {0, 2147483648},               -- -0
	  {1, 0},                        -- smallest subnormal
	  {4294967295, 1048575},         -- largest subnormal, e = 0
	  {0, 1048576},                  -- smallest normal, e = 1
	  {1, 1048576},                  -- smallest normal + 1 ulp
	  {4294967295, 2146435071},      -- largest normal, e = 2046
	  {0, 2146435072},               -- +inf, e = 2047
	  {0, 4293918720},               -- -inf
	  {0, 1072693248},               -- 1.0
	  {0, 3220176896},               -- -1.0
	  {1413754136, 1074340347},      -- pi, 0x400921FB54442D18
	}
	local want = {0.0, -0.0, 4.9406564584124654e-324, 2.2250738585072009e-308,
	              2.2250738585072014e-308, 2.225073858507202e-308,
	              1.7976931348623157e308, 1/0, -1/0, 1.0, -1.0,
	              3.141592653589793}
	local bad = {}
	for k, c in ipairs(cases) do
	  st32(4096, c[1]) st32(4100, c[2])
	  local r = ldf(4096)
	  local w = want[k]
	  -- BITS, not values: -0.0 == 0.0 in Lua, so a sign bit dropped by the
	  -- inlined path would compare equal and pass.
	  if r ~= w or (w == 0.0 and 1/r ~= 1/w) then
	    bad[#bad+1] = k .. ":" .. tostring(r)
	  end
	end
	-- NaN separately. It is never equal to itself, so the only assertion
	-- available is that it stayed one -- and in exact mode a NaN whose bits
	-- matter comes back as a BOXED table, which the inlined fast path must not
	-- intercept (it has no box to return).
	st32(4096, 0) st32(4100, 2146959360)
	local n = ldf(4096)
	if not (type(n) == "table" or n ~= n) then bad[#bad+1] = "nan:" .. tostring(n) end
	if #bad == 0 then return "ok" end
	return table.concat(bad, " ")
end)()`

func TestTheInlinedLoadAgreesWithTheHelperAtEveryExponentBoundary(t *testing.T) {
	for _, lvl := range allLevels {
		if got := runAt(t, boundaryWAT, boundaryScript, lvl); got != "ok" {
			t.Errorf("-opt=%s: %s", lvl, got)
		}
		// Exact NaN mode routes the fallback through xld_f64, which returns a
		// BOXED table for a NaN whose bits matter. The inlined fast path must
		// not intercept one -- it has no box to return.
		if got := runAtMode(t, boundaryWAT, boundaryScript, lvl, NaNExact); got != "ok" {
			t.Errorf("-opt=%s --nan=exact: %s", lvl, got)
		}
	}
}

// A trapping VALUE operand traps before the store's own bounds check does.
//
// wasm evaluates address, then value, then performs the access. The inlined
// store hoists the address into a scratch and bounds-checks it, so evaluating
// the value after that check would swap the two traps -- and they carry
// different codes, which a host and the conformance suite both compare. The
// value therefore lands in a scratch BEFORE the check.
func TestTheInlinedStoreEvaluatesItsValueBeforeTheBoundsCheck(t *testing.T) {
	// The address is out of range AND the value divides by zero. wasm says
	// div0, because the value is evaluated first.
	sameAtEveryLevel(t, `(module (memory 1)
		(func (export "f") (param i32)
			(f64.store (i32.const 1000000)
				(f64.convert_i32_u (i32.div_u (i32.const 1) (local.get 0))))))`,
		`(function()
	local ok, e = pcall(function() M.exports["f"](0) end)
	return tostring(ok) .. "|" .. tostring(type(e) == "table" and e.fk_trap or e)
end)()`, "false|integer divide by zero")
}

// --persist=packed keeps the store OUT of line, because the dirty-range
// page marking lives inside st64.
//
// This is the hazard the gate exists for and it is not hypothetical: a store
// that writes MEM directly lands in the live table, the flush never learns the
// page changed, and the bytes go silently missing from the save -- one
// save/load cycle away from the code that caused them.
func TestPackedModeKeepsTheEightByteStoreOutOfLine(t *testing.T) {
	for _, lvl := range allLevels {
		src := emitWith(t, f64WAT, lvl, PersistPacked)
		for _, name := range []string{"stf", "sti"} {
			body := functionBody(src, name)
			if strings.Contains(body, "MEM[t1 + 1] = ") {
				t.Errorf("-opt=%s packed: %s inlines its store and so bypasses "+
					"the dirty-page mark in st64:\n%s", lvl, name, body)
			}
		}
		// The LOAD is unaffected -- nothing records a read -- so the level-3
		// expansion must still be there.
		if lvl >= analysis.O3 {
			if !strings.Contains(functionBody(src, "ldf"), "+ 4503599627370496.0) * PE[t3]") {
				t.Errorf("-opt=%s packed: the f64 load should still inline", lvl)
			}
		}
	}
}

// And the same thing behaviourally: an 8-byte store made in packed mode
// survives the save it is supposed to.
//
// A structural assertion can only say the emitter did not write the inline
// form. This says the marking actually saw the write, which is the property
// the structure exists to protect -- and it is what fails if the persist gate
// on inlineWideStores is removed.
func TestAnEightByteStoreInPackedModeReachesTheSave(t *testing.T) {
	const script = `
local storage = {}
local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["stf"](1024, 3.141592653589793)
a.exports["sti"](2048, 67305985, 134678021)

a.persist.flush(storage.fk_pages)

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("f " .. tostring(b.exports["ldf"](1024) == 3.141592653589793))
local lo, hi = b.exports["ldi"](2048)
print("i " .. tostring(lo == 67305985 and hi == 134678021))
`
	want := "f true\ni true"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, f64WAT, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q -- an 8-byte store did not reach "+
				"the packed save, so it bypassed the page mark in st64", lvl, got, want)
		}
	}
}
