//go:build gc.custom

package fkgc

import "unsafe"

// Enabled reports whether a collector is compiled in. True here by definition:
// this file only exists under -gc=custom.
func Enabled() bool { return true }

// ---------------------------------------------------------------------------
// The phases, and what a step may assume about the world between two of them.
//
// Stage B was one indivisible Collect(). Stage C splits it, and everything that
// is subtle here comes from one question: what is the mutator allowed to have
// done between two steps of the same collection?
//
// THE SAFE-POINT PRECONDITION, which is the whole argument:
//
//	A COLLECTION STEP RUNS ONLY AT AN OUTERMOST DISPATCH BOUNDARY. At such a
//	point the wasm operand stack is empty, the shadow stack is empty (verified
//	two ways in agents/gc.md section 1), and therefore EVERY live reference the
//	mutator holds is either in the guest heap or in [__global_base,
//	__heap_base). There is no third place.
//
// That is what makes a terminate-time barrier sufficient where a real
// incremental collector would need a tricolour one. The mutator cannot hide a
// pointer in a register or a stack slot across a step, so it cannot delete the
// last heap reference to an object and keep it alive privately. Marking
// therefore terminates by looking at the FINAL state of two things:
//
//	1. the root range, re-scanned wholesale (~576 bytes on the churn guest)
//	2. every marked object in every heap page the mutator wrote since the mark
//	   began -- which is exactly what MEMDIRTY's page set records
//
// An object created since the mark began is reachable, at the safe point,
// either from a root, or from a marked object (whose page the store dirtied),
// or from an unmarked object -- which is scanned if and when it is marked, and
// is garbage if it never is. All three are covered.
//
// If a guest ever gains a way to be re-entered mid-collection -- a host callback
// into guest code from inside a step, a scheduler, a coroutine -- this argument
// dies and the barrier's obligations change. The precondition is asserted by
// TestACollectionStepRunsOnlyBetweenOutermostDispatches, not left as a comment.
// ---------------------------------------------------------------------------

const (
	// phaseIdle is no collection in progress: the barrier is disarmed, no
	// on_tick registration exists, and an idle guest pays exactly nothing.
	phaseIdle = 0
	// phaseMark is marking, and it is the ONLY phase the write barrier is
	// armed for.
	phaseMark = 1
	// phaseSweep is sweeping. The mark bitmap is stable -- nothing marks
	// during a sweep -- so sweep needs no barrier at all, which is the
	// property agents/gc.md told stage C to exploit and is why the expensive
	// half is also the cheap half to incrementalize.
	phaseSweep = 2
)

// DirtyAll is the count fk_gc_step is handed when the host cannot say which
// pages were written -- after a save/load cycle, where the page set lived in a
// Lua table that `storage` never carried, or on a synchronous Collect() from
// inside a guest call, where there is no host to ask.
//
// It is not an error. It degrades to a full re-scan of every marked object,
// which is the same recovery gray-stack overflow already uses, and it is
// budgeted and resumable like everything else here.
const DirtyAll = ^uint32(0)

// dirtyCap is how many dirtied page numbers one step can be handed. It is .bss
// like everything else the collector owns, and 256 pages is 1 MiB of dirtied
// heap in a single tick -- far more than an ordinary handler produces. Beyond
// it the count arrives as DirtyAll and the step re-scans everything marked.
const dirtyCap = 256

// pendCap is how many dirtied spans the collector may have PENDING across
// steps, which is a different question from what one step can be handed and
// deserves a different number.
//
// A step's budget goes on whatever it goes on -- a large object's granules, a
// re-scan pass -- and the pages it did not reach are still owed. Four batches'
// worth of headroom, 4 KiB of .bss, covers a guest dirtying at an ordinary rate
// while the collector spends a hundred steps on one big slice, which is the
// shape that was tipping it into a full re-scan every other step. Past this the
// recovery is the full pass, as before.
const pendCap = 4 * dirtyCap

// defaultBudget is one step's work allowance, in GRANULES OF HEAP TOUCHED, and
// the calibration is stage B's measured pause table rather than a guess.
//
//	Stage B, agents/gc.md: a full stop-the-world mark and sweep costs 13.9 to
//	32.8 ms per MiB of heap, and 32.8 is the figure at the sizes where a pause
//	is a problem (20 MiB and 40 MiB heaps).
//
//	1 MiB / 16 B = 65,536 granules, so 32.8 ms/MiB is 0.50 us per granule.
//	A 0.5 ms budget is therefore 0.5 / 0.0005 = 1,000 granules.
//
// 1024 it is, which is 16 KiB of heap per step and agrees with gc.md's own
// arithmetic ("a 0.5 ms step at 33 ms/MiB is about 15 KiB of heap"). At 60 UPS
// that is a sustained ~1 MiB/s of collector throughput.
//
// A granule is charged when it is TOUCHED, which means the same unit prices
// both phases honestly: marking charges the size of an object it scans, and
// sweeping charges the size of a span it walks. The unit is deliberately not
// "objects" or "spans" -- a span of 16-byte slots costs 256 times what a span
// of one 4 KiB object costs, and a budget denominated in spans would be a
// budget that means something different for every size class.
const defaultBudget = 1024

// rootScanMargin is what a termination attempt is given ON TOP of the root
// re-scan it is about to pay for, in granules.
//
// THE ROOT RE-SCAN IS INDIVISIBLE AND THE BUDGET HAS TO ACCOMMODATE IT. A
// termination attempt walks [__global_base, __heap_base) wholesale and charges
// what it walked -- gcm.rootWords>>2 granules -- and a guest chooses that number
// by declaring globals. When it exceeds one step's whole allowance the charge
// saturates to zero, the post-scan check reads "out of budget", and the phase
// defers to a step that will do exactly the same thing. Marking then NEVER
// terminates: nothing is reclaimed, the barrier stays armed, and the only thing
// that ends it is markDeadline -- hundreds of steps later, each having re-walked
// the whole root range for nothing. Measured on examples/gctorture with 390 root
// words (97 granules of charge):
//
//	budget   steps   termination attempts   deadlines
//	  1024       3                      1           0
//	    64     915                    903           1
//	    32   1,222                  1,205           1
//	     8   3,051                  3,014           1
//
// and it gets WORSE as the budget falls, because markDeadline scales as
// heap/budget. Reported from the field by BetterBeltBalancer, whose globals grew
// 104 bytes past its own budget's cliff; the symptom there was a rising
// Deadlines count and a collector that appeared to be working.
//
// WHY THE SCAN IS NOT MADE RESUMABLE INSTEAD, which is the obvious alternative
// and is unsound: the roots live BELOW the heap, and ingestDirty drops every
// dirty page below heapBase, so there is no write barrier over the globals. The
// terminate-time barrier is sufficient only because the root range is read in
// ONE uninterrupted pass at ONE safe point -- see the safe-point precondition at
// the top of this file. A scan resumed across two safe points would read the
// first half at one and the second at the next, and a reference moved from the
// second half to the first in between is a live object swept. So the budget
// yields to the scan and not the other way round.
//
// The margin is what is left after the charge, and it is what makes the attempt
// strictly progressive rather than merely affordable: whatever the scan
// discovers gets drained. 64 granules is 1 KiB of heap and 6% of the default
// budget, so on any guest whose roots fit inside its budget -- which is every
// guest that has ever been measured here at the default -- the floor does not
// bind and nothing changes at all.
const rootScanMargin = 64

// markDeadlineFloor and markDeadlineSlack set how long the mark phase may spend
// yielding to the budget before it stops yielding and finishes.
//
// THIS IS NOT A TUNING PARAMETER, IT IS A LIVELOCK FIX, and the livelock is
// real: it was hit by the gcsave guest at a small budget, which sat in phase 1
// for 120 ticks of a real Factorio and never collected anything.
//
// The mechanism is the one every incremental collector has and is worth stating
// plainly. Each step first re-scans the marked objects in every page the mutator
// dirtied since the last one, and only then tries to terminate. Re-scanning one
// dirtied 4 KiB page costs about a span's walk, so a guest that dirties a page
// per tick against a budget smaller than that spends every step on the backlog
// and never reaches the termination attempt. Marking then runs forever, the
// write barrier stays armed forever, and nothing is ever reclaimed. There is no
// error and no pause; the heap grows exactly as if there were no collector,
// which is the worst failure available to a guest that opted in to one.
//
// The escape is a forward-progress guarantee rather than a bigger budget,
// because "big enough" depends on the guest's write rate and the guest chooses
// that. After the deadline the mark phase finishes in one unbudgeted step, which
// IS a pause -- bounded by the live set plus the outstanding dirty set, i.e. the
// MARK half of a stage-B pause on a heap whose sweep (the expensive half, per
// agents/gc.md) is still paced.
//
// The deadline SCALES WITH THE HEAP, and a flat step count was wrong for a
// reason worth keeping: a legitimately slow mark -- a big live set at a small
// budget -- is not a livelock, and a flat 600 steps fired on a gctorture heap
// that was converging perfectly well. What is actually being detected is a mark
// that has taken several times longer than scanning the whole heap once would
// have, which is only possible if it is going backwards.
//
//	limit = markDeadlineSlack * (heap granules / budget) + markDeadlineFloor
//
// The floor keeps a tiny heap from tripping it on fixed per-step costs alone.
const (
	markDeadlineSlack = 4
	markDeadlineFloor = 600

	// THE SECOND DEADLINE, and it is the one that fires on a guest whose dirty
	// rate is over its budget. The step-count deadline above detects a mark
	// taking several times longer than one pass over the heap should; on a
	// mutator that outruns the collector it works far too late (4 x heap/budget
	// steps of allocating flat out is tens of megabytes that never come back),
	// and on examples/gcsave in a real Factorio it is 664 steps away while the
	// run is 140.
	//
	// WHAT IT MEASURES IS NET SHRINKAGE OF THE WORK STILL OWED, over a window,
	// and every part of that sentence was got wrong once first:
	//
	//   - Not "the record of what changed was lost". That counter reads ZERO for
	//     the whole of gcsave's livelock, because nothing is ever lost: the
	//     pending list absorbs every page and the collector simply never
	//     empties it. An escape keyed on losses cannot see a guest that is
	//     merely too fast.
	//   - Not "did this step mark anything new". A mutator that allocates every
	//     tick has new objects marked every step -- the mark is chasing a moving
	//     target, and counting that as progress is counting the target's motion
	//     as the chase closing.
	//   - Not step-to-step. workOwed oscillates: it falls whenever a step drains
	//     more than the mutator added and rises otherwise, so a consecutive-step
	//     test resets constantly on a guest that is not converging at all.
	//     gcsave's owed reads 673, 721, 757, 1061, 841, 877 over fifty ticks --
	//     no trend and no progress.
	//
	//   - And not one scalar. The owed work has TWO OWNERS and they have to be
	//     asked separately, which is the whole shape of the answer:
	//
	//         SCAN work -- the gray stack, the resumable object scan, the
	//         remainder of a full re-scan pass. Only the COLLECTOR adds to it,
	//         and it consumes it monotonically.
	//
	//         DIRTY work -- spans the mutator wrote and the collector owes a
	//         re-scan. Only the MUTATOR adds to it.
	//
	//     A legitimately slow mark is scan-dominated: a guest with a 40 MiB live
	//     set spends sixty consecutive steps inside one 1 MiB object, and its
	//     pending dirty list is non-empty that whole time because the budget is
	//     going somewhere useful. A livelock is dirty-dominated: gcsave's scan
	//     work is nil -- the live set is tiny and marked in a handful of steps --
	//     and every step afterwards re-scans pages the mutator dirties faster
	//     than the budget drains them.
	//
	// So a window is STALLED when both are true: the pending dirty list did not
	// reach empty at any step in it, AND the scan work did not shrink across
	// it. Neither alone is a livelock; together they are exactly "no net
	// shrinkage of (unmarked + dirty)", and nothing else in the collector can
	// produce them. After markStallLimit consecutive stalled windows the phase
	// stops yielding.
	//
	// Verified in game on both sides: gcsave escapes and completes collections,
	// pacedhuge (a 40 MiB live set at the same budget) never stalls a window.
	markStallWindow = 8
	markStallLimit  = 4
)

// markDeadline computes this collection's limit, ONCE, at the moment it starts.
//
// Fixed at the start rather than recomputed per step, and that is the second
// thing a flat count got wrong: a livelocked collector's heap GROWS -- nothing
// is being reclaimed -- so a limit derived from the current heap grows with it
// and the deadline recedes exactly when it is needed. The size that matters is
// the one the mark set out to cover.
func markDeadline() uint32 {
	b := gcm.budget
	if b == 0 {
		b = defaultBudget
	}
	return markDeadlineSlack*((gcm.spanCount<<(spanLog-granuleLog))/b) + markDeadlineFloor
}

// sweepAheadUnits is how much sweeping an ALLOCATION does when it finds no free
// span and the sweep has not reached one yet. See allocSpans: this is the lazy
// half of the sweep, and it exists so that a mutator outrunning the paced sweep
// sweeps for itself instead of growing the heap past free space it has not
// looked at. Four spans, i.e. one paced step's worth.
const sweepAheadUnits = 1024

// SetThreshold sets how many bytes may be handed out between collections
// before CollectIfNeeded says yes. Zero restores the default.
//
// CALLING IT FROM init() IS SUPPORTED AND USED TO BE SILENTLY DISCARDED. The
// collector brings the heap up in initialize(), which assigned both knobs their
// defaults unconditionally -- and on -target=wasm-unknown that runs AFTER a
// guest's package initialisers, whatever runtime_wasmentry.go reads like. It
// latches a non-zero value now; see the comment on those two lines in heap.go
// and examples/gcconfig, which is the guest that measures it.
func SetThreshold(bytes uint32) {
	if bytes == 0 {
		bytes = defaultThreshold
	}
	gcm.threshold = bytes
}

// SetBudget sets one step's work allowance in granules of heap touched. Zero
// restores the default, which is calibrated to ~0.5 ms on stage B's numbers --
// see defaultBudget for the arithmetic.
//
// This is the pacing knob. Raising it collects faster and pauses longer, in a
// straight line: the budget IS the pause, because a step charges every granule
// it touches. Lowering it below a few hundred is not useful -- the fixed cost
// of a step (a dispatch, a root re-scan, the dirty drain) stops being small
// next to the work it does.
//
// THE ~0.5 ms CALIBRATION IS A HOST-SIDE NUMBER AND IT UNDER-STATES THE GAME,
// always in that direction, by between 1.2x and 65x depending on the heap. Two
// independent in-game measurements, both recorded in agents/gc.md's stage-D
// section:
//
//	guest              heap     one step at 1024 granules
//	the first real mod  1.5 MB   623 us median, 1.30 ms p90, 2.1 ms worst
//	examples/gcbench    2.8 MB   0.10 ms median, 1.2 ms p90, 32.6 ms worst
//
// The MEDIAN tracks the calibration and the TAIL does not, and the tail is what
// a budget is chosen for. Read 1024 as roughly half a millisecond of median and
// single- to double-digit milliseconds of worst tick, and re-measure in game
// before believing any particular figure -- there is no clock in the sandbox,
// so nothing on this side can check itself.
//
// AND THE BUDGET HAS TWO FLOORS THAT ARE NOT ABOUT PAUSE LENGTH AT ALL. They
// produce the SAME SYMPTOM -- Phase() stuck at 1, Stats().Deadlines rising,
// nothing reclaimed -- from opposite causes, and telling them apart is the whole
// of diagnosing a collector that appears wired and does nothing.
//
//  1. THE DIRTY RATE, which this knob does fix. Every step re-scans the pages
//     the mutator dirtied since the last one BEFORE it attempts to terminate, so
//     a budget under the guest's own per-tick dirty rate spends itself entirely
//     on the backlog and never reaches the attempt. examples/gcbench sat in that
//     state for 600 straight ticks in a real Factorio.
//
//  2. THE ROOT SET, which this knob does NOT fix and which the collector now
//     fixes itself. A termination attempt walks [__global_base, __heap_base) in
//     one indivisible pass and charges what it walked, so a budget under
//     rootScanCost() could never terminate a mark at any allocation rate,
//     including zero. EffectiveBudget() floors it and the collector logs one
//     `fkgc:` line saying so. THIS PARAGRAPH USED TO BE MISSING and its absence
//     sent the first downstream mod's whole investigation at the allocation
//     rate for a day; see rootScanMargin for the measurement.
//
// So: if Deadlines rises, compare EffectiveBudget() against Budget() FIRST. Equal
// means cause 1 and this is the knob. Larger means cause 2, the collector has
// already applied the floor, and what is left to decide is whether the pause it
// implies is one this guest wants -- which is a question about how many
// package-level variables it declares, not about how fast it allocates.
//
// AND READ THE SPLIT BEFORE READING A CAUSE INTO THE TOTAL. Deadlines is
// StepEscapes + StallEscapes, and only the second of those says "not
// converging"; the first is the far-out backstop and fires for a mark that is
// affordable but slow as well. Attributing a bare Deadlines count to the
// allocation rate is exactly the mistake this comment used to invite -- twice,
// downstream, once for a day. See MemStats.StepEscapes.
//
// markDeadline escapes both and is deliberately far enough out that a short run
// finishes first, so a test that passes is not evidence that neither is present.
func SetBudget(units uint32) {
	if units == 0 {
		units = defaultBudget
	}
	gcm.budget = units
}

// Budget reports the per-step work allowance the guest ASKED for.
//
// It is not necessarily what a mark step spends -- see EffectiveBudget, which is
// the same number floored so that a termination attempt can pay for its own root
// re-scan.
func Budget() uint32 { return gcm.budget }

// rootScanCost is what the next termination attempt's wholesale root re-scan
// will charge, in granules, from the last scan that actually happened.
//
// The number is stable across the attempts of one collection and across
// collections: it is the size of [__global_base, __heap_base) less gcMeta, which
// is fixed at link time, plus the shadow stack, which is empty at every safe
// point where a step may run. startCollection primes it before the first
// markStep, so it is never read as a stale zero except on a heap that has not
// collected yet -- where there is no attempt to pay for either.
func rootScanCost() uint32 { return gcm.rootWords >> 2 }

// EffectiveBudget is what one mark step actually spends: Budget(), floored at
// what a termination attempt costs plus rootScanMargin.
//
// A guest whose globals are large enough for this to exceed Budget() cannot have
// the pause it asked for, and this is the number that says so. See
// rootScanMargin for the failure it replaced -- which was silent, and was not a
// longer pause but no collection at all.
func EffectiveBudget() uint32 {
	if fl := rootScanCost() + rootScanMargin; gcm.budget < fl {
		return fl
	}
	return gcm.budget
}

// Phase reports what the collector is doing: 0 idle, 1 marking, 2 sweeping.
func Phase() uint32 { return uint32(gcm.phase) }

// CollectIfNeeded starts a PACED collection if the guest has taken more heap
// than the threshold since the last one, and reports whether it did.
//
// This is the call a guest puts in fk_on_tick, and since stage C it starts a
// collection rather than performing one. It returns after the initial root
// scan; the rest is driven from a one-shot on_tick registered by the host, one
// bounded step per tick, until the collection finishes and the registration is
// torn down again. An idle guest is back to zero registrations and zero cost,
// which is the stage-A property this feature was given a GO on.
//
// Pacing is by heap pressure and by tick count because there is no clock in the
// sandbox to be tempted by, and because collectgarbage("count") is a HOST
// memory number that differs between machines and must never reach a decision
// the simulation can see.
func CollectIfNeeded() bool {
	if gcm.sinceGC < gcm.threshold {
		return false
	}
	return Start()
}

// Start begins a paced collection now, whatever the heap pressure is, and
// reports whether one began. It is what CollectIfNeeded does once its threshold
// says yes, exported separately for a guest that has its own idea of when a
// collection is due -- and for the tests, which need a collection to start on
// demand rather than when a size class happens to want a fresh span.
//
// It is a no-op while one is already in progress: two collections cannot be in
// flight at once, and starting one on top of another would throw away the mark
// state the first has built.
func Start() bool {
	if !inited || gcm.phase != phaseIdle {
		return false
	}
	// ARM BEFORE STARTING, and the order is load-bearing rather than tidy.
	// Arming early can only over-record: a page dirtied before the mark began
	// is a page re-scanned for nothing. Arming late loses writes. The host call
	// also registers the one-shot on_tick that will drive the steps, which is
	// the fk.defer machinery with a different payload exactly as agents/gc.md
	// section 4 predicted.
	hostGCPace()
	startCollection()
	return true
}

// Collect runs one complete mark and sweep SYNCHRONOUSLY and returns when the
// heap is swept. It is the stop-the-world path stage B shipped, kept for two
// callers and no others.
//
// The first is the safety valve in allocSpans: the heap cap is hard, so a guest
// that opted in and then outran its own pacing must collect where it stands
// rather than trap with a bare `unreachable`. That collection lands inside an
// event handler, which is exactly the pause this feature exists to avoid -- it
// is the difference between a pause and a dead mod, not the pacing mechanism.
//
// The second is a guest or a test that wants a full cycle now.
//
// It finishes whatever is in flight rather than starting a second collection on
// top of one, and it owes a full re-scan while doing so: the dirty page set
// lives in a Lua table on the other side of the boundary, and there is no host
// to ask from in here. That is DirtyAll, and it is why this is the expensive
// path as well as the pausing one.
//
// ONE THING IT CANNOT DO is disarm the barrier, which is a chunk local the host
// owns. After a synchronous Collect the guest keeps paying the armed store cost
// until the next scheduled step runs and reports phase 0. That is a few percent
// for at most one tick, and it is a cost of the valve rather than of the design.
func Collect() {
	if !inited || gcm.collecting {
		return
	}
	if gcm.phase == phaseIdle {
		startCollection()
	}
	oweRescan()
	for gcm.phase != phaseIdle {
		step(^uint32(0), 0)
	}
}

// startCollection opens a mark phase. It does NOT scan the whole heap: the
// initial root scan is the ~576-byte globals range plus whatever the shadow
// stack holds, which between dispatches is nothing.
func startCollection() {
	gcm.grayTop = 0
	gcm.grayOvf = false
	gcm.rescanOwed = false
	gcm.rescanCursor = 0
	clearPending()
	gcm.partialBase = 0
	gcm.partialOff = 0
	gcm.partialEnd = 0
	gcm.steps = 0
	gcm.markLimit = markDeadline()
	gcm.maxWork = 0
	gcm.totalWork = 0
	gcm.maxUnpacedWork = 0
	gcm.unpacedWork = 0
	gcm.callWork = 0
	gcm.terminations = 0
	gcm.marked = 0
	gcm.stalls = 0
	gcm.maxStalls = 0
	gcm.stallSteps = 0
	gcm.owedMark = 0
	gcm.pendEmptied = false
	gcm.pendEmpties = 0
	gcm.markForced = false
	gcm.rescanRestarts = 0
	gcm.outruns = 0
	// No bitmap wipe: see the note where clearMarkBits used to be. Every mark
	// bit over the covered heap is already zero at phaseIdle.
	gcm.phase = phaseMark
	gcm.collecting = true
	gcm.rootWords = 0
	gcMarkReachable()
	// The initial root scan is charged like the terminating one, and for the
	// same reason: it is unbudgeted work in a tick a guest did not schedule.
	// It is small -- the globals range less gcMeta, ~576 B on the churn guest
	// -- but "small" was an assumption for two stages and nothing measured it.
	gcm.stepWork = 0
	charge(0, gcm.rootWords<<2)
	gcm.collecting = false
	endUnpaced()
}

// endUnpaced closes off a burst of collector work done INSIDE A GUEST CALL --
// the sweep-ahead in allocSpans, the initial root scan, the last-resort
// Collect() -- and folds it into the counters a guest can see.
//
// It exists because step() zeroes stepWork on entry, so before this every
// granule charged between two steps was silently discarded. That is why the
// host-side gate could report a 1.17x worst step while the game showed 65x: the
// two were not measuring the same work.
func endUnpaced() {
	w := gcm.stepWork
	if w == 0 {
		return
	}
	gcm.stepWork = 0
	gcm.callWork += w
	gcm.callFolds++
	gcm.unpacedWork += w
	if gcm.callWork > gcm.maxUnpacedWork {
		gcm.maxUnpacedWork = gcm.callWork
		gcm.maxUnpacedFolds = gcm.callFolds
	}
}

// Step performs one bounded unit of collection and returns the phase the
// collector is in afterwards -- 0 idle, 1 marking, 2 sweeping.
//
// ndirty is how many page numbers the host wrote into the buffer at
// DirtyBase(), or DirtyAll when it cannot say. The host reads the return value
// to decide whether to keep the barrier armed (1), disarm it (0 or 2), and
// whether to schedule another step (anything but 0).
func Step(ndirty uint32) uint32 {
	return uint32(step(gcm.budget, ndirty))
}

func step(budget, ndirty uint32) uint8 {
	if !inited || gcm.collecting || gcm.phase == phaseIdle {
		return gcm.phase
	}
	gcm.collecting = true
	gcm.steps++
	// Whatever the mutator's own calls charged since the last step is closed
	// off and attributed to them, not to this step. callWork resets HERE: it
	// measures one gap between two steps, which for a paced collection is one
	// tick, and that is the number a worst-tick claim is made of.
	endUnpaced()
	gcm.callWork = 0
	gcm.callFolds = 0
	gcm.stepWork = 0
	if gcm.phase == phaseMark {
		if gcm.steps > gcm.markLimit || gcm.stalls >= markStallLimit {
			// The deadline. See markDeadline and markStallWindow: yielding to
			// the budget has stopped making progress toward termination, so
			// this step stops yielding. It terminates because marks are
			// monotone -- an unbudgeted pass drains the gray stack, the dirty
			// set and the roots, and nothing can add to them while it runs.
			//
			// THE CAUSE IS RECORDED HERE AND NOWHERE ELSE, because here is the
			// only place both conditions are still in hand -- one step later
			// the latch remembers only THAT it fired. The two are separate
			// counters because they are separate diagnoses: see
			// MemStats.StepEscapes. The stall wins a tie, and the tie-break is
			// not arbitrary -- markLimit is deliberately far out (it scales as
			// heap/budget and floors at 600 steps) so that a short run finishes
			// first, while the stall window fires within a few dozen steps of
			// the mark actually ceasing to converge. If both are true, the
			// collector stopped converging long ago and the step count merely
			// caught up.
			if !gcm.markForced {
				if gcm.stalls >= markStallLimit {
					gcm.stallEscapes++
				} else {
					gcm.stepEscapes++
				}
			}
			//
			// IT LATCHES, and that is not a detail. One unbudgeted step is not
			// enough: an unbudgeted step finishes the re-scan pass, which resets
			// the restart counter, which turns the escape off again -- and the
			// very same step's final drainGray can overflow the gray stack and
			// owe a fresh pass, so the collector drops back to a budget that has
			// already been shown not to converge. Measured on gctorture at 8,000
			// nodes and a 256-granule budget: six escapes over sixty steps, zero
			// termination attempts, and a heap climbing 1.4 -> 4.2 MB. Latched,
			// the same run terminates within a few steps of the first escape.
			gcm.markForced = true
		}
		if gcm.markForced {
			// FINISHES THE PHASE, not merely one step of it, and the loop is
			// what makes "stop yielding" mean what agents/gc.md says it means.
			//
			// One unbudgeted markStep does not terminate marking, and the
			// reason is structural rather than incidental: markStep's last act
			// before the termination attempt is a drainGray, and a drainGray
			// that overflows the gray stack owes a fresh full re-scan -- so the
			// step ends with rescanOwed set and the phase still marking. Under a
			// budget that is correct and the next step continues; under a
			// deadline that is supposed to END the phase it is an escape that
			// escapes nothing, which is what a run of gctorture at 8,000 nodes
			// and a 256-granule budget showed: forty unbudgeted steps, zero
			// termination attempts, and a heap still climbing.
			//
			// It terminates because MARKS ARE MONOTONE and the mutator is not
			// running: every pass either marks something new -- strictly
			// reducing a finite set -- or finds nothing and ends the phase.
			gcm.deadlines++
			for gcm.phase == phaseMark {
				markStep(^uint32(0), ndirty)
				ndirty = 0
			}
			// THE ESCAPE FINISHES THE MARK PHASE AND NOTHING ELSE. The
			// unbudgeted allowance must not be carried into the sweep by the
			// fall-through below: the sweep is the EXPENSIVE half, it needs no
			// barrier, and keeping it paced is the whole of stage C's design.
			// Carried, it swept the entire heap in the same step -- which on
			// examples/gcsave finished a collection inside one tick and made the
			// roundtrip leg's "save landed mid-sweep" case unreachable, because
			// there was no sweep left to land in.
			budget = gcm.budget
		} else {
			budget = markStep(budget, ndirty)
		}
		// THE FORWARD-PROGRESS METRIC, sampled over a window. See
		// markStallWindow.
		if gcm.phase == phaseMark {
			if gcm.dirtyCursor >= gcm.dirtyN {
				gcm.pendEmptied = true
				gcm.pendEmpties++
			}
			gcm.stallSteps++
			if gcm.stallSteps >= markStallWindow {
				gcm.stallSteps = 0
				w := scanOwed()
				if !gcm.pendEmptied && gcm.steps > markStallWindow && w >= gcm.owedMark {
					gcm.stalls++
					if gcm.stalls > gcm.maxStalls {
						gcm.maxStalls = gcm.stalls
					}
				} else {
					gcm.stalls = 0
				}
				gcm.owedMark = w
				gcm.pendEmptied = false
			}
		}
	}
	// Falling straight into the sweep when marking terminated with budget left
	// is not an optimisation, it is what keeps a step's cost bounded by the
	// budget rather than by the budget plus a whole phase transition.
	if gcm.phase == phaseSweep && budget > 0 {
		sweepStep(budget)
	}
	if gcm.stepWork > gcm.maxWork {
		gcm.maxWork = gcm.stepWork
	}
	gcm.totalWork += gcm.stepWork
	// ZEROED ON THE WAY OUT, not only on the way in. stepWork is the shared
	// accumulator charge() writes to, and leaving a paced step's total sitting
	// in it meant the next endUnpaced -- a sweep-ahead bite inside some later
	// guest call -- folded THIS step's work into that call's. In game that read
	// as a 131x in-call burst on an arm whose worst paced step was 1.08x.
	gcm.stepWork = 0
	gcm.collecting = false
	return gcm.phase
}

// MaxStepWork is the most any single step of the current (or last) collection
// charged, in granules of heap touched. A guest logging this against Budget()
// is logging whether the pacing is actually bounding anything -- see
// TestNoPacedStepOverrunsItsBudget for what it is allowed to exceed and why.
func MaxStepWork() uint32 { return gcm.maxWork }

// TotalWork is what the current (or last) collection charged in total, in
// granules of heap touched. MaxStepWork over TotalWork is the fraction of a
// whole collection that lands in the worst single tick, which is the ratio the
// stage-C acceptance gate is stated in -- there is no clock in the sandbox, so
// a pause is DERIVED from a measured whole-collection time and this fraction
// rather than sampled.
func TotalWork() uint32 { return gcm.totalWork }

// ---------------------------------------------------------------------------
// Marking
// ---------------------------------------------------------------------------

// markStep drains gray work under a budget and terminates the mark phase when
// there is nothing left to do. It returns the budget it did not spend.
//
// TERMINATION IS ATTEMPTED ONLY WITH BUDGET IN HAND, and only when three
// queues are simultaneously empty: the gray stack, the pending dirty spans, and
// the full-rescan pass. A step that runs out of budget defers, and the next one
// tries again. That the loop terminates at all is the same argument stage B
// made for gray overflow: marks are monotone and there are finitely many
// objects, so an attempt that finds nothing new is an attempt that ends the
// phase, and an attempt that finds something new has strictly reduced what is
// left to find.
func markStep(budget, ndirty uint32) uint32 {
	// THE RESERVATION, and it is the first thing that happens because everything
	// below spends against what is left after it.
	//
	// A termination attempt re-scans the roots wholesale and charges what it
	// walked, and that cost is INDIVISIBLE (see rootScanMargin). Two separate
	// things follow, and the first cut of this fix did only the first:
	//
	//  1. a step whose WHOLE allowance is smaller than the scan can never
	//     terminate the phase at all -- so the allowance is floored;
	//  2. a step that arrives at the attempt having already SPENT its allowance
	//     on the dirty queue cannot pay for the scan either -- so the scan's
	//     cost is held back rather than left to compete with the queues.
	//
	// Doing (1) alone made the guard `budget <= rootScanCost()` bite on a guest
	// with a high dirty rate and large roots: the queues took the budget every
	// step, the attempt was deferred every step, and the mark never terminated
	// -- which is the livelock this was meant to remove, arrived at from the
	// other side. examples/gcsave under Rust showed it as terms=0 with phase
	// stuck at 1 across 300 ticks.
	//
	// Reserving keeps both properties at once: the attempt is always affordable
	// when the queues are empty, and no step spends more than its budget,
	// because the reserve is part of it rather than added to it. The deadline
	// path calls in with ^uint32(0), where this is a no-op.
	reserve := rootScanCost()
	if fl := reserve + rootScanMargin; budget < fl {
		budget = fl
		warnRootBudget()
	}
	budget -= reserve
	ingestDirty(ndirty)
	// The one in-flight object scan first, because only one may exist at a time
	// and both of the next two want to start one.
	budget = finishPartial(budget)
	// THE DIRTY SPANS COME BEFORE THE GRAY STACK, and that order is a fix
	// rather than a preference.
	//
	// The dirty page numbers live in a fixed buffer the HOST OVERWRITES at every
	// step, so a batch that is not drained in the step it arrived in is GONE --
	// ingestDirty can only degrade to "assume everything changed" and owe a full
	// re-scan. With the gray stack drained first, a step that spent its whole
	// allowance on one large object reached drainDirtySpans with nothing left,
	// and so owed a full pass; measured on gctorture at the default budget, that
	// happened on 876 of 1,752 steps, and each owed pass restarts from span
	// zero. Total work: 2.19 million granules for a 4.5 MiB heap, i.e. eight
	// passes over it, against one.
	//
	// Draining them first costs the gray stack nothing it does not get back on
	// the next step -- marks are monotone and the gray stack is not lost when a
	// step ends -- while the dirty batch is lost. Spend the perishable one.
	budget = drainDirtySpans(budget)
	budget = drainGray(budget)
	budget = runRescan(budget)
	budget = drainGray(budget)
	// THE QUEUES ARE SPENT; THE RESERVE IS NOT. `budget` here is what is left of
	// the queue allowance, and it is deliberately NOT tested against the scan's
	// cost: the scan is paid for out of the reserve held back above, which is
	// why an attempt is affordable on every step that gets this far. The
	// `budget == 0` term stays because a step that ran out mid-queue has not
	// finished draining and its queue predicates would be answering about a
	// partial pass.
	if budget == 0 || gcm.grayTop != 0 || gcm.grayOvf || gcm.partialBase != 0 ||
		gcm.rescanOwed || gcm.dirtyCursor < gcm.dirtyN {
		return budget + reserve
	}
	budget += reserve
	// The termination attempt: re-scan the roots wholesale and see whether
	// anything falls out. The mutator has run since the last time they were
	// looked at, and the safe-point precondition says the roots plus the dirty
	// pages are the only two places a new reference can be.
	//
	// IT IS CHARGED NOW, and that is agents/gc.md's open item 5. The scan was
	// free by omission for three stages: markStep charged the objects the scan
	// DISCOVERED, through drainGray, and never the range it walked. That range
	// is the guest's whole globals section less gcMeta -- a number the guest
	// chooses and nothing here bounds -- and the residual it explains is the
	// part of TestNoPacedStepOverrunsItsBudget's 1.17x that grows with a guest.
	gcm.terminations++
	gcm.rootWords = 0
	gcMarkReachable()
	budget = charge(budget, gcm.rootWords<<2)
	budget = drainGray(budget)
	// NO `budget == 0` TERM HERE, and its absence is the fix rather than a
	// tidy-up. The scan above has already happened -- charge() is post-hoc
	// accounting for a walk that is complete -- so "out of budget" says nothing
	// about whether marking is done. The four predicates that remain say it
	// exactly: an empty gray stack, no overflow, no object half-scanned and no
	// re-scan owed IS a finished mark, at any budget. Keeping the term meant a
	// guest whose roots cost more than its budget deferred forever, having done
	// all of the work and banked none of it.
	if gcm.grayTop != 0 || gcm.grayOvf || gcm.partialBase != 0 || gcm.rescanOwed {
		return budget
	}
	beginSweep()
	return budget
}

// ingestDirty accepts the page numbers the host wrote into gcm.dirtyQ.
//
// A page and a span are the same object -- spanBytes is 4096 because
// --persist=packed's page size is 4096, which heap.go calls out as deliberate
// -- so a page number converts to a span index by subtraction and nothing else.
// Pages below the heap are the guest's statics and the collector's own .bss;
// they are re-scanned wholesale as roots at every termination attempt, so a
// dirty record for one is dropped here rather than tracked.
func ingestDirty(n uint32) {
	if n == 0 {
		return
	}
	if n == DirtyAll || n > dirtyCap {
		gcm.dirtyOverflows++
		oweRescan()
		clearPending()
		return
	}
	// THE PENDING LIST SURVIVES THE STEP, AND THE LANDING PAD DOES NOT.
	//
	// dirtyQ is where the HOST writes, and it overwrites it at every step -- so
	// a batch not fully drained in the step it arrived in used to be simply
	// LOST, and the only recovery was "assume everything changed", i.e. a full
	// re-scan of the heap from span zero. That put a recovery path on the NORMAL
	// path: any step whose budget went on a large object reached here with an
	// undrained batch. Measured on gctorture at the default budget, 876 of 1,752
	// steps owed a pass -- eight passes over a 4.5 MiB heap for one collection,
	// and a mark that only ended at the deadline.
	//
	// So the batch is copied off the landing pad into a list of the collector's
	// own, four times the size, drained over as many steps as it takes. The full
	// re-scan comes back only when THAT fills, and a guest filling 1,024 pending
	// spans faster than its budget drains them is precisely the storm the
	// recovery is for.
	if gcm.dirtyN+n > pendCap {
		r := gcm.dirtyN - gcm.dirtyCursor
		for i := uint32(0); i < r; i++ {
			gcm.pend[i] = gcm.pend[gcm.dirtyCursor+i]
		}
		gcm.dirtyN = r
		gcm.dirtyCursor = 0
	}
	// AND IT MUST NOT BECOME PERMANENT, which is the other half and cost a real
	// Factorio 140 ticks in phase 1.
	//
	// Termination waits on `dirtyCursor < dirtyN`, so a backlog that never
	// drains is a mark that never ends -- and a guest dirtying more pages per
	// tick than its budget re-scans has exactly that. examples/gcsave writes
	// most of a 128 KiB heap every tick against a 512-granule budget (two spans
	// a step) on purpose. Holding those pages faithfully is correct and useless:
	// the collector cannot catch up, and the recovery it should have taken --
	// one full re-scan, which costs O(heap) ONCE instead of O(dirty) forever --
	// is the thing the list was postponing.
	//
	// So the list is a buffer for a TRANSIENT stall (a step whose budget went on
	// one large object) and not a queue for a guest that is over its rate. Half
	// of it is the line: past that, degrade the way everything else here
	// degrades.
	if gcm.dirtyN+n > pendCap || gcm.dirtyN-gcm.dirtyCursor > pendCap/2 {
		gcm.dirtyOverflows++
		oweRescan()
		clearPending()
		return
	}
	// THE LIST IS A SET, and this is where it becomes one. A page whose span is
	// already pending is dropped; so is one below the heap, above the coverage
	// line, unassigned, or holding the collector's own metadata -- none of those
	// can hold a marked object, so re-scanning them was always a no-op that
	// occupied a slot. See clsPending.
	base := heapBase >> spanLog
	covered := coveredSpans()
	for i := uint32(0); i < n; i++ {
		p := gcm.dirtyQ[i]
		if p < base {
			continue
		}
		si := p - base
		if si >= covered {
			continue
		}
		c := spanClassOf(si)
		if c&clsPending != 0 || c&^clsFlags == 0 || c&^clsFlags == clsMeta {
			continue
		}
		setSpanClass(si, c|clsPending)
		gcm.pend[gcm.dirtyN] = si
		gcm.dirtyN++
	}
}

// drainDirtySpans re-scans every marked object in each dirtied span.
//
// Re-scanning a MARKED object is the whole of the barrier's obligation. An
// unmarked object needs nothing: if it is ever marked it is scanned then, and
// if it never is, it is garbage. A marked one may have been scanned already,
// which is what makes a store into it invisible without this.
//
// THE COLLECTOR'S OWN METADATA IS DROPPED HERE, and since the metadata moved
// into the heap that is worth stating plainly. Marking writes mark bits, mark
// bits are heap words now, and every one of those writes goes through the same
// store funnel the guest's do -- so a card for a metadata page arrives in this
// queue like any other. rescanSpan resolves it to clsMeta and returns without
// touching anything, which is the same one compare a sub-heap card costs above.
// That is the whole answer to agents/gc.md section 6's hazard, and it is why
// static placement was buying a bound rather than a property.
func drainDirtySpans(budget uint32) uint32 {
	for gcm.dirtyCursor < gcm.dirtyN {
		if budget == 0 {
			return 0
		}
		// SPAN INDICES, already filtered and deduplicated by ingestDirty.
		si := gcm.pend[gcm.dirtyCursor]
		gcm.dirtyCursor++
		if c := spanClassOf(si); c&clsPending != 0 {
			setSpanClass(si, c&^clsPending)
		}
		// NO LARGE-OBJECT DEDUP HERE ANY MORE. It existed because a re-scan
		// resolved a continuation span to its head and re-read the whole run,
		// so several dirtied pages of one object were several whole re-scans.
		// A re-scan is one SPAN now, so every dirtied page has its own bytes to
		// re-read and skipping one would skip the store that dirtied it.
		budget = rescanSpan(si, budget)
		if gcm.grayTop > grayCap/2 {
			budget = drainGray(budget) // see runRescan
		}
	}
	return budget
}

// oweRescan says "the record of what changed was lost; assume everything did",
// AND RESTARTS THE PASS FROM SPAN ZERO. The restart is the whole of it.
//
// THIS WAS A REAL DEFECT AND IT SURVIVED THREE STAGES, hidden behind the heap
// cap. rescanOwed is resumable through rescanCursor, and drainGray's overflow
// path always reset the cursor -- but ingestDirty and Collect asserted the flag
// and left the cursor where it stood. A pass already halfway up the heap then
// declared itself COMPLETE without ever revisiting the spans below the cursor,
// so a store into a marked object down there, made after the cursor had gone
// past, was never re-scanned. Marking terminated, the sweep freed a live object,
// and the only symptom was a checksum.
//
// Why it was not reachable before: a guest that dirties pages faster than its
// budget re-scans them used to hit the 16 MiB cap within a few hundred steps,
// and the valve's synchronous Collect() forced an unbudgeted pass that started
// from zero. Removing the cap removed the accident that was covering this.
//
// A restart is O(heap) and a guest in this state is already paying O(heap) per
// pass; what it must not do is finish a pass that did not cover everything.
func oweRescan() {
	// A LOSS WHILE A PASS IS ALREADY RUNNING IS THE LIVELOCK SIGNAL, and it is a
	// far sharper one than a step count. It says the mutator is losing the
	// record faster than the collector can rebuild it, which is precisely the
	// condition markDeadline exists to escape -- and it says so within a few
	// steps rather than after 4 x heap/budget of them, which on a storm is tens
	// of megabytes of growth later. See markStallWindow.
	gcm.rescanRestarts++
	gcm.rescanOwed = true
	gcm.rescanCursor = 0
}

// runRescan is the resumable full pass: every marked object in the heap, in
// span order, re-scanned. It is the recovery for gray-stack overflow, for a
// dirty record that did not fit, and for a collection resumed after a save --
// three different ways of saying "the record of what changed was lost, so
// assume everything did".
//
// Resumable rather than atomic because a full pass is O(heap) and this is the
// one place where an unbudgeted fallback would put a stage-B-sized pause back
// into a paced collector. gcm.rescanCursor is what makes it a step like any
// other, and it is in linear memory like everything else, so it survives a save.
func runRescan(budget uint32) uint32 {
	if !gcm.rescanOwed {
		return budget
	}
	covered := coveredSpans()
	for gcm.rescanCursor < covered {
		if budget == 0 {
			return 0
		}
		si := gcm.rescanCursor
		gcm.rescanCursor++
		// A CONTINUATION SPAN IS NO LONGER SKIPPED, and the rule it replaces is
		// worth keeping in view. rescanSpan used to resolve a continuation to
		// its head and re-read the whole run, so a pass over every span in order
		// met an n-span object n times and re-scanned it n times -- O(size^2),
		// which cost 400 full re-scans of one 1.6 MiB object and 39.6 million
		// granules on a 7 MiB heap. Skipping continuations patched that. Now a
		// re-scan is one SPAN, so the pass covers a run exactly once by walking
		// it, and the skip would leave every continuation unread.
		budget = rescanSpan(si, budget)
		// DRAINED AS THE PASS GOES, and leaving that out is what stopped a big
		// heap converging at all.
		//
		// A re-scan pushes every newly discovered object onto the gray stack,
		// and markStep only drains BETWEEN its phases -- so a pass over
		// thousands of spans accumulated hundreds of thousands of entries
		// against a 4,096-deep stack, overflowed, and owed itself a fresh pass
		// from span zero. Measured in a real Factorio on a 40 MiB heap: 5,900
		// ticks in phase 1 with cycles=0, a bounded worst step the whole time,
		// and no deadline because the step-count deadline is 10,840 steps at
		// that size. The pass was restarting faster than it could finish.
		if gcm.grayTop > grayCap/2 {
			budget = drainGray(budget)
		}
	}
	gcm.rescanOwed = false
	gcm.rescanCursor = 0
	gcm.rescans++
	return budget
}

// rescanSpan scans every marked object based in span si and charges what it
// touched. A continuation span of a large object resolves to its head, so a
// store into the middle of a 40 KiB object re-scans the object rather than the
// 4 KiB the store landed in.
func rescanSpan(si, budget uint32) uint32 {
	c := spanClassOf(si) &^ clsFlags
	// 0 is unassigned and clsMeta is the collector's own bookkeeping. Neither
	// holds an object, and clsMeta is the compare that replaces .bss placement:
	// see drainDirtySpans.
	if c == 0 || c == clsMeta {
		return budget
	}
	sb := heapBase + si<<spanLog
	if c == clsLarge || c == clsLargeMid {
		// ONE SPAN OF THE RUN, NOT THE WHOLE RUN, and that is the fix for a
		// livelock this stage found in a real Factorio.
		//
		// A re-scan exists to answer one question: did a store into an object
		// the collector has already scanned install a reference it has not
		// seen? The store is in the dirtied PAGE, so the page is what has to be
		// re-read -- and a page is a span. Re-scanning the whole run instead
		// made a store into a big object cost its whole SIZE: examples/gcbench
		// writes one slot of a 44,000-entry pointer array per tick, which is
		// 176 KiB, which is eleven steps of the default budget -- for one word.
		// Marking could then never terminate, because the mutator dirtied that
		// object again every tick. Measured: 1,100 ticks in phase 1 with
		// cycles=0 and the heap climbing, which agents/gc.md calls the worst
		// failure available to a guest that opted in to a collector.
		//
		// It is also what makes the full re-scan pass linear. runRescan walks
		// every span in order, so an n-span object used to be resolved to its
		// head n times and re-scanned n times -- O(size^2), 39.6 million
		// granules for a 7 MiB heap when stage C found it, and patched then
		// with a "skip a continuation span" rule. There is nothing to skip now:
		// each span carries its own share and the pass covers the run exactly
		// once.
		head := si
		if c == clsLargeMid {
			head = spanAuxOf(si)
		}
		if !isMarked(heapBase + head<<spanLog) {
			return budget
		}
		scanObject(sb, spanBytes)
		return charge(budget, spanBytes)
	}
	sz := classSize[c]
	slots := uint32(gcm.classSlots[c])
	// One bitmap address for the whole span, then a bit test per slot. The old
	// form recomputed (b-heapBase)>>4 and indexed a global array per SLOT; this
	// is why the bitmap moving into the heap did not cost the re-scan anything.
	mw := markWordBase(si)
	for i := uint32(0); i < slots; i++ {
		if isMarkedAt(mw, i*sz) {
			scanObject(sb+i*sz, sz)
		}
	}
	return charge(budget, spanBytes)
}

// scanOwed is the COLLECTOR-owned half of the work the mark phase still has to
// do, in granules: the gray stack, the resumable object scan, and the remainder
// of a full re-scan pass. The mutator cannot add to it except by making objects
// reachable, and the collector consumes it monotonically -- which is what makes
// "did it shrink" mean "did the mark converge". See markStallWindow.
//
// It is an estimate and it is allowed to be: what it is asked is whether it went
// DOWN across a window, not what it is. A gray entry is an object of unknown
// size and counts as one granule -- an undercount that only makes the metric
// more conservative, because an emptying gray stack still registers as
// shrinkage.
func scanOwed() uint32 {
	w := gcm.grayTop
	if gcm.partialBase != 0 {
		w += (gcm.partialEnd - gcm.partialBase - gcm.partialOff) >> granuleLog
	}
	if gcm.rescanOwed {
		if c := coveredSpans(); c > gcm.rescanCursor {
			w += (c - gcm.rescanCursor) << (spanLog - granuleLog)
		}
	}
	return w
}

// workOwed is scanOwed plus the MUTATOR-owned half -- the spans it wrote that
// the collector still owes a re-scan. Reported by Stats(); the metric asks the
// two halves separately.
func workOwed() uint32 {
	return scanOwed() + (gcm.dirtyN-gcm.dirtyCursor)<<(spanLog-granuleLog)
}

// charge deducts n bytes of touched heap from a budget denominated in granules,
// saturating at zero. Saturating rather than wrapping is the difference between
// a step that overran its budget and a step that runs until the heap ends.
//
// It also records the UNSATURATED total, which is what makes "no step overran
// its budget" a measurement rather than a claim: a saturating subtraction hides
// exactly the case worth knowing about, where one indivisible unit of work cost
// more than the whole allowance. gcm.maxWork is what
// TestNoPacedStepOverrunsItsBudget reads.
func charge(budget, bytes uint32) uint32 {
	n := bytes >> granuleLog
	gcm.stepWork += n
	if n >= budget {
		return 0
	}
	return budget - n
}

// scanRoots is what the runtime's markRoots hook lands in.
//
// The subtraction is the whole reason gcMeta is one struct. findGlobals reports
// [__global_base, __heap_base), and the collector's own metadata is in .bss,
// which is inside that range. Scanned as roots it would be catastrophic in two
// separate ways: 128 KiB of mark bitmap would be read as candidate pointers,
// and the free-list heads would mark every free object live, so the sweep that
// rebuilds those lists from scratch would drop every block on them.
//
// It is also what keeps the collector from dirtying its own cards, which
// agents/gc.md calls the one hazard in this design with no precedent in the
// repo: the bitmap, the gray stack and the dirty queue are all inside gcm, so
// writing them dirties pages BELOW the heap, and ingestDirty drops those.
func scanRoots(start, end uint32) {
	if start < metaHi && end > metaLo {
		if start < metaLo {
			scanRange(start, metaLo)
		}
		if end > metaHi {
			scanRange(metaHi, end)
		}
		return
	}
	scanRange(start, end)
}

// scanRange reads every aligned word of [start, end) as a candidate pointer,
// and COUNTS what it read.
//
// The count is what makes the root re-scan chargeable. It used to be free by
// omission -- markStep called gcMarkReachable and then charged the objects it
// discovered but never the range it walked -- which is one of the three things
// TestNoPacedStepOverrunsItsBudget's 1.17x residual was made of, and the only
// one that scales with anything a guest controls (its own statics).
func scanRange(start, end uint32) {
	p := (start + 3) &^ 3
	n := uint32(0)
	for ; p+4 <= end; p += 4 {
		markCandidate(load32(p))
		n++
	}
	gcm.rootWords += n
}

// markCandidate is the conservative test, and it is the whole hot loop.
//
// agents/gc.md measured what it is up against on the real churn heap: only
// 5.9% of heap words fall inside the heap range at all, 5.0% are
// granule-aligned, and they point at 10.4% of granules. The heap is dominated
// by string bytes and small integers, not by plausible pointers -- so the range
// test rejects nineteen words in twenty and everything after it runs on the
// twentieth.
//
// Interior pointers are handled by construction rather than by a search: a span
// serves exactly one size class, so an address anywhere inside an object
// resolves to that object's base with a table lookup and a multiply. That is
// required, not decorative -- a parked goroutine's asyncifysp is stack+8, an
// interior pointer into an ordinary heap block.
// AND IT REJECTS THE COLLECTOR'S OWN METADATA, which is new since the metadata
// moved into the heap. A chunk is heap MEMORY and never a heap OBJECT: an
// integer that happens to look like an address inside one must not mark it,
// because a chunk is not swept, is not freed, and has no mark bit of its own
// that means anything. It is the same one compare against the span class that
// the unassigned case already costs.
func markCandidate(v uint32) {
	if v < heapBase || v >= heapTop {
		return
	}
	si := (v - heapBase) >> spanLog
	c := spanClassOf(si) &^ clsFlags
	if c == 0 || c == clsMeta {
		return
	}
	var base uint32
	if c == clsLargeMid {
		si = spanAuxOf(si)
		c = clsLarge
	}
	if c == clsLarge {
		base = heapBase + si<<spanLog
	} else {
		sb := heapBase + si<<spanLog
		idx := gcm.slotTab[c][(v-sb)>>granuleLog]
		if idx == slotNone {
			return // the class's tail waste: not an object
		}
		base = sb + uint32(idx)*classSize[c]
	}
	markObject(base)
}

func markObject(base uint32) {
	d := base - heapBase
	wa := gcm.metaDir[d>>sliceLog] + metaMarkOff + ((d>>(granuleLog+5))&(sliceWords/32-1))<<2
	b := uint32(1) << ((d >> granuleLog) & 31)
	w := load32(wa)
	if w&b != 0 {
		return
	}
	store32(wa, w|b)
	// MARKS ARE MONOTONE, so this counter only rises within a collection, and
	// "did it rise across this step" is the only cheap question that means
	// "did the unmarked set shrink". See markStallLimit.
	gcm.marked++
	if gcm.grayTop == grayCap {
		// Marked but not queued. The full re-scan pass picks it up; see
		// runRescan for why that terminates.
		gcm.grayOvf = true
		return
	}
	gcm.gray[gcm.grayTop] = base
	gcm.grayTop++
}

func isMarked(base uint32) bool {
	d := base - heapBase
	wa := gcm.metaDir[d>>sliceLog] + metaMarkOff + ((d>>(granuleLog+5))&(sliceWords/32-1))<<2
	return load32(wa)&(uint32(1)<<((d>>granuleLog)&31)) != 0
}

// objectSize is the span-derived size of the object based at base. For a large
// object that is the whole span run, INCLUDING the slack past the requested
// size -- scanning slack can only over-retain, and the alternative is a header
// word on every allocation.
func objectSize(base uint32) uint32 {
	si := (base - heapBase) >> spanLog
	c := spanClassOf(si) &^ clsFlags
	if c == clsLarge {
		return spanAuxOf(si) << spanLog
	}
	return classSize[c]
}

// drainGray empties the mark stack under a budget, and turns overflow into a
// full re-scan rather than into a failure.
//
// Overflow is a performance event and not an error. A newly marked object that
// does not fit on the stack stays MARKED, so the mark bitmap is still the
// truth; what is lost is the record that its contents have not been scanned
// yet. Stage B recovered by re-scanning immediately and unboundedly. Stage C
// hands the same recovery to runRescan, which is budgeted and resumable -- so
// the one path that could put a full heap walk inside a single tick no longer
// can.
//
// A SINGLE OBJECT CAN BE BIGGER THAN A WHOLE STEP'S BUDGET, and that is the trap
// agents/gc.md names by name:
//
//	Lua traverses a table in one indivisible propagatemark, so there is nothing
//	to pace, whereas this collector's gray unit is a 16-byte granule and a step
//	is splittable by construction. If that ever stops being true -- a gray unit
//	that is a whole object of unbounded size -- the same trap has been walked
//	into twice.
//
// A Go guest's 1 MiB slice is exactly such an object: scanning it is 65,536
// granules against a 1,024-granule budget, i.e. one ~32 ms tick, which is the
// stage-B pause put back where it was. So an object is scanned through a
// RESUMABLE cursor and the gray unit really is the granule. One partial scan
// exists at a time, because gray is drained LIFO one object at a time, and it
// lives in gcm like everything else -- so it survives a save taken mid-object.
func drainGray(budget uint32) uint32 {
	for {
		if gcm.partialBase != 0 {
			budget = scanPartial(budget)
			if gcm.partialBase != 0 {
				return 0 // the object did not fit in what was left
			}
		}
		if gcm.grayTop == 0 {
			break
		}
		if budget == 0 {
			return 0
		}
		gcm.grayTop--
		base := gcm.gray[gcm.grayTop]
		gcm.partialBase = base
		gcm.partialOff = 0
		gcm.partialEnd = base + objectSize(base)
	}
	if gcm.grayOvf {
		gcm.grayOvf = false
		oweRescan()
	}
	return budget
}

// finishPartial completes the one in-flight object scan if there is one, so
// that a caller which is about to start another may. It is markStep's first act
// for that reason.
func finishPartial(budget uint32) uint32 {
	for gcm.partialBase != 0 && budget != 0 {
		budget = scanPartial(budget)
	}
	return budget
}

// scanPartial advances the one in-flight object scan by at most budget
// granules, and clears partialBase when the object is finished.
func scanPartial(budget uint32) uint32 {
	p := gcm.partialBase + gcm.partialOff
	end := gcm.partialEnd
	limit := end
	if n := budget << granuleLog; n < end-p {
		limit = p + n
	}
	for ; p+4 <= limit; p += 4 {
		markCandidate(load32(p))
	}
	done := p - (gcm.partialBase + gcm.partialOff)
	gcm.partialOff = p - gcm.partialBase
	if p >= end {
		gcm.partialBase = 0
		gcm.partialOff = 0
		gcm.partialEnd = 0
	}
	return charge(budget, done)
}

// scanObject is the unbounded scan, kept for the one caller whose unit is a
// small-class SLOT -- at most 2 KiB, which is a quarter of the default budget
// and the natural quantum of a span walk. Nothing else may use it: an
// unbounded scan of an object whose size the guest chooses is the indivisible
// step drainGray exists to prevent.
func scanObject(base, size uint32) {
	end := base + size
	for p := base; p+4 <= end; p += 4 {
		markCandidate(load32(p))
	}
}

// THERE IS NO clearMarkBits ANY MORE, AND THAT IS A FIX RATHER THAN A SAVING.
//
// Stage C wiped the whole bitmap at the start of every collection, as belt and
// braces over a sweep that already clears each span's eight words as it passes.
// Since the bitmap moved into the heap that wipe is not merely redundant, it is
// actively harmful: it is O(heap/128) of HEAP STORES made while the write
// barrier is armed, so at a 128 MiB heap it dirties 256 pages in one go, which
// is exactly `dirtyCap`. The host then hands the next step DirtyAll and every
// collection opens with a mandatory full-heap re-scan.
//
// The invariant that makes it unnecessary is worth stating, because it is what
// a later change must not break:
//
//	AT phaseIdle, EVERY MARK BIT OVER THE COVERED HEAP IS ZERO.
//
// Three things maintain it and there is no fourth writer. finishSweep is the
// only path to phaseIdle and sweepStep visits every covered span before it,
// clearing as it goes. growCoverage wipes a chunk when it creates it. growHeap
// adds spans inside an existing chunk, and no bit above the old heapTop can
// ever have been set because markCandidate's range test rejects those
// addresses. TestTheMarkBitmapIsZeroBetweenCollections is what stops that being
// a comment.

// ---------------------------------------------------------------------------
// Sweeping -- the expensive half, and the half that needs no barrier
// ---------------------------------------------------------------------------

// beginSweep closes the mark phase and opens the sweep.
//
// STAGE B RESET EVERY CLASS'S CURRENT RUN HERE, and stage C must not. Stage B's
// reason was sound: the unconsumed blocks in a half-consumed run are unmarked,
// so the walk re-discovers them, and a surviving cursor would hand the same
// block out twice. But a stop-the-world sweep ran to completion before the
// mutator saw the heap again, and a paced one does not -- dropping every
// class's run at the moment marking ends would leave the very next allocation
// with nothing to bump through and no swept span to refill from, which is a
// synchronous sweep-ahead in the first tick after every mark.
//
// So the run is PROTECTED rather than dropped: sweep skips the slots inside
// [curPtr, curEnd) of the class that owns the span, counting them neither live
// nor free and threading them onto no run. The class keeps bumping through
// exactly the blocks it already owned, which is up to one span of headroom per
// active class -- enough to cover the ticks before the paced sweep releases its
// first span.
//
// The run LISTS are dropped, which is stage B's rule intact for everything the
// class is not currently bumping through: those blocks are unmarked and the
// walk re-discovers them, in ascending address order, deterministically.
// clearPending drops every clsPending flag and empties the list. Marking is over
// or has not begun, so nothing is owed a re-scan either way.
func clearPending() {
	for i := gcm.dirtyCursor; i < gcm.dirtyN; i++ {
		si := gcm.pend[i]
		if c := spanClassOf(si); c&clsPending != 0 {
			setSpanClass(si, c&^clsPending)
		}
	}
	gcm.dirtyN = 0
	gcm.dirtyCursor = 0
}

func beginSweep() {
	for c := uint32(1); c <= numClasses; c++ {
		gcm.runHead[c] = 0
		gcm.runTail[c] = 0
		// THE HOLD WINDOW IS SNAPSHOTTED HERE, and reading the live curPtr
		// instead was a bug that reclaimed thirteen live blocks out of
		// thirty-two on the gcsave guest at a small budget.
		//
		// curPtr ADVANCES while the sweep runs: every allocation the class
		// serves from this run moves it forward. A span the sweep has not
		// reached yet still holds that run, so by the time it is walked, every
		// block handed out since marking ended is BELOW the live cursor -- and
		// therefore outside a window computed from it, and unmarked, because
		// nothing marks after termination. The sweep frees them while live.
		//
		// The window that is actually correct is the one as it stood when
		// marking terminated. Everything above it is untouched free space;
		// everything the class hands out of it afterwards is inside it and is
		// held rather than freed.
		gcm.holdLo[c] = gcm.curPtr[c]
		gcm.holdHi[c] = gcm.curEnd[c]
	}
	clearPending()
	gcm.markForced = false
	gcm.sweepCursor = 0
	gcm.largeKeep = 0
	gcm.liveAcc = 0
	gcm.freeAcc = 0
	gcm.freedAcc = 0
	gcm.liveObjAcc = 0
	gcm.phase = phaseSweep
}

// sweepStep sweeps spans under a budget and finishes the collection when the
// cursor reaches the end.
//
// It needs no write barrier and that is the point of doing it second. After
// mark termination the bitmap is not written again by anything, so a mutator
// running between two sweep steps cannot invalidate a decision this makes: a
// slot is live or it is not, and the answer was fixed when marking ended.
// The only coupling with the mutator is in the other direction -- what it may
// ALLOCATE while a sweep is in flight -- and findSpanRun is where that is
// handled.
//
// The order is deterministic by construction -- spans ascending, slots
// ascending -- which matters for the same reason agents/gc.md says the dirty
// page set must be consumed through DPQ rather than pairs(DPG): what lands in
// storage is CRC'd and multiplayer-synchronised, and a free list whose shape
// depended on iteration order would be a per-client heap layout. Pacing does
// not weaken that: a step boundary is a tick boundary, which every client
// shares.
func sweepStep(budget uint32) {
	covered := coveredSpans()
	for gcm.sweepCursor < covered {
		if budget == 0 {
			return
		}
		budget = sweepSpan(gcm.sweepCursor, budget)
	}
	finishSweep()
}

// sweepSpan sweeps the span (or large-object run) at si, advances the cursor
// past it, and returns the budget left.
func sweepSpan(si, budget uint32) uint32 {
	raw := spanClassOf(si)
	c := raw &^ clsFlags
	if c == 0 {
		gcm.freeAcc += spanBytes
		gcm.sweepCursor = si + 1
		clearSpanMarks(si)
		return budget // an unassigned span is no work at all
	}
	if c == clsMeta {
		// The collector's own tables. Never swept, never freed, never counted
		// as free -- and its mark words are never set, because markCandidate
		// rejects a clsMeta span, so there is nothing to clear either.
		gcm.sweepCursor = si + 1
		return charge(budget, spanBytes/16)
	}
	if raw&clsFresh != 0 {
		// CLAIMED AFTER MARKING TERMINATED. Every slot in it is either live or
		// part of the class's current run, and none of them has a mark bit
		// because nothing marks after termination. Skipping it whole is the
		// same treatment beginSweep's hold window gives the run the class was
		// already bumping through -- generalised to the runs it acquires while
		// the sweep is in flight, which is what lets the mutator allocate
		// anywhere. See clsFresh.
		//
		// The flag is cleared here, and here is the only place it can be:
		// freshBit sets it only for a span at or above the cursor, so the sweep
		// is guaranteed to arrive.
		if c == clsLarge || c == clsLargeMid {
			for gcm.sweepCursor < coveredSpans() {
				k := spanClassOf(gcm.sweepCursor)
				if k&clsFresh == 0 {
					break
				}
				kc := k &^ clsFlags
				if kc != clsLarge && kc != clsLargeMid {
					break
				}
				if kc == clsLarge && gcm.sweepCursor != si {
					break
				}
				setSpanClass(gcm.sweepCursor, kc)
				gcm.sweepCursor++
				budget = charge(budget, spanBytes/16)
			}
			return budget
		}
		setSpanClass(si, c)
		gcm.sweepCursor = si + 1
		return charge(budget, spanBytes/16)
	}
	sb := heapBase + si<<spanLog
	if c == clsLarge || c == clsLargeMid {
		// A LARGE RUN IS SWEPT SPAN BY SPAN, RESUMABLY, for the same reason a
		// large object is SCANNED granule by granule: an object whose size the
		// guest chooses must not be an indivisible step. A 1.6 MiB object is
		// 400 spans, and freeing all 400 in one go is a few thousand stores in
		// one tick -- small next to stage B's pause, and still an unbounded
		// unit that grows with whatever the guest allocates.
		//
		// The head decides the run's fate and accounts for it once; largeKeep
		// carries that decision across a step boundary, because the cursor can
		// stop in the middle of a run and the head's mark bit is cleared on the
		// way past.
		if c == clsLarge {
			n := spanAuxOf(si)
			if isMarked(sb) {
				gcm.liveAcc += n << spanLog
				gcm.liveObjAcc++
				gcm.largeKeep = 1
			} else {
				gcm.freeAcc += n << spanLog
				gcm.freedAcc++
				gcm.largeKeep = 2
			}
		}
		covered := coveredSpans()
		for budget > 0 && gcm.sweepCursor < covered {
			k := spanClassOf(gcm.sweepCursor)
			if k&clsFresh != 0 {
				break // a run claimed after termination; it gets its own pass
			}
			if k != clsLarge && k != clsLargeMid {
				break
			}
			if k == clsLarge && gcm.sweepCursor != si {
				break // the next object's head; it gets its own decision
			}
			clearSpanMarks(gcm.sweepCursor)
			if gcm.largeKeep == 2 {
				setSpanClass(gcm.sweepCursor, 0)
				setSpanAux(gcm.sweepCursor, 0)
			}
			gcm.sweepCursor++
			// Charged for the WORK and not for the bytes: this touches none of
			// the object's contents -- nothing to zero, no slots to thread --
			// so a span of it is a handful of stores against a small-class
			// span's 256 bitmap tests. A sixteenth is the honest order.
			budget = charge(budget, spanBytes/16)
		}
		if gcm.sweepCursor >= covered ||
			spanClassOf(gcm.sweepCursor)&^clsFresh != clsLargeMid {
			gcm.largeKeep = 0
		}
		return budget
	}

	// A small-class span.
	sz := classSize[c]
	slots := uint32(gcm.classSlots[c])
	// The protected window: if this span holds class c's current run, the
	// blocks between the bump cursor and the run's end belong to the class
	// already. See beginSweep.
	pl, ph := uint32(0), uint32(0)
	if p := gcm.holdLo[c]; p != 0 && (p-heapBase)>>spanLog == si {
		pl, ph = p, gcm.holdHi[c]
	}
	// One bitmap address for the whole span; see rescanSpan.
	mw := markWordBase(si)
	nlive := uint32(0)
	nheld := uint32(0)
	for i := uint32(0); i < slots; i++ {
		if isMarkedAt(mw, i*sz) {
			nlive++
		} else if a := sb + i*sz; a >= pl && a < ph {
			nheld++
		}
	}
	if nlive == 0 && nheld == 0 {
		// A span that sweeps completely empty is RETURNED to the span
		// allocator rather than kept by its class. That is the
		// anti-fragmentation lever: without it a burst of one size permanently
		// reserves spans another size then has to grow the heap to get, and a
		// heap that grows never un-grows.
		setSpanClass(si, 0)
		gcm.freeAcc += spanBytes
		gcm.freedAcc += slots
		clearSpanMarks(si)
		gcm.sweepCursor = si + 1
		return charge(budget, spanBytes)
	}
	// Dead slots become RUNS, in address order: one mem_fill to zero the run
	// and one descriptor written into its first eight bytes. This is what pays
	// for the allocation path having no memory operation in it at all (see
	// freeInvariant and allocate) -- an allocate-and-drop workload leaves long
	// runs, so a span a per-slot free list would have called mem_fill 256 times
	// for is usually one call here.
	runStart := uint32(0)
	runLen := uint32(0)
	for i := uint32(0); i <= slots; i++ {
		if i < slots {
			a := sb + i*sz
			if !isMarkedAt(mw, i*sz) && !(a >= pl && a < ph) {
				if runLen == 0 {
					runStart = i
				}
				runLen++
				continue
			}
		}
		if runLen != 0 {
			zero(sb+runStart*sz, runLen*sz)
			pushRun(c, sb+runStart*sz, sb+(runStart+runLen)*sz)
			runLen = 0
		}
	}
	gcm.liveAcc += nlive * sz
	gcm.liveObjAcc += nlive
	gcm.freeAcc += (slots - nlive - nheld) * sz
	gcm.freedAcc += slots - nlive - nheld
	clearSpanMarks(si)
	gcm.sweepCursor = si + 1
	return charge(budget, spanBytes)
}

func finishSweep() {
	gcm.liveBytes = gcm.liveAcc
	gcm.liveObjs = gcm.liveObjAcc
	gcm.freeBytes = gcm.freeAcc
	// freedObjs counts slots returned to a free list plus slots inside released
	// spans -- everything this sweep made available.
	gcm.freedObjs = gcm.freedAcc
	// A released span is only reachable to the span allocator if the cursor can
	// reach it, and the cursor only moves forward.
	// The hold windows are done with: every span they covered has been swept,
	// so the blocks in them are ordinary members of their class's current run
	// again and the next collection will decide them on the bitmap like
	// anything else.
	for c := uint32(1); c <= numClasses; c++ {
		gcm.holdLo[c] = 0
		gcm.holdHi[c] = 0
	}
	gcm.spanCursor = 0
	gcm.collections++
	gcm.sinceGC = 0
	gcm.lastSteps = gcm.steps
	gcm.phase = phaseIdle
}

// ---------------------------------------------------------------------------
// The host-facing surface: three exports and one import.
// ---------------------------------------------------------------------------

// dirtyBuf is where the host writes the page numbers written since the last
// step. It is a field of gcm rather than a package-level array for the reason
// gcMeta documents: everything mutable this package owns has to be one
// contiguous range so scanRoots can subtract it, and a separate var is not
// guaranteed adjacent to anything.

// DirtyBase is the address the host writes dirtied page numbers to, as i32
// words. DirtyCap is how many fit. Both mirror the fk_scratch_base /
// fk_scratch_size pair the string ABI already uses, and for the same reason:
// the GUEST owns the address, because a pointer the host invented would land in
// the middle of something.
func DirtyBase() uint32 { return uint32(uintptr(unsafe.Pointer(&gcm.dirtyQ[0]))) }

// DirtyCap is how many page numbers the buffer holds. A count larger than this
// arrives as DirtyAll.
func DirtyCap() uint32 { return dirtyCap }

//go:wasmexport fk_gc_step
func fkGCStep(ndirty uint32) uint32 { return Step(ndirty) }

//go:wasmexport fk_gc_dirty_base
func fkGCDirtyBase() uint32 { return DirtyBase() }

//go:wasmexport fk_gc_dirty_cap
func fkGCDirtyCap() uint32 { return DirtyCap() }

// hostGCPace asks the host to arm the write barrier and schedule collection
// steps until the collection finishes.
//
// It is fk.defer with a different payload, which is exactly what agents/gc.md
// section 4 said the pacing machinery would be: a one-shot on_tick registered
// only while there is something to do, torn down by its own handler, with the
// armed flag mirrored into `storage` because Factorio does not save event
// registrations. An idle guest registers nothing and pays nothing.
//
//go:wasmimport fk gc
func hostGCPace() uint32

// Stats reports what the collector knows about itself. See MemStats.
func Stats() MemStats {
	return MemStats{
		HeapBytes:    heapTop - heapBase,
		LiveBytes:    gcm.liveBytes,
		FreeBytes:    gcm.freeBytes,
		SinceGC:      gcm.sinceGC,
		Collections:  gcm.collections,
		LiveObjects:  gcm.liveObjs,
		FreedObjects: gcm.freedObjs,
		Grows:        gcm.grows,
		Phase:        uint32(gcm.phase),
		Steps:        gcm.lastSteps,
		Deadlines:    gcm.deadlines,
		StepEscapes:  gcm.stepEscapes,
		StallEscapes: gcm.stallEscapes,
		Outruns:      gcm.outruns,
		UnpacedWork:  gcm.unpacedWork,
		MaxUnpaced:   gcm.maxUnpacedWork,
		MetaBytes:    MetaBytes(),
	}
}

// MaxUnpacedWork is the most collector work, in granules, that landed in a
// single GUEST CALL rather than in a paced step -- the sweep-ahead in
// allocSpans, and the initial root scan of a collection the guest started.
//
// It is the number MaxStepWork could never see. Both belong in a worst-tick
// claim: a tick's collector cost is one step plus whatever the handler in that
// tick charged, and until this existed only the first half was measured.
func MaxUnpacedWork() uint32 { return gcm.maxUnpacedWork }

// UnpacedWork is the same quantity summed over the current (or last)
// collection.
func UnpacedWork() uint32 { return gcm.unpacedWork }

// Outruns is how many times an allocation had to grow the heap while a
// collection was still running. It is the honest measure of "the mutator beat
// the pacer", and the response to it is growth rather than a pause -- see
// allocSpans.
func Outruns() uint32 { return gcm.outruns }

// Marked is how many objects this collection has marked. Stalls is how many
// CONSECUTIVE windows of markStallWindow mark steps have failed to reduce the
// work still owed, and MaxStalls is the longest such run this collection has
// seen. WorkOwed is the scalar behind both.
func Marked() uint32    { return gcm.marked }
func Stalls() uint32    { return gcm.stalls }
func MaxStalls() uint32 { return gcm.maxStalls }

// PendEmpties is how many mark steps this collection ended with the pending
// dirty list empty. Zero over a long mark is the mutator winning.
func PendEmpties() uint32 { return gcm.pendEmpties }
func WorkOwed() uint32 {
	if gcm.phase != phaseMark {
		return 0
	}
	return workOwed()
}

// MaxUnpacedFolds is how many separate bursts made up MaxUnpacedWork. One means
// a single indivisible unit; many means an allocation loop that kept asking.
func MaxUnpacedFolds() uint32 { return gcm.maxUnpacedFolds }

// RootWords is how many 4-byte words the last root scan read. A guest with
// large statics pays it at every termination attempt, and since stage C it is
// charged against the step budget rather than being free by omission.
func RootWords() uint32 { return gcm.rootWords }

// Rescans counts completed full re-scan passes, and DirtyOverflows counts the
// times the record of what changed was lost -- a dirty set larger than the
// buffer, a gray-stack overflow, or a resumed save. Both are performance
// events rather than errors, and both are how a mark phase becomes long.
func Rescans() uint32        { return gcm.rescans }
func RescanRestarts() uint32 { return gcm.rescanRestarts }
func DirtyOverflows() uint32 { return gcm.dirtyOverflows }

// Terminations counts mark-termination attempts in the current (or last)
// collection. One is the healthy number.
func Terminations() uint32 { return gcm.terminations }

// MarkBitsSet counts the set mark bits over the covered heap. It is O(heap) and
// exists for one assertion: at phaseIdle the answer must be zero, which is the
// invariant that lets the start of a collection skip a bitmap wipe. Nothing on
// a hot path calls it.
func MarkBitsSet() uint32 {
	n := uint32(0)
	covered := coveredSpans()
	for si := uint32(0); si < covered; si++ {
		w := markWordBase(si)
		for k := uint32(0); k < markWordsPerSpan; k++ {
			v := load32(w + k<<2)
			for v != 0 {
				n += v & 1
				v >>= 1
			}
		}
	}
	return n
}

// BackedBytes is the linear memory the allocator has claimed as heap, which is
// not the same as HeapBytes: HeapBytes is the COVERED heap, and coverage lags a
// memory.grow until the chunk describing the new slice exists. The gap is at
// most one slice and closes on the next allocation that wants room; nothing is
// lost in it.
func BackedBytes() uint32 { return gcm.spanCount << spanLog }

// Reinitialize re-runs initHeap against the linear memory that exists now.
//
// IT IS DESTRUCTIVE AND IT IS FOR TESTS. Every free list and every span
// assignment is forgotten, so nothing allocated before it may be touched after.
// It exists so that "the allocator adopts ALL pre-existing linear memory" is an
// assertion rather than a reading of initialize(), which is where the cap used
// to clamp -- silently, with no log line and no counter.
func Reinitialize() { initialize() }

// HeapBase and HeapTop bracket the region the allocator owns. They exist for
// the stage-B test guests, which assert things about retention that need an
// address range, and for a guest that wants to log where its heap is.
func HeapBase() uint32 { return heapBase }

// HeapTop is the exclusive upper bound of the allocator's region.
func HeapTop() uint32 { return heapTop }

// MetaBytes and the rest of the metadata surface live in meta.go, with the
// scaling design they describe.
