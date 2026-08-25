//! The Rust mirror of `guest/go/examples/lasterror`, line for line.
//!
//! `fk::last_error` carries WHAT THE ENGINE SAID across the boundary beside a
//! status. A host call returns an `i32` and never raises into wasm, so a binding
//! that fails can tell a guest the KIND of failure and nothing else; the
//! engine's own sentence was recorded by `fk_abi.lua` and reachable from this
//! repo's own tests and from nowhere a mod could stand.
//!
//! Two guests agreeing line for line against one host stub is what says one
//! analysis has two renderings rather than two implementations -- the same
//! argument `examples/dict` makes for the generated event readers, and it
//! matters more here because the two wrappers are hand-written and DIFFER: Go
//! uses a package-level scratch because TinyGo's ptrtoint defeats stack
//! promotion, Rust uses a stack array because rustc does not.
//!
//! The bytes come back as a `Vec<u8>` rather than a `String`, which is the
//! LuaStr lesson stated in the one place a guest touches raw engine text: a Lua
//! string is arbitrary bytes and `from_utf8_lossy` rewrites them, silently, and
//! changes the length while it does it.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;

use fkapi::GAME;

/// Renders the message the way this fixture asserts it: as bytes, through a
/// lossy conversion done HERE and named, rather than inside the wrapper.
fn shown(b: &[u8]) -> String {
    String::from_utf8_lossy(b).into_owned()
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    // NOTHING HAS FAILED YET.
    fk::log(&format!("empty: [{}]", shown(&fk::last_error())));

    // A CALL THAT RAISES. game.tick is an ordinary attribute read; the host
    // stub's __index raises for that key, which is what Factorio's own does for
    // some accesses -- the reason the member read is inside the pcall at all.
    match GAME.tick() {
        Ok(_) => fk::log("raised: game.tick did not fail, so this fixture proves nothing"),
        Err(e) => {
            // THE STATUS AS A NUMBER, not as its language's sentence. Rust's
            // Status::as_str has no prefix and Go's error convention adds
            // "fklua: ", and this fixture is two renderings held to ONE set of
            // expectations -- so what it prints is the ABI's own code, which is
            // 5, ERR_CALL_FAILED. The status is asserted BESIDE the message
            // because a message with the wrong status next to it would be a
            // guest reading a stale slot.
            let m = fk::last_error();
            fk::log(&format!(
                "raised: st={} len={} msg=[{}]",
                e.0,
                m.len(),
                shown(&m)
            ));
        }
    }

    // A CALL THAT SUCCEEDS CLEARS IT. The slot is cleared by M.call on the way
    // IN, so an OK call leaves it empty rather than leaving the previous failure
    // standing.
    match GAME.speed() {
        Ok(_) => fk::log(&format!("after-ok: [{}]", shown(&fk::last_error()))),
        Err(e) => fk::log(&format!("after-ok: game.speed failed: {}", e.as_str())),
    }

    // TRUNCATION. The stub raises a message longer than the wrapper's 256-byte
    // buffer, so the first host call reports a length that did not fit and the
    // wrapper asks again with room. A wrapper that trusted its own buffer would
    // report 256 and a message ending mid-word.
    match GAME.ticks_played() {
        Ok(_) => fk::log("long: game.ticks_played did not fail"),
        Err(_) => {
            let m = fk::last_error();
            let (head, tail) = if m.len() >= 8 {
                (shown(&m[..8]), shown(&m[m.len() - 8..]))
            } else {
                (String::new(), String::new())
            };
            fk::log(&format!("long: len={} head={} tail={}", m.len(), head, tail));
        }
    }
}
