//go:build growbig

package main

// FIVE MEGABYTES, taken half a megabyte at a time -- the size the sharded
// representation needs a save/load round trip over.
//
// It is chosen for its SHAPE, not for being large. 5 MiB crosses the 2 MiB and
// 4 MiB shard boundaries and ends inside a third shard rather than on its edge,
// so the memory a save carries is a vector of three word tables of which the
// last is PARTIAL -- and the guest writes every byte of it, so an absent or
// short shard is a wrong answer rather than a coincidence.
//
// It also crosses what used to be the 4 MiB wall, where a Lua table in Factorio
// stopped behaving like an array. Nothing here can reach that any more, which
// is the point: this guest is the round trip proving it.
//
// Half-megabyte blocks so the whole heap is taken in ten ticks, well before the
// save tick.
const (
	blockSize = 512 << 10
	blocks    = 10
)
