//go:build gc.custom

package fkgc

import "unsafe" // also what makes go:linkname legal in this file

// ---------------------------------------------------------------------------
// The TinyGo -gc=custom contract.
//
// Seven functions the application provides and two the runtime gives back.
// src/runtime/gc_custom.go is the whole specification and agents/gc.md section
// 2 is the reading of it. Three things about it are load-bearing:
//
//   - The runtime NEVER calls the collector. There is no allocation-failure
//     callback; alloc returning is the entire contract. Deciding to collect is
//     the application's, which is exactly what a tick-paced collector wants and
//     is why Collect is exported rather than triggered from alloc.
//   - setHeapEnd is a documented no-op under gc.custom, so runtime.growHeap
//     would grow linear memory and throw the new bound away. THE ALLOCATOR
//     OWNS memory.grow AND ITS OWN HEAP BOUND. That is the whole difference
//     from gc.leaking and it is the one that does not show up as a compile
//     error.
//   - layout is unused and passed as an opaque pointer, so there is no map of
//     which words in a block are pointers. Marking is conservative because
//     TinyGo leaves no alternative, not because conservative was chosen.
// ---------------------------------------------------------------------------

//go:linkname initHeap runtime.initHeap
func initHeap() { initialize() }

//go:linkname alloc runtime.alloc
func alloc(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(allocate(uint32(size)))
}

// free is required to exist and nothing on this target calls it. An explicit
// free into a rebuilt-every-sweep free list would be a double-free the next
// sweep, so this stays a no-op on purpose rather than by omission.
//
//go:linkname free runtime.free
func free(ptr unsafe.Pointer) {}

//go:linkname markRoots runtime.markRoots
func markRoots(start, end uintptr) { scanRoots(uint32(start), uint32(end)) }

//go:linkname gcCollect runtime.GC
func gcCollect() { Collect() }

// SetFinalizer is accepted and ignored. A finalizer needs a precise notion of
// death, and a conservative collector's answer to "is this dead" is "probably,
// unless an integer happened to look like its address".
//
//go:linkname setFinalizer runtime.SetFinalizer
func setFinalizer(obj interface{}, finalizer interface{}) {}

// runtimeMemStats mirrors runtime.MemStats field for field. It is redeclared
// rather than imported because the guest cannot import the runtime package;
// go:linkname matches on the symbol, and the layout has to match by hand. A
// field added upstream shows up here as garbage in whatever follows it.
type runtimeMemStats struct {
	Alloc        uint64
	Sys          uint64
	HeapAlloc    uint64
	HeapSys      uint64
	HeapIdle     uint64
	HeapInuse    uint64
	HeapReleased uint64
	HeapObjects  uint64
	TotalAlloc   uint64
	Mallocs      uint64
	Frees        uint64
	GCSys        uint64
}

//go:linkname readMemStats runtime.ReadMemStats
func readMemStats(ms *runtimeMemStats) {
	s := Stats()
	ms.HeapSys = uint64(s.HeapBytes)
	ms.HeapInuse = uint64(s.HeapBytes - s.FreeBytes)
	ms.HeapIdle = uint64(s.FreeBytes)
	ms.HeapAlloc = uint64(s.LiveBytes + s.SinceGC)
	ms.Alloc = ms.HeapAlloc
	// GCSys is the collector's own footprint, and since the metadata scales it
	// is the fixed .bss part PLUS the chunks in the heap -- which are inside
	// HeapBytes, so they are not added twice.
	ms.Sys = uint64(s.HeapBytes) + uint64(MetaFixedBytes())
	ms.GCSys = uint64(MetaBytes())
	ms.Mallocs = uint64(s.LiveObjects + s.FreedObjects)
	ms.Frees = uint64(s.FreedObjects)
	ms.HeapObjects = uint64(s.LiveObjects)
	// TotalAlloc is not carried: a running byte total is a read-modify-write on
	// the allocation path, and -gc=leaking pays for its own as a uint64 add.
	// What is here instead is what the collector actually knows.
	ms.TotalAlloc = uint64(s.LiveBytes) + uint64(s.SinceGC)
}

// The two the runtime provides, and the half that makes this cheap.
//
// gcMarkReachable is markStack() followed by findGlobals(markRoots): the
// shadow stack, then the one contiguous byte range [__global_base,
// __heap_base). Both are compiled for gc.custom && tinygo.wasm --
// gc_stack_portable.go's build tag names gc.custom explicitly -- so root
// DISCOVERY is not this package's problem at all. What roots mean is.
//
//go:linkname gcMarkReachable runtime.gcMarkReachable
func gcMarkReachable()

//go:linkname runtimePanic runtime.runtimePanic
func runtimePanic(msg string)

//go:extern __heap_base
var heapBaseSymbol [0]byte

//export llvm.wasm.memory.size.i32
func wasmMemorySize(index int32) int32

//export llvm.wasm.memory.grow.i32
func wasmMemoryGrow(index int32, delta int32) int32

// ---------------------------------------------------------------------------
// Shape
// ---------------------------------------------------------------------------

const (
	// granule is the allocation quantum and the resolution of the mark
	// bitmap. 16 is TinyGo's own heap alignment (the alignment of
	// max_align_t), so an object base is 16-aligned by construction and a
	// granule index identifies one.
	granule    = 16
	granuleLog = 4

	// spanBytes is the unit the heap is carved into, and 4 KiB is not
	// arbitrary: it is --persist=packed's page size. A span and a dirty page
	// are the same object, which is what lets stage C intersect the dirty-page
	// set with span metadata without a second mapping.
	spanBytes = 4096
	spanLog   = 12

	// grayCap is the mark stack depth, in objects. Overflow is handled rather
	// than fatal (see drainGray), so this is a performance parameter and not a
	// limit -- which is what lets it be small. It is .bss, and .bss is linear
	// memory, and linear memory is 0.2 ms of Factorio worst tick per MiB
	// whether or not anything is in it.
	grayCap = 4096

	// Size-class ladder. Power-of-two-ish, every entry a multiple of the
	// granule, every entry dividing 4 KiB with at most ~6% tail waste.
	// agents/gc.md's argument for size classes is not speed: without
	// compaction a heap that fragments GROWS, and a heap that grows never
	// un-grows.
	numClasses  = 21
	maxSmall    = 2048
	clsLarge    = numClasses + 1 // a span run serving one big object
	clsLargeMid = numClasses + 2 // a continuation span of that run
	clsMeta     = numClasses + 3 // a chunk of the collector's own metadata

	// clsFresh is OR'd into a span's class word when the MUTATOR claims that
	// span AFTER marking terminated, i.e. during a sweep the cursor has not yet
	// reached. It is a flag rather than a class because the span still serves
	// its size class in every other respect.
	//
	// IT IS WHAT REPLACES THE SWEEP-CURSOR WINDOW, and that window was the
	// worst tick this stage set out to fix. Stage C forbade findSpanRun from
	// handing out a span above the sweep cursor, because a span claimed there
	// would be walked afterwards, found to hold unmarked slots -- nothing marks
	// after termination -- and freed with live objects in it. The consequence
	// was that at the instant marking terminated the mutator's window was
	// EMPTY, so the next allocation swept ahead, in an unbounded loop, inside an
	// event handler, until a span fell free. That is the 65x-over-budget tick
	// agents/gc.md carried as open item 5, and it lands in the same tick as the
	// terminating step, which is why it was attributed to one.
	//
	// The flag answers the same question the cursor did and answers it per span:
	// a span carrying it was claimed after the bitmap froze, so every slot in it
	// is either live or part of the class's current run, and the sweep must skip
	// it whole and clear the flag. It is set ONLY when si >= sweepCursor -- below
	// the cursor the sweep has already decided the span and will never revisit
	// it, so a flag left there would survive into the next cycle and leak the
	// span forever.
	clsFresh = 1 << 8

	// clsPending marks a span that is ALREADY IN THE PENDING DIRTY LIST, and it
	// is what makes that list a SET.
	//
	// Without it a page the mutator writes every tick takes a new slot every
	// tick. Measured in a real Factorio on examples/gcsave -- a guest that
	// rewrites most of a 128 KiB heap per tick -- the owed work climbed past
	// 103,000 granules, which is 402 pending spans on a THIRTY-TWO span heap.
	// The mark could not terminate, the queue could never drain, and no
	// forward-progress metric could tell that apart from a big heap being
	// marked slowly, because the number it would have to read was nonsense.
	//
	// Set when a page is ingested, cleared when its span is re-scanned, and
	// cleared wholesale at beginSweep. Deduplication does not weaken the
	// barrier: re-scanning a span once covers every store made into it, because
	// what the re-scan reads is the span as it stands now.
	clsPending = 1 << 9

	// clsFlags is every flag bit. A reader that wants the CLASS masks them all.
	clsFlags = clsFresh | clsPending
)

// classSize[c] is the byte size class c hands out. Index 0 is "no class" and
// doubles as "this span is unassigned", which is what makes a zeroed span
// table the correct initial state.
var classSize = [numClasses + 1]uint32{
	0,
	16, 32, 48, 64, 80, 96, 112, 128,
	160, 192, 224, 256, 320, 384, 448, 512,
	640, 768, 1024, 1280, 2048,
}

// ---------------------------------------------------------------------------
// Metadata
//
// EVERYTHING mutable this package owns is one struct, and that is a
// correctness requirement rather than tidiness.
//
// findGlobals hands the collector [__global_base, __heap_base) as a root range,
// and .bss is inside it -- so without care the mark bitmap would be scanned as
// roots, every bitmap word would be a candidate pointer, and the free-list
// heads would keep every free object alive through the sweep that is supposed
// to rebuild them from scratch. One struct is one contiguous address range, so
// scanRoots can subtract it with two compares. Separate package-level vars are
// not guaranteed adjacent and could not be subtracted at all.
// ---------------------------------------------------------------------------

type gcMeta struct {
	// metaDir[k] is the address of the chunk holding the mark bitmap, the span
	// table and the span-aux table for the k'th 4 MiB slice of heap, or zero if
	// that slice is not covered yet. See meta.go, which is the whole of the
	// scaling-metadata design.
	//
	// IT IS A FIELD OF THIS STRUCT AND NOT A PACKAGE-LEVEL ARRAY, for a reason
	// sharper than the one the rest of the struct has: it holds HEAP ADDRESSES.
	// findGlobals reports [__global_base, __heap_base) as roots, and a directory
	// outside [metaLo, metaHi) would be scanned there -- so every chunk would be
	// marked as a live object by the collector's own bookkeeping, and the sweep
	// would then keep them, which is the one failure that looks like it works.
	metaDir [maxChunks]uint32

	// chunks is how many entries of metaDir are live. Coverage grows upward and
	// never shrinks, so this is also the index of the next chunk to create.
	chunks uint32

	// gray is the mark stack, holding object base addresses.
	gray    [grayCap]uint32
	grayTop uint32
	grayOvf bool

	// slotTab[c][off>>4] is the slot index an offset within a span belongs
	// to, or slotNone if the offset is in the class's tail waste. This is
	// what resolves an INTERIOR POINTER in O(1) with no division: agents/gc.md
	// section 1 requires interior pointers (a parked goroutine's asyncifysp is
	// stack+8) and a division per candidate would be a helper call in the
	// emitted Lua.
	slotTab [numClasses + 1][spanBytes / granule]uint8

	// classSlots[c] is how many objects of class c fit in a span.
	classSlots [numClasses + 1]uint16

	// sizeToClass[(n+15)>>4] is the class serving n bytes, for n <= maxSmall.
	sizeToClass [maxSmall/granule + 1]uint8

	// curPtr/curEnd are the class's current run: the bump cursor and the byte
	// past its last block. They are the ONLY state runtime.alloc touches, and
	// they are Go arrays rather than heap words for the reason allocate
	// documents.
	curPtr [numClasses + 1]uint32
	curEnd [numClasses + 1]uint32

	// runHead/runTail are the class's remaining free runs, threaded through
	// the first eight bytes of each run. See pushRun.
	runHead [numClasses + 1]uint32
	runTail [numClasses + 1]uint32

	// holdLo/holdHi are the class's current run AS IT STOOD WHEN MARKING
	// TERMINATED. A paced sweep runs while the mutator allocates, so curPtr has
	// moved on by the time the sweep reaches the span holding it; the blocks in
	// between are live and unmarked, and a window computed from the live cursor
	// misses every one of them. See beginSweep.
	holdLo [numClasses + 1]uint32
	holdHi [numClasses + 1]uint32

	// dirtyQ is where the HOST writes the page numbers written since the last
	// collection step -- the MEMDIRTY page set, handed across the boundary.
	//
	// It is a field of this struct rather than a package-level array for the
	// reason the struct exists at all: it has to be inside [metaLo, metaHi) so
	// that scanRoots subtracts it. Otherwise every page number in it would be
	// read as a candidate pointer at every termination attempt -- and a page
	// number is a small integer, which is exactly the shape the conservative
	// range test is least able to reject.
	dirtyQ [dirtyCap]uint32

	// pend is the collector's own pending list of dirtied page numbers, and it
	// SURVIVES A STEP where dirtyQ does not: the host overwrites the landing pad
	// at every step. dirtyN and dirtyCursor index THIS, not dirtyQ. See
	// ingestDirty for what losing the difference cost.
	pend        [pendCap]uint32
	dirtyN      uint32 // how many are pending
	dirtyCursor uint32 // how many have been re-scanned

	// rescanOwed is the "the record of what changed was lost, assume
	// everything did" flag, and rescanCursor is how far the resumable full
	// pass has got. Three things set it: gray-stack overflow, a dirty record
	// that did not fit, and a collection resumed after a save.
	rescanOwed   bool
	rescanCursor uint32
	// The fate of the large-object run the sweep cursor is inside: 0 none,
	// 1 keeping, 2 freeing. It exists because a run is swept over several
	// steps and the head -- which is where the decision is made and where the
	// mark bit lives -- is behind the cursor by then.
	largeKeep uint8

	// The one in-flight object scan. A gray unit has to be a GRANULE and not a
	// whole object, or a guest's 1 MiB slice is an indivisible 32 ms step --
	// which is the trap agents/gc.md names and the reason Lua's own collector
	// could not be paced. partialBase is zero when there is none; zero is never
	// a heap address.
	partialBase uint32
	partialOff  uint32
	partialEnd  uint32

	// stepWork is what the current step has charged, UNSATURATED, and maxWork
	// is the largest any step of the current collection charged. A saturating
	// budget hides the one case worth knowing about -- an indivisible unit
	// bigger than the whole allowance -- so it is counted separately.
	stepWork  uint32
	maxWork   uint32
	totalWork uint32

	// The work done INSIDE A GUEST CALL rather than inside a paced step, which
	// is the only collector work that can land in the middle of an event
	// handler and is therefore the only collector work a pause budget cannot
	// see. Two sources: the bounded sweep-ahead in allocSpans, and the
	// last-resort Collect() when memory.grow itself is refused.
	//
	// It is counted separately rather than folded into stepWork BECAUSE THE
	// OLD ACCOUNTING COULD NOT SEE IT AT ALL: step() zeroes stepWork on entry,
	// so everything the sweep-ahead charged between two steps was wiped before
	// maxWork was ever compared against it. Host-side the collector reported
	// 1.17x of budget while the game showed 65x, and this is the gap.
	unpacedWork     uint32
	maxUnpacedWork  uint32
	callFolds       uint32
	maxUnpacedFolds uint32
	callWork        uint32 // charged since the last paced step; feeds maxUnpacedWork

	// rootWords is how many 4-byte words the LAST termination attempt scanned
	// as roots. The root re-scan used to be free by omission; it is charged
	// now, and this is what makes the charge auditable.
	rootWords uint32
	// terminations counts termination attempts -- gcMarkReachable calls made
	// from markStep. One per collection is the healthy number; a rising count
	// against a flat Collections is a mark that keeps draining its queues and
	// then finding new work.
	terminations   uint32
	rescans        uint32
	rescanRestarts uint32
	dirtyOverflows uint32
	// outruns counts the times an allocation had to GROW the heap while a
	// collection was in flight, i.e. the times the mutator beat the pacer. It
	// is the honest name for what used to be a synchronous collection.
	outruns uint32

	spanCount   uint32 // spans backed by linear memory
	metaSpans   uint32 // of those, spans holding metadata chunks
	spanCursor  uint32 // rotating first-fit cursor for span allocation
	sweepCursor uint32 // the sweep's position; spans below it are swept

	// The sweep's accumulators. They are separate from the published
	// liveBytes/freeBytes because a paced sweep is only half-true until it
	// finishes, and a guest reading Stats() mid-sweep should see the LAST
	// completed cycle's numbers rather than a partial sum that means nothing.
	liveAcc    uint32
	freeAcc    uint32
	freedAcc   uint32
	liveObjAcc uint32

	freeBytes   uint32
	liveBytes   uint32
	liveObjs    uint32
	freedObjs   uint32
	sinceGC     uint32
	threshold   uint32
	budget      uint32
	collections uint32
	grows       uint32
	steps       uint32
	lastSteps   uint32
	deadlines   uint32
	// stepEscapes and stallEscapes are `deadlines` SPLIT BY CAUSE, and their
	// sum is deadlines exactly. The two escapes are different diagnoses with
	// different remedies and only one number reported them; see the escape in
	// markStep and MemStats.StepEscapes.
	stepEscapes  uint32
	stallEscapes uint32
	// markLimit is this collection's mark-phase step limit, fixed when it
	// started. See markDeadline.
	markLimit uint32
	// phase is the collection state machine: idle, marking, sweeping. It is in
	// linear memory like everything else here, which is what makes a save taken
	// between two steps of one collection carry the collection.
	phase uint8
	// marked counts objects marked in this collection, and stalls counts
	// CONSECUTIVE mark steps that neither marked anything new nor reduced the
	// work owed. See markStallLimit.
	marked      uint32
	stalls      uint32
	maxStalls   uint32
	stallSteps  uint32
	owedMark    uint32
	pendEmptied bool
	pendEmpties uint32
	// markForced is the latched livelock escape: once the mark phase has been
	// shown not to converge under the budget, every remaining mark step of THIS
	// collection runs unbudgeted. Cleared when the sweep opens. See step().
	markForced bool
	// collecting is a RE-ENTRANCY guard and not the phase. It is true only
	// while a step is executing, so that the safety valve in allocSpans can
	// tell "a collection is in progress" (phase) from "we are inside the
	// collector right now" (this), which are different questions with different
	// answers.
	collecting bool
	// valveWarned is whether the "collecting inside a guest call" line has been
	// logged. Once per guest: the valve firing repeatedly is a guest in trouble
	// and a line per allocation would be a second problem on top of the first.
	valveWarned bool
	// rootWarned is whether the "root set larger than the budget" line has been
	// logged. Once per guest: the condition is a property of the guest's globals,
	// which do not change while it runs.
	rootWarned bool
}

const slotNone = 255

var gcm gcMeta

var (
	heapBase uint32 // 4 KiB-aligned; spans are indexed from here
	// heapTop is the exclusive top of the COVERED heap -- heapBase plus
	// coveredSpans, not plus spanCount -- and the distinction is load-bearing
	// rather than pedantic.
	//
	// It is markCandidate's whole range test, and markCandidate's next act is
	// to read the span class through metaDir. A candidate pointing into the
	// backed-but-uncovered gap between the two would index a directory entry
	// that is still ZERO, so the class would be read out of address 36,864 --
	// somewhere in the guest's DATA segment -- and, worse, a mark bit would be
	// WRITTEN there. That is silent corruption of the guest's own statics,
	// which is exactly how it presented: a barrier test lost a store it had
	// correctly recorded.
	//
	// Nothing can be allocated above the coverage line (findSpanRun is bounded
	// by it), so no object is excluded by using the smaller bound.
	heapTop uint32
	metaLo  uint32
	metaHi  uint32
	inited  bool
)

// defaultThreshold is how many bytes may be handed out between collections
// before CollectIfNeeded says yes. 256 KiB is two orders of magnitude below
// the point where the Lua-side tail becomes measurable and comfortably above
// one blueprint paste's 403 KiB... which is to say a paste triggers exactly one
// collection, which is the intent.
const defaultThreshold = 256 << 10

func initialize() {
	base := uint32(uintptr(unsafe.Pointer(&heapBaseSymbol)))
	heapBase = (base + spanBytes - 1) &^ (spanBytes - 1)

	metaLo = uint32(uintptr(unsafe.Pointer(&gcm)))
	metaHi = metaLo + uint32(unsafe.Sizeof(gcm))

	// THE DEFAULTS DO NOT CLOBBER A VALUE THE GUEST ALREADY INSTALLED, and this
	// was a silent defect in both languages until 2026-08-03.
	//
	// Zero is not a legal setting -- SetThreshold(0) and SetBudget(0) mean
	// "restore the default" and write the default, so a non-zero field here is
	// always something a guest asked for, and .bss gives us the zero for free.
	// That makes the latch independent of WHEN initialize() runs, which is the
	// property that matters: the two languages reach it at completely different
	// moments and a fix keyed on call ordering would be right in one of them.
	//
	// The ordering is not what TinyGo's source reads like. wasmEntryReactor
	// calls initHeap() and then initAll(), so a package initialiser looks like
	// it lands after this -- and measured through examples/gcconfig at TinyGo
	// 0.41.1 on -target=wasm-unknown it does NOT: a counter incremented here
	// reads zero from a guest's init() and one from its first export. So the
	// obvious shape, `func init() { fkgc.SetThreshold(n) }`, was writing a value
	// this line then overwrote.
	//
	// It fails in the direction that hides itself. A guest that also arms its
	// own deferred flush on `Stats().SinceGC >= n` -- which agents/gc.md
	// prescribes and the first downstream mod ships -- then disagrees with the
	// collector by construction: it asks on every event and the collector
	// declines every time, with nothing logged. Found in the field on the Rust
	// arm (fklua-ports' AutoDeconstruct) and confirmed here for Go.
	if gcm.threshold == 0 {
		gcm.threshold = defaultThreshold
	}
	if gcm.budget == 0 {
		gcm.budget = defaultBudget
	}

	// The class tables. Built here rather than as initialised globals because
	// an initialised global of this size is 5.6 KiB of DATA segment in every
	// packaged mod, and this loop is ~5,600 iterations once, at load.
	for c := 1; c <= numClasses; c++ {
		sz := classSize[c]
		slots := spanBytes / sz
		gcm.classSlots[c] = uint16(slots)
		for g := uint32(0); g < spanBytes/granule; g++ {
			idx := (g * granule) / sz
			if idx >= slots {
				gcm.slotTab[c][g] = slotNone
			} else {
				gcm.slotTab[c][g] = uint8(idx)
			}
		}
	}
	cur := uint32(1)
	for n := uint32(0); n <= maxSmall/granule; n++ {
		need := n * granule
		if need == 0 {
			need = granule
		}
		for cur < numClasses && classSize[cur] < need {
			cur++
		}
		gcm.sizeToClass[n] = uint8(cur)
	}

	// WHATEVER LINEAR MEMORY ALREADY EXISTS ABOVE heapBase IS HEAP, ALL OF IT.
	//
	// This used to clamp to HeapCap and drop the rest on the floor -- silently,
	// with no log line and no counter, so a guest handed 40 MiB of adopted
	// memory came up believing it had 16 and grew a second time to get what it
	// already had. There is no cap to clamp to now, and the only bound left is
	// arithmetic (see maxSpans), which is 4 GiB less one span.
	//
	// Nothing is GROWN here: a guest that allocates nothing should not pay for
	// a heap, and no chunk is created either -- coverage is brought up by the
	// first allocation that needs it.
	//
	// Computed in SPANS rather than in bytes because bytes overflow: a wasm32
	// memory can be 65,536 pages and 65536<<16 is zero in a uint32. Sixteen
	// spans to the page, so the same arithmetic in spans has 4 bits of headroom.
	spans := uint32(wasmMemorySize(0)) << (16 - spanLog)
	if hb := heapBase >> spanLog; spans > hb {
		n := spans - hb
		if lim := maxSpans(); n > lim {
			n = lim
		}
		gcm.spanCount = n
	}
	// heapTop follows COVERAGE, which is zero here: no chunk exists until an
	// allocation asks for one. See the declaration.
	heapTop = heapBase
	gcm.freeBytes = gcm.spanCount << spanLog
	inited = true
}

// maxSpans is where the heap stops, and it is wasm32 rather than policy.
//
// heapTop is a uint32 and markCandidate's whole hot loop is the half-open range
// test [heapBase, heapTop). A heap whose top is exactly 2^32 wraps to zero, the
// range test accepts nothing, and a collector that marks nothing frees
// everything -- so the last span of the address space is refused rather than
// wrapped. agents/sharding.md section 9 asked for exactly this: "in practice a
// little under it, where uint32 span arithmetic wraps, and the code must say so
// rather than wrap".
//
// The directory bounds it too, at 1,024 chunks x 4 MiB = 4 GiB, which is the
// same number one span higher; the subtraction is what binds.
func maxSpans() uint32 {
	n := (^uint32(0) - heapBase) >> spanLog
	if n > maxChunks<<(sliceLog-spanLog) {
		n = maxChunks << (sliceLog - spanLog)
	}
	return n
}

// ---------------------------------------------------------------------------
// Allocation
//
// Free-list-first, span-second, memory.grow last. agents/gc.md makes that
// ordering a design decision rather than a tuning one: the usual argument for
// bump-first is locality and it is real, but it loses, because a bump pointer
// that walks past the end of the heap grows it PERMANENTLY -- wasm has no
// memory.shrink -- and every doubling avoided is 0.2 ms per MiB of worst tick
// that no later collection can give back.
// ---------------------------------------------------------------------------

// allocate is the hot path, and its SHAPE is load-bearing in a way that is
// invisible in Go and obvious in the emitted Lua. Stage B measured all of the
// following rather than reasoning about it; the numbers are in agents/gc.md.
//
// Two facts about -gc=custom drive everything here.
//
// FIRST: the TinyGo compiler gives every Go POINTER live in a function a
// shadow-stack slot, and zeroes those slots on entry so the collector can scan
// them. In Lua a slot is a bounds-checked 8-byte store. The first draft of this
// allocator dereferenced a free-list link inline, declared eleven slots, and
// ran at 1.75x -gc=leaking on churn. Nothing in the Go source says so.
//
// SECOND: a memory operation in emitted Lua is expensive enough that a counter
// is a design decision. -gc=leaking's own alloc carries two uint64 running
// totals and pays for them; this one carries nothing per allocation, which is
// most of how it gets UNDER the allocator it replaces.
//
// So free space is tracked as RUNS of adjacent free blocks rather than as a
// list of individual ones, and a run is bumped through. Sweep already walks
// dead slots in address order and already coalesces them into runs to zero
// them, so the runs cost nothing to produce; what they buy is that handing out
// a block touches no heap memory at all -- four Go array reads and one write,
// no dereference, no call, no pointer. The dereference happens once per RUN,
// which on an allocate-and-drop workload is once per span.
//
// The rule for anything added here: no unsafe.Pointer, no string, no call, no
// multi-value return. The way to check is to read F[n] for runtime.alloc in an
// emitted chunk -- -gc=leaking's alloc has no shadow frame, and neither should
// this.
func allocate(size uint32) uintptr {
	if size > maxSmall {
		return allocLarge(size)
	}
	c := uint32(gcm.sizeToClass[(size+granule-1)>>granuleLog])
	p := gcm.curPtr[c]
	if p == gcm.curEnd[c] {
		// Out of run. Everything expensive is behind this one call.
		p = nextRun(c)
	}
	gcm.curPtr[c] = p + classSize[c]
	return uintptr(p)
}

// freeInvariant is the rule that makes allocation cheap, stated where it can be
// found and tested rather than left implicit in three functions:
//
//	EVERY FREE BLOCK IS ZERO, EXCEPT THE FIRST EIGHT BYTES OF A RUN, WHICH
//	ARE THAT RUN'S {next, end} DESCRIPTOR.
//
// runtime.alloc must return zeroed memory, and the first draft honoured that
// with a memset per allocation. Measured on churn that was 31,502 calls into
// the runtime's mem_fill for an average of 32 bytes each -- the call, not the
// bytes.
//
// The bytes still have to be zeroed, but only in the two places that touch a
// whole span at once: refill zeroes a span it has just claimed, in one mem_fill
// of 4 KiB rather than 256 of 16 bytes, and sweep zeroes each run of dead slots
// in one call. Handing a block out then costs nothing, because the block is
// already zero -- and the eight descriptor bytes are cleared once per run by
// nextRun.
//
// Three places maintain it: refill, sweep and nextRun. TestAFreshBlockIsZeroed
// is what stops that being a comment.
const freeInvariant = "a free block is zero; a run's first eight bytes are its descriptor"

// nextRun retires the class's current run and installs the next one, returning
// the first block of it.
//
// This is where every expensive thing in allocation has been pushed: the two
// heap dereferences that read a run descriptor, the refill that claims a new
// span, and the out-of-memory path with its string. It is noinline so that none
// of them lands in runtime.alloc's shadow frame.
//
//go:noinline
func nextRun(c uint32) uint32 {
	r := gcm.runHead[c]
	if r == 0 {
		if !refill(c) {
			oom(1)
		}
		r = gcm.runHead[c]
	}
	next := load32(r)
	end := load32(r + 4)
	gcm.runHead[c] = next
	if next == 0 {
		gcm.runTail[c] = 0
	}
	// Clear the descriptor: from here on the run obeys freeInvariant's first
	// clause and every block in it is zero.
	store32(r, 0)
	store32(r+4, 0)
	gcm.curEnd[c] = end
	return r
}

// pushRun threads [start, end) onto class c's run list, at the TAIL, so the
// list is in ascending address order.
//
// Address order is not cosmetic. What lands in storage is saved, CRC'd and
// multiplayer-synchronised, so a heap whose layout depended on the order the
// collector happened to walk something would be a per-client heap -- the same
// reason agents/gc.md says the dirty-page set must be consumed through DPQ and
// never through pairs(DPG).
func pushRun(c, start, end uint32) {
	store32(start, 0)
	store32(start+4, end)
	if gcm.runTail[c] == 0 {
		gcm.runHead[c] = start
	} else {
		store32(gcm.runTail[c], start)
	}
	gcm.runTail[c] = start
}

// refill claims one span for class c and makes it that class's next run.
// It reports false only when the heap cannot grow.
//
//go:noinline
func refill(c uint32) bool {
	si := allocSpans(1)
	if si == noSpan {
		return false
	}
	setSpanClass(si, c|freshBit(si))
	base := heapBase + si<<spanLog
	slots := uint32(gcm.classSlots[c])
	sz := classSize[c]
	gcm.freeBytes -= spanBytes - slots*sz // the class's tail waste, lost for good
	// One mem_fill for the whole span, which is what establishes
	// freeInvariant for all of its slots at once. A span released by sweep
	// holds the last tenant's bytes; a span from a fresh memory.grow is
	// already zero and this is redundant for it, which is cheap next to
	// tracking which.
	zero(base, spanBytes)
	// Heap pressure is accounted HERE and not per allocation, and that is a
	// design decision rather than a saving.
	//
	// What CollectIfNeeded is really asking is "has the heap had to get
	// bigger", because growing is the thing this collector exists to prevent
	// and a byte handed back out of a run a collection reclaimed has not grown
	// anything. A class recycling reclaimed blocks therefore registers no
	// pressure and provokes no collection, which is the behaviour wanted --
	// arrived at by taking a read-modify-write off the allocation path rather
	// than by adding a rule.
	gcm.sinceGC += spanBytes
	pushRun(c, base, base+slots*sz)
	return true
}

//go:noinline
func allocLarge(size uint32) uintptr {
	n := (size + spanBytes - 1) >> spanLog
	si := allocSpans(n)
	if si == noSpan {
		oom(n)
	}
	f := freshBit(si)
	setSpanClass(si, clsLarge|f)
	setSpanAux(si, n)
	for k := uint32(1); k < n; k++ {
		setSpanClass(si+k, clsLargeMid|f)
		setSpanAux(si+k, si)
	}
	p := heapBase + si<<spanLog
	total := n << spanLog
	gcm.sinceGC += total
	zero(p, total)
	return uintptr(p)
}

// allocSpans finds a run of n unassigned spans, growing linear memory if there
// is not one. First fit from a rotating cursor: the span table is 4,096 entries
// at the cap and a scan of it is cheap next to a memory.grow that can never be
// undone.
// noSpan is "no run of that length exists". A sentinel rather than a second
// return value on purpose: a (uint32, bool) return is an sret POINTER, and a
// pointer live across a call in the allocation path is a stack slot the TinyGo
// GC lowering zeroes on every allocation. See allocate.
const noSpan = ^uint32(0)

// freshBit is clsFresh when the span is one the sweep has yet to walk, and zero
// otherwise. See clsFresh: it is what lets the mutator claim a span ANYWHERE
// while a sweep is in flight, which is what removes the unbounded sweep-ahead.
func freshBit(si uint32) uint32 {
	if gcm.phase == phaseSweep && si >= gcm.sweepCursor {
		return clsFresh
	}
	return 0
}

// IT SWEEPS ONE BITE BEFORE IT GROWS, AND IT NEVER COLLECTS.
//
// This is the shape stage C got wrong twice, and both mistakes came from the
// same place: the heap had a hard cap, so running out of span was FATAL and any
// price was worth paying to avoid it. There is no cap now, so the price is the
// thing to minimise.
//
// What the old path did, in order: sweep ahead in an UNBOUNDED loop until a run
// fell free; then grow; then, if that failed, run a whole synchronous mark and
// sweep INSIDE the event handler, per failing span allocation, repeatedly --
// measured at about 1.4 s a time in a real Factorio. Both of those are pauses in
// a lockstep game and the second is a pause per allocation.
//
// What it does now:
//
//  1. Look for a run. During a sweep this now searches the WHOLE heap, because
//     clsFresh protects a span claimed above the cursor. Stage C's cursor
//     window made the mutator's search space EMPTY at the instant marking
//     terminated, which is what made step 2 unbounded.
//  2. Sweep ONE bite -- one paced step's worth -- and look again. That is what
//     still bounds the heap: a mutator outrunning the pacing sweeps for itself
//     rather than growing past free space nobody has looked at yet. Bounded,
//     so the cost it can add to a tick is one step and not one collection.
//  3. Bring more of the heap under metadata, then GROW. Growing is what a
//     guest that outruns its pacer gets, and that is the whole product
//     position: --gc=collected degrades to -gc=leaking under a storm and
//     recovers when the paced collection catches up. It does not pause.
//
// ALLOCATION NEVER COLLECTS, which is what doc.go always claimed and what the
// heap cap had made false. A synchronous Collect() survives in exactly one
// place -- below, when memory.grow itself is refused -- and there the
// alternative is not a pause, it is a dead mod. Recovery from a storm is the
// guest's next CollectIfNeeded, which is where a collection can start without
// landing in the middle of somebody's event handler.
//
// Starting a PACED collection from here was built and removed. It is sound --
// startCollection only reads roots, and reading the shadow stack mid-call can
// only over-approximate -- but it collects for a guest that never asked, and it
// makes `outruns` count the collection this function itself started rather than
// the guest's own pacing falling behind. The counter is worth more than the
// convenience.
func allocSpans(n uint32) uint32 {
	if si := findSpanRun(n); si != noSpan {
		return si
	}
	// ONE BITE OF SWEEP-AHEAD PER DISPATCH, not per allocation, and the
	// distinction is what makes this bound a TICK rather than a call.
	//
	// A bite bounded at one step's worth bounds the ALLOCATION; it does not
	// bound the tick, because a dispatch makes as many allocations as it likes.
	// Measured in a real Factorio on examples/gcbench with the bite bounded but
	// the LOOP left in: one dispatch took 131 separate bites, each inside its
	// budget, 131x the budget between two steps. That is the pause this replaced,
	// reached by a different route, and the instrument that finds it --
	// Stats().MaxUnpaced and the fold count behind it -- is what stage C did not
	// have.
	//
	// gcm.callWork is exactly "collector work charged since the last paced step",
	// because a step resets it. Gating on it makes the in-call cost one bite per
	// tick whatever the dispatch does, and every allocation after that GROWS --
	// which is what a guest outrunning its pacer is supposed to get.
	if gcm.phase == phaseSweep && !gcm.collecting && gcm.callWork < sweepAheadUnits {
		gcm.collecting = true
		sweepStep(sweepAheadUnits)
		gcm.collecting = false
		endUnpaced()
		if si := findSpanRun(n); si != noSpan {
			return si
		}
	}
	for {
		// Coverage first: a span above the coverage line is backed memory the
		// heap already owns and is cheaper than any memory.grow.
		if growCoverage() {
			if si := findSpanRun(n); si != noSpan {
				return si
			}
			continue
		}
		if !growHeap(n) {
			break
		}
		// GREW WHILE A COLLECTION WAS RUNNING is the outrun, and it is the only
		// shape worth a counter or a log line. Growing with the collector idle
		// is a guest building its live set, which is what a heap is for.
		if gcm.phase != phaseIdle {
			gcm.outruns++
			warnOutrun()
		}
		growCoverage()
		if si := findSpanRun(n); si != noSpan {
			return si
		}
	}
	if gcm.collecting {
		// Nothing in the collector allocates, so this cannot happen. It costs
		// one compare on a path that is about to trap either way.
		return noSpan
	}
	// memory.grow said no, which on this target means the host refused or the
	// address space ended. THIS is the last resort, and it is the only place
	// left that collects inside a guest call.
	warnGrowRefused()
	Collect()
	endUnpaced()
	if si := findSpanRun(n); si != noSpan {
		return si
	}
	if growHeap(n) {
		growCoverage()
		return findSpanRun(n)
	}
	return noSpan
}

// findSpanRun looks for n consecutive unassigned spans.
//
// IT SEARCHES THE WHOLE HEAP, INCLUDING DURING A SWEEP, and that is stage C's
// one piece of mutator/sweep coupling deleted rather than relaxed. The rule it
// replaces was "not above the sweep cursor", whose justification was sound -- a
// span claimed above the cursor is walked by the sweep afterwards, found to hold
// unmarked slots because nothing marks after termination, and freed with live
// objects in it -- and whose cost was that the mutator's search space was empty
// at the moment marking ended. clsFresh answers the same question per span and
// leaves the search space whole.
func findSpanRun(n uint32) uint32 {
	count := coveredSpans()
	if count < n {
		return noSpan
	}
	start := gcm.spanCursor
	if start > count-n {
		start = 0
	}
	// Two passes: cursor to end, then start to cursor.
	if si := scanSpanRun(start, count, n); si != noSpan {
		gcm.spanCursor = si + n
		return si
	}
	if si := scanSpanRun(0, start, n); si != noSpan {
		gcm.spanCursor = si + n
		return si
	}
	return noSpan
}

// scanSpanRun looks for n consecutive unassigned spans in [from, to). The
// window is checked from its TOP end so that a blocker lets the scan restart
// past it rather than one span along, which is what keeps a run-of-n search
// linear in the table rather than quadratic.
func scanSpanRun(from, to, n uint32) uint32 {
	i := from
	for i+n <= to {
		blocked := false
		for k := n; k > 0; k-- {
			if spanClassOf(i+k-1) != 0 {
				i += k // the blocker is at i+k-1
				blocked = true
				break
			}
		}
		if !blocked {
			return i
		}
	}
	return noSpan
}

// growHeap is memory.grow, and under gc.custom the allocator owns it outright:
// runtime.setHeapEnd is a documented no-op, so runtime.growHeap would grow the
// memory and discard the new bound.
//
// It grows by a quarter, not by doubling. TinyGo's own growHeap doubles, and
// doubling is exactly the ladder this package exists to keep a guest off:
// mem_grow zeroes every new word, MEMSIZE is authoritative on the Lua side, and
// a table that has held 16 million slots is walked as 16 million slots for the
// rest of the session.
//
// AND THE QUARTER IS CAPPED, because a quarter of a large heap is the worst
// tick a growing guest has. mem_grow's zero-fill is ~107 ns a word in
// Factorio's Lua with NO fixed cost to amortise it against -- measured over
// four increments at three heap sizes, the least-squares intercept is negative
// at every one, and reaching 40 MiB in 640 grows of one wasm page costs 0.984x
// what reaching it in 10 grows of 4 MiB costs (scripts/run-growprobe.sh). So
// the argument for a large increment was never a throughput argument; it was an
// assumption about fixed cost that this allocator's own numbers do not support.
//
// The cap bounds the SPECULATIVE part only. `needSpans` always wins, because a
// single allocation of n spans needs n spans whatever the policy says -- a
// megabyte-sized object still forces a megabyte-sized grow, and that is what
// the runtime's paced pre-build is for rather than this.
const growCapSpans = 16 // 64 KiB, one wasm page

// The cap must clear metaChunkSpans, or the coverage-crossing round-up below
// asks for more than the cap allows and the two rules fight over every grow
// that reaches a 4 MiB slice boundary. Compile-time, because the failure would
// be a silently wrong increment rather than an error.
const _ = uint32(growCapSpans - metaChunkSpans)

func growHeap(needSpans uint32) bool {
	want := needSpans
	if q := gcm.spanCount / 4; q > want {
		want = q
	}
	if want > growCapSpans && want > needSpans {
		want = growCapSpans
		if want < needSpans {
			want = needSpans
		}
	}
	if want < 4 {
		want = 4
	}
	// A grow that CROSSES the coverage line must clear the next chunk's ten
	// spans, or the coverage line stays where it is, the new spans are backed
	// and unusable, and allocSpans's loop repeats the grow. A grow that stays
	// inside the covered slice needs nothing -- which is the common case and
	// the reason for the first conjunct: without it, the very first chunk makes
	// every later grow round up to 4 MiB.
	if c := gcm.chunks << (sliceLog - spanLog); gcm.spanCount+want > c &&
		gcm.spanCount+want < c+metaChunkSpans {
		want = c + metaChunkSpans - gcm.spanCount
	}
	lim := maxSpans()
	newCount := gcm.spanCount + want
	if newCount > lim || newCount < gcm.spanCount {
		newCount = lim
	}
	if newCount < gcm.spanCount+needSpans {
		// wasm32's ceiling, not a policy. Reported by returning rather than by
		// trapping, so allocSpans can try the last resort first.
		return false
	}
	needBytes := heapBase + newCount<<spanLog
	havePages := uint32(wasmMemorySize(0))
	// (needBytes-1)>>16 + 1 rather than (needBytes+65535)>>16: needBytes can be
	// within 64 KiB of 2^32 at the ceiling and the rounded-up form wraps there.
	needPages := (needBytes-1)>>16 + 1
	if needPages > havePages {
		if wasmMemoryGrow(0, int32(needPages-havePages)) < 0 {
			return false
		}
		gcm.grows++
	}
	added := newCount - gcm.spanCount
	gcm.spanCount = newCount
	gcm.freeBytes += added << spanLog
	// A grow inside an ALREADY covered slice moves the coverage line too, so
	// the marking range has to follow it here as well as in growCoverage.
	syncHeapTop()
	return true
}

// ---------------------------------------------------------------------------
// Raw memory
// ---------------------------------------------------------------------------

func load32(addr uint32) uint32 { return *(*uint32)(unsafe.Pointer(uintptr(addr))) }
func store32(addr, v uint32)    { *(*uint32)(unsafe.Pointer(uintptr(addr))) = v }

// zero clears a freshly handed-out block. alloc's contract says the memory it
// returns is zeroed, and unlike -gc=leaking -- whose wasm zero_new_alloc is a
// documented no-op because linear memory starts zero and it never reuses a byte
// -- a collector hands back memory somebody has already written. This is a real
// cost that leaking does not pay, and it is part of what the stage-B allocation
// A/B is measuring.
//
// clear() on an unsafe.Slice lowers to llvm.memset, which the FkLua emitter
// then substitutes with the runtime's mem_fill: one bounds check and one page
// mark for the whole span, instead of the byte loop compiler-rt would ship.
//
//go:noinline
func zero(addr, n uint32) {
	if !zeroOnAlloc {
		return
	}
	wipe(addr, n)
}

// wipe is zero WITHOUT the zeroOnAlloc gate, for the bytes that are not a
// freshly handed-out block and whose zeroing is not optional.
//
// -tags fkgcnozero turns zero() off to isolate the cost of zeroing a recycled
// block from the cost of the free list that produced it. That arm is already
// documented as incorrect for a guest's objects; it must not also be incorrect
// for the collector's own tables, where a chunk arriving with the last tenant's
// bytes is a bitmap of set bits and a span table of invented classes.
//
//go:noinline
func wipe(addr, n uint32) {
	clear(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(addr))), n))
}

// hostLog is env.fk_log, the same import guest/go/fk uses -- a (pointer,
// length) into guest memory that the host reads back as a string.
//
// It is declared HERE rather than reached through the fk package for two
// reasons. fkgc is imported by every collected guest and must not drag in a
// package that guest may not want, and more importantly the two functions below
// are called from inside the allocator, where fk.Log's own shadow frame would
// be a cost on a path that must not have one. Everything that touches a string
// here is //go:noinline for the reason allocate documents.
//
//go:wasmimport env fk_log
func hostLog(ptr, length uint32)

//go:noinline
func logString(s string) {
	p := unsafe.StringData(s)
	if p == nil {
		return
	}
	hostLog(uint32(uintptr(unsafe.Pointer(p))), uint32(len(s)))
}

// warnOutrun says, once per guest, that the heap had to GROW because the paced
// collection had not caught up -- which is a cost, not a failure, and the
// message says so.
//
// It replaces a line that said the collector was "collecting INSIDE a guest
// call", because it no longer is: the storm response is growth. The advice is
// the same and the consequence is different -- what a guest pays now is linear
// memory it will not get back rather than a pause it cannot schedule.
//
// Once, because the condition firing repeatedly is a guest in trouble and a
// line per allocation would be a second problem on top of the first.
//
//go:noinline
func warnOutrun() {
	if gcm.valveWarned {
		return
	}
	gcm.valveWarned = true
	logString("fkgc: the heap GREW while a collection was still running -- this " +
		"guest allocates faster than its budget reclaims, so --gc=collected is " +
		"behaving like -gc=leaking until the paced collection catches up. Nothing " +
		"paused and nothing is wrong; the cost is linear memory, which never " +
		"shrinks, at about 0.2 ms of worst tick per MiB. Raise fkgc.SetBudget, " +
		"call fkgc.CollectIfNeeded() more often, or allocate less. See " +
		"agents/gc.md, the reclaim-rate table.")
}

// warnRootBudget says, once per guest, that this guest's ROOT SET is bigger than
// the step budget it asked for, so the collector has raised the budget on its own
// authority.
//
// IT IS HERE BECAUSE NOBODY OUTSIDE CAN SEE THE CONDITION. rootWords is measured
// inside a scan the collector does and the host never sees; Budget() is a number
// the guest chose. Their ratio is the whole of the diagnosis and the collector is
// the only thing that holds both. Before the floor the symptom a guest COULD see
// was a rising Deadlines count, which agents/gc.md and SetBudget's own comment
// both attribute to an allocation rate over the budget -- true for the other
// cause of that symptom, and the wrong end of the mod entirely for this one. It
// cost the first downstream mod a day.
//
// Once, because the condition is a property of the guest's globals and does not
// change while it runs.
//
//go:noinline
func warnRootBudget() {
	if gcm.rootWarned {
		return
	}
	gcm.rootWarned = true
	logString("fkgc: this guest's ROOT SET is larger than one step's budget, so " +
		"the collector has raised the budget to cover it. A termination attempt " +
		"re-scans [__global_base, __heap_base) in one indivisible pass and charges " +
		"what it walked, so a budget under that cost can never finish a mark -- " +
		"nothing would be reclaimed and only the mark deadline would end it. This " +
		"is NOT the allocation rate and fkgc.SetBudget is not the fix: raise the " +
		"budget above fkgc.EffectiveBudget() to choose the pause deliberately, or " +
		"declare fewer/smaller package-level variables. See agents/gc.md, 'the " +
		"root-scan floor'.")
}

// warnGrowRefused is the one remaining place a collection runs inside a guest
// call, and it is reached only when memory.grow itself said no.
//
//go:noinline
func warnGrowRefused() {
	logString("fkgc: memory.grow was REFUSED, so the collector is running a full " +
		"mark and sweep inside a guest call to find room. That is a pause in a " +
		"lockstep game and it is the alternative to trapping. The heap is at the " +
		"limit of what this wasm runtime will hand out.")
}

// oom is noinline for the reason allocate documents: a string constant is a
// pointer and a length, and a pointer live across a call in the allocation path
// is a stack slot zeroed on EVERY allocation. Out of memory is not a hot path;
// paying for it on the hot path would be.
//
// THERE IS NO fkgc HEAP CAP TO REPORT ANY MORE, and the message has to say what
// actually happened instead. Two things reach here and they want different
// advice, so the span count the request wanted is the discriminator:
//
//   - memory.grow was refused. The host, the browser or the address space said
//     no. Nothing a build tag can change.
//   - a run of n contiguous spans could not be found although the heap has the
//     room. That is fragmentation, and above 1,014 spans (just under 4 MiB) it
//     can be structural: a metadata chunk placed under pressure sits at a 4 MiB
//     boundary and a single object cannot straddle one. See growCoverage.
//
// On wasm-unknown there is no stderr, so this is an `unreachable` and the log
// line is the only thing anyone will ever see.
//
//go:noinline
func oom(needSpans uint32) {
	if needSpans > sliceSpans-metaChunkSpans {
		logString("fkgc: OUT OF MEMORY on a SINGLE OBJECT larger than 4 MiB. The " +
			"collector's metadata is allocated from the heap in 40 KiB chunks, one " +
			"per 4 MiB slice, and under memory pressure a chunk lands at a slice " +
			"boundary that a single object cannot straddle. The heap itself has no " +
			"cap. Allocate the object in pieces, or keep enough headroom that the " +
			"chunks stay low. See agents/gc.md.")
	} else {
		logString("fkgc: OUT OF MEMORY -- memory.grow was refused and a full " +
			"collection did not free a span. There is no fkgc heap cap: this is " +
			"the wasm runtime's limit or the 4 GiB wasm32 address space. The mod " +
			"is about to trap with `unreachable`, which is what a wasm guest with " +
			"no stderr can do. See agents/gc.md.")
	}
	runtimePanic("fkgc: out of memory -- memory.grow was refused (there is no fkgc " +
		"heap cap; this is wasm32's limit)")
}
