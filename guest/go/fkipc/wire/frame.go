package wire

import "errors"

// The header, and the sizes derived from it.
const (
	// HeaderBytes is the fixed frame header. Every offset below is inside it.
	HeaderBytes = 24

	// Version is the protocol major this package speaks. A frame carrying any
	// other value is dropped: a minor difference would be expressed by a flag
	// bit or an unused frame type, both of which degrade gracefully, so a major
	// bump means the layout itself moved.
	Version = 1

	// Magic is bytes 'F','K' read little-endian. Two bytes rather than four:
	// the socket is a shared local port and anything on the machine can send to
	// it, so the magic's job is rejecting junk -- and the acceptance test is
	// compound (magic, a version we speak, a type in range, a length agreeing
	// with the datagram, an epoch we recognise), which is far stronger than four
	// bytes of magic and two bytes cheaper. ASCII "FK" also means a hexdump
	// identifies the protocol.
	Magic = 0x4B46
)

// Field offsets. They are named because two implementations reading "the u32 at
// 12" out of prose is how a wire format drifts.
const (
	offMagic   = 0
	offVersion = 2
	offType    = 3
	offFlags   = 4
	offChannel = 6
	offEpoch   = 8
	offSeq     = 12
	offCorr    = 16
	offLength  = 20
	offFrag    = 22
	offNFrag   = 23
)

// Type is the frame type at offset 3.
type Type uint8

const (
	TypeHello      Type = 0x01 // guest -> peer, opens a session
	TypeHelloAck   Type = 0x02 // peer -> guest, mints the epoch
	TypeHeartbeat  Type = 0x03 // both, liveness and flow control
	TypeMsg        Type = 0x04 // fire-and-forget, seq'd, gap-detectable
	TypeReq        Type = 0x05 // correlated request
	TypeResp       Type = 0x06 // correlated response, or an error record
	TypeFileNotify Type = 0x07 // "there is a file at X"
	TypeResync     Type = 0x08 // "channel N is stale, send me a snapshot"
	TypeBye        Type = 0x09 // advisory clean shutdown
)

// Known reports whether t is a type this version defines.
//
// An unknown type is dropped and counted, never guessed at. A receiver that
// treated one as "probably a MSG" would deliver an app payload with a meaning
// nobody agreed on.
func (t Type) Known() bool { return t >= TypeHello && t <= TypeBye }

func (t Type) String() string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeHelloAck:
		return "HELLO_ACK"
	case TypeHeartbeat:
		return "HEARTBEAT"
	case TypeMsg:
		return "MSG"
	case TypeReq:
		return "REQ"
	case TypeResp:
		return "RESP"
	case TypeFileNotify:
		return "FILE_NOTIFY"
	case TypeResync:
		return "RESYNC"
	case TypeBye:
		return "BYE"
	}
	return "UNKNOWN"
}

// Flags is the bitfield at offset 4.
//
// UNKNOWN BITS ARE IGNORED, which is the opposite of the rule for types and
// versions and is not an inconsistency: a flag is by construction an optional
// refinement of a frame the receiver already understands, and a type is not.
type Flags uint16

const (
	// FlagRetry marks a REQ or RESP as a retransmission, so the peer can count
	// dedup hits and a log can tell "slow" from "lost".
	FlagRetry Flags = 1 << 0
	// FlagError says a RESP payload is an error record rather than a result.
	FlagError Flags = 1 << 1
	// FlagSnapshot says a MSG payload is a complete state rather than a delta,
	// which clears the receiver's gap condition.
	FlagSnapshot Flags = 1 << 2
	// FlagHasDigest says a FILE_NOTIFY carries a length and checksum the peer
	// can verify exactly, instead of having to stabilize-poll.
	FlagHasDigest Flags = 1 << 3
)

// Has reports whether every bit of f is set in fs.
func (fs Flags) Has(f Flags) bool { return fs&f == f }

// Protocol size budgets.
//
// MaxFrameCeiling is under the guest's 4 KiB string scratch region on purpose.
// An inbound payload larger than what is left of that region falls back to
// fk_alloc, and while the outermost dispatch now brackets the marshalling arena
// so that is no longer a permanent leak, it is still a per-packet allocation
// and a memcpy, and the arena keeps its chunks as capacity once taken -- so the
// peak frame size sets a floor on guest memory, which is in the save. A frame
// that fits the scratch touches none of it.
//
// It is also under every wall the probe found, and the INBOUND one is the
// binding constraint rather than the OS. Outbound reaches 9,188 B on macOS
// (net.inet.udp.maxdgram - 28); inbound on 2.1.14 delivers 4,000 B byte-exact
// and silently delivers nothing at 8,192, so the real ceiling is somewhere in
// between and 3900 clears it. Far more important than either number: AN
// OVERSIZED send_udp FAILS SILENTLY -- no error, no raise, nothing on the wire,
// and the same is true of an oversized datagram arriving. The transport will
// not tell a guest it went too far, so the cap has to be enforced here.
const (
	MaxFrameCeiling = 3900
	DefaultMaxFrame = 2048
	MaxFragments    = 16

	// MinMaxFrame is the smallest useful negotiated frame. A peer advertising
	// less than a header plus a token payload is either confused or hostile,
	// and clamping is kinder than fragmenting a heartbeat.
	MinMaxFrame = HeaderBytes + 64

	// MaxPayload is what the Length field can express, independently of any
	// negotiated cap.
	MaxPayload = 65535
)

// The decode failures, each separately counted by a session because they mean
// different things: junk on a shared port, a peer speaking a format we do not,
// and a datagram that was cut.
var (
	ErrShort    = errors.New("fkipc/wire: datagram shorter than a header")
	ErrMagic    = errors.New("fkipc/wire: not an fkipc frame")
	ErrVersion  = errors.New("fkipc/wire: unsupported protocol version")
	ErrType     = errors.New("fkipc/wire: unknown frame type")
	ErrLength   = errors.New("fkipc/wire: length disagrees with the datagram")
	ErrFragment = errors.New("fkipc/wire: impossible fragment index or count")
	ErrTooLong  = errors.New("fkipc/wire: payload longer than the length field")
)

// Header is one frame's fixed part, decoded.
type Header struct {
	Type    Type
	Flags   Flags
	Channel uint16
	Epoch   uint32
	Seq     uint32
	Corr    uint32
	Length  uint16
	Frag    uint8
	NFrag   uint8
}

// AppendFrame appends one complete frame to dst and returns the extended slice.
//
// Length is taken from payload and whatever the caller left in h.Length is
// overwritten -- the field exists so a RECEIVER can check the datagram, and
// letting a sender disagree with itself would only ever produce frames the
// other end drops. Magic and Version are written here for the same reason.
//
// A caller that reuses one buffer -- dst[:0] every time -- allocates nothing
// after the first frame, which is the whole reason this appends rather than
// returning a fresh slice.
func AppendFrame(dst []byte, h Header, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return dst, ErrTooLong
	}
	if h.NFrag == 0 {
		h.NFrag = 1
	}
	if h.Frag >= h.NFrag {
		return dst, ErrFragment
	}
	var hdr [HeaderBytes]byte
	putU16(hdr[offMagic:], Magic)
	hdr[offVersion] = Version
	hdr[offType] = byte(h.Type)
	putU16(hdr[offFlags:], uint16(h.Flags))
	putU16(hdr[offChannel:], h.Channel)
	putU32(hdr[offEpoch:], h.Epoch)
	putU32(hdr[offSeq:], h.Seq)
	putU32(hdr[offCorr:], h.Corr)
	putU16(hdr[offLength:], uint16(len(payload)))
	hdr[offFrag] = h.Frag
	hdr[offNFrag] = h.NFrag
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst, nil
}

// Decode reads one datagram as one frame.
//
// ONE DATAGRAM IS ONE FRAME. Frames are never split across datagrams and a
// datagram never carries two, so there is no offset to return and no resume
// point: UDP delivers a datagram whole or not at all. Anything above that is
// the message layer, which fragments MESSAGES into frames and is not this
// package's problem.
//
// The returned payload ALIASES dg. A caller that keeps it past the buffer's
// next use must copy it, which is the same rule the guest's own handlers
// impose on the application for the same reason.
func Decode(dg []byte) (Header, []byte, error) {
	var h Header
	if len(dg) < HeaderBytes {
		return h, nil, ErrShort
	}
	if u16(dg[offMagic:]) != Magic {
		return h, nil, ErrMagic
	}
	if dg[offVersion] != Version {
		return h, nil, ErrVersion
	}
	h.Type = Type(dg[offType])
	if !h.Type.Known() {
		return h, nil, ErrType
	}
	h.Flags = Flags(u16(dg[offFlags:]))
	h.Channel = u16(dg[offChannel:])
	h.Epoch = u32(dg[offEpoch:])
	h.Seq = u32(dg[offSeq:])
	h.Corr = u32(dg[offCorr:])
	h.Length = u16(dg[offLength:])
	h.Frag = dg[offFrag]
	h.NFrag = dg[offNFrag]
	// THE ABSOLUTE RULE. A length that disagrees with the datagram means the
	// frame was truncated, two were coalesced, or the peer is speaking
	// something else -- and none of those is a frame to act on.
	if int(h.Length) != len(dg)-HeaderBytes {
		return Header{}, nil, ErrLength
	}
	if h.NFrag == 0 || h.Frag >= h.NFrag {
		return Header{}, nil, ErrFragment
	}
	return h, dg[HeaderBytes:], nil
}

// SerialDelta is RFC-1982-style serial arithmetic over the per-channel seq.
//
// It is a named function rather than an inlined int32(a-b) in each
// implementation because this is the one comparison two implementations
// silently disagree about, and a disagreement here does not fail -- it delivers
// or drops the wrong frames forever. The caller's rule:
//
//	d > 1   a gap: deliver, raise the gap, advance
//	d == 1  in order: deliver, advance
//	d <= 0  old: DROP
//
// The wrap is a non-event by construction: at one frame per tick a channel
// wraps after about two thousand years, and the comparison would be right
// anyway.
func SerialDelta(seq, last uint32) int32 { return int32(seq - last) }

// FNV1a32 is the digest a FILE_NOTIFY carries.
//
// FNV-1a over the guest's own bytes: it needs no table, no allocation and no
// host call, and the peer's test is exact rather than a stabilize-poll. It is
// not a security property and is not claimed as one -- the transport is
// localhost.
func FNV1a32(b []byte) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(b); i++ {
		h ^= uint32(b[i])
		h *= prime32
	}
	return h
}

// Byte-wise little-endian, hand-rolled.
//
// Not encoding/binary: binary.Read is reflection, and binary.LittleEndian.Uint32
// would be fine here but the guest half of this repo writes its own for the
// same reason everywhere else -- the emitted Lua for a shift-and-or is
// something this project can read.

func u16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func u32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func putU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
