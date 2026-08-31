package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// HOW LONG EVERY TIMED LEG RUNS, in milliseconds -- not how many iterations it
// runs for. This is the whole of the de-flaking and it is agents/benchmarks.md's
// rule stated as code: "make the floor run for the same WALL TIME, not the same
// iteration count."
//
// The old constant was 2,000 iterations for every leg, and iterations are the
// wrong unit here because the legs differ by three orders of magnitude. At 2,000
// reps the cheapest leg -- a dispatch that makes no host call, 464 ns -- is
// 0.9 ms of signal differenced out of a ~10 ms process, while the most expensive
// packed leg is 300 ms. So the cheap legs were almost entirely process startup
// and the expensive ones almost entirely not, which means the two were never
// comparable and a machine that got slower for 10 ms could invert them.
//
// Sizing each leg by TIME instead gives every measurement the same signal, the
// same exposure to whatever else is running, and the same fraction of startup
// differenced away. The floor below is sized the same way and is therefore a
// denominator that actually feels the load -- which is the specific thing the
// first attempt at the abicost fix got wrong (agents/gc.md, "The flaky gate").
const callCostTargetMS = 100.0

// The pilot that sizes a leg. Small enough to be cheap at 300 µs a call, large
// enough not to be pure startup at 400 ns a call. Its own accuracy does not
// matter: it picks a rep count, and a rep count that is 2x off still leaves the
// leg running for tens of milliseconds.
const callCostPilotReps = 60

// Bounds on what the sizing may choose. The floor stops a mis-timed pilot from
// producing a leg with no signal at all.
//
// THE CEILING HAS TO CLEAR THE CHEAPEST LEG AT THE TARGET, and getting that
// wrong is how the first version of this fix was useless -- exactly as it was
// for the first instance of this bug, which agents/gc.md records in the same
// words. The floor body is ~19 ns, so 100 ms of it is 5.3M iterations; a
// 400,000 ceiling ran it for 7.6 ms instead, and a 7.6 ms leg taken as
// best-of-three finds a quiet slice on a machine where the 100 ms legs are
// fighting for a core. Measured under 22 spinning threads: it reported a floor
// A/A of "15 and -5 ns, spread 412%" -- a NEGATIVE floor -- while the legs it
// was supposed to qualify were dilating by 2-3x.
//
// 20M is ~380 ms of the floor body, which is headroom rather than a target: the
// sizing loop below converges on wall time and this is only what stops a
// pathological estimate running forever.
const callCostMinReps = 200
const callCostMaxReps = 20000000

// THE FLOOR'S BODY: the same stub environment, the same chunk, the same
// interpreter, and NO DISPATCH -- a plain Lua function call in the loop where
// every other leg calls the mod's event handler. It touches nothing this
// project generated, so what it prices is lua52f on this machine right now.
//
// It is what says whether a run had the resolution to make an ordering claim.
// Without it, "the string return measured cheaper than a dispatch" is
// indistinguishable from "the machine got slower between two runs", and this
// test asserted the first of those for a year.
const callCostFloorTick = -1

// WHAT A HOST CALL COSTS THROUGH A REAL GUEST, and where the time goes.
//
// internal/factorio's TestAHostCallIsUnderM0sTwoMicrosecondGate measures the
// HOST half -- decode, dispatch, encode -- with hand-written Lua standing in for
// the guest, and reports ~500 ns for a no-op. The first downstream mod measured
// ~12 µs per host call end to end, against ~1.5-3 µs for a plain Lua-to-C++ API
// call, and nothing here could say where the difference went.
//
// Two things the host-side number cannot see, and this one can:
//
//   - the GUEST's own encode, which is compiled wasm running as interpreted
//     Lua -- writeDyn, the block setup, the field stores;
//   - fk_mod.lua's dispatch wrapper (pcall, depth, transient release, globals
//     sync, packed flush), which is paid once per EVENT, not per call.
//
// So the baseline probe is a dispatch that makes no host call at all, and every
// other number is reported against it. This is a PROFILE, not a gate: it prints,
// and it asserts only things that survive a busy machine.
//
// # A WALL-CLOCK PROFILE NEEDS A FLOOR, and this one is the SECOND instance
//
// agents/gc.md's "The flaky gate" records the first: a bare 2000 ns threshold in
// internal/factorio that failed at 2005-2156 ns whenever tinygo builds ran
// beside it, on clean master, with nothing about the ABI changed. This test is
// the same failure in a different costume, and it was found the same way -- by
// failing mid-session and then passing on a re-run, which is the shape that
// trains people to re-run.
//
// It had no wall-clock threshold, so what fired was the ORDERING check: "a step
// that does strictly more cannot cost less". That sentence is true about work
// and false about MEASUREMENTS of work, and the difference is the resolution of
// the run. Two of the legs here genuinely cost the same:
//
//	--persist=packed  dispatch, no host call   1195 ns
//	--persist=packed  call, no blocks          1113 ns
//
// ReloadScript takes no argument block and returns no return block, so it
// dirties no page, so packed's flush -- which is the whole cost of a packed
// dispatch -- has nothing extra to do. The two are the same work to within a few
// percent, and demanding a strict ordering between them is demanding resolution
// the harness never had. It got it by luck most of the time.
//
// So this now follows agents/benchmarks.md's discipline, the same one applied to
// the first instance:
//
//	floor    a plain Lua call in the loop where the other legs dispatch, in the
//	         same chunk, under the same interpreter. No ABI, no guest, no
//	         emitter -- it prices THIS MACHINE AT THIS MOMENT.
//	A/A      the floor twice, BRACKETING the measurements rather than sitting
//	         beside them, so what it certifies is the whole window and not an
//	         instant. Its spread is this run's resolution, and it is printed
//	         whether or not anything fails.
//	sizing   every leg AND the floor run for the same WALL TIME (see
//	         callCostTargetMS), which is what makes the floor a denominator that
//	         feels the load rather than a decoration.
//	ratios   the assertions are ratios between legs measured in the same window,
//	         because both terms dilate together and the answer is the same on a
//	         busy machine as on an idle one.
//
// A NOISY RUN IS REPORTED LOUDLY AND STILL ASSERTS. Skipping on noise is the
// silent-SKIP shape this repo spent stage D removing, in a costume of its own.
func TestWhatAHostCallCostsThroughARealGuest(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	root, tmp := repoRoot(t), t.TempDir()
	wasmPath := filepath.Join(tmp, "callcost.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/callcost", wasmPath); err != nil {
		t.Fatalf("building the Go guest: %v", err)
	}

	probes := []struct {
		name string
		tick int
	}{
		{"dispatch, no host call", 0},
		{"call, no blocks", 1},
		{"scalar in, scalar out", 2},
		{"string return (44 B)", 3},
		{"tier-2 map argument", 4},
		{"name read + compare", 5},
		{"NameIs (host-side)", 6},
		{"NameIs (no match)", 7},
		{"array return (4)", 8},
		{"array into (4)", 9},
		// THE BULK READ, at two sizes. Both are ONE host call, so the number to
		// compare against "scalar in, scalar out" is this divided by the count --
		// which is what the log line below spells out rather than leaving to a
		// reader with a calculator.
		{"bulk read of 4 (one call)", 10},
		{"bulk read of 256 (one call)", 11},
	}
	// The element counts, so the per-element line is derived from the same table
	// the probes are rather than from a second list that can drift.
	perElement := map[int]int{10: 4, 11: 256}

	for _, persist := range []luagen.PersistMode{luagen.PersistTable, luagen.PersistPacked} {
		dir := packCallCost(t, root, tmp, wasmPath, persist)

		// The floor first and last, with every measurement between them, so the
		// A/A pair brackets what it is being used to qualify. Taken back to
		// back it would say the machine was steady for 100 ms; taken this way
		// it says the machine was steady for the whole window.
		floorA := callCostTime(t, h, dir, callCostFloorTick)
		ns := make([]float64, len(probes))
		for i, p := range probes {
			ns[i] = callCostTime(t, h, dir, p.tick)
		}
		floorB := callCostTime(t, h, dir, callCostFloorTick)

		floor := (floorA + floorB) / 2
		aa := 0.0
		if floor > 0 {
			if aa = (floorB - floorA) / floor; aa < 0 {
				aa = -aa
			}
		}
		base := ns[0]

		t.Logf("--persist=%s", persist)
		t.Logf("  %-24s %8.0f ns   (A/A %.0f and %.0f ns, spread %.1f%% -- this "+
			"run's resolution)", "plain Lua call (floor)", floor, floorA, floorB, aa*100)
		for i, p := range probes {
			if i == 0 {
				t.Logf("  %-24s %8.0f ns   %5.2fx the floor; the baseline every "+
					"other line is above", p.name, ns[i], ns[i]/floor)
				continue
			}
			if n := perElement[p.tick]; n > 0 {
				// PER ELEMENT, which is the only number comparable with the
				// per-call probes above. A bulk read is one crossing however
				// many handles it carries, so its raw nanoseconds say nothing
				// about whether an author should use it.
				t.Logf("  %-24s %8.0f ns   (+%.0f over the dispatch; %.0f ns per "+
					"element of %d, against %.0f for one scalar read)",
					p.name, ns[i], ns[i]-base, (ns[i]-base)/float64(n), n,
					ns[2]-base)
				continue
			}
			t.Logf("  %-24s %8.0f ns   (+%.0f over the dispatch, %.2fx it)",
				p.name, ns[i], ns[i]-base, ns[i]/base)
		}

		// THE FLOOR HAS TO BE BELOW THE THING IT IS A FLOOR FOR. A plain Lua
		// call cannot cost more than the same loop going through control.lua's
		// dispatch wrapper into compiled guest code -- so if it does, the floor
		// is measuring something other than what it claims and nothing scaled by
		// it means anything. This is a claim about the HARNESS and it fires
		// regardless of how noisy the machine is.
		if floor >= base {
			t.Errorf("--persist=%s: the floor (%.0f ns) is not below a dispatch "+
				"that does nothing (%.0f ns); a plain Lua call cannot cost more "+
				"than the same loop reached through fk_mod.lua's dispatch, so the "+
				"floor is not measuring a plain Lua call", persist, floor, base)
		}

		// This run's own resolution, with a floor under it. An A/A of 2% does not
		// license a 2% ordering claim: the floor is one body measured twice and
		// the legs are five different ones, so the tolerance is the A/A spread
		// with a 10% minimum -- which is roughly what a lua52f process's own
		// startup variance contributes even on a quiet machine.
		tol := aa
		if tol < 0.10 {
			tol = 0.10
		}
		const aaCeiling = 0.35
		if aa > aaCeiling {
			t.Logf("  NOISY RUN: the A/A spread is %.1f%%, over %.0f%%. The ratio "+
				"assertions below are load-invariant and all of them still run -- "+
				"but do not quote the nanosecond figures above from this run.",
				aa*100, aaCeiling*100)
		}

		for i, p := range probes {
			if i == 0 {
				continue
			}
			// ORDERING, WITHIN THIS RUN'S RESOLUTION. The claim worth making is
			// that a step doing strictly more did not come back MEASURABLY
			// cheaper; "measurably" is what the A/A is for. The old form of this
			// check omitted that word and fired on packed's dispatch-versus-
			// no-blocks pair, whose two members do the same work.
			if ns[i] < base*(1-tol) {
				t.Errorf("--persist=%s: %s measured %.0f ns against a %.0f ns "+
					"dispatch that does nothing -- %.1f%% cheaper, past this "+
					"run's %.1f%% resolution (A/A %.1f%%). A step that does more "+
					"cannot cost less, so the harness is not measuring what it "+
					"says", persist, p.name, ns[i], base,
					(1-ns[i]/base)*100, tol*100, aa*100)
			}
		}

		// THE ONE REAL GATE, AND IT IS A RATIO. The tier-2 map argument runs
		// writeDyn on the guest side and read_dyn on the host side over a
		// two-entry map of strings; a dispatch that makes no host call runs
		// neither. That is a large and load-invariant difference -- measured
		// 23.9x under --persist=table and 124x under packed -- so a 3x bar
		// cannot fire on a busy machine and does fire if tier 2 ever collapses
		// into dispatch, which would mean the profile had stopped distinguishing
		// the two things it exists to distinguish.
		// BY NAME AND NOT BY POSITION. This read `ns[len(ns)-1]` while tier 2
		// happened to be the last probe; appending the name and container legs
		// would have silently re-pointed the repo's one real ABI gate at a
		// different measurement and left it green.
		tier2Idx := -1
		for i, p := range probes {
			if p.name == "tier-2 map argument" {
				tier2Idx = i
			}
		}
		if tier2Idx < 0 {
			t.Fatal("the tier-2 probe is gone, so the gate below measures nothing")
		}
		const tier2Bar = 3.0
		if tier2 := ns[tier2Idx]; tier2 < base*tier2Bar {
			t.Errorf("--persist=%s: a tier-2 map argument costs %.0f ns against a "+
				"%.0f ns bare dispatch (%.2fx, under the %.1fx bar). Either the "+
				"marshalling path stopped running or the baseline stopped being a "+
				"baseline; both mean this profile no longer separates dispatch "+
				"from marshalling", persist, tier2, base, tier2/base, tier2Bar)
		}
	}
}

// packCallCost compiles and packages the guest at one persistence mode, and
// returns the mod directory.
func packCallCost(t *testing.T, root, tmp, wasmPath string, persist luagen.PersistMode) string {
	t.Helper()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	// AT THE DEFAULT OPTIMIZATION LEVEL, which is what `fklua mod` packages at.
	// luagen.Options{} means -opt=0, and a profile of a level nobody ships is a
	// profile of nothing.
	src, err := luagen.EmitModuleWith(im, luagen.Options{
		Persist: persist, Opt: analysis.DefaultLevel})
	if err != nil {
		t.Fatal(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)
	used, complete := factorio.UsedMembers(im)
	if !complete {
		t.Fatal("a member id was not a compile-time constant, so the id scan broke")
	}
	table, err := report.Only(used).LuaSourceWith(a, events)
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-callcost", Version: "0.1.0", Title: "FkLua call cost",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, APITable: table,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	dir, err := pkg.WriteDir(filepath.Join(tmp, persist.String()))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// callCostTime measures one leg, sized so that it runs for callCostTargetMS of
// wall time rather than for a fixed number of iterations.
//
// The measurement itself is unchanged and is the part that was always right: the
// same driver is run at N repetitions and at zero and the two are differenced,
// which subtracts process startup and the chunk parse EXACTLY rather than
// estimating them. lua52f has no clock -- Factorio removes the `os` library and
// the oracle is patched to match, which is the oracle being right rather than a
// limitation, since a guest cannot read a clock either -- so the timing comes
// from outside the process.
//
// WHAT IS NEW IS THE SIZING, and it is the whole de-flaking. A pilot run
// estimates the per-iteration cost, and the real run asks for however many
// iterations that estimate says will take callCostTargetMS. The legs here span
// 400 ns to 300 µs, so a shared iteration count gives the cheap ones almost no
// signal and the expensive ones plenty -- and then compares them. Sizing by time
// gives every leg the same signal, the same share of startup differenced away,
// and the same exposure to whatever else is running on the machine.
func callCostTime(t *testing.T, h *luahost.Host, modDir string, tick int) float64 {
	t.Helper()
	run := func(reps int) time.Duration {
		script := callCostDriver(modDir, tick, reps)
		best := time.Duration(1<<62 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			out, err := h.RunString(script)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if strings.TrimSpace(out) != "done" {
				t.Fatalf("the driver did not complete: %q", strings.TrimSpace(out))
			}
			if el := time.Since(start); el < best {
				best = el
			}
		}
		return best
	}

	// THE SIZING CONVERGES ON MEASURED WALL TIME rather than trusting one
	// estimate, because an estimate is exactly what cannot be trusted here: the
	// pilot is short, and a short run on a loaded machine is the measurement
	// most likely to be wrong in the direction that matters. A single
	// pilot-then-scale underestimated the floor by 700x under load and produced
	// a NEGATIVE per-iteration cost.
	//
	// Each pass measures how long the leg actually took and rescales toward the
	// target, so a bad estimate costs one extra pass instead of poisoning the
	// result. Growth is capped per pass so an underestimate cannot leap to
	// something that runs for minutes, and the loop exits as soon as the leg is
	// within half the target -- which is close enough, since what the target buys
	// is exposure to load and not precision.
	zero := run(0)
	reps := callCostPilotReps
	elapsed := run(reps) - zero
	target := time.Duration(callCostTargetMS * float64(time.Millisecond))
	for pass := 0; pass < 3 && elapsed < target/2 && reps < callCostMaxReps; pass++ {
		scale := 20.0
		if elapsed > 0 {
			if s := float64(target) / float64(elapsed); s < scale {
				scale = s
			}
		}
		next := int(float64(reps) * scale)
		if next <= reps {
			next = reps * 2
		}
		if next > callCostMaxReps {
			next = callCostMaxReps
		}
		reps = next
		elapsed = run(reps) - zero
	}
	if reps < callCostMinReps {
		reps = callCostMinReps
		elapsed = run(reps) - zero
	}
	return float64(elapsed) / float64(reps)
}

// callCostDriver builds the timing script for one leg.
//
// tick selects which probe the guest runs; callCostFloorTick selects no probe at
// all and loops over a plain Lua call instead. THE FLOOR SHARES EVERYTHING ELSE
// -- the same package.path, the same stub `script`/`game`/`storage`, the same
// require("control"), the same on_init -- so the only difference between the
// floor and a measurement is the body of the loop. Anything the two had
// separately would be a difference the differencing could not remove.
func callCostDriver(modDir string, tick, reps int) string {
	body := fmt.Sprintf("local fire, ev = handlers[1], { tick = %d }\n"+
		"for _ = 1, %d do fire(ev) end", tick, reps)
	if tick == callCostFloorTick {
		// A plain Lua call with an argument and a return, so it is a call and
		// not a table read -- the same shape internal/factorio's floor uses,
		// for the same reason.
		body = fmt.Sprintf("local function noop(n) return n + 1 end\n"+
			"local acc = 0\nfor _ = 1, %d do acc = noop(acc) end\n"+
			"if acc < 0 then print(acc) end", reps)
	}
	return fmt.Sprintf(`package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-callcost",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local thing
thing = {
  valid = true,
  name = "transport-belt-of-a-perfectly-ordinary-length",
  health = 100.0,
  reload_script = function() end,
}
thing.create_entity = function(_) return thing end
-- FOUR entities, so the two container probes both walk a non-empty result and
-- the only difference between them is where the slice came from.
thing.find_entities_filtered = function(_) return { thing, thing, thing, thing } end
game = setmetatable({ valid = true, reload_script = function() end },
                    { __index = function(_, k) return thing[k] end })
require("control")
handlers.on_init()
%s
print("done")
`, filepath.Join(modDir, "?.lua"), body)
}
