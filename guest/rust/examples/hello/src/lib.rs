//! The M8 end-to-end guest: a Rust program that becomes a Factorio mod.
//!
//! This is deliberately a LINE-FOR-LINE mirror of `guest/go/examples/hello`,
//! because the milestone's gate is that the corpus produces *identical* results
//! under both toolchains. Two programs that merely both work would not test the
//! thing M8 exists to test -- whether the same semantics survive two very
//! different front ends.
//!
//! It exercises what a hello-world would not: a map, a growing vector, string
//! building, 64-bit arithmetic and floating point. Each is a different part of
//! the emitter.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown
//! fklua mod hello.wasm --name fk-hello-rs --version 0.1.0 --author you
//! ```

#![no_std]

extern crate alloc;

use alloc::collections::BTreeMap;
use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;

// Guest state. Under the default --persist=table this SURVIVES a save: the
// whole linear memory is carried, so the map, the vector and the bump
// allocator's cursor all come back as they were.
//
// `static mut` rather than a lock: wasm without the threads proposal has one
// thread, so there is no second accessor to race with. The 2024-edition
// `static_mut_refs` lint dislikes this shape, which is why access goes through
// raw pointers below rather than through &mut.
static mut COUNTS: Option<BTreeMap<&'static str, u32>> = None;
static mut HISTORY: Option<Vec<u32>> = None;
static mut TOTAL: u64 = 0;

fn counts() -> &'static mut BTreeMap<&'static str, u32> {
    unsafe {
        let p = &raw mut COUNTS;
        (*p).get_or_insert_with(BTreeMap::new)
    }
}

fn history() -> &'static mut Vec<u32> {
    unsafe {
        let p = &raw mut HISTORY;
        (*p).get_or_insert_with(Vec::new)
    }
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    fk::log("hello from Rust, running as Lua inside Factorio");
    let mut s = String::from("guest built with Rust: ");
    s.push_str(&describe());
    fk::log(&s);
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    history().push(tick);
    unsafe {
        let p = &raw mut TOTAL;
        *p += tick as u64;
    }

    let bucket = if tick % 15 == 0 {
        "fizzbuzz"
    } else if tick % 3 == 0 {
        "fizz"
    } else if tick % 5 == 0 {
        "buzz"
    } else {
        "plain"
    };
    *counts().entry(bucket).or_insert(0) += 1;

    // On a schedule rather than every tick: the shape a real mod wants, and it
    // keeps the expected output small.
    if tick % 10 == 0 {
        fk::log(&report(tick));
    }
}

/// String building, map lookup, i64 and f64 in one line -- each a different
/// part of the emitter.
fn report(tick: u32) -> String {
    let hist = history();
    let total = unsafe {
        let p = &raw const TOTAL;
        *p
    };
    let mean = if hist.is_empty() {
        0.0
    } else {
        total as f64 / hist.len() as f64
    };
    let c = counts();
    let get = |k: &str| *c.get(k).unwrap_or(&0);
    format!(
        "tick {} seen={} fizz={} buzz={} fizzbuzz={} sum={} mean={:.2}",
        tick,
        hist.len(),
        get("fizz"),
        get("buzz"),
        get("fizzbuzz"),
        total,
        mean
    )
}

/// A 64-bit multiply and an xor, which lower to (lo, hi) pair arithmetic rather
/// than anything native.
fn describe() -> String {
    let mut h: u64 = 1469598103934665603;
    for c in b"fklua" {
        h ^= *c as u64;
        h = h.wrapping_mul(1099511628211);
    }
    format!("fnv64(fklua)={:x}", h)
}
