// Command gctorture is the collector's retention gate: build a structure, drop
// a KNOWN half of it, collect, and check that what should have survived did and
// what should not have did not.
//
// agents/gc.md's stage-B gate (2), and the first of its two kill criteria:
//
//	Conservative false retention keeps the heap on the ladder anyway. [...]
//	The gate is the retention ratio on churn after a full collection: if the
//	live set the collector believes in is more than ~2x the live set the
//	guest actually has, the heap doubles regardless.
//
// and risk 2, which says where that has to be measured:
//
//	the range test gets MORE permissive as the heap grows: at 16 MiB every
//	integer below 16,777,216 is a plausible pointer, and a Go program holds a
//	lot of those. [...] Stage B's retention gate must be measured on a guest
//	with a LARGE LIVE SET, not on churn.
//
// So this guest's live set is a parameter, and the interesting runs are the big
// ones. It knows exactly how many bytes it is holding, because it built them,
// which is what makes "the collector believes in 1.06x what I actually have" a
// measurement rather than an impression.
//
// It also pins the three shapes a conservative non-moving collector gets wrong
// quietly rather than loudly, each with its own export: an INTERIOR pointer
// (the only reference to a block is to its middle), a ONE-PAST-THE-END pointer
// (agents/gc.md section 1: a parked goroutine's csp is stack+stackSize), and a
// LARGE object spanning several spans.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
//	    -o gctorture.wasm ./examples/gctorture
//
// Under -gc=leaking every export here still works and still returns the same
// checksums -- nothing is reclaimed, so every "did it survive" answer is yes.
// That is the point: the same guest is the differential control.
package main

import (
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fkgc"
)

// node is deliberately bigger than one granule and not a power of two, so it
// lands in a size class with a tail and exercises the slot arithmetic rather
// than a shift.
type node struct {
	id    uint32
	tag   uint32
	left  *node
	right *node
	pad   [5]uint32
}

const nodeBytes = 48 // 4+4+4+4+20, rounded to the 48-byte class

var (
	roots   []*node
	dropped uint32
	kept    uint32

	// interior is the ONLY reference to a block, and it points into the
	// middle of it. A collector that accepts base pointers only reclaims the
	// block underneath this and the read below returns a zero.
	interior *uint32

	// onePast points one byte past the end of a block. Nothing else refers to
	// that block. A collector that treats it as a reference retains the block;
	// one that does not, does not. Either is defensible -- what is not
	// defensible is not knowing which, which is what this measures.
	onePast   uintptr
	onePastID uint32

	big     []uint32
	bigMark uint32
)

// torture_build makes n nodes in a chain of binary fragments, remembering every
// eighth one as a root. Everything reachable from a root must survive; nothing
// else is referenced at all.
//
//go:wasmexport torture_build
func tortureBuild(n uint32) uint32 {
	roots = roots[:0]
	kept, dropped = 0, 0
	var acc uint32
	for i := uint32(0); i < n; i++ {
		a := &node{id: i, tag: i * 2654435761}
		b := &node{id: i ^ 0x5555, tag: i * 40503}
		a.left = b
		if i%8 == 0 {
			roots = append(roots, a)
			kept += 2
		} else {
			dropped += 2
		}
		acc = acc*31 + a.tag + b.tag
	}
	return acc
}

// torture_collect runs a full collection. It is its own export because a
// collection may only begin BETWEEN guest calls -- agents/gc.md section 1 --
// and calling it from inside the build would be measuring something else.
//
//go:wasmexport torture_collect
func tortureCollect() uint32 {
	fkgc.Collect()
	return fkgc.Stats().Collections
}

// torture_verify walks everything that should have survived and checksums it.
//
// A node the collector wrongly reclaimed does not vanish -- the memory is still
// addressable -- it gets ZEROED and handed to somebody else, so the checksum
// moves. That is the only symptom there is, and it is why this returns a
// checksum rather than a boolean: a boolean would have to decide in advance
// what wrong looks like.
//
//go:wasmexport torture_verify
func tortureVerify() uint32 {
	var acc uint32
	for _, r := range roots {
		acc = acc*31 + r.id
		acc = acc*31 + r.tag
		if r.left != nil {
			acc = acc*31 + r.left.id
			acc = acc*31 + r.left.tag
		}
	}
	return acc
}

// torture_kept_bytes is what the guest KNOWS it is holding, counted rather
// than estimated: the nodes reachable from a root, the roots slice's own
// backing array, the large block, and the block the interior pointer refers to.
// The collector's LiveBytes divided by this is the RETENTION RATIO, and
// agents/gc.md's kill criterion is ~2x.
//
// It has to include everything, because a number that leaves the 160 KB `big`
// slice out reports 1.76x for a heap that is actually retaining 1.03x -- which
// is the difference between failing the gate and passing it.
//
//go:wasmexport torture_kept_bytes
func tortureKeptBytes() uint32 {
	n := kept * nodeBytes
	n += uint32(cap(roots)) * 4
	n += uint32(cap(big)) * 4
	if interior != nil {
		n += 256 // the block torture_interior allocated
	}
	return n
}

// torture_stat mirrors churn's: 0 heap, 1 live, 2 free, 3 collections, 4 grows,
// 5 live objects, 6 freed objects, 7 pressure, 8 metadata bytes.
//
//go:wasmexport torture_stat
func tortureStat(which uint32) uint32 {
	s := fkgc.Stats()
	switch which {
	case 0:
		return s.HeapBytes
	case 1:
		return s.LiveBytes
	case 2:
		return s.FreeBytes
	case 3:
		return s.Collections
	case 4:
		return s.Grows
	case 5:
		return s.LiveObjects
	case 6:
		return s.FreedObjects
	case 7:
		return s.SinceGC
	case 8:
		return fkgc.MetaBytes()
	case 9:
		return s.Phase
	case 10:
		return s.Steps
	case 11:
		return fkgc.Budget()
	case 12:
		return fkgc.MaxStepWork()
	case 13:
		return fkgc.TotalWork()
	case 14:
		return s.Deadlines
	case 15:
		return s.Outruns
	case 16:
		return s.MaxUnpaced
	case 17:
		return s.UnpacedWork
	case 18:
		return fkgc.Terminations()
	case 19:
		return fkgc.RootWords()
	case 20:
		return fkgc.MarkBitsSet()
	case 21:
		return fkgc.MetaChunks()
	case 22:
		return fkgc.MetaFixedBytes()
	case 23:
		return fkgc.Rescans()
	case 24:
		return fkgc.DirtyOverflows()
	case 25:
		return fkgc.RescanRestarts()
	case 26:
		return fkgc.EffectiveBudget()
	// The mark escape SPLIT BY CAUSE. 14 is still the total, so a reader that
	// only wants "did the collector give up" is unchanged; these two say
	// WHICH escape, which is the difference between a mark that stopped
	// converging and one that was merely slow for its heap.
	case 27:
		return s.StepEscapes
	case 28:
		return s.StallEscapes
	}
	return 0
}

// ---------------------------------------------------------------------------
// Stage C: the paced collector, and the write barrier that makes it sound.
// ---------------------------------------------------------------------------

// torture_gc_start forces a PACED collection to begin, whatever the heap
// pressure is, and reports whether one did.
//
// fkgc.Start rather than CollectIfNeeded, because the pressure counter is per
// SPAN and not per byte: a class recycling blocks a collection reclaimed has not
// made the heap bigger and registers no pressure, so a second collection in a
// steady-state guest cannot be provoked by allocating. Start still goes through
// the host call that arms the write barrier, which is the part a test must not
// skip -- marking with the barrier off would pass for the wrong reason.
//
//go:wasmexport torture_gc_start
func tortureGCStart() uint32 {
	if fkgc.Start() {
		return 1
	}
	return 0
}

// torture_gc_budget sets the per-step work allowance in granules of heap
// touched and returns what it ended up as. Zero restores the default.
//
//go:wasmexport torture_gc_budget
func tortureGCBudget(units uint32) uint32 {
	fkgc.SetBudget(units)
	return fkgc.Budget()
}

// fresh is the payload torture_repoint hangs off every root. It is a distinct
// type from node only so that reading this file makes the shape obvious.
var freshSum uint32

// torture_repoint IS THE WRITE-BARRIER TEST, and everything about its shape is
// chosen to make a missing barrier show up as a wrong number.
//
// Called BETWEEN two steps of a paced mark, it stores a freshly allocated
// object into a slot of an object the collector has, in general, already marked
// AND ALREADY SCANNED -- a black object, in the tricolour vocabulary. The fresh
// object is white, is referenced from nowhere else, and the mutator's own stack
// is empty by the time the next step runs.
//
// Without a barrier that is a lost object: the root re-scan at mark termination
// reaches the root node, finds it already marked, and returns without looking
// inside it, so nothing ever discovers the child. The sweep then reclaims a
// live object and hands it to somebody else. There is no crash and no error --
// the checksum simply moves, which is the only symptom a use-after-free has
// inside a lockstep simulation.
//
// With the barrier the store dirties the root's page, the next step re-scans
// every marked object in that page, and the child is found.
//
//go:wasmexport torture_repoint
func tortureRepoint(seed uint32) uint32 {
	var acc uint32
	for i, r := range roots {
		f := &node{id: seed + uint32(i), tag: (seed + uint32(i)) * 2654435761}
		// The store into an already-scanned object. r.right was nil, so this
		// creates a reference that did not exist when marking began.
		r.right = f
		acc = acc*31 + f.tag
	}
	freshSum = acc
	return acc
}

// torture_repoint_one stores ONE fresh object into ONE root, which is the
// mutator shape examples/gcbench has: a single slot of a large retained array
// rewritten per tick.
//
//go:wasmexport torture_repoint_one
func tortureRepointOne(seed uint32) uint32 {
	if len(roots) == 0 {
		return 0
	}
	i := int(seed) % len(roots)
	roots[i].right = &node{id: seed, tag: seed * 2654435761}
	return roots[i].right.tag
}

// torture_repoint_verify re-derives the checksum from what is actually in
// memory now. A fresh object the collector reclaimed comes back zeroed, or
// holding whatever the allocator handed its slot to next.
//
//go:wasmexport torture_repoint_verify
func tortureRepointVerify() uint32 {
	var acc uint32
	for _, r := range roots {
		if r.right == nil {
			acc = acc*31 + 0xdeadbeef
			continue
		}
		acc = acc*31 + r.right.tag
	}
	return acc
}

// torture_repoint_want is what the verify must equal.
//
//go:wasmexport torture_repoint_want
func tortureRepointWant() uint32 { return freshSum }

// torture_garbage allocates n nodes and drops every one of them, so a paced
// collection has something to reclaim and the allocator has to hand out blocks
// while a sweep is in flight. That second half is the one with no other test:
// a span claimed above the sweep cursor would be walked afterwards, found to
// hold unmarked slots, and freed with live objects in it.
//
//go:wasmexport torture_garbage
func tortureGarbage(n uint32) uint32 {
	var acc uint32
	for i := uint32(0); i < n; i++ {
		g := &node{id: i, tag: i * 40503}
		// The store to a package-level sink is what makes this a HEAP
		// allocation. Without it the node does not escape, TinyGo puts it on
		// the stack, and this export allocates nothing at all -- a test that
		// would then be measuring a collector with no garbage in front of it.
		// The sink holds exactly one, so every earlier node is dropped.
		garbageSink = g
		acc = acc*31 + g.tag
	}
	return acc
}

var garbageSink *node

// torture_interior allocates a block, keeps a pointer into its MIDDLE and
// nothing else, and returns the value it wrote there. After a collection the
// same read must return the same value.
//
//go:wasmexport torture_interior
func tortureInterior(seed uint32) uint32 {
	b := make([]uint32, 64) // 256 bytes: a whole size class, well past a granule
	for i := range b {
		b[i] = seed + uint32(i)
	}
	interior = &b[37] // deliberately not the base, and not granule-aligned
	return *interior
}

//go:wasmexport torture_interior_read
func tortureInteriorRead() uint32 { return *interior }

// torture_one_past records a pointer one byte past the end of a block and the
// value at its LAST word. agents/gc.md names this shape precisely -- a task's
// csp is stack+stackSize -- and says stage B should assert it rather than
// inherit it.
//
// The uintptr is deliberately NOT a Go pointer: a uintptr does not keep
// anything alive by Go's rules, and whether it keeps anything alive HERE is
// exactly the question, because a conservative collector cannot tell a uintptr
// from a pointer.
//
//go:wasmexport torture_one_past
func tortureOnePast(seed uint32) uint32 {
	b := make([]uint32, 8) // 32 bytes
	for i := range b {
		b[i] = seed ^ uint32(i*7)
	}
	onePastID = b[7]
	onePast = uintptr(unsafe.Pointer(&b[0])) + 32
	return onePastID
}

// torture_one_past_read reads the block back THROUGH the recorded
// one-past-the-end address. It returns 1 if the last word still holds what was
// written, 0 if the block was reclaimed and reused.
//
//go:wasmexport torture_one_past_read
func tortureOnePastRead() uint32 {
	last := *(*uint32)(unsafe.Pointer(onePast - 4))
	if last == onePastID {
		return 1
	}
	return 0
}

// torture_large allocates a block far bigger than a span, so it takes a run of
// them, and writes a pattern through it. Nothing about a multi-span object is
// shared with the small-object path: a different allocation route, a different
// span-table encoding, a different sweep arm.
//
//go:wasmexport torture_large
func tortureLarge(words uint32) uint32 {
	big = make([]uint32, words)
	var acc uint32
	for i := uint32(0); i < words; i++ {
		big[i] = i * 2654435761
		acc = acc*31 + big[i]
	}
	bigMark = acc
	return acc
}

//go:wasmexport torture_large_read
func tortureLargeRead() uint32 {
	var acc uint32
	for i := 0; i < len(big); i++ {
		acc = acc*31 + big[i]
	}
	return acc
}

// torture_drop_all releases every root, so a collection afterwards should
// reclaim essentially the whole heap. This is the other half of the gate: a
// collector that retains everything passes "did the survivors survive" and
// fails the only question that matters.
//
//go:wasmexport torture_drop_all
func tortureDropAll() uint32 {
	roots = nil
	interior = nil
	big = nil
	kept = 0
	return 0
}

func main() {}

// ---------------------------------------------------------------------------
// Sharding stage C: the heap cap is gone, so the interesting sizes are the ones
// the cap used to refuse.
// ---------------------------------------------------------------------------

// held is a retained set sized in MEGABYTES rather than in nodes, because what
// stage C has to demonstrate is a heap growing THROUGH the old 16 MiB cap and
// past 32 MiB -- and getting there in 48-byte nodes is millions of allocations
// under an interpreter with no clock.
//
// Each block is a large object (a run of spans), which is also the arm that the
// metadata chunks can block: a chunk placed under pressure sits at a 4 MiB
// boundary and a single object cannot straddle one.
var (
	held    [][]uint32
	heldSum uint32
)

// torture_hold adds `blocks` retained blocks of `words` words each, writing a
// position-dependent pattern through every one, and returns the checksum.
//
//go:wasmexport torture_hold
func tortureHold(blocks, words uint32) uint32 {
	for b := uint32(0); b < blocks; b++ {
		s := make([]uint32, words)
		k := uint32(len(held)) + 1
		for i := uint32(0); i < words; i++ {
			s[i] = k*2654435761 + i
		}
		held = append(held, s)
	}
	return tortureHoldVerify()
}

// torture_hold_verify re-derives the checksum from what is in memory now.
//
// EVERY SIXTEENTH WORD, not every word, and the reason is the oracle rather
// than laziness: a full walk of a 40 MiB heap is ten million interpreted
// iterations per call and this is called on both sides of every collection. A
// reclaimed block comes back zeroed or holding somebody else's bytes, and no
// failure of this collector is confined to fifteen words in sixteen -- a span
// is the unit of everything it does, and a span is 1,024 words.
//
//go:wasmexport torture_hold_verify
func tortureHoldVerify() uint32 {
	var acc uint32
	for k, s := range held {
		want := uint32(k) + 1
		for i := 0; i < len(s); i += 16 {
			acc = acc*31 + s[i]
			acc = acc*31 + want
		}
	}
	heldSum = acc
	return acc
}

// torture_hold_bytes is what the guest KNOWS it is holding in `held`.
//
//go:wasmexport torture_hold_bytes
func tortureHoldBytes() uint32 {
	var n uint32
	for _, s := range held {
		n += uint32(len(s)) * 4
	}
	return n
}

// torture_drop_held drops the whole retained set.
//
//go:wasmexport torture_drop_held
func tortureDropHeld() uint32 {
	held = nil
	heldSum = 0
	return 0
}

// torture_backed is the linear memory the allocator has claimed as heap, which
// is NOT the same question as HeapBytes: HeapBytes is the covered heap, and
// coverage lags a memory.grow until the next chunk is created.
//
//go:wasmexport torture_backed
func tortureBacked() uint32 { return fkgc.BackedBytes() }

// The metadata size model, exported so a test can ASSERT it rather than repeat
// it. See TestTheMetadataSizeModelHolds.
//
//go:wasmexport torture_meta_chunk_bytes
func tortureMetaChunkBytes() uint32 { return fkgc.MetaChunkBytes() }

//go:wasmexport torture_meta_slice_bytes
func tortureMetaSliceBytes() uint32 { return fkgc.MetaSliceBytes() }

// torture_reinit re-runs the allocator's initialisation against the linear
// memory that exists NOW, and reports the heap it decided it has.
//
// IT IS A WHITE-BOX PROBE AND IT IS DESTRUCTIVE -- every free list and every
// span assignment is forgotten, so nothing allocated before it may be touched
// after. It exists for one assertion, and that assertion is a defect this stage
// removed: initialize() used to CLAMP the pre-existing memory to fkgc.HeapCap
// and drop everything above it on the floor, silently, with no log line and no
// counter. A guest handed 40 MiB of adopted memory came up believing it had 16.
//
//go:wasmexport torture_reinit
func tortureReinit() uint32 {
	fkgc.Reinitialize()
	return fkgc.BackedBytes()
}
