// Command growbench is the acceptance vehicle for what a `memory.grow` costs
// the WORST TICK, in a real Factorio, measured by Factorio.
//
// WHY IT IS NOT gcbench. gcbench measures a COLLECTION and its arms are sized
// to put a collection where a pause would be felt. Sharding stage C found that
// the worst tick of a growing guest is not the collection at all -- the
// collector's worst paced step is 1.2x its budget and the worst TICK is
// 22.7-30.0 ms at a 3.5 MiB heap and 288-365 ms at 40 MiB -- and attributed the
// difference to mem_grow's zero-fill at 107 ns a word. That attribution was
// arithmetic against gcbench's worst tick; this guest measures the thing
// directly, and it measures it on BOTH growth paths, which gcbench cannot
// because it requires fkgc.
//
//	(default)         -gc=leaking, --gc=leaking -- TinyGo's growHeap DOUBLES
//	-tags gbcollect   -gc=custom,  --gc=collected -- fkgc grows by a quarter
//
// THE TARGET IS A BUILD TAG (size_4.go / size_16.go / size_40.go) because the
// whole question is how the grow tick scales with the heap, and the two growth
// laws scale differently: a quarter of the heap against the whole of it.
//
// THE RATE IS DELIBERATE AND IS NOT A DETAIL. The runtime's pre-build is paced
// at 8,192 words -- 32 KiB -- of materialisation per tick, so a guest that
// allocates faster than that outruns it and falls back to filling inline. 8 KiB
// a tick is 480 KB/s, which is inside agents/gc.md's ~190 KB/s reclaim envelope
// by the same order the collector's own benchmark uses, and it leaves the
// pre-build 4x headroom. A guest that wants tens of megabytes NOW is the
// storm case, and the storm case is what the bounded increment is for; it is
// asserted host-side by TestAnAllocationStormGrowsInsteadOfCollectingSynchronously.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o growbench.wasm ./examples/growbench
//	fklua mod growbench.wasm --gc=leaking
//
// scripts/run-growbench.sh builds every arm and prints the distribution.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// memory.size, declared here rather than imported: fkgc has the same three
// lines and this guest must build under -gc=leaking too, where fkgc is not
// linked in at all.
//
//export llvm.wasm.memory.size.i32
func wasmMemorySize(index int32) int32

// blockWords is 8 KiB of uint32. Retained, and WRITTEN -- an allocation the
// guest never touches proves nothing about whether the words it was handed are
// real, and "the pre-built slot was never materialised" is exactly the failure
// this benchmark has to be able to see.
const blockWords = 2048

var (
	blocks [][]uint32
	sum    uint32
	seen   uint32
	grows  uint32
	pages  uint32
	done   bool
)

func u(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// stride is how sparsely verify samples a block, and it is a benchmark
// decision rather than a coverage one: this runs in the game, and a full walk
// of 40 MiB inside one tick would be the thing being measured rather than the
// grow. Four samples a block still catches every failure this instrument
// exists for -- a pre-built slot that was never materialised, a slot
// materialised at the wrong index, a grow that handed back somebody else's
// bytes -- because all of them are span-sized or larger and a span is 1,024
// words.
const stride = 512

// fold extends an in-order checksum by one block, so the running `sum` costs
// four operations per tick instead of a walk of the whole heap.
func fold(acc uint32, s []uint32, k uint32) uint32 {
	for i := 0; i < len(s); i += stride {
		acc = acc*31 + s[i]
		acc = acc*31 + k
	}
	return acc
}

// verify re-derives the same number from what is in memory NOW.
func verify() uint32 {
	var acc uint32
	for b, s := range blocks {
		acc = fold(acc, s, uint32(b)+1)
	}
	return acc
}

//go:wasmexport fk_on_init
func onInit() {
	pages = uint32(wasmMemorySize(0))
	armCollector()
	fk.Log("[growbench] target " + u(targetBlocks*blockWords*4) + " B, mode=" + mode +
		", initial memory " + u(pages*65536) + " B")
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++

	if len(blocks) < targetBlocks {
		s := make([]uint32, blockWords)
		k := uint32(len(blocks)) + 1
		for i := range s {
			s[i] = k*2654435761 + uint32(i)
		}
		blocks = append(blocks, s)
		sum = fold(sum, s, k)
	} else if !done {
		done = true
		fk.Log("[growbench] TARGET tick=" + u(tick) + " blocks=" + u(uint32(len(blocks))) +
			" mem=" + u(uint32(wasmMemorySize(0))*65536) + " grows=" + u(grows))
	}

	// A GROW LINE PER GROW, with the tick on it, because the whole measurement
	// is "which tick was the grow and what did that tick cost" and Factorio's
	// --benchmark-verbose reports per tick. Nothing else in the guest can say
	// which row of that CSV to look at.
	if p := uint32(wasmMemorySize(0)); p != pages {
		grows++
		fk.Log("[growbench] GROW tick=" + u(tick) + " from=" + u(pages*65536) +
			" to=" + u(p*65536) + " added=" + u((p-pages)*65536))
		pages = p
	}

	tickCollect()

	if tick%200 == 0 {
		ok := uint32(1)
		if verify() != sum {
			ok = 0
		}
		fk.Log("[growbench] tick " + u(tick) + " mode=" + mode +
			" ok=" + u(ok) +
			" blocks=" + u(uint32(len(blocks))) +
			" mem=" + u(uint32(wasmMemorySize(0))*65536) +
			" grows=" + u(grows) +
			" sum=" + u(sum))
	}
}

func main() {}
