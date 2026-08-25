//! Guest-side half of the FkLua host boundary, for Rust.
//!
//! A guest depends on this crate, writes ordinary `no_std` Rust, and marks its
//! entry points with `#[no_mangle] pub extern "C"`. FkLua compiles the wasm to
//! Lua and packages it as a mod; the mod's `control.lua` binds the other half
//! of every function declared here.
//!
//! ```ignore
//! #[no_mangle]
//! pub extern "C" fn fk_on_init() { fk::log("hello from Rust"); }
//! ```
//!
//! # Why `no_std`
//!
//! `std` for `wasm32-unknown-unknown` assumes an operating system this target
//! does not have. `alloc` is what a guest actually wants -- `String`, `Vec`,
//! `BTreeMap` -- and it needs only a global allocator, which this crate
//! provides. Factorio's sandbox has no files, no clock and no threads, so
//! nothing above `alloc` would have anywhere to go.
//!
//! # Subscriptions go in `_initialize`, not `fk_on_init`
//!
//! `control.lua` calls a guest's `_initialize` export on **every load**;
//! `script.on_init` fires once, when a save is CREATED. A subscription made in
//! `fk_on_init` therefore vanishes the first time the save is reloaded — the
//! API calls keep working and the events silently stop arriving, which is a
//! nasty shape of bug to find.
//!
//! A Go guest never meets this: TinyGo runs package `init()` from
//! `_initialize`. Rust has no pre-main initialiser in a cdylib reactor, so a
//! guest exports the hook itself:
//!
//! ```ignore
//! #[no_mangle]
//! pub extern "C" fn _initialize() { fkapi::subscribe(fkapi::EVENT_ON_TICK); }
//! ```
//!
//! # Build
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown
//! ```
//!
//! That is the whole recipe. It used to need `RUSTFLAGS` disabling bulk-memory
//! plus a `wasm-opt` lowering pass, because rustc enables bulk-memory by
//! default and a precompiled `compiler_builtins` puts `memory.copy` in every
//! module regardless of the flag. FkLua compiles those two instructions
//! natively now -- 49x faster than the lowering it replaced -- so a stock build
//! goes straight through. `internal/guest` checks rustc's feature set on every
//! test run and fails if a new one appears.

#![no_std]

extern crate alloc;

use alloc::alloc::Layout;
#[cfg(not(feature = "fkgc"))]
use alloc::alloc::GlobalAlloc;
use core::cell::UnsafeCell;

// wasm has no string type, so a string crosses as a (pointer, length) pair into
// the guest's linear memory and the host reassembles it. Bound in
// runtime/lua/fk_mod.lua.
#[link(wasm_import_module = "env")]
extern "C" {
    fn fk_log(ptr: u32, len: u32);
    fn fk_print(ptr: u32, len: u32);
}

/// Writes a line to `factorio-current.log`.
pub fn log(s: &str) {
    unsafe { fk_log(s.as_ptr() as u32, s.len() as u32) }
}

/// Writes a line to the in-game console.
///
/// Falls back to the log at load time, when `game` does not exist yet -- the
/// host side decides, not the guest.
pub fn print(s: &str) {
    unsafe { fk_print(s.as_ptr() as u32, s.len() as u32) }
}

#[link(wasm_import_module = "fk")]
extern "C" {
    #[link_name = "defer"]
    fn fk_defer() -> u32;
    #[link_name = "last_error"]
    fn fk_last_error(ptr: u32, cap: u32) -> u32;
}

/// What Factorio said when the last host call failed.
///
/// A status is an `i32` and a message is not, so a binding that returns
/// `Err(Status)` can only tell you the KIND of failure. This is the sentence the
/// engine raised WITH, which is the difference between knowing that a call was
/// refused and knowing why:
///
/// ```ignore
/// if something().is_err() {
///     fk::log(&format!("refused: {}", String::from_utf8_lossy(&fk::last_error())));
/// }
/// ```
///
/// IT DESCRIBES THE CALL THAT JUST RETURNED. The host clears the slot as each
/// host call begins, so this is empty after a call that succeeded rather than
/// carrying some earlier tick's failure -- read it immediately, where the error
/// is still in hand. An empty `Vec` means the last call did not fail.
///
/// # Why `Vec<u8>` and not `String`
///
/// A Lua string is an arbitrary byte sequence and a Rust `String` is not. The
/// generated bindings learned that the expensive way -- 738 readers were
/// `from_utf8_lossy`, which rewrites every byte outside UTF-8 and changes the
/// length while it does it -- so nothing here hands back a type whose invariant
/// the engine never promised. Call `String::from_utf8_lossy` yourself if a
/// display string is what you want; the bytes are what arrived.
///
/// # It is diagnostic
///
/// Log it; do not branch on it. The text is an engine implementation detail that
/// a point release may reword, and a mod that behaved differently because of a
/// wording is a mod that behaves differently on two Factorios. A TEST asserting
/// the exact text is the honest exception, and is what this exists for.
pub fn last_error() -> alloc::vec::Vec<u8> {
    // A STACK BUFFER, unlike the Go side's static one: rustc keeps an array
    // whose address is taken on the stack, where TinyGo's ptrtoint defeats the
    // promotion and forces a heap allocation. 256 bytes because engine refusals
    // are sentences; a longer one costs a second call rather than being cut.
    let mut buf = [0u8; 256];
    let n = unsafe { fk_last_error(buf.as_mut_ptr() as u32, buf.len() as u32) } as usize;
    if n == 0 {
        return alloc::vec::Vec::new();
    }
    if n <= buf.len() {
        return buf[..n].to_vec();
    }
    // THE RETURN IS THE FULL LENGTH RATHER THAN WHAT WAS COPIED, which is what
    // makes a fixed buffer safe: a message that did not fit is asked for again
    // with room, instead of silently arriving short.
    let mut big = alloc::vec![0u8; n];
    let m = unsafe { fk_last_error(big.as_mut_ptr() as u32, n as u32) } as usize;
    big.truncate(core::cmp::min(m, n));
    big
}

/// Asks for this guest's `fk_on_deferred` export to be called **once** on the
/// next tick.
///
/// This is how a guest batches. Factorio delivers a blueprint paste as one
/// `on_built_entity` **per entity**, each a separate dispatch raised by the
/// engine's own loop, so there is no "after this event's handlers finish" to
/// hook -- accumulating during the burst and doing the work once is the only
/// shape that costs O(1) rather than O(P). Call it as often as you like within
/// a tick: the registration happens once and is torn down again the moment the
/// flush runs, so an idle guest pays nothing per tick.
///
/// ```ignore
/// #[no_mangle]
/// pub extern "C" fn fk_on_deferred() { rebuild_whatever_got_dirty(); }
/// ```
///
/// The flush lands on the FOLLOWING tick, not at the end of the current one:
/// Factorio has no end-of-tick hook, and on_tick for the current tick has
/// already been raised by the time a build event arrives. Work deferred and
/// then saved before it ran is re-armed when the save is loaded.
pub fn defer() {
    unsafe {
        fk_defer();
    }
}

// ---------------------------------------------------------------------------
// The allocator -- ONE `#[global_allocator]` SITE, TWO IMPLEMENTATIONS.
//
// The site is here and only here, and the `fkgc` feature chooses what backs it.
// That is not tidiness: two `#[global_allocator]` attributes in one module graph
// is a hard link error, so a design that put one in `fkgc` and one here could be
// broken by any build that happened to link both. There is nothing to link both
// of now.
//
// WITHOUT the feature: a bump allocator that never frees, matching TinyGo's
// -gc=leaking, and for the same reason -- a collector's cost lands inside a
// lockstep game loop, where one client stalling desyncs everyone. Guest memory
// is an arena that only grows, and `agents/guests.md`'s heap budget is the
// advice that follows.
//
// WITH it: `fkgc::Collector`, a conservative paced mark/sweep over size-class
// spans. See `guest/rust/fkgc` and `agents/gc.md`.
//
// Deterministic either way, which is the property that actually matters here --
// every client runs identical Lua over identical bytes and must reach identical
// addresses. The bump arena is deterministic because it never reuses; the
// collector is deterministic because its sweep walks spans and slots in
// ascending address order and its free runs are threaded in ascending address
// order. A free-list allocator whose layout depended on DROP order would not be,
// which is why `dealloc` is a no-op in both.
// ---------------------------------------------------------------------------

/// The collector, when there is one.
///
/// Without the `fkgc` feature this is a no-op shim with the same surface, so a
/// guest writes `fk::gc::collect_if_needed()` unconditionally and changes ONE
/// FLAG to switch arms. That is `guest/go/fkgc/off.go`'s job, done with Cargo.
#[cfg(feature = "fkgc")]
pub use fkgc as gc;
#[cfg(not(feature = "fkgc"))]
pub mod gc;

#[cfg(feature = "fkgc")]
#[global_allocator]
static ALLOC: fkgc::Collector = fkgc::Collector;

#[cfg(not(feature = "fkgc"))]
struct Bump {
    next: UnsafeCell<usize>,
}

// Sound because a Factorio mod is single-threaded by construction: wasm without
// the threads proposal has one thread, and the target does not enable it.
#[cfg(not(feature = "fkgc"))]
unsafe impl Sync for Bump {}

#[cfg(not(feature = "fkgc"))]
unsafe impl GlobalAlloc for Bump {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let next = self.next.get();
        // First call: start after everything the linker placed. __heap_base is
        // where wasm-ld says the static data ends.
        if *next == 0 {
            *next = heap_base();
        }
        let align = layout.align();
        let start = (*next + align - 1) & !(align - 1);
        let end = start + layout.size();

        // Grow linear memory in 64 KiB pages when the arena runs past it.
        let have = core::arch::wasm32::memory_size(0) * 65536;
        if end > have {
            let need = (end - have + 65535) / 65536;
            if core::arch::wasm32::memory_grow(0, need) == usize::MAX {
                return core::ptr::null_mut();
            }
        }
        *next = end;
        start as *mut u8
    }

    // Never reclaims, by design. See the comment above the type.
    unsafe fn dealloc(&self, _ptr: *mut u8, _layout: Layout) {}
}

#[cfg(not(feature = "fkgc"))]
fn heap_base() -> usize {
    extern "C" {
        static __heap_base: u8;
    }
    unsafe { &__heap_base as *const u8 as usize }
}

#[cfg(not(feature = "fkgc"))]
#[global_allocator]
static ALLOC: Bump = Bump {
    next: UnsafeCell::new(0),
};

/// The panic handler.
///
/// A panic cannot unwind out of wasm into Lua, so it reports and traps. The
/// message reaches `factorio-current.log` first, because a bare `unreachable`
/// arriving in the game tells an author nothing about where it came from.
#[panic_handler]
fn panic(info: &core::panic::PanicInfo) -> ! {
    // Formatting a panic message allocates, and the allocator may be the thing
    // that just failed. The location is static, so it costs nothing.
    if let Some(loc) = info.location() {
        log("fklua: rust panic in");
        log(loc.file());
    } else {
        log("fklua: rust panic");
    }
    core::arch::wasm32::unreachable()
}

// ---------------------------------------------------------------------------
// The host-call ABI's guest-side exports.
//
// The host allocates INTO guest memory through these whenever a value coming
// back is variable-length -- a string, an array, a dictionary, a tier-2 value.
// See agents/abi.md.
// ---------------------------------------------------------------------------

/// Allocates `n` bytes for the host to write into.
///
/// CALL-SCOPED BY CONSTRUCTION, and under the collector that is a load-bearing
/// claim rather than a description. The buffer is reachable from nothing the
/// collector can see -- the host holds the only pointer, on the Lua side -- so it
/// survives exactly as long as the guest call it was made in, and no longer.
/// That is sound because a collection step runs only at an outermost dispatch
/// boundary and therefore never while this buffer is in use. It is the same
/// precondition `CLAUDE.md` states for every `(ptr, len)` a guest hands the host:
/// consume it BEFORE the call returns. Anything that buffers one needs
/// [`fk_alloc_static`].
///
/// THERE IS NO `fk_arena_mark` HERE AND THAT IS DELIBERATE, because there is no
/// arena to rewind. The Go substrate routes `fk_alloc` through a chunked
/// marshalling arena that every generated binding brackets, and exports
/// `fk_arena_mark`/`fk_arena_release` so `fk_mod.lua` can bracket the direction
/// that has no binding -- a host-INITIATED dispatch, where nothing on the guest
/// side made the call. Missing that bracket, an event whose string payload
/// overflows the 4 KiB scratch region advanced Go's bump pointer forever, and
/// the arena's chunks are rooted so even the collector could not take them back.
///
/// This side does not have that shape. `fk_alloc` is the GLOBAL allocator, so:
/// under `--features fkgc` the block is ordinary garbage -- nothing the
/// collector can see refers to it once the dispatch returns -- and the next
/// collection reclaims it, which is bounded rather than a leak; without the
/// feature it is the same never-reclaimed allocation as every other allocation
/// in that arm, whose whole contract is that nothing is ever reclaimed.
///
/// So exporting the pair would be a claim this allocator cannot honour: the
/// host would take a mark, believe it had reclaimed, and have reclaimed
/// nothing. `fk_mod.lua` feature-detects the PAIR for exactly this reason, and
/// a guest that exports neither behaves as it always has. Closing the asymmetry
/// means giving this crate a real marshalling arena of its own -- which is also
/// what would finally make fkapi's generated `AllocMark` mean something -- and
/// it is a change to what every Rust host call costs, not a bug fix.
#[no_mangle]
pub extern "C" fn fk_alloc(n: u32) -> u32 {
    if n == 0 {
        return 0;
    }
    let layout = match Layout::from_size_align(n as usize, 8) {
        Ok(l) => l,
        Err(_) => return 0,
    };
    // Through the GLOBAL allocator rather than through a named one, so this is
    // the same line in both arms and switching them cannot change what it means.
    unsafe { alloc::alloc::alloc(layout) as u32 }
}

/// Allocates `n` bytes that OUTLIVE the call that asked for them.
///
/// `fk_mod.lua` caches an event scratch buffer per nesting level, and every
/// level past the first is taken from inside a nested dispatch. On the Go side
/// [`fk_alloc`] is an arena the calling binding reclaims on return, so such a
/// buffer has to come from somewhere else; under the bump arena here the two are
/// the same leak, and the export exists so that `control.lua` has ONE name to
/// call whichever guest language wrote it. See `agents/abi.md`.
///
/// UNDER THE COLLECTOR THE TWO ARE NOT THE SAME, and the difference is the whole
/// reason this export is not an alias any more. Nothing the collector can see
/// refers to this buffer -- the only pointer to it is in a Lua table -- so the
/// first sweep after the call that made it would hand its span to somebody else
/// while `fk_mod.lua` went on writing event data into it. Making the reclaim real
/// turns `guest/go/fkapi`'s pin list from a leftover into a requirement, and this
/// is that list: a `Vec` in a `static`, which the root scan reads, whose contents
/// are the addresses being pinned.
///
/// It grows without bound, exactly as the Go side's does, and that is correct
/// rather than sloppy: the host asks for one buffer per DISPATCH NESTING LEVEL
/// and caches it in `storage` beside the HEAP the buffer lives in, so the list
/// reaches the deepest nesting the mod ever performs and stops. That bound held
/// only WITHIN A SESSION until P12: the cache was a Lua local, which a load
/// rebuilds empty while the heap comes back from the save, so every load
/// allocated a twin beside a buffer that was already there. See
/// `fk_mod.lua`'s `publish_buffers`.
#[no_mangle]
pub extern "C" fn fk_alloc_static(n: u32) -> u32 {
    let p = fk_alloc(n);
    #[cfg(feature = "fkgc")]
    if p != 0 {
        unsafe { (*KEPT.0.get()).push(p) };
    }
    p
}

/// The pin list. See [`fk_alloc_static`].
///
/// A `Vec` and not a fixed array because the bound is the mod's nesting depth,
/// which the mod chooses; and a `Vec` in a `static` rather than a `static mut`
/// array of addresses because the indirection is what makes it WORK -- the static
/// holds the vector's pointer, the root scan marks the vector's buffer through
/// it, and scanning that buffer marks every address in it. A fixed array of
/// addresses in `.bss` would also be scanned, which is why the choice is about
/// the bound and not about reachability.
#[cfg(feature = "fkgc")]
struct Kept(UnsafeCell<alloc::vec::Vec<u32>>);
#[cfg(feature = "fkgc")]
unsafe impl Sync for Kept {}
#[cfg(feature = "fkgc")]
static KEPT: Kept = Kept(UnsafeCell::new(alloc::vec::Vec::new()));

/// Pairs with [`fk_alloc`]. A no-op, like TinyGo's.
#[no_mangle]
pub extern "C" fn fk_free(_ptr: u32) {}

/// The string scratch region: where the host writes returned strings instead of
/// calling [`fk_alloc`] for each one.
///
/// Sound because the lifetime is call-scoped -- the generated binding copies the
/// bytes into a `String` and never looks at the pointer again. See
/// `bind_scratch` in `runtime/lua/fk_abi.lua` for the re-entrancy rules the host
/// keeps, which are the non-obvious half.
///
/// A `static mut` rather than an allocation: the address must be fixed for the
/// life of the module so the host can be told once, and nothing may reclaim it.
/// It lives in linear memory like everything else, so it is saved and restored
/// with the heap and needs no persistence handling -- its contents between calls
/// mean nothing either way.
///
/// 4 KiB is one packed-mode page, which is the point: a bump allocator walking
/// the heap dirties a different page on every call, this dirties the same one.
/// Anything that does not fit falls back to [`fk_alloc`], so the size is a
/// tuning choice and never a correctness one.
static mut FK_SCRATCH: [u8; 4096] = [0; 4096];

#[no_mangle]
pub extern "C" fn fk_scratch_base() -> u32 {
    core::ptr::addr_of!(FK_SCRATCH) as u32
}

#[no_mangle]
pub extern "C" fn fk_scratch_size() -> u32 {
    4096
}
