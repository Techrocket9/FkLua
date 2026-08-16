//! The Rust half of the collector's configuration guest -- the MIRROR of
//! `guest/go/examples/gcconfig`, whose header carries the long form.
//!
//! One question: does the configuration a guest installs BEFORE ITS FIRST
//! ALLOCATION survive the collector coming up? On this side the answer was NO,
//! and this guest is what says so.
//!
//! `fkgc` has no `initHeap` a runtime calls, so `heap::initialize()` is LAZY --
//! funnelled through `alloc_spans`, i.e. reached by the first allocation. It
//! assigns `threshold` and `budget` their defaults unconditionally, so the
//! obvious shape
//!
//! ```ignore
//! #[no_mangle] pub extern "C" fn fk_on_init() {
//!     fk::gc::set_threshold(128 << 10);   // discarded
//!     build_everything();                 // ...by the first allocation in here
//! }
//! ```
//!
//! kept the default and said nothing. Measured downstream (fklua-ports'
//! AutoDeconstruct, finding 3): `since_gc=135168` against a requested 131,072
//! with `cycles=0` for a whole verification run, because the collector's own
//! copy was 262,144.
//!
//! **WHERE THIS GUEST IS NOT A LINE-FOR-LINE MIRROR, AND WHY.** The Go guest
//! installs its configuration from a package `init`; rustc emits no `_initialize`
//! for a plain `cdylib`, so there is no equivalent hook and the honest Rust
//! shape is the first statement of an export -- which is the shape the port
//! actually wrote. The property under test is identical either way: the setter
//! runs, then the first allocation happens, then the collector is asked what it
//! thinks the value is.
//!
//! **EACH EXPORT WANTS ITS OWN INSTANCE**, and that is a property of the bug
//! rather than of the guest: there is only one first allocation per module, so
//! whichever export runs first is the only one whose setter is exposed to it. A
//! test that called both in one Lua state would find the second passing for the
//! wrong reason. `TestConfigurationInstalledBeforeTheFirstAllocationSurvives`
//! runs them separately and says so.

#![no_std]

extern crate alloc;

use alloc::vec::Vec;
use core::cell::UnsafeCell;

use fk::gc;

/// Deliberately not the defaults, and deliberately far from them: 777 against a
/// default budget of 1024, and 4 KiB against a default threshold of 256 KiB. A
/// value that merely LOOKED plausible next to the default would let a clobber
/// pass as rounding. Transcribed from the Go guest.
const EARLY_BUDGET: u32 = 777;
const EARLY_THRESHOLD: u32 = 4 << 10;

/// The allocation this guest makes is kept alive on purpose. A collection that
/// reclaimed it would move `since_gc` under our feet, and the question here is
/// about configuration rather than about reclamation.
struct Kept(UnsafeCell<Vec<Vec<u32>>>);
unsafe impl Sync for Kept {}
static KEPT: Kept = Kept(UnsafeCell::new(Vec::new()));

fn allocate() {
    // SAFETY: single-threaded wasm, and nothing re-enters this.
    let kept = unsafe { &mut *KEPT.0.get() };
    for _ in 0..64 {
        kept.push(alloc::vec![0u32; 64]);
    }
}

/// The budget the collector is actually pacing with.
#[no_mangle]
pub extern "C" fn config_budget() -> u32 {
    gc::set_budget(EARLY_BUDGET);
    allocate();
    gc::budget()
}

/// Whether the collector agrees that enough has been handed out.
///
/// It returns 1 only if the collector's threshold is the one that was installed
/// above the allocation: ~16 KiB is four times the early threshold and one
/// sixteenth of the default, so the two answers are 1 and 0 rather than a
/// difference in timing.
#[no_mangle]
pub extern "C" fn config_collects() -> u32 {
    gc::set_threshold(EARLY_THRESHOLD);
    allocate();
    if gc::collect_if_needed() {
        1
    } else {
        0
    }
}

/// How many bytes the collector thinks have been handed out. Reported so that a
/// failure of `config_collects` can be read: a zero here is a guest that
/// allocated nothing, which is a different bug from a threshold that was
/// clobbered.
#[no_mangle]
pub extern "C" fn config_since() -> u32 {
    gc::stats().since_gc
}
