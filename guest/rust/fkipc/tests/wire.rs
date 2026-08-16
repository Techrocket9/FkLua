//! The frame codec, mirroring `guest/go/fkipc/wire/frame_test.go` case for
//! case. Where a case here has no Go twin the comment says so.

use fkipc::wire::{self, Error, Flags, Header, Type};

/// Every type and every flag survives a round trip, with the payload intact.
///
/// The payload here is all 256 byte values because that is the property the
/// probe measured on the real transport in both directions -- NUL does not
/// truncate, high bytes are not UTF-8-mangled -- and a codec that could not
/// carry them would make the measurement moot.
#[test]
fn every_type_and_flag_round_trips() {
    let payload: Vec<u8> = (0..=255u8).collect();
    let types = [
        Type::HELLO,
        Type::HELLO_ACK,
        Type::HEARTBEAT,
        Type::MSG,
        Type::REQ,
        Type::RESP,
        Type::FILE_NOTIFY,
        Type::RESYNC,
        Type::BYE,
    ];
    let flags = [
        Flags::NONE,
        Flags::RETRY,
        Flags::ERROR,
        Flags::SNAPSHOT,
        Flags::HAS_DIGEST,
        Flags::RETRY | Flags::ERROR | Flags::SNAPSHOT | Flags::HAS_DIGEST,
    ];
    let mut buf = Vec::new();
    for ty in types {
        for fl in flags {
            let mut want = Header {
                ty,
                flags: fl,
                channel: 0xBEEF,
                epoch: 0x11223344,
                seq: u32::MAX,
                corr: 0xDEADBEEF,
                length: 0,
                frag: 3,
                nfrag: 9,
            };
            buf.clear();
            wire::append_frame(&mut buf, want, &payload).unwrap();
            assert_eq!(buf.len(), wire::HEADER_BYTES + payload.len());
            let (got, p) = wire::decode(&buf).unwrap();
            want.length = payload.len() as u16;
            assert_eq!(got, want, "{}/{:?}", ty.as_str(), fl);
            assert_eq!(p, &payload[..], "{} payload did not survive", ty.as_str());
        }
    }
}

/// A zero-length payload is a frame, not a degenerate case: RESYNC and BYE have
/// no payload at all and a receiver must not read the length as "unset".
#[test]
fn a_zero_length_payload_is_a_frame() {
    let mut buf = Vec::new();
    wire::append_frame(
        &mut buf,
        Header {
            ty: Type::RESYNC,
            channel: 7,
            ..Default::default()
        },
        &[],
    )
    .unwrap();
    let (h, p) = wire::decode(&buf).unwrap();
    assert_eq!((h.length, p.len(), h.channel, h.nfrag), (0, 0, 7, 1));
}

/// TRUNCATION AT EVERY BOUNDARY BYTE.
///
/// A datagram cut anywhere must be rejected, and the two ways it can be cut are
/// different failures: inside the header there is nothing to read, and after it
/// the length field is what catches the loss. The second is the whole reason
/// length is carried on a transport that already knows it.
#[test]
fn truncation_at_every_byte_is_rejected() {
    let mut full = Vec::new();
    wire::append_frame(
        &mut full,
        Header {
            ty: Type::MSG,
            channel: 1,
            epoch: 5,
            seq: 9,
            ..Default::default()
        },
        b"0123456789abcdef",
    )
    .unwrap();
    for n in 0..full.len() {
        let want = if n < wire::HEADER_BYTES {
            Error::Short
        } else {
            Error::Length
        };
        match wire::decode(&full[..n]) {
            Err(e) => assert_eq!(e, want, "prefix {}", n),
            Ok(_) => panic!("a {}-byte prefix of a {}-byte frame decoded", n, full.len()),
        }
    }
    wire::decode(&full).expect("the untruncated frame");
}

/// A datagram LONGER than its length field is rejected too, which is the
/// coalescing case: two frames in one datagram, or a peer that wrote a stale
/// length. It is the same test and the same rule, from the other side.
#[test]
fn an_overlong_datagram_is_rejected() {
    let mut full = Vec::new();
    wire::append_frame(
        &mut full,
        Header {
            ty: Type::MSG,
            ..Default::default()
        },
        b"ab",
    )
    .unwrap();
    let mut over = full.clone();
    over.push(b'x');
    assert_eq!(wire::decode(&over), Err(Error::Length));
    // ...and the same bytes with the length field lying, low as well as high.
    full[20] = 1;
    assert_eq!(wire::decode(&full), Err(Error::Length));
}

/// Junk on a shared local port is what the magic is for, and a version we do
/// not speak is a separate answer because a session logs it once rather than
/// counting it per frame.
#[test]
fn junk_and_an_unknown_version_are_distinguished() {
    let mut good = Vec::new();
    wire::append_frame(
        &mut good,
        Header {
            ty: Type::MSG,
            ..Default::default()
        },
        b"hi",
    )
    .unwrap();

    let mut junk = good.clone();
    junk[0] = b'X';
    assert_eq!(wire::decode(&junk), Err(Error::Magic));

    let mut future = good.clone();
    future[2] = wire::VERSION + 1;
    assert_eq!(wire::decode(&future), Err(Error::Version));

    // The version is checked BEFORE the type, so a v2 frame using a type this
    // version has never heard of reports the version -- which is the useful
    // message, because the type is only unknown as a consequence.
    future[3] = 0x40;
    assert_eq!(wire::decode(&future), Err(Error::Version));
}

/// An unknown type is dropped and counted, never guessed at.
#[test]
fn an_unknown_type_is_rejected() {
    let mut good = Vec::new();
    wire::append_frame(
        &mut good,
        Header {
            ty: Type::MSG,
            ..Default::default()
        },
        &[],
    )
    .unwrap();
    for ty in [0x00u8, 0x0A, 0x7F, 0xFF] {
        let mut bad = good.clone();
        bad[3] = ty;
        assert_eq!(wire::decode(&bad), Err(Error::Type), "type {:#04x}", ty);
    }
}

/// Unknown FLAG bits are ignored, which is the opposite rule and is deliberate:
/// a flag refines a frame the receiver already understands.
#[test]
fn unknown_flag_bits_are_ignored() {
    let mut buf = Vec::new();
    wire::append_frame(
        &mut buf,
        Header {
            ty: Type::MSG,
            flags: Flags::SNAPSHOT | Flags(0x8000),
            ..Default::default()
        },
        b"x",
    )
    .unwrap();
    let (h, _) = wire::decode(&buf).expect("a reserved flag bit made a frame undecodable");
    assert!(h.flags.has(Flags::SNAPSHOT), "the known bit was lost");
}

/// An impossible fragment description is structurally undecodable: nfrag 0 says
/// there is no message, and frag >= nfrag names a piece outside it. Either one
/// would index a reassembly buffer out of range in a caller that trusted the
/// header, which is exactly what "no partial parse" means.
#[test]
fn impossible_fragments_are_rejected() {
    let mut good = Vec::new();
    wire::append_frame(
        &mut good,
        Header {
            ty: Type::MSG,
            ..Default::default()
        },
        &[],
    )
    .unwrap();
    for (frag, nfrag) in [(0u8, 0u8), (1, 1), (5, 3), (255, 0)] {
        let mut bad = good.clone();
        bad[22] = frag;
        bad[23] = nfrag;
        assert_eq!(
            wire::decode(&bad),
            Err(Error::Fragment),
            "frag {} of {}",
            frag,
            nfrag
        );
    }
    let mut out = Vec::new();
    assert_eq!(
        wire::append_frame(
            &mut out,
            Header {
                ty: Type::MSG,
                frag: 4,
                nfrag: 4,
                ..Default::default()
            },
            &[],
        ),
        Err(Error::Fragment),
        "the encoder accepted frag == nfrag"
    );
    assert!(out.is_empty(), "a refused encode still wrote bytes");
}

/// `append_frame` owns length, magic and version. A caller that filled length
/// in itself and got it wrong would emit frames the far end drops for the rest
/// of the session, and nothing would say so.
#[test]
fn the_encoder_owns_length_magic_and_version() {
    let mut buf = Vec::new();
    wire::append_frame(
        &mut buf,
        Header {
            ty: Type::MSG,
            length: 9999,
            ..Default::default()
        },
        b"four",
    )
    .unwrap();
    let (h, p) = wire::decode(&buf).unwrap();
    assert_eq!((h.length, p), (4, &b"four"[..]));
    assert_eq!(&buf[..3], &[b'F', b'K', wire::VERSION]);
}

/// Appending into a reused buffer is what the guest does on every send, and it
/// must not depend on the buffer being empty or on the previous frame's size.
#[test]
fn appending_into_a_reused_buffer_is_clean() {
    let mut buf = Vec::new();
    for n in [1000usize, 4, 700, 0, 3] {
        let payload = vec![n as u8; n];
        buf.clear();
        wire::append_frame(
            &mut buf,
            Header {
                ty: Type::MSG,
                ..Default::default()
            },
            &payload,
        )
        .unwrap();
        let (h, p) = wire::decode(&buf).unwrap();
        assert_eq!(h.length as usize, n);
        assert_eq!(p, &payload[..]);
    }
}

/// RFC-1982 serial arithmetic, including the wrap the u32 seq will never
/// actually reach. Both arms, because a receiver that got the drop arm wrong
/// would deliver stale state forever and a receiver that got the gap arm wrong
/// would never resync.
#[test]
fn serial_delta_at_the_wrap() {
    let cases: &[(u32, u32, i32)] = &[
        (1, 0, 1),
        (2, 1, 1),
        (5, 1, 4),
        (1, 5, -4),
        (1, 1, 0),
        (0, u32::MAX, 1),             // the wrap, in order
        (2, u32::MAX, 3),             // the wrap, with a gap
        (u32::MAX, 0, -1),            // the wrap, backwards
        (u32::MAX - 1, u32::MAX, -1), //
        (0x80000000, 0, i32::MIN),    // the antipode, which is "old"
    ];
    for &(seq, last, want) in cases {
        assert_eq!(
            wire::serial_delta(seq, last),
            want,
            "serial_delta({}, {})",
            seq,
            last
        );
    }
}

/// The digest is a fixed function and both ends compute it, so it is pinned
/// against the published FNV-1a-32 vectors rather than against itself.
#[test]
fn the_digest_is_fnv1a32() {
    for (input, want) in [
        ("", 2166136261u32),
        ("a", 0xe40c292c),
        ("foobar", 0xbf9cf968),
    ] {
        assert_eq!(
            wire::fnv1a32(input.as_bytes()),
            want,
            "fnv1a32({:?})",
            input
        );
    }
}

/// A payload longer than the length field can express is refused rather than
/// silently truncated.
#[test]
fn an_overlong_payload_is_refused() {
    let mut out = Vec::new();
    assert_eq!(
        wire::append_frame(
            &mut out,
            Header {
                ty: Type::MSG,
                ..Default::default()
            },
            &vec![0u8; wire::MAX_PAYLOAD + 1],
        ),
        Err(Error::TooLong)
    );
}

/// THE CONSTANTS, checked against the values the as-built report fixes.
///
/// They are a shared wire format rather than a tuning table: a Rust guest whose
/// `MAX_FRAGMENTS` disagreed with the Go half's would reassemble a message the
/// other end split differently, and nothing in either implementation would say
/// anything. Written out as literals ON PURPOSE -- a test that read them from
/// the same constants it is checking would assert nothing.
#[test]
fn the_wire_constants_are_the_agreed_ones() {
    assert_eq!(wire::HEADER_BYTES, 24);
    assert_eq!(wire::VERSION, 1);
    assert_eq!(wire::MAGIC, 0x4B46);
    assert_eq!(wire::MAX_FRAME_CEILING, 3900);
    assert_eq!(wire::DEFAULT_MAX_FRAME, 2048);
    assert_eq!(wire::MAX_FRAGMENTS, 16);
    assert_eq!(wire::MIN_MAX_FRAME, 88);
    assert_eq!(wire::MAX_PAYLOAD, 65535);

    assert_eq!(
        [
            Type::HELLO.0,
            Type::HELLO_ACK.0,
            Type::HEARTBEAT.0,
            Type::MSG.0,
            Type::REQ.0,
            Type::RESP.0,
            Type::FILE_NOTIFY.0,
            Type::RESYNC.0,
            Type::BYE.0,
        ],
        [1, 2, 3, 4, 5, 6, 7, 8, 9]
    );
    assert_eq!(
        [
            Flags::RETRY.0,
            Flags::ERROR.0,
            Flags::SNAPSHOT.0,
            Flags::HAS_DIGEST.0
        ],
        [1, 2, 4, 8]
    );
    assert_eq!(
        [
            wire::CODE_NO_HANDLER,
            wire::CODE_BAD_FRAME,
            wire::CODE_DUPLICATE,
            wire::CODE_BUSY,
            wire::CODE_APP,
        ],
        [1, 2, 3, 4, 5]
    );
}

/// The protocol's timers and budgets, same reasoning as above: two
/// implementations of one protocol whose `DEDUP_TICKS` disagree produce a
/// re-executed request rather than an error.
#[test]
fn the_protocol_constants_are_the_agreed_ones() {
    assert_eq!(fkipc::RETRY_TICKS_SERVER, 15);
    assert_eq!(fkipc::RETRY_TICKS_CLIENT, 6);
    assert_eq!(fkipc::RETRY_BACKOFF_CAP, 60);
    assert_eq!(fkipc::MAX_RETRIES, 4);
    assert_eq!(fkipc::DEDUP_TICKS, 600);
    assert_eq!(fkipc::MAX_DEDUP, 256);
    assert_eq!(fkipc::MAX_DEDUP_PAYLOAD, 512);
    assert_eq!(fkipc::HEARTBEAT_TICKS, 60);
    assert_eq!(fkipc::LIVENESS_TICKS, 180);
    assert_eq!(fkipc::REASSEMBLY_TICKS, 120);
    assert_eq!(fkipc::SEARCH_TICKS, 60);
    assert_eq!(fkipc::SEND_BUDGET, 8);
    assert_eq!(fkipc::DRAIN_MAX, 1);
    assert_eq!(fkipc::MAX_QUEUE, 64);
    assert_eq!(fkipc::MAX_PENDING, 16);
}
