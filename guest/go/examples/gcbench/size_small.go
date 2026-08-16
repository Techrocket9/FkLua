//go:build !gcbig && !gchuge

package main

// The UNDER-THE-WALL arm. 44,000 nodes at 48 bytes is about 2.0 MiB live, which
// with a cycle's float lands the heap near the 2.39 MiB row of agents/gc.md's
// stage-B pause table -- the row that costs 32.39 ms stopped -- and keeps the
// whole linear memory under 2^20 words, where a Factorio Lua table is still an
// array.
const nlive = 44000

// No bulk blocks: this arm's size is its node count. See size_huge.go.
const nbulk = 0
const nbulkkeep = 0
