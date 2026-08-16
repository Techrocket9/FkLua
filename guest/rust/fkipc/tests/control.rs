//! The control payloads, mirroring `guest/go/fkipc/wire/control_test.go`, plus
//! the version parse the gate is built on.

use fkipc::wire::{self, control, Error, Header, Type};
use fkipc::{parse_version, Version, MIN_ENGINE_VERSION};

#[test]
fn hello_round_trips() {
    let want = wire::Hello {
        proto_min: 1,
        proto_max: 1,
        max_frame: 2048,
        max_fragments: 16,
        boot: 0xCAFEF00D,
        tick: 123456,
        profile: wire::Profile::CLIENT,
        name: "fk-demo".to_string(),
    };
    let mut buf = Vec::new();
    control::append_hello(&mut buf, &want).unwrap();
    assert_eq!(control::decode_hello(&buf).unwrap(), want);

    // An empty name is legal: the field is for the peer's logs.
    buf.clear();
    control::append_hello(
        &mut buf,
        &wire::Hello {
            proto_min: 1,
            proto_max: 1,
            ..Default::default()
        },
    )
    .unwrap();
    assert_eq!(control::decode_hello(&buf).unwrap().name, "");
}

/// A truncated control payload is a dropped frame, not a value with zeros where
/// the missing bytes were. A `max_frame` of 0 read out of a short HELLO would
/// be obeyed by the sender and the session would never carry a byte.
#[test]
fn a_truncated_control_payload_is_rejected() {
    let mut hello = Vec::new();
    control::append_hello(
        &mut hello,
        &wire::Hello {
            proto_min: 1,
            proto_max: 1,
            max_frame: 2048,
            max_fragments: 16,
            name: "abcdef".to_string(),
            ..Default::default()
        },
    )
    .unwrap();
    for n in 0..hello.len() {
        assert_eq!(
            control::decode_hello(&hello[..n]),
            Err(Error::Control),
            "hello prefix {}",
            n
        );
    }

    let mut hb = Vec::new();
    control::append_heartbeat(
        &mut hb,
        wire::Heartbeat {
            tick: 1,
            rx: 2,
            drops: 3,
            gaps: 4,
        },
    );
    for n in 0..hb.len() {
        assert_eq!(
            control::decode_heartbeat(&hb[..n]),
            Err(Error::Control),
            "heartbeat prefix {}",
            n
        );
    }

    let mut fnote = Vec::new();
    control::append_file_notify(
        &mut fnote,
        &wire::FileNotify {
            bytes: 10,
            fnv1a32: 11,
            name: "shot.png".to_string(),
        },
    )
    .unwrap();
    for n in 0..fnote.len() {
        assert_eq!(
            control::decode_file_notify(&fnote[..n]),
            Err(Error::Control),
            "file-notify prefix {}",
            n
        );
    }

    assert_eq!(control::decode_error_record(&[7]), Err(Error::Control));
}

/// A name_len that overruns the payload is the same failure from the other
/// side: the fixed part is present, the variable part is not.
#[test]
fn a_control_name_length_cannot_overrun() {
    let mut hello = Vec::new();
    control::append_hello(
        &mut hello,
        &wire::Hello {
            proto_min: 1,
            proto_max: 1,
            name: "abc".to_string(),
            ..Default::default()
        },
    )
    .unwrap();
    hello[16] = 200; // claim a 200-byte name after three bytes of it
    assert_eq!(control::decode_hello(&hello), Err(Error::Control));
}

#[test]
fn heartbeat_and_file_notify_round_trip() {
    let hb = wire::Heartbeat {
        tick: u32::MAX,
        rx: 1,
        drops: 2,
        gaps: 3,
    };
    let mut buf = Vec::new();
    control::append_heartbeat(&mut buf, hb);
    assert_eq!(control::decode_heartbeat(&buf).unwrap(), hb);

    let fnote = wire::FileNotify {
        bytes: 4096,
        fnv1a32: 0xDEADBEEF,
        name: "fkipc/dump.bin".to_string(),
    };
    buf.clear();
    control::append_file_notify(&mut buf, &fnote).unwrap();
    assert_eq!(control::decode_file_notify(&buf).unwrap(), fnote);
}

/// The error record's message runs to the end of the payload, so it needs no
/// length of its own -- and an empty one is legal, because a code is sometimes
/// the whole story.
#[test]
fn an_error_record_runs_to_the_end() {
    for msg in ["", "no such handler", &"x".repeat(1000)] {
        let mut buf = Vec::new();
        control::append_error_record(
            &mut buf,
            &wire::ErrorRecord {
                code: wire::CODE_APP,
                message: msg.to_string(),
            },
        )
        .unwrap();
        let got = control::decode_error_record(&buf).unwrap();
        assert_eq!((got.code, got.message.as_str()), (wire::CODE_APP, msg));
        // ...and the borrowing decoder the reply path uses reads the same pair.
        let (code, body) = control::decode_error_record_ref(&buf).unwrap();
        assert_eq!((code, body), (wire::CODE_APP, msg.as_bytes()));
    }
}

/// A control payload the caller cannot express is refused rather than silently
/// truncated by the u16 length field.
#[test]
fn an_overlong_control_name_is_refused() {
    let huge = "n".repeat(wire::MAX_PAYLOAD);
    let mut buf = Vec::new();
    assert_eq!(
        control::append_hello(
            &mut buf,
            &wire::Hello {
                name: huge.clone(),
                ..Default::default()
            }
        ),
        Err(Error::TooLong)
    );
    assert_eq!(
        control::append_file_notify(
            &mut buf,
            &wire::FileNotify {
                name: huge,
                ..Default::default()
            }
        ),
        Err(Error::TooLong)
    );
    assert_eq!(
        wire::append_frame(
            &mut buf,
            Header {
                ty: Type::MSG,
                ..Default::default()
            },
            &vec![0u8; wire::MAX_PAYLOAD + 1]
        ),
        Err(Error::TooLong)
    );
}

#[test]
fn parse_version_reads_what_the_engine_reports() {
    for (input, want) in [
        ("2.1.14", Version::new(2, 1, 14)),
        ("0.0.0", Version::ZERO),
        ("2.0.77", Version::new(2, 0, 77)),
        // helpers.game_version is documented as the version and nothing more,
        // but a trailing build tag has appeared in Factorio's version strings
        // before and must not make the whole read fail closed.
        ("2.1.14 (build 84539)", Version::new(2, 1, 14)),
        ("10.20.30", Version::new(10, 20, 30)),
    ] {
        assert_eq!(
            parse_version(input),
            Some(want),
            "parse_version({:?})",
            input
        );
    }
    for bad in [
        "",
        "2",
        "2.1",
        "2..1",
        "x.y.z",
        "2.1.x",
        "-1.0.0",
        "999999.0.0",
        "2.1.",
    ] {
        assert_eq!(
            parse_version(bad),
            None,
            "parse_version({:?}) was accepted",
            bad
        );
    }
}

/// `less` is the gate's whole decision, so both directions and the equal case
/// are pinned rather than assumed. AT THE FLOOR THE GATE OPENS --
/// `MIN_ENGINE_VERSION` is the version that was measured working, not the one
/// after it.
#[test]
fn version_ordering_decides_the_gate() {
    let f = MIN_ENGINE_VERSION;
    for v in [
        Version::new(2, 0, 77),
        Version::new(2, 1, 13),
        Version::new(1, 9, 99),
        Version::ZERO,
    ] {
        assert!(v.less(f), "{:?} is not below the floor {:?}", v, f);
    }
    for v in [
        f,
        Version::new(2, 1, 15),
        Version::new(2, 2, 0),
        Version::new(3, 0, 0),
    ] {
        assert!(!v.less(f), "{:?} reads as below the floor {:?}", v, f);
    }
    assert_eq!(
        f,
        Version::new(2, 1, 14),
        "the floor is the version the probe MEASURED working, so moving it \
         wants a probe run and not an argument from a changelog"
    );
}
