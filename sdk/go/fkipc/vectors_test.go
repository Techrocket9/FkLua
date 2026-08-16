package fkipc_test

// THE COMMITTED WIRE VECTORS, read by the GO codec.
//
// testdata/ipc/wire-vectors.txt is the shared artifact between two hand-written
// implementations of one wire format. guest/rust/fkipc has read it since it was
// written (tests/vectors.rs); the Go side did not, which meant the file pinned
// the Rust codec against bytes the Go codec had produced ONCE and could drift
// from afterwards. Every offset in it was Go's opinion, and nothing asked Go
// again.
//
// This is that reader, and it lives here rather than under internal/ for a
// module reason: the producer must import guest/go/fkipc/wire, the root module
// does not require guest/go, and this module already does.
//
// Three assertions, and the second is the one that matters:
//
//   - every committed frame DECODES to the header and payload recorded beside
//     it, and every control payload decodes to its recorded fields;
//   - every committed frame RE-ENCODES byte for byte. A decoder that read a
//     field from the wrong offset would pass the first as long as it wrote it
//     back to the same wrong one; this is the half that says the offsets are
//     the ones on the wire;
//   - the file still holds every frame type and every flag bit this version
//     defines, so a type nobody wrote a vector for is a visible gap.
//
// REGENERATING: `go test ./fkipc -run TestTheCommittedWireVectors -update`,
// from sdk/go. It rewrites the file from the DECLARED lines of each record --
// the header fields, the opaque payload, the control kind and its fields --
// recomputing the two DERIVED ones, `frame` always and a control record's
// `payload`. So a codec change that moves the bytes shows up as a diff in the
// golden, which is the review artifact, exactly as it is for the emitter's own
// goldens. Adding a vector means writing its declared lines and running that.
//
// What it cannot catch, stated rather than implied: a change made to both the
// codec and this file in one commit. That is what the diff is for.

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
)

var updateVectors = flag.Bool("update", false,
	"rewrite testdata/ipc/wire-vectors.txt from the Go codec")

// The header comment the file carries, rewritten with it.
//
// It is here rather than preserved from the file because -update must be able
// to produce the whole artifact: a regeneration that copied an unread prefix
// would let the prose and the bytes disagree, which is the drift this file
// exists to prevent one level down.
const vectorsHeader = `# fkipc wire vectors -- ONE FORMAT, TWO CODECS, PINNED BY DATA.
#
# guest/go/fkipc/wire produced every frame below. Both codecs read this file --
# sdk/go/fkipc/vectors_test.go on the Go side, guest/rust/fkipc/tests/vectors.rs
# on the Rust one -- and each must decode every field out of it and re-encode
# the identical bytes. internal/guest's parity gate additionally injects a
# selection of these frames into BOTH compiled example guests and requires the
# same reaction.
#
# It exists because two implementations of one wire format is the AD5 shape --
# the same defect in the same function, fixed on one backend and left standing
# on the other for two milestones because the test was written against one --
# and the only cure that does not depend on somebody remembering is to make the
# BYTES the shared artifact rather than the parallel authorship.
#
# A record is a run of "key value" lines ended by a blank line. Lines beginning
# with '#' are comments. Hex is lower-case and unspaced; an empty payload is an
# empty value. "control" names a protocol-defined payload and the "field" lines
# after it are what a decoder must read back out of it.
#
# REGENERATING: this is a golden file, and its generator is the Go reader --
# ` + "`go test ./fkipc -run TestTheCommittedWireVectors -update`" + `, from sdk/go.
# It rewrites every record from its DECLARED lines, recomputing the two derived
# ones: ` + "`frame`" + ` always, and a control record's ` + "`payload`" + `. A codec change
# that moves these bytes is a wire-format change and the diff is the review
# artifact, exactly as it is for the emitter's own goldens.
`

type vector struct {
	name    string
	h       wire.Header
	payload []byte
	frame   []byte
	control string
	fields  [][2]string
	line    int
}

func vectorsPath() string {
	// sdk/go/fkipc -> the checkout root.
	return filepath.Join("..", "..", "..", "testdata", "ipc", "wire-vectors.txt")
}

// The committed frames decode to what is recorded beside them, and re-encode to
// the identical bytes.
func TestTheCommittedWireVectors(t *testing.T) {
	path := vectorsPath()
	vs := loadVectors(t, path)

	if *updateVectors {
		writeVectors(t, path, vs)
		t.Logf("rewrote %s: %d vectors", path, len(vs))
		return
	}

	var enc []byte
	for _, v := range vs {
		h, p, err := wire.Decode(v.frame)
		if err != nil {
			t.Errorf("%s (line %d): decode: %v", v.name, v.line, err)
			continue
		}
		want := v.h
		want.Length = uint16(len(v.payload))
		if h != want {
			t.Errorf("%s: header\n  got  %+v\n  want %+v", v.name, h, want)
		}
		if !bytes.Equal(p, v.payload) {
			t.Errorf("%s: payload\n  got  %x\n  want %x", v.name, p, v.payload)
		}
		enc, err = wire.AppendFrame(enc[:0], v.h, v.payload)
		if err != nil {
			t.Errorf("%s: re-encode: %v", v.name, err)
			continue
		}
		if !bytes.Equal(enc, v.frame) {
			t.Errorf("%s: re-encoded\n  got  %x\n  want %x", v.name, enc, v.frame)
		}
	}
	checkControlVectors(t, vs)
	checkVectorCoverage(t, vs)
}

// The protocol-defined payloads decode to the fields recorded beside them.
//
// A payload is opaque to the framing layer, so everything above would pass on a
// control codec that put max_frame where max_fragments goes.
func checkControlVectors(t *testing.T, vs []vector) {
	t.Helper()
	seen := 0
	for _, v := range vs {
		if v.control == "" {
			continue
		}
		seen++
		got, err := renderControl(v.control, v.payload)
		if err != nil {
			t.Errorf("%s: %v", v.name, err)
			continue
		}
		if len(got) != len(v.fields) {
			t.Errorf("%s: decoded %d fields, %d recorded", v.name, len(got), len(v.fields))
			continue
		}
		for i, f := range got {
			if f != v.fields[i] {
				t.Errorf("%s: field %d is %q %q, recorded as %q %q",
					v.name, i, f[0], f[1], v.fields[i][0], v.fields[i][1])
			}
		}
		// ...and the control payload re-encodes to the committed bytes, which is
		// the same argument as the frame's re-encode one level in.
		re, err := encodeControl(v.control, got)
		if err != nil {
			t.Errorf("%s: control re-encode: %v", v.name, err)
			continue
		}
		if !bytes.Equal(re, v.payload) {
			t.Errorf("%s: control re-encoded\n  got  %x\n  want %x", v.name, re, v.payload)
		}
	}
	if seen < 6 {
		t.Errorf("only %d control vectors -- the file lost some", seen)
	}
}

// Every frame type, every flag bit, a non-first fragment, and all 256 byte
// values, so a shape nobody wrote a vector for is a visible gap rather than a
// silent one. The Rust reader asserts the identical set.
func checkVectorCoverage(t *testing.T, vs []vector) {
	t.Helper()
	for _, ty := range []wire.Type{
		wire.TypeHello, wire.TypeHelloAck, wire.TypeHeartbeat, wire.TypeMsg,
		wire.TypeReq, wire.TypeResp, wire.TypeFileNotify, wire.TypeResync,
		wire.TypeBye,
	} {
		if !anyVector(vs, func(v vector) bool { return v.h.Type == ty }) {
			t.Errorf("no committed vector carries a %s", ty)
		}
	}
	for _, fl := range []wire.Flags{
		wire.FlagRetry, wire.FlagError, wire.FlagSnapshot, wire.FlagHasDigest,
	} {
		if !anyVector(vs, func(v vector) bool { return v.h.Flags.Has(fl) }) {
			t.Errorf("no committed vector carries flag %#x", uint16(fl))
		}
	}
	if !anyVector(vs, func(v vector) bool { return v.h.NFrag > 1 && v.h.Frag > 0 }) {
		t.Error("no committed vector is a non-first fragment, which is where a " +
			"frag/nfrag byte swap hides")
	}
	if !anyVector(vs, func(v vector) bool {
		if len(v.payload) != 256 {
			return false
		}
		for i, b := range v.payload {
			if b != byte(i) {
				return false
			}
		}
		return true
	}) {
		t.Error("no committed vector carries all 256 byte values, which is the " +
			"property the probe measured on the real transport")
	}
}

func anyVector(vs []vector, pred func(vector) bool) bool {
	for _, v := range vs {
		if pred(v) {
			return true
		}
	}
	return false
}

// renderControl decodes a control payload and returns its fields in the order
// the file records them. It is the reader AND the -update writer, so the two
// cannot disagree about a field's name or its rendering.
func renderControl(kind string, p []byte) ([][2]string, error) {
	switch kind {
	case "hello":
		h, err := wire.DecodeHello(p)
		if err != nil {
			return nil, err
		}
		return [][2]string{
			{"proto_min", u(uint64(h.ProtoMin))},
			{"proto_max", u(uint64(h.ProtoMax))},
			{"max_frame", u(uint64(h.MaxFrame))},
			{"max_fragments", u(uint64(h.MaxFragments))},
			{"boot", u(uint64(h.Boot))},
			{"tick", u(uint64(h.Tick))},
			{"profile", u(uint64(h.Profile))},
			{"name", h.Name},
		}, nil
	case "heartbeat":
		hb, err := wire.DecodeHeartbeat(p)
		if err != nil {
			return nil, err
		}
		return [][2]string{
			{"tick", u(uint64(hb.Tick))},
			{"rx", u(uint64(hb.Rx))},
			{"drops", u(uint64(hb.Drops))},
			{"gaps", u(uint64(hb.Gaps))},
		}, nil
	case "filenotify":
		f, err := wire.DecodeFileNotify(p)
		if err != nil {
			return nil, err
		}
		return [][2]string{
			{"bytes", u(uint64(f.Bytes))},
			{"fnv1a32", u(uint64(f.FNV1a32))},
			{"name", f.Name},
		}, nil
	case "error":
		e, err := wire.DecodeErrorRecord(p)
		if err != nil {
			return nil, err
		}
		return [][2]string{
			{"code", u(uint64(e.Code))},
			{"message", e.Message},
		}, nil
	}
	return nil, fmt.Errorf("unknown control kind %q", kind)
}

// encodeControl is renderControl's inverse, over the same field names.
func encodeControl(kind string, fields [][2]string) ([]byte, error) {
	get := func(k string) string {
		for _, f := range fields {
			if f[0] == k {
				return f[1]
			}
		}
		return ""
	}
	n := func(k string) uint64 {
		v, _ := strconv.ParseUint(get(k), 10, 64)
		return v
	}
	switch kind {
	case "hello":
		return wire.AppendHello(nil, wire.Hello{
			ProtoMin: uint8(n("proto_min")), ProtoMax: uint8(n("proto_max")),
			MaxFrame: uint16(n("max_frame")), MaxFragments: uint16(n("max_fragments")),
			Boot: uint32(n("boot")), Tick: uint32(n("tick")),
			Profile: wire.Profile(n("profile")), Name: get("name"),
		})
	case "heartbeat":
		return wire.AppendHeartbeat(nil, wire.Heartbeat{
			Tick: uint32(n("tick")), Rx: uint32(n("rx")),
			Drops: uint32(n("drops")), Gaps: uint32(n("gaps")),
		}), nil
	case "filenotify":
		return wire.AppendFileNotify(nil, wire.FileNotify{
			Bytes: uint32(n("bytes")), FNV1a32: uint32(n("fnv1a32")), Name: get("name"),
		})
	case "error":
		return wire.AppendErrorRecord(nil, wire.ErrorRecord{
			Code: uint16(n("code")), Message: get("message"),
		})
	}
	return nil, fmt.Errorf("unknown control kind %q", kind)
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }

// loadVectors parses the file. It knows the format and nothing about the codec,
// which is what makes it a reader of the committed BYTES rather than a second
// opinion about them.
func loadVectors(t *testing.T, path string) []vector {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the committed wire vectors: %v", err)
	}
	var out []vector
	var cur *vector
	for i, line := range strings.Split(string(raw), "\n") {
		n := i + 1
		line = strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == "" {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		if key == "name" {
			if cur != nil {
				t.Fatalf("line %d: a record without a blank line before it", n)
			}
			cur = &vector{name: val, line: n}
			continue
		}
		if cur == nil {
			t.Fatalf("line %d: %q before any name", n, key)
		}
		num := func() uint64 {
			v, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				t.Fatalf("line %d: %q: %v", n, val, err)
			}
			return v
		}
		unhex := func() []byte {
			b, err := hex.DecodeString(val)
			if err != nil {
				t.Fatalf("line %d: %v", n, err)
			}
			return b
		}
		switch key {
		case "type":
			cur.h.Type = wire.Type(num())
		case "flags":
			cur.h.Flags = wire.Flags(num())
		case "channel":
			cur.h.Channel = uint16(num())
		case "epoch":
			cur.h.Epoch = uint32(num())
		case "seq":
			cur.h.Seq = uint32(num())
		case "corr":
			cur.h.Corr = uint32(num())
		case "frag":
			cur.h.Frag = uint8(num())
		case "nfrag":
			cur.h.NFrag = uint8(num())
		case "payload":
			cur.payload = unhex()
		case "frame":
			cur.frame = unhex()
		case "control":
			cur.control = val
		case "field":
			k, v, _ := strings.Cut(val, " ")
			cur.fields = append(cur.fields, [2]string{k, v})
		default:
			t.Fatalf("line %d: unknown key %q", n, key)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	if len(out) < 10 {
		t.Fatalf("%s held %d vectors", path, len(out))
	}
	return out
}

// writeVectors regenerates the file: every declared line as it was read, and
// the derived ones -- a control record's payload, and every record's frame --
// recomputed through the Go codec.
func writeVectors(t *testing.T, path string, vs []vector) {
	t.Helper()
	var b strings.Builder
	b.WriteString(vectorsHeader)
	for _, v := range vs {
		payload := v.payload
		fields := v.fields
		if v.control != "" {
			p, err := encodeControl(v.control, v.fields)
			if err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			payload = p
			// ...and the fields are re-rendered from the encoded bytes, so a
			// regenerated file records what a DECODER sees rather than what the
			// previous file said.
			if fields, err = renderControl(v.control, payload); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
		}
		frame, err := wire.AppendFrame(nil, v.h, payload)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		fmt.Fprintf(&b, "\nname %s\n", v.name)
		fmt.Fprintf(&b, "type %d\n", v.h.Type)
		fmt.Fprintf(&b, "flags %d\n", v.h.Flags)
		fmt.Fprintf(&b, "channel %d\n", v.h.Channel)
		fmt.Fprintf(&b, "epoch %d\n", v.h.Epoch)
		fmt.Fprintf(&b, "seq %d\n", v.h.Seq)
		fmt.Fprintf(&b, "corr %d\n", v.h.Corr)
		fmt.Fprintf(&b, "frag %d\n", v.h.Frag)
		fmt.Fprintf(&b, "nfrag %d\n", v.h.NFrag)
		fmt.Fprintf(&b, "payload %s\n", hex.EncodeToString(payload))
		if v.control != "" {
			fmt.Fprintf(&b, "control %s\n", v.control)
			for _, f := range fields {
				fmt.Fprintf(&b, "field %s %s\n", f[0], f[1])
			}
		}
		fmt.Fprintf(&b, "frame %s\n", hex.EncodeToString(frame))
	}
	// The trailing blank line is the last record's terminator. Both parsers
	// close an open record at EOF anyway, so this is convention rather than
	// grammar -- but a regeneration that dropped it would put a spurious line in
	// every future diff of a file whose diff IS the review artifact.
	b.WriteString("\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
