package modsettings

import (
	"bytes"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// engineDat is a mod-settings.dat THE ENGINE ITSELF WROTE, kept because every
// other test in this package compares this code against itself and would stay
// green for a reader and a writer that agreed on the wrong layout. How it was
// produced is in the file beside it.
const engineDat = "../../testdata/modsettings/engine-2.0.77.dat"

// THE ROUND TRIP OVER EVERY TYPE. A writer nobody reads back is a writer whose
// first reader is the engine, and the engine's diagnosis for a byte in the
// wrong place is a refusal that names the file rather than the byte.
func TestDecodeReadsBackEverythingEncodeWrites(t *testing.T) {
	long := strings.Repeat("x", 300)
	// EXACTLY 255, which is the space optimisation's escape value and therefore
	// the one length a reader can get wrong in both directions at once.
	edge := strings.Repeat("y", 255)
	just := strings.Repeat("z", 254)

	doc := Document{
		Version: [4]uint16{2, 0, 77, 65535},
		Flag:    1,
		Root: Dict(
			Pair("startup", Dict(
				Pair("a-bool", Dict(Pair("value", Bool(true)))),
				Pair("a-false", Dict(Pair("value", Bool(false)))),
				Pair("a-double", Dict(Pair("value", Number(12.5)))),
				Pair("a-negative-double", Dict(Pair("value", Number(-0.5)))),
				Pair("an-int", Dict(Pair("value", Int(40)))),
				Pair("a-negative-int", Dict(Pair("value", Int(-9007199254740993)))),
				Pair("the-biggest-int", Dict(Pair("value", Int(math.MaxInt64)))),
				Pair("the-smallest-int", Dict(Pair("value", Int(math.MinInt64)))),
				Pair("a-string", Dict(Pair("value", Str("1 steel-plate, 10 water")))),
				Pair("an-empty-string", Dict(Pair("value", Str("")))),
				Pair("a-254-byte-string", Dict(Pair("value", Str(just)))),
				Pair("a-255-byte-string", Dict(Pair("value", Str(edge)))),
				Pair("a-long-string", Dict(Pair("value", Str(long)))),
				Pair("a-unicode-string", Dict(Pair("value", Str("unicode ✓ café")))),
				// The any-type byte is carried rather than normalised, so a
				// file the engine wrote with it set reads back as itself.
				Pair("an-any-typed-value", Dict(Pair("value",
					Value{Kind: KindString, AnyType: true, Str: "any"}))),
			)),
			Pair("runtime-global", Dict()),
			Pair("runtime-per-user", Dict(
				Pair("somebody-elses", Dict(Pair("value", Str("ignored")))),
			)),
		),
	}

	got, err := Decode(Encode(doc))
	if err != nil {
		t.Fatalf("a document this package wrote does not read back: %v", err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Errorf("the round trip changed the document\n got: %#v\nwant: %#v", got, doc)
	}
}

// An empty dictionary is one node and a count of zero, and Decode has to give
// back a nil slice for it rather than an empty one, or the round trip above is
// comparing two shapes that only look the same.
func TestAnEmptyDictionaryRoundTrips(t *testing.T) {
	doc := Document{Version: [4]uint16{2, 0, 77, 0}, Root: Dict()}
	got, err := Decode(Encode(doc))
	if err != nil {
		t.Fatalf("an empty root does not read back: %v", err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Errorf("an empty dictionary did not round trip: %#v", got)
	}
}

// THE HAND-ASSEMBLED FILE, byte by byte from the measured layout. It is the
// only thing in this package that says WHICH layout the encoder and the decoder
// agree on.
func TestDecodeReadsAHandAssembledFile(t *testing.T) {
	raw := []byte{
		0x02, 0x00, // version part 1: 2
		0x00, 0x00, // version part 2: 0
		0x4d, 0x00, // version part 3: 77
		0x00, 0x00, // version part 4: 0
		0x00, // the flag byte that closes the header

		0x05, 0x00, // the root: a dictionary, any-type false
		0x02, 0x00, 0x00, 0x00, // holding two entries

		0x00, 0x07, 's', 't', 'a', 'r', 't', 'u', 'p', // key "startup"
		0x05, 0x00, // a dictionary
		0x01, 0x00, 0x00, 0x00, // holding one entry
		0x00, 0x01, 'a', // key "a"
		0x05, 0x00, // a dictionary
		0x01, 0x00, 0x00, 0x00, // holding one entry
		0x00, 0x05, 'v', 'a', 'l', 'u', 'e', // key "value"
		0x03, 0x00, // a string
		0x00, 0x02, 'h', 'i', // not empty, two bytes, "hi"

		0x00, 0x0e, 'r', 'u', 'n', 't', 'i', 'm', 'e', '-', 'g', 'l', 'o', 'b', 'a', 'l',
		0x05, 0x00, // a dictionary
		0x00, 0x00, 0x00, 0x00, // holding nothing
	}

	want := Document{
		Version: [4]uint16{2, 0, 77, 0},
		Root: Dict(
			Pair("startup", Dict(Pair("a", Dict(Pair("value", Str("hi")))))),
			Pair("runtime-global", Dict()),
		),
	}

	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("the hand-assembled file did not decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the hand-assembled file decoded to the wrong document\n got: %#v\nwant: %#v", got, want)
	}
	// And back out again, byte for byte: the encoder writes the same layout the
	// hand-assembled bytes describe rather than a private one of its own.
	if again := Encode(got); !bytes.Equal(again, raw) {
		t.Errorf("re-encoding the hand-assembled file changed it\n got: % x\nwant: % x", again, raw)
	}
}

// THE FILE THE ENGINE WROTE. Two properties, and they are different: the reader
// consumes EVERY byte of it (a reader that stops early has misread a length
// upstream and would otherwise report success on a file it does not
// understand), and re-encoding what it read reproduces the file exactly (a
// reader that quietly normalised something would pass the first and fail this).
//
// It is a HARD FAILURE rather than a skip when the file is missing, because a
// skipped gate reads exactly like a pass and this is the only test here that
// measures against Factorio.
func TestAFileTheEngineWroteRoundTripsByteForByte(t *testing.T) {
	raw, err := os.ReadFile(engineDat)
	if err != nil {
		t.Fatalf("the engine-written file is the whole point of this test and it is not there: %v\n"+
			"take it again the way testdata/modsettings/PROVENANCE describes: run\n"+
			"scripts/run-datastage.sh and copy the file the engine wrote", err)
	}
	doc, err := Decode(raw)
	if err != nil {
		t.Fatalf("the engine's own file does not decode: %v", err)
	}
	again := Encode(doc)
	if !bytes.Equal(again, raw) {
		t.Fatalf("re-encoding the engine's own file changed it (%d bytes in, %d out)", len(raw), len(again))
	}
	// And it is the shape this package claims a mod-settings.dat has, rather
	// than merely some property tree: three sections, in the engine's order.
	var stages []string
	for _, e := range doc.Root.Dict {
		stages = append(stages, e.Key)
	}
	if !reflect.DeepEqual(stages, stageOrder) {
		t.Errorf("the engine's own file carries the sections %v, and this package writes %v", stages, stageOrder)
	}
}

func TestDecodeRefusesWhatItDoesNotUnderstand(t *testing.T) {
	header := []byte{0x02, 0x00, 0x00, 0x00, 0x4d, 0x00, 0x00, 0x00, 0x00}
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "a file that stops inside the header",
			raw:  header[:5],
			want: "ends in the middle of",
		},
		{
			name: "a node type nothing writes",
			raw:  append(append([]byte{}, header...), 0x04, 0x00),
			want: "node type 4 at offset 9",
		},
		{
			name: "a string whose length runs off the end",
			raw:  append(append([]byte{}, header...), 0x03, 0x00, 0x00, 0x20, 'h', 'i'),
			want: "a string of 32 bytes",
		},
		{
			name: "a dictionary that promises more entries than it holds",
			raw: append(append([]byte{}, header...),
				0x05, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x01, 'a', 0x01, 0x00, 0x01),
			want: "ends in the middle of",
		},
		{
			name: "bytes after the root node",
			raw:  append(append([]byte{}, header...), 0x01, 0x00, 0x01, 0xff),
			want: "1 byte is left over after the root node, at offset 12",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode(c.raw)
			if err == nil {
				t.Fatalf("it was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why\n got: %s\nwant it to contain: %s", err, c.want)
			}
		})
	}
}
