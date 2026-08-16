//! The collector's save/load guest, in Rust -- the MIRROR of
//! `guest/go/examples/gcsave`.
//!
//! Does a heap that is being COLLECTED survive a real Factorio save and reload?
//! What has to survive is not just the guest's own data: it is the ALLOCATOR'S
//! OWN BOOKKEEPING -- the span table, the mark bitmap, the free-run lists, the
//! class cursors, the sweep cursor. `agents/gc.md` says that carriage is free in
//! `table` and `packed` alike, because every one of those lives in guest memory
//! rather than in a Lua structure beside it. "For free" is a claim about a
//! design, and this is the guest that makes it a measurement.
//!
//! The failure it is looking for is specific and quiet: a heap that reloads with
//! a free run pointing at a live object does not trap. It hands the same block
//! out twice, some ticks later, and the guest reads a value it never wrote.
//! `intact` is what catches that -- every retained block is checksummed against
//! what was written into it, so a block reclaimed while live shows up as a
//! number that moved.
//!
//! **IT LOGS THE SAME LINES THE GO GUEST LOGS, BYTE FOR BYTE**, and that is not
//! cosmetic: `scripts/run-roundtrip.sh` and `scripts/run-guest.sh` assert on
//! that text, so a mirror that reworded one line would need a second copy of
//! every pattern in both scripts. Sharing them is what lets one Rust leg reuse
//! assertions the Go leg already proved discriminate.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p gcsave \
//!     --features fk/fkgc
//! fklua mod gcsave.wasm --gc=collected --persist=table
//! ```

#![no_std]

extern crate alloc;

use alloc::string::String;
use alloc::vec::Vec;
use core::cell::UnsafeCell;

use fk::gc;

/// The retained set: a fixed number of slots, each holding a block whose
/// contents are derived from the tick that wrote it. Rewriting one slot per tick
/// means there is always both a live set to keep and fresh garbage to reclaim,
/// which is what makes a collection between ticks do real work.
const NBLOCKS: u32 = 32;

struct Block {
    /// Stored and never read, exactly as the Go guest stores it: it is here so
    /// that a block is more than its payload and a save carries a struct rather
    /// than a slice.
    #[allow(dead_code)]
    tick: u32,
    sum: u32,
    data: Vec<u32>,
}

struct State {
    kept: [Option<Block>; NBLOCKS as usize],
    seen: u32,
    churn: Vec<String>,
    last_phase: u32,
}

impl State {
    const NEW: State = State {
        kept: [const { None }; NBLOCKS as usize],
        seen: 0,
        churn: Vec::new(),
        last_phase: 0,
    };
}

struct Cell(UnsafeCell<State>);
unsafe impl Sync for Cell {}
static S: Cell = Cell(UnsafeCell::new(State::NEW));

#[allow(clippy::mut_from_ref)]
#[inline(always)]
fn s() -> &'static mut State {
    unsafe { &mut *S.0.get() }
}

/// Unsigned decimal, matching Go's `strconv.FormatUint`: no padding, no sign,
/// `0` for zero. Hand-rolled rather than `format!` because every log line in this
/// guest is compared against the Go guest's character for character, and because
/// `core::fmt` is a lot of module for a number.
fn u(n: u32) -> String {
    let mut out = String::new();
    push_u32(&mut out, n);
    out
}

fn push_u32(out: &mut String, mut v: u32) {
    if v == 0 {
        out.push('0');
        return;
    }
    let mut buf = [0u8; 10];
    let mut i = buf.len();
    while v > 0 {
        i -= 1;
        buf[i] = b'0' + (v % 10) as u8;
        v /= 10;
    }
    out.push_str(core::str::from_utf8(&buf[i..]).unwrap_or("?"));
}

/// Fills slot `i` with a block derived from `tick`, and remembers the checksum so
/// a later read can prove the bytes are still the ones that were written.
fn write(i: u32, tick: u32) {
    let n = 8 + (tick % 24);
    let mut data: Vec<u32> = Vec::with_capacity(n as usize);
    let mut sum: u32 = 0;
    for k in 0..n {
        let v = tick.wrapping_mul(2654435761).wrapping_add(k);
        data.push(v);
        sum = sum.wrapping_mul(31).wrapping_add(v);
    }
    s().kept[i as usize] = Some(Block { tick, sum, data });
}

/// Counts the retained blocks whose contents still hash to what was written.
/// This is the whole assertion: a collector that reclaimed a live block does not
/// make it unreadable, it makes it ZERO and then somebody else's.
fn intact() -> u32 {
    let mut ok = 0u32;
    for b in s().kept.iter() {
        let Some(b) = b else { continue };
        let mut sum: u32 = 0;
        for &v in b.data.iter() {
            sum = sum.wrapping_mul(31).wrapping_add(v);
        }
        if sum == b.sum {
            ok += 1;
        }
    }
    ok
}

/// The allocation that gives the collector something to reclaim, written the way
/// `agents/guests.md` tells authors NOT to write it -- which is the point, since
/// the whole argument for a collector is that they should not have to.
fn garbage(tick: u32) {
    let st = s();
    st.churn.clear();
    for k in 0..12u32 {
        let mut line = String::from("gcsave-");
        push_u32(&mut line, tick);
        line.push('-');
        push_u32(&mut line, k);
        st.churn.push(line);
    }
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    // DELIBERATELY AGGRESSIVE, and a real mod should not copy this line. The
    // default threshold is 256 KiB of heap footprint taken since the last
    // collection, which for a guest this small is never -- and a roundtrip that
    // saves a heap no collection has touched proves nothing about carrying one.
    gc::set_threshold(8 << 10);
    // AND A SMALL STEP BUDGET, so that a collection is spread over enough ticks
    // that a save taken at an arbitrary one lands in the MIDDLE of it.
    //
    // 512 IS ALREADY NEAR THE FLOOR ON THIS SIDE and the floor is not the same
    // number it is in Go. Every step re-scans the pages dirtied since the last
    // one and then charges the ROOT RE-SCAN against the same budget, and a Rust
    // guest's statics are far larger than a TinyGo guest's -- this one's root
    // range costs a few hundred granules a termination attempt where the Go
    // churn guest's costs about 36. Below ~512 the mark stops terminating under
    // budget and only `mark_deadline` gets it out, which works, and is a pause,
    // and is not what this guest should be demonstrating. See fkgc's
    // `set_budget`.
    gc::set_budget(512);
    for i in 0..NBLOCKS {
        write(i, i + 1);
    }
    let mut line = String::from("[gcsave] ");
    push_u32(&mut line, NBLOCKS);
    line.push_str(" blocks retained, collector ");
    line.push_str(if gc::enabled() { "ON" } else { "OFF" });
    fk::log(&line);
}

/// Lets the harness sweep the pacing knob without a rebuild.
#[no_mangle]
pub extern "C" fn fk_gc_budget(units: u32) -> u32 {
    gc::set_budget(units);
    gc::budget()
}

/// How many retained blocks still hold what was written into them.
#[no_mangle]
pub extern "C" fn fk_gc_intact() -> u32 {
    intact()
}

/// The collector's own view of itself, by the same indices the Go guest uses.
#[no_mangle]
pub extern "C" fn fk_gc_stat(which: u32) -> u32 {
    let st = gc::stats();
    match which {
        0 => st.heap_bytes,
        1 => st.live_bytes,
        3 => st.collections,
        4 => st.grows,
        9 => st.phase,
        10 => st.steps,
        14 => st.deadlines,
        _ => 0,
    }
}

/// The first tick after a save is loaded, and then it is gone.
///
/// THIS IS WHERE THE EVIDENCE IS. A collection can be half done when a save is
/// taken, and what has to come back is two different things.
///
/// The collector's own state -- phase, mark bitmap, gray stack, sweep cursor,
/// free runs -- is all linear memory and comes back with it, which is what
/// `phase` here reports: a 1 or a 2 means the save landed INSIDE a collection and
/// the guest resumed it rather than starting over.
///
/// What did NOT come back is the write barrier and the page set. `MEMDIRTY` is a
/// chunk local, so a guest that was marking resumes with it false unless
/// control.lua re-arms from `storage.fk_gc` -- which it does -- and the dirty
/// page set is a Lua table no `storage` entry mirrors, so the first step after
/// the load is told the record was lost and re-scans everything it had marked.
/// `intact` is what says all of that worked.
#[no_mangle]
pub extern "C" fn fk_after_load() {
    let st = gc::stats();
    let mut line = String::from("[gcsave] loaded: ");
    line.push_str(&u(intact()));
    line.push('/');
    line.push_str(&u(NBLOCKS));
    line.push_str(" blocks intact, ");
    line.push_str(&u(st.collections));
    line.push_str(" collections so far, phase=");
    line.push_str(&u(st.phase));
    fk::log(&line);
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    let st = s();
    st.seen += 1;
    garbage(tick);
    write(tick % NBLOCKS, tick);

    // A COLLECTION IS STARTED AT AN OUTERMOST DISPATCH, which is the only place
    // one may begin: the wasm operand stack is empty between exported calls and
    // the shadow stack is back at its initial value, so every live reference is
    // in a static or in the heap. Pressure-gated, so an idle tick costs a
    // comparison.
    //
    // ...and then started again the moment it is not, which is the second
    // DELIBERATELY AGGRESSIVE line in this file: a save/load roundtrip has to
    // land INSIDE a collection to prove that a half-done one is carried, and a
    // guest that collects for seven ticks out of every twenty is a guest whose
    // save tick mostly misses. A real mod must not do this -- it keeps the write
    // barrier armed forever.
    if !gc::collect_if_needed() {
        gc::start();
    }

    // A LINE WHENEVER THE PHASE CHANGES, because the roundtrip leg has to choose
    // save ticks that land INSIDE a collection and the only honest way to choose
    // them is to read where the phases are. The cadence of a collection is a
    // property of the collector and it moves when the collector does.
    let p = gc::phase();
    if p != st.last_phase {
        st.last_phase = p;
        let mut line = String::from("phase ");
        line.push_str(&u(tick));
        line.push_str(" -> ");
        line.push_str(&u(p));
        line.push_str(" cycles=");
        line.push_str(&u(gc::stats().collections));
        fk::log(&line);
    }

    if tick % 10 == 0 {
        let g = gc::stats();
        let mut line = String::from("tick ");
        for (k, v) in [
            ("", tick),
            (" seen=", st.seen),
            (" live=", g.live_bytes),
            (" cycles=", g.collections),
            (" grows=", g.grows),
            (" deadlines=", g.deadlines),
            (" marked=", gc::marked()),
            (" stalls=", gc::stalls()),
            (" maxstalls=", gc::max_stalls()),
            (" owed=", gc::work_owed()),
            (" pempty=", gc::pend_empties()),
            (" terms=", gc::terminations()),
            (" phase=", g.phase),
            (" steps=", g.steps),
            (" blocks=", NBLOCKS),
            (" intact=", intact()),
        ] {
            line.push_str(k);
            push_u32(&mut line, v);
        }
        fk::log(&line);
    }
}
