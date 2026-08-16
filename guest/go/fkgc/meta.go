//go:build gc.custom

package fkgc

import "unsafe"

// ---------------------------------------------------------------------------
// Collector metadata that GROWS WITH THE HEAP, which is what removes the cap.
//
// Until sharding stage C the mark bitmap, the span table and the span-aux table
// were three fixed-size arrays inside the .bss `gcMeta` struct, and .bss is
// sized at link time -- so the heap they described had a compile-time maximum
// and `fkgc.HeapCap` was a HARD cap a guest trapped against. Three build tags
// moved it. All of that is deleted: FkLua imposes no memory cap beyond what
// Factorio imposes, and a collected guest now grows exactly like a leaking one.
//
// THE SHAPE. The metadata splits by GROWTH LAW rather than by what it means:
//
//	fixed      the class tables, the mark stack, the free-run heads, the dirty
//	           queue, the counters. ~28 KiB, still .bss, still exactly one
//	           struct (see gcMeta for why that is a correctness requirement).
//	scaling    the mark bitmap, spanClass and spanAux. One CHUNK per 4 MiB
//	           slice of heap, allocated FROM THE HEAP under the class clsMeta,
//	           reached through a directory that is itself a fixed .bss array.
//
// `metaDir` is 1,024 entries of 4 KiB of .bss and that covers the ENTIRE wasm32
// address space: 1,024 x 4 MiB is 4 GiB. A flat table sized for 4 GiB would
// have been 35 MiB of .bss -- about 7 ms of Factorio worst tick before the
// guest allocated a byte -- which is why the directory exists at all.
//
// WHY THIS IS SAFE, and it is the one hazard in the design with no precedent
// elsewhere in the repo. agents/gc.md section 6 put metadata outside the heap so
// that the collector writing its own bitmap would not dirty cards the next
// incremental step has to re-scan. Static placement is NOT what neutralises
// that, and sharding stage A measured why: `drainDirtySpans` already drops a
// sub-heap card with one compare, and a metadata card inside the heap is
// dropped by the span-class load `rescanSpan` ALREADY PERFORMS. The compare
// moves; it does not multiply. What static placement really bought was a bound,
// and the bound was the defect.
//
// Two things follow and both are enforced below rather than assumed:
//
//   - `markCandidate` REJECTS a clsMeta span, so no mark bit is ever set for a
//     metadata byte and the collector cannot retain its own bookkeeping as if
//     it were an object.
//   - `metaDir` holds HEAP ADDRESSES and lives inside [metaLo, metaHi), so
//     `scanRoots` subtracts it. A directory in an ordinary global would be
//     scanned as roots, and every chunk would be marked live through it.
//
// THE COST, stated rather than buried. Per MiB of heap the scaling part is
// 10,240 B (0.977%) against the old static scheme's 8,960 B (0.854%): the three
// tables are uint32 now rather than uint32/uint8/uint16, because a chunk is
// LINEAR MEMORY and a byte or halfword store there is a read-modify-write in
// emitted Lua while a word store is one table assignment. 14% more metadata to
// take a read-modify-write off the sweep's inner loop is the right side of that
// trade, and the exact model is asserted by TestTheMetadataSizeModelHolds
// rather than written down as prose that goes stale (it did: the shipped
// comments said 42 and 645 KiB against a measured 58.32 and 583.32).
// ---------------------------------------------------------------------------

const (
	// sliceLog is how much heap one chunk describes: 4 MiB.
	//
	// It is the whole tuning decision here and it trades two things against
	// each other. Bigger slices mean a smaller directory and a larger FLOOR --
	// the first chunk is paid by any guest that allocates at all. Smaller
	// slices mean the reverse. At 4 MiB the floor is 40 KiB and the directory
	// is 4 KiB; at 64 MiB the floor would be 573 KiB, which is worse than the
	// 163 KiB static scheme this replaces for every guest that never gets
	// there.
	sliceLog   = 22
	sliceSpans = 1 << (sliceLog - spanLog)    // 1,024 spans per chunk
	sliceWords = 1 << (sliceLog - granuleLog) // 262,144 granules per chunk

	// The chunk's layout, in bytes from its base. Three arrays of uint32, in
	// the order they are hottest.
	metaMarkOff  = 0
	metaMarkLen  = sliceWords / 32 * 4 // 32,768 B: one bit per granule
	metaClassOff = metaMarkOff + metaMarkLen
	metaClassLen = sliceSpans * 4 // 4,096 B: one word per span
	metaAuxOff   = metaClassOff + metaClassLen
	metaAuxLen   = sliceSpans * 4 // 4,096 B

	// 40,960 B is exactly ten spans, with nothing left over. That is not luck
	// -- it is what picking uint32 for all three tables buys, and a layout that
	// did not land on a span boundary would waste the remainder of the last
	// one forever.
	metaChunkBytes = metaAuxOff + metaAuxLen
	metaChunkSpans = metaChunkBytes >> spanLog

	// maxChunks covers all of wasm32. See maxSpans for the bound that actually
	// binds, which is smaller and is about uint32 arithmetic rather than about
	// this.
	maxChunks = 1 << (32 - sliceLog)

	// markWordsPerSpan is 8: a span is 4,096 B, a granule is 16 B, so a span is
	// exactly 256 granules and exactly eight bitmap words. The bitmap
	// partitions on span boundaries with nothing left over, which is what lets
	// a sweep clear as it goes and what lets every per-span loop below compute
	// ONE address and then index within it.
	markWordsPerSpan = spanBytes / granule / 32
)

// spanClassOf is what span si holds: 0 unassigned, 1..numClasses a size class,
// clsLarge/clsLargeMid a large-object run, clsMeta a metadata chunk.
//
// The value may carry clsFresh; every reader that cares about the CLASS masks
// it off, and sweepSpan is the one reader that wants the raw word. See clsFresh.
func spanClassOf(si uint32) uint32 {
	return load32(gcm.metaDir[si>>(sliceLog-spanLog)] + metaClassOff + (si&(sliceSpans-1))<<2)
}

func setSpanClass(si, c uint32) {
	store32(gcm.metaDir[si>>(sliceLog-spanLog)]+metaClassOff+(si&(sliceSpans-1))<<2, c)
}

// spanAuxOf is the run length for a clsLarge head and the head's span index for
// a clsLargeMid continuation. Unused otherwise.
func spanAuxOf(si uint32) uint32 {
	return load32(gcm.metaDir[si>>(sliceLog-spanLog)] + metaAuxOff + (si&(sliceSpans-1))<<2)
}

func setSpanAux(si, v uint32) {
	store32(gcm.metaDir[si>>(sliceLog-spanLog)]+metaAuxOff+(si&(sliceSpans-1))<<2, v)
}

// markWordBase is the address of the FIRST of span si's eight bitmap words.
//
// Every per-span loop in the sweep and the re-scan calls this once and then
// indexes within the span, which is why moving the bitmap into the heap did not
// make either of them slower: the old code recomputed (addr-heapBase)>>4 and
// indexed a global array per SLOT, and this computes one address per SPAN.
func markWordBase(si uint32) uint32 {
	return gcm.metaDir[si>>(sliceLog-spanLog)] + metaMarkOff +
		(si&(sliceSpans-1))*(markWordsPerSpan*4)
}

// isMarkedAt tests the bit for the object at byte offset off within the span
// whose bitmap words start at mw. off is a span offset, so it is under 4,096
// and the granule index is under 256.
func isMarkedAt(mw, off uint32) bool {
	g := off >> granuleLog
	return load32(mw+(g>>5)<<2)&(uint32(1)<<(g&31)) != 0
}

// clearSpanMarks wipes span si's eight bitmap words.
//
// Eight explicit stores rather than a clear() over a slice: clear() lowers to
// llvm.memset, which the emitter substitutes with the runtime's mem_fill -- a
// call with its own bounds check and page mark, for 32 bytes. Stage B measured
// that shape and the finding was the same one (see freeInvariant): the CALL is
// the cost, not the bytes.
func clearSpanMarks(si uint32) {
	w := markWordBase(si)
	store32(w, 0)
	store32(w+4, 0)
	store32(w+8, 0)
	store32(w+12, 0)
	store32(w+16, 0)
	store32(w+20, 0)
	store32(w+24, 0)
	store32(w+28, 0)
}

// syncHeapTop republishes the marking range after anything moves the coverage
// line -- a new chunk, or a memory.grow that extends an already-covered slice.
// It is called from BOTH, and forgetting the second was a silent corruption:
// markCandidate accepted addresses in the covered heap that heapTop said were
// outside it, so the sweep freed live objects nothing had marked.
func syncHeapTop() { heapTop = heapBase + coveredSpans()<<spanLog }

// coveredSpans is how much of the heap has metadata behind it, and it -- not
// spanCount -- is what every span loop in this package bounds itself by.
//
// The two differ only between a memory.grow and the growCoverage that follows
// it. A span above this line is backed by real linear memory and is simply not
// heap yet: nothing can be allocated in it, the sweep does not walk it, and the
// next allocation that wants room brings it in. Nothing is ever LOST there,
// which is the difference from the cap this replaces -- initialize() used to
// clamp an adopted memory to HeapCap and drop everything above it on the floor.
func coveredSpans() uint32 {
	c := gcm.chunks << (sliceLog - spanLog)
	if c > gcm.spanCount {
		c = gcm.spanCount
	}
	return c
}

// growCoverage brings one more 4 MiB slice of the heap under metadata, and
// reports whether it could.
//
// WHERE THE CHUNK GOES is the only interesting decision, and it is made to keep
// the TOP of the heap contiguous, because that is where a large object goes.
//
//   - First choice: the LOWEST free run of ten spans in the region that is
//     already covered. A chunk placed there is a permanent blocker in a part of
//     the heap that is already fragmented, and it leaves everything above it in
//     one piece.
//   - Fallback: the first ten spans of the slice being covered, which are
//     guaranteed unassigned because they are above the old coverage line and
//     nothing can have been allocated there. This is the case under real
//     pressure, and it is the one that caps a single object at just under 4 MiB.
//
// agents/sharding.md section 9 proposed a FIXED position at the top of each
// slice. That does not work and the reason is worth keeping: a chunk at the top
// of slice k cannot be written until the heap has grown through the whole slice,
// so a guest wanting 64 KiB of heap would have had to take 4 MiB to describe it.
// Placing it at the bottom is what makes coverage incremental, and the
// "so a run can stretch between chunks" argument for the top is not one --
// consecutive chunks leave a 1,014-span gap either way.
func growCoverage() bool {
	k := gcm.chunks
	if k >= maxChunks {
		return false
	}
	// The slice must be backed at least as far as the chunk itself.
	if gcm.spanCount < k<<(sliceLog-spanLog)+metaChunkSpans {
		return false
	}
	t := scanSpanRun(0, coveredSpans(), metaChunkSpans)
	if t == noSpan {
		t = k << (sliceLog - spanLog)
	}
	addr := heapBase + t<<spanLog
	// ALWAYS wiped, not zero()d: zero() is a no-op under -tags fkgcnozero,
	// which is a measurement arm for allocation cost. A chunk arriving with the
	// last tenant's bytes in it is a bitmap full of set bits and a span table
	// full of invented classes, which is not a measurement arm, it is a heap
	// that walks into somebody else's memory.
	wipe(addr, metaChunkBytes)
	gcm.metaDir[k] = addr
	gcm.chunks = k + 1
	// Only reachable now: setSpanClass for a span inside the new slice needs
	// metaDir[k], and in the fallback case t IS inside the new slice.
	for j := uint32(0); j < metaChunkSpans; j++ {
		setSpanClass(t+j, clsMeta)
	}
	gcm.freeBytes -= metaChunkSpans << spanLog
	gcm.metaSpans += metaChunkSpans
	syncHeapTop()
	return true
}

// MetaBytes is how much linear memory the collector's own metadata occupies,
// and since sharding stage C it is a FUNCTION OF THE HEAP rather than a
// constant.
//
//	MetaBytes = MetaFixedBytes + chunks * 40,960
//	chunks    = ceil(covered heap / 4 MiB)
//
// It is .bss plus heap, so it costs nothing in the wasm binary -- but it IS
// linear memory, and agents/guests.md prices linear memory at 0.2 ms of
// Factorio worst tick per MiB whether or not anything is using it. This number
// belongs in a heap budget rather than hiding in one.
func MetaBytes() uint32 { return MetaFixedBytes() + gcm.chunks*metaChunkBytes }

// MetaFixedBytes is the part that does not scale: the class tables, the mark
// stack, the dirty queue, the directory and the counters. This is what a guest
// pays for having a collector at all, before it allocates anything.
func MetaFixedBytes() uint32 { return uint32(unsafe.Sizeof(gcm)) }

// MetaChunks is how many 4 MiB slices of heap currently have metadata behind
// them, and MetaChunkBytes is what one costs. Exported so that the size model
// is TESTED rather than documented -- see TestTheMetadataSizeModelHolds.
func MetaChunks() uint32     { return gcm.chunks }
func MetaChunkBytes() uint32 { return metaChunkBytes }

// MetaSliceBytes is how much heap one chunk describes.
func MetaSliceBytes() uint32 { return 1 << sliceLog }
