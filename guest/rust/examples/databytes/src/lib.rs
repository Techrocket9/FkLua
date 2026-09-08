//! The data stage's BYTE-EXACTNESS fixture: a Rust program that reads two kinds
//! of string out of the stage, logs each one's length and bytes, and writes both
//! back through the two write primitives.
//!
//! It exists because a data-stage string is an arbitrary byte sequence and the
//! two guest libraries have to agree about that to the byte. The engine carries
//! invalid UTF-8 and control characters through `mod-settings.dat` unchanged,
//! so a stored setting value is where a guest meets one for real; a prototype
//! field another mod wrote is the other place, and a prototype NAME another mod
//! wrote is the third. (A NUL truncates the stored value inside the engine,
//! which is the engine's behaviour and not this ABI's; the NUL here is driving
//! `fk_data.lua`, which is the ABI.) This half used to
//! decode both with `String::from_utf8_lossy`, which rewrote every byte outside
//! UTF-8 into U+FFFD and changed the length while it did it, so the two halves
//! saw different strings for one wire. Nothing in this repository's corpus could
//! see that: every string in the `datastage` example is a prototype name, which
//! is ASCII by engine constraint.
//!
//! IT IS A LINE-FOR-LINE MIRROR of `guest/go/examples/databytes`, and one test
//! drives both through one `fk_data.lua` against one stand-in and requires
//! identical transcripts out of each.
//!
//! Each value is logged as `<len>:<hex>`, because a lossy reader gets the LENGTH
//! wrong as well as the contents and a check that asserted only one of them
//! could have passed.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p databytes
//! ```

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;
use fkdata::{bytes_, str_, P, V};

/// The mod's own prefix, so nothing here can collide with a real mod.
const PREFIX: &str = "fkb-";

/// The prototype this reads its fields out of and writes its echoes back into.
const PROBE_TYPE: &str = "fkb-probe";
const PROBE_NAME: &str = "fkb-source";

/// The prototype type this ENUMERATES. Its prototype names hold bytes outside
/// UTF-8, because a name is a key in a table any mod can write and `keys` is the
/// one surface that hands back a whole set of them.
const KEYED_TYPE: &str = "fkb-keyed";

/// The three shapes a byte string comes in, named so the transcript says which
/// one moved. In sorted order, which is also the order the host hands the
/// settings map back in, so the log reads in one order rather than two.
const PROBES: [&str; 3] = ["all", "empty", "nul"];

#[no_mangle]
pub extern "C" fn fk_data() {
    let mut echo: Vec<(V, V)> = alloc::vec![
        (str_("type"), str_(&format!("{PREFIX}echo"))),
        (str_("name"), str_(&format!("{PREFIX}echo-out"))),
    ];

    for p in PROBES {
        // IN through env(3): a stored startup setting's value, which is the one
        // string at this stage that a player can put arbitrary bytes into.
        match fkdata::startup_setting(&format!("{PREFIX}{p}")) {
            Some(s) => {
                fkdata::log(&format!("databytes: setting {p} {}", hexline(s.bytes())));
                // OUT through set: straight back into data.raw, unaltered. The
                // guest never looks at what it is carrying.
                fkdata::set(
                    &bytes_(s.bytes()),
                    &[
                        P::S(PROBE_TYPE),
                        P::S(PROBE_NAME),
                        P::S(&format!("echo-{p}")),
                    ],
                );
            }
            None => fkdata::log(&format!("databytes: setting {p} ABSENT")),
        }

        // IN through get: a string FIELD of a prototype, which is a different
        // decode path over the same wire.
        match fkdata::get(&[P::S(PROBE_TYPE), P::S(PROBE_NAME), P::S(p)]) {
            Some(f) => {
                fkdata::log(&format!("databytes: field {p} {}", hexline(f.bytes())));
                // OUT through extend, below.
                echo.push((str_(p), bytes_(f.bytes())));
            }
            None => fkdata::log(&format!("databytes: field {p} ABSENT")),
        }
    }

    // IN through keys: the prototype NAMES at a type, sorted. A name is a key
    // in a global namespace any mod can write, so it is bytes like everything
    // else -- and this is the read path a fixture that only ever enumerated
    // ASCII names cannot hold to the contract.
    for k in fkdata::keys(&[P::S(KEYED_TYPE)]) {
        fkdata::log(&format!("databytes: key {}", hexline(k.as_bytes())));
        // And BACK IN as a path element: the name a mod wrote, spelled as
        // the bytes it is, reaching the prototype that carries it. A path
        // that could only be text would be the one string on this surface
        // that does not cross byte-exact.
        match fkdata::get(&[P::S(KEYED_TYPE), P::B(k.as_bytes()), P::S("name")]) {
            Some(v) => fkdata::log(&format!("databytes: roundtrip {}", hexline(v.bytes()))),
            None => fkdata::log("databytes: roundtrip ABSENT"),
        }
    }

    fkdata::extend(&[V::Map(echo)]);
}

/// Renders a byte string as `<len>:<hex>`.
///
/// The bytes come out of [`V::bytes`], which is the accessor that cannot fail:
/// a Lua string need not be UTF-8, so the text accessor answers an Option and
/// this fixture wants neither the text nor the question. The Go twin says the
/// same thing about a `string`.
fn hexline(b: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut out = format!("{}:", b.len());
    for x in b {
        out.push(DIGITS[(x >> 4) as usize] as char);
        out.push(DIGITS[(x & 0x0f) as usize] as char);
    }
    out
}
