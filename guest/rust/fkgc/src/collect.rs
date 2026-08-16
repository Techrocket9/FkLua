//! The paced collector: phases, marking, sweeping, and the three exports the
//! host drives them through.
//!
//! The mirror of `guest/go/fkgc/collect.go`. The state machine, the budget, the
//! escape metric and the storm valve are the same because the design is the
//! same; `mark_reachable` is the one function with no counterpart, because
//! TinyGo provides root DISCOVERY and rustc does not.
//!
//! ---------------------------------------------------------------------------
//! The phases, and what a step may assume about the world between two of them.
//!
//! THE SAFE-POINT PRECONDITION, which is the whole argument:
//!
//! > A COLLECTION STEP RUNS ONLY AT AN OUTERMOST DISPATCH BOUNDARY. At such a
//! > point the wasm operand stack is empty, the shadow stack is empty, and
//! > therefore EVERY live reference the mutator holds is either in the guest
//! > heap or in `[__global_base, __heap_base)`. There is no third place.
//!
//! That is what makes a terminate-time barrier sufficient where a real
//! incremental collector would need a tricolour one. The mutator cannot hide a
//! pointer in a register or a stack slot across a step, so it cannot delete the
//! last heap reference to an object and keep it alive privately. Marking
//! therefore terminates by looking at the FINAL state of two things:
//!
//!   1. the root range, re-scanned wholesale
//!   2. every marked object in every heap page the mutator wrote since the mark
//!      began -- which is exactly what `MEMDIRTY`'s page set records
//!
//! **In Rust the precondition is load-bearing where in Go it is merely true**,
//! and that is the whole difference between the two ports. TinyGo spills every
//! live pointer to a shadow stack it then scans, so a Go collection that runs
//! mid-call still sees the frame. rustc keeps live references in wasm LOCALS,
//! which nothing can scan -- so the two paths where `guest/go/fkgc` marks inside
//! a guest call had to go. There is exactly one left, the initial root scan of a
//! collection the guest itself started, and `lib.rs` argues why missing a local
//! there cannot lose an object: marking does not free anything, and marking
//! cannot TERMINATE except inside [`step`], which only [`fk_gc_step`] calls.
//! ---------------------------------------------------------------------------

use crate::heap::{
    self, gcm, linker_global_base, linker_heap_base, linker_stack_high, load32, push_run, store32,
    CLASS_SIZE, CLS_FLAGS, CLS_FRESH, CLS_LARGE, CLS_LARGE_MID, CLS_META, CLS_PENDING, GRANULE_LOG,
    GRAY_CAP, NUM_CLASSES, SLOT_NONE, SPAN_BYTES, SPAN_LOG,
};
use crate::meta::{
    clear_span_marks, covered_spans, is_marked_at, mark_word_base, set_span_aux, set_span_class,
    span_aux_of, span_class_of, MARK_WORDS_PER_SPAN, META_MARK_OFF, SLICE_LOG, SLICE_WORDS,
};

/// No collection in progress: the barrier is disarmed, no `on_tick`
/// registration exists, and an idle guest pays exactly nothing.
pub(crate) const PHASE_IDLE: u8 = 0;
/// Marking, and it is the ONLY phase the write barrier is armed for.
pub(crate) const PHASE_MARK: u8 = 1;
/// Sweeping. The mark bitmap is stable -- nothing marks during a sweep -- so
/// sweep needs no barrier at all, which is why the expensive half is also the
/// cheap half to incrementalize.
pub(crate) const PHASE_SWEEP: u8 = 2;

/// The count [`fk_gc_step`] is handed when the host cannot say which pages were
/// written -- after a save/load cycle, where the page set lived in a Lua table
/// that `storage` never carried.
///
/// It is not an error. It degrades to a full re-scan of every marked object,
/// which is the same recovery gray-stack overflow already uses, and it is
/// budgeted and resumable like everything else here.
pub const DIRTY_ALL: u32 = u32::MAX;

/// How many dirtied page numbers one step can be handed. 256 pages is 1 MiB of
/// dirtied heap in a single tick -- far more than an ordinary handler produces.
/// Beyond it the count arrives as [`DIRTY_ALL`].
pub(crate) const DIRTY_CAP: u32 = 256;

/// How many dirtied spans the collector may have PENDING across steps, which is
/// a different question from what one step can be handed and deserves a
/// different number. Four batches' worth of headroom, 4 KiB of `.bss`.
pub(crate) const PEND_CAP: u32 = 4 * DIRTY_CAP;

/// One step's work allowance, in GRANULES OF HEAP TOUCHED.
///
/// A full stop-the-world mark and sweep costs 13.9 to 32.8 ms per MiB of heap,
/// and 32.8 is the figure at the sizes where a pause is a problem. 1 MiB / 16 B
/// is 65,536 granules, so 32.8 ms/MiB is 0.50 µs per granule and a 0.5 ms budget
/// is 1,000 granules. 1024 it is, which is 16 KiB of heap per step; at 60 UPS
/// that is a sustained ~1 MiB/s of collector throughput.
///
/// A granule is charged when it is TOUCHED, which means the same unit prices
/// both phases honestly: marking charges the size of an object it scans, and
/// sweeping charges the size of a span it walks.
pub(crate) const DEFAULT_BUDGET: u32 = 1024;

/// How long the mark phase may spend yielding to the budget before it stops
/// yielding and finishes.
///
/// THIS IS NOT A TUNING PARAMETER, IT IS A LIVELOCK FIX. Each step first
/// re-scans the marked objects in every page the mutator dirtied since the last
/// one, and only then tries to terminate -- so a guest that dirties a page per
/// tick against a budget smaller than that spends every step on the backlog and
/// never reaches the termination attempt. Marking then runs forever, the write
/// barrier stays armed forever, and nothing is ever reclaimed. There is no error
/// and no pause; the heap grows exactly as if there were no collector, which is
/// the worst failure available to a guest that opted in to one.
///
/// The deadline SCALES WITH THE HEAP, because a legitimately slow mark -- a big
/// live set at a small budget -- is not a livelock:
///
/// ```text
/// limit = MARK_DEADLINE_SLACK * (heap granules / budget) + MARK_DEADLINE_FLOOR
/// ```
const MARK_DEADLINE_SLACK: u32 = 4;
const MARK_DEADLINE_FLOOR: u32 = 600;

/// What a termination attempt is given ON TOP of the root re-scan it is about to
/// pay for, in granules.
///
/// THE ROOT RE-SCAN IS INDIVISIBLE AND THE BUDGET HAS TO ACCOMMODATE IT. A
/// termination attempt walks `[__global_base, __heap_base)` wholesale and charges
/// what it walked -- `root_words >> 2` granules -- and a guest chooses that
/// number by declaring statics. When it exceeds one step's whole allowance the
/// charge saturates to zero, the post-scan check reads "out of budget", and the
/// phase defers to a step that will do exactly the same thing. Marking then NEVER
/// terminates: nothing is reclaimed, the barrier stays armed, and the only thing
/// that ends it is `mark_deadline` -- hundreds of steps later, each having
/// re-walked the whole root range for nothing. Measured on the Go arm's
/// `examples/gctorture` with 390 root words (97 granules of charge):
///
/// ```text
/// budget   steps   termination attempts   deadlines
///   1024       3                      1           0
///     64     915                    903           1
///      8   3,051                  3,014           1
/// ```
///
/// and it gets WORSE as the budget falls, because `mark_deadline` scales as
/// heap/budget. Reported from the field by BetterBeltBalancer, whose globals grew
/// 104 bytes past its own budget's cliff; the symptom there was a rising
/// `deadlines` count and a collector that appeared to be working.
///
/// WHY THE SCAN IS NOT MADE RESUMABLE INSTEAD, which is the obvious alternative
/// and is unsound: the roots live BELOW the heap, and `ingest_dirty` drops every
/// dirty page below `heap_base`, so there is no write barrier over the statics.
/// The terminate-time barrier is sufficient only because the root range is read
/// in ONE uninterrupted pass at ONE safe point. A scan resumed across two safe
/// points would read the first half at one and the second at the next, and a
/// reference moved from the second half to the first in between is a live object
/// swept. So the budget yields to the scan and not the other way round.
///
/// The margin is what is left after the charge, and it is what makes the attempt
/// strictly progressive rather than merely affordable. 64 granules is 1 KiB of
/// heap and 6% of the default budget, so on any guest whose roots fit inside its
/// budget the floor does not bind and nothing changes at all.
const ROOT_SCAN_MARGIN: u32 = 64;

/// What the next termination attempt's wholesale root re-scan will charge, in
/// granules, from the last scan that actually happened.
///
/// Stable across the attempts of one collection and across collections: it is the
/// size of `[__global_base, __heap_base)` less the collector's own metadata,
/// fixed at link time, plus the shadow stack, which is empty at every safe point
/// where a step may run.
fn root_scan_cost() -> u32 {
    gcm().root_words >> 2
}

/// [`budget`], floored at what a termination attempt costs plus
/// [`ROOT_SCAN_MARGIN`].
///
/// A guest whose statics are large enough for this to exceed [`budget`] cannot
/// have the pause it asked for, and this is the number that says so. See
/// [`ROOT_SCAN_MARGIN`] for the failure it replaced -- which was silent, and was
/// not a longer pause but no collection at all.
pub fn effective_budget() -> u32 {
    let fl = root_scan_cost() + ROOT_SCAN_MARGIN;
    let b = gcm().budget;
    if b < fl {
        fl
    } else {
        b
    }
}

/// THE SECOND DEADLINE, and it is the one that fires on a guest whose dirty rate
/// is over its budget.
///
/// WHAT IT MEASURES IS NET SHRINKAGE OF THE WORK STILL OWED, over a window, and
/// the owed work has TWO OWNERS which have to be asked separately:
///
/// * SCAN work -- the gray stack, the resumable object scan, the remainder of a
///   full re-scan pass. Only the COLLECTOR adds to it, and it consumes it
///   monotonically.
/// * DIRTY work -- spans the mutator wrote and the collector owes a re-scan.
///   Only the MUTATOR adds to it.
///
/// A legitimately slow mark is scan-dominated. A livelock is dirty-dominated. So
/// a window is STALLED when both are true: the pending dirty list did not reach
/// empty at any step in it, AND the scan work did not shrink across it.
const MARK_STALL_WINDOW: u32 = 8;
const MARK_STALL_LIMIT: u32 = 4;

/// How much sweeping an ALLOCATION does when it finds no free span and the sweep
/// has not reached one yet. Four spans, i.e. one paced step's worth.
pub(crate) const SWEEP_AHEAD_UNITS: u32 = 1024;

/// This collection's limit, computed ONCE, at the moment it starts.
///
/// Fixed at the start rather than recomputed per step: a livelocked collector's
/// heap GROWS -- nothing is being reclaimed -- so a limit derived from the
/// current heap grows with it and the deadline recedes exactly when it is
/// needed. The size that matters is the one the mark set out to cover.
fn mark_deadline() -> u32 {
    let g = gcm();
    let mut b = g.budget;
    if b == 0 {
        b = DEFAULT_BUDGET;
    }
    MARK_DEADLINE_SLACK * ((g.span_count << (SPAN_LOG - GRANULE_LOG)) / b) + MARK_DEADLINE_FLOOR
}

/// Sets how many bytes may be handed out between collections before
/// [`collect_if_needed`] says yes. Zero restores the default.
///
/// CALLING IT BEFORE THE FIRST ALLOCATION IS SUPPORTED AND USED TO BE SILENTLY
/// DISCARDED. [`crate::heap::initialize`] is lazy -- the first allocation
/// reaches it -- and it assigned both knobs their defaults unconditionally, so
/// the obvious `set_threshold(n); build_everything();` kept the default and said
/// nothing. It latches a non-zero value now; see the comment on those two lines
/// in `heap.rs` and `examples/gcconfig`, which is the guest that measures it.
pub fn set_threshold(bytes: u32) {
    let g = gcm();
    g.threshold = if bytes == 0 {
        heap::DEFAULT_THRESHOLD
    } else {
        bytes
    };
}

/// Sets one step's work allowance in granules of heap touched. Zero restores the
/// default, which is calibrated to ~0.5 ms -- see [`DEFAULT_BUDGET`].
///
/// This is the pacing knob. Raising it collects faster and pauses longer, in a
/// straight line: the budget IS the pause, because a step charges every granule
/// it touches.
///
/// THE CALIBRATION IS A HOST-SIDE NUMBER AND IT UNDER-STATES THE GAME, always in
/// that direction, by between 1.2× and 65× depending on the heap. Read 1024 as
/// roughly half a millisecond of median and single- to double-digit
/// milliseconds of worst tick, and re-measure in game before believing any
/// particular figure -- there is no clock in the sandbox.
///
/// AND THE BUDGET HAS TWO FLOORS THAT ARE NOT ABOUT PAUSE LENGTH AT ALL. They
/// produce the SAME SYMPTOM -- [`phase`] stuck at 1, `stats().deadlines` rising,
/// nothing reclaimed -- from opposite causes, and telling them apart is the whole
/// of diagnosing a collector that appears wired and does nothing.
///
/// 1. THE DIRTY RATE, which this knob does fix. A budget under the guest's own
///    per-tick dirty rate spends itself entirely on the backlog before it ever
///    reaches the termination attempt.
///
/// 2. THE ROOT SET, which this knob does NOT fix and which the collector now
///    fixes itself. A termination attempt walks `[__global_base, __heap_base)`
///    in one indivisible pass and charges what it walked, so a budget under that
///    cost could never terminate a mark at any allocation rate, including zero.
///    [`effective_budget`] floors it and the collector logs one `fkgc:` line
///    saying so. THIS PARAGRAPH USED TO BE MISSING and its absence sent the
///    first downstream mod's whole investigation at the allocation rate for a
///    day; see [`ROOT_SCAN_MARGIN`] for the measurement.
///
/// So: if `deadlines` rises, compare [`effective_budget`] against [`budget`]
/// FIRST. Equal means cause 1 and this is the knob. Larger means cause 2, the
/// collector has already applied the floor, and what is left to decide is
/// whether the pause it implies is one this guest wants -- which is a question
/// about how many statics it declares, not about how fast it allocates.
///
/// AND READ THE SPLIT BEFORE READING A CAUSE INTO THE TOTAL. `deadlines` is
/// `step_escapes + stall_escapes`, and only the second of those says "not
/// converging"; the first is the far-out backstop and fires for a mark that is
/// affordable but slow as well. Attributing a bare `deadlines` count to the
/// allocation rate is exactly the mistake this comment used to invite -- twice,
/// downstream, once for a day. See [`MemStats::step_escapes`].
pub fn set_budget(units: u32) {
    let g = gcm();
    g.budget = if units == 0 { DEFAULT_BUDGET } else { units };
}

/// The current per-step work allowance.
pub fn budget() -> u32 {
    gcm().budget
}

/// What the collector is doing: 0 idle, 1 marking, 2 sweeping.
pub fn phase() -> u32 {
    gcm().phase as u32
}

/// Starts a PACED collection if the guest has taken more heap than the threshold
/// since the last one, and reports whether it did.
///
/// This is the call a guest puts in `fk_on_tick`. It returns after the initial
/// root scan; the rest is driven from a one-shot `on_tick` registered by the
/// host, one bounded step per tick, until the collection finishes and the
/// registration is torn down again. An idle guest is back to zero registrations
/// and zero cost.
///
/// Pacing is by heap pressure and by tick count because there is no clock in the
/// sandbox to be tempted by, and because `collectgarbage("count")` is a HOST
/// memory number that differs between machines and must never reach a decision
/// the simulation can see.
pub fn collect_if_needed() -> bool {
    let g = gcm();
    if g.since_gc < g.threshold {
        return false;
    }
    start()
}

/// Begins a paced collection now, whatever the heap pressure is, and reports
/// whether one began. A no-op while one is already in progress.
pub fn start() -> bool {
    let g = gcm();
    if !g.inited || g.phase != PHASE_IDLE {
        return false;
    }
    // ARM BEFORE STARTING, and the order is load-bearing rather than tidy.
    // Arming early can only over-record: a page dirtied before the mark began is
    // a page re-scanned for nothing. Arming late loses writes. The host call also
    // registers the one-shot `on_tick` that will drive the steps.
    unsafe { host_gc_pace() };
    start_collection();
    true
}

/// Runs one complete mark and sweep SYNCHRONOUSLY and returns when the heap is
/// swept.
///
/// **PRECONDITION, and it is not decorative: the calling frame must hold no heap
/// reference.** Marking runs here, and a reference rustc left in a wasm local of
/// the frame that called this is invisible to [`mark_reachable`] -- so this is
/// sound only as the ENTIRE body of an exported function, invoked by the host at
/// an outermost dispatch, where the only frame is the export's own. That is how
/// the corpus uses it and it is the only use this crate blesses. Use
/// [`collect_if_needed`] from inside a handler; it cannot terminate a mark and
/// therefore cannot free anything.
///
/// `guest/go/fkgc` also reaches this from its allocator when `memory.grow` is
/// refused. That caller is gone here -- see `heap::alloc_spans`.
///
/// It finishes whatever is in flight rather than starting a second collection on
/// top of one, and it owes a full re-scan while doing so: the dirty page set
/// lives in a Lua table on the other side of the boundary, and there is no host
/// to ask from in here.
///
/// ONE THING IT CANNOT DO is disarm the barrier, which is a chunk local the host
/// owns. After a synchronous collect the guest keeps paying the armed store cost
/// until the next scheduled step runs and reports phase 0.
pub fn collect() {
    let g = gcm();
    if !g.inited || g.collecting {
        return;
    }
    if g.phase == PHASE_IDLE {
        start_collection();
    }
    owe_rescan();
    while gcm().phase != PHASE_IDLE {
        step_inner(u32::MAX, 0);
    }
}

/// Opens a mark phase. It does NOT scan the whole heap: the initial root scan is
/// the statics range plus whatever the shadow stack holds, which between
/// dispatches is nothing.
fn start_collection() {
    let g = gcm();
    g.gray_top = 0;
    g.gray_ovf = false;
    g.rescan_owed = false;
    g.rescan_cursor = 0;
    clear_pending();
    g.partial_base = 0;
    g.partial_off = 0;
    g.partial_end = 0;
    g.steps = 0;
    g.mark_limit = mark_deadline();
    g.max_work = 0;
    g.total_work = 0;
    g.max_unpaced_work = 0;
    g.unpaced_work = 0;
    g.call_work = 0;
    g.terminations = 0;
    g.marked = 0;
    g.stalls = 0;
    g.max_stalls = 0;
    g.stall_steps = 0;
    g.owed_mark = 0;
    g.pend_emptied = false;
    g.pend_empties = 0;
    g.mark_forced = false;
    g.rescan_restarts = 0;
    g.outruns = 0;
    // No bitmap wipe: every mark bit over the covered heap is already zero at
    // PHASE_IDLE. See the note where `clear_mark_bits` would have been.
    g.phase = PHASE_MARK;
    g.collecting = true;
    g.root_words = 0;
    mark_reachable();
    // The initial root scan is charged like the terminating one, and for the
    // same reason: it is unbudgeted work in a tick a guest did not schedule.
    g.step_work = 0;
    charge(0, g.root_words << 2);
    g.collecting = false;
    end_unpaced();
}

/// Closes off a burst of collector work done INSIDE A GUEST CALL -- the
/// sweep-ahead in `alloc_spans`, the initial root scan -- and folds it into the
/// counters a guest can see.
///
/// It exists because [`step_inner`] zeroes `step_work` on entry, so before this
/// every granule charged between two steps was silently discarded.
pub(crate) fn end_unpaced() {
    let g = gcm();
    let w = g.step_work;
    if w == 0 {
        return;
    }
    g.step_work = 0;
    g.call_work += w;
    g.call_folds += 1;
    g.unpaced_work += w;
    if g.call_work > g.max_unpaced_work {
        g.max_unpaced_work = g.call_work;
        g.max_unpaced_folds = g.call_folds;
    }
}

/// Performs one bounded unit of collection and returns the phase the collector
/// is in afterwards -- 0 idle, 1 marking, 2 sweeping.
///
/// `ndirty` is how many page numbers the host wrote into the buffer at
/// [`dirty_base`], or [`DIRTY_ALL`] when it cannot say. The host reads the
/// return value to decide whether to keep the barrier armed (1), disarm it
/// (0 or 2), and whether to schedule another step (anything but 0).
pub fn step(ndirty: u32) -> u32 {
    step_inner(gcm().budget, ndirty) as u32
}

fn step_inner(mut budget: u32, mut ndirty: u32) -> u8 {
    let g = gcm();
    if !g.inited || g.collecting || g.phase == PHASE_IDLE {
        return g.phase;
    }
    g.collecting = true;
    g.steps += 1;
    // Whatever the mutator's own calls charged since the last step is closed off
    // and attributed to them, not to this step. `call_work` resets HERE: it
    // measures one gap between two steps, which for a paced collection is one
    // tick, and that is the number a worst-tick claim is made of.
    end_unpaced();
    g.call_work = 0;
    g.call_folds = 0;
    g.step_work = 0;
    if g.phase == PHASE_MARK {
        if g.steps > g.mark_limit || g.stalls >= MARK_STALL_LIMIT {
            // The deadline. Yielding to the budget has stopped making progress
            // toward termination, so this step stops yielding.
            //
            // THE CAUSE IS RECORDED HERE AND NOWHERE ELSE, because here is the
            // only place both conditions are still in hand -- one step later
            // the latch remembers only THAT it fired. The two are separate
            // counters because they are separate diagnoses; see
            // `MemStats::step_escapes`. The stall wins a tie, and the tie-break
            // is not arbitrary: `mark_limit` is deliberately far out (it scales
            // as heap/budget and floors at 600 steps) so that a short run
            // finishes first, while the stall window fires within a few dozen
            // steps of the mark actually ceasing to converge. If both hold, the
            // collector stopped converging long ago and the step count merely
            // caught up.
            if !g.mark_forced {
                if g.stalls >= MARK_STALL_LIMIT {
                    g.stall_escapes += 1;
                } else {
                    g.step_escapes += 1;
                }
            }
            //
            // IT LATCHES, and that is not a detail. One unbudgeted step is not
            // enough: it finishes the re-scan pass, which resets the restart
            // counter, which turns the escape off again -- and the very same
            // step's final `drain_gray` can overflow the gray stack and owe a
            // fresh pass, so the collector drops back to a budget that has
            // already been shown not to converge.
            g.mark_forced = true;
        }
        if g.mark_forced {
            // FINISHES THE PHASE, not merely one step of it. One unbudgeted
            // `mark_step` does not terminate marking, because its last act before
            // the termination attempt is a `drain_gray`, and a `drain_gray` that
            // overflows owes a fresh full re-scan.
            //
            // It terminates because MARKS ARE MONOTONE and the mutator is not
            // running: every pass either marks something new -- strictly reducing
            // a finite set -- or finds nothing and ends the phase.
            g.deadlines += 1;
            while gcm().phase == PHASE_MARK {
                mark_step(u32::MAX, ndirty);
                ndirty = 0;
            }
            // THE ESCAPE FINISHES THE MARK PHASE AND NOTHING ELSE. The unbudgeted
            // allowance must not be carried into the sweep: the sweep is the
            // EXPENSIVE half, it needs no barrier, and keeping it paced is the
            // whole design.
            budget = g.budget;
        } else {
            budget = mark_step(budget, ndirty);
        }
        // THE FORWARD-PROGRESS METRIC, sampled over a window.
        if g.phase == PHASE_MARK {
            if g.dirty_cursor >= g.dirty_n {
                g.pend_emptied = true;
                g.pend_empties += 1;
            }
            g.stall_steps += 1;
            if g.stall_steps >= MARK_STALL_WINDOW {
                g.stall_steps = 0;
                let w = scan_owed();
                if !g.pend_emptied && g.steps > MARK_STALL_WINDOW && w >= g.owed_mark {
                    g.stalls += 1;
                    if g.stalls > g.max_stalls {
                        g.max_stalls = g.stalls;
                    }
                } else {
                    g.stalls = 0;
                }
                g.owed_mark = w;
                g.pend_emptied = false;
            }
        }
    }
    // Falling straight into the sweep when marking terminated with budget left is
    // not an optimisation, it is what keeps a step's cost bounded by the budget
    // rather than by the budget plus a whole phase transition.
    if g.phase == PHASE_SWEEP && budget > 0 {
        sweep_step(budget);
    }
    if g.step_work > g.max_work {
        g.max_work = g.step_work;
    }
    g.total_work += g.step_work;
    // ZEROED ON THE WAY OUT, not only on the way in. `step_work` is the shared
    // accumulator `charge` writes to, and leaving a paced step's total sitting in
    // it means the next `end_unpaced` -- a sweep-ahead bite inside some later
    // guest call -- folds THIS step's work into that call's.
    g.step_work = 0;
    g.collecting = false;
    g.phase
}

/// The most any single step of the current (or last) collection charged, in
/// granules of heap touched.
pub fn max_step_work() -> u32 {
    gcm().max_work
}

/// What the current (or last) collection charged in total, in granules of heap
/// touched. [`max_step_work`] over this is the fraction of a whole collection
/// that lands in the worst single tick.
pub fn total_work() -> u32 {
    gcm().total_work
}

// ---------------------------------------------------------------------------
// Root discovery -- the half TinyGo gives the Go collector for free
// ---------------------------------------------------------------------------

/// Hands every root range to [`scan_roots`].
///
/// `guest/go/fkgc` links against `runtime.gcMarkReachable`, which is
/// `markStack()` followed by `findGlobals(markRoots)`. rustc has no such seam, so
/// this is the whole of it, and it is two ranges:
///
/// * `[approximate stack pointer, __stack_high)` -- the live shadow stack. At a
///   real safe point it is EMPTY, because rustc links this target stack-first
///   and `__stack_pointer` is back at `__stack_high` between calls, so this
///   costs nothing where the safe-point argument is load-bearing. Inside
///   `fk_on_tick` it catches whatever rustc happened to spill, which is a cheap
///   widening of the net and is NOT what makes the design sound. See `lib.rs`.
/// * `[__global_base, __heap_base)` -- the statics, less this crate's own
///   metadata, which [`scan_roots`] subtracts.
///
/// The stack pointer is approximated by the address of a local, because there is
/// no stable way to read the `__stack_pointer` global from Rust. `black_box`
/// stops LLVM promoting the local out of the frame it is being used to locate.
/// Approximating LOW is what a conservative scan wants: a lower bound scans more
/// and can only over-retain.
#[inline(never)]
fn mark_reachable() {
    let probe: u32 = 0;
    let sp = core::hint::black_box(&probe) as *const u32 as u32;
    let hi = linker_stack_high();
    if sp < hi {
        scan_roots(sp, hi);
    }
    scan_roots(linker_global_base(), linker_heap_base());
}

/// The subtraction is the whole reason [`crate::heap::GcMeta`] is one struct.
///
/// The statics range covers this crate's own metadata, and scanned as roots it
/// would be catastrophic in two separate ways: 32 KiB of directory and mark
/// stack would be read as candidate pointers, and the free-list heads would mark
/// every free object live, so the sweep that rebuilds those lists from scratch
/// would drop every block on them.
///
/// It is also what keeps the collector from dirtying its own cards: the
/// directory, the gray stack and the dirty queue are all inside the struct, so
/// writing them dirties pages BELOW the heap, and `ingest_dirty` drops those.
fn scan_roots(start: u32, end: u32) {
    let g = gcm();
    if start < g.meta_hi && end > g.meta_lo {
        if start < g.meta_lo {
            scan_range(start, g.meta_lo);
        }
        if end > g.meta_hi {
            scan_range(g.meta_hi, end);
        }
        return;
    }
    scan_range(start, end);
}

/// Reads every aligned word of `[start, end)` as a candidate pointer, and COUNTS
/// what it read. The count is what makes the root re-scan chargeable.
fn scan_range(start: u32, end: u32) {
    let mut p = (start + 3) & !3;
    let mut n = 0u32;
    while p + 4 <= end {
        mark_candidate(load32(p));
        p += 4;
        n += 1;
    }
    gcm().root_words += n;
}

// ---------------------------------------------------------------------------
// Marking
// ---------------------------------------------------------------------------

/// Drains gray work under a budget and terminates the mark phase when there is
/// nothing left to do. Returns the budget it did not spend.
///
/// TERMINATION IS ATTEMPTED ONLY WITH BUDGET IN HAND, and only when three queues
/// are simultaneously empty: the gray stack, the pending dirty spans, and the
/// full-rescan pass. That the loop terminates at all is the same argument gray
/// overflow makes: marks are monotone and there are finitely many objects, so an
/// attempt that finds nothing new is an attempt that ends the phase, and an
/// attempt that finds something new has strictly reduced what is left to find.
fn mark_step(mut budget: u32, ndirty: u32) -> u32 {
    // THE RESERVATION, and it is the first thing that happens because everything
    // below spends against what is left after it.
    //
    // A termination attempt re-scans the roots wholesale and charges what it
    // walked, and that cost is INDIVISIBLE (see ROOT_SCAN_MARGIN). Two separate
    // things follow, and the first cut of this fix did only the first:
    //
    //  1. a step whose WHOLE allowance is smaller than the scan can never
    //     terminate the phase at all -- so the allowance is floored;
    //  2. a step that arrives at the attempt having already SPENT its allowance
    //     on the dirty queue cannot pay for the scan either -- so the scan's
    //     cost is held back rather than left to compete with the queues.
    //
    // Doing (1) alone made the guard bite on a guest with a high dirty rate and
    // large roots: the queues took the budget every step, the attempt was
    // deferred every step, and the mark never terminated -- the livelock this
    // was meant to remove, arrived at from the other side. examples/gcsave
    // showed it as terms=0 with phase stuck at 1 across 300 ticks of a real
    // Factorio, and it is the reason run-roundtrip.sh is a gate.
    //
    // Reserving keeps both properties at once: the attempt is always affordable
    // when the queues are empty, and no step spends more than its budget,
    // because the reserve is part of it rather than added to it. The deadline
    // path calls in with u32::MAX, where this is a no-op.
    let reserve = root_scan_cost();
    let fl = reserve + ROOT_SCAN_MARGIN;
    if budget < fl {
        budget = fl;
        crate::heap::warn_root_budget();
    }
    budget -= reserve;
    ingest_dirty(ndirty);
    // The one in-flight object scan first, because only one may exist at a time
    // and both of the next two want to start one.
    budget = finish_partial(budget);
    // THE DIRTY SPANS COME BEFORE THE GRAY STACK, and that order is a fix rather
    // than a preference. The dirty page numbers live in a fixed buffer the HOST
    // OVERWRITES at every step, so a batch that is not drained in the step it
    // arrived in is GONE. Draining them first costs the gray stack nothing it
    // does not get back on the next step -- marks are monotone and the gray stack
    // is not lost when a step ends -- while the dirty batch is lost. Spend the
    // perishable one.
    budget = drain_dirty_spans(budget);
    budget = drain_gray(budget);
    budget = run_rescan(budget);
    budget = drain_gray(budget);
    let g = gcm();
    // THE QUEUES ARE SPENT; THE RESERVE IS NOT. `budget` here is what is left of
    // the queue allowance, and it is deliberately NOT tested against the scan's
    // cost: the scan is paid for out of the reserve held back above, which is
    // why an attempt is affordable on every step that gets this far.
    if budget == 0
        || g.gray_top != 0
        || g.gray_ovf
        || g.partial_base != 0
        || g.rescan_owed
        || g.dirty_cursor < g.dirty_n
    {
        return budget + reserve;
    }
    budget += reserve;
    // The termination attempt: re-scan the roots wholesale and see whether
    // anything falls out. The mutator has run since the last time they were
    // looked at, and the safe-point precondition says the roots plus the dirty
    // pages are the only two places a new reference can be.
    g.terminations += 1;
    g.root_words = 0;
    mark_reachable();
    budget = charge(budget, g.root_words << 2);
    budget = drain_gray(budget);
    // NO `budget == 0` TERM HERE, and its absence is the fix rather than a
    // tidy-up. The scan above has already happened -- charge() is post-hoc
    // accounting for a walk that is complete -- so "out of budget" says nothing
    // about whether marking is done. The four predicates that remain say it
    // exactly: an empty gray stack, no overflow, no object half-scanned and no
    // re-scan owed IS a finished mark, at any budget.
    if g.gray_top != 0 || g.gray_ovf || g.partial_base != 0 || g.rescan_owed {
        return budget;
    }
    begin_sweep();
    budget
}

/// Accepts the page numbers the host wrote into `dirty_q`.
///
/// A page and a span are the same object -- `SPAN_BYTES` is 4096 because
/// `--persist=packed`'s page size is 4096 -- so a page number converts to a span
/// index by subtraction and nothing else. Pages below the heap are the guest's
/// statics and this crate's own `.bss`; they are re-scanned wholesale as roots at
/// every termination attempt, so a dirty record for one is dropped here rather
/// than tracked.
fn ingest_dirty(n: u32) {
    if n == 0 {
        return;
    }
    let g = gcm();
    if n == DIRTY_ALL || n > DIRTY_CAP {
        g.dirty_overflows += 1;
        owe_rescan();
        clear_pending();
        return;
    }
    // THE PENDING LIST SURVIVES THE STEP, AND THE LANDING PAD DOES NOT. The batch
    // is copied off the landing pad into a list of the collector's own, four
    // times the size, drained over as many steps as it takes. The full re-scan
    // comes back only when THAT fills.
    if g.dirty_n + n > PEND_CAP {
        let r = g.dirty_n - g.dirty_cursor;
        let mut i = 0u32;
        while i < r {
            g.pend[i as usize] = g.pend[(g.dirty_cursor + i) as usize];
            i += 1;
        }
        g.dirty_n = r;
        g.dirty_cursor = 0;
    }
    // AND IT MUST NOT BECOME PERMANENT. Termination waits on
    // `dirty_cursor < dirty_n`, so a backlog that never drains is a mark that
    // never ends -- and a guest dirtying more pages per tick than its budget
    // re-scans has exactly that. Holding those pages faithfully is correct and
    // useless: the recovery it should have taken -- one full re-scan, which costs
    // O(heap) ONCE instead of O(dirty) forever -- is the thing the list was
    // postponing. Half of it is the line.
    if g.dirty_n + n > PEND_CAP || g.dirty_n - g.dirty_cursor > PEND_CAP / 2 {
        g.dirty_overflows += 1;
        owe_rescan();
        clear_pending();
        return;
    }
    // THE LIST IS A SET, and this is where it becomes one. A page whose span is
    // already pending is dropped; so is one below the heap, above the coverage
    // line, unassigned, or holding the collector's own metadata -- none of those
    // can hold a marked object.
    let base = g.heap_base >> SPAN_LOG;
    let covered = covered_spans();
    let mut i = 0u32;
    while i < n {
        let p = g.dirty_q[i as usize];
        i += 1;
        if p < base {
            continue;
        }
        let si = p - base;
        if si >= covered {
            continue;
        }
        let c = span_class_of(si);
        if c & CLS_PENDING != 0 || c & !CLS_FLAGS == 0 || c & !CLS_FLAGS == CLS_META {
            continue;
        }
        set_span_class(si, c | CLS_PENDING);
        g.pend[g.dirty_n as usize] = si;
        g.dirty_n += 1;
    }
}

/// Re-scans every marked object in each dirtied span.
///
/// Re-scanning a MARKED object is the whole of the barrier's obligation. An
/// unmarked object needs nothing: if it is ever marked it is scanned then, and if
/// it never is, it is garbage. A marked one may have been scanned already, which
/// is what makes a store into it invisible without this.
///
/// THE COLLECTOR'S OWN METADATA IS DROPPED HERE: marking writes mark bits, mark
/// bits are heap words, and every one of those writes goes through the same store
/// funnel the guest's do -- so a card for a metadata page arrives in this queue
/// like any other. `rescan_span` resolves it to `CLS_META` and returns without
/// touching anything.
fn drain_dirty_spans(mut budget: u32) -> u32 {
    let g = gcm();
    while g.dirty_cursor < g.dirty_n {
        if budget == 0 {
            return 0;
        }
        let si = g.pend[g.dirty_cursor as usize];
        g.dirty_cursor += 1;
        let c = span_class_of(si);
        if c & CLS_PENDING != 0 {
            set_span_class(si, c & !CLS_PENDING);
        }
        // NO LARGE-OBJECT DEDUP HERE. A re-scan is one SPAN, so every dirtied
        // page has its own bytes to re-read and skipping one would skip the store
        // that dirtied it.
        budget = rescan_span(si, budget);
        if g.gray_top > (GRAY_CAP / 2) as u32 {
            budget = drain_gray(budget); // see run_rescan
        }
    }
    budget
}

/// Says "the record of what changed was lost; assume everything did", AND
/// RESTARTS THE PASS FROM SPAN ZERO. The restart is the whole of it.
///
/// `rescan_owed` is resumable through `rescan_cursor`, and a pass already halfway
/// up the heap that declares itself COMPLETE without revisiting the spans below
/// the cursor misses a store into a marked object down there. Marking terminates,
/// the sweep frees a live object, and the only symptom is a checksum.
fn owe_rescan() {
    let g = gcm();
    // A LOSS WHILE A PASS IS ALREADY RUNNING IS THE LIVELOCK SIGNAL, and it is a
    // far sharper one than a step count: it says the mutator is losing the record
    // faster than the collector can rebuild it.
    g.rescan_restarts += 1;
    g.rescan_owed = true;
    g.rescan_cursor = 0;
}

/// The resumable full pass: every marked object in the heap, in span order,
/// re-scanned. It is the recovery for gray-stack overflow, for a dirty record
/// that did not fit, and for a collection resumed after a save.
///
/// Resumable rather than atomic because a full pass is O(heap) and this is the
/// one place where an unbudgeted fallback would put a stop-the-world-sized pause
/// back into a paced collector.
fn run_rescan(mut budget: u32) -> u32 {
    let g = gcm();
    if !g.rescan_owed {
        return budget;
    }
    let covered = covered_spans();
    while g.rescan_cursor < covered {
        if budget == 0 {
            return 0;
        }
        let si = g.rescan_cursor;
        g.rescan_cursor += 1;
        // A CONTINUATION SPAN IS NOT SKIPPED: a re-scan is one SPAN, so the pass
        // covers a run exactly once by walking it, and a skip would leave every
        // continuation unread.
        budget = rescan_span(si, budget);
        // DRAINED AS THE PASS GOES, and leaving that out is what stops a big heap
        // converging at all: a pass over thousands of spans accumulates hundreds
        // of thousands of gray entries against a 4,096-deep stack, overflows, and
        // owes itself a fresh pass from span zero.
        if g.gray_top > (GRAY_CAP / 2) as u32 {
            budget = drain_gray(budget);
        }
    }
    g.rescan_owed = false;
    g.rescan_cursor = 0;
    g.rescans += 1;
    budget
}

/// Scans every marked object based in span `si` and charges what it touched.
fn rescan_span(si: u32, budget: u32) -> u32 {
    let g = gcm();
    let c = span_class_of(si) & !CLS_FLAGS;
    // 0 is unassigned and CLS_META is the collector's own bookkeeping. Neither
    // holds an object, and CLS_META is the compare that replaces `.bss`
    // placement.
    if c == 0 || c == CLS_META {
        return budget;
    }
    let sb = g.heap_base + (si << SPAN_LOG);
    if c == CLS_LARGE || c == CLS_LARGE_MID {
        // ONE SPAN OF THE RUN, NOT THE WHOLE RUN. A re-scan exists to answer one
        // question: did a store into an object the collector has already scanned
        // install a reference it has not seen? The store is in the dirtied PAGE,
        // so the page is what has to be re-read -- and a page is a span.
        // Re-scanning the whole run instead makes a store into a big object cost
        // its whole SIZE, and marking can then never terminate.
        let mut head = si;
        if c == CLS_LARGE_MID {
            head = span_aux_of(si);
        }
        if !is_marked(g.heap_base + (head << SPAN_LOG)) {
            return budget;
        }
        scan_object(sb, SPAN_BYTES);
        return charge(budget, SPAN_BYTES);
    }
    let sz = CLASS_SIZE[c as usize];
    let slots = g.class_slots[c as usize] as u32;
    // One bitmap address for the whole span, then a bit test per slot.
    let mw = mark_word_base(si);
    let mut i = 0u32;
    while i < slots {
        if is_marked_at(mw, i * sz) {
            scan_object(sb + i * sz, sz);
        }
        i += 1;
    }
    charge(budget, SPAN_BYTES)
}

/// The COLLECTOR-owned half of the work the mark phase still has to do, in
/// granules: the gray stack, the resumable object scan, and the remainder of a
/// full re-scan pass. The mutator cannot add to it except by making objects
/// reachable, and the collector consumes it monotonically -- which is what makes
/// "did it shrink" mean "did the mark converge".
///
/// It is an estimate and it is allowed to be: what it is asked is whether it went
/// DOWN across a window, not what it is.
fn scan_owed() -> u32 {
    let g = gcm();
    let mut w = g.gray_top;
    if g.partial_base != 0 {
        w += (g.partial_end - g.partial_base - g.partial_off) >> GRANULE_LOG;
    }
    if g.rescan_owed {
        let c = covered_spans();
        if c > g.rescan_cursor {
            w += (c - g.rescan_cursor) << (SPAN_LOG - GRANULE_LOG);
        }
    }
    w
}

/// [`scan_owed`] plus the MUTATOR-owned half -- the spans it wrote that the
/// collector still owes a re-scan.
fn work_owed_now() -> u32 {
    let g = gcm();
    scan_owed() + ((g.dirty_n - g.dirty_cursor) << (SPAN_LOG - GRANULE_LOG))
}

/// Deducts `bytes` of touched heap from a budget denominated in granules,
/// saturating at zero. Saturating rather than wrapping is the difference between
/// a step that overran its budget and a step that runs until the heap ends.
///
/// It also records the UNSATURATED total, which is what makes "no step overran
/// its budget" a measurement rather than a claim.
fn charge(budget: u32, bytes: u32) -> u32 {
    let n = bytes >> GRANULE_LOG;
    let g = gcm();
    g.step_work += n;
    if n >= budget {
        return 0;
    }
    budget - n
}

/// The conservative test, and it is the whole hot loop.
///
/// Only about 6% of heap words fall inside the heap range at all, so the range
/// test rejects nineteen words in twenty and everything after it runs on the
/// twentieth.
///
/// Interior pointers are handled by construction rather than by a search: a span
/// serves exactly one size class, so an address anywhere inside an object
/// resolves to that object's base with a table lookup and a multiply.
///
/// AND IT REJECTS THE COLLECTOR'S OWN METADATA. A chunk is heap MEMORY and never
/// a heap OBJECT: an integer that happens to look like an address inside one must
/// not mark it, because a chunk is not swept, is not freed, and has no mark bit
/// of its own that means anything.
#[inline]
fn mark_candidate(v: u32) {
    let g = gcm();
    if v < g.heap_base || v >= g.heap_top {
        return;
    }
    let mut si = (v - g.heap_base) >> SPAN_LOG;
    let mut c = span_class_of(si) & !CLS_FLAGS;
    if c == 0 || c == CLS_META {
        return;
    }
    if c == CLS_LARGE_MID {
        si = span_aux_of(si);
        c = CLS_LARGE;
    }
    let base;
    if c == CLS_LARGE {
        base = g.heap_base + (si << SPAN_LOG);
    } else {
        let sb = g.heap_base + (si << SPAN_LOG);
        let idx = g.slot_tab[c as usize][((v - sb) >> GRANULE_LOG) as usize];
        if idx == SLOT_NONE {
            return; // the class's tail waste: not an object
        }
        base = sb + (idx as u32) * CLASS_SIZE[c as usize];
    }
    mark_object(base);
}

fn mark_object(base: u32) {
    let g = gcm();
    let d = base - g.heap_base;
    let wa = g.meta_dir[(d >> SLICE_LOG) as usize]
        + META_MARK_OFF
        + (((d >> (GRANULE_LOG + 5)) & (SLICE_WORDS / 32 - 1)) << 2);
    let b = 1u32 << ((d >> GRANULE_LOG) & 31);
    let w = load32(wa);
    if w & b != 0 {
        return;
    }
    store32(wa, w | b);
    // MARKS ARE MONOTONE, so this counter only rises within a collection, and
    // "did it rise across this step" is the only cheap question that means "did
    // the unmarked set shrink".
    g.marked += 1;
    if g.gray_top == GRAY_CAP as u32 {
        // Marked but not queued. The full re-scan pass picks it up.
        g.gray_ovf = true;
        return;
    }
    g.gray[g.gray_top as usize] = base;
    g.gray_top += 1;
}

fn is_marked(base: u32) -> bool {
    let g = gcm();
    let d = base - g.heap_base;
    let wa = g.meta_dir[(d >> SLICE_LOG) as usize]
        + META_MARK_OFF
        + (((d >> (GRANULE_LOG + 5)) & (SLICE_WORDS / 32 - 1)) << 2);
    load32(wa) & (1u32 << ((d >> GRANULE_LOG) & 31)) != 0
}

/// The span-derived size of the object based at `base`. For a large object that
/// is the whole span run, INCLUDING the slack past the requested size -- scanning
/// slack can only over-retain, and the alternative is a header word on every
/// allocation.
fn object_size(base: u32) -> u32 {
    let g = gcm();
    let si = (base - g.heap_base) >> SPAN_LOG;
    let c = span_class_of(si) & !CLS_FLAGS;
    if c == CLS_LARGE {
        return span_aux_of(si) << SPAN_LOG;
    }
    CLASS_SIZE[c as usize]
}

/// Empties the mark stack under a budget, and turns overflow into a full re-scan
/// rather than into a failure.
///
/// Overflow is a performance event and not an error. A newly marked object that
/// does not fit on the stack stays MARKED, so the mark bitmap is still the truth;
/// what is lost is the record that its contents have not been scanned yet.
///
/// A SINGLE OBJECT CAN BE BIGGER THAN A WHOLE STEP'S BUDGET. A guest's 1 MiB
/// `Vec` is 65,536 granules against a 1,024-granule budget, i.e. one ~32 ms tick,
/// which is the stop-the-world pause put back where it was. So an object is
/// scanned through a RESUMABLE cursor and the gray unit really is the granule.
/// One partial scan exists at a time, because gray is drained LIFO one object at
/// a time, and it lives in the metadata struct like everything else -- so it
/// survives a save taken mid-object.
fn drain_gray(mut budget: u32) -> u32 {
    let g = gcm();
    loop {
        if g.partial_base != 0 {
            budget = scan_partial(budget);
            if g.partial_base != 0 {
                return 0; // the object did not fit in what was left
            }
        }
        if g.gray_top == 0 {
            break;
        }
        if budget == 0 {
            return 0;
        }
        g.gray_top -= 1;
        let base = g.gray[g.gray_top as usize];
        g.partial_base = base;
        g.partial_off = 0;
        g.partial_end = base + object_size(base);
    }
    if g.gray_ovf {
        g.gray_ovf = false;
        owe_rescan();
    }
    budget
}

/// Completes the one in-flight object scan if there is one, so that a caller
/// which is about to start another may.
fn finish_partial(mut budget: u32) -> u32 {
    while gcm().partial_base != 0 && budget != 0 {
        budget = scan_partial(budget);
    }
    budget
}

/// Advances the one in-flight object scan by at most `budget` granules, and
/// clears `partial_base` when the object is finished.
fn scan_partial(budget: u32) -> u32 {
    let g = gcm();
    let mut p = g.partial_base + g.partial_off;
    let end = g.partial_end;
    let mut limit = end;
    let n = budget << GRANULE_LOG;
    if n < end - p {
        limit = p + n;
    }
    let from = p;
    while p + 4 <= limit {
        mark_candidate(load32(p));
        p += 4;
    }
    let done = p - from;
    g.partial_off = p - g.partial_base;
    if p >= end {
        g.partial_base = 0;
        g.partial_off = 0;
        g.partial_end = 0;
    }
    charge(budget, done)
}

/// The unbounded scan, kept for the one caller whose unit is a small-class SLOT
/// -- at most 2 KiB, which is a quarter of the default budget and the natural
/// quantum of a span walk. Nothing else may use it: an unbounded scan of an
/// object whose size the guest chooses is the indivisible step [`drain_gray`]
/// exists to prevent.
fn scan_object(base: u32, size: u32) {
    let end = base + size;
    let mut p = base;
    while p + 4 <= end {
        mark_candidate(load32(p));
        p += 4;
    }
}

// THERE IS NO clear_mark_bits, AND THAT IS A FIX RATHER THAN A SAVING.
//
// Wiping the whole bitmap at the start of every collection is O(heap/128) of
// HEAP STORES made while the write barrier is armed, so at a 128 MiB heap it
// dirties 256 pages in one go, which is exactly DIRTY_CAP -- and every
// collection then opens with a mandatory full-heap re-scan.
//
// The invariant that makes it unnecessary is worth stating, because it is what a
// later change must not break:
//
//	AT PHASE_IDLE, EVERY MARK BIT OVER THE COVERED HEAP IS ZERO.
//
// Three things maintain it and there is no fourth writer. `finish_sweep` is the
// only path to PHASE_IDLE and `sweep_step` visits every covered span before it,
// clearing as it goes. `grow_coverage` wipes a chunk when it creates it.
// `grow_heap` adds spans inside an existing chunk, and no bit above the old
// `heap_top` can ever have been set because `mark_candidate`'s range test
// rejects those addresses.

// ---------------------------------------------------------------------------
// Sweeping -- the expensive half, and the half that needs no barrier
// ---------------------------------------------------------------------------

/// Drops every `CLS_PENDING` flag and empties the list. Marking is over or has
/// not begun, so nothing is owed a re-scan either way.
fn clear_pending() {
    let g = gcm();
    let mut i = g.dirty_cursor;
    while i < g.dirty_n {
        let si = g.pend[i as usize];
        let c = span_class_of(si);
        if c & CLS_PENDING != 0 {
            set_span_class(si, c & !CLS_PENDING);
        }
        i += 1;
    }
    g.dirty_n = 0;
    g.dirty_cursor = 0;
}

/// Closes the mark phase and opens the sweep.
///
/// A stop-the-world sweep may reset every class's current run here and a paced
/// one must not: dropping every class's run at the moment marking ends leaves the
/// very next allocation with nothing to bump through and no swept span to refill
/// from, which is a synchronous sweep-ahead in the first tick after every mark.
///
/// So the run is PROTECTED rather than dropped: the sweep skips the slots inside
/// `[cur_ptr, cur_end)` of the class that owns the span, counting them neither
/// live nor free and threading them onto no run.
fn begin_sweep() {
    let g = gcm();
    let mut c = 1u32;
    while c <= NUM_CLASSES {
        g.run_head[c as usize] = 0;
        g.run_tail[c as usize] = 0;
        // THE HOLD WINDOW IS SNAPSHOTTED HERE, and reading the live `cur_ptr`
        // instead reclaims live blocks: `cur_ptr` ADVANCES while the sweep runs,
        // so by the time a span is walked, every block handed out since marking
        // ended is BELOW the live cursor -- outside a window computed from it, and
        // unmarked, because nothing marks after termination.
        g.hold_lo[c as usize] = g.cur_ptr[c as usize];
        g.hold_hi[c as usize] = g.cur_end[c as usize];
        c += 1;
    }
    clear_pending();
    g.mark_forced = false;
    g.sweep_cursor = 0;
    g.large_keep = 0;
    g.live_acc = 0;
    g.free_acc = 0;
    g.freed_acc = 0;
    g.live_obj_acc = 0;
    g.phase = PHASE_SWEEP;
}

/// Sweeps spans under a budget and finishes the collection when the cursor
/// reaches the end.
///
/// It needs no write barrier and that is the point of doing it second. After mark
/// termination the bitmap is not written again by anything, so a mutator running
/// between two sweep steps cannot invalidate a decision this makes.
///
/// The order is deterministic by construction -- spans ascending, slots ascending
/// -- which matters because what lands in `storage` is CRC'd and
/// multiplayer-synchronised, and a free list whose shape depended on iteration
/// order would be a per-client heap layout.
pub(crate) fn sweep_step(mut budget: u32) {
    let g = gcm();
    let covered = covered_spans();
    while g.sweep_cursor < covered {
        if budget == 0 {
            return;
        }
        budget = sweep_span(g.sweep_cursor, budget);
    }
    finish_sweep();
}

/// Sweeps the span (or large-object run) at `si`, advances the cursor past it,
/// and returns the budget left.
fn sweep_span(si: u32, mut budget: u32) -> u32 {
    let g = gcm();
    let raw = span_class_of(si);
    let c = raw & !CLS_FLAGS;
    if c == 0 {
        g.free_acc += SPAN_BYTES;
        g.sweep_cursor = si + 1;
        clear_span_marks(si);
        return budget; // an unassigned span is no work at all
    }
    if c == CLS_META {
        // The collector's own tables. Never swept, never freed, never counted as
        // free -- and its mark words are never set, because `mark_candidate`
        // rejects a CLS_META span, so there is nothing to clear either.
        g.sweep_cursor = si + 1;
        return charge(budget, SPAN_BYTES / 16);
    }
    if raw & CLS_FRESH != 0 {
        // CLAIMED AFTER MARKING TERMINATED. Every slot in it is either live or
        // part of the class's current run, and none of them has a mark bit
        // because nothing marks after termination. Skipping it whole is the same
        // treatment the hold window gives the run the class was already bumping
        // through, generalised to the runs it acquires while the sweep is in
        // flight.
        //
        // The flag is cleared here, and here is the only place it can be:
        // `fresh_bit` sets it only for a span at or above the cursor, so the
        // sweep is guaranteed to arrive.
        if c == CLS_LARGE || c == CLS_LARGE_MID {
            while g.sweep_cursor < covered_spans() {
                let k = span_class_of(g.sweep_cursor);
                if k & CLS_FRESH == 0 {
                    break;
                }
                let kc = k & !CLS_FLAGS;
                if kc != CLS_LARGE && kc != CLS_LARGE_MID {
                    break;
                }
                if kc == CLS_LARGE && g.sweep_cursor != si {
                    break;
                }
                set_span_class(g.sweep_cursor, kc);
                g.sweep_cursor += 1;
                budget = charge(budget, SPAN_BYTES / 16);
            }
            return budget;
        }
        set_span_class(si, c);
        g.sweep_cursor = si + 1;
        return charge(budget, SPAN_BYTES / 16);
    }
    let sb = g.heap_base + (si << SPAN_LOG);
    if c == CLS_LARGE || c == CLS_LARGE_MID {
        // A LARGE RUN IS SWEPT SPAN BY SPAN, RESUMABLY, for the same reason a
        // large object is SCANNED granule by granule: an object whose size the
        // guest chooses must not be an indivisible step.
        //
        // The head decides the run's fate and accounts for it once; `large_keep`
        // carries that decision across a step boundary, because the cursor can
        // stop in the middle of a run and the head's mark bit is cleared on the
        // way past.
        if c == CLS_LARGE {
            let n = span_aux_of(si);
            if is_marked(sb) {
                g.live_acc += n << SPAN_LOG;
                g.live_obj_acc += 1;
                g.large_keep = 1;
            } else {
                g.free_acc += n << SPAN_LOG;
                g.freed_acc += 1;
                g.large_keep = 2;
            }
        }
        let covered = covered_spans();
        while budget > 0 && g.sweep_cursor < covered {
            let k = span_class_of(g.sweep_cursor);
            if k & CLS_FRESH != 0 {
                break; // a run claimed after termination; it gets its own pass
            }
            if k != CLS_LARGE && k != CLS_LARGE_MID {
                break;
            }
            if k == CLS_LARGE && g.sweep_cursor != si {
                break; // the next object's head; it gets its own decision
            }
            clear_span_marks(g.sweep_cursor);
            if g.large_keep == 2 {
                set_span_class(g.sweep_cursor, 0);
                set_span_aux(g.sweep_cursor, 0);
            }
            g.sweep_cursor += 1;
            // Charged for the WORK and not for the bytes: this touches none of the
            // object's contents, so a span of it is a handful of stores against a
            // small-class span's 256 bitmap tests. A sixteenth is the honest order.
            budget = charge(budget, SPAN_BYTES / 16);
        }
        if g.sweep_cursor >= covered || span_class_of(g.sweep_cursor) & !CLS_FRESH != CLS_LARGE_MID
        {
            g.large_keep = 0;
        }
        return budget;
    }

    // A small-class span.
    let sz = CLASS_SIZE[c as usize];
    let slots = g.class_slots[c as usize] as u32;
    // The protected window: if this span holds class c's current run, the blocks
    // between the bump cursor and the run's end belong to the class already.
    let (mut pl, mut ph) = (0u32, 0u32);
    let p = g.hold_lo[c as usize];
    if p != 0 && (p - g.heap_base) >> SPAN_LOG == si {
        pl = p;
        ph = g.hold_hi[c as usize];
    }
    let mw = mark_word_base(si);
    let mut nlive = 0u32;
    let mut nheld = 0u32;
    let mut i = 0u32;
    while i < slots {
        if is_marked_at(mw, i * sz) {
            nlive += 1;
        } else {
            let a = sb + i * sz;
            if a >= pl && a < ph {
                nheld += 1;
            }
        }
        i += 1;
    }
    if nlive == 0 && nheld == 0 {
        // A span that sweeps completely empty is RETURNED to the span allocator
        // rather than kept by its class. That is the anti-fragmentation lever:
        // without it a burst of one size permanently reserves spans another size
        // then has to grow the heap to get, and a heap that grows never un-grows.
        set_span_class(si, 0);
        g.free_acc += SPAN_BYTES;
        g.freed_acc += slots;
        clear_span_marks(si);
        g.sweep_cursor = si + 1;
        return charge(budget, SPAN_BYTES);
    }
    // Dead slots become RUNS, in address order: one `memory.fill` to zero the run
    // and one descriptor written into its first eight bytes. This is what pays for
    // the allocation path having no memory operation in it at all -- an
    // allocate-and-drop workload leaves long runs, so a span a per-slot free list
    // would have called `mem_fill` 256 times for is usually one call here.
    let mut run_start = 0u32;
    let mut run_len = 0u32;
    let mut i = 0u32;
    while i <= slots {
        if i < slots {
            let a = sb + i * sz;
            if !is_marked_at(mw, i * sz) && !(a >= pl && a < ph) {
                if run_len == 0 {
                    run_start = i;
                }
                run_len += 1;
                i += 1;
                continue;
            }
        }
        if run_len != 0 {
            heap::zero(sb + run_start * sz, run_len * sz);
            push_run(c, sb + run_start * sz, sb + (run_start + run_len) * sz);
            run_len = 0;
        }
        i += 1;
    }
    g.live_acc += nlive * sz;
    g.live_obj_acc += nlive;
    g.free_acc += (slots - nlive - nheld) * sz;
    g.freed_acc += slots - nlive - nheld;
    clear_span_marks(si);
    g.sweep_cursor = si + 1;
    charge(budget, SPAN_BYTES)
}

fn finish_sweep() {
    let g = gcm();
    g.live_bytes = g.live_acc;
    g.live_objs = g.live_obj_acc;
    g.free_bytes = g.free_acc;
    // `freed_objs` counts slots returned to a free list plus slots inside released
    // spans -- everything this sweep made available.
    g.freed_objs = g.freed_acc;
    // The hold windows are done with: every span they covered has been swept, so
    // the blocks in them are ordinary members of their class's current run again
    // and the next collection will decide them on the bitmap like anything else.
    let mut c = 1u32;
    while c <= NUM_CLASSES {
        g.hold_lo[c as usize] = 0;
        g.hold_hi[c as usize] = 0;
        c += 1;
    }
    // A released span is only reachable to the span allocator if the cursor can
    // reach it, and the cursor only moves forward.
    g.span_cursor = 0;
    g.collections += 1;
    g.since_gc = 0;
    g.last_steps = g.steps;
    g.phase = PHASE_IDLE;
}

// ---------------------------------------------------------------------------
// The host-facing surface: three exports and one import.
// ---------------------------------------------------------------------------

/// The address the host writes dirtied page numbers to, as i32 words.
///
/// It mirrors the `fk_scratch_base` / `fk_scratch_size` pair the string ABI
/// already uses, and for the same reason: the GUEST owns the address, because a
/// pointer the host invented would land in the middle of something.
pub fn dirty_base() -> u32 {
    core::ptr::addr_of!(gcm().dirty_q[0]) as u32
}

/// How many page numbers the buffer holds. A count larger than this arrives as
/// [`DIRTY_ALL`].
pub fn dirty_cap() -> u32 {
    DIRTY_CAP
}

/// One bounded collection step, paced from a one-shot `on_tick`.
///
/// The export is written HERE, in the collector crate, and it survives into a
/// cdylib because a `#[no_mangle]` item in a linked rlib does --
/// `TestARustCollectedGuestCarriesTheCollectorSurface` is what stops that being
/// an assumption, since a `--gc=collected` build whose exports were dropped would
/// arm a barrier with nothing behind it.
#[no_mangle]
pub extern "C" fn fk_gc_step(ndirty: u32) -> u32 {
    step(ndirty)
}

/// Where the host writes the dirtied page numbers.
#[no_mangle]
pub extern "C" fn fk_gc_dirty_base() -> u32 {
    dirty_base()
}

/// How many dirtied page numbers that buffer holds.
#[no_mangle]
pub extern "C" fn fk_gc_dirty_cap() -> u32 {
    dirty_cap()
}

// Asks the host to arm the write barrier and schedule collection steps until
// the collection finishes.
//
// It is `fk.defer` with a different payload: a one-shot `on_tick` registered
// only while there is something to do, torn down by its own handler, with the
// armed flag mirrored into `storage` because Factorio does not save event
// registrations. An idle guest registers nothing and pays nothing.
#[link(wasm_import_module = "fk")]
extern "C" {
    #[link_name = "gc"]
    fn host_gc_pace() -> u32;
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

/// The most collector work, in granules, that landed in a single GUEST CALL
/// rather than in a paced step -- the sweep-ahead in `alloc_spans`, and the
/// initial root scan of a collection the guest started.
///
/// It is the number [`max_step_work`] could never see. Both belong in a
/// worst-tick claim: a tick's collector cost is one step plus whatever the
/// handler in that tick charged.
pub fn max_unpaced_work() -> u32 {
    gcm().max_unpaced_work
}

/// The same quantity summed over the current (or last) collection.
pub fn unpaced_work() -> u32 {
    gcm().unpaced_work
}

/// How many separate bursts made up [`max_unpaced_work`]. One means a single
/// indivisible unit; many means an allocation loop that kept asking.
pub fn max_unpaced_folds() -> u32 {
    gcm().max_unpaced_folds
}

/// How many times an allocation had to grow the heap while a collection was
/// still running. It is the honest measure of "the mutator beat the pacer", and
/// the response to it is growth rather than a pause.
pub fn outruns() -> u32 {
    gcm().outruns
}

/// How many objects this collection has marked.
pub fn marked() -> u32 {
    gcm().marked
}

/// How many CONSECUTIVE windows of mark steps have failed to reduce the work
/// still owed.
pub fn stalls() -> u32 {
    gcm().stalls
}

/// The longest such run this collection has seen.
pub fn max_stalls() -> u32 {
    gcm().max_stalls
}

/// How many mark steps this collection ended with the pending dirty list empty.
/// Zero over a long mark is the mutator winning.
pub fn pend_empties() -> u32 {
    gcm().pend_empties
}

/// The scalar behind [`stalls`]: the scan work plus the dirty work still owed.
pub fn work_owed() -> u32 {
    if gcm().phase != PHASE_MARK {
        return 0;
    }
    work_owed_now()
}

/// How many 4-byte words the last root scan read. A guest with large statics
/// pays it at every termination attempt, and it is charged against the step
/// budget rather than being free by omission.
pub fn root_words() -> u32 {
    gcm().root_words
}

/// Mark-termination attempts in the current (or last) collection. One is the
/// healthy number.
pub fn terminations() -> u32 {
    gcm().terminations
}

/// Completed full re-scan passes.
pub fn rescans() -> u32 {
    gcm().rescans
}

/// How many times a pass was restarted from span zero because the record of what
/// changed was lost.
pub fn rescan_restarts() -> u32 {
    gcm().rescan_restarts
}

/// How many times the record of what changed was lost -- a dirty set larger than
/// the buffer, a gray-stack overflow, or a resumed save. A performance event
/// rather than an error.
pub fn dirty_overflows() -> u32 {
    gcm().dirty_overflows
}

/// How many times mark termination stopped yielding to the budget because it was
/// making no progress. Zero is the expected value forever.
pub fn deadlines() -> u32 {
    gcm().deadlines
}

/// Counts the set mark bits over the covered heap. It is O(heap) and exists for
/// one assertion: at [`PHASE_IDLE`] the answer must be zero, which is the
/// invariant that lets the start of a collection skip a bitmap wipe. Nothing on a
/// hot path calls it.
pub fn mark_bits_set() -> u32 {
    let mut n = 0u32;
    let covered = covered_spans();
    let mut si = 0u32;
    while si < covered {
        let w = mark_word_base(si);
        let mut k = 0u32;
        while k < MARK_WORDS_PER_SPAN {
            let mut v = load32(w + (k << 2));
            while v != 0 {
                n += v & 1;
                v >>= 1;
            }
            k += 1;
        }
        si += 1;
    }
    n
}
