package modsettings

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// THE RENDER ROUND TRIP, and it is deliberately six steps rather than two.
// parse, encode, decode, render, parse, encode: the second encoding has to be
// the first one byte for byte. Two steps would only say the renderer and the
// parser agree with each other; six says the JSON that comes back out of a
// decoded FILE describes that same file.
func TestRenderJSONRoundTripsThroughTheBytes(t *testing.T) {
	source := []byte(`{
	  "comment": "a comment reaches no byte, so it does not survive the render either",
	  "version": [2, 0, 77, 0],
	  "startup": {
	    "z-a-bool": true,
	    "a-false": false,
	    "an-int": 40,
	    "a-negative-int": -9007199254740993,
	    "a-double": 20.0,
	    "a-fractional-double": 12.5,
	    "a-tiny-double": 1e-7,
	    "a-huge-double": 1e21,
	    "a-negative-double": -0.5,
	    "a-string": "1 steel-plate, 10 water",
	    "an-empty-string": "",
	    "a-unicode-string": "unicode ✓ café",
	    "an-html-looking-string": "a < b && c > d"
	  },
	  "runtime-global": {"somebody": "else"},
	  "runtime-per-user": {}
	}`)

	first, err := ParseJSON(source)
	if err != nil {
		t.Fatalf("the source did not parse: %v", err)
	}
	firstBytes := Encode(first)

	back, err := Decode(firstBytes)
	if err != nil {
		t.Fatalf("what was encoded does not decode: %v", err)
	}
	rendered, err := RenderJSON(back)
	if err != nil {
		t.Fatalf("a decoded document does not render: %v", err)
	}
	second, err := ParseJSON(rendered)
	if err != nil {
		t.Fatalf("the rendered JSON does not parse (%v):\n%s", err, rendered)
	}
	if secondBytes := Encode(second); !bytes.Equal(firstBytes, secondBytes) {
		t.Errorf("the render lost something: %d bytes became %d\n%s",
			len(firstBytes), len(secondBytes), rendered)
	}
}

// THE TYPE OF A NUMBER SURVIVES THE RENDER, which is the one thing the shortest
// float printer would silently lose: 20.0 prints as "20", and a "20" read back
// is the signed 64-bit type, so a double setting would quietly become an int
// setting on the next write.
func TestRenderJSONKeepsADoubleLookingLikeADouble(t *testing.T) {
	doc := Document{
		Version: [4]uint16{2, 0, 77, 0},
		Root: Dict(
			Pair("startup", Dict(
				Pair("whole-double", Dict(Pair("value", Number(20)))),
				Pair("whole-int", Dict(Pair("value", Int(20)))),
				Pair("negative-whole-double", Dict(Pair("value", Number(-3)))),
				Pair("big-double", Dict(Pair("value", Number(1e21)))),
			)),
			Pair("runtime-global", Dict()),
			Pair("runtime-per-user", Dict()),
		),
	}
	out, err := RenderJSON(doc)
	if err != nil {
		t.Fatalf("it did not render: %v", err)
	}
	for _, want := range []string{
		`"whole-double": 20.0`,
		`"whole-int": 20`,
		`"negative-whole-double": -3.0`,
		`"big-double": 1e+21`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the rendering does not contain %s:\n%s", want, out)
		}
	}

	// And the proof that the spelling is load-bearing rather than cosmetic:
	// parsing what was just rendered gives back the same node types.
	again, err := ParseJSON(out)
	if err != nil {
		t.Fatalf("what was rendered does not parse: %v", err)
	}
	for i, want := range []Kind{KindNumber, KindInt, KindNumber, KindNumber} {
		got := again.Root.Dict[0].Value.Dict[i].Value.Dict[0].Value.Kind
		if got != want {
			t.Errorf("setting %d came back as %s, want %s",
				i, kindName(got), kindName(want))
		}
	}
}

// The rendering is in FILE order, not sorted, because that is the order the
// bytes are in and this output is compared against the file it came from.
func TestRenderJSONKeepsTheFileOrder(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"version":[2,0,77,0],"startup":{"z":1,"m":2,"a":3}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	out, err := RenderJSON(doc)
	if err != nil {
		t.Fatalf("it did not render: %v", err)
	}
	text := string(out)
	z, m, a := strings.Index(text, `"z"`), strings.Index(text, `"m"`), strings.Index(text, `"a"`)
	if z < 0 || m < 0 || a < 0 || !(z < m && m < a) {
		t.Errorf("the rendering is not in file order (z at %d, m at %d, a at %d):\n%s", z, m, a, text)
	}
}

// EVERY REFUSAL NAMES THE KEY. The alternative to refusing is JSON that parses
// back into a DIFFERENT file, which is worse than an error because nothing
// downstream would notice.
func TestRenderJSONRefusesWhatTheJSONFormCannotSay(t *testing.T) {
	setting := func(v Value) Document {
		return Document{
			Version: [4]uint16{2, 0, 77, 0},
			Root:    Dict(Pair("startup", Dict(Pair("a-setting", v)))),
		}
	}
	cases := []struct {
		name string
		doc  Document
		want string
	}{
		{
			name: "a header flag the engine never writes",
			doc:  Document{Version: [4]uint16{2, 0, 77, 0}, Flag: 3, Root: Dict()},
			want: "the header flag is 3",
		},
		{
			name: "a root that is not a dictionary",
			doc:  Document{Version: [4]uint16{2, 0, 77, 0}, Root: Bool(true)},
			want: "the root node is a bool",
		},
		{
			name: "a section nothing knows",
			doc:  Document{Version: [4]uint16{2, 0, 77, 0}, Root: Dict(Pair("data", Dict()))},
			want: `the root names "data"`,
		},
		{
			name: "a section named twice",
			doc: Document{Version: [4]uint16{2, 0, 77, 0},
				Root: Dict(Pair("startup", Dict()), Pair("startup", Dict()))},
			want: "names the section startup twice",
		},
		{
			name: "a section that is not a dictionary",
			doc:  Document{Version: [4]uint16{2, 0, 77, 0}, Root: Dict(Pair("startup", Int(1)))},
			want: "the section startup is a signed 64-bit number",
		},
		{
			name: "a setting that is not a dictionary",
			doc:  setting(Str("bare")),
			want: "startup.a-setting is a string",
		},
		{
			name: "a setting holding something other than value",
			doc:  setting(Dict(Pair("value", Bool(true)), Pair("extra", Bool(true)))),
			want: "startup.a-setting holds 2 keys (value, extra)",
		},
		{
			name: "a value with the any-type byte set",
			doc:  setting(Dict(Pair("value", Value{Kind: KindString, AnyType: true}))),
			want: "the value of startup.a-setting has its any-type byte set",
		},
		{
			name: "a value of a node type the JSON form has no word for",
			doc:  setting(Dict(Pair("value", Value{Kind: Kind(4)}))),
			want: "the value of startup.a-setting is node type 4",
		},
		{
			name: "a double JSON cannot write",
			doc:  setting(Dict(Pair("value", Number(inf())))),
			want: "the value of startup.a-setting is +Inf",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RenderJSON(c.doc)
			if err == nil {
				t.Fatalf("it was rendered")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why\n got: %s\nwant it to contain: %s", err, c.want)
			}
		})
	}
}

func inf() float64 {
	f := 0.0
	return 1 / f
}

// THE FILE THE ENGINE WROTE, rendered and read back. Every other test in here
// renders something this package built; this one renders Factorio's own bytes,
// which is the case the gate actually runs.
func TestTheEngineWrittenFileRendersAndParsesBackToItself(t *testing.T) {
	raw, err := os.ReadFile(engineDat)
	if err != nil {
		t.Fatalf("the engine-written file is not there: %v", err)
	}
	doc, err := Decode(raw)
	if err != nil {
		t.Fatalf("the engine's own file does not decode: %v", err)
	}
	rendered, err := RenderJSON(doc)
	if err != nil {
		t.Fatalf("the engine's own file does not render: %v", err)
	}
	again, err := ParseJSON(rendered)
	if err != nil {
		t.Fatalf("the rendering of the engine's own file does not parse (%v):\n%s", err, rendered)
	}
	if got := Encode(again); !bytes.Equal(got, raw) {
		t.Errorf("the engine's own file did not survive the render (%d bytes in, %d out):\n%s",
			len(raw), len(got), rendered)
	}
}

// BYTES THAT ARE NOT UTF-8 ARE A REFUSAL, and this is the one case where "it
// cannot say that" does not look like anything: encoding/json REPLACES each
// invalid byte with U+FFFD instead of failing, so before this refusal existed a
// dat holding one rendered, parsed back and re-encoded to a DIFFERENT dat with
// exit 0 (measured on the bytes below: 114 in, 116 out). The dat is built with
// Encode and read back with Decode rather than asserted on a Value, because the
// claim is about a FILE: nothing stops a byte string from reaching the reader,
// and the reader keeps it.
func TestRenderJSONRefusesBytesThatAreNotUTF8(t *testing.T) {
	cases := []struct {
		name string
		root Value
		want string
	}{
		{
			name: "a stored value",
			root: Dict(Pair("startup", Dict(
				Pair("a-setting", Dict(Pair("value", Str("caf\xe9 latte"))))))),
			want: "the value of startup.a-setting holds bytes that are not UTF-8",
		},
		{
			name: "a setting name",
			root: Dict(Pair("startup", Dict(
				Pair("caf\xe9", Dict(Pair("value", Bool(true))))))),
			want: `the section startup names the setting "caf\xe9", which holds bytes that are not UTF-8`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := Encode(Document{Version: [4]uint16{2, 0, 77, 0}, Root: c.root})
			doc, err := Decode(raw)
			if err != nil {
				t.Fatalf("the reader refused bytes it does hold: %v", err)
			}
			out, err := RenderJSON(doc)
			if err == nil {
				t.Fatalf("it was rendered, and the U+FFFD in it re-encodes to a different file:\n%s", out)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why\n got: %s\nwant it to contain: %s", err, c.want)
			}
		})
	}
}

// A NUL IS NOT THAT. It is valid UTF-8, JSON writes it as \u0000, and a setting
// holding one has to survive the round trip the refusal above protects: the
// guard is "not UTF-8" and not "not printable".
func TestRenderJSONKeepsAStringHoldingANul(t *testing.T) {
	raw := Encode(Document{
		Version: [4]uint16{2, 0, 77, 0},
		Root: Dict(
			Pair("startup", Dict(Pair("a-setting", Dict(Pair("value", Str("a\x00b")))))),
			Pair("runtime-global", Dict()),
			Pair("runtime-per-user", Dict()),
		),
	})
	doc, err := Decode(raw)
	if err != nil {
		t.Fatalf("it does not decode: %v", err)
	}
	rendered, err := RenderJSON(doc)
	if err != nil {
		t.Fatalf("a string holding a nul was refused: %v", err)
	}
	if !strings.Contains(string(rendered), `\u0000`) {
		t.Errorf("the nul is not written as an escape:\n%s", rendered)
	}
	again, err := ParseJSON(rendered)
	if err != nil {
		t.Fatalf("the rendering does not parse (%v):\n%s", err, rendered)
	}
	if got := Encode(again); !bytes.Equal(got, raw) {
		t.Errorf("a nul did not survive the round trip (%d bytes in, %d out):\n%s",
			len(raw), len(got), rendered)
	}
}
