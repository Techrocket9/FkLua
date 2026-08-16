//go:build !gc.custom

package fkgc

// This file is the whole package under any -gc except custom.
//
// It exists so that `import _ ".../fkgc"` is unconditional in guest source. A
// guest is built at different -gc settings by the same harnesses (the stage-B
// allocation A/B builds one source tree three ways), and a guest author should
// not have to put a build tag on an import to change one flag.
//
// Nothing here allocates, links against the runtime, or has any state, so a
// -gc=leaking build that imports this package emits what it emitted before.

// Enabled reports whether a collector is compiled in. It is false unless the
// guest was built with -gc=custom.
func Enabled() bool { return false }

// Collect is a no-op without a collector.
func Collect() {}

// CollectIfNeeded is a no-op without a collector.
func CollectIfNeeded() bool { return false }

// Start is a no-op without a collector: there is nothing to collect and no
// barrier to arm.
func Start() bool { return false }

// Stats returns a zero value without a collector. HeapBytes is still the honest
// answer for a leaking build in the sense that matters -- nothing here can see
// the runtime's own bump pointer.
func Stats() MemStats { return MemStats{} }

// SetThreshold is a no-op without a collector.
func SetThreshold(bytes uint32) {}

// SetBudget is a no-op without a collector: there are no steps to pace.
func SetBudget(units uint32) {}

// Budget reports the per-step work allowance, zero without a collector.
func Budget() uint32 { return 0 }

// EffectiveBudget is Budget floored at what a termination attempt's root re-scan
// costs. Zero without a collector: there is no scan to pay for.
func EffectiveBudget() uint32 { return 0 }

// Phase reports what the collector is doing: always idle without one.
func Phase() uint32 { return 0 }

// MaxStepWork is the most any single collection step charged. There are no
// steps without a collector.
func MaxStepWork() uint32 { return 0 }

// TotalWork is what a collection charged in total. There are no collections
// without a collector.
func TotalWork() uint32 { return 0 }

// Step is a no-op without a collector and reports idle. It exists so that a
// guest driving the collector by hand compiles under both -gc settings.
func Step(ndirty uint32) uint32 { return 0 }

// DirtyBase and DirtyCap describe the buffer the host writes dirtied page
// numbers into. There is no barrier without a collector, so there is no buffer.
func DirtyBase() uint32 { return 0 }

// DirtyCap is zero without a collector.
func DirtyCap() uint32 { return 0 }

// DirtyAll is the count a step is handed when the host cannot say which pages
// were written -- after a load, or from a synchronous collection with no host
// to ask. Declared in both builds so a guest can name it either way.
const DirtyAll = ^uint32(0)

// HeapBase, HeapTop and the metadata surface are all zero without a collector:
// this build does not own the heap, the runtime does, and there is no metadata.
func HeapBase() uint32  { return 0 }
func HeapTop() uint32   { return 0 }
func MetaBytes() uint32 { return 0 }

// MetaFixedBytes, MetaChunks, MetaChunkBytes and MetaSliceBytes describe the
// scaling-metadata model. Zero here, so that a guest logging its own memory
// model compiles under both -gc settings.
func MetaFixedBytes() uint32 { return 0 }
func MetaChunks() uint32     { return 0 }
func MetaChunkBytes() uint32 { return 0 }
func MetaSliceBytes() uint32 { return 0 }

// The pacing diagnostics, all zero without a collector.
func MaxUnpacedWork() uint32  { return 0 }
func UnpacedWork() uint32     { return 0 }
func Outruns() uint32         { return 0 }
func MaxUnpacedFolds() uint32 { return 0 }
func Marked() uint32          { return 0 }
func Stalls() uint32          { return 0 }
func MaxStalls() uint32       { return 0 }
func PendEmpties() uint32     { return 0 }
func WorkOwed() uint32        { return 0 }
func RootWords() uint32       { return 0 }
func Terminations() uint32    { return 0 }
func MarkBitsSet() uint32     { return 0 }
func Rescans() uint32         { return 0 }
func DirtyOverflows() uint32  { return 0 }
func RescanRestarts() uint32  { return 0 }
func BackedBytes() uint32     { return 0 }
func Reinitialize()           {}

// THERE IS NO HeapCap ANY MORE, IN EITHER BUILD.
//
// It was a HARD cap -- 16 MiB by default, moved by -tags fkgcheap4/fkgcheap64 --
// and it existed only because the mark bitmap and the span table were statically
// reserved .bss. Sharding stage C made that metadata scale with the heap, and
// FkLua imposes no memory cap beyond what Factorio imposes: a collected guest
// grows exactly like a leaking one, on a sharded linear memory with no wall in
// it. A guest naming the old constant now fails to compile, which is the
// intended way to find out, because what replaced it is a COST -- Stats().
// MetaBytes, and agents/guests.md's memory table -- and not a bigger number.
