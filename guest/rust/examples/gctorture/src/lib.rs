//! The Rust half of the collector's torture guest -- the MIRROR of
//! `guest/go/examples/gctorture`.
//!
//! It exists for the corpus-mirror tradition and its bar is stated as a
//! property rather than as a hope:
//!
//! > **Every export whose value is pure arithmetic over the workload returns
//! > the same `u32` as the Go guest's export of the same name.**
//!
//! That is what makes a cross-language comparison a comparison rather than two
//! programs that both finished. `TestTheTwoCollectorsAgreeOnTheTortureCorpus`
//! runs both and diffs them; the folds below are transcribed from
//! `guest/go/examples/gctorture/main.go` and every one of them is
//! `acc = acc*31 + v` in wrapping `u32`, which Go's unsigned overflow gives for
//! free and Rust spells out.
//!
//! Three exports are deliberately NOT bit-comparable, and they are the ones that
//! read the ALLOCATOR rather than the workload:
//!
//! * `torture_kept_bytes` folds `roots.capacity()`, and Go's `append` and Rust's
//!   `Vec` do not grow on the same curve. It is a heap-accounting probe, not a
//!   checksum.
//! * `torture_stat` reports collector counters, which are a function of the
//!   heap the guest happens to have.
//! * `torture_backed` is linear memory, which starts at a different place in the
//!   two toolchains -- rustc's statics begin at 1 MiB and TinyGo's at 64 KiB.
//!
//! Everything else agrees exactly, and the test asserts which is which rather
//! than leaving it to a reader.
//!
//! It emits no log lines at all, exactly as the Go guest does not: every
//! assertion here is a returned number, which is what lets the harness compare
//! the two without normalising any text.

#![no_std]

extern crate alloc;

use alloc::boxed::Box;
use alloc::vec::Vec;
use core::cell::UnsafeCell;

use fk::gc;

/// The node the graph is built out of.
///
/// `left` and `right` are RAW pointers and not `Box`, and that is the whole
/// point of the guest: a `Box` would make the graph a tree with ownership and
/// drop it deterministically, which tests Rust's allocator rather than the
/// collector. Raw pointers let a node be shared, dropped without being freed,
/// and reachable only through the conservative scan -- which is the thing under
/// test.
///
/// 4 + 4 + 4 + 4 + 20 = 40 bytes, which lands in the 48-byte size class, the
/// same class the Go node lands in. `NODE_BYTES` is that class and not
/// `size_of`, for the same reason the Go guest uses 48: what the heap costs is
/// the slot, not the struct.
#[repr(C)]
struct Node {
    id: u32,
    tag: u32,
    left: *mut Node,
    right: *mut Node,
    pad: [u32; 5],
}

const NODE_BYTES: u32 = 48;

struct State {
    roots: Vec<*mut Node>,
    dropped: u32,
    kept: u32,
    interior: *const u32,
    one_past: u32,
    one_past_id: u32,
    big: Vec<u32>,
    big_mark: u32,
    fresh_sum: u32,
    /// Forces every garbage node to escape to the heap. One slot: the point is
    /// that the allocation is real and the reference is not kept.
    garbage_sink: *mut Node,
    held: Vec<Vec<u32>>,
    held_sum: u32,
}

impl State {
    const NEW: State = State {
        roots: Vec::new(),
        dropped: 0,
        kept: 0,
        interior: core::ptr::null(),
        one_past: 0,
        one_past_id: 0,
        big: Vec::new(),
        big_mark: 0,
        fresh_sum: 0,
        garbage_sink: core::ptr::null_mut(),
        held: Vec::new(),
        held_sum: 0,
    };
}

/// The guest's whole retained root set, and it is a `static` on purpose: that is
/// what puts it in `[__global_base, __heap_base)`, which is the range the
/// collector scans. A `Vec` here is one pointer in the statics, the collector
/// marks its buffer through that pointer, and scanning the buffer marks every
/// node address in it. Nothing else in this guest keeps anything alive.
struct Cell(UnsafeCell<State>);
unsafe impl Sync for Cell {}
static S: Cell = Cell(UnsafeCell::new(State::NEW));

#[allow(clippy::mut_from_ref)]
#[inline(always)]
fn s() -> &'static mut State {
    unsafe { &mut *S.0.get() }
}

fn node(id: u32, tag: u32) -> *mut Node {
    Box::into_raw(Box::new(Node {
        id,
        tag,
        left: core::ptr::null_mut(),
        right: core::ptr::null_mut(),
        pad: [0; 5],
    }))
}

/// Builds `n` pairs of nodes and keeps every eighth pair.
///
/// The fold absorbs BOTH tags in one `*31` step, which is what the Go guest
/// does, and getting that wrong is the easiest way to produce a checksum that
/// looks plausible and is not the same number.
#[no_mangle]
pub extern "C" fn torture_build(n: u32) -> u32 {
    let st = s();
    st.roots.clear();
    st.kept = 0;
    st.dropped = 0;
    let mut acc: u32 = 0;
    for i in 0..n {
        let a = node(i, i.wrapping_mul(2654435761));
        let b = node(i ^ 0x5555, i.wrapping_mul(40503));
        unsafe { (*a).left = b };
        if i % 8 == 0 {
            st.roots.push(a);
            st.kept += 2;
        } else {
            st.dropped += 2;
        }
        acc = acc
            .wrapping_mul(31)
            .wrapping_add(unsafe { (*a).tag })
            .wrapping_add(unsafe { (*b).tag });
    }
    acc
}

/// A full synchronous cycle.
///
/// SOUND HERE BECAUSE THIS IS THE WHOLE BODY OF AN EXPORT. `fkgc::collect` marks,
/// and marking cannot see a reference rustc left in a wasm local of the calling
/// frame -- so its precondition is that the calling frame holds none. An export
/// invoked by the host at an outermost dispatch has no such frame, which is the
/// only shape the crate blesses. See `fkgc`'s `lib.rs`.
#[no_mangle]
pub extern "C" fn torture_collect() -> u32 {
    gc::collect();
    gc::stats().collections
}

/// Folds the surviving graph. `right` is deliberately not folded here -- that is
/// what `torture_repoint_verify` is for.
#[no_mangle]
pub extern "C" fn torture_verify() -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for &r in st.roots.iter() {
        unsafe {
            acc = acc.wrapping_mul(31).wrapping_add((*r).id);
            acc = acc.wrapping_mul(31).wrapping_add((*r).tag);
            if !(*r).left.is_null() {
                acc = acc.wrapping_mul(31).wrapping_add((*(*r).left).id);
                acc = acc.wrapping_mul(31).wrapping_add((*(*r).left).tag);
            }
        }
    }
    acc
}

/// What the guest believes it is keeping.
///
/// NOT BIT-COMPARABLE WITH THE GO GUEST, and the reason is in the file header:
/// it folds `capacity()`, which is the allocator's answer and not the workload's.
#[no_mangle]
pub extern "C" fn torture_kept_bytes() -> u32 {
    let st = s();
    let mut n = st.kept * NODE_BYTES;
    n += st.roots.capacity() as u32 * 4;
    n += st.big.capacity() as u32 * 4;
    if !st.interior.is_null() {
        n += 256;
    }
    n
}

/// The collector's counters, indexed exactly as
/// `guest/go/examples/gctorture`'s `torture_stat` indexes them. Keeping the two
/// tables in step is what lets one harness drive both guests.
#[no_mangle]
pub extern "C" fn torture_stat(which: u32) -> u32 {
    let st = gc::stats();
    match which {
        0 => st.heap_bytes,
        1 => st.live_bytes,
        2 => st.free_bytes,
        3 => st.collections,
        4 => st.grows,
        5 => st.live_objects,
        6 => st.freed_objects,
        7 => st.since_gc,
        8 => gc::meta_bytes(),
        9 => st.phase,
        10 => st.steps,
        11 => gc::budget(),
        12 => gc::max_step_work(),
        13 => gc::total_work(),
        14 => st.deadlines,
        15 => st.outruns,
        16 => st.max_unpaced,
        17 => st.unpaced_work,
        18 => gc::terminations(),
        19 => gc::root_words(),
        20 => gc::mark_bits_set(),
        21 => gc::meta_chunks(),
        22 => gc::meta_fixed_bytes(),
        23 => gc::rescans(),
        24 => gc::dirty_overflows(),
        25 => gc::rescan_restarts(),
        26 => gc::effective_budget(),
        // The mark escape SPLIT BY CAUSE. 14 is still the total.
        27 => st.step_escapes,
        28 => st.stall_escapes,
        _ => 0,
    }
}

/// Starts a PACED collection. Unlike [`torture_collect`] this only opens the
/// mark phase, so it has no precondition on the calling frame at all.
#[no_mangle]
pub extern "C" fn torture_gc_start() -> u32 {
    if gc::start() {
        1
    } else {
        0
    }
}

#[no_mangle]
pub extern "C" fn torture_gc_budget(units: u32) -> u32 {
    gc::set_budget(units);
    gc::budget()
}

/// Hangs a fresh node off every root, which is the write-barrier test: the roots
/// were marked in an earlier step, so a store into one is invisible unless the
/// dirty page carrying it is re-scanned.
#[no_mangle]
pub extern "C" fn torture_repoint(seed: u32) -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for (i, &r) in st.roots.iter().enumerate() {
        let v = seed.wrapping_add(i as u32);
        let f = node(v, v.wrapping_mul(2654435761));
        unsafe { (*r).right = f };
        acc = acc.wrapping_mul(31).wrapping_add(unsafe { (*f).tag });
    }
    st.fresh_sum = acc;
    acc
}

/// One store into one marked object, which is the smallest thing a missing
/// barrier can lose.
///
/// The index is `(seed as i32) % len` and not `seed as usize % len`, because the
/// Go guest writes `int(seed) % len(roots)` and `int` on wasm32 is SIGNED. For
/// the small seeds the harness uses the two agree; transcribing the signed form
/// is what keeps them agreeing if that ever stops being true.
#[no_mangle]
pub extern "C" fn torture_repoint_one(seed: u32) -> u32 {
    let st = s();
    if st.roots.is_empty() {
        return 0;
    }
    let i = ((seed as i32) % st.roots.len() as i32) as usize;
    let f = node(seed, seed.wrapping_mul(2654435761));
    unsafe {
        (*st.roots[i]).right = f;
        (*f).tag
    }
}

/// The oracle for [`torture_repoint`]: a root whose `right` was reclaimed folds
/// `0xdeadbeef` instead, so a lost object is a different number rather than a
/// trap.
#[no_mangle]
pub extern "C" fn torture_repoint_verify() -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for &r in st.roots.iter() {
        unsafe {
            if (*r).right.is_null() {
                acc = acc.wrapping_mul(31).wrapping_add(0xdead_beef);
                continue;
            }
            acc = acc.wrapping_mul(31).wrapping_add((*(*r).right).tag);
        }
    }
    acc
}

#[no_mangle]
pub extern "C" fn torture_repoint_want() -> u32 {
    s().fresh_sum
}

/// `n` nodes that nothing keeps. The sink holds exactly one so that the
/// allocation is real; a Rust port that let the optimiser see the whole
/// lifetime would delete the workload.
#[no_mangle]
pub extern "C" fn torture_garbage(n: u32) -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for i in 0..n {
        let g = node(i, i.wrapping_mul(40503));
        st.garbage_sink = g;
        acc = acc.wrapping_mul(31).wrapping_add(unsafe { (*g).tag });
    }
    acc
}

/// An INTERIOR pointer, deliberately not at the base and deliberately not
/// granule-aligned: `&b[37]` is 148 bytes into a 256-byte block. A collector
/// that only accepts base pointers reclaims this block and the read below comes
/// back as somebody else's bytes.
#[no_mangle]
pub extern "C" fn torture_interior(seed: u32) -> u32 {
    let st = s();
    let mut b: Vec<u32> = Vec::with_capacity(64);
    for i in 0..64u32 {
        b.push(seed.wrapping_add(i));
    }
    st.interior = unsafe { b.as_ptr().add(37) };
    // The Vec header is dropped here and the buffer is not freed: `dealloc` is a
    // no-op, so the only thing keeping the block alive from now on is the
    // interior pointer in the statics -- which is the whole assertion.
    core::mem::forget(b);
    unsafe { *st.interior }
}

#[no_mangle]
pub extern "C" fn torture_interior_read() -> u32 {
    unsafe { *s().interior }
}

/// A pointer ONE PAST THE END of a block, which is the shape `agents/gc.md`
/// section 1 names as the wasip1 hazard and which this collector does NOT retain
/// through -- the read below is the assertion that it does not, not that it
/// does.
#[no_mangle]
pub extern "C" fn torture_one_past(seed: u32) -> u32 {
    let st = s();
    let mut b: Vec<u32> = Vec::with_capacity(8);
    for i in 0..8u32 {
        b.push(seed ^ (i * 7));
    }
    st.one_past_id = b[7];
    st.one_past = b.as_ptr() as u32 + 32;
    core::mem::forget(b);
    st.one_past_id
}

#[no_mangle]
pub extern "C" fn torture_one_past_read() -> u32 {
    let st = s();
    let last = unsafe { core::ptr::read((st.one_past - 4) as *const u32) };
    if last == st.one_past_id {
        1
    } else {
        0
    }
}

/// One object bigger than any size class, so the span-run path and the resumable
/// granule-by-granule scan are both exercised.
#[no_mangle]
pub extern "C" fn torture_large(words: u32) -> u32 {
    let st = s();
    let mut b: Vec<u32> = Vec::with_capacity(words as usize);
    let mut acc: u32 = 0;
    for i in 0..words {
        let v = i.wrapping_mul(2654435761);
        b.push(v);
        acc = acc.wrapping_mul(31).wrapping_add(v);
    }
    st.big = b;
    st.big_mark = acc;
    acc
}

#[no_mangle]
pub extern "C" fn torture_large_read() -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for &v in st.big.iter() {
        acc = acc.wrapping_mul(31).wrapping_add(v);
    }
    acc
}

/// Drops everything. `roots` is REPLACED rather than cleared, so its capacity
/// goes too -- which is what the Go guest's `roots = nil` does and what
/// `torture_kept_bytes` reads afterwards.
#[no_mangle]
pub extern "C" fn torture_drop_all() -> u32 {
    let st = s();
    st.roots = Vec::new();
    st.interior = core::ptr::null();
    st.big = Vec::new();
    st.kept = 0;
    0
}

/// Holds `blocks` blocks of `words` words each, so that a large LIVE set can be
/// built and then verified. `k` is the 1-based block ordinal across the whole
/// lifetime of the list, not within this call.
#[no_mangle]
pub extern "C" fn torture_hold(blocks: u32, words: u32) -> u32 {
    let st = s();
    for _ in 0..blocks {
        let k = st.held.len() as u32 + 1;
        let mut v: Vec<u32> = Vec::with_capacity(words as usize);
        for i in 0..words {
            v.push(k.wrapping_mul(2654435761).wrapping_add(i));
        }
        st.held.push(v);
    }
    torture_hold_verify()
}

/// The sparse oracle: every sixteenth word, folded with the block ordinal after
/// it. Sparse because the point is to notice a block that moved or was
/// reclaimed, and walking every word of a 40 MiB live set to notice that would
/// make the harness the slow part.
#[no_mangle]
pub extern "C" fn torture_hold_verify() -> u32 {
    let st = s();
    let mut acc: u32 = 0;
    for (k, v) in st.held.iter().enumerate() {
        let want = k as u32 + 1;
        let mut i = 0usize;
        while i < v.len() {
            acc = acc.wrapping_mul(31).wrapping_add(v[i]);
            acc = acc.wrapping_mul(31).wrapping_add(want);
            i += 16;
        }
    }
    st.held_sum = acc;
    acc
}

#[no_mangle]
pub extern "C" fn torture_hold_bytes() -> u32 {
    let st = s();
    let mut n = 0u32;
    for v in st.held.iter() {
        n += v.len() as u32 * 4;
    }
    n
}

#[no_mangle]
pub extern "C" fn torture_drop_held() -> u32 {
    let st = s();
    st.held = Vec::new();
    st.held_sum = 0;
    0
}

#[no_mangle]
pub extern "C" fn torture_backed() -> u32 {
    gc::backed_bytes()
}

#[no_mangle]
pub extern "C" fn torture_meta_chunk_bytes() -> u32 {
    gc::meta_chunk_bytes()
}

#[no_mangle]
pub extern "C" fn torture_meta_slice_bytes() -> u32 {
    gc::meta_slice_bytes()
}

/// Destructive white-box probe: forget every free list and every span assignment
/// and adopt the linear memory that exists now. Nothing allocated before it may
/// be touched after.
#[no_mangle]
pub extern "C" fn torture_reinit() -> u32 {
    gc::reinitialize();
    gc::backed_bytes()
}

/// The collector's own root range, so a test can assert where it looks rather
/// than infer it. There is no Go counterpart: TinyGo's `findGlobals` reports the
/// range and nothing in the guest chooses it, while here the crate does.
#[no_mangle]
pub extern "C" fn torture_heap_base() -> u32 {
    gc::heap_base()
}

#[no_mangle]
pub extern "C" fn torture_heap_top() -> u32 {
    gc::heap_top()
}

/// Whether a collector is compiled in, which is the one thing a leaking build of
/// this same source can still answer.
#[no_mangle]
pub extern "C" fn torture_enabled() -> u32 {
    if gc::enabled() {
        1
    } else {
        0
    }
}

/// One field of one root, which has NO Go counterpart and earned its place.
///
/// `torture_verify` folds the whole graph into one number, and a fold that comes
/// back wrong says only that something is wrong. This says WHICH something: the
/// first defect in this port was a checksum that differed between the leaking and
/// collected arms of the SAME Rust guest before any collection had run, and what
/// localised it was reading the nodes one field at a time and finding that every
/// value agreed -- which ruled out the workload and pointed at the allocator,
/// where `allocate` was reading a size-class table that lazy initialisation had
/// not filled in yet. See `fkgc`'s `allocate`.
///
/// `f`: 0 id, 1 tag, 2 left.id, 3 left.tag, 4 the node's address, 5 left's
/// address. The two addresses are expected to differ between toolchains and
/// between arms; the four values are not.
#[no_mangle]
pub extern "C" fn torture_root_at(i: u32, f: u32) -> u32 {
    let st = s();
    if i as usize >= st.roots.len() {
        return 0xffff_ffff;
    }
    let r = st.roots[i as usize];
    unsafe {
        match f {
            0 => (*r).id,
            1 => (*r).tag,
            2 => {
                if (*r).left.is_null() { 0xdead0000 } else { (*(*r).left).id }
            }
            3 => {
                if (*r).left.is_null() { 0xdead0001 } else { (*(*r).left).tag }
            }
            4 => r as u32,
            5 => (*r).left as u32,
            _ => 0,
        }
    }
}
