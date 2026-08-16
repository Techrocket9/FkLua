package guest_test

import (
	"testing"
)

// SHARDING STAGE C: THERE IS NO fkgc HEAP CAP, and these are the gates that say
// so rather than the comments that used to.
//
// What was here before was a constant. `fkgc.HeapCap` was 16 MiB by default and
// a HARD cap -- a guest that grew past it trapped, with a bare `unreachable`,
// because wasm-unknown has no stderr -- and two build tags moved it to 4 and 64.
// It existed for one reason: the mark bitmap and the span table were a
// statically reserved .bss struct, and static means sized at link time.
//
// The metadata scales with the heap now (guest/go/fkgc/meta.go), so the cap is
// deleted rather than raised, and what replaces it is a COST rather than a
// smaller number. Every assertion below is about that cost or about the sizes
// the cap used to refuse.

// THE SIZE MODEL IS GENERATED AND CHECKED, not written down.
//
// The prose it replaces was wrong in both directions and nothing tested it:
// `cap4.go` said ~42 KiB against a measured 58.32, `cap64.go` said ~645 against
// 583.32. A number in a comment that no test reads is a number that drifts, and
// with a metadata size that is now a FUNCTION of the heap there is a model to
// assert instead of three figures to maintain:
//
//	MetaBytes(heap) = MetaFixedBytes + ceil(covered / MetaSliceBytes) * MetaChunkBytes
//
// The test drives a real guest to several heap sizes and checks the identity at
// each, so both halves -- the model and the chunk accounting that implements it
// -- have to agree with each other and with a heap that actually exists.
func TestTheMetadataSizeModelHolds(t *testing.T) {
	h := needGuest(t)
	// 0, then a few sizes either side of the 4 MiB slice boundary the chunks
	// are cut on, and one well past the old 16 MiB cap.
	body := `
local function row(tag)
  print(string.format('%s meta=%d fixed=%d chunks=%d chunkb=%d sliceb=%d heap=%d backed=%d',
    tag, K['torture_stat'](8), K['torture_stat'](22), K['torture_stat'](21),
    K['torture_meta_chunk_bytes'](), K['torture_meta_slice_bytes'](),
    K['torture_stat'](0), K['torture_backed']()))
end
row('idle')
K['torture_hold'](1, 262144)      -- 1 MiB
row('m1')
K['torture_hold'](4, 262144)      -- 5 MiB: crosses one slice boundary
row('m5')
K['torture_hold'](15, 262144)     -- 20 MiB: past the old 16 MiB cap
row('m20')
`
	out := gcRun(t, h, "./examples/gctorture", true, body)
	rows := parseRows(t, out, []string{"idle", "m1", "m5", "m20"})

	fixed := rows["idle"]["fixed"]
	chunkB := rows["idle"]["chunkb"]
	sliceB := rows["idle"]["sliceb"]
	if fixed == 0 || chunkB == 0 || sliceB == 0 {
		t.Fatalf("the metadata model reports zeros: fixed=%d chunk=%d slice=%d",
			fixed, chunkB, sliceB)
	}
	// The FIXED part is what a guest pays for having a collector at all, before
	// it allocates anything, and it is the number that used to be 163 KiB. It is
	// asserted rather than logged for the reason the old budget test gives: it
	// is the kind of number that grows by accident, one array at a time.
	if fixed > 48*1024 {
		t.Errorf("the fixed metadata is %d B (%.1f KiB), over the 48 KiB budget. "+
			"That is linear memory every collected guest pays before it allocates "+
			"anything -- about %.3f ms of Factorio worst tick",
			fixed, float64(fixed)/1024, float64(fixed)/(1024*1024)*0.2)
	}
	for _, tag := range []string{"idle", "m1", "m5", "m20"} {
		r := rows[tag]
		want := fixed + r["chunks"]*chunkB
		if r["meta"] != want {
			t.Errorf("%s: MetaBytes reports %d, but fixed %d + %d chunks x %d is %d",
				tag, r["meta"], fixed, r["chunks"], chunkB, want)
		}
		// One chunk per slice of COVERED heap, and the covered heap is exactly
		// what HeapBytes is.
		wantChunks := (r["heap"] + sliceB - 1) / sliceB
		if r["chunks"] != wantChunks {
			t.Errorf("%s: %d chunks for a %d B heap; %d B per slice wants %d",
				tag, r["chunks"], r["heap"], sliceB, wantChunks)
		}
		t.Logf("%-4s heap %8.3f MiB  metadata %7d B = %d fixed + %d x %d  (%.3f%% of heap)",
			tag, float64(r["heap"])/(1<<20), r["meta"], fixed, r["chunks"], chunkB,
			100*float64(r["meta"]-fixed)/float64(maxi(r["heap"], 1)))
	}
	// And the slope, stated as the cost it is. One chunk covers one slice, so
	// the scaling part is chunkB/sliceB of the heap forever.
	t.Logf("scaling metadata is %d B per %d B of heap = %.3f%%, on top of a %d B floor",
		chunkB, sliceB, 100*float64(chunkB)/float64(sliceB), fixed)
}

// A HEAP THAT GROWS THROUGH THE OLD CAP AND PAST 32 MiB, WITH THE ANSWER INTACT.
//
// This is the acceptance gate for the cap removal and it is deliberately stated
// in the sizes the cap used to refuse: 16 MiB was the default wall a guest
// trapped against, and 32 MiB is past the largest build tag's half-way point.
// The checksum is what makes it a gate rather than a size report -- a collector
// that reclaims something live does not produce an error, it produces a
// different number.
func TestAHeapGrowsThroughTheOldCapWithItsAnswerIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: a 36 MiB heap under the oracle is slow")
	}
	h := needGuest(t)
	// 36 MiB in 1 MiB blocks, checked after every collection.
	body := `
local sum = K['torture_hold'](36, 262144)
local before = K['torture_hold_verify']()
K['torture_collect']()
local after = K['torture_collect']() and K['torture_hold_verify']()
K['torture_garbage'](20000)
K['torture_collect']()
print(string.format('sum=%d before=%d after=%d final=%d bytes=%d heap=%d backed=%d chunks=%d meta=%d cycles=%d grows=%d marks=%d',
  sum, before, after, K['torture_hold_verify'](), K['torture_hold_bytes'](),
  K['torture_stat'](0), K['torture_backed'](), K['torture_stat'](21),
  K['torture_stat'](8), K['torture_stat'](3), K['torture_stat'](4),
  K['torture_stat'](20)))
`
	f := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))
	for _, k := range []string{"before", "after", "final"} {
		if f[k] != f["sum"] {
			t.Errorf("%s checksum is %d against %d written: the collector reclaimed "+
				"something the guest was still holding", k, f[k], f["sum"])
		}
	}
	const cap16, want32 = 16 << 20, 32 << 20
	if f["heap"] <= cap16 {
		t.Fatalf("the heap reached only %d B, which is under the 16 MiB cap this "+
			"stage removed -- the test is not exercising what it says", f["heap"])
	}
	if f["heap"] <= want32 {
		t.Errorf("the heap reached %d B (%.1f MiB); the gate is PAST 32 MiB",
			f["heap"], float64(f["heap"])/(1<<20))
	}
	if f["bytes"] < want32 {
		t.Errorf("the guest is holding only %d B; it was asked for 36 MiB", f["bytes"])
	}
	// The mark bitmap is zero once a collection has finished, which is the
	// invariant that lets a collection start without wiping it -- see the note
	// where clearMarkBits used to be.
	if f["marks"] != 0 {
		t.Errorf("%d mark bits are still set with the collector idle. A collection "+
			"no longer wipes the bitmap on the way in, and that is only sound "+
			"because the sweep clears every span on the way out", f["marks"])
	}
	t.Logf("36 MiB held: heap %.2f MiB in %d chunks, metadata %d B (%.2f%%), "+
		"%d collections, %d grows, checksum unmoved",
		float64(f["heap"])/(1<<20), f["chunks"], f["meta"],
		100*float64(f["meta"])/float64(f["heap"]), f["cycles"], f["grows"])
}

// AN ALLOCATION STORM GROWS. IT DOES NOT COLLECT, AND IT DOES NOT THRASH.
//
// This is the pathology stage A triaged and the shape stage C had to design
// away. At heap pressure the old allocator ran a full synchronous Collect() per
// failing span allocation -- about 1.4 s at a time in a real Factorio, inside an
// event handler, repeatedly -- because the heap could not grow past the cap and
// the alternative was a trap.
//
// With no cap the answer is the one the product position asks for: a burst far
// beyond the reclaim rate degrades to GROW-LIKE-LEAKING and recovers on the
// paced ticks that follow. What has to be asserted is the negative, and the
// instruments for it are new: Outruns counts the grows that happened while a
// collection was in flight, and MaxUnpaced counts collector work charged inside
// a guest call -- the number MaxStepWork could never see, and the reason the
// host-side gate read 1.17x of budget while the game read 65x.
func TestAnAllocationStormGrowsInsteadOfCollectingSynchronously(t *testing.T) {
	h := needGuest(t)
	// THE SHAPE IS BURST THEN RECOVERY, because that is what a storm is. A
	// mutator that never stops outrunning its collector is not a storm, it is a
	// guest whose steady state is over its budget, and agents/gc.md already says
	// what that costs: the heap grows exactly as if there were no collector.
	// What has to be true here is that the burst costs GROWTH and not a pause,
	// and that the collector comes back the moment the burst ends.
	//
	// 120 steps of 2,000 dropped nodes each -- 96 KB a step against a budget
	// that reclaims about 16 KB of it -- and then nothing.
	body := `
K['torture_gc_budget'](1024)
K['torture_build'](20000)
local verify0 = K['torture_verify']()
K['torture_gc_start']()
local ph, steps = 1, 0
while ph ~= 0 and steps < 120 do
  ph = STEP()
  steps = steps + 1
  K['torture_garbage'](2000)
end
local stormheap, stormgrows = K['torture_stat'](0), K['torture_stat'](4)
-- The burst is over. Nothing allocates from here.
local rec = 0
while ph ~= 0 and rec < 4000 do
  ph = STEP()
  rec = rec + 1
end
print(string.format('steps=%d rec=%d phase=%d verify0=%d verify=%d cycles=%d grows=%d stormgrows=%d outruns=%d maxunpaced=%d unpaced=%d budget=%d maxstep=%d heap=%d stormheap=%d deadlines=%d stepesc=%d stallesc=%d',
  steps, rec, K['torture_stat'](9), verify0, K['torture_verify'](),
  K['torture_stat'](3), K['torture_stat'](4), stormgrows, K['torture_stat'](15),
  K['torture_stat'](16), K['torture_stat'](17), K['torture_stat'](11),
  K['torture_stat'](12), K['torture_stat'](0), stormheap, K['torture_stat'](14),
  K['torture_stat'](27), K['torture_stat'](28)))
`
	f := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))

	if f["verify"] != f["verify0"] {
		t.Errorf("the retained structure changed across the storm: %d before, %d after",
			f["verify0"], f["verify"])
	}
	// RECOVERY. Once the burst stops the paced collection has to finish, on its
	// own, without a pause -- that is the whole of "grow-like-leaking plus paced
	// recovery". A phase that never leaves 1 is the mark livelock agents/gc.md
	// calls the worst failure available to a guest that opted in to a collector.
	if f["phase"] != 0 {
		t.Errorf("the collection was still in phase %d after %d storm steps and "+
			"%d quiet ones; a burst is meant to cost growth and then RECOVER",
			f["phase"], f["steps"], f["rec"])
	}
	// And the heap stops growing when the burst does. Growth during the storm is
	// the design; growth after it is a collector that never caught up.
	if f["grows"] != f["stormgrows"] {
		t.Errorf("the heap grew %d more times after the burst ended (%d during, %d "+
			"total): the collector did not catch up", f["grows"]-f["stormgrows"],
			f["stormgrows"], f["grows"])
	}
	// THE ASSERTION. Collector work inside a guest call is what a paced budget
	// cannot see and what lands in the middle of somebody's event handler. It is
	// allowed to be one bounded sweep-ahead bite; it is not allowed to be a
	// collection.
	if f["maxunpaced"] > 4*f["budget"] {
		t.Errorf("%d granules of collector work landed inside ONE guest call, "+
			"against a budget of %d -- %.1fx. That is a synchronous collection in "+
			"an event handler, which is the pause the pacing exists to avoid; the "+
			"storm response is supposed to be a bounded sweep-ahead and then GROWTH",
			f["maxunpaced"], f["budget"],
			float64(f["maxunpaced"])/float64(f["budget"]))
	}
	// THE PACED STEPS DO NOT ALL STAY PACED, AND THAT IS THE DESIGNED TRADE.
	//
	// A burst 30x over the budget is indistinguishable, while it is running,
	// from a guest whose steady state is 30x over the budget -- the only thing
	// that would tell them apart is waiting, and waiting costs linear memory
	// that never comes back. So the mark-phase forward-progress escape fires,
	// ONCE, latched, and finishes the phase unbudgeted; `Deadlines` counts it
	// and this test asserts that count rather than pretending it is zero.
	//
	// What it buys is a smaller steady heap than riding the burst out — the
	// exact figures moved when the grow pacing landed (an earlier build measured
	// 5.7 vs 13.9 MiB; current master measures 13.10 vs 13.88) and this comment
	// does not repeat them: the ASSERTED property is the one that matters, the
	// escape fires once, latched. What must NOT happen is the
	// escape firing repeatedly, which would be the per-allocation synchronous
	// collection this whole path replaced.
	if f["deadlines"] > 1 {
		t.Errorf("the mark escape fired %d times in one collection; it is latched "+
			"and must fire at most once, or it is a synchronous collection with "+
			"extra steps", f["deadlines"])
	}
	if f["deadlines"] == 0 && f["maxstep"] > 4*f["budget"] {
		t.Errorf("no escape fired, yet the worst paced step charged %d granules "+
			"against a budget of %d -- %.1fx. Without the escape every step is "+
			"supposed to be inside the budget", f["maxstep"], f["budget"],
			float64(f["maxstep"])/float64(f["budget"]))
	}
	// AND THE SPLIT SAYS WHICH ESCAPE, which is the assertion `deadlines` alone
	// could never make. The two causes want different answers -- a stall is the
	// mark failing to converge against the mutator's dirty rate, a step escape
	// is the far-out backstop catching a mark that is affordable but slow -- and
	// reading the total as though it were always the first is a mistake that has
	// been made twice, downstream, in writing.
	if got := f["stepesc"] + f["stallesc"]; got != f["deadlines"] {
		t.Errorf("the escape causes sum to %d and deadlines is %d; they are the "+
			"same events split two ways and every existing reader depends on the "+
			"total being unchanged (step=%d stall=%d)",
			got, f["deadlines"], f["stepesc"], f["stallesc"])
	}
	// THIS RUN IS A DIRTY-RATE STORM BY CONSTRUCTION -- 2,000 dropped nodes per
	// step against a budget that reclaims a fraction of them -- so the escape it
	// provokes has to be the forward-progress one. A step escape here would mean
	// the stall window did not see a mark that was demonstrably not converging
	// and the 600-step backstop cleaned up after it, which is a different defect
	// wearing the same number.
	if f["deadlines"] > 0 && f["stallesc"] == 0 {
		t.Errorf("the mark escaped %d time(s) and none of them was the "+
			"forward-progress stall (step=%d stall=%d) -- but this leg IS a dirty-"+
			"rate storm, so the stall window is what should have fired. A bare "+
			"backstop escape means the window did not see a mark that was not "+
			"converging", f["deadlines"], f["stepesc"], f["stallesc"])
	}
	// The storm has to have actually been a storm: if the heap never grew while
	// a collection was running, nothing above was tested.
	if f["outruns"] == 0 && f["grows"] == 0 {
		t.Fatalf("the heap never grew, so this run did not reproduce a storm at all")
	}
	t.Logf("storm: %d burst steps then %d quiet ones to finish; heap %.2f MiB at "+
		"the end of the burst and %.2f MiB after, %d grows (%d while collecting), "+
		"%d mark escape(s) (%d stall, %d backstop); worst step %d granules and "+
		"worst IN-CALL burst %d against a %d budget -- no synchronous collection "+
		"inside a guest call",
		f["steps"], f["rec"], float64(f["stormheap"])/(1<<20),
		float64(f["heap"])/(1<<20), f["grows"], f["outruns"], f["deadlines"],
		f["stallesc"], f["stepesc"],
		f["maxstep"], f["maxunpaced"], f["budget"])
}

// THE ALLOCATOR ADOPTS ALL PRE-EXISTING LINEAR MEMORY, AND THAT USED TO BE
// FALSE ABOVE 16 MiB.
//
// initialize() computed the heap from `memory.size` and then clamped it to
// HeapCap, so a guest handed a larger memory -- a migrate/adopt, or a module
// whose declared minimum is large -- came up believing it had 16 MiB and grew
// again to get memory it already owned. Silently: no log line, no counter, and
// a Stats() that agreed with itself.
//
// The probe is white-box on purpose. Growing the memory and re-running the
// allocator's initialisation is the only way to put "pre-existing memory" in
// front of initialize() from inside a test, and it asks exactly the question the
// clamp used to answer wrongly.
func TestTheAllocatorAdoptsEveryPageOfAPreExistingMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: this grows linear memory past 32 MiB")
	}
	h := needGuest(t)
	body := `
-- Grow linear memory well past the old cap, then drop everything, then ask the
-- allocator to adopt what is there.
K['torture_hold'](36, 262144)
local words = WORDS()
K['torture_drop_held']()
print(string.format('words=%d backed_before=%d reinit=%d', words,
  K['torture_backed'](), K['torture_reinit']()))
`
	f := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))
	mem := f["words"] * 4
	if mem <= 16<<20 {
		t.Fatalf("linear memory only reached %d B; the probe needs to be above the "+
			"old 16 MiB cap to say anything", mem)
	}
	// The heap is whatever is above __heap_base, so it is a little under the
	// whole memory -- but it must not be a ROUND number near 16 MiB, which is
	// what the clamp produced.
	if f["reinit"] <= 16<<20 {
		t.Errorf("with %d B of linear memory in front of it, the allocator adopted "+
			"%d B -- at or under the 16 MiB the cap used to clamp to. Pre-existing "+
			"memory above the cap is being discarded, which is the defect this "+
			"stage removed", mem, f["reinit"])
	}
	// Everything above __heap_base, to within one span of rounding.
	if lost := mem - f["reinit"]; lost > 1<<20 {
		t.Errorf("%d B of a %d B linear memory did not become heap. Only the "+
			"statics below __heap_base and at most one span of rounding may be "+
			"missing", lost, mem)
	}
	t.Logf("%.2f MiB of pre-existing linear memory, %.2f MiB adopted as heap "+
		"(%d B below __heap_base or rounded off)",
		float64(mem)/(1<<20), float64(f["reinit"])/(1<<20), mem-f["reinit"])
}

// parseRows pulls one map of key=value fields per tagged line.
func parseRows(t *testing.T, out string, tags []string) map[string]map[string]int {
	t.Helper()
	rows := map[string]map[string]int{}
	for _, line := range splitLines(out) {
		for _, tag := range tags {
			if len(line) > len(tag) && line[:len(tag)] == tag && line[len(tag)] == ' ' {
				rows[tag] = gcFields(t, line)
			}
		}
	}
	for _, tag := range tags {
		if rows[tag] == nil {
			t.Fatalf("no %q row in:\n%s", tag, out)
		}
	}
	return rows
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trimSpace(s[start:]))
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// THE GROW INCREMENT IS BOUNDED, AND THAT IS WHAT MAKES A GROW SURVIVABLE WHEN
// THE PRE-BUILD HAS NOT KEPT UP.
//
// fkgc grew by a QUARTER of the current heap, which at 40 MiB is a 10 MiB grow
// -- 2.6 million words at ~107 ns each, i.e. the 288-365 ms tick
// agents/guests.md records as the worst tick a growing guest has. The runtime's
// paced pre-build takes the common case to microseconds, but a guest allocating
// faster than the pre-build's 32 KiB a tick outruns it, and what it falls back
// to has to be bounded by something. This is that something.
//
// WHY THIS COSTS ALMOST NOTHING, which is the part that had to be measured
// rather than assumed: mem_grow has no fixed cost to amortise a large increment
// against. Fitted over four increments at three heap sizes in a real Factorio
// (scripts/run-growprobe.sh), the least-squares intercept is NEGATIVE at every
// size, and reaching 40 MiB in 640 grows of one wasm page costs 0.984x what
// reaching it in 10 grows of 4 MiB costs. The quarter law was buying nothing.
//
// The assertion is on the AVERAGE increment rather than on a maximum, because
// nothing exports a per-grow size and the average is what the cap controls: a
// single allocation always gets the spans it asked for, so a maximum would be a
// statement about the allocation and not about the policy. gctorture's node is
// 48 bytes -- one span, always -- so every grow here is the speculative kind.
func TestTheGrowIncrementIsBounded(t *testing.T) {
	h := needGuest(t)
	body := `
local heap0, grows0 = K['torture_stat'](0), K['torture_stat'](4)
-- 200,000 48-byte nodes is ~9.6 MiB, which under the old quarter law is about
-- fifteen grows and under a bounded one is about a hundred and fifty.
K['torture_build'](200000)
local v = K['torture_verify']()
print(string.format('heap0=%d grows0=%d heap=%d grows=%d verify=%d chunks=%d',
  heap0, grows0, K['torture_stat'](0), K['torture_stat'](4), v,
  K['torture_stat'](21)))
`
	f := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))

	grows := f["grows"] - f["grows0"]
	added := f["heap"] - f["heap0"]
	if grows <= 0 || added <= 0 {
		t.Fatalf("this run did not grow at all (%d grows, %d bytes): nothing below "+
			"is about the increment", grows, added)
	}
	avg := added / grows
	// The cap is 16 spans = 64 KiB. The slack is 2x rather than exact because a
	// grow that crosses a 4 MiB slice boundary is rounded up by the metadata
	// chunk's ten spans, and because `initialize` adopts whatever linear memory
	// the module was born with.
	const cap = 16 * 4096
	if avg > 2*cap {
		t.Errorf("the average grow added %d bytes over %d grows (%.1f KiB each) "+
			"against a %d KiB cap. A quarter of a large heap is the 288-365 ms "+
			"tick this bound exists to remove; if the cap is gone the pre-build "+
			"is the only thing left holding a growing guest's worst tick down, "+
			"and it is best-effort by construction.",
			added, grows, float64(avg)/1024, cap/1024)
	}
	// And the metadata chunks still cover the heap. A bounded increment reaches
	// a 4 MiB slice boundary far more often than a quarter-grow does, so the
	// coverage-crossing round-up in growHeap runs far more often too -- and if
	// the cap were below metaChunkSpans the two rules would fight over every one
	// of those grows.
	wantChunks := (added + f["heap0"] + (4 << 20) - 1) / (4 << 20)
	if f["chunks"] < wantChunks-1 {
		t.Errorf("a %d-byte heap has only %d metadata chunks, want about %d: a "+
			"bounded increment crosses a slice boundary far more often and the "+
			"coverage-crossing round-up has to keep up with it",
			f["heap"], f["chunks"], wantChunks)
	}
	t.Logf("bounded increment: %d grows added %.2f MiB, %.1f KiB each, %d metadata chunks",
		grows, float64(added)/(1<<20), float64(avg)/1024, f["chunks"])
}
