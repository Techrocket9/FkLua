//! The control payloads: HELLO/HELLO_ACK, HEARTBEAT, FILE_NOTIFY and the RESP
//! error record.
//!
//! These have a defined shape because both ends must agree on them and there is
//! nothing app-shaped about them. Everything else -- MSG, REQ, RESP results --
//! stays opaque.
//!
//! Each decoder either returns a complete value or an error. A short control
//! payload is a dropped frame, not a value with zeros in the fields that were
//! missing: the whole reason HELLO carries `max_frame` is that the sender obeys
//! it, and a `max_frame` of 0 read out of a truncated datagram would be worse
//! than no HELLO at all.

use alloc::string::String;
use alloc::vec::Vec;

use super::{put_u16, put_u32, u16_at, u32_at, Error, MAX_PAYLOAD};

/// Which side of the two driving shapes a peer is.
///
/// It rides in HELLO because the peer's sensible defaults differ -- a headless
/// server's inbound budget is ~6 kB/s once anyone is connected and a single
/// player client's is not -- and because a log that says which one it is
/// talking to is worth one byte.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Profile(pub u8);

impl Profile {
    pub const SERVER: Profile = Profile(0);
    pub const CLIENT: Profile = Profile(1);

    pub fn as_str(self) -> &'static str {
        if self == Profile::CLIENT {
            "client"
        } else {
            "server"
        }
    }
}

/// The payload of both HELLO and HELLO_ACK.
///
/// One struct for both directions because the fields mean the same thing from
/// either end, with two exceptions the field comments name: the peer sends
/// `boot = 0` (it has no save to time-travel with) and `tick` as the last tick
/// it saw rather than the current one.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct Hello {
    pub proto_min: u8,
    pub proto_max: u8,
    /// The largest frame this side will ACCEPT, not the largest it will send.
    ///
    /// The sender respects the peer's number. It is negotiated rather than
    /// constant because it is a budget shared with the application: the guest's
    /// string scratch region is reset once per outermost dispatch, so an
    /// inbound payload holds its own length for the whole handler, and a guest
    /// that reads entity names from inside a message handler wants a smaller
    /// frame than one that only decodes.
    pub max_frame: u16,
    /// The most fragments this side will reassemble.
    pub max_fragments: u16,
    /// The guest's load counter, and 0 from the peer.
    ///
    /// Best-effort and monotone WITHIN a timeline only: two loads of one save
    /// produce the same value, by construction, which is exactly why it cannot
    /// be a session id and why the peer mints the epoch instead.
    pub boot: u32,
    /// The guest's current tick, or the last tick the peer saw. It is what
    /// reconciles the guest's tick-based timers with the peer's real clock.
    pub tick: u32,
    pub profile: Profile,
    /// The mod name, for the peer's logs and for multiplexing.
    pub name: String,
}

const HELLO_FIXED: usize = 18;

/// Writes a HELLO/HELLO_ACK payload.
pub fn append_hello(dst: &mut Vec<u8>, h: &Hello) -> Result<(), Error> {
    if h.name.len() > MAX_PAYLOAD - HELLO_FIXED {
        return Err(Error::TooLong);
    }
    let mut b = [0u8; HELLO_FIXED];
    b[0] = h.proto_min;
    b[1] = h.proto_max;
    put_u16(&mut b[2..], h.max_frame);
    put_u16(&mut b[4..], h.max_fragments);
    put_u32(&mut b[6..], h.boot);
    put_u32(&mut b[10..], h.tick);
    b[14] = h.profile.0;
    b[15] = 0; // reserved
    put_u16(&mut b[16..], h.name.len() as u16);
    dst.extend_from_slice(&b);
    dst.extend_from_slice(h.name.as_bytes());
    Ok(())
}

/// Reads a HELLO/HELLO_ACK payload.
///
/// The name is COPIED into an owned String rather than borrowing `p`, because a
/// HELLO is kept for the life of the session while `p` is the receive buffer.
/// It is also the one place this crate is allowed to be lossy about bytes: a
/// mod name is a mod name, and a `String` is what the application wants.
pub fn decode_hello(p: &[u8]) -> Result<Hello, Error> {
    if p.len() < HELLO_FIXED {
        return Err(Error::Control);
    }
    let n = u16_at(p, 16) as usize;
    if p.len() < HELLO_FIXED + n {
        return Err(Error::Control);
    }
    Ok(Hello {
        proto_min: p[0],
        proto_max: p[1],
        max_frame: u16_at(p, 2),
        max_fragments: u16_at(p, 4),
        boot: u32_at(p, 6),
        tick: u32_at(p, 10),
        profile: Profile(p[14]),
        name: String::from_utf8_lossy(&p[HELLO_FIXED..HELLO_FIXED + n]).into_owned(),
    })
}

/// The payload of HEARTBEAT.
///
/// THIS IS FLOW CONTROL, NOT TELEMETRY. The counters give the peer a real rate
/// to aim at once per second of game time, and the guest's SILENCE is the
/// signal that matters: the peer has a clock and the guest's heartbeats stop
/// when the game does, so a peer that has heard nothing for its own quiet
/// threshold stops sending everything but its own heartbeat. That, and not a
/// bigger OS buffer, is what keeps a long pause or a slow save from dropping
/// frames -- the buffer is 256 KB and overflows silently.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Heartbeat {
    pub tick: u32,
    /// Frames accepted since the last heartbeat.
    pub rx: u32,
    /// Frames dropped since the last heartbeat.
    pub drops: u32,
    /// Gaps observed since the last heartbeat.
    pub gaps: u32,
}

const HEARTBEAT_BYTES: usize = 16;

pub fn append_heartbeat(dst: &mut Vec<u8>, h: Heartbeat) {
    let mut b = [0u8; HEARTBEAT_BYTES];
    put_u32(&mut b[0..], h.tick);
    put_u32(&mut b[4..], h.rx);
    put_u32(&mut b[8..], h.drops);
    put_u32(&mut b[12..], h.gaps);
    dst.extend_from_slice(&b);
}

pub fn decode_heartbeat(p: &[u8]) -> Result<Heartbeat, Error> {
    if p.len() < HEARTBEAT_BYTES {
        return Err(Error::Control);
    }
    Ok(Heartbeat {
        tick: u32_at(p, 0),
        rx: u32_at(p, 4),
        drops: u32_at(p, 8),
        gaps: u32_at(p, 12),
    })
}

/// The payload of FILE_NOTIFY: "there is a file at X".
///
/// `bytes` and `fnv1a32` are meaningful only when the frame carries
/// `Flags::HAS_DIGEST`, and the distinction is the whole design. A file the
/// GUEST wrote it also hashed, so the peer's test is exact -- read until
/// `bytes` and the checksum matches, or keep waiting. A file the ENGINE wrote
/// (a screenshot) the guest has never held and cannot describe, so the digest
/// is absent and the peer falls back to stabilize-polling. Nothing documents a
/// flush guarantee for `write_file`, which is why this is a test rather than a
/// promise.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct FileNotify {
    pub bytes: u32,
    pub fnv1a32: u32,
    pub name: String,
}

const FILE_NOTIFY_FIXED: usize = 10;

pub fn append_file_notify(dst: &mut Vec<u8>, f: &FileNotify) -> Result<(), Error> {
    if f.name.len() > MAX_PAYLOAD - FILE_NOTIFY_FIXED {
        return Err(Error::TooLong);
    }
    let mut b = [0u8; FILE_NOTIFY_FIXED];
    put_u32(&mut b[0..], f.bytes);
    put_u32(&mut b[4..], f.fnv1a32);
    put_u16(&mut b[8..], f.name.len() as u16);
    dst.extend_from_slice(&b);
    dst.extend_from_slice(f.name.as_bytes());
    Ok(())
}

pub fn decode_file_notify(p: &[u8]) -> Result<FileNotify, Error> {
    if p.len() < FILE_NOTIFY_FIXED {
        return Err(Error::Control);
    }
    let n = u16_at(p, 8) as usize;
    if p.len() < FILE_NOTIFY_FIXED + n {
        return Err(Error::Control);
    }
    Ok(FileNotify {
        bytes: u32_at(p, 0),
        fnv1a32: u32_at(p, 4),
        name: String::from_utf8_lossy(&p[FILE_NOTIFY_FIXED..FILE_NOTIFY_FIXED + n]).into_owned(),
    })
}

// The RESP error codes.
//
// CODE_DUPLICATE is the interesting one: it is the answer to a retried REQ
// whose response was too large to cache. The application learns that the
// operation EXECUTED and the result is gone, which is strictly better than the
// two alternatives -- silently re-executing it, or growing the save without
// bound to hold every reply. A handler with a large result should write a file
// and answer with a FILE_NOTIFY, which is the right shape for a large result
// anyway.
pub const CODE_NO_HANDLER: u16 = 1;
pub const CODE_BAD_FRAME: u16 = 2;
pub const CODE_DUPLICATE: u16 = 3;
pub const CODE_BUSY: u16 = 4;
/// After which the rest of the payload is the app's own.
pub const CODE_APP: u16 = 5;

/// The payload of a RESP carrying `Flags::ERROR`.
#[derive(Clone, PartialEq, Eq, Debug, Default)]
pub struct ErrorRecord {
    pub code: u16,
    /// UTF-8, to the end of the payload.
    pub message: String,
}

pub fn append_error_record(dst: &mut Vec<u8>, e: &ErrorRecord) -> Result<(), Error> {
    if e.message.len() > MAX_PAYLOAD - 2 {
        return Err(Error::TooLong);
    }
    let mut b = [0u8; 2];
    put_u16(&mut b[0..], e.code);
    dst.extend_from_slice(&b);
    dst.extend_from_slice(e.message.as_bytes());
    Ok(())
}

pub fn decode_error_record(p: &[u8]) -> Result<ErrorRecord, Error> {
    let (code, msg) = decode_error_record_ref(p)?;
    Ok(ErrorRecord {
        code,
        message: String::from_utf8_lossy(msg).into_owned(),
    })
}

/// [`decode_error_record`] without the copy: the code, and the message bytes
/// still borrowing `p`.
///
/// It exists because a [`crate::Reply`] is handed to a `fn` pointer and must
/// not allocate. The Go half's `PeerError` carries an owned message string; on
/// this side that would be an allocation per failed request in a heap that is
/// in the save, so the message travels as the reply's own payload instead and
/// the code travels beside it.
pub fn decode_error_record_ref(p: &[u8]) -> Result<(u16, &[u8]), Error> {
    if p.len() < 2 {
        return Err(Error::Control);
    }
    Ok((u16_at(p, 0), &p[2..]))
}
