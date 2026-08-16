package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
	luart "github.com/Techrocket9/fklua/runtime"
)

// What a host call COSTS, against the threshold M0 wrote down for it.
//
// M0 set an explicit red flag while sizing the project: "no-op host round trip
// > 2 us -> the ABI is too heavy". The ABI was then built across M7 -- one
// generic `fk.call(handle, member, argp, retp)`, a handle table, a dispatcher
// and three marshalling tiers -- and the threshold was never checked against
// the thing it was written for. A gate nobody runs is not a gate.
//
// This runs it. Every number below is measured by THIS test on THIS machine, in
// the same run that asserts on it, so a regression moves the number rather than
// leaving a stale one in a comment.
//
// The shape matters as much as the total. A call crosses the boundary in four
// distinguishable steps, and they are separated here because "the ABI is too
// heavy" and "marshalling a string is expensive" call for completely different
// fixes:
//
//	dispatch only  -- resolve the handle, find the member, call nothing
//	nullary call   -- the above, plus a real Lua method invocation
//	i32 in, i32 out -- the above, plus reading and writing guest memory
//	string return  -- the above, plus an allocation and fk_wstr
//
// The gate asserts on the nullary call, since that is what "no-op round trip"
// means: everything the ABI does, and nothing the API does.
//
// A WALL-CLOCK GATE NEEDS A FLOOR, and stage D gave this one two after it
// turned out to be FLAKY: it failed at 2005-2156 ns against its 2000 ns
// threshold whenever tinygo builds ran beside it, on clean master, with
// nothing about the ABI changed. And the assertion that fired was never M0's.
// `nop` is ~537 ns here and has 3.7x of headroom; the measurement sitting 5%
// under the wall was the STRING RETURN, which this file already admitted "is
// not covered by M0's wording". A gate that fails for a reason its own comment
// disclaims is worse than no gate: it trains people to re-run.
//
// So this now follows the discipline agents/benchmarks.md and agents/gc.md
// apply to every number in the repo -- nothing is asserted against a bare
// wall-clock figure without something measured IN THE SAME RUN to say what the
// machine was doing:
//
//	floor   a plain Lua method call on the same object, timed by the same
//	        instrument. It does no ABI work at all, so it prices THIS MACHINE
//	        AT THIS MOMENT, and it is what M0's threshold is scaled by.
//	A/A     the floor, measured a second time. Two measurements of ONE body
//	        bound this run's own resolution, and the test says so out loud
//	        rather than pretending a 4% difference is a result.
//
// M0's 2000 ns survives unchanged on a quiet machine and is scaled by the
// measured dilation on a loaded one, so an ABI regression still fires it and a
// parallel build does not. The string return moves to a RATIO against the
// no-op measured beside it, which is both load-invariant and the thing that
// check actually wants to say -- the same shape the `inc` assertion below it
// has used all along.

// Enough iterations that the loop dominates what surrounds it. At ~500 ns a
// call this is ~200 ms of work against a ~10 ms process, which is the ratio the
// first attempt at this test got backwards: at 20,000 reps the difference was
// 11 ms buried in process startup, and it reported a string return as CHEAPER
// than a no-op -- an ordering that cannot happen and which is how the bad
// measurement announced itself.
const abiCostReps = 400000

// The floor's body: a plain Lua method call on the same object the ABI
// measurements dispatch to, with an argument and a return so it is a call and
// not a table read. It touches nothing this project wrote -- no handle table,
// no member table, no marshalling, no guest memory -- so what it prices is
// lua52f on this machine under whatever else is running.
//
// `obj` and the timed bodies live in one chunk, so this reaches the same object
// through the same upvalue the ABI path resolves a handle to.
const abiFloorBody = `local _ = obj.inc(7)`

// The floor runs MORE iterations than the ABI bodies, so that it takes about as
// long in wall time and is exposed to the same contention.
//
// This is not a detail, it is the difference between a floor and a decoration.
// At 33 ns the floor finishes 400,000 iterations in 13 ms; the no-op ABI call
// spends 211 ms on the same count. Under 22 spinning cores, measured: the ABI
// bodies dilated 2.5x and the floor -- short enough to land inside a quiet
// slice, and taken as best-of-three, which finds that slice -- reported 1.13x.
// A denominator that does not feel the load cannot scale anything by it.
//
// 6.4M is 16x, which is the ratio of the two costs on a quiet machine, so both
// legs run for roughly 200 ms.
const abiFloorReps = abiCostReps * 16

// What abiFloorBody costs on a quiet machine: 33 ns, measured by this test on
// an M3 Pro under lua52f. It is the denominator M0's 2000 ns threshold is
// scaled by, and it is the ONE constant in this file that is a property of the
// MACHINE rather than of the ABI.
//
// What the scaling does and does not concede. A machine running at this speed
// gets M0's threshold verbatim, and a 4x ABI regression fires it -- 528 ns to
// 2112 ns, over 2000. A machine three times slower, whether from load or from
// being an older machine, gets 6000 ns instead. That is deliberate: M0's number
// was a claim about the ABI's weight, and on a slower interpreter everything
// including the interpreter is slower. What the ratio gates below hold onto
// across all of that is the shape -- 16x the floor for dispatch, 3.4x dispatch
// for a string -- which is the part a regression would move and a busy machine
// would not.
//
// Re-measure it (the test logs the floor every run) only if the floor moves for
// a reason -- a new lua52f, a different interpreter, a different machine as the
// reference -- and never to make a failing run pass, which is the first thing
// it will be reached for.
const abiFloorReference = 33.0

// abiTime runs a snippet abiCostReps times under lua52f and reports ns per
// iteration.
//
// The timing is taken from OUTSIDE the process, because there is nothing inside
// it to take one with: lua52f is patched to Factorio's sandbox and Factorio
// removes the `os` library entirely, so `os.clock` is nil. That is the oracle
// being right rather than a limitation -- a guest cannot read a clock either.
//
// The harness is therefore built ONCE and only the lua52f run is timed, since
// compiling the wasm and writing the temp files is Go-side work with more
// variance than the thing being measured. What remains is differenced against
// the same script at zero iterations, which subtracts process startup and the
// chunk parse exactly.
func abiTime(t *testing.T, h *abiHarness, body string) float64 {
	return abiTimeN(t, h, body, abiCostReps)
}

// abiTimeN is abiTime with the iteration count named, so the floor can run for
// the same WALL TIME as the measurements it qualifies rather than the same
// number of iterations. See abiFloorReps.
func abiTimeN(t *testing.T, h *abiHarness, body string, reps int) float64 {
	t.Helper()
	run := func(reps int) time.Duration {
		script := "\nlocal work = function() " + body + " end\n" +
			"for _ = 1, " + strconv.Itoa(reps) + " do work() end\nprint(\"done\")"
		best := time.Duration(1<<62 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			if got := h.run(t, script); got != "done" {
				t.Fatalf("the benchmark script did not complete: %q", got)
			}
			if el := time.Since(start); el < best {
				best = el
			}
		}
		return best
	}
	return float64(run(reps)-run(0)) / float64(reps)
}

// abiHarness is runMarshal's setup, done once and reused.
type abiHarness struct {
	host   *luahost.Host
	prefix string
}

func newABIHarness(t *testing.T, setup string) *abiHarness {
	t.Helper()
	host, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(memWAT)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fk_abi.lua"), []byte(luart.ABI()), 0o644); err != nil {
		t.Fatal(err)
	}
	return &abiHarness{host: host, prefix: "package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		"local H = require(\"fk_abi\")\n" +
		"local M = (function(...)\n" + chunk + "\nend)({})\n" +
		"local IO = M.memio\n" +
		"H.bind_memory(IO)\n" +
		"H.bind_read_string(M.read_string)\n" +
		"H.bind_globals({})\n" + setup}
}

func (h *abiHarness) run(t *testing.T, script string) string {
	t.Helper()
	out, err := h.host.RunString(h.prefix + script)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.TrimSpace(out)
}

func TestAHostCallIsUnderM0sTwoMicrosecondGate(t *testing.T) {
	nullary, err := Layout(nil)
	if err != nil {
		t.Fatal(err)
	}
	one, err := Layout([]Kind{KindI32})
	if err != nil {
		t.Fatal(err)
	}
	str, err := Layout([]Kind{KindString})
	if err != nil {
		t.Fatal(err)
	}

	// One bound object with three members, so every measurement below differs
	// only in the member it names.
	//
	// The allocator is not decoration. Without one bound, a string return is
	// REFUSED rather than performed -- and the first version of this test left
	// it out, so the string case timed a failed encode and came back cheaper
	// than a no-op. Hence the status check below, which is the real fix: a
	// benchmark that does not assert the work happened is measuring an error
	// path at full confidence.
	setup := fmt.Sprintf(`
-- A bump allocator that wraps, since the timed loop allocates 400,000 times
-- into a memory of one 64 KiB page.
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n
                         if next_ > 60000 then next_ = 8192 end
                         return p end,
             function() end)
local obj = {
  valid = true,
  nop = function() end,
  inc = function(n) return n + 1 end,
  name = "an-entity-name-of-unremarkable-length",
}
H.bind_members({
  [1] = { kind = H.CALL, name = "nop",  sig = { args = %s, rets = %s } },
  [2] = { kind = H.CALL, name = "inc",  sig = { args = %s, rets = %s } },
  [3] = { kind = H.GET,  name = "name", sig = { args = %s, rets = %s } },
})
local h = H.transient(obj)
local call = H.call
local argp, retp = 2048, 3072
IO.st32(argp, 7)`,
		nullary.LuaTable(), nullary.LuaTable(),
		one.LuaTable(), one.LuaTable(),
		nullary.LuaTable(), str.LuaTable())

	hh := newABIHarness(t, setup)

	// Every member actually dispatches and succeeds, checked BEFORE anything is
	// timed. Status 0 is success; anything else means the loop below would be
	// measuring how fast this ABI returns an error.
	if got := hh.run(t, `
local ok = {}
for _, id in ipairs({1, 2, 3}) do ok[#ok+1] = id .. "=" .. call(h, id, argp, retp) end
print(table.concat(ok, " "))`); got != "1=0 2=0 3=0" {
		t.Fatalf("not every member succeeds, so the timings below would be "+
			"measuring an error path: %s", got)
	}
	// And the i32 member really computed something, so its cost is a real
	// decode-call-encode rather than a decode that fell over.
	if got := hh.run(t, `call(h, 2, argp, retp) print(IO.ld32(retp))`); got != "8" {
		t.Fatalf("inc(7) returned %s, want 8", got)
	}
	// The floor first and last, with the ABI measurements between them, so the
	// A/A pair brackets everything it is being used to qualify. An A/A taken
	// back to back would say the machine was steady for 200 ms; taken this way
	// it says the machine was steady for the whole measurement.
	floorA := abiTimeN(t, hh, abiFloorBody, abiFloorReps)
	nop := abiTime(t, hh, `call(h, 1, argp, retp)`)
	inc := abiTime(t, hh, `call(h, 2, argp, retp)`)
	name := abiTime(t, hh, `call(h, 3, argp, retp)`)
	floorB := abiTimeN(t, hh, abiFloorBody, abiFloorReps)

	floor := (floorA + floorB) / 2
	aa := 0.0
	if floor > 0 {
		aa = (floorB - floorA) / floor
		if aa < 0 {
			aa = -aa
		}
	}
	// How much slower this machine is right now than the one M0's threshold
	// was last confirmed against. Never below 1: a FASTER machine does not earn
	// a tighter gate than M0 wrote, it just passes with more room.
	dilation := floor / abiFloorReference
	if dilation < 1 {
		dilation = 1
	}

	t.Logf("plain Lua call (floor) %7.0f ns   (A/A %.0f and %.0f ns, spread %.1f%%)",
		floor, floorA, floorB, aa*100)
	t.Logf("no-op call             %7.0f ns   %5.2fx the floor", nop, nop/floor)
	t.Logf("i32 in, i32 out        %7.0f ns   %5.2fx the floor", inc, inc/floor)
	t.Logf("string return (%2d B)   %7.0f ns   %5.2fx the floor, %.2fx the no-op",
		37, name, name/floor, name/nop)
	t.Logf("machine dilation       %7.2fx against the %.0f ns reference floor",
		dilation, abiFloorReference)

	// The floor has to be BELOW the thing it is a floor for. A plain Lua method
	// call cannot cost more than the same call reached through a handle table,
	// a member table and a marshalling tier -- so if it does, the floor is
	// measuring something other than what it claims and nothing scaled by it
	// means anything.
	if floor >= nop {
		t.Fatalf("the floor (%.0f ns) is not below the no-op ABI call (%.0f ns); "+
			"a plain Lua call cannot cost more than the ABI wrapping one, so the "+
			"floor is not measuring a plain Lua call", floor, nop)
	}

	// An A/A wider than this and the run had no resolution to spend. Reported
	// LOUDLY and not skipped: a timing test that quietly declines to assert is
	// the silent-SKIP shape this repo spent stage D removing. The ratio gates
	// below survive a noisy machine, so they still run.
	const aaCeiling = 0.35
	noisy := aa > aaCeiling
	if noisy {
		t.Logf("NOISY RUN: the A/A spread is %.1f%%, over %.0f%%. The wall-clock "+
			"gate below is scaled by the measured dilation and the ratio gates "+
			"are load-invariant, so all of them still run -- but do not quote "+
			"the nanosecond figures above from this run.", aa*100, aaCeiling*100)
	}

	// The gate itself. 2000 ns is M0's number, not one chosen to fit the
	// result -- if this ever fires, the ABI has to get cheaper or the
	// threshold has to be re-argued, and either way somebody has to look.
	//
	// It is scaled by the dilation and by nothing else. On a quiet machine
	// dilation is 1.00 and this is M0's threshold verbatim.
	const gate = 2000.0
	budget := gate * dilation
	if nop > budget {
		t.Errorf("a no-op host round trip costs %.0f ns, over M0's %.0f ns red flag "+
			"(budget %.0f ns after a measured %.2fx machine dilation; the floor "+
			"was %.0f ns against a %.0f ns reference). The ABI is too heavy and "+
			"M0 said so in advance",
			nop, gate, budget, dilation, floor, abiFloorReference)
	}

	// A measurement that came back implausibly cheap is a broken measurement,
	// not a fast ABI. A real call through the handle table, the member table
	// and a Lua method cannot be free.
	if nop < 20 {
		t.Errorf("a host call measured %.1f ns, which is too cheap to be real -- "+
			"the loop was probably optimized away or the member did not dispatch", nop)
	}

	// Marshalling must not dwarf dispatch. If it does, the generic ABI is not
	// the thing to fix and this test would otherwise point at the wrong place.
	if inc > nop*4 {
		t.Errorf("one i32 each way costs %.0f ns against a %.0f ns no-op -- "+
			"marshalling now dominates dispatch, which inverts where to optimize",
			inc, nop)
	}

	// A string return is the most expensive shape a guest routinely asks for,
	// and it is the one that used to sit closest to the wall -- 1906 ns against
	// a 2000 ns threshold, which is how this test came to fail under load
	// without anything about the ABI moving.
	//
	// It is held to a RATIO against the no-op instead, for the reason the check
	// above it already uses one: what a reader needs to know is whether the
	// allocation and fk_wstr have started to dwarf dispatch, and that question
	// has roughly the same answer on a busy machine as on an idle one.
	//
	// Measured: 3.56x quiet, and 4.53x under 22 spinning threads on 11 cores --
	// far past the parallel tinygo build this test has to survive. The ratio
	// does drift with load, because the string path allocates and the no-op does
	// not, so the bar is 7x rather than the 4x its neighbour uses. That still
	// fires on a 2x regression in the string path and cannot fire on a busy
	// machine, which is the trade this whole rework is.
	//
	// If this ever needs to be TIGHTER, the stabler denominator is `inc` rather
	// than `nop` -- name/inc measured 1.64x quiet and 1.59x loaded, because both
	// of them marshal. It is not used here because it answers a different
	// question than the one this check is for.
	//
	// M0's wording never covered this case, and pretending it did is what put a
	// wall-clock number on it in the first place.
	const stringRatio = 7.0
	if name > nop*stringRatio {
		t.Errorf("returning a %d-byte string costs %.0f ns against a %.0f ns no-op "+
			"(%.2fx, over the %.1fx bar); the allocation and fk_wstr now dominate "+
			"dispatch, which is a different problem from a heavy ABI and wants a "+
			"different fix", 37, name, nop, name/nop, stringRatio)
	}

	// The ordering itself is a check, and it is the one that survives any amount
	// of machine load because every term dilates together. Each of these does
	// strictly more than the one before, so an inversion means a measurement is
	// wrong rather than a path being fast -- which is exactly how this test's
	// own missing allocator was caught, with the string case reporting less than
	// the no-op.
	if !(floor < nop && nop < inc && inc < name) {
		t.Errorf("costs are not monotone in the work done (floor %.0f, no-op %.0f, "+
			"i32 %.0f, string %.0f ns); a step that does more cannot cost less, so "+
			"one of these is not measuring what it claims", floor, nop, inc, name)
	}
}
