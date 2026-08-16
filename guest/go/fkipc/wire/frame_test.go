package wire_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

// Every type and every flag survives a round trip, with the payload intact.
//
// The payload here is all 256 byte values because that is the property the
// probe measured on the real transport in both directions -- NUL does not
// truncate, high bytes are not UTF-8-mangled -- and a codec that could not
// carry them would make the measurement moot.
func TestEveryTypeAndFlagRoundTrips(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	types := []wire.Type{
		wire.TypeHello, wire.TypeHelloAck, wire.TypeHeartbeat, wire.TypeMsg,
		wire.TypeReq, wire.TypeResp, wire.TypeFileNotify, wire.TypeResync,
		wire.TypeBye,
	}
	flags := []wire.Flags{
		0, wire.FlagRetry, wire.FlagError, wire.FlagSnapshot, wire.FlagHasDigest,
		wire.FlagRetry | wire.FlagError | wire.FlagSnapshot | wire.FlagHasDigest,
	}
	var buf []byte
	for _, ty := range types {
		for _, fl := range flags {
			in := wire.Header{
				Type: ty, Flags: fl, Channel: 0xBEEF, Epoch: 0x11223344,
				Seq: math.MaxUint32, Corr: 0xDEADBEEF, Frag: 3, NFrag: 9,
			}
			var err error
			buf, err = wire.AppendFrame(buf[:0], in, payload)
			if err != nil {
				t.Fatalf("%v/%v: %v", ty, fl, err)
			}
			if len(buf) != wire.HeaderBytes+len(payload) {
				t.Fatalf("%v: frame is %d bytes, want %d", ty, len(buf),
					wire.HeaderBytes+len(payload))
			}
			got, p, err := wire.Decode(buf)
			if err != nil {
				t.Fatalf("%v/%v: %v", ty, fl, err)
			}
			in.Length = uint16(len(payload))
			if got != in {
				t.Errorf("%v/%v: header round trip\n got %+v\nwant %+v", ty, fl, got, in)
			}
			if !bytes.Equal(p, payload) {
				t.Errorf("%v/%v: payload did not survive", ty, fl)
			}
		}
	}
}

// A zero-length payload is a frame, not a degenerate case: RESYNC and BYE have
// no payload at all and a receiver must not read the length as "unset".
func TestAZeroLengthPayloadIsAFrame(t *testing.T) {
	buf, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeResync, Channel: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, p, err := wire.Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Length != 0 || len(p) != 0 || h.Channel != 7 || h.NFrag != 1 {
		t.Errorf("got %+v payload %d", h, len(p))
	}
}

// TRUNCATION AT EVERY BOUNDARY BYTE.
//
// A datagram cut anywhere must be rejected, and the two ways it can be cut are
// different failures: inside the header there is nothing to read, and after it
// the length field is what catches the loss. The second is the whole reason
// length is carried on a transport that already knows it.
func TestTruncationAtEveryByteIsRejected(t *testing.T) {
	full, err := wire.AppendFrame(nil,
		wire.Header{Type: wire.TypeMsg, Channel: 1, Epoch: 5, Seq: 9},
		[]byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(full); n++ {
		_, _, err := wire.Decode(full[:n])
		if err == nil {
			t.Fatalf("a %d-byte prefix of a %d-byte frame decoded", n, len(full))
		}
		want := wire.ErrLength
		if n < wire.HeaderBytes {
			want = wire.ErrShort
		}
		if err != want {
			t.Errorf("prefix %d: got %v, want %v", n, err, want)
		}
	}
	if _, _, err := wire.Decode(full); err != nil {
		t.Fatalf("the untruncated frame: %v", err)
	}
}

// A datagram LONGER than its length field is rejected too, which is the
// coalescing case: two frames in one datagram, or a peer that wrote a stale
// length. It is the same test and the same rule, from the other side.
func TestAnOverlongDatagramIsRejected(t *testing.T) {
	full, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeMsg}, []byte("ab"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := wire.Decode(append(full, 'x')); err != wire.ErrLength {
		t.Errorf("got %v, want ErrLength", err)
	}
	// ...and the same bytes with the length field lying, low as well as high.
	full[20] = 1
	if _, _, err := wire.Decode(full); err != wire.ErrLength {
		t.Errorf("short length: got %v, want ErrLength", err)
	}
}

// Junk on a shared local port is what the magic is for, and a version we do not
// speak is a separate answer because a session logs it once rather than
// counting it per frame.
func TestJunkAndAnUnknownVersionAreDistinguished(t *testing.T) {
	good, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeMsg}, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}

	junk := append([]byte(nil), good...)
	junk[0] = 'X'
	if _, _, err := wire.Decode(junk); err != wire.ErrMagic {
		t.Errorf("bad magic: got %v", err)
	}

	future := append([]byte(nil), good...)
	future[2] = wire.Version + 1
	if _, _, err := wire.Decode(future); err != wire.ErrVersion {
		t.Errorf("future version: got %v", err)
	}

	// The version is checked BEFORE the type, so a v2 frame using a type this
	// version has never heard of reports the version -- which is the useful
	// message, because the type is only unknown as a consequence.
	future[3] = 0x40
	if _, _, err := wire.Decode(future); err != wire.ErrVersion {
		t.Errorf("future version with a future type: got %v", err)
	}
}

// An unknown type is dropped and counted, never guessed at.
func TestAnUnknownTypeIsRejected(t *testing.T) {
	good, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeMsg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range []byte{0x00, 0x0A, 0x7F, 0xFF} {
		bad := append([]byte(nil), good...)
		bad[3] = ty
		if _, _, err := wire.Decode(bad); err != wire.ErrType {
			t.Errorf("type 0x%02x: got %v, want ErrType", ty, err)
		}
	}
}

// Unknown FLAG bits are ignored, which is the opposite rule and is deliberate:
// a flag refines a frame the receiver already understands.
func TestUnknownFlagBitsAreIgnored(t *testing.T) {
	buf, err := wire.AppendFrame(nil,
		wire.Header{Type: wire.TypeMsg, Flags: wire.FlagSnapshot | 0x8000}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := wire.Decode(buf)
	if err != nil {
		t.Fatalf("a reserved flag bit made a frame undecodable: %v", err)
	}
	if !h.Flags.Has(wire.FlagSnapshot) {
		t.Error("the known bit was lost")
	}
}

// An impossible fragment description is structurally undecodable: nfrag 0 says
// there is no message, and frag >= nfrag names a piece outside it. Either one
// would index a reassembly buffer out of range in a caller that trusted the
// header, which is exactly what "no partial parse" means.
func TestImpossibleFragmentsAreRejected(t *testing.T) {
	good, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeMsg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ frag, nfrag byte }{{0, 0}, {1, 1}, {5, 3}, {255, 0}} {
		bad := append([]byte(nil), good...)
		bad[22], bad[23] = c.frag, c.nfrag
		if _, _, err := wire.Decode(bad); err != wire.ErrFragment {
			t.Errorf("frag %d of %d: got %v, want ErrFragment", c.frag, c.nfrag, err)
		}
	}
	if _, err := wire.AppendFrame(nil,
		wire.Header{Type: wire.TypeMsg, Frag: 4, NFrag: 4}, nil); err != wire.ErrFragment {
		t.Error("the encoder accepted frag == nfrag")
	}
}

// AppendFrame owns Length and Magic and Version. A caller that filled Length in
// itself and got it wrong would emit frames the far end drops for the rest of
// the session, and nothing would say so.
func TestTheEncoderOwnsLengthMagicAndVersion(t *testing.T) {
	buf, err := wire.AppendFrame(nil,
		wire.Header{Type: wire.TypeMsg, Length: 9999}, []byte("four"))
	if err != nil {
		t.Fatal(err)
	}
	h, p, err := wire.Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Length != 4 || string(p) != "four" {
		t.Errorf("got length %d payload %q", h.Length, p)
	}
	if buf[0] != 'F' || buf[1] != 'K' || buf[2] != wire.Version {
		t.Errorf("magic/version: % x", buf[:3])
	}
}

// Appending into a reused buffer is what the guest does on every send, and it
// must not depend on the buffer being empty or on the previous frame's size.
func TestAppendingIntoAReusedBufferIsClean(t *testing.T) {
	var buf []byte
	sizes := []int{1000, 4, 700, 0, 3}
	for _, n := range sizes {
		payload := bytes.Repeat([]byte{byte(n)}, n)
		var err error
		buf, err = wire.AppendFrame(buf[:0], wire.Header{Type: wire.TypeMsg}, payload)
		if err != nil {
			t.Fatal(err)
		}
		h, p, err := wire.Decode(buf)
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if int(h.Length) != n || !bytes.Equal(p, payload) {
			t.Errorf("size %d: length %d, %d payload bytes", n, h.Length, len(p))
		}
	}
}

// RFC-1982 serial arithmetic, including the wrap the u32 seq will never
// actually reach. Both arms, because a receiver that got the drop arm wrong
// would deliver stale state forever and a receiver that got the gap arm wrong
// would never resync.
func TestSerialDeltaAtTheWrap(t *testing.T) {
	cases := []struct {
		seq, last uint32
		want      int32
	}{
		{1, 0, 1},
		{2, 1, 1},
		{5, 1, 4},
		{1, 5, -4},
		{1, 1, 0},
		{0, math.MaxUint32, 1},                   // the wrap, in order
		{2, math.MaxUint32, 3},                   // the wrap, with a gap
		{math.MaxUint32, 0, -1},                  // the wrap, backwards
		{math.MaxUint32 - 1, math.MaxUint32, -1}, //
		{0x80000000, 0, math.MinInt32},           // the antipode, which is "old"
	}
	for _, c := range cases {
		if got := wire.SerialDelta(c.seq, c.last); got != c.want {
			t.Errorf("SerialDelta(%d, %d) = %d, want %d", c.seq, c.last, got, c.want)
		}
	}
}

// The digest is a fixed function and both ends compute it, so it is pinned
// against the published FNV-1a-32 vectors rather than against itself.
func TestTheDigestIsFNV1a32(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"", 2166136261},
		{"a", 0xe40c292c},
		{"foobar", 0xbf9cf968},
	}
	for _, c := range cases {
		if got := wire.FNV1a32([]byte(c.in)); got != c.want {
			t.Errorf("FNV1a32(%q) = %#x, want %#x", c.in, got, c.want)
		}
	}
}
