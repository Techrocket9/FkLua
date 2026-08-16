package wire_test

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

func TestHelloRoundTrips(t *testing.T) {
	in := wire.Hello{
		ProtoMin: 1, ProtoMax: 1, MaxFrame: 2048, MaxFragments: 16,
		Boot: 0xCAFEF00D, Tick: 123456, Profile: wire.ProfileClient,
		Name: "fk-demo",
	}
	buf, err := wire.AppendHello(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.DecodeHello(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("\n got %+v\nwant %+v", got, in)
	}
	// An empty name is legal: the field is for the peer's logs.
	buf, err = wire.AppendHello(buf[:0], wire.Hello{ProtoMin: 1, ProtoMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wire.DecodeHello(buf); err != nil || got.Name != "" {
		t.Errorf("empty name: %+v %v", got, err)
	}
}

// A truncated control payload is a dropped frame, not a value with zeros where
// the missing bytes were. A MaxFrame of 0 read out of a short HELLO would be
// obeyed by the sender and the session would never carry a byte.
func TestATruncatedControlPayloadIsRejected(t *testing.T) {
	hello, err := wire.AppendHello(nil, wire.Hello{
		ProtoMin: 1, ProtoMax: 1, MaxFrame: 2048, MaxFragments: 16, Name: "abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(hello); n++ {
		if _, err := wire.DecodeHello(hello[:n]); err != wire.ErrControl {
			t.Errorf("hello prefix %d: got %v, want ErrControl", n, err)
		}
	}

	hb := wire.AppendHeartbeat(nil, wire.Heartbeat{Tick: 1, Rx: 2, Drops: 3, Gaps: 4})
	for n := 0; n < len(hb); n++ {
		if _, err := wire.DecodeHeartbeat(hb[:n]); err != wire.ErrControl {
			t.Errorf("heartbeat prefix %d: got %v", n, err)
		}
	}

	fn, err := wire.AppendFileNotify(nil,
		wire.FileNotify{Bytes: 10, FNV1a32: 11, Name: "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(fn); n++ {
		if _, err := wire.DecodeFileNotify(fn[:n]); err != wire.ErrControl {
			t.Errorf("file-notify prefix %d: got %v", n, err)
		}
	}

	if _, err := wire.DecodeErrorRecord([]byte{7}); err != wire.ErrControl {
		t.Errorf("one-byte error record: got %v", err)
	}
}

// A name_len that overruns the payload is the same failure from the other side:
// the fixed part is present, the variable part is not.
func TestAControlNameLengthCannotOverrun(t *testing.T) {
	hello, err := wire.AppendHello(nil, wire.Hello{ProtoMin: 1, ProtoMax: 1, Name: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	hello[16] = 200 // claim a 200-byte name after three bytes of it
	if _, err := wire.DecodeHello(hello); err != wire.ErrControl {
		t.Errorf("got %v, want ErrControl", err)
	}
}

func TestHeartbeatAndFileNotifyRoundTrip(t *testing.T) {
	hb := wire.Heartbeat{Tick: 4294967295, Rx: 1, Drops: 2, Gaps: 3}
	got, err := wire.DecodeHeartbeat(wire.AppendHeartbeat(nil, hb))
	if err != nil || got != hb {
		t.Errorf("heartbeat: %+v %v", got, err)
	}

	fn := wire.FileNotify{Bytes: 4096, FNV1a32: 0xDEADBEEF, Name: "fkipc/dump.bin"}
	buf, err := wire.AppendFileNotify(nil, fn)
	if err != nil {
		t.Fatal(err)
	}
	gotFN, err := wire.DecodeFileNotify(buf)
	if err != nil || gotFN != fn {
		t.Errorf("file notify: %+v %v", gotFN, err)
	}
}

// The error record's message runs to the end of the payload, so it needs no
// length of its own -- and an empty one is legal, because a code is sometimes
// the whole story.
func TestErrorRecordRunsToTheEnd(t *testing.T) {
	for _, msg := range []string{"", "no such handler", strings.Repeat("x", 1000)} {
		buf, err := wire.AppendErrorRecord(nil,
			wire.ErrorRecord{Code: wire.CodeApp, Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		got, err := wire.DecodeErrorRecord(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != wire.CodeApp || got.Message != msg {
			t.Errorf("got %+v", got)
		}
	}
}

// A control payload the caller cannot express is refused rather than silently
// truncated by the u16 length field.
func TestAnOverlongControlNameIsRefused(t *testing.T) {
	huge := strings.Repeat("n", wire.MaxPayload)
	if _, err := wire.AppendHello(nil, wire.Hello{Name: huge}); err != wire.ErrTooLong {
		t.Errorf("hello: got %v", err)
	}
	if _, err := wire.AppendFileNotify(nil, wire.FileNotify{Name: huge}); err != wire.ErrTooLong {
		t.Errorf("file notify: got %v", err)
	}
	if _, err := wire.AppendFrame(nil, wire.Header{Type: wire.TypeMsg},
		make([]byte, wire.MaxPayload+1)); err != wire.ErrTooLong {
		t.Errorf("frame: got %v", err)
	}
}
