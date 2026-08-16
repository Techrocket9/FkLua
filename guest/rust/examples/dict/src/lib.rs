//! The Rust half of the EVENT PAYLOAD STRUCT and the DICTIONARY FIELD inside
//! one, end to end -- and, since 2026-08-06, of A BINARY STRING through a
//! generated reader.
//!
//! A line-for-line mirror of `guest/go/examples/dict`, so the SAME host stub
//! drives both and their output is compared -- a runtime exercise of the
//! generated readers and a differential check at once. It exists because until
//! 2026-08-03 the Rust generator emitted NO event payload structs at all: every
//! Rust guest in the ports campaign read `fk_on_event`'s pointer at
//! hand-derived byte offsets, twelve to seventeen of them per mod, plus a
//! script that re-derived each one from the GO bindings because nothing else
//! could check them.
//!
//! `on_built_entity` is the first event, and it is not an arbitrary choice:
//! `tags` is the dictionary field that deferred it, and it plus
//! `on_robot_built_entity` are what a mod that builds things subscribes to.
//!
//! `on_console_chat` is the second, and it is here for its `message` -- a plain
//! mandatory STRING field on an event payload, which is the shape that carried
//! the backend asymmetry this file now pins. The Rust reader was
//! `String::from_utf8_lossy(..).into_owned()` where Go's was `string(b)`, so a
//! payload with a byte outside UTF-8 arrived mangled AND the wrong length, in
//! one language only. Both readers hand back bytes now; the two guests printing
//! the same hex is what says so.
//!
//! The compile gate cannot reach any of this. rustc removes every member a
//! guest does not call, so it proves the decoder type-checks and stops there; a
//! pair stride off by the key's padding, a value read from the key's offset, or
//! a string reader that rewrites what it reads all live past the type checker
//! and are only visible when the values come back.

#![no_std]

extern crate alloc;

use alloc::collections::BTreeMap;
use alloc::format;
use alloc::string::String;

use fkapi::{
    read_on_built_entity, read_on_console_chat, LuaEntity, LuaStr, Value, EVENT_ON_BUILT_ENTITY,
    EVENT_ON_CONSOLE_CHAT, HELPERS,
};

/// Where a Rust guest's subscriptions go -- see `examples/api` for why this is
/// `_initialize` rather than `fk_on_init`.
#[no_mangle]
pub extern "C" fn _initialize() {
    fkapi::subscribe(EVENT_ON_BUILT_ENTITY);
    fkapi::subscribe(EVENT_ON_CONSOLE_CHAT);
}

/// Looks a tag up by key rather than printing the map in order.
///
/// The host builds the tags table with `pairs`, whose order this ABI
/// explicitly does not promise, so a test that printed the pairs would be
/// asserting on something nobody owes it. What IS owed is that every pair
/// arrives intact and that the count is right.
///
/// `get` takes BYTES because the key is a [`LuaStr`]: `Borrow<str>` cannot
/// exist for a byte string, so the lookup borrows the bytes instead.
fn tag(tags: &BTreeMap<LuaStr, Value>, k: &str) -> String {
    match tags.get(k.as_bytes()) {
        Some(Value::Str(s)) => format!("'{}'", s),
        Some(Value::Number(n)) => format!("{}", n),
        Some(Value::Bool(b)) => format!("{}", b),
        Some(_) => String::from("?"),
        None => String::from("MISSING"),
    }
}

/// A tag read as BYTES rather than as text -- the tier-2 half of the same
/// claim, since a `tags` value crosses as a tier-2 `Value::Str` while the
/// `message` below crosses as a struct field.
fn tag_hex(tags: &BTreeMap<LuaStr, Value>, k: &str) -> String {
    match tags.get(k.as_bytes()) {
        Some(Value::Str(s)) => format!("{}:{}", s.len(), hex(s.as_bytes())),
        Some(_) => String::from("?"),
        None => String::from("MISSING"),
    }
}

/// Lower-case hex, hand-rolled so the guest pulls in nothing for it.
fn hex(b: &[u8]) -> String {
    const D: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(b.len() * 2);
    for c in b {
        s.push(D[(c >> 4) as usize] as char);
        s.push(D[(c & 15) as usize] as char);
    }
    s
}

#[no_mangle]
pub extern "C" fn fk_on_event(id: u32, ptr: u32) {
    if id == EVENT_ON_CONSOLE_CHAT {
        // A MANDATORY STRING FIELD, printed as bytes and as a length. Both
        // halves matter: the lossy reader changed the length as well as the
        // contents, so a test asserting only one of them could have passed.
        let e = read_on_console_chat(ptr);
        fk::log(&format!(
            "chat: {}:{}",
            e.message.len(),
            hex(e.message.as_bytes())
        ));
        // ...AND STRAIGHT BACK OUT, which is the other direction of the same
        // claim: the host prints what it received, so the assertion is on
        // bytes that made a full round trip through both marshalling
        // directions. The value is taken BY REFERENCE, so nothing is copied
        // into the call.
        let echo = Value::Str(e.message);
        if let Err(err) = HELPERS.write_file("echo.bin", &echo, Some(false), None) {
            fk::log(&format!("write_file failed: {}", err.as_str()));
        }
        return;
    }
    let e = read_on_built_entity(ptr);
    fk::log(&format!(
        "tags: {} colour={} count={} live={} blob={}",
        e.tags.len(),
        tag(&e.tags, "colour"),
        tag(&e.tags, "count"),
        tag(&e.tags, "live"),
        tag_hex(&e.tags, "blob")
    ));
    // The scalar fields AFTER the dictionary in the layout. A dict field whose
    // (ptr, count) header were the wrong width would leave these reading
    // somebody else's bytes, and they are the half a guest actually acts on.
    fk::log(&format!("player={} tick={}", e.player_index, e.tick));
    // And a handle from BEFORE it still resolves, which is what says the fields
    // ahead of the dictionary did not move either.
    match LuaEntity(e.entity).name() {
        Ok(n) => fk::log(&format!("entity={}", n)),
        Err(err) => fk::log(&format!("entity: {}", err.as_str())),
    }
}
