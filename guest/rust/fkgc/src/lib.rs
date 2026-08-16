//! The guest-side garbage collector for a FkLua **Rust** guest.
//!
//! It is a port of `guest/go/fkgc`'s DESIGN, not a transliteration of its code:
//! the same conservative mark/sweep over size-class spans, the same scaling
//! metadata, the same paced state machine behind the same three exports, the
//! same escape metric and the same storm valve. `agents/gc.md` is the design
//! and this crate is the second implementation of it; where the two differ, the
//! difference is a fact about Rust and is written down where it lives.
//!
//! A guest turns it on with one flag and no source change:
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p myguest \
//!     --features fk/fkgc
//! ```
//!
//! `fk` owns the single `#[global_allocator]` site and the feature chooses what
//! backs it -- the bump arena or this. A guest calls the collector through
//! [`fk::gc`], which is this crate under the feature and a no-op shim without
//! it, so the same source builds both arms. That is `guest/go/fkgc/off.go`'s
//! job done with Cargo instead of build tags.
//!
//! # The root set, which was the go/no-go
//!
//! `CLAUDE.md` recorded the blocking question against this work:
//!
//! > the **root set is the go/no-go** -- `-gc=custom`'s value here is that
//! > TinyGo HANDS OVER `markStack()` and `findGlobals()`, rustc has no
//! > equivalent seam, and Rust keeps live references in wasm locals that a
//! > conservative scan of `[__global_base, __heap_base)` plus the shadow stack
//! > cannot see.
//!
//! The premise is TRUE and the conclusion does not follow, because of WHEN a
//! collection looks at anything. `agents/gc.md`'s safe-point precondition says a
//! step runs only at an outermost dispatch boundary, and at such a point **there
//! is no guest frame at all** -- so there are no live wasm locals to miss. Every
//! reference the guest still holds is in a `static`, which is in
//! `[__global_base, __heap_base)`, or in the heap, which is scanned by tracing.
//! Rust has no third place to keep something across a return.
//!
//! The layout that makes the range right is rustc's, and it was measured rather
//! than assumed (`TestARustGuestsRootRangeIsWhereTheCollectorLooks`):
//!
//! ```text
//! __stack_low  = 0          __stack_high = 1048576   <- the shadow stack
//! __global_base = 1048576   __heap_base  = 1048576+  <- the statics
//! Global[1]: global[0] i32 mutable=1 <__stack_pointer> - init i32=1048576
//! ```
//!
//! rustc links the wasm32 target **stack-first**, so the statics are one
//! contiguous range above the stack and the module has exactly one mutable
//! global, which holds a stack pointer and never a heap pointer. That is the
//! same shape TinyGo's `wasm-unknown.json` produces with `--stack-first`, and
//! it is why the range test is the same two compares in both languages.
//!
//! ## What that costs, stated rather than buried
//!
//! Two things follow, and both are real differences from the Go collector.
//!
//! **1. Allocation never collects, with NO exception.** `guest/go/fkgc` keeps
//! one synchronous `Collect()` inside `allocSpans`, reached when `memory.grow`
//! itself is refused, and it is sound there only because TinyGo's `markStack()`
//! scans the live shadow stack of whatever event handler it landed in. Rust has
//! no such scan and cannot have one, so that path is **deleted** rather than
//! ported: a refused `memory.grow` traps with a diagnostic. The alternative is
//! not a pause, it is a mark that cannot see the frame it is standing in --
//! which frees live objects and reports nothing. See `alloc_spans`.
//!
//! **2. [`collect`] has a precondition and it is not decorative.** It runs a
//! whole cycle synchronously, and marking during it must not miss a frame, so it
//! is sound only when the calling frame holds no heap reference -- i.e. as the
//! ENTIRE body of an exported function, called by the host at an outermost
//! dispatch. That is how the test corpus uses it and it is the only use this
//! crate blesses. [`collect_if_needed`] has no such precondition: it only OPENS
//! a mark phase, and marking cannot TERMINATE except inside [`step`], which only
//! [`fk_gc_step`] calls.
//!
//! The initial root scan [`start`] performs does run inside the guest's own
//! `fk_on_tick`, and missing a wasm local there is harmless in a way worth
//! spelling out: an object reachable only from a local at that instant is either
//! stored somewhere reachable before the call returns -- and the termination
//! attempt re-scans the roots wholesale at a real safe point, which finds it --
//! or it is dropped, and it is garbage. Nothing is swept in between, because a
//! sweep cannot begin until marking terminates.
//!
//! For belt and braces the scan still covers `[approximate stack pointer,
//! __stack_high)`, which is what TinyGo's `markStack()` covers. At a real safe
//! point that range is EMPTY, so it costs nothing where the argument is load
//! bearing; inside `fk_on_tick` it catches whatever rustc happened to spill.
//! **It is not what makes this sound** -- rustc spills only what it must, and a
//! pointer living in a wasm local is invisible to it. The safe-point
//! precondition is the argument; this is a cheap widening of the same net.
//!
//! # What it does not do
//!
//! * Compaction. A conservative collector cannot move what it cannot prove is a
//!   pointer, and `layout` tells us the size and the alignment of a block but
//!   never which of its words are references.
//! * Reclaim on `dealloc`. Rust drops eagerly and this allocator ignores it, for
//!   the reason `guest/go/fkgc/heap.go` gives its own no-op `free`: the free
//!   lists are rebuilt from the bitmap by every sweep, so an explicit free is a
//!   double-free one sweep later. What Rust's eager drop buys here is nothing;
//!   what the collector buys is the span the drop could not release.
//! * Finalizers, threads, or any notion of death more precise than "no integer
//!   in the root set or in a live object happened to look like its address".

#![no_std]
#![allow(clippy::missing_safety_doc)]

mod collect;
mod heap;
mod meta;
mod stats;

pub use collect::{
    budget, collect, collect_if_needed, deadlines, dirty_base, dirty_cap, dirty_overflows,
    effective_budget, mark_bits_set, marked, max_stalls, max_step_work, max_unpaced_folds,
    max_unpaced_work, outruns, pend_empties, phase, rescan_restarts, rescans, root_words,
    set_budget, set_threshold, stalls, start, step, terminations, total_work, unpaced_work,
    work_owed, DIRTY_ALL,
};
pub use heap::{backed_bytes, heap_base, heap_top, init, reinitialize, Collector, FREE_INVARIANT};
pub use meta::{meta_bytes, meta_chunk_bytes, meta_chunks, meta_fixed_bytes, meta_slice_bytes};
pub use stats::{stats, MemStats};

/// Reports whether a collector is compiled in. `true` here by definition: this
/// crate is only in the graph when `fk`'s `fkgc` feature is on.
///
/// The mirror of `guest/go/fkgc`'s `Enabled()`, which is `true` in the
/// `gc.custom` build and `false` in `off.go`. A guest logs it to say which arm
/// it is.
pub const fn enabled() -> bool {
    true
}
