//! The data-stage value model's BYTE CONTRACT, tested natively.
//!
//! These run on the host, which is why the crate's `fk` dependency is
//! target-gated: see `Cargo.toml`. What is here is the half two hand-written
//! implementations get wrong and the half that does not touch the wire. The
//! wire is guest-only by construction (it addresses linear memory with u32),
//! and it is pinned end to end instead, by `internal/guest`'s
//! `TestADataStageStringCrossesFkdataByteExact`: both example guests, through
//! the real `fk_data.lua`, under `bin/lua52f`, all 256 byte values.

use fkdata::{arr, bool_, bytes_, num, obj, str_, LuaStr, V};

/// Every byte value survives the type, and the LENGTH survives with it.
///
/// The length is asserted separately from the contents because a lossy
/// conversion gets both wrong and a test checking one of them could have
/// passed: `String::from_utf8_lossy` over these 256 bytes answers 512.
#[test]
fn every_byte_value_survives_the_byte_type() {
    let all: Vec<u8> = (0..=255u8).collect();
    let s = LuaStr::from(&all[..]);
    assert_eq!(s.as_bytes().len(), 256);
    assert_eq!(s.as_bytes(), &all[..]);
    assert_eq!(
        s.to_string_lossy().len(),
        512,
        "the lossy reading, for scale"
    );
    assert!(s.as_str().is_none(), "these bytes are not UTF-8");
}

/// The checked conversion answers for text and refuses for everything else,
/// which is the whole reason it is an Option.
#[test]
fn the_text_conversion_is_checked() {
    assert_eq!(
        LuaStr::from("transport-belt").as_str(),
        Some("transport-belt")
    );
    // A lone 0x80 is a continuation byte with nothing in front of it.
    assert_eq!(LuaStr::from(&b"\x80"[..]).as_str(), None);
    // NUL is valid UTF-8 and is not a terminator: a Lua string is counted.
    let nul = LuaStr::from(&b"a\x00b"[..]);
    assert_eq!(nul.as_bytes().len(), 3);
    assert_eq!(nul.as_str(), Some("a\u{0}b"));
}

/// `V::bytes` never fails and `V::string` fails honestly, and neither of them
/// answers for a value that is not a string.
#[test]
fn the_two_string_accessors_split_the_question() {
    let raw = bytes_(&[0xff, 0x00, 0xfe]);
    assert_eq!(raw.bytes(), &[0xff, 0x00, 0xfe]);
    assert_eq!(raw.string(), None);

    let text = str_("item");
    assert_eq!(text.bytes(), b"item");
    assert_eq!(text.string(), Some("item"));

    // Not a string: no bytes and no text, rather than a plausible answer.
    for v in [V::Nil, num(3.0), bool_(true), arr(&[num(1.0)])] {
        assert_eq!(v.bytes(), b"", "{v:?}");
        assert_eq!(v.string(), None, "{v:?}");
    }
}

/// `str_` and `bytes_` build the same value for text, so the convenient
/// constructor is not a different one.
#[test]
fn the_two_constructors_agree_on_text() {
    assert_eq!(str_("belt"), bytes_(b"belt"));
    assert_eq!(str_(""), bytes_(b""));
}

/// A map lookup compares BYTES, so a key the engine wrote is findable whatever
/// is in it.
#[test]
fn a_map_lookup_compares_bytes() {
    let m = V::Map(alloc_pairs());
    assert_eq!(m.at("name").and_then(V::string), Some("iron-plate"));
    assert_eq!(m.at("blob").map(V::bytes), Some(&[0xffu8, 0x00, 0xfe][..]));
    assert_eq!(m.at("absent"), None);
    // A key is a string key or it is not a match: a number 1 is not "1".
    assert_eq!(m.len(), 3);
    assert!(!m.is_empty());
}

fn alloc_pairs() -> Vec<(V, V)> {
    vec![
        (str_("name"), str_("iron-plate")),
        (str_("blob"), bytes_(&[0xff, 0x00, 0xfe])),
        (num(1.0), str_("a numeric key")),
    ]
}

/// A key the ENGINE wrote is SPELLABLE, which is the half a `&str` parameter
/// took away without saying so: the comparison inside `at` was already over
/// bytes, and no caller could hand it a key that was not UTF-8, so the byte
/// comparison could not be reached and the doc comment claiming it could not be
/// falsified. `K: AsRef<[u8]>` is what makes the claim testable, and this is the
/// test.
#[test]
fn a_lookup_finds_a_key_that_is_not_utf8() {
    let m = V::Map(vec![
        (bytes_(&[0xff, 0xfe]), str_("what the engine wrote")),
        (str_("name"), str_("iron-plate")),
    ]);
    assert_eq!(
        m.at(&[0xffu8, 0xfe][..]).and_then(V::string),
        Some("what the engine wrote")
    );
    // Text still reads the way it looks, from a &str and from a String.
    assert_eq!(m.at("name").and_then(V::string), Some("iron-plate"));
    assert_eq!(
        m.at(&String::from("name")).and_then(V::string),
        Some("iron-plate")
    );
    // And the LOSSY reading of those two bytes is not the key: a lookup that
    // went through the text accessor would find neither this nor the bytes.
    assert_eq!(m.at("\u{fffd}\u{fffd}"), None);
}

/// `obj` builds string keys and the lookup finds them, which is the shape every
/// prototype crosses in.
#[test]
fn obj_builds_string_keys() {
    let p = obj(&[("type", str_("item")), ("stack_size", num(50.0))]);
    assert_eq!(p.at("type").and_then(V::string), Some("item"));
    assert_eq!(p.at("stack_size").map(V::number), Some(50.0));
}

/// The byte type's ORDER is byte-lexicographic, which is what the sort on the
/// way out depends on: `fk_data.lua` sorts a dictionary's keys, the Go twin
/// compares Go strings, and all three have to be the same order or the wire
/// order is a fact about which language built it.
#[test]
fn the_order_is_byte_lexicographic() {
    let mut v = vec![
        LuaStr::from(&b"\xff"[..]),
        LuaStr::from("b"),
        LuaStr::from("a"),
        LuaStr::from("ab"),
        LuaStr::from(&b"\x00"[..]),
    ];
    v.sort();
    let got: Vec<&[u8]> = v.iter().map(LuaStr::as_bytes).collect();
    assert_eq!(got, vec![&b"\x00"[..], b"a", b"ab", b"b", b"\xff"]);
}

/// Borrow<[u8]>, which is what lets a keyed container be looked up without
/// building a LuaStr first. Borrow and not Borrow<str>: a LuaStr holding bytes
/// that are not UTF-8 has no &str to hand back.
#[test]
fn a_keyed_container_is_looked_up_by_bytes() {
    use std::collections::BTreeMap;
    let mut m: BTreeMap<LuaStr, u32> = BTreeMap::new();
    m.insert(LuaStr::from("colour"), 1);
    m.insert(LuaStr::from(&b"\xff\xfe"[..]), 2);
    assert_eq!(m.get("colour".as_bytes()), Some(&1));
    assert_eq!(m.get(&b"\xff\xfe"[..]), Some(&2));
}

/// `set` refills in place, which is what a guest sending the same shape twice
/// uses instead of allocating a value per call.
#[test]
fn set_refills_without_changing_the_contract() {
    let mut s = LuaStr::from("short");
    s.set(&[0x00, 0xff]);
    assert_eq!(s.as_bytes(), &[0x00, 0xff]);
    s.clear();
    assert!(s.as_bytes().is_empty());
}

/// Equality against text is by bytes, so `k == "name"` reads the way it looks
/// and answers false for anything else.
#[test]
fn equality_against_text_is_by_bytes() {
    let s = LuaStr::from("name");
    assert!(s == "name");
    assert!(s == *"name");
    assert!(s != "names");
    assert!(LuaStr::from(&b"\xff"[..]) != "\u{fffd}");
}
