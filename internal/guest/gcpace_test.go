package guest_test

import (
	"fmt"
	"math"

	"strconv"
	"strings"
	"testing"
)

// The stage-C gates, from agents/gc.md.
//
// Stage B's collector was one indivisible Collect() and its pause was measured
// at 13.9 to 32.8 ms per MiB of heap. Everything here is about what happens
// when that is cut into bounded pieces with the mutator running between them,
// and there are exactly two things that can go wrong: a step can cost more than
// its budget, which is a worst tick in a lockstep game, and a reference created
// between two steps can be missed, which is a live object swept.
//
// The first is measured. The second is proved by a differential against a
// deliberately broken barrier, because a missed reference does not raise -- the
// memory stays addressable, is zeroed, and is handed to somebody else.

// A STORE INTO AN ALREADY-MARKED OBJECT DURING MARKING MUST NOT LOSE THE
// OBJECT. This is the whole of the barrier's obligation and the whole of what
// stage C added.
//
// The shape is the classic one and it is worth stating in the vocabulary,
// because the reason it is a hazard is not obvious from the code. Marking makes
// an object BLACK when it has been marked and its contents scanned. A store
// that puts a WHITE object into a BLACK one creates a reference along which the
// marker will never travel again: the black object is not re-visited, and the
// root re-scan at termination reaches it, finds it already marked, and returns
// without looking inside. The white object is then swept while live.
//
// torture_repoint does exactly that, between two steps of a paced mark: every
// root gets a freshly allocated child, referenced from nowhere else, at a
// moment when some of those roots have certainly been scanned already.
//
// The differential is against the SAME guest paced with the dirty page set
// never drained, which is what a broken barrier looks like from the collector's
// side. If that leg also passed, this test would be asserting nothing.
func TestAStoreIntoAMarkedObjectDuringMarkingIsNotLost(t *testing.T) {
	h := needGuest(t)
	// Small budget and a big structure, so the mark phase is many steps long
	// and the store lands in the middle of one rather than before it starts.
	const nodes, budget = 8000, 256
	const body = `
K['torture_gc_budget'](BUDGET)
K['torture_build'](NODES)
local verify0 = K['torture_verify']()
local started = K['torture_gc_start']()
-- Repoint after EVERY step that leaves the collector still marking, rather than
-- after one chosen step. Which objects are black at any particular step is a
-- fact about the mark order and the budget, and a test that guessed would be
-- testing that guess -- but the store made just before the step that TERMINATES
-- marking lands when the structure is as black as it ever gets, and that is the
-- one the comparison below reads back.
local ph, steps, want = 1, 0, 0
while ph ~= 0 do
  ph = ONESTEP()
  steps = steps + 1
  if ph == 1 then want = K['torture_repoint'](steps) end
end
-- Churn afterwards, so that a slot the sweep wrongly reclaimed is handed to
-- somebody else and holds their bytes. Without this a reclaimed-but-untouched
-- block still reads back correctly and the failure is invisible -- which is
-- exactly how a use-after-free behaves in the field, and why this line is here.
K['torture_garbage'](4000)
print(string.format('started=%d steps=%d verify0=%d want=%d got=%d verify=%d',
  started, steps, verify0, want, K['torture_repoint_verify'](),
  K['torture_verify']()))
`

	leg := func(one string) map[string]int {
		t.Helper()
		src := strings.NewReplacer(
			"BUDGET", strconv.Itoa(budget),
			"NODES", strconv.Itoa(nodes),
			"ONESTEP", one,
		).Replace(body)
		return gcFields(t, gcRun(t, h, "./examples/gctorture", true, src))
	}
	barriered := leg("STEP")
	blind := leg("STEPBLIND")

	for name, f := range map[string]map[string]int{"barriered": barriered, "blind": blind} {
		if f["started"] != 1 {
			t.Fatalf("%s: no paced collection started; the rest of this test is "+
				"measuring a guest that never collected", name)
		}
		if f["steps"] < 4 {
			t.Fatalf("%s: the paced collection took %d steps, so there was no "+
				"'between two mark steps' for the store to land in", name, f["steps"])
		}
	}

	// The assertion.
	if barriered["got"] != barriered["want"] {
		t.Errorf("a fresh object stored into an already-marked object during "+
			"marking was LOST: the guest wrote checksum %d and reads back %d. "+
			"That is a use-after-free inside a lockstep simulation, and the only "+
			"symptom it has is this number",
			barriered["want"], barriered["got"])
	}
	if barriered["verify"] != barriered["verify0"] {
		t.Errorf("the retained structure itself changed across a paced "+
			"collection: %d before, %d after",
			barriered["verify0"], barriered["verify"])
	}

	// And the control: without the barrier the same run loses them. If this
	// ever stops failing, the test above has stopped testing the barrier and
	// something else is keeping those objects alive.
	if blind["got"] == blind["want"] {
		t.Errorf("the BLIND leg -- the same paced collection with the dirty page "+
			"set never drained -- kept every fresh object anyway (checksum %d). "+
			"The barrier is then not what is keeping them, so the assertion above "+
			"proves nothing; find what is and make this leg lose them again",
			blind["got"])
	}
	t.Logf("%d nodes at a %d-granule budget: %d steps; barriered checksum %d "+
		"(want %d), blind %d -- the barrier is load-bearing",
		nodes, budget, barriered["steps"], barriered["got"], barriered["want"],
		blind["got"])
}

// THE HEADLINE: what one paced step costs against the same collection stopped
// the world, at the same heap.
//
// agents/gc.md's stage-B table is the target, and the rows that matter are the
// ones where a stop-the-world collection is most of a frame or worse:
//
//	heap       live      objects   pause      ms/MiB
//	2.39 MiB   412 KiB     5,031   32.39 ms     13.9
//	20.71 MiB  2.69 MiB   50,486  677.40 ms     32.7
//
// THERE IS NO CLOCK IN THE SANDBOX -- bin/lua52f is patched to Factorio's shape
// and has no `os` at all -- so a pause is DERIVED rather than sampled, the same
// way stage A derived the reference collector's and stage B derived its own:
//
//	one whole collection's wall time is measured from the Go side, across a
//	pair of runs that differ only in how many collections they do, so
//	everything that is not a collection cancels; and the collector reports
//	what fraction of that collection's WORK landed in its worst single step.
//	The worst tick is the product.
//
// That is stronger than sampling a clock per step would have been, because it
// separates the two things that can go wrong: whether the collector is fast
// (the whole-collection time, which pacing does not change and must not) and
// whether it is BOUNDED (the fraction, which is the entire point of stage C).
func TestAPacedStepCostsFarLessThanTheStopTheWorldPause(t *testing.T) {
	h := needGuest(t)
	// A live set big enough that a stop-the-world collection is a real pause.
	// gctorture's nodes are 48 bytes in a 48-byte class.
	const nodes, garbage = 40000, 20000

	// Leg one: K collections against one, differenced. The build, the parse and
	// everything else the run does are identical in both, so they cancel.
	stwBody := func(k int) string {
		return fmt.Sprintf(`
K['torture_build'](%d)
for i = 1, %d do
  K['torture_garbage'](%d)
  K['torture_collect']()
end
print(string.format('heap=%%d live=%%d verify=%%d', K['torture_stat'](0),
  K['torture_stat'](1), K['torture_verify']()))
`, nodes, k, garbage)
	}
	const kMany, kFew = 6, 1
	manyOut, tMany := gcTimed(t, h, "./examples/gctorture", true, stwBody(kMany))
	_, tFew := gcTimed(t, h, "./examples/gctorture", true, stwBody(kFew))
	perCollect := (tMany - tFew).Seconds() * 1000 / float64(kMany-kFew)

	// A NOISY RUN MUST NOT SKIP, which is what this used to do the moment the
	// difference came out at or below zero. agents/benchmarks.md's rule is the
	// reason: a timing test that quietly declines to assert is the silent-skip
	// shape in a different costume, and `go test` prints nothing for a skip
	// without -v -- so a busy machine turned the whole pacing gate into `ok`.
	//
	// Nothing below actually NEEDS the milliseconds. The gate is `frac > 0.1`,
	// stated in granules of work, and work is what this machine and Factorio have
	// in common; the wall-clock derivation is a REPORT sitting on top of it. So
	// the noise case loses the report and keeps every assertion, and says so
	// where a reader will see it rather than in a skip nobody reads.
	timed := perCollect > 0
	if !timed {
		t.Logf("NOTE: the differenced stop-the-world time came out at %.3f ms, "+
			"which is noise rather than a measurement -- this machine is busy. The "+
			"derived worst-tick milliseconds below are suppressed; the pacing gate "+
			"is in granules of work and still runs.", perCollect)
	}

	// Leg two: the same collection, paced at the default budget. What comes back
	// is the WORK split, which is what the derivation needs.
	pacedBody := fmt.Sprintf(`
K['torture_build'](%d)
K['torture_gc_budget'](0)
K['torture_garbage'](%d)
K['torture_gc_start']()
local steps = PACE()
print(string.format('heap=%%d live=%%d steps=%%d budget=%%d maxwork=%%d totalwork=%%d verify=%%d',
  K['torture_stat'](0), K['torture_stat'](1), steps, K['torture_stat'](11),
  K['torture_stat'](12), K['torture_stat'](13), K['torture_verify']()))
`, nodes, garbage)
	p := gcFields(t, gcRun(t, h, "./examples/gctorture", true, pacedBody))

	many := gcFields(t, manyOut)
	if p["verify"] != many["verify"] {
		t.Fatalf("the paced collection changed the answer: stop-the-world %d, "+
			"paced %d", many["verify"], p["verify"])
	}
	if p["steps"] < 8 || p["totalwork"] == 0 {
		t.Fatalf("the paced collection took %d steps and charged %d granules; "+
			"nothing below means anything", p["steps"], p["totalwork"])
	}

	frac := float64(p["maxwork"]) / float64(p["totalwork"])
	worst := perCollect * frac
	heapMiB := float64(p["heap"]) / (1 << 20)
	t.Logf("heap %.2f MiB, live %.0f KiB, budget %d granules:",
		heapMiB, float64(p["live"])/1024, p["budget"])
	t.Logf("  paced          : %d steps, %d granules total, worst step %d granules "+
		"(%.2f%% of the collection)", p["steps"], p["totalwork"], p["maxwork"],
		frac*100)
	if timed {
		t.Logf("  stop-the-world : %.1f ms in ONE tick (%.1f ms/MiB of heap), "+
			"differenced over %d collections", perCollect, perCollect/heapMiB, kMany-kFew)
		t.Logf("  worst tick     : ~%.3f ms paced against %.1f ms stopped -- %.0fx lower",
			worst, perCollect, perCollect/worst)
	}

	// THE GATE, in work rather than in milliseconds, because work is what this
	// machine and Factorio have in common. The whole point of pacing is the
	// worst tick and not the total, and a step that is a tenth of the whole
	// collection has not paced it.
	//
	// It runs whether or not the timing legs came back usable, which is the
	// difference between this and what it replaced.
	if frac > 0.1 {
		if timed {
			t.Errorf("the worst paced step charged %d of the collection's %d "+
				"granules -- %.1f%%, i.e. ~%.1f ms of the %.1f ms stop-the-world "+
				"pause. Pacing has to bound the WORST tick",
				p["maxwork"], p["totalwork"], frac*100, worst, perCollect)
		} else {
			t.Errorf("the worst paced step charged %d of the collection's %d "+
				"granules -- %.1f%%. Pacing has to bound the WORST tick",
				p["maxwork"], p["totalwork"], frac*100)
		}
	}
}

// NO STEP OVERRUNS ITS BUDGET, and the one shape that could is named.
//
// The budget is charged per granule of heap TOUCHED, and charge() saturates --
// so a step stops as soon as the allowance is gone. What a saturating
// subtraction cannot show is the case that matters: an INDIVISIBLE unit of work
// bigger than the whole allowance. agents/gc.md is explicit that this is the
// trap Lua's own collector fell into ("a gray unit that is a whole object of
// unbounded size"), so the collector counts what it charged unsaturated and
// this reads it back.
//
// A LARGE object is the shape: a 1 MiB Go slice is 65,536 granules against a
// 1,024-granule budget, which would be a ~32 ms tick -- stage B's pause, back
// where it started. It is scanned through a resumable cursor instead, and this
// test is what says so.
func TestNoPacedStepOverrunsItsBudget(t *testing.T) {
	h := needGuest(t)
	const budget = 1024
	body := fmt.Sprintf(`
K['torture_gc_budget'](%d)
K['torture_build'](20000)
-- A large object, deliberately far bigger than one step's whole allowance:
-- 400,000 words is 1.6 MiB, i.e. 100,000 granules against a 1,024 budget.
K['torture_large'](400000)
K['torture_garbage'](8000)
K['torture_gc_start']()
local steps = PACE()
print(string.format('steps=%%d budget=%%d maxwork=%%d totalwork=%%d large=%%d verify=%%d',
  steps, K['torture_stat'](11), K['torture_stat'](12), K['torture_stat'](13),
  K['torture_large_read'](), K['torture_verify']()))
`, budget)
	f := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))
	t.Logf("a 1.6 MiB object at a %d-granule budget: %d steps, %d granules "+
		"total, worst step %d granules (%.2fx the budget)",
		f["budget"], f["steps"], f["totalwork"], f["maxwork"],
		float64(f["maxwork"])/float64(f["budget"]))

	// The bar is not 1.00x. Three things a step does are legitimately not
	// charged against the heap budget and are all bounded by construction: the
	// root re-scan (~576 bytes of globals), ingesting at most dirtyCap page
	// numbers, and finishing the small-class SLOT the sweep is inside. What is
	// being looked for here is an unbounded unit, which shows up as a multiple
	// and not as a margin.
	if f["maxwork"] > 4*f["budget"] {
		t.Errorf("a single step charged %d granules against a budget of %d -- "+
			"%.1fx. Some unit of work is indivisible and bigger than a step, "+
			"which is exactly the trap agents/gc.md says killed pacing for Lua's "+
			"own collector: the budget then bounds nothing",
			f["maxwork"], f["budget"], float64(f["maxwork"])/float64(f["budget"]))
	}
	// ...and the object has to have survived being scanned in pieces.
	if f["large"] == 0 {
		t.Errorf("the large object did not survive a paced collection that " +
			"scanned it across several steps")
	}
}

// THE BUDGET IS THE KNOB, and it has to behave like one.
//
// Raising the allowance should take fewer steps and cost more per step, and a
// guest author picks a point on that line: a mod that cannot afford 0.5 ms a
// tick turns it down and pays in latency, one that wants the collection over
// with turns it up and pays in worst tick. If the two did not move together the
// budget would be a constant somebody picked rather than a pacing parameter.
func TestTheBudgetTradesStepsAgainstWorstTick(t *testing.T) {
	h := needGuest(t)
	body := `
K['torture_build'](20000)
for _, b in ipairs({256, 1024, 4096}) do
  K['torture_gc_budget'](b)
  K['torture_garbage'](8000)
  K['torture_gc_start']()
  local steps = PACE()
  print(string.format('budget=%d steps=%d maxwork=%d', b, steps,
    K['torture_stat'](12)))
end
print(string.format('verify=%d', K['torture_verify']()))
`
	out := gcRun(t, h, "./examples/gctorture", true, body)

	type row struct{ budget, steps, maxwork int }
	var rows []row
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "budget=") {
			continue
		}
		var r row
		for _, kv := range strings.Fields(strings.TrimSpace(line)) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			switch k {
			case "budget":
				r.budget = n
			case "steps":
				r.steps = n
			case "maxwork":
				r.maxwork = n
			}
		}
		rows = append(rows, r)
	}
	if len(rows) != 3 {
		t.Fatalf("expected three budget rows, got %d:\n%s", len(rows), out)
	}
	for _, r := range rows {
		t.Logf("budget %5d granules: %4d steps, worst step %5d granules (%.2fx budget)",
			r.budget, r.steps, r.maxwork, float64(r.maxwork)/float64(r.budget))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].steps > rows[i-1].steps {
			t.Errorf("raising the budget from %d to %d took the step count from "+
				"%d UP to %d; the budget is not the pacing knob it is documented as",
				rows[i-1].budget, rows[i].budget, rows[i-1].steps, rows[i].steps)
		}
		if rows[i].maxwork < rows[i-1].maxwork {
			t.Errorf("raising the budget from %d to %d took the worst step DOWN "+
				"from %d to %d granules; a bigger allowance has to buy a bigger "+
				"step, or the allowance is not what is bounding one",
				rows[i-1].budget, rows[i].budget, rows[i-1].maxwork, rows[i].maxwork)
		}
	}
	if rows[len(rows)-1].steps >= rows[0].steps {
		t.Errorf("a 16x budget produced %d steps against %d at the smallest, "+
			"i.e. no change. Either the budget is not being charged or something "+
			"other than the budget decides how long a step is",
			rows[len(rows)-1].steps, rows[0].steps)
	}
	// And every point on the line has to stay bounded, which is the property a
	// guest author is actually buying.
	for _, r := range rows {
		if r.maxwork > 4*r.budget {
			t.Errorf("at a budget of %d the worst step charged %d granules -- "+
				"%.1fx. The budget bounds the worst tick or it means nothing",
				r.budget, r.maxwork, float64(r.maxwork)/float64(r.budget))
		}
	}
}

// PACED, THE HEAP IS STILL BOUNDED -- which is the one thing that could quietly
// stop being true when the sweep stops running to completion.
//
// A stop-the-world sweep hands the mutator a fully swept heap. A paced one does
// not: an allocation can land while half the heap is still undecided, and if it
// were served from a span the sweep has not reached, that span would afterwards
// be walked, found to hold unmarked slots -- nothing marks after termination --
// and freed with live objects in it. findSpanRun's window is the fix, and
// allocSpans sweeping ahead rather than growing is what stops the window from
// starving the mutator into a memory.grow.
//
// This drives churn through the collector paced rather than stopped, over
// enough events for the interaction to happen thousands of times, and asserts
// the two things that would break: the answer, and the heap.
func TestPacedChurnAgreesAndStaysBounded(t *testing.T) {
	h := needGuest(t)
	const events, batch = 20000, 200
	// The paced leg drives exactly the protocol control.lua drives: the guest
	// asks for a collection when its own pressure says so, and the host runs
	// one bounded step per tick until it ends.
	paced := fmt.Sprintf(`
local done, sum, steps, cycles = 0, 0, 0, 0
while done < %d do
  local n = %d if %d - done < n then n = %d - done end
  sum = K['churn_events'](n)
  done = done + n
  K['churn_gc_tick']()
  -- ONE STEP PER EVENT, because one event per tick is the case agents/gc.md
  -- says this feature is for and one step per tick is what control.lua runs.
  -- Stepping once per BATCH instead would model a guest allocating 200 events'
  -- worth in a single tick, which is the blueprint-paste case -- covered with
  -- latency and a bulge -- not the steady one this gate is about.
  for _ = 1, n do
    if COLLECTING() then steps = steps + 1 STEP() end
  end
end
-- Drain whatever is still in flight, so the final numbers are a settled heap.
steps = steps + PACE()
print(string.format('checksum=%%d words=%%d heap=%%d live=%%d grows=%%d cycles=%%d steps=%%d',
  sum, WORDS(), K['churn_gc_stat'](0), K['churn_gc_stat'](1),
  K['churn_gc_stat'](4), K['churn_gc_stat'](3), steps))
`, events, batch, events, events)

	stw := fmt.Sprintf(`
local done, sum = 0, 0
while done < %d do
  local n = %d if %d - done < n then n = %d - done end
  sum = K['churn_events'](n)
  done = done + n
  K['churn_collect']()
end
print(string.format('checksum=%%d words=%%d heap=%%d live=%%d grows=%%d cycles=%%d steps=0',
  sum, WORDS(), K['churn_gc_stat'](0), K['churn_gc_stat'](1),
  K['churn_gc_stat'](4), K['churn_gc_stat'](3)))
`, events, batch, events, events)

	p := gcFields(t, gcRun(t, h, "./examples/churn", true, paced))
	s := gcFields(t, gcRun(t, h, "./examples/churn", true, stw))
	l := gcFields(t, gcRun(t, h, "./examples/churn", false, stw))

	if p["checksum"] != l["checksum"] {
		t.Errorf("paced collection changed the answer: -gc=leaking checksum %d, "+
			"paced %d. A collector that reclaims something live does not raise; "+
			"this number is the only symptom", l["checksum"], p["checksum"])
	}
	if p["cycles"] == 0 || p["steps"] == 0 {
		t.Fatalf("%d collections in %d steps -- the paced leg did not pace "+
			"anything and nothing below means anything", p["cycles"], p["steps"])
	}
	t.Logf("%d events: leaking %d words, stop-the-world %d words, paced %d "+
		"words (%d B heap, %d B live, %d grows, %d collections over %d steps)",
		events, l["words"], s["words"], p["words"], p["heap"], p["live"],
		p["grows"], p["cycles"], p["steps"])

	// The heap bound. Paced is allowed to float higher than stop-the-world --
	// it is collecting later, by exactly the pacing latency -- but not by an
	// order of magnitude, and nowhere near -gc=leaking.
	if p["words"]*4 >= l["words"] {
		t.Errorf("paced linear memory is %d words against -gc=leaking's %d; the "+
			"collector is meant to keep the guest OFF the doubling ladder and a "+
			"heap within 4x of the leaking one has not", p["words"], l["words"])
	}
	if s["words"] > 0 && p["words"] > 4*s["words"] {
		t.Errorf("paced linear memory is %d words against the same guest "+
			"stop-the-world at %d -- more than 4x. Pacing costs latency and a "+
			"bounded bulge; this is the sweep failing to keep up, which shows as "+
			"a memory.grow that can never be undone", p["words"], s["words"])
	}
}

// gcFloats is gcFields for values that are not integers. Kept separate rather
// than widening gcFields, because gcFields' "a value that is not a number is
// not a field" rule is what keeps guest LOG lines out of the map, and a float
// parser would let "fnv64(fklua)=449d" through as something.
func gcFloats(t *testing.T, out string) map[string]float64 {
	t.Helper()
	got := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		for _, kv := range strings.Fields(strings.TrimSpace(line)) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(f) {
				continue
			}
			got[k] = f
		}
	}
	if len(got) == 0 {
		t.Fatalf("no key=value fields in:\n%s", out)
	}
	return got
}

// ALLOCATING THROUGH A PACED SWEEP MUST NOT LOSE A LIVE OBJECT, and this is the
// regression test for the bug that says why it is not obvious.
//
// A stop-the-world sweep hands the mutator a decided heap. A paced one runs
// while the mutator allocates, and everything a class hands out during it is
// UNMARKED -- nothing marks after termination -- so every one of those blocks is
// a candidate for being freed by the sweep that has not reached it yet. Three
// separate things keep them:
//
//  1. Span allocation is restricted to below the sweep cursor, so a fresh span
//     is never claimed in territory the sweep is still going to decide.
//  2. A span released by the sweep is zeroed and re-threaded before anything can
//     allocate from it, so a block handed out of it is in swept territory.
//  3. Each class's current run is HELD, at the window it had when marking
//     terminated -- and holding the LIVE cursor instead is the bug this test
//     exists for. curPtr advances as the class serves allocations, so by the
//     time the sweep reaches the span holding that run, every block handed out
//     since termination is below the cursor, outside a window computed from it,
//     unmarked, and freed while live.
//
// Measured before the fix on this exact guest: 19 of 32 retained blocks intact,
// at a 256-granule budget, with no error anywhere -- the blocks came back zeroed
// and then somebody else's, which is what a use-after-free looks like from
// inside a lockstep simulation.
//
// The small budget is the point. It stretches the sweep over more ticks, which
// is more allocation inside it; a test at the default budget would have passed.
func TestAllocatingThroughAPacedSweepKeepsLiveObjects(t *testing.T) {
	h := needGuest(t)
	// 128 is deliberately below what this guest's write rate needs, and 800
	// ticks is past the mark-termination deadline. See the assertions below:
	// the small budget is also the livelock case, and it has to come out with
	// its blocks intact and a collection finished, not just without an error.
	deadlines := map[int]int{}
	for _, budget := range []int{128, 512, 2048} {
		body := fmt.Sprintf(`
K['fk_gc_budget'](%d)
K['fk_on_init']()
K['fk_gc_budget'](%d)
local worst = 32
for tick = 0, 800 do
  K['fk_on_tick'](tick)
  if COLLECTING() then STEP() end
  local n = K['fk_gc_intact']()
  if n < worst then worst = n end
end
print(string.format('budget=%d worst=%%d cycles=%%d grows=%%d deadlines=%%d',
  worst, K['fk_gc_stat'](3), K['fk_gc_stat'](4), K['fk_gc_stat'](14)))
`, budget, budget, budget)
		f := gcFields(t, gcRun(t, h, "./examples/gcsave", true, body))
		t.Logf("budget %4d: %d/32 blocks intact at the worst tick, %d collections, "+
			"%d grows, %d deadlines", f["budget"], f["worst"], f["cycles"],
			f["grows"], f["deadlines"])
		deadlines[f["budget"]] = f["deadlines"]
		if f["cycles"] < 1 {
			t.Errorf("budget %d: no collection completed over 800 ticks -- the "+
				"sweep this test is about never ran, and neither did the "+
				"mark-termination deadline that is supposed to guarantee it does",
				f["budget"])
		}
		if f["worst"] != 32 {
			t.Errorf("budget %d: only %d of 32 retained blocks were intact at the "+
				"worst tick. A live object was reclaimed by a sweep running "+
				"alongside the allocation that produced it -- there is no error "+
				"and no trap, only this number", f["budget"], f["worst"])
		}
	}

	// THE LIVELOCK, and the escape from it.
	//
	// At 128 granules a step cannot even re-scan one dirtied 4 KiB span, so
	// every step spends its whole allowance on the backlog and never reaches
	// the termination attempt. Before the deadline existed this guest sat in
	// phase 1 for 120 ticks of a REAL Factorio with cycles=0 -- no error, no
	// pause, and a heap growing exactly as if there were no collector, which is
	// the worst failure available to a guest that opted in to one.
	//
	// So the small budget must show deadlines FIRING, and the large ones must
	// show them not firing -- otherwise the deadline is either not working or
	// is being hit by guests that did not need it.
	if deadlines[128] == 0 {
		t.Errorf("at a 128-granule budget the mark-termination deadline never " +
			"fired. Either this guest's write rate now fits inside that budget " +
			"-- in which case the livelock case has stopped being covered and " +
			"needs a smaller one -- or the deadline is not wired up")
	}
	if deadlines[2048] != 0 {
		t.Errorf("at a 2048-granule budget the deadline fired %d times. It is a "+
			"livelock escape and not part of normal pacing; a guest hitting it "+
			"at a comfortable budget is taking an unbounded mark-termination "+
			"pause it did not need", deadlines[2048])
	}
	t.Logf("mark-termination deadlines by budget: 128 -> %d, 512 -> %d, 2048 -> %d",
		deadlines[128], deadlines[512], deadlines[2048])
}

// A GUEST WHOSE ROOTS ARE BIGGER THAN ITS BUDGET STILL FINISHES A MARK.
//
// This is the other way to never terminate a mark phase, and it is a different
// defect from the livelock above with an identical symptom -- which is what made
// it expensive. The livelock is the guest's WRITE RATE against the budget, and
// SetBudget is its fix. This one is the guest's ROOT SET against the budget, and
// SetBudget is not: a termination attempt walks [__global_base, __heap_base)
// wholesale and charges what it walked, so when that charge saturates the whole
// allowance the post-scan check reads "out of budget" and defers to a step that
// will do exactly the same thing, forever, at any allocation rate including
// zero. Nothing is reclaimed and nothing is logged.
//
// Reported from the field by BetterBeltBalancer, whose globals grew 104 bytes
// past its own budget's cliff and whose deadline count was then read -- following
// SetBudget's own comment, which said so -- as an allocation rate. Measured on
// this guest before the fix, at 390 root words (97 granules of charge):
//
//	budget   steps   termination attempts   deadlines
//	  1024       3                      1           0
//	    64     915                    903           1
//	     8   3,051                  3,014           1
//
// and worse as the budget falls, because markDeadline scales as heap/budget.
//
// The fix is a floor rather than a resumable scan, and the choice is forced
// rather than measured: the roots are BELOW the heap, ingestDirty drops every
// dirty page below heapBase, so there is no write barrier over the globals and
// the terminate-time barrier is sound only because the range is read in one
// uninterrupted pass at one safe point. A scan resumed across two safe points
// would miss a reference moved backwards across the cursor between them, which
// is a live object swept. See fkgc's rootScanMargin.
func TestAMarkTerminatesWhenTheRootsCostMoreThanTheBudget(t *testing.T) {
	h := needGuest(t)
	// 8 granules is 128 bytes of heap: far below any plausible root set, so the
	// floor certainly binds. The control at the default budget is what says the
	// floor does not bind where it is not needed.
	const starved, comfortable = 8, 1024
	leg := func(budget int) map[string]int {
		t.Helper()
		body := fmt.Sprintf(`
K['torture_gc_budget'](%d)
K['torture_build'](200)
local verify0 = K['torture_verify']()
local started = K['torture_gc_start']()
local ph, steps = 1, 0
-- A hard cap rather than PACE(), because the defect under test is an infinite
-- loop: a bare `+"`while ph ~= 0`"+` would hang the suite instead of failing it.
while ph ~= 0 and steps < 4000 do ph = STEP() steps = steps + 1 end
print(string.format('budget=%%d eff=%%d started=%%d steps=%%d phase=%%d rootwords=%%d '..
  'terms=%%d deadlines=%%d verify0=%%d verify=%%d warned=%%d',
  K['torture_stat'](11), K['torture_stat'](26), started, steps,
  K['torture_stat'](9), K['torture_stat'](19), K['torture_stat'](18),
  K['torture_stat'](14), verify0, K['torture_verify'](),
  (LOGS():find('ROOT SET') and 1 or 0)))
`, budget)
		return gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))
	}

	f := leg(starved)
	if f["started"] != 1 {
		t.Fatalf("no paced collection started; nothing below means anything")
	}
	if f["rootwords"] == 0 {
		t.Fatalf("the guest reports no root words at all, so the condition this " +
			"test is about cannot be present and the assertions are vacuous")
	}
	// The condition really is the one under test: the roots cost more than the
	// budget asked for. If a future guest's globals shrank below 8 granules this
	// would stop being true and the test would be measuring nothing.
	if cost := f["rootwords"] / 4; cost <= starved {
		t.Fatalf("the root re-scan costs %d granules against a %d-granule budget, "+
			"so the budget is NOT the smaller of the two and this test no longer "+
			"reproduces the starvation. Lower the budget", cost, starved)
	}

	// THE ASSERTION. Before the floor this was 3,014 attempts and a deadline.
	if f["phase"] != 0 {
		t.Errorf("the collector is still in phase %d after %d steps at a "+
			"%d-granule budget: the mark never terminated", f["phase"], f["steps"],
			starved)
	}
	if f["terms"] > 4 {
		t.Errorf("the mark phase made %d termination attempts over %d steps. Each "+
			"one re-walks the whole root range and banks nothing, which is the "+
			"starvation this test is about -- a converging mark needs one",
			f["terms"], f["steps"])
	}
	if f["deadlines"] != 0 {
		t.Errorf("the mark-termination deadline fired %d times at a %d-granule "+
			"budget on a guest that is not writing anything. The deadline is a "+
			"livelock escape for a write rate over the budget; reaching it here "+
			"means the root scan is starving termination again", f["deadlines"],
			starved)
	}
	// The floor is what did it, and the guest was told.
	if f["eff"] <= f["budget"] {
		t.Errorf("EffectiveBudget is %d against a requested %d -- the floor did "+
			"not bind, so whatever terminated the mark was not the fix under test",
			f["eff"], f["budget"])
	}
	if f["warned"] != 1 {
		t.Errorf("the collector raised the budget from %d to %d and logged no "+
			"fkgc: line about it. Nothing outside the collector can see this "+
			"condition -- rootWords is measured in a scan the host never sees -- "+
			"so an unlogged floor is a guest silently not getting the pause it "+
			"asked for", f["budget"], f["eff"])
	}
	// ...and it is still a correct collection.
	if f["verify"] != f["verify0"] {
		t.Errorf("the retained structure changed across the collection: %d "+
			"before, %d after. Terminating a mark early reclaims live objects",
			f["verify0"], f["verify"])
	}
	t.Logf("%d root words (%d granules) at a %d-granule budget: effective %d, "+
		"%d steps, %d termination attempts, %d deadlines",
		f["rootwords"], f["rootwords"]/4, f["budget"], f["eff"], f["steps"],
		f["terms"], f["deadlines"])

	// THE CONTROL: at a budget the roots fit inside, nothing moves. The floor
	// must not be a tax on every guest that never had the problem.
	c := leg(comfortable)
	if c["eff"] != c["budget"] {
		t.Errorf("at a %d-granule budget the effective budget is %d. This guest's "+
			"roots cost %d granules and fit; a floor that binds here would be "+
			"raising every guest's pause for a condition it does not have",
			comfortable, c["eff"], c["rootwords"]/4)
	}
	if c["warned"] != 0 {
		t.Errorf("the collector logged the root-set line at a %d-granule budget "+
			"the roots fit inside. It is once-per-guest advice about a real "+
			"misconfiguration and must not fire on guests that are fine",
			comfortable)
	}
}
