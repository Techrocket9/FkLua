//! The Rust half of the LINE BUILDER and the VALUE DUMPER, end to end.
//!
//! A line-for-line mirror of `guest/go/examples/logdump`, so the same host stub
//! drives both and one expectation covers them. The two crates are hand-written
//! twins that nothing generates, and `census.json` cannot see a hand-written
//! guest library at all -- this is the only thing that compares them.

#![no_std]

extern crate alloc;

use alloc::format;

use fkapi::{LuaStr, Value};

#[no_mangle]
pub extern "C" fn fk_on_init() {
    // THE APPENDERS, one line, no allocation.
    fklog::start("nums ");
    fklog::u(0);
    fklog::s(" ");
    fklog::u(42);
    fklog::s(" ");
    fklog::i(-7);
    fklog::s(" ");
    fklog::b(true);
    fklog::s(" ");
    fklog::b(false);
    fklog::end();

    // THE SIGNED EDGE. See the Go file: negating at the minimum overflows.
    fklog::start("edge ");
    fklog::i(i64::MIN);
    fklog::s(" ");
    fklog::u(u64::MAX);
    fklog::end();

    // ONE DECIMAL, INCLUDING THE CARRY.
    fklog::start("f1 ");
    fklog::f1(1.25);
    fklog::s(" ");
    fklog::f1(9.96);
    fklog::s(" ");
    fklog::f1(-0.04);
    fklog::end();

    // TRUNCATION OVER GROWTH.
    fklog::start("");
    for _ in 0..200 {
        fklog::s("0123456789");
    }
    fk::log(&format!("trunc {}", fklog::len()));

    // THE DUMPER, through fklog's own tail.
    let v = Value::Map(alloc::vec![
        (
            Value::Str(LuaStr::from("name")),
            Value::Str(LuaStr::from("belt"))
        ),
        (Value::Str(LuaStr::from("count")), Value::Number(42.0)),
        (Value::Str(LuaStr::from("ratio")), Value::Number(1.5)),
        (Value::Str(LuaStr::from("on")), Value::Bool(true)),
        (Value::Str(LuaStr::from("gone")), Value::Nil),
        (
            Value::Str(LuaStr::from("list")),
            Value::Array(alloc::vec![
                Value::Number(1.0),
                Value::Str(LuaStr::from("two")),
                Value::Bool(false)
            ])
        ),
        (
            Value::Str(LuaStr::from("inner")),
            Value::Map(alloc::vec![(
                Value::Str(LuaStr::from("deep")),
                Value::Number(7.0)
            )])
        ),
        (Value::Number(7.0), Value::Str(LuaStr::from("seven"))),
    ]);
    fklog::start("dump ");
    let k = v.dump(fklog::tail());
    fklog::advance(k);
    fklog::end();

    // A SCALAR AT THE TOP LEVEL, and the empty containers.
    fklog::start("scalars ");
    let k = Value::Number(-0.5).dump(fklog::tail());
    fklog::advance(k);
    fklog::s(" ");
    let k = Value::Nil.dump(fklog::tail());
    fklog::advance(k);
    fklog::s(" ");
    let k = Value::Array(alloc::vec![]).dump(fklog::tail());
    fklog::advance(k);
    fklog::s(" ");
    let k = Value::Map(alloc::vec![]).dump(fklog::tail());
    fklog::advance(k);
    fklog::end();

    // THE DUMPER TRUNCATES TOO.
    let mut small = [0u8; 8];
    let k = Value::Str(LuaStr::from("0123456789")).dump(&mut small);
    fk::log(&format!(
        "dumptrunc {} {}",
        k,
        core::str::from_utf8(&small).unwrap_or("?")
    ));
}
