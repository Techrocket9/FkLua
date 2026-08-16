package analysis

import (
	"strings"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Intrinsic names a guest function the runtime can do far better itself.
type Intrinsic int

const (
	// NotIntrinsic is every function that has to be compiled normally.
	NotIntrinsic Intrinsic = iota
	// IntrinsicCopy is C `memcpy`/`memmove`: (dst, src, n) -> dst.
	IntrinsicCopy
	// IntrinsicFill is C `memset`: (dst, byte, n) -> dst.
	IntrinsicFill
)

// NativeIntrinsic reports a function whose whole body can be replaced by the
// runtime's own `mem_copy`/`mem_fill`.
//
// The prize is large and it is entirely about WHERE the loop runs. A guest
// toolchain that cannot emit bulk memory ships compiler-rt's `memcpy` as a
// hand-rolled byte-and-word loop, and that loop compiles to Lua and runs one
// interpreted iteration per word. `mem_copy` is a Lua loop over the word table
// with one bounds check and one dirty-page update for the whole span, measured
// at 3.5 ns/byte against 173 for a byte loop.
//
// **Measured: 0.252x on a copy-heavy TinyGo guest** -- a 3.97x speedup, taking a
// 64 KiB `copy()` repeated 400 times from 420 ms to 106 ms, checksum-compared.
// **And flat on every kernel in `bench/guests`**: `real_grid` 1.008x and
// `real_names` 0.988x, both inside their A/A floor, because those copy tens of
// bytes at a time and pay call overhead rather than per-byte cost. That is the
// same split `agents/guests.md` records for the TinyGo bulk-memory custom
// target, which reaches the same place by making the guest emit `memory.copy`
// -- except this needs nothing installed and no $TINYGOROOT packaging.
//
// # Why this is allowed to look at a name at all
//
// The name section is a CUSTOM section: it carries no semantics, a producer may
// omit it, and nothing in the spec stops a module calling any function
// anything. So a name alone must never change what a program computes, and it
// does not here -- it only selects a CANDIDATE, which then has to survive a
// structural check that the body really is a self-contained memory shuffle.
//
// What the substitution changes, stated exactly, because "it is obviously the
// same" is how this kind of change goes wrong:
//
//   - **Overlap.** C `memcpy` is undefined on overlapping ranges; `mem_copy` has
//     `memory.copy`'s memmove semantics and is defined. Strictly more defined,
//     never less -- which is also why `memmove` is accepted under the same name
//     set.
//   - **Trap timing.** The byte loop writes bytes and then traps when it reaches
//     one that is out of range; `mem_copy` bounds-checks the whole span first
//     and writes nothing. Only reachable from a guest that was already going to
//     trap, and the spec's own `memory.copy` takes the check-first side.
//   - **The dirty-page mark.** `mem_copy` and `mem_fill` mark their whole
//     span in one update. That is the same guarantee the byte loop gave through
//     `st8b`, so `--persist=packed` is unaffected.
//   - **Nothing else.** Both return their first argument, as C does.
func NativeIntrinsic(f *ir.Func) Intrinsic {
	if f == nil || f.Unsupported != nil {
		return NotIntrinsic
	}
	// The two decode paths disagree about the leading `$`: a binary module's
	// name section stores `memcpy`, while a .wat identifier keeps the sigil it
	// is written with. That is pre-existing and only cosmetic everywhere else,
	// but here it decides whether a function is recognised at all, so both
	// spellings are accepted rather than only whichever one the tests use.
	var want Intrinsic
	switch strings.TrimPrefix(f.Name, "$") {
	case "memcpy", "memmove":
		want = IntrinsicCopy
	case "memset":
		want = IntrinsicFill
	default:
		return NotIntrinsic
	}
	// (i32, i32, i32) -> i32, exactly. C's signature returns the destination,
	// and a guest that has been through wasm-opt keeps it.
	if len(f.Params) != 3 || len(f.Results) != 1 {
		return NotIntrinsic
	}
	for _, p := range f.Params {
		if p != wasm.I32 {
			return NotIntrinsic
		}
	}
	if f.Results[0] != wasm.I32 {
		return NotIntrinsic
	}
	if !isMemoryShuffle(f) {
		return NotIntrinsic
	}
	return want
}

// isMemoryShuffle reports a self-contained leaf that does nothing but move bytes
// around linear memory.
//
// It cannot prove the function IS memcpy -- that would need to execute it, and
// this project deliberately has no wasm interpreter to execute it with. What it
// does is refuse everything a memory-shuffling leaf cannot contain, so a
// function that merely shares the name has to also be shaped like one before its
// body is thrown away: no call of any kind, nothing touching a global, no float
// arithmetic, no memory.grow, and at least one store, because a `memcpy` that
// writes nothing is not one.
func isMemoryShuffle(f *ir.Func) bool {
	stores := 0
	for i := range f.Steps {
		switch op := f.Steps[i].Op; op {
		case wasm.OpCall, wasm.OpCallIndirect,
			wasm.OpGlobalGet, wasm.OpGlobalSet,
			wasm.OpMemoryGrow, wasm.OpMemorySize,
			wasm.OpMemoryCopy, wasm.OpMemoryFill:
			// A body that already uses bulk memory needs no help, and one that
			// calls out or reads a global is not a leaf.
			return false
		case wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
			wasm.OpI64Store, wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32:
			stores++
		case wasm.OpF32Store, wasm.OpF64Store:
			return false
		default:
			if isFloatOp(op) {
				return false
			}
		}
	}
	return stores > 0
}

// isFloatOp reports an operation that observes or produces a float. A byte
// mover has no reason to contain one, and their presence is the cheapest signal
// that a same-named function is doing something else.
func isFloatOp(op wasm.Op) bool {
	switch op {
	case wasm.OpF32Load, wasm.OpF64Load,
		wasm.OpF32Const, wasm.OpF64Const,
		wasm.OpF32Add, wasm.OpF32Sub, wasm.OpF32Mul, wasm.OpF32Div,
		wasm.OpF64Add, wasm.OpF64Sub, wasm.OpF64Mul, wasm.OpF64Div:
		return true
	}
	return false
}
