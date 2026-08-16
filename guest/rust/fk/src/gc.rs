//! The collector's surface when there is no collector.
//!
//! This module is the whole of `fk::gc` without the `fkgc` feature, and it is
//! the mirror of `guest/go/fkgc/off.go` -- which exists for the same reason and
//! says so:
//!
//! > It exists so that `import _ ".../fkgc"` is unconditional in guest source. A
//! > guest is built at different `-gc` settings by the same harnesses, and a
//! > guest author should not have to put a build tag on an import to change one
//! > flag.
//!
//! Nothing here allocates, links against anything, or has any state, so a
//! leaking build that calls through this emits what it emitted before. In
//! particular it declares **no** `fk_gc_step` / `fk_gc_dirty_base` /
//! `fk_gc_dirty_cap`, which is what makes `fklua compile --gc=collected` refuse
//! such a guest instead of arming a barrier with nothing behind it.
//!
//! **IT IS A MIRROR, AND A MIRROR CHECKED IN ONE DIRECTION DRIFTS IN THE
//! OTHER.** `TestTheRustCollectorShimMirrorsTheCollector` compares this file's
//! public functions and [`MemStats`]'s fields against `guest/rust/fkgc`'s, in
//! both directions, because that is the failure `CLAUDE.md` records
//! `factorio.Hooks` sitting on for two milestones.

/// Reports whether a collector is compiled in. False without the `fkgc` feature.
pub const fn enabled() -> bool {
    false
}

/// Brings the collector up. A no-op without one.
pub fn init() {}

/// A no-op without a collector.
pub fn collect() {}

/// A no-op without a collector.
pub fn collect_if_needed() -> bool {
    false
}

/// A no-op without a collector: there is nothing to collect and no barrier to
/// arm.
pub fn start() -> bool {
    false
}

/// A no-op without a collector, reporting idle. It exists so that a guest
/// driving the collector by hand compiles under both arms.
pub fn step(_ndirty: u32) -> u32 {
    0
}

/// A no-op without a collector.
pub fn set_threshold(_bytes: u32) {}

/// A no-op without a collector: there are no steps to pace.
pub fn set_budget(_units: u32) {}

/// The per-step work allowance, zero without a collector.
pub fn budget() -> u32 {
    0
}

/// The per-step allowance floored at what a root re-scan costs. Zero without a
/// collector: there is no scan to pay for.
pub fn effective_budget() -> u32 {
    0
}

/// What the collector is doing: always idle without one.
pub fn phase() -> u32 {
    0
}

/// The most any single collection step charged. There are no steps without a
/// collector.
pub fn max_step_work() -> u32 {
    0
}

/// What a collection charged in total. There are no collections without a
/// collector.
pub fn total_work() -> u32 {
    0
}

/// Where the host writes dirtied page numbers. There is no barrier without a
/// collector, so there is no buffer.
pub fn dirty_base() -> u32 {
    0
}

/// Zero without a collector.
pub fn dirty_cap() -> u32 {
    0
}

/// The count a step is handed when the host cannot say which pages were written.
/// Declared in both arms so a guest can name it either way.
pub const DIRTY_ALL: u32 = u32::MAX;

/// Zero without a collector: this build does not own the heap, the bump arena
/// does.
pub fn heap_base() -> u32 {
    0
}

/// Zero without a collector.
pub fn heap_top() -> u32 {
    0
}

/// Zero without a collector.
pub fn backed_bytes() -> u32 {
    0
}

/// Destructive re-initialisation, for tests. Nothing to re-initialise here.
pub fn reinitialize() {}

/// The scaling-metadata model, all zero here, so that a guest logging its own
/// memory model compiles under both arms.
pub fn meta_bytes() -> u32 {
    0
}

/// Zero without a collector.
pub fn meta_fixed_bytes() -> u32 {
    0
}

/// Zero without a collector.
pub fn meta_chunks() -> u32 {
    0
}

/// Zero without a collector.
pub fn meta_chunk_bytes() -> u32 {
    0
}

/// Zero without a collector.
pub fn meta_slice_bytes() -> u32 {
    0
}

/// The pacing diagnostics, all zero without a collector.
pub fn max_unpaced_work() -> u32 {
    0
}

/// Zero without a collector.
pub fn unpaced_work() -> u32 {
    0
}

/// Zero without a collector.
pub fn max_unpaced_folds() -> u32 {
    0
}

/// Zero without a collector.
pub fn outruns() -> u32 {
    0
}

/// Zero without a collector.
pub fn marked() -> u32 {
    0
}

/// Zero without a collector.
pub fn stalls() -> u32 {
    0
}

/// Zero without a collector.
pub fn max_stalls() -> u32 {
    0
}

/// Zero without a collector.
pub fn pend_empties() -> u32 {
    0
}

/// Zero without a collector.
pub fn work_owed() -> u32 {
    0
}

/// Zero without a collector.
pub fn root_words() -> u32 {
    0
}

/// Zero without a collector.
pub fn terminations() -> u32 {
    0
}

/// Zero without a collector.
pub fn mark_bits_set() -> u32 {
    0
}

/// Zero without a collector.
pub fn rescans() -> u32 {
    0
}

/// Zero without a collector.
pub fn rescan_restarts() -> u32 {
    0
}

/// Zero without a collector.
pub fn dirty_overflows() -> u32 {
    0
}

/// Zero without a collector.
pub fn deadlines() -> u32 {
    0
}

/// A zero value without a collector.
///
/// `heap_bytes` being zero is the honest answer for a leaking build in the sense
/// that matters: nothing here can see the bump arena's own pointer, and the
/// number this field exists to report -- the size that must stop growing -- is
/// one only a collector knows.
///
/// THERE IS NO HEAP CAP, IN EITHER ARM. `guest/go/fkgc` carried one until the
/// metadata was made to scale; this port never had one. A collected Rust guest
/// grows exactly like a leaking one, on a sharded linear memory with no wall in
/// it, and what replaced the cap is a COST -- [`meta_bytes`] -- and not a bigger
/// number.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct MemStats {
    pub heap_bytes: u32,
    pub live_bytes: u32,
    pub free_bytes: u32,
    pub since_gc: u32,
    pub collections: u32,
    pub live_objects: u32,
    pub freed_objects: u32,
    pub grows: u32,
    pub phase: u32,
    pub steps: u32,
    pub outruns: u32,
    pub unpaced_work: u32,
    pub max_unpaced: u32,
    pub meta_bytes: u32,
    pub deadlines: u32,
    /// `deadlines` split by cause; their sum is `deadlines`. Mirrors
    /// `fkgc::MemStats`, which this struct has to field for field: an
    /// example builds against BOTH arms from one source, so a field only
    /// the collected arm has is a leaking build that does not compile.
    pub step_escapes: u32,
    pub stall_escapes: u32,
}

/// A zero value without a collector.
pub fn stats() -> MemStats {
    MemStats::default()
}
