//! Shape, metadata and allocation.
//!
//! The mirror of `guest/go/fkgc/heap.go`. Everything structural is the same
//! because the design is the same; what differs is written down where it
//! differs, and there are exactly four places:
//!
//!   * there is no `-gc=custom` contract to satisfy, so the seven `//go:linkname`
//!     hooks become one [`Collector`] and the runtime never calls us at all;
//!   * `initialize` is LAZY, because Rust has no `initHeap` the runtime invokes
//!     -- it is funnelled through [`alloc_spans`], which every path that needs a
//!     heap goes through, so the hot path pays nothing for it;
//!   * `Layout` carries an ALIGNMENT that Go's `alloc` does not, and an
//!     over-aligned request picks a size class that is a multiple of it;
//!   * a refused `memory.grow` traps instead of collecting. See `alloc_spans`,
//!     and `lib.rs` for why that one is not negotiable here.

use core::alloc::{GlobalAlloc, Layout};
use core::cell::UnsafeCell;

use crate::collect::{self, DIRTY_CAP, PEND_CAP, PHASE_IDLE, PHASE_SWEEP, SWEEP_AHEAD_UNITS};
use crate::meta::{
    self, covered_spans, grow_coverage, sync_heap_top, MAX_CHUNKS, META_CHUNK_SPANS, SLICE_LOG,
    SLICE_SPANS,
};

// ---------------------------------------------------------------------------
// Shape
// ---------------------------------------------------------------------------

/// The allocation quantum and the resolution of the mark bitmap.
///
/// 16 is what TinyGo aligns its heap to and it is `align_of::<u128>()` here, so
/// it covers every ordinary Rust type; an object base is 16-aligned by
/// construction and a granule index identifies one. An over-aligned `Layout` is
/// handled by [`alloc_over_aligned`] rather than by widening this.
pub(crate) const GRANULE: u32 = 16;
pub(crate) const GRANULE_LOG: u32 = 4;

/// The unit the heap is carved into, and 4 KiB is not arbitrary: it is
/// `--persist=packed`'s page size. A span and a dirty page are the same object,
/// which is what lets a step intersect the dirty-page set with span metadata
/// without a second mapping.
pub(crate) const SPAN_BYTES: u32 = 4096;
pub(crate) const SPAN_LOG: u32 = 12;

/// The mark stack depth, in objects. Overflow is handled rather than fatal (see
/// `drain_gray`), so this is a performance parameter and not a limit -- which is
/// what lets it be small. It is `.bss`, and `.bss` is linear memory, and linear
/// memory is 0.2 ms of Factorio worst tick per MiB whether or not anything is in
/// it.
pub(crate) const GRAY_CAP: usize = 4096;

/// Size-class ladder. Power-of-two-ish, every entry a multiple of the granule,
/// every entry dividing 4 KiB with at most ~6% tail waste.
pub(crate) const NUM_CLASSES: u32 = 21;
pub(crate) const MAX_SMALL: u32 = 2048;
/// A span run serving one big object.
pub(crate) const CLS_LARGE: u32 = NUM_CLASSES + 1;
/// A continuation span of such a run.
pub(crate) const CLS_LARGE_MID: u32 = NUM_CLASSES + 2;
/// A chunk of the collector's own metadata.
pub(crate) const CLS_META: u32 = NUM_CLASSES + 3;

/// OR'd into a span's class word when the MUTATOR claims that span AFTER
/// marking terminated, i.e. during a sweep the cursor has not yet reached.
///
/// IT IS WHAT REPLACES THE SWEEP-CURSOR WINDOW. A span carrying it was claimed
/// after the bitmap froze, so every slot in it is either live or part of the
/// class's current run, and the sweep must skip it whole and clear the flag. It
/// is set ONLY when `si >= sweep_cursor` -- below the cursor the sweep has
/// already decided the span and will never revisit it, so a flag left there
/// would survive into the next cycle and leak the span forever.
pub(crate) const CLS_FRESH: u32 = 1 << 8;

/// Marks a span that is ALREADY IN THE PENDING DIRTY LIST, and it is what makes
/// that list a SET. Without it a page the mutator writes every tick takes a new
/// slot every tick, the owed work climbs without bound, and no forward-progress
/// metric can tell that apart from a big heap being marked slowly.
pub(crate) const CLS_PENDING: u32 = 1 << 9;

pub(crate) const CLS_FLAGS: u32 = CLS_FRESH | CLS_PENDING;

/// `CLASS_SIZE[c]` is the byte size class `c` hands out. Index 0 is "no class"
/// and doubles as "this span is unassigned", which is what makes a zeroed span
/// table the correct initial state.
pub(crate) const CLASS_SIZE: [u32; NUM_CLASSES as usize + 1] = [
    0, 16, 32, 48, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 640, 768, 1024,
    1280, 2048,
];

/// "This offset is the class's tail waste and not an object."
///
/// IT MUST BE OUTSIDE THE RANGE OF REAL SLOT INDICES, which the assertion below
/// enforces rather than states. The largest index any class can produce is
/// `SPAN_BYTES / GRANULE - 1`, reached by the smallest class, whose objects ARE
/// granules; a sentinel at or below that is a live object the mark phase cannot
/// see. It was 255 against a smallest class of exactly 256 slots per span, and
/// the last block of every such span was therefore unmarkable and still
/// sweepable -- see the `slot_tab` field for the whole of it.
pub(crate) const SLOT_NONE: u16 = 0xFFFF;

/// The biggest slot index the ladder can produce, from the smallest class.
pub(crate) const MAX_SLOT_INDEX: u16 = (SPAN_BYTES / GRANULE - 1) as u16;

// The invariant above, held by the compiler rather than by the comment.
const _: () = assert!(SLOT_NONE > MAX_SLOT_INDEX);

/// How many bytes may be handed out between collections before
/// [`crate::collect_if_needed`] says yes. 256 KiB is two orders of magnitude
/// below the point where the Lua-side tail becomes measurable and comfortably
/// above one blueprint paste's 403 KiB -- which is to say a paste triggers
/// exactly one collection, which is the intent.
pub(crate) const DEFAULT_THRESHOLD: u32 = 256 << 10;

// ---------------------------------------------------------------------------
// Metadata
//
// EVERYTHING mutable this crate owns is one struct, and that is a correctness
// requirement rather than tidiness.
//
// The root scan covers [__global_base, __heap_base), and a `static` is inside
// it -- so without care the mark bitmap directory would be scanned as roots,
// every free-list head would keep a free object alive through the sweep that is
// supposed to rebuild them from scratch, and every metadata chunk would be
// marked live by the collector's own bookkeeping. One struct is one contiguous
// address range, so `scan_roots` subtracts it with two compares. Separate
// statics are not guaranteed adjacent and could not be subtracted at all.
// ---------------------------------------------------------------------------

pub(crate) struct GcMeta {
    /// `meta_dir[k]` is the address of the chunk holding the mark bitmap, the
    /// span table and the span-aux table for the k'th 4 MiB slice of heap, or
    /// zero if that slice is not covered yet. See `meta.rs`.
    ///
    /// IT IS A FIELD OF THIS STRUCT for a reason sharper than the rest of it:
    /// it holds HEAP ADDRESSES. A directory outside `[meta_lo, meta_hi)` would
    /// be scanned as roots, so every chunk would be marked as a live object by
    /// the collector's own bookkeeping and the sweep would then keep them --
    /// which is the one failure that looks like it works.
    pub meta_dir: [u32; MAX_CHUNKS as usize],
    /// How many entries of `meta_dir` are live. Coverage grows upward and never
    /// shrinks, so this is also the index of the next chunk to create.
    pub chunks: u32,

    /// The mark stack, holding object base addresses.
    pub gray: [u32; GRAY_CAP],
    pub gray_top: u32,
    pub gray_ovf: bool,

    /// `slot_tab[c][off>>4]` is the slot index an offset within a span belongs
    /// to, or [`SLOT_NONE`] if the offset is in the class's tail waste. This is
    /// what resolves an INTERIOR POINTER in O(1) with no division, and a
    /// division per candidate would be a helper call in the emitted Lua.
    ///
    /// IT IS `u16` AND IT MUST BE. An entry has to represent every real slot
    /// index PLUS a sentinel, and the smallest class is the granule itself --
    /// so a 4 KiB span holds 4096/16 = 256 of them and the last one's index is
    /// 255. `SLOT_NONE` was 255. `mark_candidate` read that entry, saw the
    /// sentinel, concluded "tail waste, not an object", and marked nothing: the
    /// last 16-byte block of every span was invisible to the conservative scan
    /// while remaining perfectly sweepable, so a live one was freed under a
    /// reference that was still standing. One class in twenty-one could reach
    /// it -- every larger class fits at most 128 objects in a span.
    ///
    /// The width costs 5,632 bytes of `.bss`, which at agents/gc.md's own rate
    /// (0.2 ms of Factorio worst tick per MiB) is 0.0011 ms. The alternative
    /// that keeps the byte is a second bound to test per candidate, i.e. a load
    /// in `mark_candidate`'s hot path, and that is the more expensive half.
    pub slot_tab: [[u16; (SPAN_BYTES / GRANULE) as usize]; NUM_CLASSES as usize + 1],
    /// How many objects of class `c` fit in a span.
    pub class_slots: [u16; NUM_CLASSES as usize + 1],
    /// `size_to_class[(n+15)>>4]` is the class serving `n` bytes, for
    /// `n <= MAX_SMALL`.
    pub size_to_class: [u8; (MAX_SMALL / GRANULE) as usize + 1],

    /// The class's current run: the bump cursor and the byte past its last
    /// block. They are the ONLY state [`Collector::alloc`] touches.
    pub cur_ptr: [u32; NUM_CLASSES as usize + 1],
    pub cur_end: [u32; NUM_CLASSES as usize + 1],
    /// The class's remaining free runs, threaded through the first eight bytes
    /// of each run. See `push_run`.
    pub run_head: [u32; NUM_CLASSES as usize + 1],
    pub run_tail: [u32; NUM_CLASSES as usize + 1],
    /// The class's current run AS IT STOOD WHEN MARKING TERMINATED. A paced
    /// sweep runs while the mutator allocates, so `cur_ptr` has moved on by the
    /// time the sweep reaches the span holding it; the blocks in between are
    /// live and unmarked, and a window computed from the live cursor misses
    /// every one of them. See `begin_sweep`.
    pub hold_lo: [u32; NUM_CLASSES as usize + 1],
    pub hold_hi: [u32; NUM_CLASSES as usize + 1],

    /// Where the HOST writes the page numbers written since the last collection
    /// step -- the `MEMDIRTY` page set, handed across the boundary.
    ///
    /// It is a field of this struct for the reason the struct exists at all: it
    /// has to be inside `[meta_lo, meta_hi)` so that `scan_roots` subtracts it.
    /// Otherwise every page number in it would be read as a candidate pointer at
    /// every termination attempt -- and a page number is a small integer, which
    /// is exactly the shape the conservative range test is least able to reject.
    pub dirty_q: [u32; DIRTY_CAP as usize],

    /// The collector's own pending list of dirtied page numbers, and it SURVIVES
    /// A STEP where `dirty_q` does not: the host overwrites the landing pad at
    /// every step.
    pub pend: [u32; PEND_CAP as usize],
    pub dirty_n: u32,
    pub dirty_cursor: u32,

    /// "The record of what changed was lost, assume everything did", and how far
    /// the resumable full pass has got. Three things set it: gray-stack
    /// overflow, a dirty record that did not fit, and a collection resumed after
    /// a save.
    pub rescan_owed: bool,
    pub rescan_cursor: u32,
    /// The fate of the large-object run the sweep cursor is inside: 0 none,
    /// 1 keeping, 2 freeing.
    pub large_keep: u8,

    /// The one in-flight object scan. A gray unit has to be a GRANULE and not a
    /// whole object, or a guest's 1 MiB `Vec` is an indivisible 32 ms step.
    /// `partial_base` is zero when there is none; zero is never a heap address.
    pub partial_base: u32,
    pub partial_off: u32,
    pub partial_end: u32,

    /// What the current step has charged, UNSATURATED, and the largest any step
    /// of the current collection charged. A saturating budget hides the one case
    /// worth knowing about -- an indivisible unit bigger than the whole
    /// allowance -- so it is counted separately.
    pub step_work: u32,
    pub max_work: u32,
    pub total_work: u32,

    /// The work done INSIDE A GUEST CALL rather than inside a paced step, which
    /// is the only collector work that can land in the middle of an event
    /// handler and is therefore the only collector work a pause budget cannot
    /// see. Two sources here: the bounded sweep-ahead in [`alloc_spans`], and
    /// the initial root scan of a collection the guest started.
    pub unpaced_work: u32,
    pub max_unpaced_work: u32,
    pub call_folds: u32,
    pub max_unpaced_folds: u32,
    pub call_work: u32,

    /// How many 4-byte words the LAST termination attempt scanned as roots.
    pub root_words: u32,
    pub terminations: u32,
    pub rescans: u32,
    pub rescan_restarts: u32,
    pub dirty_overflows: u32,
    /// How many times an allocation had to GROW the heap while a collection was
    /// in flight, i.e. the times the mutator beat the pacer.
    pub outruns: u32,

    pub span_count: u32,
    pub meta_spans: u32,
    pub span_cursor: u32,
    pub sweep_cursor: u32,

    /// The sweep's accumulators, separate from the published `live_bytes` /
    /// `free_bytes` because a paced sweep is only half-true until it finishes.
    pub live_acc: u32,
    pub free_acc: u32,
    pub freed_acc: u32,
    pub live_obj_acc: u32,

    pub free_bytes: u32,
    pub live_bytes: u32,
    pub live_objs: u32,
    pub freed_objs: u32,
    pub since_gc: u32,
    pub threshold: u32,
    pub budget: u32,
    pub collections: u32,
    pub grows: u32,
    pub steps: u32,
    pub last_steps: u32,
    pub deadlines: u32,
    /// `deadlines` SPLIT BY CAUSE, and their sum is `deadlines` exactly. The
    /// two escapes are different diagnoses with different remedies and only
    /// one number reported them; see `MemStats::step_escapes`.
    pub step_escapes: u32,
    pub stall_escapes: u32,
    /// This collection's mark-phase step limit, fixed when it started.
    pub mark_limit: u32,
    /// The collection state machine: idle, marking, sweeping. It is in linear
    /// memory like everything else here, which is what makes a save taken
    /// between two steps of one collection carry the collection.
    pub phase: u8,
    pub marked: u32,
    pub stalls: u32,
    pub max_stalls: u32,
    pub stall_steps: u32,
    pub owed_mark: u32,
    pub pend_emptied: bool,
    pub pend_empties: u32,
    /// The latched livelock escape: once the mark phase has been shown not to
    /// converge under the budget, every remaining mark step of THIS collection
    /// runs unbudgeted. Cleared when the sweep opens.
    pub mark_forced: bool,
    /// A RE-ENTRANCY guard and not the phase. True only while a step is
    /// executing, so that the valve in [`alloc_spans`] can tell "a collection is
    /// in progress" from "we are inside the collector right now".
    pub collecting: bool,
    /// Whether the outrun line has been logged. Once per guest: the valve firing
    /// repeatedly is a guest in trouble and a line per allocation would be a
    /// second problem on top of the first.
    pub valve_warned: bool,
    /// Whether the "root set larger than the budget" line has been logged. Once
    /// per guest: the condition is a property of the guest's statics, which do
    /// not change while it runs.
    pub root_warned: bool,

    // The bounds. They are FIELDS OF THIS STRUCT and the Go collector keeps the
    // equivalents as separate package-level vars, which is the one place this
    // port deliberately improves on it: `heap_base` and `heap_top` are heap
    // ADDRESSES, so outside the subtracted range they are two candidate pointers
    // the root scan feeds to `mark_candidate` at every termination attempt, and
    // `heap_base` retains whatever object is based at span zero for nothing.
    /// 4 KiB-aligned; spans are indexed from here.
    pub heap_base: u32,
    /// The exclusive top of the COVERED heap -- `heap_base` plus covered spans,
    /// not plus `span_count` -- and the distinction is load-bearing.
    ///
    /// It is `mark_candidate`'s whole range test, and `mark_candidate`'s next
    /// act is to read the span class through `meta_dir`. A candidate pointing
    /// into the backed-but-uncovered gap between the two would index a directory
    /// entry that is still ZERO, so the class would be read out of the guest's
    /// DATA segment -- and, worse, a mark bit would be WRITTEN there.
    pub heap_top: u32,
    pub meta_lo: u32,
    pub meta_hi: u32,
    pub inited: bool,
}

impl GcMeta {
    const NEW: GcMeta = GcMeta {
        meta_dir: [0; MAX_CHUNKS as usize],
        chunks: 0,
        gray: [0; GRAY_CAP],
        gray_top: 0,
        gray_ovf: false,
        slot_tab: [[0u16; (SPAN_BYTES / GRANULE) as usize]; NUM_CLASSES as usize + 1],
        class_slots: [0; NUM_CLASSES as usize + 1],
        size_to_class: [0; (MAX_SMALL / GRANULE) as usize + 1],
        cur_ptr: [0; NUM_CLASSES as usize + 1],
        cur_end: [0; NUM_CLASSES as usize + 1],
        run_head: [0; NUM_CLASSES as usize + 1],
        run_tail: [0; NUM_CLASSES as usize + 1],
        hold_lo: [0; NUM_CLASSES as usize + 1],
        hold_hi: [0; NUM_CLASSES as usize + 1],
        dirty_q: [0; DIRTY_CAP as usize],
        pend: [0; PEND_CAP as usize],
        dirty_n: 0,
        dirty_cursor: 0,
        rescan_owed: false,
        rescan_cursor: 0,
        large_keep: 0,
        partial_base: 0,
        partial_off: 0,
        partial_end: 0,
        step_work: 0,
        max_work: 0,
        total_work: 0,
        unpaced_work: 0,
        max_unpaced_work: 0,
        call_folds: 0,
        max_unpaced_folds: 0,
        call_work: 0,
        root_words: 0,
        terminations: 0,
        rescans: 0,
        rescan_restarts: 0,
        dirty_overflows: 0,
        outruns: 0,
        span_count: 0,
        meta_spans: 0,
        span_cursor: 0,
        sweep_cursor: 0,
        live_acc: 0,
        free_acc: 0,
        freed_acc: 0,
        live_obj_acc: 0,
        free_bytes: 0,
        live_bytes: 0,
        live_objs: 0,
        freed_objs: 0,
        since_gc: 0,
        threshold: 0,
        budget: 0,
        collections: 0,
        grows: 0,
        steps: 0,
        last_steps: 0,
        deadlines: 0,
        step_escapes: 0,
        stall_escapes: 0,
        mark_limit: 0,
        phase: 0,
        marked: 0,
        stalls: 0,
        max_stalls: 0,
        stall_steps: 0,
        owed_mark: 0,
        pend_emptied: false,
        pend_empties: 0,
        mark_forced: false,
        collecting: false,
        valve_warned: false,
        root_warned: false,
        heap_base: 0,
        heap_top: 0,
        meta_lo: 0,
        meta_hi: 0,
        inited: false,
    };
}

/// The one static, wrapped so it is `Sync` without being `static mut`.
///
/// Sound because a Factorio mod is single-threaded by construction: wasm without
/// the threads proposal has one thread and this target does not enable it. That
/// is the same argument `fk`'s bump allocator makes for its own `UnsafeCell`.
///
/// The initialiser is all-zero, which is what keeps ~32 KiB of collector
/// bookkeeping in `.bss` instead of in the `.data` segment of every packaged
/// mod. `TestTheCollectorAddsNoDataSegment` is what stops that being a hope.
struct Meta(UnsafeCell<GcMeta>);
unsafe impl Sync for Meta {}
static GCM: Meta = Meta(UnsafeCell::new(GcMeta::NEW));

/// The accessor, and it carries ONE RULE that is a soundness property rather
/// than a style preference:
///
/// > **No function in this crate takes a `&mut GcMeta` as a parameter.**
///
/// Every function that needs the state calls this. Two overlapping `&mut` to one
/// object is UB by Rust's aliasing model, and this code shape produces them
/// constantly -- `sweep_span` holds one while calling `push_run`, which takes its
/// own. What makes that harmless is that LLVM attaches `noalias` to `&mut`
/// FUNCTION PARAMETERS and to nothing else here: a reference derived from
/// `UnsafeCell::get` inside a function and never passed on carries no aliasing
/// metadata, so there is no miscompilation vector. Passing one as a parameter
/// creates exactly the metadata that would make the aliasing observable, which is
/// why the rule is stated here and why `take` does not take one.
#[inline(always)]
#[allow(clippy::mut_from_ref)]
pub(crate) fn gcm() -> &'static mut GcMeta {
    unsafe { &mut *GCM.0.get() }
}

extern "C" {
    static __heap_base: u8;
    static __global_base: u8;
    static __stack_high: u8;
}

/// Where the linker says the static image ends. Everything above it is ours.
#[inline]
pub(crate) fn linker_heap_base() -> u32 {
    core::ptr::addr_of!(__heap_base) as u32
}

/// The low end of the root range: where the linker starts placing statics.
#[inline]
pub(crate) fn linker_global_base() -> u32 {
    core::ptr::addr_of!(__global_base) as u32
}

/// The top of the shadow stack, which grows DOWN towards `__stack_low`.
#[inline]
pub(crate) fn linker_stack_high() -> u32 {
    core::ptr::addr_of!(__stack_high) as u32
}

/// Brings the collector up against the linear memory that exists now.
///
/// Idempotent, and lazy: there is no `initHeap` the runtime calls here, so this
/// is funnelled through [`alloc_spans`] -- which `refill` and `alloc_large` both
/// go through -- and the hot path never tests it. A guest may still call
/// [`crate::heap::init`] from its own `_initialize` to pay it at load.
pub fn init() {
    if !gcm().inited {
        initialize();
    }
}

pub(crate) fn initialize() {
    let g = gcm();
    let base = linker_heap_base();
    g.heap_base = (base + SPAN_BYTES - 1) & !(SPAN_BYTES - 1);

    g.meta_lo = GCM.0.get() as u32;
    g.meta_hi = g.meta_lo + core::mem::size_of::<GcMeta>() as u32;

    // THE DEFAULTS DO NOT CLOBBER A VALUE THE GUEST ALREADY INSTALLED, and this
    // was a silent defect in both languages until 2026-08-03.
    //
    // Zero is not a legal setting -- `set_threshold(0)` and `set_budget(0)` mean
    // "restore the default" and write the default -- so a non-zero field here is
    // always something a guest asked for, and `GcMeta`'s zero initialiser gives
    // us the sentinel for free. That makes the latch independent of WHEN this
    // runs, which is the property that matters: `initialize` is LAZY on this
    // side (funnelled through `alloc_spans`, so the guest's first allocation
    // reaches it) and eager-ish on the Go side, and a fix keyed on call ordering
    // would be right in one language only.
    //
    // Measured downstream on this arm (fklua-ports' AutoDeconstruct, finding 3):
    // a guest asking for 131,072 ran with the collector's 262,144, reported
    // `since_gc=135168` and `cycles=0` for a whole verification run, and nothing
    // anywhere said so. Worse than a wrong number, because a guest that also
    // arms its own deferred flush on `stats().since_gc >= n` -- which
    // `agents/gc.md` prescribes -- then disagrees with the collector by
    // construction: it asks on every event and the collector declines every one.
    if g.threshold == 0 {
        g.threshold = DEFAULT_THRESHOLD;
    }
    if g.budget == 0 {
        g.budget = collect::DEFAULT_BUDGET;
    }

    // The class tables. Built here rather than as initialised constants because
    // a constant of this size is 5.6 KiB of DATA segment in every packaged mod,
    // and this loop is ~5,600 iterations once, at load.
    let mut c = 1usize;
    while c <= NUM_CLASSES as usize {
        let sz = CLASS_SIZE[c];
        let slots = SPAN_BYTES / sz;
        g.class_slots[c] = slots as u16;
        let mut gr = 0u32;
        while gr < SPAN_BYTES / GRANULE {
            let idx = (gr * GRANULE) / sz;
            g.slot_tab[c][gr as usize] = if idx >= slots { SLOT_NONE } else { idx as u16 };
            gr += 1;
        }
        c += 1;
    }
    let mut cur = 1u32;
    let mut n = 0u32;
    while n <= MAX_SMALL / GRANULE {
        let mut need = n * GRANULE;
        if need == 0 {
            need = GRANULE;
        }
        while cur < NUM_CLASSES && CLASS_SIZE[cur as usize] < need {
            cur += 1;
        }
        g.size_to_class[n as usize] = cur as u8;
        n += 1;
    }

    // WHATEVER LINEAR MEMORY ALREADY EXISTS ABOVE heap_base IS HEAP, ALL OF IT.
    // There is no cap to clamp to, and the only bound left is arithmetic (see
    // `max_spans`), which is 4 GiB less one span.
    //
    // Nothing is GROWN here: a guest that allocates nothing should not pay for a
    // heap, and no chunk is created either -- coverage is brought up by the first
    // allocation that needs it.
    //
    // Computed in SPANS rather than in bytes because bytes overflow: a wasm32
    // memory can be 65,536 pages and 65536<<16 is zero in a u32.
    g.span_count = 0;
    let spans = (memory_size() as u32) << (16 - SPAN_LOG);
    let hb = g.heap_base >> SPAN_LOG;
    if spans > hb {
        let mut k = spans - hb;
        let lim = max_spans();
        if k > lim {
            k = lim;
        }
        g.span_count = k;
    }
    // heap_top follows COVERAGE, which is zero here.
    g.heap_top = g.heap_base;
    g.free_bytes = g.span_count << SPAN_LOG;
    g.chunks = 0;
    g.inited = true;
}

/// Where the heap stops, and it is wasm32 rather than policy.
///
/// `heap_top` is a `u32` and `mark_candidate`'s whole hot loop is the half-open
/// range test `[heap_base, heap_top)`. A heap whose top is exactly 2^32 wraps to
/// zero, the range test accepts nothing, and a collector that marks nothing
/// frees everything -- so the last span of the address space is refused rather
/// than wrapped.
pub(crate) fn max_spans() -> u32 {
    let g = gcm();
    let mut n = (u32::MAX - g.heap_base) >> SPAN_LOG;
    let dir = MAX_CHUNKS << (SLICE_LOG - SPAN_LOG);
    if n > dir {
        n = dir;
    }
    n
}

// ---------------------------------------------------------------------------
// Allocation
//
// Free-list-first, span-second, memory.grow last. `agents/gc.md` makes that
// ordering a design decision rather than a tuning one: the usual argument for
// bump-first is locality and it is real, but it loses, because a bump pointer
// that walks past the end of the heap grows it PERMANENTLY -- wasm has no
// memory.shrink -- and every doubling avoided is 0.2 ms per MiB of worst tick
// that no later collection can give back.
// ---------------------------------------------------------------------------

/// The rule that makes allocation cheap, stated where it can be found and tested
/// rather than left implicit in three functions:
///
/// > EVERY FREE BLOCK IS ZERO, EXCEPT THE FIRST EIGHT BYTES OF A RUN, WHICH ARE
/// > THAT RUN'S `{next, end}` DESCRIPTOR.
///
/// The bytes have to be zeroed somewhere, and it is in the two places that touch
/// a whole span at once: `refill` zeroes a span it has just claimed in one
/// `memory.fill` of 4 KiB rather than 256 of 16 bytes, and the sweep zeroes each
/// run of dead slots in one call. Handing a block out then costs nothing, and
/// the eight descriptor bytes are cleared once per run by `next_run`.
///
/// It is also what lets [`Collector::alloc_zeroed`] be [`Collector::alloc`]:
/// `vec![0u8; n]` costs one span walk rather than one `memset` per allocation.
/// Three places maintain it -- `refill`, `sweep_span` and `next_run` -- and
/// `TestAFreshRustBlockIsZeroed` is what stops that being a comment.
pub const FREE_INVARIANT: &str =
    "a free block is zero; a run's first eight bytes are its descriptor";

/// The hot path, and its SHAPE is load-bearing in a way that is invisible in
/// Rust and obvious in the emitted Lua.
///
/// Rust puts a value in a wasm LOCAL where it can, and spills to the shadow
/// stack -- a bounds-checked store in emitted Lua -- where it cannot. Anything
/// with an address taken, any aggregate, any `Option<T>` that does not fit a
/// register, forces a frame. So the rule for anything added here is the rule
/// `guest/go/fkgc` states for its own: no aggregates, no slices, no calls, no
/// multi-value returns. Everything expensive is behind [`next_run`], which is
/// `#[inline(never)]` so that none of it lands in this frame.
///
/// Free space is tracked as RUNS of adjacent free blocks rather than as a list
/// of individual ones, and a run is bumped through. The sweep already walks dead
/// slots in address order and already coalesces them to zero them, so the runs
/// cost nothing to produce; what they buy is that handing out a block touches no
/// heap memory at all -- four array reads and one write, no dereference, no
/// call. The dereference happens once per RUN, which on an allocate-and-drop
/// workload is once per span.
#[inline]
pub(crate) fn allocate(size: u32, align: u32) -> u32 {
    // THE INIT CHECK IS HERE AND NOT DEEPER, AND THAT COST A WRONG ANSWER.
    //
    // TinyGo calls `initHeap` from `_initialize` before anything allocates, so
    // `guest/go/fkgc` may read its class tables unconditionally. Rust has no such
    // hook, so this crate initialises lazily -- and the first draft funnelled that
    // through `alloc_spans`, which is BELOW the table lookup on the next line.
    //
    // The very first allocation therefore read a `size_to_class` that was still
    // all zeroes, got class 0, and class 0 means UNASSIGNED: `refill` claimed a
    // span, wrote class 0 back into the span table, computed `class_slots[0] = 0`
    // slots of `CLASS_SIZE[0] = 0` bytes, and pushed an EMPTY run. The block came
    // back at the span base and the span stayed marked free, so the next class to
    // want a span was handed the same one and zeroed it with a live object in it.
    //
    // Nothing traps and nothing logs. The whole symptom was that a checksum over a
    // graph the collector had not even looked at yet came back different -- which
    // is the failure shape `gc_test.go`'s header describes, arriving before any
    // collection had run. One load and one predictable branch is what it costs to
    // make the tables valid before they are read.
    let g = gcm();
    if !g.inited {
        initialize();
    }
    if size > MAX_SMALL || align > GRANULE {
        return alloc_big(size, align);
    }
    let c = g.size_to_class[((size + GRANULE - 1) >> GRANULE_LOG) as usize] as u32;
    take(c)
}

/// The bump, and the only state it touches is the class's own cursor pair.
///
/// It takes a class rather than a `&mut GcMeta` on purpose -- see `gcm`.
#[inline(always)]
fn take(c: u32) -> u32 {
    let g = gcm();
    let mut p = g.cur_ptr[c as usize];
    if p == g.cur_end[c as usize] {
        // Out of run. Everything expensive is behind this one call.
        p = next_run(c);
    }
    g.cur_ptr[c as usize] = p + CLASS_SIZE[c as usize];
    p
}

/// The cold half of [`allocate`]: a large object, or one whose `Layout` asks for
/// more alignment than a granule.
///
/// Go's `alloc` hook is handed a size and nothing else, so `guest/go/fkgc` has
/// no equivalent of the second case. Here it is reachable -- `#[repr(align(32))]`
/// is ordinary Rust -- and it is answered without widening the granule, by
/// picking a size class whose SIZE is a multiple of the requested alignment. A
/// span base is 4 KiB-aligned and a slot is `base + i*size`, so `align | size`
/// makes every slot of that class aligned. Above 4 KiB of alignment there is no
/// such class and no span run either, and the request is refused.
#[inline(never)]
fn alloc_big(size: u32, align: u32) -> u32 {
    if align > GRANULE {
        if align > SPAN_BYTES {
            return 0; // nothing this allocator can promise
        }
        if size <= MAX_SMALL {
            let g = gcm();
            let mut c = g.size_to_class[((size + GRANULE - 1) >> GRANULE_LOG) as usize] as u32;
            while c <= NUM_CLASSES && CLASS_SIZE[c as usize] % align != 0 {
                c += 1;
            }
            if c <= NUM_CLASSES {
                return take(c);
            }
        }
        // A span run is 4 KiB-aligned, which covers every remaining alignment.
        return alloc_large(if size < SPAN_BYTES { SPAN_BYTES } else { size });
    }
    alloc_large(size)
}

/// Retires the class's current run and installs the next one, returning the
/// first block of it.
///
/// This is where every expensive thing in allocation has been pushed: the two
/// heap dereferences that read a run descriptor, the refill that claims a new
/// span, and the out-of-memory path with its string.
#[inline(never)]
fn next_run(c: u32) -> u32 {
    let g = gcm();
    let mut r = g.run_head[c as usize];
    if r == 0 {
        if !refill(c) {
            oom(1);
        }
        r = g.run_head[c as usize];
    }
    let next = load32(r);
    let end = load32(r + 4);
    g.run_head[c as usize] = next;
    if next == 0 {
        g.run_tail[c as usize] = 0;
    }
    // Clear the descriptor: from here on the run obeys FREE_INVARIANT's first
    // clause and every block in it is zero.
    store32(r, 0);
    store32(r + 4, 0);
    g.cur_end[c as usize] = end;
    r
}

/// Threads `[start, end)` onto class `c`'s run list, at the TAIL, so the list is
/// in ascending address order.
///
/// Address order is not cosmetic. What lands in `storage` is saved, CRC'd and
/// multiplayer-synchronised, so a heap whose layout depended on the order the
/// collector happened to walk something would be a per-client heap.
pub(crate) fn push_run(c: u32, start: u32, end: u32) {
    let g = gcm();
    store32(start, 0);
    store32(start + 4, end);
    if g.run_tail[c as usize] == 0 {
        g.run_head[c as usize] = start;
    } else {
        store32(g.run_tail[c as usize], start);
    }
    g.run_tail[c as usize] = start;
}

/// Claims one span for class `c` and makes it that class's next run. Reports
/// false only when the heap cannot grow.
#[inline(never)]
fn refill(c: u32) -> bool {
    let si = alloc_spans(1);
    if si == NO_SPAN {
        return false;
    }
    meta::set_span_class(si, c | fresh_bit(si));
    let g = gcm();
    let base = g.heap_base + (si << SPAN_LOG);
    let slots = g.class_slots[c as usize] as u32;
    let sz = CLASS_SIZE[c as usize];
    // The class's tail waste, lost for good.
    g.free_bytes -= SPAN_BYTES - slots * sz;
    // One `memory.fill` for the whole span, which is what establishes
    // FREE_INVARIANT for all of its slots at once.
    zero(base, SPAN_BYTES);
    // Heap pressure is accounted HERE and not per allocation, and that is a
    // design decision rather than a saving: what `collect_if_needed` is really
    // asking is "has the heap had to get bigger", and a byte handed back out of
    // a run a collection reclaimed has not grown anything.
    g.since_gc += SPAN_BYTES;
    push_run(c, base, base + slots * sz);
    true
}

#[inline(never)]
fn alloc_large(size: u32) -> u32 {
    let n = (size + SPAN_BYTES - 1) >> SPAN_LOG;
    let si = alloc_spans(n);
    if si == NO_SPAN {
        oom(n);
    }
    let f = fresh_bit(si);
    meta::set_span_class(si, CLS_LARGE | f);
    meta::set_span_aux(si, n);
    let mut k = 1u32;
    while k < n {
        meta::set_span_class(si + k, CLS_LARGE_MID | f);
        meta::set_span_aux(si + k, si);
        k += 1;
    }
    let g = gcm();
    let p = g.heap_base + (si << SPAN_LOG);
    let total = n << SPAN_LOG;
    g.since_gc += total;
    zero(p, total);
    p
}

/// "No run of that length exists."
pub(crate) const NO_SPAN: u32 = u32::MAX;

/// [`CLS_FRESH`] when the span is one the sweep has yet to walk, and zero
/// otherwise. It is what lets the mutator claim a span ANYWHERE while a sweep is
/// in flight, which is what removes the unbounded sweep-ahead.
fn fresh_bit(si: u32) -> u32 {
    let g = gcm();
    if g.phase == PHASE_SWEEP && si >= g.sweep_cursor {
        CLS_FRESH
    } else {
        0
    }
}

/// IT SWEEPS ONE BITE BEFORE IT GROWS, AND IT NEVER COLLECTS.
///
///  1. Look for a run. During a sweep this searches the WHOLE heap, because
///     [`CLS_FRESH`] protects a span claimed above the cursor.
///  2. Sweep ONE bite -- one paced step's worth -- and look again. That is what
///     bounds the heap: a mutator outrunning the pacing sweeps for itself rather
///     than growing past free space nobody has looked at yet. Bounded, so the
///     cost it can add to a tick is one step and not one collection.
///  3. Bring more of the heap under metadata, then GROW. Growing is what a guest
///     that outruns its pacer gets, and that is the whole product position:
///     `--gc=collected` degrades to a leaking arena under a storm and recovers
///     when the paced collection catches up. It does not pause.
///
/// **AND WHEN `memory.grow` ITSELF IS REFUSED IT TRAPS, WHERE THE GO COLLECTOR
/// RUNS A SYNCHRONOUS `Collect()`.** That is the one structural divergence in
/// this port and it is forced. Go's last resort is sound because TinyGo's
/// `markStack()` scans the shadow stack of whatever event handler the allocation
/// landed in; rustc keeps live references in wasm LOCALS, which no scan can
/// reach, so the same code here would mark without seeing the frame it is
/// standing in, sweep live objects, and report nothing. `lib.rs` has the whole
/// argument. A refused `memory.grow` on this target means the host or the
/// address space said no, and a wrong answer is worse than a dead mod.
pub(crate) fn alloc_spans(n: u32) -> u32 {
    let g = gcm();
    if !g.inited {
        initialize();
    }
    let si = find_span_run(n);
    if si != NO_SPAN {
        return si;
    }
    // ONE BITE OF SWEEP-AHEAD PER DISPATCH, not per allocation, and the
    // distinction is what makes this bound a TICK rather than a call. A bite
    // bounded at one step's worth bounds the ALLOCATION; it does not bound the
    // tick, because a dispatch makes as many allocations as it likes.
    //
    // `call_work` is exactly "collector work charged since the last paced step",
    // because a step resets it. Gating on it makes the in-call cost one bite per
    // tick whatever the dispatch does, and every allocation after that GROWS.
    if g.phase == PHASE_SWEEP && !g.collecting && g.call_work < SWEEP_AHEAD_UNITS {
        g.collecting = true;
        collect::sweep_step(SWEEP_AHEAD_UNITS);
        g.collecting = false;
        collect::end_unpaced();
        let si = find_span_run(n);
        if si != NO_SPAN {
            return si;
        }
    }
    loop {
        // Coverage first: a span above the coverage line is backed memory the
        // heap already owns and is cheaper than any memory.grow.
        if grow_coverage() {
            let si = find_span_run(n);
            if si != NO_SPAN {
                return si;
            }
            continue;
        }
        if !grow_heap(n) {
            break;
        }
        // GREW WHILE A COLLECTION WAS RUNNING is the outrun, and it is the only
        // shape worth a counter or a log line. Growing with the collector idle
        // is a guest building its live set, which is what a heap is for.
        if g.phase != PHASE_IDLE {
            g.outruns += 1;
            warn_outrun();
        }
        grow_coverage();
        let si = find_span_run(n);
        if si != NO_SPAN {
            return si;
        }
    }
    NO_SPAN
}

/// Looks for `n` consecutive unassigned spans.
///
/// IT SEARCHES THE WHOLE HEAP, INCLUDING DURING A SWEEP. The rule it replaces
/// was "not above the sweep cursor", whose cost was that the mutator's search
/// space was empty at the moment marking ended; [`CLS_FRESH`] answers the same
/// question per span and leaves the search space whole.
fn find_span_run(n: u32) -> u32 {
    let g = gcm();
    let count = covered_spans();
    if count < n {
        return NO_SPAN;
    }
    let mut start = g.span_cursor;
    if start > count - n {
        start = 0;
    }
    // Two passes: cursor to end, then start to cursor.
    let si = scan_span_run(start, count, n);
    if si != NO_SPAN {
        g.span_cursor = si + n;
        return si;
    }
    let si = scan_span_run(0, start, n);
    if si != NO_SPAN {
        g.span_cursor = si + n;
        return si;
    }
    NO_SPAN
}

/// Looks for `n` consecutive unassigned spans in `[from, to)`. The window is
/// checked from its TOP end so that a blocker lets the scan restart past it
/// rather than one span along, which is what keeps a run-of-n search linear in
/// the table rather than quadratic.
pub(crate) fn scan_span_run(from: u32, to: u32, n: u32) -> u32 {
    let mut i = from;
    while i + n <= to {
        let mut blocked = false;
        let mut k = n;
        while k > 0 {
            if meta::span_class_of(i + k - 1) != 0 {
                i += k; // the blocker is at i+k-1
                blocked = true;
                break;
            }
            k -= 1;
        }
        if !blocked {
            return i;
        }
    }
    NO_SPAN
}

/// It grows by a quarter, not by doubling, and the quarter is CAPPED.
///
/// Doubling is exactly the ladder this crate exists to keep a guest off:
/// `mem_grow` zeroes every new word, `MEMSIZE` is authoritative on the Lua side,
/// and a table that has held 16 million slots is walked as 16 million slots for
/// the rest of the session. The cap bounds the SPECULATIVE part only --
/// `need_spans` always wins, because a single allocation of `n` spans needs `n`
/// spans whatever the policy says.
pub(crate) const GROW_CAP_SPANS: u32 = 16; // 64 KiB, one wasm page

// The cap must clear META_CHUNK_SPANS, or the coverage-crossing round-up below
// asks for more than the cap allows and the two rules fight over every grow that
// reaches a 4 MiB slice boundary. Compile-time, because the failure would be a
// silently wrong increment rather than an error.
const _: () = assert!(GROW_CAP_SPANS >= META_CHUNK_SPANS);

fn grow_heap(need_spans: u32) -> bool {
    let g = gcm();
    let mut want = need_spans;
    let q = g.span_count / 4;
    if q > want {
        want = q;
    }
    if want > GROW_CAP_SPANS && want > need_spans {
        want = GROW_CAP_SPANS;
        if want < need_spans {
            want = need_spans;
        }
    }
    if want < 4 {
        want = 4;
    }
    // A grow that CROSSES the coverage line must clear the next chunk's ten
    // spans, or the coverage line stays where it is, the new spans are backed and
    // unusable, and `alloc_spans`'s loop repeats the grow. A grow that stays
    // inside the covered slice needs nothing -- which is the common case and the
    // reason for the first conjunct: without it, the very first chunk makes every
    // later grow round up to 4 MiB.
    let c = g.chunks << (SLICE_LOG - SPAN_LOG);
    if g.span_count + want > c && g.span_count + want < c + META_CHUNK_SPANS {
        want = c + META_CHUNK_SPANS - g.span_count;
    }
    let lim = max_spans();
    let mut new_count = g.span_count + want;
    if new_count > lim || new_count < g.span_count {
        new_count = lim;
    }
    if new_count < g.span_count + need_spans {
        // wasm32's ceiling, not a policy.
        return false;
    }
    let need_bytes = g.heap_base + (new_count << SPAN_LOG);
    let have_pages = memory_size() as u32;
    // (need_bytes-1)>>16 + 1 rather than (need_bytes+65535)>>16: need_bytes can
    // be within 64 KiB of 2^32 at the ceiling and the rounded-up form wraps.
    let need_pages = ((need_bytes - 1) >> 16) + 1;
    if need_pages > have_pages {
        if memory_grow((need_pages - have_pages) as usize) == usize::MAX {
            return false;
        }
        g.grows += 1;
    }
    let added = new_count - g.span_count;
    g.span_count = new_count;
    g.free_bytes += added << SPAN_LOG;
    // A grow inside an ALREADY covered slice moves the coverage line too, so the
    // marking range has to follow it here as well as in `grow_coverage`.
    sync_heap_top();
    true
}

// ---------------------------------------------------------------------------
// Raw memory
// ---------------------------------------------------------------------------

#[inline(always)]
pub(crate) fn load32(addr: u32) -> u32 {
    unsafe { core::ptr::read(addr as *const u32) }
}

#[inline(always)]
pub(crate) fn store32(addr: u32, v: u32) {
    unsafe { core::ptr::write(addr as *mut u32, v) }
}

#[inline]
fn memory_size() -> usize {
    core::arch::wasm32::memory_size(0)
}

#[inline]
fn memory_grow(pages: usize) -> usize {
    core::arch::wasm32::memory_grow(0, pages)
}

/// Clears a freshly handed-out block.
///
/// `write_bytes` lowers to `llvm.memset`, which the wasm backend emits as
/// `memory.fill` and the FkLua emitter compiles natively: one bounds check and
/// one page mark for the whole span, rather than the byte loop binaryen's
/// `--llvm-memory-copy-fill-lowering` would substitute at 173 ns/byte.
#[inline(never)]
pub(crate) fn zero(addr: u32, n: u32) {
    if !ZERO_ON_ALLOC {
        return;
    }
    wipe(addr, n);
}

/// [`zero`] WITHOUT the [`ZERO_ON_ALLOC`] gate, for the bytes that are not a
/// freshly handed-out block and whose zeroing is not optional -- a metadata
/// chunk arriving with the last tenant's bytes is a bitmap of set bits and a
/// span table of invented classes.
#[inline(never)]
pub(crate) fn wipe(addr: u32, n: u32) {
    unsafe { core::ptr::write_bytes(addr as *mut u8, 0, n as usize) }
}

/// The measurement arm, mirroring `guest/go/fkgc`'s `-tags fkgcnozero`.
///
/// A build with `--cfg fkgcnozero` is WRONG -- a recycled block arrives holding
/// the previous object's bytes -- and exists only so the cost of zeroing a
/// recycled block can be separated from the cost of the free list that produced
/// it. Nothing but a measurement harness sets it.
#[cfg(fkgcnozero)]
pub(crate) const ZERO_ON_ALLOC: bool = false;
#[cfg(not(fkgcnozero))]
pub(crate) const ZERO_ON_ALLOC: bool = true;

// ---------------------------------------------------------------------------
// Diagnostics
//
// `fk_log` is declared HERE rather than reached through the `fk` crate for two
// reasons. `fk` DEPENDS on this crate -- it is where the one
// `#[global_allocator]` site lives -- so the other direction would be a cycle;
// and everything that touches a string here is `#[inline(never)]` for the reason
// `allocate` documents, because a `&str` is a pointer and a length and a spilled
// pair in the allocation path is a shadow frame on every allocation.
// ---------------------------------------------------------------------------

#[link(wasm_import_module = "env")]
extern "C" {
    fn fk_log(ptr: u32, len: u32);
}

#[inline(never)]
pub(crate) fn log_string(s: &str) {
    unsafe { fk_log(s.as_ptr() as u32, s.len() as u32) }
}

/// Says, once per guest, that the heap had to GROW because the paced collection
/// had not caught up -- which is a cost, not a failure, and the message says so.
#[inline(never)]
fn warn_outrun() {
    let g = gcm();
    if g.valve_warned {
        return;
    }
    g.valve_warned = true;
    log_string(
        "fkgc: the heap GREW while a collection was still running -- this guest \
         allocates faster than its budget reclaims, so --gc=collected is behaving \
         like a leaking arena until the paced collection catches up. Nothing paused \
         and nothing is wrong; the cost is linear memory, which never shrinks, at \
         about 0.2 ms of worst tick per MiB. Raise fk::gc::set_budget, call \
         fk::gc::collect_if_needed() more often, or allocate less. See \
         agents/gc.md, the reclaim-rate table.",
    );
}

/// Says, once per guest, that this guest's ROOT SET is bigger than the step
/// budget it asked for, so the collector has raised the budget on its own
/// authority.
///
/// IT IS HERE BECAUSE NOBODY OUTSIDE CAN SEE THE CONDITION. `root_words` is
/// measured inside a scan the collector does and the host never sees; `budget()`
/// is a number the guest chose. Their ratio is the whole of the diagnosis and the
/// collector is the only thing that holds both. Before the floor the symptom a
/// guest COULD see was a rising `deadlines` count, which agents/gc.md and
/// `set_budget`'s own comment both attribute to an allocation rate over the
/// budget -- true for the other cause of that symptom, and the wrong end of the
/// mod entirely for this one. It cost the first downstream mod a day.
#[inline(never)]
pub(crate) fn warn_root_budget() {
    let g = gcm();
    if g.root_warned {
        return;
    }
    g.root_warned = true;
    log_string(
        "fkgc: this guest's ROOT SET is larger than one step's budget, so the \
         collector has raised the budget to cover it. A termination attempt \
         re-scans [__global_base, __heap_base) in one indivisible pass and charges \
         what it walked, so a budget under that cost can never finish a mark -- \
         nothing would be reclaimed and only the mark deadline would end it. This \
         is NOT the allocation rate and fkgc::set_budget is not the fix: raise the \
         budget above fkgc::effective_budget() to choose the pause deliberately, \
         or declare fewer/smaller statics. See agents/gc.md, 'the root-scan \
         floor'.",
    );
}

/// Out of memory, which on this target is an `unreachable` and a log line.
///
/// Two things reach here and they want different advice, so the span count the
/// request wanted is the discriminator.
///
/// **The Go collector runs a full synchronous mark and sweep before it gets
/// here, and this one does not.** See [`alloc_spans`]: that recovery reads the
/// shadow stack of the event handler it landed in, which is a thing TinyGo can
/// do and rustc cannot.
#[inline(never)]
#[cold]
pub(crate) fn oom(need_spans: u32) -> ! {
    if need_spans > SLICE_SPANS - META_CHUNK_SPANS {
        log_string(
            "fkgc: OUT OF MEMORY on a SINGLE OBJECT larger than 4 MiB. The \
             collector's metadata is allocated from the heap in 40 KiB chunks, one \
             per 4 MiB slice, and under memory pressure a chunk lands at a slice \
             boundary that a single object cannot straddle. The heap itself has no \
             cap. Allocate the object in pieces, or keep enough headroom that the \
             chunks stay low. See agents/gc.md.",
        );
    } else {
        log_string(
            "fkgc: OUT OF MEMORY -- memory.grow was REFUSED. There is no fkgc heap \
             cap: this is the wasm runtime's limit or the 4 GiB wasm32 address \
             space. A Rust guest does NOT get the Go collector's last-resort \
             synchronous collection here, because marking inside a guest call \
             cannot see references rustc left in wasm locals -- it would free live \
             objects silently instead of trapping loudly. The mod is about to trap \
             with `unreachable`. See agents/gc.md and fkgc's lib.rs.",
        );
    }
    core::arch::wasm32::unreachable()
}

// ---------------------------------------------------------------------------
// The global allocator
// ---------------------------------------------------------------------------

/// The `#[global_allocator]` a collected Rust guest runs on.
///
/// It is installed by `fk` under its `fkgc` feature and never here, so a module
/// has exactly one global-allocator site whichever arm it is built in and the
/// two can never both be linked.
pub struct Collector;

unsafe impl GlobalAlloc for Collector {
    #[inline]
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        allocate(layout.size() as u32, layout.align() as u32) as *mut u8
    }

    /// A no-op, and deliberately so.
    ///
    /// The free lists are rebuilt from the mark bitmap by every sweep, so an
    /// explicit free is a double-free one sweep later -- which is the same
    /// reasoning `guest/go/fkgc`'s no-op `free` records. Rust calls this far more
    /// often than Go's runtime ever would, and the answer is the same: the sweep
    /// owns reclamation, and a `Drop` that ran early bought the guest nothing the
    /// next collection was not already going to give it.
    #[inline]
    unsafe fn dealloc(&self, _ptr: *mut u8, _layout: Layout) {}

    /// Free by [`FREE_INVARIANT`]: every block this allocator hands out is
    /// already zero, so `vec![0u8; n]` costs one span walk rather than one
    /// `memset` per allocation.
    ///
    /// Under the `fkgcnozero` measurement arm the invariant does not hold and
    /// this pays for itself, because that arm is allowed to be slow and is not
    /// allowed to be wrong.
    #[inline]
    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        let p = allocate(layout.size() as u32, layout.align() as u32) as *mut u8;
        if !ZERO_ON_ALLOC && !p.is_null() {
            core::ptr::write_bytes(p, 0, layout.size());
        }
        p
    }

    /// Grows in place when the new size still fits the class the old one got.
    ///
    /// The default `GlobalAlloc::realloc` is allocate-copy-deallocate, and with a
    /// no-op `dealloc` that turns a `Vec` doubling from `n` bytes of copy into
    /// `n` bytes of copy plus a whole block of garbage. A class is a size RANGE,
    /// so most of a `Vec`'s early growth is answered without touching the heap at
    /// all. Nothing about retention changes: the block is the same block, its
    /// span class is the same class, and the bitmap still describes it.
    #[inline]
    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        let old = layout.size() as u32;
        let new = new_size as u32;
        if old <= MAX_SMALL && new <= MAX_SMALL && layout.align() as u32 <= GRANULE {
            let g = gcm();
            let oc = g.size_to_class[((old + GRANULE - 1) >> GRANULE_LOG) as usize];
            let nc = g.size_to_class[((new + GRANULE - 1) >> GRANULE_LOG) as usize];
            if oc == nc {
                // Shrinking within a class leaves the tail as it was, and
                // FREE_INVARIANT is about FREE blocks, so a live block's tail is
                // nobody's business. Growing within a class hands back bytes that
                // were zero when the block was handed out and that nothing has
                // written since, which is exactly what `alloc` promises.
                return ptr;
            }
        }
        let p = allocate(new, layout.align() as u32) as *mut u8;
        if !p.is_null() {
            let n = if old < new { old } else { new };
            core::ptr::copy_nonoverlapping(ptr, p, n as usize);
        }
        p
    }
}

// ---------------------------------------------------------------------------
// The surface a guest and the tests read
// ---------------------------------------------------------------------------

/// The base of the region the allocator owns.
pub fn heap_base() -> u32 {
    gcm().heap_base
}

/// The exclusive upper bound of the region the allocator owns.
pub fn heap_top() -> u32 {
    gcm().heap_top
}

/// The linear memory the allocator has claimed as heap, which is not the same as
/// `MemStats::heap_bytes`: that is the COVERED heap, and coverage lags a
/// `memory.grow` until the chunk describing the new slice exists. The gap is at
/// most one slice and closes on the next allocation that wants room.
pub fn backed_bytes() -> u32 {
    gcm().span_count << SPAN_LOG
}

/// Re-runs initialisation against the linear memory that exists now.
///
/// IT IS DESTRUCTIVE AND IT IS FOR TESTS. Every free list and every span
/// assignment is forgotten, so nothing allocated before it may be touched after.
/// It exists so that "the allocator adopts ALL pre-existing linear memory" is an
/// assertion rather than a reading of `initialize`.
pub fn reinitialize() {
    initialize();
}
