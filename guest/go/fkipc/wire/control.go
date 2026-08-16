package wire

import "errors"

// The control payloads: HELLO/HELLO_ACK, HEARTBEAT, FILE_NOTIFY and the RESP
// error record.
//
// These have a defined shape because both ends must agree on them and there is
// nothing app-shaped about them. Everything else -- MSG, REQ, RESP results --
// stays opaque.
//
// Each decoder either returns a complete value or an error. A short control
// payload is a dropped frame, not a value with zeros in the fields that were
// missing: the whole reason HELLO carries max_frame is that the sender obeys
// it, and a max_frame of 0 read out of a truncated datagram would be worse than
// no HELLO at all.

var ErrControl = errors.New("fkipc/wire: malformed control payload")

// Profile is which side of the two driving shapes a peer is.
//
// It rides in HELLO because the peer's sensible defaults differ -- a headless
// server's inbound budget is ~6 kB/s once anyone is connected and a single
// player client's is not -- and because a log that says which one it is talking
// to is worth one byte.
type Profile uint8

const (
	ProfileServer Profile = 0
	ProfileClient Profile = 1
)

// Hello is the payload of both HELLO and HELLO_ACK.
//
// One struct for both directions because the fields mean the same thing from
// either end, with two exceptions the field comments name: the peer sends
// Boot = 0 (it has no save to time-travel with) and Tick as the last tick it
// saw rather than the current one.
type Hello struct {
	ProtoMin, ProtoMax uint8
	// MaxFrame is the largest frame this side will ACCEPT, not the largest it
	// will send. The sender respects the peer's number. It is negotiated rather
	// than constant because it is a budget shared with the application: the
	// guest's string scratch region is reset once per outermost dispatch, so an
	// inbound payload holds its own length for the whole handler, and a guest
	// that reads entity names from inside a message handler wants a smaller
	// frame than one that only decodes.
	MaxFrame uint16
	// MaxFragments is the most fragments this side will reassemble.
	MaxFragments uint16
	// Boot is the guest's load counter, and 0 from the peer. It is
	// best-effort and monotone WITHIN a timeline only: two loads of one save
	// produce the same value, by construction, which is exactly why it cannot
	// be a session id and why the peer mints the epoch instead.
	Boot uint32
	// Tick is the guest's current tick, or the last tick the peer saw. It is
	// what reconciles the guest's tick-based timers with the peer's real clock.
	Tick    uint32
	Profile Profile
	// Name is the mod name, for the peer's logs and for multiplexing.
	Name string
}

const helloFixed = 18

// AppendHello writes a HELLO/HELLO_ACK payload.
func AppendHello(dst []byte, h Hello) ([]byte, error) {
	if len(h.Name) > MaxPayload-helloFixed {
		return dst, ErrTooLong
	}
	var b [helloFixed]byte
	b[0] = h.ProtoMin
	b[1] = h.ProtoMax
	putU16(b[2:], h.MaxFrame)
	putU16(b[4:], h.MaxFragments)
	putU32(b[6:], h.Boot)
	putU32(b[10:], h.Tick)
	b[14] = byte(h.Profile)
	b[15] = 0 // reserved
	putU16(b[16:], uint16(len(h.Name)))
	dst = append(dst, b[:]...)
	return append(dst, h.Name...), nil
}

// DecodeHello reads a HELLO/HELLO_ACK payload.
//
// The name is COPIED into a Go string rather than aliasing p, because a HELLO
// is kept for the life of the session while p is the receive buffer.
func DecodeHello(p []byte) (Hello, error) {
	var h Hello
	if len(p) < helloFixed {
		return h, ErrControl
	}
	h.ProtoMin = p[0]
	h.ProtoMax = p[1]
	h.MaxFrame = u16(p[2:])
	h.MaxFragments = u16(p[4:])
	h.Boot = u32(p[6:])
	h.Tick = u32(p[10:])
	h.Profile = Profile(p[14])
	n := int(u16(p[16:]))
	if len(p) < helloFixed+n {
		return Hello{}, ErrControl
	}
	h.Name = string(p[helloFixed : helloFixed+n])
	return h, nil
}

// Heartbeat is the payload of HEARTBEAT.
//
// THIS IS FLOW CONTROL, NOT TELEMETRY. The counters give the peer a real rate
// to aim at once per second of game time, and the guest's SILENCE is the
// signal that matters: the peer has a clock and the guest's heartbeats stop
// when the game does, so a peer that has heard nothing for its own quiet
// threshold stops sending everything but its own heartbeat. That, and not a
// bigger OS buffer, is what keeps a long pause or a slow save from dropping
// frames -- the buffer is 256 KB and overflows silently.
type Heartbeat struct {
	Tick  uint32
	Rx    uint32 // frames accepted since the last heartbeat
	Drops uint32 // frames dropped since the last heartbeat
	Gaps  uint32 // gaps observed since the last heartbeat
}

const heartbeatBytes = 16

func AppendHeartbeat(dst []byte, h Heartbeat) []byte {
	var b [heartbeatBytes]byte
	putU32(b[0:], h.Tick)
	putU32(b[4:], h.Rx)
	putU32(b[8:], h.Drops)
	putU32(b[12:], h.Gaps)
	return append(dst, b[:]...)
}

func DecodeHeartbeat(p []byte) (Heartbeat, error) {
	if len(p) < heartbeatBytes {
		return Heartbeat{}, ErrControl
	}
	return Heartbeat{
		Tick:  u32(p[0:]),
		Rx:    u32(p[4:]),
		Drops: u32(p[8:]),
		Gaps:  u32(p[12:]),
	}, nil
}

// FileNotify is the payload of FILE_NOTIFY: "there is a file at X".
//
// Bytes and FNV1a32 are meaningful only when the frame carries FlagHasDigest,
// and the distinction is the whole design. A file the GUEST wrote it also
// hashed, so the peer's test is exact -- read until Bytes and the checksum
// matches, or keep waiting. A file the ENGINE wrote (a screenshot) the guest
// has never held and cannot describe, so the digest is absent and the peer
// falls back to stabilize-polling. Nothing documents a flush guarantee for
// write_file, which is why this is a test rather than a promise.
type FileNotify struct {
	Bytes   uint32
	FNV1a32 uint32
	Name    string
}

const fileNotifyFixed = 10

func AppendFileNotify(dst []byte, f FileNotify) ([]byte, error) {
	if len(f.Name) > MaxPayload-fileNotifyFixed {
		return dst, ErrTooLong
	}
	var b [fileNotifyFixed]byte
	putU32(b[0:], f.Bytes)
	putU32(b[4:], f.FNV1a32)
	putU16(b[8:], uint16(len(f.Name)))
	dst = append(dst, b[:]...)
	return append(dst, f.Name...), nil
}

func DecodeFileNotify(p []byte) (FileNotify, error) {
	if len(p) < fileNotifyFixed {
		return FileNotify{}, ErrControl
	}
	n := int(u16(p[8:]))
	if len(p) < fileNotifyFixed+n {
		return FileNotify{}, ErrControl
	}
	return FileNotify{
		Bytes:   u32(p[0:]),
		FNV1a32: u32(p[4:]),
		Name:    string(p[fileNotifyFixed : fileNotifyFixed+n]),
	}, nil
}

// The RESP error codes.
//
// CodeDuplicate is the interesting one: it is the answer to a retried REQ whose
// response was too large to cache. The application learns that the operation
// EXECUTED and the result is gone, which is strictly better than the two
// alternatives -- silently re-executing it, or growing the save without bound
// to hold every reply. A handler with a large result should write a file and
// answer with a FILE_NOTIFY, which is the right shape for a large result
// anyway.
const (
	CodeNoHandler uint16 = 1
	CodeBadFrame  uint16 = 2
	CodeDuplicate uint16 = 3
	CodeBusy      uint16 = 4
	CodeApp       uint16 = 5 // after which the rest of the payload is the app's own
)

// ErrorRecord is the payload of a RESP carrying FlagError.
type ErrorRecord struct {
	Code    uint16
	Message string // UTF-8, to the end of the payload
}

func AppendErrorRecord(dst []byte, e ErrorRecord) ([]byte, error) {
	if len(e.Message) > MaxPayload-2 {
		return dst, ErrTooLong
	}
	var b [2]byte
	putU16(b[0:], e.Code)
	dst = append(dst, b[:]...)
	return append(dst, e.Message...), nil
}

func DecodeErrorRecord(p []byte) (ErrorRecord, error) {
	if len(p) < 2 {
		return ErrorRecord{}, ErrControl
	}
	return ErrorRecord{Code: u16(p[0:]), Message: string(p[2:])}, nil
}

func (c Profile) String() string {
	if c == ProfileClient {
		return "client"
	}
	return "server"
}
