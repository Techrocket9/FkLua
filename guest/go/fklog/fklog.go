// Package fklog builds a log line in one fixed buffer and hands it to the host
// without allocating.
//
// WHY THIS IS A LIBRARY AND NOT ADVICE. At least nine guests across two
// languages have hand-rolled the same thing, because the obvious way is wrong
// in a way nothing reports: `fk.Log("x=" + strconv.Itoa(n))` allocates every
// intermediate string, and under `-gc=leaking` -- which is the default and is
// mandatory for a guest with no collector -- every one of them is PERMANENT, in
// the guest's linear memory, in every save and in every multiplayer join. `+`
// in a loop is quadratic on top of that. One downstream mod measured its ENTIRE
// guest heap as log lines: 64 MiB of linear memory and a 19.9 ms idle worst
// tick, against under 16 MiB and 2.3 ms once the lines were built here instead
// (BetterBeltBalancer, `guest/go/logline.go`, whose header carries the table).
// And one of the nine copies has already grown a real ROUNDING divergence from
// its twin, which is what a shared library is for -- F1's carry below is that
// class of defect, and the mirror test is what would catch it here.
//
// OPT-IN, AND NOT WIRED INTO fk. A guest that logs nothing must not link a
// buffer, and a guest that wants `fmt` is entitled to it -- this is for the one
// that has measured why it cannot.
//
// IT DOES NOT IMPORT fkapi, deliberately. fkapi is generated, pinned to one API
// description and stamped with it; a hand-written library that imported it would
// drag the pin into every consumer that only wanted a line builder. What it
// needs from the runtime is one function, `fk.Log`. [Value.Dump] is the other
// side of that boundary and lives in fkapi, writing into a buffer this package
// lends it through [Tail] and [Advance].
//
// USAGE:
//
//	fklog.Start("[mymod] compiled cluster ")
//	fklog.U(uint64(root))
//	fklog.S(" parts=")
//	fklog.U(uint64(n))
//	fklog.End()
//
// A CALL SITE MAY NOT MAKE A HOST CALL BETWEEN Start AND End. There is one
// buffer, and a synchronously-raised event whose handler logged would interleave
// with a line that is half built. Every call site builds its line in one
// uninterrupted run; that is an invariant of the caller, because a guard would
// cost a branch on every append to catch a mistake a reader can see.
package fklog

import (
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// Cap is the buffer's size, and a line longer than it is TRUNCATED rather than
// grown.
//
// `copy` into a fixed array is one memcpy with no reallocation path behind it,
// where `append` on a slice carries a growth branch that the compiler inlines
// into every call site. Measured downstream over ~200 of them: 98,373 bytes of
// wasm `code` with `append` against 81,457 with `copy`, which is 1,339,988
// bytes of generated Lua against 1,035,941. Truncation is the price and it is
// the right one: a log line is a diagnostic, and a diagnostic that costs 300 KB
// of every mod is a worse diagnostic.
const Cap = 512

var buf [Cap]byte
var n int

// digits is package level for the same reason the buffer is: a local array whose
// address is taken is not reliably stack-promoted under TinyGo, and a byte that
// is not reliably on the stack is a byte in every save.
var digits [20]byte

// Start opens a line, discarding anything half-built.
func Start(s string) {
	n = 0
	S(s)
}

// S appends a string.
func S(s string) { n += copy(buf[n:], s) }

// B appends "true" or "false".
func B(v bool) {
	if v {
		S("true")
		return
	}
	S("false")
}

// U appends an unsigned integer in base 10.
func U(v uint64) {
	i := len(digits)
	for {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	n += copy(buf[n:], digits[i:])
}

// I appends a signed integer in base 10.
func I(v int64) {
	if v < 0 {
		S("-")
		// NEGATED AS UNSIGNED, and the reason is Rust rather than Go: `-v` at
		// the most negative value is an overflow, which Go DEFINES as the
		// two's-complement wrap (so `uint64(-v)` and this agree, measured) and
		// Rust does not (a debug build panics; a release build wraps). Writing
		// the form that is defined in both is what keeps the twins one program.
		U(-uint64(v))
		return
	}
	U(uint64(v))
}

// F1 appends a number to one decimal place, rounded half away from zero.
//
// ONE DECIMAL AND NOT strconv.FormatFloat, which links a large chunk of
// formatting code into a guest that has no other use for it. A guest that needs
// real float formatting should say so and pay for it; what this covers is the
// common case, a tile coordinate or a ratio in a diagnostic.
func F1(v float64) {
	if v < 0 {
		S("-")
		v = -v
	}
	whole := uint64(v)
	frac := uint64((v-float64(whole))*10 + 0.5)
	// 9.96 rounds to 10.0 and not to 9.10, which is what a naive carry-free
	// version prints.
	if frac >= 10 {
		whole++
		frac -= 10
	}
	U(whole)
	S(".")
	U(frac)
}

// End hands the buffer to the host as a string that BORROWS it.
//
// `unsafe.String` rather than `string(buf[:n])`, which would COPY -- and a copy
// here is exactly the permanent allocation this package exists to remove. It is
// safe because the host copies the bytes into a Lua string before `fk.Log`
// returns, so the Go string never outlives the call. `buf` is a package-level
// array, so its address is a static and never nil.
func End() { fk.Log(unsafe.String(&buf[0], n)) }

// Line is Start plus End, for a message with nothing appended to it.
func Line(s string) {
	Start(s)
	End()
}

// Len is how many bytes the line holds.
func Len() int { return n }

// Tail LENDS the rest of the buffer to a caller that writes into it directly,
// and Advance is how that caller says how much it wrote.
//
// This is the seam [Value.Dump] uses, and it is what keeps fklog free of fkapi:
// the dumper writes bytes into a destination it was handed and returns a count,
// and nothing about it knows where the destination came from.
//
//	fklog.Start("v=")
//	fklog.Advance(v.Dump(fklog.Tail()))
//	fklog.End()
//
// The slice is empty when the line is already full, which is the same
// truncation every other appender here does.
func Tail() []byte { return buf[n:] }

// Advance records that Tail's first `k` bytes were written. A count past the
// end is clamped, so a dumper that miscounted truncates rather than handing the
// host a length past the buffer.
func Advance(k int) {
	if k < 0 {
		return
	}
	if n+k > Cap {
		k = Cap - n
	}
	n += k
}
