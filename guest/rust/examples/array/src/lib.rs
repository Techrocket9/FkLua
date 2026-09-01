//! The Rust half of the marshalling round trip.
//!
//! A line-for-line mirror of `guest/go/examples/array`, and it exists for the
//! same reason: the compile gate type-checks every bound member
//! (`rust_members_bound` in census.json) and cannot see a single wrong offset.
//! Only running one and comparing the value can.
//!
//! Because it mirrors the Go example, the SAME host stub drives both and their
//! output is compared -- so this is a runtime exercise and a differential check
//! at once.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;
use alloc::vec;
use alloc::vec::Vec;

use fkapi::{LuaControl, LuaEntity, LuaEntityPrototype, LuaStr, LuaSurface, Object, Value, GAME};

#[no_mangle]
pub extern "C" fn fk_on_init() {
    // 1. An array of HANDLES coming back.
    let players = match GAME.connected_players() {
        Ok(p) => p,
        Err(e) => {
            fk::log(&format!("connected_players failed: {}", e.as_str()));
            return;
        }
    };
    fk::log(&format!("handles: {}", players.len()));
    if players.is_empty() {
        fk::log("no players, so the element loop never ran -- the rest would prove nothing");
        return;
    }

    // A handle is a handle: the class a guest wraps it in decides which members
    // it can reach, and the host looks the member up by id either way.
    let first = players[0];

    // 2. An array of STRINGS: (ptr, len) pairs the host wrote through fk_alloc.
    match LuaEntityPrototype(first).accepted_seeds() {
        Ok(seeds) => {
            let seeds = seeds.unwrap_or_default();
            let mut out = format!("strings: {}", seeds.len());
            for s in &seeds {
                out.push(' ');
                // to_string_lossy, not as_str: a prototype name is ASCII by
                // engine constraint, and this is a log line. The CHECKED
                // conversion is one call away where it matters.
                out.push_str(&s.to_string_lossy());
            }
            fk::log(&out);
        }
        Err(e) => fk::log(&format!("accepted_seeds failed: {}", e.as_str())),
    }

    // 2b. THE SAME OPTIONAL ARRAY, ABSENT. `accepted_seeds` is declared
    //     optional, so "no value" and "the empty list" are different answers and
    //     the API means them differently -- reported by fklua-ports'
    //     fuel-train-stop (FTS1) against both backends. Rust says it with
    //     Option, because a Vec has no absent value; Go says it with nil,
    //     because make([]T, 0) is guaranteed NON-nil. Same host, same stub,
    //     same log line, two conventions.
    match LuaEntity(first).last_user() {
        Err(e) => fk::log(&format!("last_user failed: {}", e.as_str())),
        Ok(None) => fk::log("optional array: no last_user"),
        Ok(Some(bare)) => match LuaEntityPrototype(bare).accepted_seeds() {
            Err(e) => fk::log(&format!("absent accepted_seeds failed: {}", e.as_str())),
            Ok(None) => fk::log("optional array: absent"),
            Ok(Some(v)) => fk::log(&format!("optional array: present {}", v.len())),
        },
    }

    // 3. An array of STRUCTS, whose stride is the element's PADDED size.
    match LuaEntity(first).autopilot_destinations() {
        Ok(dests) => {
            let mut out = format!("structs: {}", dests.len());
            for p in &dests {
                out.push_str(&format!(" ({:.1},{:.1})", p.x, p.y));
            }
            fk::log(&out);
        }
        Err(e) => fk::log(&format!("autopilot_destinations failed: {}", e.as_str())),
    }

    // 3b. THE SAME STRUCT SHAPE ARRIVING AS AN ARRAY. `Vector` is declared
    //     `table{x,y} | tuple[float,float]` and the description says outright
    //     which one the engine sends: "The game will always provide the array
    //     format". The MapPositions above are the KEYED form of the same shape,
    //     so the two lines together say the host reads either -- and until the
    //     descriptor carried pos=, this one printed (0.00,0.00) with status OK
    //     and nothing logged, which is what WormholeBelts measured off
    //     inserter_drop_position (item 8).
    match LuaEntityPrototype(first).inserter_drop_position() {
        Err(e) => fk::log(&format!("inserter_drop_position failed: {}", e.as_str())),
        Ok(None) => fk::log("shorthand struct: absent"),
        Ok(Some(d)) => fk::log(&format!("shorthand struct: ({:.2},{:.2})", d.x, d.y)),
    }

    // 4. A DICTIONARY -- the same walk over key/value pairs. Looked up by name
    //    rather than iterated, matching the Go side, which cannot iterate a map
    //    in a defined order. (A BTreeMap could, which is the difference the
    //    binding buys; the comparison is what forces the restraint here.)
    match LuaEntity(first).get_fluid_contents() {
        Ok(f) => fk::log(&format!(
            "dict: {} water={:.1} steam={:.1}",
            f.len(),
            // Keyed by LuaStr, so the lookup borrows BYTES -- Borrow<str>
            // cannot exist for a byte string.
            f.get("water".as_bytes()).copied().unwrap_or(0.0),
            f.get("steam".as_bytes()).copied().unwrap_or(0.0)
        )),
        Err(e) => fk::log(&format!("get_fluid_contents failed: {}", e.as_str())),
    }

    // 5. An array going OUT, the direction that allocates.
    if let Err(e) = LuaControl(first)
        .set_character_additional_mining_categories(&["basic-solid", "hard-solid"])
    {
        fk::log(&format!("set mining categories failed: {}", e.as_str()));
    }

    // 6. Empty is not absent. The host must see ptr=0, count=0.
    if let Err(e) = LuaControl(first).set_character_additional_mining_categories(&[]) {
        fk::log(&format!("set empty failed: {}", e.as_str()));
    }

    // 7. TIER 2 coming back, as an enum rather than a tagged struct.
    match LuaEntity(first).ghost_localised_name() {
        Ok(v) => fk::log(&format!("dyn in: {}", render(&v))),
        Err(e) => fk::log(&format!("ghost_localised_name failed: {}", e.as_str())),
    }

    // 8. And going out, nested. The trailing Nil shows what it costs: a nil
    //    inside a Lua sequence IS the end of that sequence, so the host receives
    //    [true] and there is nothing tier 2 could do about it.
    if let Err(e) = LuaControl(first).set_cursor_ghost(&Value::Array(vec![
        Value::Str(LuaStr::from("item-name.iron-plate")),
        Value::Number(42.0),
        Value::Array(vec![Value::Bool(true), Value::Nil]),
    ])) {
        fk::log(&format!("set_cursor_ghost failed: {}", e.as_str()));
    }

    // 9. Array fields INSIDE a struct, at two different offsets.
    match LuaEntity(first).belt_neighbours() {
        Ok(b) => fk::log(&format!(
            "struct arrays: inputs={} outputs={}",
            b.inputs.len(),
            b.outputs.len()
        )),
        Err(e) => fk::log(&format!("belt_neighbours failed: {}", e.as_str())),
    }

    // 10. A VARIANT PARAMETER GROUP method: the argument table is a
    //     discriminated union, so it crosses as one tier-2 value.
    match LuaSurface(first).create_entity(&Value::Map(vec![
        (Value::Str(LuaStr::from("name")), Value::Str(LuaStr::from("iron-chest"))),
        (Value::Str(LuaStr::from("force")), Value::Str(LuaStr::from("player"))),
        (Value::Str(LuaStr::from("bar")), Value::Number(4.0)),
    ])) {
        Ok(e) => fk::log(&format!(
            "create_entity returned {}",
            if e.is_some() { "an entity" } else { "nothing" }
        )),
        Err(e) => fk::log(&format!("create_entity failed: {}", e.as_str())),
    }

    // 11. THE DESTINATION-VECTOR VARIANT of the same array returns. Same member
    //     ids, same blocks, same host calls -- only where the container comes
    //     from differs, so what is checked is that the CONTENTS match the
    //     allocating form and that a Vec with room does not reallocate.
    //
    //     THE EQUALITY CHECK IS ON STRINGS AND NOT ON HANDLES, for the reason
    //     the Go mirror states: an array of objects comes back as fresh
    //     TRANSIENT handles, so two calls return different numbers for the same
    //     three players. Handles carry the reuse half, strings the equality one.
    //
    //     The Rust signature is `&mut Vec<T>` where Go's is `dst []T` returned:
    //     Rust has an out-parameter and Go does not, so the two mirrors differ
    //     HERE and nowhere else, and their log lines are identical anyway.
    let mut sdst: Vec<LuaStr> = Vec::with_capacity(4);
    match LuaEntityPrototype(first).accepted_seeds_into(&mut sdst) {
        Ok(_present) => {
            let mut out = format!("into strings: {}", sdst.len());
            for s in sdst.iter() {
                out.push(' ');
                out.push_str(&s.to_string_lossy());
            }
            fk::log(&out);
        }
        Err(e) => fk::log(&format!("accepted_seeds_into failed: {}", e.as_str())),
    }

    let mut dst: Vec<Object> = Vec::with_capacity(8);
    if let Err(e) = GAME.connected_players_into(&mut dst) {
        fk::log(&format!("connected_players_into failed: {}", e.as_str()));
        return;
    }
    let base = dst.as_ptr() as usize;
    let mut stable = true;
    for _ in 0..3 {
        if GAME.connected_players_into(&mut dst).is_err() || dst.as_ptr() as usize != base {
            stable = false;
        }
    }
    fk::log(&format!(
        "into: {} same-buffer={}",
        dst.len(),
        if stable { "yes" } else { "no" }
    ));

    // ...and a destination that cannot hold the answer, which has to grow. The
    // line says only the count, because that is the strongest thing BOTH
    // mirrors can truthfully say: Rust's `&mut Vec` grows the caller's own
    // vector in place, while Go's `dst []T` cannot and returns a new slice
    // leaving the caller's untouched. Asserting either language's version of
    // "who owns the new buffer" would make the two logs differ, and the whole
    // value of these mirrors is that they do not.
    let mut small: Vec<Object> = Vec::with_capacity(1);
    if let Err(e) = GAME.connected_players_into(&mut small) {
        fk::log(&format!("connected_players_into (small) failed: {}", e.as_str()));
        return;
    }
    fk::log(&format!("into grown: {}", small.len()));
}

/// Prints a dynamic value in the same notation the Go example uses, so the two
/// can be compared directly.
fn render(v: &Value) -> String {
    match v {
        Value::Nil => String::from("nil"),
        Value::Bool(b) => String::from(if *b { "true" } else { "false" }),
        Value::Number(n) => {
            // Matches Go's FormatFloat(-1): integral values print without a
            // fractional part.
            if *n == libm_trunc(*n) {
                format!("{}", *n as i64)
            } else {
                format!("{}", n)
            }
        }
        Value::Str(s) => format!("'{}'", s),
        Value::Obj(_) => String::from("obj"),
        Value::Array(items) => {
            let mut out = String::from("[");
            for (i, e) in items.iter().enumerate() {
                if i > 0 {
                    out.push(' ');
                }
                out.push_str(&render(e));
            }
            out.push(']');
            out
        }
        Value::Map(pairs) => {
            let mut out = String::from("{");
            for (i, (k, val)) in pairs.iter().enumerate() {
                if i > 0 {
                    out.push(' ');
                }
                out.push_str(&format!("{}={}", render(k), render(val)));
            }
            out.push('}');
            out
        }
    }
}

// core has no trunc for f64 without libm, and this needs only the integral
// check, so the comparison is done directly.
fn libm_trunc(x: f64) -> f64 {
    (x as i64) as f64
}
