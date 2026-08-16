//! The frame codec: one datagram is one frame.
//!
//! A transliteration of `guest/go/fkipc/wire` and deliberately nothing more.
//! The two backends must stay level constant for constant, and the instrument
//! for that is not a census row -- nothing generates this -- so it is a test:
//! `TestBothGuestLibrariesSpeakTheSameWire` drives both compiled guests against
//! one host stub with one set of expectations, and `tests/vectors.rs` reads
//! frames the GO codec produced out of `testdata/ipc/wire-vectors.txt`. Two
//! implementations of one wire format is the AD5 shape -- the same defect in
//! the same function, fixed on one backend and left standing on the other for
//! two milestones because the test was written against one -- and pinning them
//! to the same BYTES rather than to parallel authorship is the mitigation.

pub mod control;

pub use control::{
    ErrorRecord, FileNotify, Heartbeat, Hello, Profile, CODE_APP, CODE_BAD_FRAME, CODE_BUSY,
    CODE_DUPLICATE, CODE_NO_HANDLER,
};

/// The fixed frame header. Every offset below is inside it.
pub const HEADER_BYTES: usize = 24;

/// The protocol major this module speaks.
///
/// A frame carrying any other value is dropped: a minor difference would be
/// expressed by a flag bit or an unused frame type, both of which degrade
/// gracefully, so a major bump means the layout itself moved.
pub const VERSION: u8 = 1;

/// Bytes `'F'`, `'K'` read little-endian.
///
/// Two bytes rather than four: the socket is a shared local port and anything
/// on the machine can send to it, so the magic's job is rejecting junk -- and
/// the acceptance test is compound (magic, a version we speak, a type in range,
/// a length agreeing with the datagram, an epoch we recognise), which is far
/// stronger than four bytes of magic and two bytes cheaper. ASCII "FK" also
/// means a hexdump identifies the protocol.
pub const MAGIC: u16 = 0x4B46;

// Field offsets. They are named because two implementations reading "the u32 at
// 12" out of prose is how a wire format drifts.
const OFF_MAGIC: usize = 0;
const OFF_VERSION: usize = 2;
const OFF_TYPE: usize = 3;
const OFF_FLAGS: usize = 4;
const OFF_CHANNEL: usize = 6;
const OFF_EPOCH: usize = 8;
const OFF_SEQ: usize = 12;
const OFF_CORR: usize = 16;
const OFF_LENGTH: usize = 20;
const OFF_FRAG: usize = 22;
const OFF_NFRAG: usize = 23;

/// The frame type at offset 3.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Type(pub u8);

impl Type {
    pub const HELLO: Type = Type(0x01); // guest -> peer, opens a session
    pub const HELLO_ACK: Type = Type(0x02); // peer -> guest, mints the epoch
    pub const HEARTBEAT: Type = Type(0x03); // both, liveness and flow control
    pub const MSG: Type = Type(0x04); // fire-and-forget, seq'd, gap-detectable
    pub const REQ: Type = Type(0x05); // correlated request
    pub const RESP: Type = Type(0x06); // correlated response, or an error record
    pub const FILE_NOTIFY: Type = Type(0x07); // "there is a file at X"
    pub const RESYNC: Type = Type(0x08); // "channel N is stale, send me a snapshot"
    pub const BYE: Type = Type(0x09); // advisory clean shutdown

    /// Reports whether this is a type this version defines.
    ///
    /// An unknown type is dropped and counted, never guessed at. A receiver
    /// that treated one as "probably a MSG" would deliver an app payload with a
    /// meaning nobody agreed on.
    pub const fn known(self) -> bool {
        self.0 >= Type::HELLO.0 && self.0 <= Type::BYE.0
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Type::HELLO => "HELLO",
            Type::HELLO_ACK => "HELLO_ACK",
            Type::HEARTBEAT => "HEARTBEAT",
            Type::MSG => "MSG",
            Type::REQ => "REQ",
            Type::RESP => "RESP",
            Type::FILE_NOTIFY => "FILE_NOTIFY",
            Type::RESYNC => "RESYNC",
            Type::BYE => "BYE",
            _ => "UNKNOWN",
        }
    }
}

/// The bitfield at offset 4.
///
/// UNKNOWN BITS ARE IGNORED, which is the opposite of the rule for types and
/// versions and is not an inconsistency: a flag is by construction an optional
/// refinement of a frame the receiver already understands, and a type is not.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Flags(pub u16);

impl Flags {
    /// A REQ or RESP that is a retransmission, so the peer can count dedup hits
    /// and a log can tell "slow" from "lost".
    pub const RETRY: Flags = Flags(1 << 0);
    /// A RESP payload that is an error record rather than a result.
    pub const ERROR: Flags = Flags(1 << 1);
    /// A MSG payload that is a complete state rather than a delta, which clears
    /// the receiver's gap condition.
    pub const SNAPSHOT: Flags = Flags(1 << 2);
    /// A FILE_NOTIFY that carries a length and checksum the peer can verify
    /// exactly, instead of having to stabilize-poll.
    pub const HAS_DIGEST: Flags = Flags(1 << 3);

    pub const NONE: Flags = Flags(0);

    /// Reports whether every bit of `f` is set.
    pub const fn has(self, f: Flags) -> bool {
        self.0 & f.0 == f.0
    }
}

impl core::ops::BitOr for Flags {
    type Output = Flags;
    fn bitor(self, o: Flags) -> Flags {
        Flags(self.0 | o.0)
    }
}

// Protocol size budgets.
//
// MAX_FRAME_CEILING is under the guest's 4 KiB string scratch region on
// purpose. An inbound payload larger than what is left of that region falls
// back to fk_alloc, and while the outermost dispatch now brackets the
// marshalling arena so that is no longer a permanent leak, it is still a
// per-packet allocation and a memcpy, and the arena keeps its chunks as
// capacity once taken -- so the peak frame size sets a floor on guest memory,
// which is in the save. A frame that fits the scratch touches none of it.
//
// It is also under every wall the probe found, and the INBOUND one is the
// binding constraint rather than the OS. Outbound reaches 9,188 B on macOS
// (net.inet.udp.maxdgram - 28); inbound on 2.1.14 delivers 4,000 B byte-exact
// and silently delivers nothing at 8,192, so the real ceiling is somewhere in
// between and 3900 clears it. Far more important than either number: AN
// OVERSIZED send_udp FAILS SILENTLY -- no error, no raise, nothing on the wire,
// and the same is true of an oversized datagram arriving. The transport will
// not tell a guest it went too far, so the cap has to be enforced here.
pub const MAX_FRAME_CEILING: u16 = 3900;
pub const DEFAULT_MAX_FRAME: u16 = 2048;
pub const MAX_FRAGMENTS: u8 = 16;

/// The smallest useful negotiated frame.
///
/// A peer advertising less than a header plus a token payload is either
/// confused or hostile, and clamping is kinder than fragmenting a heartbeat.
pub const MIN_MAX_FRAME: u16 = HEADER_BYTES as u16 + 64;

/// What the length field can express, independently of any negotiated cap.
pub const MAX_PAYLOAD: usize = 65535;

/// The decode failures, each separately counted by a session because they mean
/// different things: junk on a shared port, a peer speaking a format we do not,
/// and a datagram that was cut.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Error {
    /// A datagram shorter than a header.
    Short,
    /// Not an fkipc frame.
    Magic,
    /// An unsupported protocol version.
    Version,
    /// An unknown frame type.
    Type,
    /// A length disagreeing with the datagram.
    Length,
    /// An impossible fragment index or count.
    Fragment,
    /// A payload longer than the length field.
    TooLong,
    /// A malformed control payload. See [`control`].
    Control,
}

impl Error {
    pub fn as_str(self) -> &'static str {
        match self {
            Error::Short => "fkipc/wire: datagram shorter than a header",
            Error::Magic => "fkipc/wire: not an fkipc frame",
            Error::Version => "fkipc/wire: unsupported protocol version",
            Error::Type => "fkipc/wire: unknown frame type",
            Error::Length => "fkipc/wire: length disagrees with the datagram",
            Error::Fragment => "fkipc/wire: impossible fragment index or count",
            Error::TooLong => "fkipc/wire: payload longer than the length field",
            Error::Control => "fkipc/wire: malformed control payload",
        }
    }
}

/// One frame's fixed part, decoded.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Header {
    pub ty: Type,
    pub flags: Flags,
    pub channel: u16,
    pub epoch: u32,
    pub seq: u32,
    pub corr: u32,
    pub length: u16,
    pub frag: u8,
    pub nfrag: u8,
}

/// Appends one complete frame to `dst`.
///
/// Length is taken from `payload` and whatever the caller left in `h.length` is
/// overwritten -- the field exists so a RECEIVER can check the datagram, and
/// letting a sender disagree with itself would only ever produce frames the
/// other end drops. Magic and version are written here for the same reason.
///
/// A caller that reuses one buffer -- `dst.clear()` every time -- allocates
/// nothing after the first frame, which is the whole reason this appends rather
/// than returning a fresh one. On an error `dst` is left as it was found.
pub fn append_frame(dst: &mut alloc::vec::Vec<u8>, h: Header, payload: &[u8]) -> Result<(), Error> {
    if payload.len() > MAX_PAYLOAD {
        return Err(Error::TooLong);
    }
    let nfrag = if h.nfrag == 0 { 1 } else { h.nfrag };
    if h.frag >= nfrag {
        return Err(Error::Fragment);
    }
    let mut hdr = [0u8; HEADER_BYTES];
    put_u16(&mut hdr[OFF_MAGIC..], MAGIC);
    hdr[OFF_VERSION] = VERSION;
    hdr[OFF_TYPE] = h.ty.0;
    put_u16(&mut hdr[OFF_FLAGS..], h.flags.0);
    put_u16(&mut hdr[OFF_CHANNEL..], h.channel);
    put_u32(&mut hdr[OFF_EPOCH..], h.epoch);
    put_u32(&mut hdr[OFF_SEQ..], h.seq);
    put_u32(&mut hdr[OFF_CORR..], h.corr);
    put_u16(&mut hdr[OFF_LENGTH..], payload.len() as u16);
    hdr[OFF_FRAG] = h.frag;
    hdr[OFF_NFRAG] = nfrag;
    dst.extend_from_slice(&hdr);
    dst.extend_from_slice(payload);
    Ok(())
}

/// Reads one datagram as one frame.
///
/// ONE DATAGRAM IS ONE FRAME. Frames are never split across datagrams and a
/// datagram never carries two, so there is no offset to return and no resume
/// point: UDP delivers a datagram whole or not at all. Anything above that is
/// the message layer, which fragments MESSAGES into frames and is not this
/// module's problem.
///
/// The returned payload BORROWS `dg`, which is the same rule the guest's own
/// handlers impose on the application and for the same reason -- except that
/// here the compiler enforces it.
pub fn decode(dg: &[u8]) -> Result<(Header, &[u8]), Error> {
    if dg.len() < HEADER_BYTES {
        return Err(Error::Short);
    }
    if u16_at(dg, OFF_MAGIC) != MAGIC {
        return Err(Error::Magic);
    }
    if dg[OFF_VERSION] != VERSION {
        return Err(Error::Version);
    }
    let ty = Type(dg[OFF_TYPE]);
    if !ty.known() {
        return Err(Error::Type);
    }
    let h = Header {
        ty,
        flags: Flags(u16_at(dg, OFF_FLAGS)),
        channel: u16_at(dg, OFF_CHANNEL),
        epoch: u32_at(dg, OFF_EPOCH),
        seq: u32_at(dg, OFF_SEQ),
        corr: u32_at(dg, OFF_CORR),
        length: u16_at(dg, OFF_LENGTH),
        frag: dg[OFF_FRAG],
        nfrag: dg[OFF_NFRAG],
    };
    // THE ABSOLUTE RULE. A length that disagrees with the datagram means the
    // frame was truncated, two were coalesced, or the peer is speaking
    // something else -- and none of those is a frame to act on.
    if h.length as usize != dg.len() - HEADER_BYTES {
        return Err(Error::Length);
    }
    if h.nfrag == 0 || h.frag >= h.nfrag {
        return Err(Error::Fragment);
    }
    Ok((h, &dg[HEADER_BYTES..]))
}

/// RFC-1982-style serial arithmetic over the per-channel seq.
///
/// A named function rather than an inlined `(a - b) as i32` in each
/// implementation because this is the one comparison two implementations
/// silently disagree about, and a disagreement here does not fail -- it
/// delivers or drops the wrong frames forever. The caller's rule:
///
/// ```text
/// d > 1   a gap: deliver, raise the gap, advance
/// d == 1  in order: deliver, advance
/// d <= 0  old: DROP
/// ```
///
/// The wrap is a non-event by construction: at one frame per tick a channel
/// wraps after about two thousand years, and the comparison would be right
/// anyway.
pub fn serial_delta(seq: u32, last: u32) -> i32 {
    seq.wrapping_sub(last) as i32
}

/// The digest a FILE_NOTIFY carries.
///
/// FNV-1a over the guest's own bytes: it needs no table, no allocation and no
/// host call, and the peer's test is exact rather than a stabilize-poll. It is
/// not a security property and is not claimed as one -- the transport is
/// localhost.
pub fn fnv1a32(b: &[u8]) -> u32 {
    const OFFSET32: u32 = 2166136261;
    const PRIME32: u32 = 16777619;
    let mut h = OFFSET32;
    for &x in b {
        h ^= x as u32;
        h = h.wrapping_mul(PRIME32);
    }
    h
}

// Byte-wise little-endian, hand-rolled -- the same six lines the Go half writes
// for the same reason: wasm linear memory is little-endian by specification, so
// a guest reads a header field with a plain load and no swap, and the emitted
// Lua for a shift-and-or is something this project can read.

pub(crate) fn u16_at(b: &[u8], at: usize) -> u16 {
    b[at] as u16 | (b[at + 1] as u16) << 8
}

pub(crate) fn u32_at(b: &[u8], at: usize) -> u32 {
    b[at] as u32 | (b[at + 1] as u32) << 8 | (b[at + 2] as u32) << 16 | (b[at + 3] as u32) << 24
}

pub(crate) fn put_u16(b: &mut [u8], v: u16) {
    b[0] = v as u8;
    b[1] = (v >> 8) as u8;
}

pub(crate) fn put_u32(b: &mut [u8], v: u32) {
    b[0] = v as u8;
    b[1] = (v >> 8) as u8;
    b[2] = (v >> 16) as u8;
    b[3] = (v >> 24) as u8;
}
