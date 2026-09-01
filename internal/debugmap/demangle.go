package debugmap

import (
	"strconv"
	"strings"
)

// Demangle turns a name-section symbol into something a person can read, and
// returns the raw symbol beside it when it changed.
//
// The second result is EMPTY when nothing was demangled, which is how the map's
// `mangled` field stays absent for the overwhelming majority of entries: a Go
// guest's names are already source-level, a Rust #[no_mangle] export is already
// its own name, and a synthetic func[N] is not a symbol at all.
//
// Scope is deliberate. This handles Rust's v0 scheme -- which is what rustc
// emits for a wasm guest today -- for the shapes a guest's own code and the
// standard library actually produce: crate roots and nested paths, with
// disambiguators dropped and generic arguments ignored. Everything else,
// including impl paths and back-references, is left exactly as the linker wrote
// it. Degrading to the raw symbol is always correct; inventing a name is not,
// and a wrong name in a stack frame costs more than a long one. Legacy _ZN
// symbols get the one cleanup that is unambiguous: the trailing hash component
// is dropped.
//
// Adding a mangling scheme here is a self-contained change with its own table
// test. Adding a DEPENDENCY for one is not: this is the whole of it, and the
// repository has exactly one third-party module.
func Demangle(sym string) (name, mangled string) {
	var out string
	switch {
	case strings.HasPrefix(sym, "_R"):
		out = demangleV0(sym)
	case strings.HasPrefix(sym, "_ZN"):
		out = demangleLegacy(sym)
	}
	if out == "" || out == sym {
		return sym, ""
	}
	return out, sym
}

// demangleV0 renders a Rust v0 path, or "" when the symbol uses a construct
// this does not model.
//
// The grammar it covers, from the v0 specification:
//
//	<path> = "C" <identifier>                    a crate root
//	       | "N" <namespace> <path> <identifier> a nested path
//	       | "I" <path> {<generic-arg>} "E"      a generic instantiation
//
// A generic instantiation renders as its underlying path with the arguments
// dropped, so a monomorphised `Vec::<T>::new` reads as `alloc::vec::Vec::new`.
// The remaining path forms -- "M" and "X" (inherent and trait impls), "Y" (a
// trait definition) and back-references -- would each need a type renderer to
// produce anything truthful, so they return "" and the caller keeps the symbol.
func demangleV0(sym string) string {
	// An encoding version may follow "_R" as a decimal number. Nothing emits
	// one yet, and a version this does not know is a grammar this cannot read.
	rest := sym[2:]
	if rest == "" || (rest[0] >= '0' && rest[0] <= '9') {
		return ""
	}
	p := &v0parser{s: rest}
	parts := p.path(0)
	if parts == nil {
		return ""
	}
	return strings.Join(parts, "::")
}

// v0parser walks a v0 symbol from the left.
type v0parser struct {
	s string
	i int
}

// maxV0Depth bounds the recursion. A nested path is one frame per component,
// and a symbol deep enough to exhaust the stack is a symbol we do not want to
// render anyway.
const maxV0Depth = 64

func (p *v0parser) path(depth int) []string {
	if depth > maxV0Depth || p.i >= len(p.s) {
		return nil
	}
	switch p.s[p.i] {
	case 'C':
		p.i++
		id, ok := p.identifier()
		if !ok {
			return nil
		}
		return []string{id}
	case 'N':
		p.i++
		// The namespace is one character: 'v' for a value, 't' for a type, an
		// uppercase letter for a compiler-internal one. Its identity does not
		// change how the path reads.
		if p.i >= len(p.s) {
			return nil
		}
		p.i++
		parent := p.path(depth + 1)
		if parent == nil {
			return nil
		}
		id, ok := p.identifier()
		if !ok {
			return nil
		}
		return append(parent, id)
	case 'I':
		p.i++
		// The instantiated path, then generic arguments through to the matching
		// "E". Nothing here reads the arguments, so the parse simply stops.
		return p.path(depth + 1)
	}
	return nil
}

// identifier reads an optional disambiguator followed by a length-prefixed
// name. Punycode identifiers ("u" before the length) are refused: the bytes are
// not the name, and decoding them is not what this is for.
func (p *v0parser) identifier() (string, bool) {
	if p.i < len(p.s) && p.s[p.i] == 's' {
		// <disambiguator> = "s" <base-62-number> "_", and the number may be
		// empty. It exists to tell two same-named items apart and carries
		// nothing a reader wants.
		j := p.i + 1
		for j < len(p.s) && isBase62(p.s[j]) {
			j++
		}
		if j >= len(p.s) || p.s[j] != '_' {
			return "", false
		}
		p.i = j + 1
	}
	if p.i < len(p.s) && p.s[p.i] == 'u' {
		return "", false
	}
	j := p.i
	for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
		j++
	}
	if j == p.i {
		return "", false
	}
	n, err := strconv.Atoi(p.s[p.i:j])
	if err != nil || n <= 0 {
		return "", false
	}
	// A "_" between the length and the bytes escapes an identifier that would
	// otherwise start with a digit or an underscore.
	if j < len(p.s) && p.s[j] == '_' {
		j++
	}
	if j+n > len(p.s) {
		return "", false
	}
	id := p.s[j : j+n]
	if !plainIdent(id) {
		return "", false
	}
	p.i = j + n
	return id, true
}

func isBase62(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// plainIdent is the guard that stops a mis-parse from producing a plausible
// lie. A length that was read out of the middle of something else slices
// arbitrary bytes; a real identifier is ASCII word characters, so anything
// else means the parse went wrong and the symbol should be kept.
func plainIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return len(s) > 0
}

// demangleLegacy handles Rust's pre-v0 scheme, which is length-prefixed
// components between "_ZN" and "E" with a hash component last.
//
// rustc emits v0 for the targets this compiler packages, so this is here for a
// guest built by an older toolchain rather than for anything measured. The one
// transformation is dropping the "17h<16 hex digits>" component, which is not
// part of the name and is the only thing about a legacy symbol that is
// unambiguously noise.
func demangleLegacy(sym string) string {
	rest := strings.TrimPrefix(sym, "_ZN")
	var parts []string
	i := 0
	for i < len(rest) {
		if rest[i] == 'E' {
			break
		}
		j := i
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == i {
			return ""
		}
		n, err := strconv.Atoi(rest[i:j])
		if err != nil || n <= 0 || j+n > len(rest) {
			return ""
		}
		parts = append(parts, rest[j:j+n])
		i = j + n
	}
	if len(parts) < 2 {
		return ""
	}
	if h := parts[len(parts)-1]; isLegacyHash(h) {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "::")
}

// isLegacyHash reports whether a component is the trailing "h<16 hex>" the
// legacy scheme appends to make a symbol unique.
func isLegacyHash(s string) bool {
	if len(s) != 17 || s[0] != 'h' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}
