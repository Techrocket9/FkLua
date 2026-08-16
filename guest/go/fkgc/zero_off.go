//go:build gc.custom && fkgcnozero

package fkgc

// See zero_on.go. A build with this tag is a MEASUREMENT ARM and is not
// correct: it isolates the cost of zeroing a recycled block from the cost of
// the free list that produced it.
const zeroOnAlloc = false
