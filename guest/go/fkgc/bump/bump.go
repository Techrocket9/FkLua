//go:build gc.custom

// Package bump is a -gc=custom allocator that only ever bumps a pointer, and it
// exists to answer exactly one question.
//
// agents/gc.md's risk 1, and stage B's second kill criterion:
//
//	-gc=leaking's alloc is a bump pointer; a free-list allocator is a walk,
//	and that cost is paid by every allocation whether or not a collection is
//	running -- by a guest that opted in, at idle, forever. [...] It is the
//	first thing stage B should measure, before writing a collector: build
//	churn with -gc=custom and a bump allocator that never frees, and compare
//	against -gc=leaking. That isolates the plumbing from the policy in one
//	A/B.
//
// That is what this is. Same hooks, same ownership of memory.grow, same
// //go:linkname seam, no free lists, no size classes, no metadata and no
// collector -- so an A/B against -gc=leaking prices the -gc=custom PLUMBING and
// nothing else, and a second A/B against the real fkgc prices the free-list
// POLICY on top of it.
//
// It is a measurement instrument, not a shipping allocator: a guest built with
// it leaks exactly as -gc=leaking does, and calling runtime.GC() does nothing.
// Nothing outside scratchpad/gc and the stage-B harness should import it.
package bump

import "unsafe"

//go:linkname initHeap runtime.initHeap
func initHeap() {
	base := uint32(uintptr(unsafe.Pointer(&heapBaseSymbol)))
	next = (base + 15) &^ 15
	limit = uint32(wasmMemorySize(0)) << 16
}

//go:linkname alloc runtime.alloc
func alloc(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	n := (uint32(size) + 15) &^ 15
	if n == 0 {
		n = 16
	}
	p := next
	for p+n > limit {
		// The allocator owns memory.grow under gc.custom, because
		// runtime.setHeapEnd is a no-op there and runtime.growHeap would throw
		// the new bound away. Doubling, which is what TinyGo's own growHeap
		// does, so that this arm is the same shape as -gc=leaking's.
		have := wasmMemorySize(0)
		if wasmMemoryGrow(0, have) < 0 {
			runtimePanic("bump: out of memory")
		}
		limit = uint32(wasmMemorySize(0)) << 16
	}
	next = p + n
	total += uint64(n)
	mallocs++
	// Deliberately NOT zeroed, exactly like -gc=leaking's wasm build, whose
	// zero_new_alloc is a documented no-op: linear memory starts zero and a
	// bump allocator never hands the same byte out twice. Zeroing is a cost of
	// REUSE, and pricing it here would put the free-list allocator's cost in
	// the plumbing arm.
	return unsafe.Pointer(uintptr(p))
}

//go:linkname free runtime.free
func free(ptr unsafe.Pointer) {}

//go:linkname markRoots runtime.markRoots
func markRoots(start, end uintptr) {}

//go:linkname gcCollect runtime.GC
func gcCollect() {}

//go:linkname setFinalizer runtime.SetFinalizer
func setFinalizer(obj interface{}, finalizer interface{}) {}

type runtimeMemStats struct {
	Alloc, Sys, HeapAlloc, HeapSys, HeapIdle, HeapInuse, HeapReleased,
	HeapObjects, TotalAlloc, Mallocs, Frees, GCSys uint64
}

//go:linkname readMemStats runtime.ReadMemStats
func readMemStats(ms *runtimeMemStats) {
	ms.HeapSys = uint64(limit)
	ms.TotalAlloc = total
	ms.Mallocs = mallocs
}

//go:linkname runtimePanic runtime.runtimePanic
func runtimePanic(msg string)

//go:extern __heap_base
var heapBaseSymbol [0]byte

//export llvm.wasm.memory.size.i32
func wasmMemorySize(index int32) int32

//export llvm.wasm.memory.grow.i32
func wasmMemoryGrow(index int32, delta int32) int32

var (
	next    uint32
	limit   uint32
	total   uint64
	mallocs uint64
)

// HeapTop is the bump pointer, which is the same probe examples/churn uses --
// the allocator's own answer rather than an accounting of what the code looks
// like it should do.
func HeapTop() uint32 { return next }
