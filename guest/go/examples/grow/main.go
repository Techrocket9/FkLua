// Command grow is the round-trip's growing guest.
//
// It exists because two save/load bugs in a row hid behind a guest whose heap
// never moved. M10 found that a memory.grow was never written back in table
// mode; the audit found the same thing again in packed mode, plus a restore
// that dropped pages past the first hole in a sparse save. Both times
// scripts/run-roundtrip.sh was green, because `hello` allocates a few hundred
// bytes and TinyGo's initial linear memory is far larger than that.
//
// So this one allocates past the initial heap on purpose, writes to every page
// it takes, and checks the bytes are still there after the load. What the
// script greps for is the same `seen=` counter `hello` reports, so the
// discriminator is unchanged: a guest that kept its state continues counting,
// and one that lost it restarts from zero.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o grow.wasm ./examples/grow
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// blockSize and blocks live in size_small.go / size_big.go behind a build tag,
// the same shape examples/gcbench uses for nlive.
//
// The DEFAULT is a megabyte: 16 blocks of 64 KiB, and TinyGo's wasm-unknown
// heap starts far below that -- so the allocator really does reach memory.grow
// rather than merely coming close to it. They are taken over the first ticks,
// well before the save.
//
// `-tags growbig` takes 5 MiB instead, which is what the SHARDED memory needs
// covered and the 1 MiB build cannot reach: a heap that crosses two shard
// boundaries and ends in a PARTIAL third shard, so the save carries a vector of
// three tables rather than one and the load has to rebuild it that way. Both
// persist modes get it wrong differently -- table mode's storage.fk_mem aliases
// the vector, packed mode's restore has to CREATE the shards the saved size
// implies -- and neither failure is reachable at 1 MiB.

var (
	heap [][]byte
	seen uint32
)

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("grow guest: heap starts here")
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++

	if len(heap) < blocks {
		b := make([]byte, blockSize)
		// Written, not just allocated. An untouched page is legitimately absent
		// from a packed save and comes back as zeros, so a guest that only
		// allocated would prove nothing about whether the save carried it.
		fill := byte(len(heap) + 1)
		for i := range b {
			b[i] = fill
		}
		heap = append(heap, b)
	}

	if tick%10 == 0 {
		fk.Log("tick " + strconv.FormatUint(uint64(tick), 10) +
			" seen=" + strconv.Itoa(int(seen)) +
			" blocks=" + strconv.Itoa(len(heap)) +
			" intact=" + strconv.Itoa(intact()))
	}
}

// intact counts the blocks whose bytes are still what was written into them.
//
// Every byte, not a spot check at the edges: the packed-mode bug lost whole
// pages out of the middle of a heap while the first and last were fine.
func intact() int {
	n := 0
	for i, b := range heap {
		want := byte(i + 1)
		ok := true
		for _, v := range b {
			if v != want {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// TinyGo builds this as a c-shared reactor, so main never runs.
func main() {}
