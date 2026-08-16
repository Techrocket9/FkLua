//go:build gb16

package main

// 16 MiB: where fkgc's deleted HeapCap used to trap, and one doubling step for
// a leaking guest.
const targetBlocks = 16 * 1048576 / (blockWords * 4)
