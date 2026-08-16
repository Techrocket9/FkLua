//! Collector metadata that GROWS WITH THE HEAP, which is what removes the cap.
//!
//! The mirror of `guest/go/fkgc/meta.go`, and structurally identical to it: the
//! metadata splits by GROWTH LAW rather than by what it means.
//!
//! ```text
//! fixed      the class tables, the mark stack, the free-run heads, the dirty
//!            queue, the counters. ~36 KiB, still .bss, still exactly one
//!            struct (see GcMeta for why that is a correctness requirement).
//! scaling    the mark bitmap, the span class and the span aux. One CHUNK per
//!            4 MiB slice of heap, allocated FROM THE HEAP under the class
//!            CLS_META, reached through a directory that is itself a fixed
//!            .bss array.
//! ```
//!
//! `meta_dir` is 1,024 entries of 4 KiB of `.bss` and that covers the ENTIRE
//! wasm32 address space: 1,024 × 4 MiB is 4 GiB. A flat table sized for 4 GiB
//! would have been 35 MiB of `.bss` -- about 7 ms of Factorio worst tick before
//! the guest allocated a byte -- which is why the directory exists at all.
//!
//! WHY THIS IS SAFE is the one hazard in the design with no precedent elsewhere
//! in the repo, and the answer is the Go collector's: `drain_dirty_spans`
//! already drops a sub-heap card with one compare, and a metadata card inside
//! the heap is dropped by the span-class load `rescan_span` ALREADY performs.
//! The compare moves; it does not multiply. Two things follow and both are
//! enforced rather than assumed -- `mark_candidate` REJECTS a `CLS_META` span,
//! and `meta_dir` holds HEAP ADDRESSES and lives inside `[meta_lo, meta_hi)` so
//! that `scan_roots` subtracts it.

use crate::heap::{
    gcm, load32, scan_span_run, store32, wipe, CLS_META, GRANULE, GRANULE_LOG, NO_SPAN, SPAN_BYTES,
    SPAN_LOG,
};

/// How much heap one chunk describes: 4 MiB.
///
/// It is the whole tuning decision here and it trades two things against each
/// other. Bigger slices mean a smaller directory and a larger FLOOR -- the first
/// chunk is paid by any guest that allocates at all. At 4 MiB the floor is
/// 40 KiB and the directory is 4 KiB.
pub(crate) const SLICE_LOG: u32 = 22;
/// 1,024 spans per chunk.
pub(crate) const SLICE_SPANS: u32 = 1 << (SLICE_LOG - SPAN_LOG);
/// 262,144 granules per chunk.
pub(crate) const SLICE_WORDS: u32 = 1 << (SLICE_LOG - GRANULE_LOG);

// The chunk's layout, in bytes from its base. Three arrays of u32, in the order
// they are hottest.
pub(crate) const META_MARK_OFF: u32 = 0;
/// 32,768 B: one bit per granule.
const META_MARK_LEN: u32 = SLICE_WORDS / 32 * 4;
const META_CLASS_OFF: u32 = META_MARK_OFF + META_MARK_LEN;
/// 4,096 B: one word per span.
const META_CLASS_LEN: u32 = SLICE_SPANS * 4;
const META_AUX_OFF: u32 = META_CLASS_OFF + META_CLASS_LEN;
const META_AUX_LEN: u32 = SLICE_SPANS * 4;

/// 40,960 B is exactly ten spans, with nothing left over. That is not luck -- it
/// is what picking `u32` for all three tables buys, and a layout that did not
/// land on a span boundary would waste the remainder of the last one forever.
pub(crate) const META_CHUNK_BYTES: u32 = META_AUX_OFF + META_AUX_LEN;
pub(crate) const META_CHUNK_SPANS: u32 = META_CHUNK_BYTES >> SPAN_LOG;

/// Covers all of wasm32. See `max_spans` for the bound that actually binds,
/// which is smaller and is about `u32` arithmetic rather than about this.
pub(crate) const MAX_CHUNKS: u32 = 1 << (32 - SLICE_LOG);

/// Eight: a span is 4,096 B, a granule is 16 B, so a span is exactly 256
/// granules and exactly eight bitmap words. The bitmap partitions on span
/// boundaries with nothing left over, which is what lets a sweep clear as it
/// goes and what lets every per-span loop compute ONE address and then index
/// within it.
pub(crate) const MARK_WORDS_PER_SPAN: u32 = SPAN_BYTES / GRANULE / 32;

const _: () = assert!(META_CHUNK_BYTES == META_CHUNK_SPANS * SPAN_BYTES);

/// What span `si` holds: 0 unassigned, `1..=NUM_CLASSES` a size class,
/// `CLS_LARGE`/`CLS_LARGE_MID` a large-object run, `CLS_META` a metadata chunk.
///
/// The value may carry `CLS_FRESH` or `CLS_PENDING`; every reader that cares
/// about the CLASS masks them off, and `sweep_span` is the one reader that wants
/// the raw word.
#[inline(always)]
pub(crate) fn span_class_of(si: u32) -> u32 {
    let g = gcm();
    load32(g.meta_dir[(si >> (SLICE_LOG - SPAN_LOG)) as usize] + META_CLASS_OFF + ((si & (SLICE_SPANS - 1)) << 2))
}

#[inline(always)]
pub(crate) fn set_span_class(si: u32, c: u32) {
    let g = gcm();
    store32(
        g.meta_dir[(si >> (SLICE_LOG - SPAN_LOG)) as usize] + META_CLASS_OFF + ((si & (SLICE_SPANS - 1)) << 2),
        c,
    )
}

/// The run length for a `CLS_LARGE` head and the head's span index for a
/// `CLS_LARGE_MID` continuation. Unused otherwise.
#[inline(always)]
pub(crate) fn span_aux_of(si: u32) -> u32 {
    let g = gcm();
    load32(g.meta_dir[(si >> (SLICE_LOG - SPAN_LOG)) as usize] + META_AUX_OFF + ((si & (SLICE_SPANS - 1)) << 2))
}

#[inline(always)]
pub(crate) fn set_span_aux(si: u32, v: u32) {
    let g = gcm();
    store32(
        g.meta_dir[(si >> (SLICE_LOG - SPAN_LOG)) as usize] + META_AUX_OFF + ((si & (SLICE_SPANS - 1)) << 2),
        v,
    )
}

/// The address of the FIRST of span `si`'s eight bitmap words.
///
/// Every per-span loop in the sweep and the re-scan calls this once and then
/// indexes within the span, which is why moving the bitmap into the heap did not
/// make either of them slower.
#[inline(always)]
pub(crate) fn mark_word_base(si: u32) -> u32 {
    let g = gcm();
    g.meta_dir[(si >> (SLICE_LOG - SPAN_LOG)) as usize]
        + META_MARK_OFF
        + (si & (SLICE_SPANS - 1)) * (MARK_WORDS_PER_SPAN * 4)
}

/// Tests the bit for the object at byte offset `off` within the span whose
/// bitmap words start at `mw`. `off` is a span offset, so it is under 4,096 and
/// the granule index is under 256.
#[inline(always)]
pub(crate) fn is_marked_at(mw: u32, off: u32) -> bool {
    let g = off >> GRANULE_LOG;
    load32(mw + ((g >> 5) << 2)) & (1u32 << (g & 31)) != 0
}

/// Wipes span `si`'s eight bitmap words.
///
/// Eight explicit stores rather than a `write_bytes` over 32 bytes: that lowers
/// to `memory.fill`, which the emitter substitutes with the runtime's `mem_fill`
/// -- a call with its own bounds check and page mark, for 32 bytes. The CALL is
/// the cost, not the bytes.
#[inline]
pub(crate) fn clear_span_marks(si: u32) {
    let w = mark_word_base(si);
    store32(w, 0);
    store32(w + 4, 0);
    store32(w + 8, 0);
    store32(w + 12, 0);
    store32(w + 16, 0);
    store32(w + 20, 0);
    store32(w + 24, 0);
    store32(w + 28, 0);
}

/// Republishes the marking range after anything moves the coverage line -- a new
/// chunk, or a `memory.grow` that extends an already-covered slice. It is called
/// from BOTH, and forgetting the second is a silent corruption: `mark_candidate`
/// would accept addresses in the covered heap that `heap_top` said were outside
/// it, so the sweep would free live objects nothing had marked.
#[inline]
pub(crate) fn sync_heap_top() {
    let g = gcm();
    g.heap_top = g.heap_base + (covered_spans() << SPAN_LOG);
}

/// How much of the heap has metadata behind it, and it -- not `span_count` -- is
/// what every span loop in this crate bounds itself by.
///
/// The two differ only between a `memory.grow` and the `grow_coverage` that
/// follows it. A span above this line is backed by real linear memory and is
/// simply not heap yet: nothing can be allocated in it, the sweep does not walk
/// it, and the next allocation that wants room brings it in. Nothing is ever
/// LOST there.
#[inline]
pub(crate) fn covered_spans() -> u32 {
    let g = gcm();
    let c = g.chunks << (SLICE_LOG - SPAN_LOG);
    if c > g.span_count {
        g.span_count
    } else {
        c
    }
}

/// Brings one more 4 MiB slice of the heap under metadata, and reports whether
/// it could.
///
/// WHERE THE CHUNK GOES is the only interesting decision, and it is made to keep
/// the TOP of the heap contiguous, because that is where a large object goes.
///
///   * First choice: the LOWEST free run of ten spans in the region that is
///     already covered. A chunk placed there is a permanent blocker in a part of
///     the heap that is already fragmented, and it leaves everything above it in
///     one piece.
///   * Fallback: the first ten spans of the slice being covered, which are
///     guaranteed unassigned because they are above the old coverage line. This
///     is the case under real pressure, and it is the one that caps a single
///     object at just under 4 MiB.
pub(crate) fn grow_coverage() -> bool {
    let g = gcm();
    let k = g.chunks;
    if k >= MAX_CHUNKS {
        return false;
    }
    // The slice must be backed at least as far as the chunk itself.
    if g.span_count < (k << (SLICE_LOG - SPAN_LOG)) + META_CHUNK_SPANS {
        return false;
    }
    let mut t = scan_span_run(0, covered_spans(), META_CHUNK_SPANS);
    if t == NO_SPAN {
        t = k << (SLICE_LOG - SPAN_LOG);
    }
    let addr = g.heap_base + (t << SPAN_LOG);
    // ALWAYS wiped, never `zero`d: `zero` is a no-op under the `fkgcnozero`
    // measurement arm. A chunk arriving with the last tenant's bytes in it is a
    // bitmap full of set bits and a span table full of invented classes, which is
    // not a measurement arm, it is a heap that walks into somebody else's memory.
    wipe(addr, META_CHUNK_BYTES);
    g.meta_dir[k as usize] = addr;
    g.chunks = k + 1;
    // Only reachable now: `set_span_class` for a span inside the new slice needs
    // meta_dir[k], and in the fallback case t IS inside the new slice.
    let mut j = 0u32;
    while j < META_CHUNK_SPANS {
        set_span_class(t + j, CLS_META);
        j += 1;
    }
    g.free_bytes -= META_CHUNK_SPANS << SPAN_LOG;
    g.meta_spans += META_CHUNK_SPANS;
    sync_heap_top();
    true
}

/// How much linear memory the collector's own metadata occupies, and it is a
/// FUNCTION OF THE HEAP rather than a constant.
///
/// ```text
/// meta_bytes = meta_fixed_bytes + chunks * 40,960
/// chunks     = ceil(covered heap / 4 MiB)
/// ```
///
/// It is `.bss` plus heap, so it costs nothing in the wasm binary -- but it IS
/// linear memory, and `agents/guests.md` prices linear memory at 0.2 ms of
/// Factorio worst tick per MiB whether or not anything is using it.
pub fn meta_bytes() -> u32 {
    meta_fixed_bytes() + gcm().chunks * META_CHUNK_BYTES
}

/// The part that does not scale: the class tables, the mark stack, the dirty
/// queue, the directory and the counters. This is what a guest pays for having a
/// collector at all, before it allocates anything.
pub fn meta_fixed_bytes() -> u32 {
    core::mem::size_of::<crate::heap::GcMeta>() as u32
}

/// How many 4 MiB slices of heap currently have metadata behind them.
pub fn meta_chunks() -> u32 {
    gcm().chunks
}

/// What one chunk costs. Exported so that the size model is TESTED rather than
/// documented.
pub fn meta_chunk_bytes() -> u32 {
    META_CHUNK_BYTES
}

/// How much heap one chunk describes.
pub fn meta_slice_bytes() -> u32 {
    1 << SLICE_LOG
}
