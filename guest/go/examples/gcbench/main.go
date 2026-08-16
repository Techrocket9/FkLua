// Command gcbench is the acceptance vehicle for what a collection costs the
// WORST TICK, in a real Factorio, measured by Factorio.
//
// agents/gc.md's host-side gate DERIVES the paced worst tick from a whole
// collection's time and the fraction of its work that lands in one step,
// because bin/lua52f is patched to Factorio's shape and has no clock. This
// guest needs no derivation -- but reading it takes an instrument that reports
// PER TICK, which stage C did not have and stage D built:
// `--benchmark-verbose <counters>` emits one CSV row per tick, so the load tick
// is a row to drop rather than a maximum that swallows everything.
//
// TWO ARMS FROM ONE SOURCE, which is what makes the comparison mean anything:
//
//	(default)      -gc=custom, PACED -- one bounded step per tick
//	-tags gcstw    -gc=custom, STOP THE WORLD -- fkgc.Collect() in fk_on_tick
//
// A -gc=leaking baseline arm was tried and removed: under leaking this guest
// keeps every node it ever allocates, so it measures the thing the feature
// exists to prevent rather than a floor to subtract. The arms above share an
// allocator, an emitted chunk and a trigger, so everything that is not the
// collection cancels between them.
//
// THE LIVE SET IS THE PARAMETER, because a stop-the-world pause is a function
// of heap size and nothing else: agents/gc.md's stage-B table puts a 2.39 MiB
// heap at 32.39 ms. nlive is sized to land in that neighbourhood, so the STW
// arm is reproducing a row of that table inside the game rather than in a
// harness -- and stage D's finding is that the row does NOT reproduce, by a
// factor this file cannot explain and gc.md records as open.
//
// THE MUTATOR LOAD IS ALSO A PARAMETER, and getting it wrong does not make the
// benchmark slow, it makes the paced arm LIVELOCK. See perTick.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
//	    -o gcbench.wasm ./examples/gcbench
//	fklua mod gcbench.wasm --gc=collected
//
// scripts/run-gcbench.sh builds both arms and prints the distribution.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkgc"
)

// node is gctorture's, deliberately: 48 bytes, a size class with a tail, so the
// slot arithmetic is exercised rather than a shift.
type node struct {
	id    uint32
	tag   uint32
	left  *node
	right *node
	pad   [5]uint32
}

// nlive is the retained set, and since the sharding work it is a BUILD TAG,
// because the whole reason to vary it is to put the guest's linear memory
// either side of the 4 MiB wall. See size_small.go and size_big.go.
//
// agents/gc.md's stage-D open item 1 is a stop-the-world collection costing
// 88 ms/MiB in game against a host-side 13.9-32.8, with the recorded lead being
// "the guest heap is a Lua TABLE and its cost is its SIZE" -- specifically, that
// gcbench's heap was past 2^20 words where a Factorio table stops being an
// array. This pair is the A/B that tests that lead, and it is the only
// instrument that can: bin/lua52f is stock 5.2.1 and has no wall.

// perTick is how much ordinary garbage a tick makes. It is the mutator load the
// collector has to keep up with, and it is what STAGE D HAD TO CHANGE.
//
// It was 200 nodes, described as "deliberately modest". It is not modest and
// the arithmetic was never done: 200 x 48 B at 60 UPS is 576 KB/s, against the
// 189 KB/s of sustained reclaim agents/gc.md measured for the default 0.5 ms
// budget and published as this feature's replacement acceptance criterion. The
// benchmark was configured THREE TIMES OUTSIDE the envelope the design
// document had already written down, and the consequence was not a slow arm:
// the paced arm LIVELOCKED -- 600 ticks in phase 1, cycles=0, the heap climbing
// 2.85 -> 8.68 MB exactly as if there were no collector, with deadlines=0
// because markDeadline's escape is 1,296 steps at this heap and the benchmark
// was shorter than that. The old instrument reported avg/min/max per run and
// could not see any of it.
//
// 20 nodes is 960 B/tick, i.e. 57.6 KB/s -- inside the envelope with the same
// 3x headroom gc.md claims for an ordinary event handler.
const perTick = 20

// threshold is the span pressure that triggers a collection, and BOTH ARMS USE
// IT so that the only difference between them is how the collection runs.
//
// 256 KiB at 960 B/tick is a collection roughly every 273 ticks: often enough
// that a 1,200-tick benchmark sees several, rare enough that the paced arm
// finishes one before the next is due. It was 2 MiB, which at the old
// allocation rate was ~218 ticks -- but a 2 MiB float on top of a 2.2 MiB live
// set is what took the heap over the Lua word table's 2^20-entry boundary, and
// crossing that boundary is a 2.8-SECOND tick in real Factorio (see
// agents/gc.md, stage D).
const threshold = 256 << 10

var (
	live    []*node
	sink    *node
	seen    uint32
	scratch uint32

	// bulk is the past-the-old-cap arm's live set, in one-megabyte blocks. See
	// size_huge.go for why it is bulk rather than nodes, and bulkSum for what
	// makes it a correctness gate rather than a size.
	bulk    [][]uint32
	bulkSum uint32
)

// bulkWords is a megabyte. Each block is a run of 256 spans, which is the
// allocation arm a metadata chunk can block -- see fkgc's growCoverage.
const bulkWords = 262144

// buildBulk fills the blocks with a position-dependent pattern and checksums
// them. verifyBulk re-derives the same number from what is in memory now.
//
// EVERY SIXTEENTH WORD, because this runs in the game: a full walk of 36 MiB is
// nine million iterations inside one tick and would be the thing being measured.
// No failure of this collector is confined to fifteen words in sixteen -- a span
// is the unit of everything it does and a span is 1,024 words.
func buildBulk() {
	bulk = make([][]uint32, nbulk)
	for b := 0; b < nbulk; b++ {
		s := make([]uint32, bulkWords)
		k := uint32(b) + 1
		for i := uint32(0); i < bulkWords; i++ {
			s[i] = k*2654435761 + i
		}
		bulk[b] = s
	}
	bulkSum = verifyBulk()
}

func verifyBulk() uint32 {
	var acc uint32
	for b, s := range bulk {
		want := uint32(b) + 1
		for i := 0; i < len(s); i += 16 {
			acc = acc*31 + s[i]
			acc = acc*31 + want
		}
	}
	return acc
}

func u(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

//go:wasmexport fk_on_init
func onInit() {
	live = make([]*node, nlive)
	for i := uint32(0); i < nlive; i++ {
		live[i] = &node{id: i, tag: i * 2654435761}
	}
	// Both arms use the same trigger, so the only difference between them is
	// HOW the collection runs. The budget is left at the default 1024 granules
	// ON PURPOSE: what this benchmark is for is what a guest gets without
	// tuning, and stage D's finding is that the default is only usable if the
	// mutator stays inside the reclaim rate gc.md priced it at.
	if nbulk > 0 {
		buildBulk()
	}
	fkgc.SetThreshold(threshold)
	fk.Log("[gcbench] " + u(nlive) + " nodes live, " + u(nbulk) + " MiB bulk, mode=" + mode +
		", collector " + map[bool]string{true: "ON", false: "OFF"}[fkgc.Enabled()])
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++
	// THE DROP, once, on the first tick of the benchmark rather than in on_init:
	// on_init runs at --create and its heap is what the map saves, so dropping
	// there would save a map that never grew. See size_huge.go.
	if nbulk > nbulkkeep && seen == 1 {
		bulk = bulk[:nbulkkeep]
		bulkSum = verifyBulk()
	}
	// Steady ordinary allocation, and a rewrite of one slot of the live set so
	// the retained graph really changes -- which is what makes the write
	// barrier do work rather than watch.
	for i := uint32(0); i < perTick; i++ {
		g := &node{id: tick ^ i, tag: (tick + i) * 40503}
		sink = g
		scratch += g.tag
	}
	live[tick%nlive] = &node{id: tick, tag: tick * 2654435761}

	collect()

	if tick%100 == 0 {
		s := fkgc.Stats()
		// THE BULK CHECKSUM IS RE-DERIVED HERE and not at the end, because the
		// run that matters is a --benchmark that never saves and never gets an
		// end. A collector that reclaimed a live block shows up as this number
		// moving, and nothing else about the run would look wrong.
		ok := uint32(1)
		if nbulk > 0 && verifyBulk() != bulkSum {
			ok = 0
		}
		fk.Log("[gcbench] tick " + u(tick) + " mode=" + mode +
			" bulkok=" + u(ok) +
			" meta=" + u(s.MetaBytes) +
			" outruns=" + u(s.Outruns) +
			" stalls=" + u(fkgc.MaxStalls()) +
			" pempty=" + u(fkgc.PendEmpties()) +
			" maxstep=" + u(fkgc.MaxStepWork()) +
			" maxunpaced=" + u(s.MaxUnpaced) +
			" folds=" + u(fkgc.MaxUnpacedFolds()) +
			" budget=" + u(fkgc.Budget()) +
			" heap=" + u(s.HeapBytes) +
			" live=" + u(s.LiveBytes) +
			" cycles=" + u(s.Collections) +
			" steps=" + u(s.Steps) +
			" grows=" + u(s.Grows) +
			" deadlines=" + u(s.Deadlines) +
			" phase=" + u(s.Phase) +
			" since=" + u(s.SinceGC) +
			" sum=" + u(scratch))
	}
}

func main() {}
