//! THE COMMITTED WIRE VECTORS, decoded by this codec.
//!
//! `testdata/ipc/wire-vectors.txt` holds frames the GO codec produced. This
//! reads every one, checks that every header field and every control field
//! comes back out of it, and then RE-ENCODES and requires the identical bytes.
//!
//! It is the AD5 mitigation and it is the only cross-language pin that needs no
//! toolchain: `TestBothGuestLibrariesSpeakTheSameWire` is stronger, because it
//! runs both real guests through the real runtime, but it needs TinyGo, cargo
//! and `bin/lua52f` and therefore does not run in CI. This needs `cargo test`.
//!
//! What it cannot catch, stated rather than implied: a change made to BOTH the
//! Go codec and this file in one commit. The golden diff is the review artifact
//! for that, exactly as it is for the emitter's own goldens.

use std::path::PathBuf;

use fkipc::wire::{self, control, Flags, Header, Type};

struct Vector {
    name: String,
    header: Header,
    payload: Vec<u8>,
    frame: Vec<u8>,
    control: String,
    fields: Vec<(String, String)>,
}

fn vectors_path() -> PathBuf {
    // guest/rust/fkipc -> the checkout root.
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/ipc/wire-vectors.txt")
}

fn unhex(s: &str) -> Vec<u8> {
    assert!(s.len() % 2 == 0, "odd hex run {:?}", s);
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).expect("hex"))
        .collect()
}

fn load() -> Vec<Vector> {
    let path = vectors_path();
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("reading {}: {}", path.display(), e));
    let mut out = Vec::new();
    let mut cur: Option<Vector> = None;
    for (n, line) in text.lines().enumerate() {
        let line = line.trim_end();
        if line.starts_with('#') {
            continue;
        }
        if line.is_empty() {
            if let Some(v) = cur.take() {
                out.push(v);
            }
            continue;
        }
        let (key, val) = match line.split_once(' ') {
            Some((k, v)) => (k, v),
            None => (line, ""),
        };
        if key == "name" {
            assert!(
                cur.is_none(),
                "line {}: a record without a blank line before it",
                n + 1
            );
            cur = Some(Vector {
                name: val.to_string(),
                header: Header::default(),
                payload: Vec::new(),
                frame: Vec::new(),
                control: String::new(),
                fields: Vec::new(),
            });
            continue;
        }
        let v = cur
            .as_mut()
            .unwrap_or_else(|| panic!("line {}: {:?} before any name", n + 1, key));
        let num = || -> u64 {
            val.parse()
                .unwrap_or_else(|_| panic!("line {}: {:?}", n + 1, val))
        };
        match key {
            "type" => v.header.ty = Type(num() as u8),
            "flags" => v.header.flags = Flags(num() as u16),
            "channel" => v.header.channel = num() as u16,
            "epoch" => v.header.epoch = num() as u32,
            "seq" => v.header.seq = num() as u32,
            "corr" => v.header.corr = num() as u32,
            "frag" => v.header.frag = num() as u8,
            "nfrag" => v.header.nfrag = num() as u8,
            "payload" => v.payload = unhex(val),
            "frame" => v.frame = unhex(val),
            "control" => v.control = val.to_string(),
            "field" => {
                let (k, fv) = val.split_once(' ').unwrap_or((val, ""));
                v.fields.push((k.to_string(), fv.to_string()));
            }
            other => panic!("line {}: unknown key {:?}", n + 1, other),
        }
    }
    if let Some(v) = cur {
        out.push(v);
    }
    assert!(!out.is_empty(), "{} held no vectors", path.display());
    out
}

/// Every committed frame decodes to the header and payload recorded beside it.
#[test]
fn the_committed_vectors_decode_as_described() {
    for v in load() {
        let (h, p) =
            wire::decode(&v.frame).unwrap_or_else(|e| panic!("{}: {}", v.name, e.as_str()));
        let mut want = v.header;
        want.length = v.payload.len() as u16;
        assert_eq!(h, want, "{}: header", v.name);
        assert_eq!(p, &v.payload[..], "{}: payload", v.name);
    }
}

/// ...and re-encoding produces the identical bytes.
///
/// The decode above would pass on a codec that read a field from the wrong
/// offset as long as it wrote it back to the same wrong one; this is the half
/// that says the OFFSETS are the Go codec's.
#[test]
fn every_committed_vector_re_encodes_byte_for_byte() {
    let mut buf = Vec::new();
    for v in load() {
        buf.clear();
        wire::append_frame(&mut buf, v.header, &v.payload)
            .unwrap_or_else(|e| panic!("{}: {}", v.name, e.as_str()));
        assert_eq!(
            buf,
            v.frame,
            "{}: re-encoded\n  got  {}\n  want {}",
            v.name,
            hexs(&buf),
            hexs(&v.frame)
        );
    }
}

/// The protocol-defined payloads decode to the fields recorded beside them.
///
/// A frame's payload is opaque to the framing layer, so the tests above would
/// pass on a control codec that put `max_frame` where `max_fragments` goes.
#[test]
fn the_committed_control_payloads_decode_as_described() {
    let mut seen = 0;
    for v in load() {
        if v.control.is_empty() {
            continue;
        }
        seen += 1;
        let get = |k: &str| -> String {
            v.fields
                .iter()
                .find(|(fk, _)| fk == k)
                .unwrap_or_else(|| panic!("{}: no field {:?}", v.name, k))
                .1
                .clone()
        };
        let n = |k: &str| -> u64 { get(k).parse().unwrap() };
        match v.control.as_str() {
            "hello" => {
                let h = control::decode_hello(&v.payload).expect(&v.name);
                assert_eq!(h.proto_min as u64, n("proto_min"), "{}", v.name);
                assert_eq!(h.proto_max as u64, n("proto_max"), "{}", v.name);
                assert_eq!(h.max_frame as u64, n("max_frame"), "{}", v.name);
                assert_eq!(h.max_fragments as u64, n("max_fragments"), "{}", v.name);
                assert_eq!(h.boot as u64, n("boot"), "{}", v.name);
                assert_eq!(h.tick as u64, n("tick"), "{}", v.name);
                assert_eq!(h.profile.0 as u64, n("profile"), "{}", v.name);
                assert_eq!(h.name, get("name"), "{}", v.name);
                let mut re = Vec::new();
                control::append_hello(&mut re, &h).unwrap();
                assert_eq!(re, v.payload, "{}: hello re-encode", v.name);
            }
            "heartbeat" => {
                let hb = control::decode_heartbeat(&v.payload).expect(&v.name);
                assert_eq!(hb.tick as u64, n("tick"), "{}", v.name);
                assert_eq!(hb.rx as u64, n("rx"), "{}", v.name);
                assert_eq!(hb.drops as u64, n("drops"), "{}", v.name);
                assert_eq!(hb.gaps as u64, n("gaps"), "{}", v.name);
                let mut re = Vec::new();
                control::append_heartbeat(&mut re, hb);
                assert_eq!(re, v.payload, "{}: heartbeat re-encode", v.name);
            }
            "filenotify" => {
                let f = control::decode_file_notify(&v.payload).expect(&v.name);
                assert_eq!(f.bytes as u64, n("bytes"), "{}", v.name);
                assert_eq!(f.fnv1a32 as u64, n("fnv1a32"), "{}", v.name);
                assert_eq!(f.name, get("name"), "{}", v.name);
                let mut re = Vec::new();
                control::append_file_notify(&mut re, &f).unwrap();
                assert_eq!(re, v.payload, "{}: file-notify re-encode", v.name);
            }
            "error" => {
                let e = control::decode_error_record(&v.payload).expect(&v.name);
                assert_eq!(e.code as u64, n("code"), "{}", v.name);
                assert_eq!(e.message, get("message"), "{}", v.name);
                let mut re = Vec::new();
                control::append_error_record(&mut re, &e).unwrap();
                assert_eq!(re, v.payload, "{}: error-record re-encode", v.name);
            }
            other => panic!("{}: unknown control kind {:?}", v.name, other),
        }
    }
    assert!(
        seen >= 6,
        "only {} control vectors -- the file lost some",
        seen
    );
}

/// The file covers every frame type, so a type nobody wrote a vector for is a
/// visible gap rather than a silent one.
#[test]
fn the_vectors_cover_every_frame_type() {
    let vs = load();
    for ty in [
        Type::HELLO,
        Type::HELLO_ACK,
        Type::HEARTBEAT,
        Type::MSG,
        Type::REQ,
        Type::RESP,
        Type::FILE_NOTIFY,
        Type::RESYNC,
        Type::BYE,
    ] {
        assert!(
            vs.iter().any(|v| v.header.ty == ty),
            "no committed vector carries a {}",
            ty.as_str()
        );
    }
    // ...and every flag bit this version defines.
    for fl in [
        Flags::RETRY,
        Flags::ERROR,
        Flags::SNAPSHOT,
        Flags::HAS_DIGEST,
    ] {
        assert!(
            vs.iter().any(|v| v.header.flags.has(fl)),
            "no committed vector carries flag {:#x}",
            fl.0
        );
    }
    // ...and a fragmented message, which is where a frag/nfrag byte swap hides.
    assert!(
        vs.iter().any(|v| v.header.nfrag > 1 && v.header.frag > 0),
        "no committed vector is a non-first fragment"
    );
    // ...and all 256 byte values, which is the property the probe measured on
    // the real transport and the reason the payload is opaque bytes.
    assert!(
        vs.iter()
            .any(|v| v.payload.len() == 256
                && v.payload.iter().enumerate().all(|(i, b)| *b == i as u8)),
        "no committed vector carries all 256 byte values"
    );
}

fn hexs(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}
