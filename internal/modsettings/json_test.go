package modsettings

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

// flippedJSON is the settings file the in-game gate's FLIPPED row hands the
// engine. It is under testdata/datastage because scripts/run-datastage.sh reads
// it too, and a second copy is a second thing to keep in step.
const flippedJSON = "../../testdata/datastage/flipped.json"

// KEY ORDER IS THE FILE'S ORDER, and this is the test that says so. Decoding
// into a map would pass every other test in this package and produce a
// different byte string on every run.
func TestParseJSONKeepsTheOrderTheFileWroteIn(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"version":[2,0,77,0],"startup":{
	  "z-last":1, "a-first":2, "m-middle":3, "b-second":4, "y-fourth":5,
	  "c-third":6, "x-fifth":7, "d-sixth":8, "w-seventh":9, "e-eighth":10
	}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	want := []string{"z-last", "a-first", "m-middle", "b-second", "y-fourth",
		"c-third", "x-fifth", "d-sixth", "w-seventh", "e-eighth"}
	var got []string
	for _, e := range doc.Root.Dict[0].Value.Dict {
		got = append(got, e.Key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the setting order moved\n got: %v\nwant: %v", got, want)
	}
}

// THE SPELLING PICKS THE TYPE. An int setting and a double setting are
// different prototypes, and the file says which one a row is for.
func TestParseJSONTypesANumberByItsSpelling(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"version":[2,0,77,0],"startup":{
	  "whole":40, "fractional":20.5, "whole-with-a-point":20.0,
	  "exponent":1e3, "negative":-7, "text":"custom", "yes":true
	}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	want := []Value{
		Int(40), Number(20.5), Number(20), Number(1000), Int(-7), Str("custom"), Bool(true),
	}
	entries := doc.Root.Dict[0].Value.Dict
	if len(entries) != len(want) {
		t.Fatalf("got %d settings, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		// Each setting is stored as a dictionary holding one key, value.
		if len(e.Value.Dict) != 1 || e.Value.Dict[0].Key != "value" {
			t.Fatalf("%s is not a dictionary holding value: %#v", e.Key, e.Value)
		}
		if got := e.Value.Dict[0].Value; !reflect.DeepEqual(got, want[i]) {
			t.Errorf("%s came out as %#v, want %#v", e.Key, got, want[i])
		}
	}
}

// All three stages are always written, empty or not: that is the shape the
// engine's own file has, and a file missing one is a file this writer did not
// produce.
func TestParseJSONWritesEveryStage(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"version":[2,0,77,0],"startup":{"a":1}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	var got []string
	for _, e := range doc.Root.Dict {
		got = append(got, e.Key)
	}
	if !reflect.DeepEqual(got, stageOrder) {
		t.Errorf("the stages are %v, want %v", got, stageOrder)
	}
	if len(doc.Root.Dict[1].Value.Dict) != 0 || doc.Root.Dict[2].Value.Dict != nil {
		t.Errorf("an unnamed stage is not empty: %#v", doc.Root.Dict[1:])
	}
}

// A section may be given in any order and the file still comes out in the
// engine's, because the order of the STAGES is the format's and only the order
// of the settings inside one is the author's.
func TestParseJSONOrdersTheStagesItself(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"runtime-per-user":{"a":1},"version":[2,0,77,0],"startup":{"b":2}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	if doc.Root.Dict[0].Key != "startup" || doc.Root.Dict[2].Key != "runtime-per-user" {
		t.Errorf("the stages came out in the file's order rather than the engine's: %#v", doc.Root.Dict)
	}
}

func TestParseJSONRefusesWhatItDoesNotUnderstand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no version", `{"startup":{}}`, "names no version"},
		{"a short version", `{"version":[2,0],"startup":{}}`, "has 2 parts"},
		{"a long version", `{"version":[2,0,77,0,1],"startup":{}}`, "more than 4 parts"},
		{"a version part that is not a number", `{"version":[2,0,"77",0]}`, "every part of a version is a number"},
		{"a version part out of range", `{"version":[2,0,70000,0],"startup":{}}`, "from 0 to 65535"},
		{"a section nothing knows", `{"version":[2,0,77,0],"data":{}}`, `names "data"`},
		{"a section named twice", `{"version":[2,0,77,0],"startup":{},"startup":{}}`, "names the section startup twice"},
		{"a version named twice", `{"version":[2,0,77,0],"version":[2,0,77,0],"startup":{}}`, "names version twice"},
		{"a version named twice with different numbers", `{"version":[2,0,77,0],"startup":{},"version":[1,1,110,0]}`, "names version twice"},
		{"a comment named twice", `{"comment":"one","comment":"two","version":[2,0,77,0]}`, "names comment twice"},
		{"a setting named twice", `{"version":[2,0,77,0],"startup":{"a":1,"a":2}}`, "names the setting a twice"},
		{"a setting holding a list", `{"version":[2,0,77,0],"startup":{"a":[1]}}`, "holds a list"},
		{"a setting holding an object", `{"version":[2,0,77,0],"startup":{"a":{"b":1}}}`, "holds an object"},
		{"a setting holding null", `{"version":[2,0,77,0],"startup":{"a":null}}`, "holds null"},
		{"a whole number too big for 64 bits", `{"version":[2,0,77,0],"startup":{"a":99999999999999999999}}`, "does not fit a signed 64-bit number"},
		{"a comment that is not a string", `{"comment":3,"version":[2,0,77,0]}`, "a comment is a string"},
		{"two documents", `{"version":[2,0,77,0],"startup":{}} {"version":[2,0,77,0]}`, "more than one document"},
		{"not an object", `[1,2]`, "the document begins with"},
		// The four the decoder would have read without a word, substituting
		// U+FFFD for what it could not carry. The check is on the document's
		// own bytes and escapes, because the substitution is valid UTF-8 by
		// the time a token comes back.
		{"a value holding a byte that is not UTF-8", "{\"version\":[2,0,77,0],\"startup\":{\"a\":\"x\x80y\"}}", "byte 39 of the JSON is not UTF-8"},
		{"a name holding a byte that is not UTF-8", "{\"version\":[2,0,77,0],\"startup\":{\"a\x80b\":\"v\"}}", "byte 35 of the JSON is not UTF-8"},
		{"a lone high surrogate escape", `{"version":[2,0,77,0],"startup":{"a":"x\ud800y"}}`, "escape at byte 39 of the JSON is a lone surrogate"},
		{"a lone low surrogate escape", `{"version":[2,0,77,0],"startup":{"a":"x\udc00y"}}`, "escape at byte 39 of the JSON is a lone surrogate"},
		{"a high surrogate escape followed by a non-surrogate escape", `{"version":[2,0,77,0],"startup":{"a":"\ud800\u0041"}}`, "escape at byte 38 of the JSON is a lone surrogate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseJSON([]byte(c.in))
			if err == nil {
				t.Fatalf("it was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why\n got: %s\nwant it to contain: %s", err, c.want)
			}
		})
	}
}

// The comment key exists so a committed row can explain itself, and it must
// reach no byte of the dat.
func TestTheCommentReachesNoByte(t *testing.T) {
	with, err := ParseJSON([]byte(`{"comment":"why this row is what it is","version":[2,0,77,0],"startup":{"a":1}}`))
	if err != nil {
		t.Fatalf("a commented document did not parse: %v", err)
	}
	without, err := ParseJSON([]byte(`{"version":[2,0,77,0],"startup":{"a":1}}`))
	if err != nil {
		t.Fatalf("it did not parse: %v", err)
	}
	if !reflect.DeepEqual(with, without) {
		t.Errorf("the comment changed the document")
	}
}

// THE COMMITTED FLIPPED ROW, which the in-game gate hands the engine. It parses
// here rather than only in the gate, so a row edited into something that does
// not encode is a red test on a machine with no Factorio on it.
func TestTheCommittedFlippedRowParsesAndReadsBack(t *testing.T) {
	data, err := os.ReadFile(flippedJSON)
	if err != nil {
		t.Fatalf("the flipped row is what the in-game gate flips and it is not there: %v", err)
	}
	doc, err := ParseJSON(data)
	if err != nil {
		t.Fatalf("the committed flipped row does not parse: %v", err)
	}
	got, err := Decode(Encode(doc))
	if err != nil {
		t.Fatalf("the encoded flipped row does not read back: %v", err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Errorf("the flipped row did not survive its own round trip")
	}
	// And it says what the gate's assertions rest on: the example guest's
	// startup setting, turned off. A row that stopped saying this would leave
	// the gate comparing two identical dumps.
	want := Dict(Pair("fkd-enabled", Dict(Pair("value", Bool(false)))))
	if !reflect.DeepEqual(doc.Root.Dict[0].Value, want) {
		t.Errorf("the flipped row's startup section is %#v, and the gate flips fkd-enabled to false",
			doc.Root.Dict[0].Value)
	}
}

// TWO PARSES OF ONE DOCUMENT ENCODE THE SAME WAY. The written file is what the
// engine reads, and what it reads has to be a function of the JSON and nothing
// else.
//
// THE FIXTURE IS NOT THE COMMITTED FLIPPED ROW, and that is the whole point of
// it. The flipped row names one section holding one setting, so the only order
// there is to lose is an order of one, and this test could not have gone red on
// it whatever the parser did. Three sections, several settings each: the root
// assembly walks stageOrder and every section walks its own token stream, so an
// ordering that came from a map instead would show up here. Parsed repeatedly
// rather than twice because Go randomises map iteration per range, and one pair
// of walks over three keys agrees by chance often enough to be no test at all.
func TestTwoParsesOfOneDocumentEncodeTheSameWay(t *testing.T) {
	source := []byte(`{
	  "version": [2, 0, 77, 0],
	  "startup": {"z-last": 1, "a-first": true, "m-middle": "text", "b-second": 20.0},
	  "runtime-global": {"g-one": false, "g-two": 3, "g-three": "x"},
	  "runtime-per-user": {"u-one": 1.5, "u-two": "y"}
	}`)
	first, err := ParseJSON(source)
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	want := Encode(first)
	for i := 0; i < 32; i++ {
		again, err := ParseJSON(source)
		if err != nil {
			t.Fatalf("parse %d of the same document failed: %v", i+2, err)
		}
		if got := Encode(again); !bytes.Equal(want, got) {
			t.Fatalf("parse %d of one document encoded to different bytes (%d and %d),"+
				" so something between the JSON and the file does not keep one order",
				i+2, len(want), len(got))
		}
	}
}

// AND THE COMMITTED FLIPPED ROW IS THE ONE THE GATE HANDS THE ENGINE, so it
// encodes here too: the row above says the parser keeps an order, this says the
// file the gate installs is a file this writer can produce at all.
func TestTheFlippedRowEncodesTheSameWayTwice(t *testing.T) {
	data, err := os.ReadFile(flippedJSON)
	if err != nil {
		t.Fatalf("the flipped row is not there: %v", err)
	}
	first, err := ParseJSON(data)
	if err != nil {
		t.Fatalf("the committed flipped row does not parse: %v", err)
	}
	second, err := ParseJSON(data)
	if err != nil {
		t.Fatalf("the committed flipped row does not parse the second time: %v", err)
	}
	if !bytes.Equal(Encode(first), Encode(second)) {
		t.Errorf("two parses of the committed flipped row encoded to different bytes")
	}
}

// The guard above is "what JSON cannot carry" and not "anything unusual": every
// one of these is a value the author really wrote, and each reaches the bytes
// unchanged. The escaped backslash is the case a naive scan for \u gets wrong.
func TestParseJSONCarriesEveryStringJSONCanSay(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
	}{
		{"a literal U+FFFD", "{\"version\":[2,0,77,0],\"startup\":{\"a\":\"x\xef\xbf\xbdy\"}}", "x\uFFFDy"},
		{"an escaped U+FFFD", `{"version":[2,0,77,0],"startup":{"a":"x\ufffdy"}}`, "x\uFFFDy"},
		{"an escaped NUL", `{"version":[2,0,77,0],"startup":{"a":"x\u0000y"}}`, "x\x00y"},
		{"a surrogate pair", `{"version":[2,0,77,0],"startup":{"a":"\ud83d\ude00"}}`, "\U0001F600"},
		{"an escaped backslash before the letters u0080", `{"version":[2,0,77,0],"startup":{"a":"\\u0080"}}`, "\\u0080"},
		{"an escaped backslash before the letters ud800", `{"version":[2,0,77,0],"startup":{"a":"\\ud800"}}`, "\\ud800"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc, err := ParseJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			back, err := Decode(Encode(doc))
			if err != nil {
				t.Fatalf("the encoding did not read back: %v", err)
			}
			got := back.Root.Dict[0].Value.Dict[0].Value.Dict[0].Value.Str
			if got != c.want {
				t.Errorf("the value came through as %q, want %q", got, c.want)
			}
		})
	}
}
