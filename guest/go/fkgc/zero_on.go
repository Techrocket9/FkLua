//go:build gc.custom && !fkgcnozero

package fkgc

// zeroOnAlloc is the contract: runtime.alloc must return zeroed memory.
//
// The `fkgcnozero` tag turns it off and produces a guest that is WRONG -- a
// recycled block arrives holding the previous object's bytes, and a Go value
// whose zero value matters (a nil pointer, a false, a zero length) arrives
// holding whatever was there. It exists for one reason: -gc=leaking on wasm
// does not zero at all (its zero_new_alloc is a documented no-op, because
// linear memory starts zero and a bump allocator never hands the same byte out
// twice), so the only way to say how much of a collector's allocation cost is
// ZEROING rather than free-list bookkeeping is to build an arm without it.
//
// Nothing but scratchpad/gc sets it.
const zeroOnAlloc = true
