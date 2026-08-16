package fkgc

// MemStats is what a guest can ask the collector about itself.
//
// Every field is bytes except the counters, and every field is exact rather
// than sampled: this allocator knows its own free lists and its own span
// assignment, so there is nothing here it has to estimate.
//
// It is deliberately NOT runtime.MemStats. That type is filled in through the
// ReadMemStats hook for whoever calls runtime.ReadMemStats, and its field names
// describe a heap organised the way Go's is. This one describes the heap this
// package actually has, and the field that matters most -- HeapBytes -- is the
// one agents/gc.md says the collector's success is measured by, because linear
// memory never shrinks.
type MemStats struct {
	// HeapBytes is the size of the region the allocator owns: every byte
	// between the heap base and the last byte the allocator has grown into.
	// This is the number that must stop growing. It is also, within one wasm
	// page, the guest's whole contribution to Factorio's 0.2 ms/MiB worst
	// tick.
	HeapBytes uint32

	// LiveBytes is the size class total of everything the last collection
	// retained. Between collections it is stale by construction -- it is what
	// was live when the sweep ran, not what is live now.
	LiveBytes uint32

	// FreeBytes is what the free lists and the unassigned spans hold: the
	// headroom before the next memory.grow.
	FreeBytes uint32

	// SinceGC is how many bytes of SPAN have been assigned since the last
	// collection finished -- heap footprint the guest has taken, not bytes it
	// has been handed. This is the number CollectIfNeeded tests against the
	// threshold, and the distinction is the point: a class recycling blocks a
	// collection reclaimed has not made the heap bigger, so it registers no
	// pressure and provokes no collection.
	SinceGC uint32

	// Collections is how many times a mark and sweep has completed.
	Collections uint32

	// LiveObjects and FreedObjects count objects the last sweep kept and
	// released.
	//
	// There is deliberately no "allocations so far" counter. A running total
	// is a read-modify-write on the allocation path -- -gc=leaking carries two
	// of them, as uint64 adds -- and stage B measured what a memory operation
	// costs there: this allocator gets under -gc=leaking's own allocation cost
	// only by carrying nothing per allocation at all. What a guest wants to
	// know is what the collector is keeping, and that is exactly what sweep
	// already counts.
	LiveObjects  uint32
	FreedObjects uint32

	// Grows is how many times memory.grow has been called. This is the
	// counter a stage-D acceptance run watches: agents/gc.md's test for the
	// churn guest is "no doubling logged", and a collector that works holds
	// this flat forever.
	Grows uint32

	// Phase is what the collector is doing right now: 0 idle, 1 marking,
	// 2 sweeping. A guest that logs this is logging the one thing a paced
	// collector has that a stop-the-world one does not -- a middle.
	Phase uint32

	// Steps is how many bounded steps the LAST completed collection took. It
	// is the pacing measurement: steps times the budget is roughly the work
	// the cycle did and steps is roughly the ticks it was spread over, so a
	// rising number against a flat heap means the budget is too small for the
	// guest's allocation rate and the heap is about to grow instead.
	Steps uint32

	// Outruns is how many times an allocation had to GROW the heap while a
	// collection was still in flight -- the mutator beating the pacer.
	//
	// It is not an error and it does not pause: since the fkgc heap cap was
	// removed the storm response is growth, so a guest that outruns its budget
	// behaves like -gc=leaking until the paced collection catches up. What it
	// costs is linear memory, which never shrinks. A number that climbs with
	// HeapBytes climbing beside it is a budget under the allocation rate.
	Outruns uint32

	// UnpacedWork and MaxUnpaced are collector work, in 16-byte granules, that
	// landed inside a GUEST CALL rather than inside a paced step: the bounded
	// sweep-ahead an allocation does before it grows, and the root scan of a
	// collection the guest started.
	//
	// They exist because MaxStepWork could not see any of it -- a step zeroes
	// its own accumulator on entry, so work charged between two steps was
	// discarded before it was ever compared to the budget. The host-side gate
	// read 1.17x of budget while a real Factorio showed 65x, and this pair is
	// that gap. A worst-tick claim needs both: one step, plus whatever the
	// handler in that same tick charged.
	UnpacedWork uint32
	MaxUnpaced  uint32

	// MetaBytes is the collector's own linear-memory footprint, and since
	// sharding stage C it SCALES WITH THE HEAP rather than being a compile-time
	// constant: a fixed part in .bss plus one 40 KiB chunk per 4 MiB slice of
	// heap. There is no fkgc heap cap any more, and this is what replaced it --
	// a cost that grows with what the guest asked for, rather than a wall.
	MetaBytes uint32

	// Deadlines counts the times mark termination stopped yielding to the
	// budget because it was making no progress -- see markDeadline in
	// collect.go. Zero is the expected value forever, so a number that rises
	// is a defect report about the guest's configuration rather than a
	// statistic.
	//
	// IT IS THE SUM OF THE TWO BELOW, exactly, and it is kept unchanged so
	// that every existing reader keeps reading the same number.
	Deadlines uint32

	// StepEscapes and StallEscapes are Deadlines SPLIT BY CAUSE, and the split
	// exists because one counter was reporting two diagnoses with two
	// different remedies.
	//
	//	StepEscapes   the mark ran past markDeadline -- 4 x (heap granules /
	//	              budget) + 600 steps. A BACKSTOP, deliberately far enough
	//	              out that a short run finishes first, so on its own it says
	//	              the mark is affordable but SLOW for the heap it is on.
	//	StallEscapes  the forward-progress window said the mark had stopped
	//	              converging: markStallLimit consecutive windows of
	//	              markStallWindow steps in which the pending dirty list
	//	              never emptied AND scan work did not shrink. A DIAGNOSIS,
	//	              and it fires far earlier than the backstop.
	//
	// THE REASON TO SPLIT THEM IS TWO REAL MISDIAGNOSES. `Deadlines` is
	// documented -- here, in SetBudget and in agents/gc.md -- as the signal
	// that the mutator has outrun the collector, and that reading sent the
	// first downstream mod's investigation at its own write rate for a day.
	// What was happening was the root scan costing more than one step's whole
	// budget (see rootScanMargin), at any allocation rate including zero. Then
	// the same mod's marathon suite showed six of these and they were written
	// down, in a document, as the write rate of the two legs they were counted
	// in -- one of which allocates 16 bytes per operation and could not outrun
	// anything.
	//
	// Neither story was about a write rate, and both would have read
	// differently against a split counter. This pair does not identify the
	// root-scan case by itself -- EffectiveBudget() > Budget() is what does
	// that, and the collector logs a line when the floor binds -- but it stops
	// one number being read as evidence for a cause it never carried.
	StepEscapes  uint32
	StallEscapes uint32
}
