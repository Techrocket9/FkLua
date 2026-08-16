// Command gcsave is the collector's save/load guest: does a heap that is being
// COLLECTED survive a real Factorio save and reload?
//
// scripts/run-roundtrip.sh already runs `hello` (state survives at all) and
// `grow` (a heap that outgrew its initial linear memory comes back whole). This
// is the third leg, and it exists because the collector puts a second kind of
// state into linear memory and neither of the other two can see it.
//
// What has to survive here is not just the guest's own data. It is the
// ALLOCATOR'S OWN BOOKKEEPING -- the span table, the mark bitmap, the free-run
// lists, the class cursors. agents/gc.md says that carriage is free, in table
// and packed alike, and the reason is that every one of those lives in guest
// memory rather than in a Lua structure beside it:
//
//	A save mid-collection carries the collection, for free, in `table` and
//	`packed` alike -- the bitmap, the gray list, the free lists and the
//	allocator's bookkeeping are all guest memory.
//
// "For free" is a claim about a design, and this is the guest that makes it a
// measurement. The failure it is looking for is specific and quiet: a heap that
// reloads with a free run pointing at a live object, or a span table that
// disagrees with the bytes, does not trap. It hands the same block out twice,
// some ticks later, and the guest reads a value it never wrote.
//
// THE REPORT LINE is the shape run-roundtrip.sh greps for -- `tick N seen=M`
// -- plus what only this guest can say. `intact` is the one that matters: every
// retained block is checksummed against what was written into it, so a block
// that was reclaimed while live shows up as a number that moved rather than as
// a crash.
//
//	tick 70 seen=71 live=2048 cycles=9 phase=1 steps=14 blocks=32 intact=32
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
//	    -o gcsave.wasm ./examples/gcsave
//	fklua mod gcsave.wasm --gc=collected --persist=table
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkgc"
)

// blocks is the retained set: a fixed number of slots, each holding a block
// whose contents are derived from the tick that wrote it. Rewriting one slot
// per tick means there is always both a live set to keep and fresh garbage to
// reclaim, which is what makes a collection between ticks do real work.
const nblocks = 32

type block struct {
	tick uint32
	sum  uint32
	data []uint32
}

var lastPhase uint32

var (
	kept  [nblocks]*block
	seen  uint32
	churn []string
)

func u(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// write fills slot i with a block derived from tick, and remembers the checksum
// so a later read can prove the bytes are still the ones that were written.
func write(i, tick uint32) {
	n := 8 + (tick % 24)
	b := &block{tick: tick, data: make([]uint32, n)}
	var s uint32
	for k := uint32(0); k < n; k++ {
		b.data[k] = tick*2654435761 + k
		s = s*31 + b.data[k]
	}
	b.sum = s
	kept[i] = b
}

// intact counts the retained blocks whose contents still hash to what was
// written. This is the whole assertion: a collector that reclaimed a live block
// does not make it unreadable, it makes it ZERO and then somebody else's.
func intact() uint32 {
	var ok uint32
	for _, b := range kept {
		if b == nil {
			continue
		}
		var s uint32
		for _, v := range b.data {
			s = s*31 + v
		}
		if s == b.sum {
			ok++
		}
	}
	return ok
}

// garbage is the allocation that gives the collector something to reclaim,
// written the way agents/guests.md tells authors NOT to write it -- which is
// the point, since the whole argument for a collector is that they should not
// have to.
func garbage(tick uint32) {
	churn = churn[:0]
	for k := uint32(0); k < 12; k++ {
		churn = append(churn, "gcsave-"+u(tick)+"-"+u(k))
	}
}

func live() uint32 { return fkgc.Stats().LiveBytes }

//go:wasmexport fk_on_init
func onInit() {
	// DELIBERATELY AGGRESSIVE, and a real mod should not copy this line. The
	// default threshold is 256 KiB of heap footprint taken since the last
	// collection, which for a guest this small is never -- and a roundtrip that
	// saves a heap no collection has touched proves nothing about carrying one.
	// 8 KiB gets several full cycles into the 60 ticks before the save and
	// several more after it.
	fkgc.SetThreshold(8 << 10)
	// AND A SMALL STEP BUDGET, so that a collection is spread over enough ticks
	// that a save taken at an arbitrary one lands in the MIDDLE of it -- which
	// is the whole thing this guest exists to prove is survivable. The default
	// is 1024 granules, calibrated to ~0.5 ms of collector time per tick; 512
	// halves that. A real mod leaves it alone.
	//
	// Do not lower it further without re-reading fkgc's markDeadline. Below
	// about 256 a step cannot re-scan even one 4 KiB page dirtied since the
	// last one, so marking stops making progress toward termination and only
	// the deadline gets it out -- which works, and is a pause, and is not what
	// this guest should be demonstrating.
	fkgc.SetBudget(512)
	for i := uint32(0); i < nblocks; i++ {
		write(i, i+1)
	}
	fk.Log("[gcsave] " + u(nblocks) + " blocks retained, collector " +
		map[bool]string{true: "ON", false: "OFF"}[fkgc.Enabled()])
}

// fk_after_load is the first tick after a save is loaded, and then it is gone.
//
// SINCE STAGE C THIS IS WHERE THE EVIDENCE IS. A collection can now be half done
// when a save is taken, and what has to come back is two different things.
//
// The collector's own state -- phase, mark bitmap, gray stack, sweep cursor,
// free runs -- is all linear memory and comes back with it, which is what
// `phase` here reports: a 1 or a 2 means the save landed INSIDE a collection and
// the guest resumed it rather than starting over.
//
// What did not come back is the write barrier and the page set. MEMDIRTY is a
// chunk LOCAL, so a guest that was marking when the save was taken resumes with
// it false unless control.lua re-arms from `storage.fk_gc` -- which it does --
// and the dirty page set is a Lua table no `storage` entry mirrors, so the first
// step after the load is told the record was lost and re-scans everything it had
// marked. `intact` is what says all of that worked: a block reclaimed while live
// does not trap, it comes back zero and then somebody else's.
//
// fk_gc_budget lets the harness sweep the pacing knob without a rebuild.
//
//go:wasmexport fk_gc_budget
func gcBudget(units uint32) uint32 {
	fkgc.SetBudget(units)
	return fkgc.Budget()
}

// fk_gc_intact and fk_gc_stat are the harness's window on this guest: how many
// retained blocks still hold what was written into them, and the collector's
// own view of itself by index (the same indices churn and gctorture use).
//
//go:wasmexport fk_gc_intact
func gcIntact() uint32 { return intact() }

//go:wasmexport fk_gc_stat
func gcStat(which uint32) uint32 {
	s := fkgc.Stats()
	switch which {
	case 0:
		return s.HeapBytes
	case 1:
		return s.LiveBytes
	case 3:
		return s.Collections
	case 4:
		return s.Grows
	case 9:
		return s.Phase
	case 10:
		return s.Steps
	case 14:
		return s.Deadlines
	}
	return 0
}

//go:wasmexport fk_after_load
func afterLoad() {
	s := fkgc.Stats()
	fk.Log("[gcsave] loaded: " + u(intact()) + "/" + u(nblocks) +
		" blocks intact, " + u(s.Collections) + " collections so far, phase=" +
		u(s.Phase))
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++
	garbage(tick)
	write(tick%nblocks, tick)

	// The collection happens BETWEEN guest calls in the only sense available
	// here -- at the end of one, before the engine calls anything else. That is
	// where agents/gc.md section 1 allows it: the wasm operand stack is empty
	// between exported calls and the shadow stack is back at its initial value.
	// Pressure-gated, so an idle tick costs a comparison.
	//
	// ...and then started again the moment it is not, which is the second
	// DELIBERATELY AGGRESSIVE line in this file and the reason it is here: a
	// save/load roundtrip has to land INSIDE a collection to prove that a
	// half-done one is carried, and a guest that collects for seven ticks out of
	// every twenty is a guest whose save tick mostly misses. Collecting
	// continuously makes the interesting case the common one. A real mod must
	// not do this -- it keeps the write barrier armed forever, which is
	// agents/gc.md's measured 7-13% on stores, for a heap this small.
	if !fkgc.CollectIfNeeded() {
		fkgc.Start()
	}

	// A LINE WHENEVER THE PHASE CHANGES, because the roundtrip leg has to choose
	// save ticks that land INSIDE a collection and the only honest way to choose
	// them is to read where the phases are. The cadence of a collection is a
	// property of the collector and it moves when the collector does; a tick
	// constant guessed against an old cadence fails as "the save did not cross
	// one", which reads like a persistence bug and is not.
	if p := fkgc.Phase(); p != lastPhase {
		lastPhase = p
		fk.Log("phase " + u(tick) + " -> " + u(p) +
			" cycles=" + u(fkgc.Stats().Collections))
	}

	if tick%10 == 0 {
		s := fkgc.Stats()
		fk.Log("tick " + u(tick) + " seen=" + u(seen) +
			" live=" + u(s.LiveBytes) +
			" cycles=" + u(s.Collections) +
			" grows=" + u(s.Grows) +
			" deadlines=" + u(s.Deadlines) +
			" marked=" + u(fkgc.Marked()) +
			" stalls=" + u(fkgc.Stalls()) +
			" maxstalls=" + u(fkgc.MaxStalls()) +
			" owed=" + u(fkgc.WorkOwed()) +
			" pempty=" + u(fkgc.PendEmpties()) +
			" terms=" + u(fkgc.Terminations()) +
			" phase=" + u(s.Phase) +
			" steps=" + u(s.Steps) +
			" blocks=" + u(nblocks) + " intact=" + u(intact()))
	}
}

func main() {}
