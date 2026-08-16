//! The cross-language benchmark guest, Rust side.
//!
//! A deliberate LINE-FOR-LINE mirror of `bench/guests/go/main.go`, for the same
//! reason `guest/rust/examples/hello` mirrors the Go one: two programs that
//! merely both work would not test whether the same semantics survive two very
//! different front ends. Here it matters twice over, because the harness
//! refuses to report a timing unless both languages and the hand-written Lua
//! return the SAME checksum.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown
//! ```

#![no_std]

extern crate alloc;

use alloc::vec;
use alloc::vec::Vec;
use core::alloc::{GlobalAlloc, Layout};

// A bump allocator that GROWS linear memory, never frees, and starts at
// whatever the static data ends at.
//
// Both halves of that matter for a fair comparison, and the first version got
// the second one wrong. It bump-allocated out of a 24 MiB static array, which
// made the module declare 26,279,936 bytes of linear memory -- and a guest's
// linear memory is a Lua TABLE, so Rust was running against 6,569,984 entries
// where TinyGo started at 32,768. A 200x difference in table size is a cache
// locality handicap that has nothing to do with Rust.
//
// Growing on demand is also what TinyGo's -gc=leaking does, so the two guests
// now reach the same size for the same work. Never freeing matches it too:
// pitting Rust's real allocator against Go's disabled one would measure the
// allocators rather than the generated code.
struct Bump;

static mut CURSOR: usize = 0;
static mut END: usize = 0;

#[global_allocator]
static ALLOC: Bump = Bump;

unsafe impl GlobalAlloc for Bump {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        unsafe {
            if END == 0 {
                // First allocation: start at the current end of linear memory,
                // which is past everything the linker laid out.
                CURSOR = core::arch::wasm32::memory_size(0) << 16;
                END = CURSOR;
            }
            let a = l.align();
            let p = (CURSOR + a - 1) & !(a - 1);
            let need = p + l.size();
            if need > END {
                let pages = (need - END + 65535) >> 16;
                if core::arch::wasm32::memory_grow(0, pages) == usize::MAX {
                    core::arch::wasm32::unreachable()
                }
                END += pages << 16;
            }
            CURSOR = need;
            p as *mut u8
        }
    }
    unsafe fn dealloc(&self, _: *mut u8, _: Layout) {}
}

#[panic_handler]
fn panic(_: &core::panic::PanicInfo) -> ! {
    core::arch::wasm32::unreachable()
}

// ---------------------------------------------------------------------------
// PURE kernels
// ---------------------------------------------------------------------------

static mut WORDS: Vec<u32> = Vec::new();

#[no_mangle]
pub extern "C" fn pure_setup(n: i32) {
    unsafe {
        let mut v: Vec<u32> = Vec::with_capacity(n as usize);
        for i in 0..n {
            v.push((i as u32).wrapping_mul(2654435761));
        }
        WORDS = v;
    }
}

#[no_mangle]
pub extern "C" fn pure_sum(passes: i32) -> u32 {
    unsafe {
        let w = &*&raw const WORDS;
        let mut acc: u32 = 0;
        for _ in 0..passes {
            let mut s: u32 = 0;
            for &x in w.iter() {
                s = s.wrapping_add(x);
            }
            acc = acc.wrapping_add(s);
        }
        acc
    }
}

#[no_mangle]
pub extern "C" fn pure_prng(n: i32) -> u32 {
    let mut x: u32 = 2463534242;
    for _ in 0..n {
        x ^= x << 13;
        x ^= x >> 17;
        x ^= x << 5;
    }
    x
}

static mut FA: Vec<f64> = Vec::new();
static mut FB: Vec<f64> = Vec::new();

#[no_mangle]
pub extern "C" fn dot_setup(n: i32) {
    unsafe {
        let mut a: Vec<f64> = Vec::with_capacity(n as usize);
        let mut b: Vec<f64> = Vec::with_capacity(n as usize);
        for i in 0..n {
            a.push(i as f64 * 0.5);
            b.push(i as f64 * 0.25 + 1.0);
        }
        FA = a;
        FB = b;
    }
}

#[no_mangle]
pub extern "C" fn pure_dot(passes: i32) -> f64 {
    unsafe {
        let (a, b) = (&*&raw const FA, &*&raw const FB);
        let mut acc = 0.0f64;
        for _ in 0..passes {
            let mut s = 0.0f64;
            for i in 0..a.len() {
                s += a[i] * b[i];
            }
            acc += s;
        }
        acc
    }
}

// ---------------------------------------------------------------------------
// REALISTIC kernels
// ---------------------------------------------------------------------------

#[derive(Clone, Copy)]
struct Entity {
    kind: u8,
    active: bool,
    x: i32,
    y: i32,
    amount: u32,
    quality: u16,
}

static mut ENTS: Vec<Entity> = Vec::new();

#[no_mangle]
pub extern "C" fn ents_setup(n: i32) {
    unsafe {
        let mut v: Vec<Entity> = Vec::with_capacity(n as usize);
        for i in 0..n {
            v.push(Entity {
                kind: (i % 7) as u8,
                active: i % 3 != 0,
                x: (i % 512) - 256,
                y: (i / 512) - 256,
                amount: (i as u32).wrapping_mul(2654435761) % 1000,
                quality: (i % 5) as u16,
            });
        }
        ENTS = v;
    }
}

#[no_mangle]
pub extern "C" fn real_entities(passes: i32) -> u32 {
    unsafe {
        let ents = &*&raw const ENTS;
        let mut acc: u32 = 0;
        for _ in 0..passes {
            let mut totals = [0u32; 7];
            for e in ents.iter() {
                if !e.active || e.quality == 0 {
                    continue;
                }
                if e.x < -128 || e.x > 128 {
                    continue;
                }
                totals[e.kind as usize] = totals[e.kind as usize].wrapping_add(e.amount);
            }
            for t in totals.iter() {
                acc = acc.wrapping_mul(31).wrapping_add(*t);
            }
        }
        acc
    }
}

static mut GRID: Vec<u8> = Vec::new();

#[no_mangle]
pub extern "C" fn grid_setup(side: i32) {
    unsafe {
        let n = (side * side) as usize;
        let mut g: Vec<u8> = vec![0u8; n];
        for i in 0..n {
            let h = (i as u32).wrapping_mul(2654435761);
            if (h >> 16) % 10 < 3 {
                g[i] = 1;
            }
        }
        GRID = g;
    }
}

#[no_mangle]
pub extern "C" fn real_grid(side: i32, passes: i32) -> u32 {
    unsafe {
        let grid = &*&raw const GRID;
        let n = (side * side) as usize;
        let mut acc: u32 = 0;
        let mut seen: Vec<u8> = vec![0u8; n];
        let mut stack: Vec<i32> = Vec::with_capacity(n);
        for _ in 0..passes {
            for s in seen.iter_mut() {
                *s = 0;
            }
            stack.clear();
            let mut filled: u32 = 0;
            // The first open cell at or after the centre -- see the Go mirror:
            // the centre itself is a wall on this maze, and starting there made
            // every language agree on a checksum of zero.
            let mut start = (side / 2) * side + side / 2;
            while (start as usize) < n && grid[start as usize] != 0 {
                start += 1;
            }
            if (start as usize) < n {
                stack.push(start);
                seen[start as usize] = 1;
            }
            while let Some(cur) = stack.pop() {
                filled += 1;
                let (cx, cy) = (cur % side, cur / side);
                if cx > 0 {
                    let m = cur - 1;
                    if grid[m as usize] == 0 && seen[m as usize] == 0 {
                        seen[m as usize] = 1;
                        stack.push(m);
                    }
                }
                if cx < side - 1 {
                    let m = cur + 1;
                    if grid[m as usize] == 0 && seen[m as usize] == 0 {
                        seen[m as usize] = 1;
                        stack.push(m);
                    }
                }
                if cy > 0 {
                    let m = cur - side;
                    if grid[m as usize] == 0 && seen[m as usize] == 0 {
                        seen[m as usize] = 1;
                        stack.push(m);
                    }
                }
                if cy < side - 1 {
                    let m = cur + side;
                    if grid[m as usize] == 0 && seen[m as usize] == 0 {
                        seen[m as usize] = 1;
                        stack.push(m);
                    }
                }
            }
            acc = acc.wrapping_mul(31).wrapping_add(filled);
        }
        acc
    }
}

#[no_mangle]
pub extern "C" fn real_names(n: i32) -> u32 {
    let digits = b"0123456789";
    let mut acc: u32 = 0;
    for i in 0..n {
        let mut buf: Vec<u8> = Vec::with_capacity(24);
        buf.extend_from_slice(b"iron-plate-");
        let mut v = i;
        if v == 0 {
            buf.push(b'0');
        } else {
            let mut tmp = [0u8; 10];
            let mut k = 0;
            while v > 0 {
                tmp[k] = digits[(v % 10) as usize];
                v /= 10;
                k += 1;
            }
            while k > 0 {
                k -= 1;
                buf.push(tmp[k]);
            }
        }
        let mut h: u32 = 2166136261;
        for c in buf.iter() {
            h ^= *c as u32;
            h = h.wrapping_mul(16777619);
        }
        acc = acc.wrapping_mul(31).wrapping_add(h);
    }
    acc
}
