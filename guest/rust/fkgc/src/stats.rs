//! What a guest can ask the collector about itself.
//!
//! The mirror of `guest/go/fkgc/stats.go`, field for field and comment for
//! comment, because a guest that logs one of these numbers in Rust and the same
//! number in Go must be logging the same thing -- that is what makes the
//! corpus-mirror comparison a comparison.

use crate::heap::gcm;
use crate::meta::meta_bytes;

/// Every field is bytes except the counters, and every field is exact rather
/// than sampled: this allocator knows its own free lists and its own span
/// assignment, so there is nothing here it has to estimate.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct MemStats {
    /// The size of the region the allocator owns: every byte between the heap
    /// base and the last byte the allocator has grown into. **This is the number
    /// that must stop growing.** It is also, within one wasm page, the guest's
    /// whole contribution to Factorio's 0.2 ms/MiB worst tick.
    pub heap_bytes: u32,

    /// The size-class total of everything the last collection retained. Between
    /// collections it is stale by construction -- it is what was live when the
    /// sweep ran, not what is live now.
    pub live_bytes: u32,

    /// What the free lists and the unassigned spans hold: the headroom before
    /// the next `memory.grow`.
    pub free_bytes: u32,

    /// How many bytes of SPAN have been assigned since the last collection
    /// finished -- heap footprint the guest has taken, not bytes it has been
    /// handed. This is the number [`crate::collect_if_needed`] tests against the
    /// threshold, and the distinction is the point: a class recycling blocks a
    /// collection reclaimed has not made the heap bigger, so it registers no
    /// pressure and provokes no collection.
    pub since_gc: u32,

    /// How many times a mark and sweep has completed.
    pub collections: u32,

    /// Objects the last sweep kept and released.
    ///
    /// There is deliberately no "allocations so far" counter. A running total is
    /// a read-modify-write on the allocation path, and that is precisely what
    /// this allocator does not carry -- it gets under a bump arena's own
    /// allocation cost only by carrying nothing per allocation at all.
    pub live_objects: u32,
    pub freed_objects: u32,

    /// How many times `memory.grow` has been called. This is the counter an
    /// acceptance run watches: the test for a churning guest is "no doubling
    /// logged", and a collector that works holds this flat forever.
    pub grows: u32,

    /// What the collector is doing right now: 0 idle, 1 marking, 2 sweeping. A
    /// guest that logs this is logging the one thing a paced collector has that
    /// a stop-the-world one does not -- a middle.
    pub phase: u32,

    /// How many bounded steps the LAST completed collection took. Steps times
    /// the budget is roughly the work the cycle did and steps is roughly the
    /// ticks it was spread over, so a rising number against a flat heap means the
    /// budget is too small for the guest's allocation rate and the heap is about
    /// to grow instead.
    pub steps: u32,

    /// How many times an allocation had to GROW the heap while a collection was
    /// still in flight -- the mutator beating the pacer.
    ///
    /// It is not an error and it does not pause: the storm response is growth, so
    /// a guest that outruns its budget behaves like a leaking arena until the
    /// paced collection catches up. What it costs is linear memory, which never
    /// shrinks.
    pub outruns: u32,

    /// Collector work, in 16-byte granules, that landed inside a GUEST CALL
    /// rather than inside a paced step: the bounded sweep-ahead an allocation
    /// does before it grows, and the root scan of a collection the guest started.
    ///
    /// They exist because `max_step_work` cannot see any of it -- a step zeroes
    /// its own accumulator on entry, so work charged between two steps was
    /// discarded before it was ever compared to the budget. A worst-tick claim
    /// needs both: one step, plus whatever the handler in that same tick charged.
    pub unpaced_work: u32,
    pub max_unpaced: u32,

    /// The collector's own linear-memory footprint, and it SCALES WITH THE HEAP
    /// rather than being a compile-time constant: a fixed part in `.bss` plus one
    /// 40 KiB chunk per 4 MiB slice of heap. There is no heap cap, and this is
    /// what replaced it -- a cost that grows with what the guest asked for,
    /// rather than a wall.
    pub meta_bytes: u32,

    /// How many times mark termination stopped yielding to the budget because it
    /// was making no progress. Zero is the expected value forever, so a number
    /// that rises is a defect report about the guest's configuration rather than
    /// a statistic.
    ///
    /// IT IS THE SUM OF THE TWO BELOW, exactly, and it is kept unchanged so that
    /// every existing reader keeps reading the same number.
    pub deadlines: u32,

    /// `deadlines` SPLIT BY CAUSE, and the split exists because one counter was
    /// reporting two diagnoses with two different remedies.
    ///
    /// - `step_escapes`: the mark ran past `mark_deadline` -- `4 * (heap
    ///   granules / budget) + 600` steps. A BACKSTOP, deliberately far enough
    ///   out that a short run finishes first, so on its own it says the mark is
    ///   affordable but SLOW for the heap it is on.
    /// - `stall_escapes`: the forward-progress window said the mark had stopped
    ///   converging -- `MARK_STALL_LIMIT` consecutive windows of
    ///   `MARK_STALL_WINDOW` steps in which the pending dirty list never emptied
    ///   AND scan work did not shrink. A DIAGNOSIS, and far earlier than the
    ///   backstop.
    ///
    /// THE REASON TO SPLIT THEM IS TWO REAL MISDIAGNOSES. `deadlines` is
    /// documented -- here, on `set_budget` and in agents/gc.md -- as the signal
    /// that the mutator has outrun the collector, and that reading sent the first
    /// downstream mod's investigation at its own write rate for a day; what was
    /// happening was the root scan costing more than one step's whole budget, at
    /// any allocation rate including zero. The same mod then wrote six of these
    /// down as the write rate of the two legs they were counted in -- one of
    /// which allocates 16 bytes per operation and could not outrun anything.
    ///
    /// This pair does not identify the root-scan case by itself --
    /// `effective_budget() > budget()` is what does that -- but it stops one
    /// number being read as evidence for a cause it never carried.
    pub step_escapes: u32,
    pub stall_escapes: u32,
}

/// Reports what the collector knows about itself.
pub fn stats() -> MemStats {
    let g = gcm();
    MemStats {
        heap_bytes: g.heap_top - g.heap_base,
        live_bytes: g.live_bytes,
        free_bytes: g.free_bytes,
        since_gc: g.since_gc,
        collections: g.collections,
        live_objects: g.live_objs,
        freed_objects: g.freed_objs,
        grows: g.grows,
        phase: g.phase as u32,
        steps: g.last_steps,
        outruns: g.outruns,
        unpaced_work: g.unpaced_work,
        max_unpaced: g.max_unpaced_work,
        meta_bytes: meta_bytes(),
        deadlines: g.deadlines,
        step_escapes: g.step_escapes,
        stall_escapes: g.stall_escapes,
    }
}
