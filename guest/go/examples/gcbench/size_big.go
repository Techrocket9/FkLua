//go:build gcbig && !gchuge

package main

// The OVER-THE-WALL arm. 110,000 nodes at 48 bytes is about 5.0 MiB live, which
// puts the whole linear memory past 4 MiB -- 2^20 words -- where Factorio's Lua
// abandons the array representation for the WHOLE table and every access costs
// ~20x more, the low indices included.
//
// This arm exists to be compared against size_small.go's, and the comparison is
// the one thing that can attribute the 88 ms/MiB stop-the-world cost. It is
// deliberately not "a bigger benchmark": everything else about the guest --
// perTick, threshold, the node shape, the budget -- is identical, so the only
// difference between the two arms is which side of the wall the word table is
// on. fk_mod.lua's own notice fires in this arm and is silent in the other,
// which is how a run says out loud which side it measured.
//
// It also needs the collector's heap cap to be out of the way: 5.0 MiB live in
// a heap that grows past it is fine under the default 16 MiB HeapCap, but this
// is the arm that would find that cap first, and agents/sharding.md's stage C
// is where the cap stops existing.
const nlive = 110000

// No bulk blocks: this arm's size is its node count. See size_huge.go.
const nbulk = 0
const nbulkkeep = 0
