package wasm

// The one place this package looks at the raw binary itself rather than at what
// watgo decoded from it.
//
// watgo keeps every custom section verbatim, which is where a guest's DWARF
// lives, but it keeps no byte offsets -- and the DWARF join needs exactly one
// number per function: where its body starts, measured from the first byte of
// the code section's payload. That is the coordinate system DW_AT_low_pc uses
// on a wasm target. Measured on both toolchains at the scaffolded flags: every
// subprogram's low_pc equals a body's payload offset exactly (TinyGo 41/41,
// rustc 61/61), and reading the same values as FILE offsets matches almost
// nothing (0/41 and 1/61). So the walk below is the join, and it is a walk of
// section headers only -- no instruction is decoded.
//
// Everything here is defensive: a malformed length or a truncated section
// returns nil rather than a partial answer, because a body offset that is off
// by one attributes one function's source line to its neighbour, silently.

// codeSectionID is the wasm code section's id.
const codeSectionID = 10

// codeSpans returns the payload-relative byte range of every function body in
// b, in code-section order, or nil when the module cannot be walked.
func codeSpans(b []byte) []CodeSpan {
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil
	}
	i := 8
	for i < len(b) {
		id := b[i]
		i++
		size, n, ok := uleb32(b, i)
		if !ok {
			return nil
		}
		i = n
		end := i + int(size)
		if end < i || end > len(b) {
			return nil
		}
		if id == codeSectionID {
			return bodySpans(b[i:end])
		}
		i = end
	}
	return nil
}

// bodySpans walks a code section payload and returns each body's range within
// it.
func bodySpans(payload []byte) []CodeSpan {
	count, j, ok := uleb32(payload, 0)
	if !ok {
		return nil
	}
	spans := make([]CodeSpan, 0, count)
	for k := uint32(0); k < count; k++ {
		size, next, ok := uleb32(payload, j)
		if !ok {
			return nil
		}
		lo := next
		hi := lo + int(size)
		if hi < lo || hi > len(payload) {
			return nil
		}
		spans = append(spans, CodeSpan{Lo: uint32(lo), Hi: uint32(hi)})
		j = hi
	}
	return spans
}

// uleb32 reads one unsigned LEB128 at i, returning the value, the offset just
// past it, and whether it decoded at all. Values wider than 32 bits are
// refused: every length this file reads is a section or body size.
func uleb32(b []byte, i int) (uint32, int, bool) {
	var v uint64
	var shift uint
	for n := 0; n < 5; n++ {
		if i >= len(b) {
			return 0, 0, false
		}
		c := b[i]
		i++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			if v > 0xffffffff {
				return 0, 0, false
			}
			return uint32(v), i, true
		}
		shift += 7
	}
	return 0, 0, false
}
