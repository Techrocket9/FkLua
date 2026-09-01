package wasm

import (
	"bytes"
	"testing"
)

// section frames a payload as a wasm section. Every length here is a single
// LEB byte, which is what keeps the offsets in the test below countable by
// hand.
func section(id byte, payload []byte) []byte {
	if len(payload) > 0x7f {
		panic("the test builder only writes one-byte lengths")
	}
	return append([]byte{id, byte(len(payload))}, payload...)
}

// twoFuncModule is a module whose body offsets can be counted off the page,
// which is the only way to test the join arithmetic against something that is
// not itself. It needs no toolchain and runs everywhere.
//
// The code section's PAYLOAD, byte by byte:
//
//	offset 0: 02          two function bodies
//	offset 1: 02          body 0 is 2 bytes...
//	offset 2: 00 0b       ...so it runs [2, 4): no locals, end
//	offset 4: 03          body 1 is 3 bytes...
//	offset 5: 00 01 0b    ...so it runs [5, 8): no locals, nop, end
//
// Hence the spans below. A change to what CodeSpan measures FROM -- the file
// rather than the payload, say -- moves both of these and fails here, which is
// the point: DWARF's low_pc is in payload coordinates, and nothing else in the
// compiler would notice the difference.
func twoFuncModule(extra ...byte) []byte {
	var b bytes.Buffer
	b.WriteString("\x00asm")
	b.Write([]byte{0x01, 0x00, 0x00, 0x00})
	// One type, () -> ().
	b.Write(section(1, []byte{0x01, 0x60, 0x00, 0x00}))
	// Two functions, both of that type.
	b.Write(section(3, []byte{0x02, 0x00, 0x00}))
	b.Write(section(10, []byte{
		0x02,
		0x02, 0x00, 0x0b,
		0x03, 0x00, 0x01, 0x0b,
	}))
	b.Write(extra)
	return b.Bytes()
}

func TestCodeSpansAreMeasuredFromTheCodePayload(t *testing.T) {
	m, err := Decode(twoFuncModule())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []CodeSpan{{Lo: 2, Hi: 4}, {Lo: 5, Hi: 8}}
	if len(m.CodeSpans) != len(want) {
		t.Fatalf("%d spans, want %d: %v", len(m.CodeSpans), len(want), m.CodeSpans)
	}
	for i := range want {
		if m.CodeSpans[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, m.CodeSpans[i], want[i])
		}
	}
	if len(m.CodeSpans) != len(m.Funcs) {
		t.Errorf("%d spans for %d functions; the two are joined by position",
			len(m.CodeSpans), len(m.Funcs))
	}
}

// A custom section survives decoding, because that is where a guest's DWARF
// lives and the compiler used to drop all of them on the floor.
func TestCustomSectionsSurviveDecoding(t *testing.T) {
	// id 0, then a length-prefixed name, then the payload.
	custom := section(0, append([]byte{0x08, '.', 'd', 'e', 'b', 'u', 'g', '_', 'x'}, 0xAA, 0xBB))
	m, err := Decode(twoFuncModule(custom...))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := m.CustomSectionByName(".debug_x")
	if !ok {
		var names []string
		for _, c := range m.Custom {
			names = append(names, c.Name)
		}
		t.Fatalf("no .debug_x section; the module carries %v", names)
	}
	if !bytes.Equal(got, []byte{0xAA, 0xBB}) {
		t.Errorf("payload = % x, want AA BB", got)
	}
	if _, ok := m.CustomSectionByName(".debug_absent"); ok {
		t.Error("a section that is not there was reported present")
	}
}

// A module decoded from TEXT has neither, and says so by leaving both nil
// rather than by returning something that looks like an answer. The debug map
// keys on that: no spans means no DWARF join, which means a name-only map.
func TestATextModuleCarriesNoBinaryOffsets(t *testing.T) {
	m, err := DecodeWAT(`(module (func) (func (nop)))`)
	if err != nil {
		t.Fatalf("wat: %v", err)
	}
	if m.CodeSpans != nil {
		t.Errorf("a text module reported code spans: %v", m.CodeSpans)
	}
	if m.Custom != nil {
		t.Errorf("a text module reported custom sections: %v", m.Custom)
	}
}

// A truncated code section produces NO spans rather than a short list. A
// partial answer here is worse than none: the join is by position, so a missing
// body shifts every function after it onto its neighbour's source line.
func TestATruncatedCodeSectionYieldsNoSpans(t *testing.T) {
	full := twoFuncModule()
	// The last byte of the second body's contents, with the section length left
	// claiming it is still there.
	if got := codeSpans(full[:len(full)-1]); got != nil {
		t.Errorf("a truncated module produced spans: %v", got)
	}
	if got := codeSpans([]byte("not a wasm module at all")); got != nil {
		t.Errorf("a non-module produced spans: %v", got)
	}
	if got := codeSpans(nil); got != nil {
		t.Errorf("no bytes produced spans: %v", got)
	}
}
