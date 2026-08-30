//! The Rust half of READING a tier-2 value, end to end.
//!
//! A line-for-line mirror of `guest/go/examples/dynread`, so the SAME host stub
//! drives both and their output is compared. It exists for the reason every
//! mirror here exists: the accessors are hand-written in a generated preamble,
//! `census.json` cannot see a preamble, and the one thing this repo has already
//! proved is that a single-language round drifts.
//!
//! The two languages spell the two families differently on purpose -- Go's
//! comma-ok is Rust's `Option`, and Go's `As-` prefix exists only to dodge its
//! own struct fields -- so what is compared is the ANSWERS rather than the
//! spelling. Every line below is order-independent for the reason the Go file
//! gives.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;

use fkapi::{LuaEntity, Object, Value, HELPERS};

fn num(f: f64) -> String {
    // Matches Go's strconv.FormatFloat(f, 'f', -1, 64) for the values here:
    // an integral f64 prints without a fractional part in both.
    if f == libm_trunc(f) && f.abs() < 1e15 {
        format!("{}", f as i64)
    } else {
        format!("{}", f)
    }
}

fn libm_trunc(f: f64) -> f64 {
    (f as i64) as f64
}

fn ok(b: bool) -> &'static str {
    if b {
        "ok"
    } else {
        "no"
    }
}

fn str_or(v: &Value, def: &str) -> String {
    match core::str::from_utf8(v.str_or(def.as_bytes())) {
        Ok(s) => String::from(s),
        Err(_) => String::from("?"),
    }
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    let t = match HELPERS.json_to_table("{}") {
        Err(e) => {
            fk::log(&format!("dynread: {}", e.as_str()));
            return;
        }
        Ok(None) => {
            fk::log("dynread: absent");
            return;
        }
        Ok(Some(v)) => v,
    };

    // A LOOKUP CHAINS BECAUSE ITS MISS IS NIL.
    fk::log(&format!(
        "get: name={} count={} on={}",
        str_or(t.get("name"), "?"),
        num(t.get("count").num_or(-1.0)),
        t.get("on").bool_or(false)
    ));
    fk::log(&format!(
        "miss: str={} num={} nil={}",
        str_or(t.get("nope"), "<none>"),
        num(t.get("nope").num_or(-1.0)),
        t.get("nope").is_nil()
    ));

    // HAS IS THE QUESTION GET CANNOT ANSWER.
    fk::log(&format!(
        "has: name={} nope={} onascalar={}",
        t.has("name"),
        t.has("nope"),
        t.get("count").has("name")
    ));

    // CHAINED THROUGH TWO LEVELS, and through a level that is not there.
    fk::log(&format!(
        "deep: hit={} miss={} via-scalar={}",
        num(t.get("inner").get("deep").num_or(-1.0)),
        num(t.get("inner").get("gone").num_or(-1.0)),
        num(t.get("count").get("deep").num_or(-1.0))
    ));

    // AN ARRAY, ZERO-BASED.
    let a = t.get("list");
    fk::log(&format!(
        "at: 0={} 2={} 9={} neg={} map={} map-nil={}",
        str_or(a.at(0), "?"),
        str_or(a.at(2), "?"),
        str_or(a.at(9), "<oob>"),
        // Rust's at takes a usize, so a negative index is not expressible --
        // the Go line's neg case is unrepresentable here and prints the same
        // answer for the same reason: there is nothing at that position.
        str_or(a.at(usize::MAX), "<oob>"),
        str_or(t.at(0), "<notarray>"),
        // See the Go file: the map case is asserted through is_nil as well,
        // because a default of "<notarray>" hides an at that walked the pair
        // slice whenever the first pair's value is not a string.
        t.at(0).is_nil()
    ));

    // LEN IS THE ONE ACCESSOR WITH AN ANSWER FOR A SCALAR.
    fk::log(&format!(
        "len: map={} arr={} scalar={} nil={}",
        t.len(),
        a.len(),
        t.get("count").len(),
        t.get("nope").len()
    ));

    // NOTHING COERCES.
    let n = t.get("name").as_num();
    let s = t.get("count").as_str();
    let b = t.get("name").as_bool();
    fk::log(&format!(
        "as: num-of-str={}/{} str-of-num='{}'/{} bool-of-str={}/{}",
        num(n.unwrap_or(0.0)),
        ok(n.is_some()),
        s.map(|x| str_of(x)).unwrap_or_default(),
        ok(s.is_some()),
        b.unwrap_or(false),
        ok(b.is_some())
    ));

    // ...and each read through the RIGHT tag, which is the control.
    let n2 = t.get("count").as_num();
    let s2 = t.get("name").as_str();
    let b2 = t.get("on").as_bool();
    fk::log(&format!(
        "as: num={}/{} str='{}'/{} bool={}/{}",
        num(n2.unwrap_or(0.0)),
        ok(n2.is_some()),
        s2.map(|x| str_of(x)).unwrap_or_default(),
        ok(s2.is_some()),
        b2.unwrap_or(false),
        ok(b2.is_some())
    ));

    // A NUMBER KEY, which get cannot spell.
    fk::log(&format!(
        "key: n7={} s7={} n8={}",
        str_or(t.get_key(&Value::Number(7.0)), "?"),
        str_or(t.get_key(&Value::Str("7".into())), "<none>"),
        str_or(t.get_key(&Value::Number(8.0)), "<none>")
    ));

    // A HANDLE THROUGH A TIER-2 VALUE, and it still resolves.
    let h = match t.get("obj").as_obj() {
        Some(h) => h,
        None => {
            fk::log("obj: not an object");
            return;
        }
    };
    match LuaEntity(h).name() {
        Err(e) => fk::log(&format!("obj: {}", e.as_str())),
        Ok(name) => fk::log(&format!(
            "obj: {} zero={}",
            str_of(&name),
            t.get("nope").obj_or(Object(0)).0
        )),
    }
}

fn str_of(s: &fkapi::LuaStr) -> String {
    match core::str::from_utf8(s.as_bytes()) {
        Ok(v) => String::from(v),
        Err(_) => String::from("?"),
    }
}
