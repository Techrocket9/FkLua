// Command churn is the allocation-churn guest: what an ORDINARY allocating Go
// event handler costs a guest that cannot collect.
//
// It exists for the incremental-collector milestone (agents/gc.md) and it has
// two jobs. Now, in stage A, it is a measurement: the allocator's own bump
// pointer says how many bytes per event a handler written the way Go is
// normally written keeps forever under the mandatory -gc=leaking, which is the
// allocation RATE the collector has to outrun. Later it is the acceptance
// vehicle: the same guest, unchanged, has to run indefinitely under the
// collector with a bounded heap, and the number below has to stop growing.
//
// The work is deliberately the shape agents/guests.md tells authors to avoid --
// per-event maps, per-event slices, per-event strings, a retained index rebuilt
// each time. That is the point. The heap-diet section in BetterBeltBalancer's
// CLAUDE.md is what a mod author has to do today to stay inside the budget
// (one reusable [512]byte, `copy` rather than `append`, `unsafe.String`), and
// the whole argument for a collector is that they should not have to.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 -o churn.wasm ./examples/churn
//
// Nothing here calls the Factorio API, so it runs under bin/lua52f as well as
// in game: the allocation is the guest's own, which is exactly the allocation a
// collector would have to reclaim.
package main

import (
	"strconv"
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// The retained state a real handler keeps between events -- small, bounded, and
// NOT what this measures. Everything the handler builds per event is transient.
var (
	index   = map[uint32]uint32{}
	cluster []uint32
	seen    uint64
	sink    string
	sinkN   uint32
)

// entity is a stand-in for whatever an on_built_entity handler reads off the
// event: an id, a position, a couple of flags.
type entity struct {
	id     uint32
	x, y   int32
	kind   uint8
	facing uint8
}

// heapTop is the allocator's bump pointer, read the only way a guest can read
// it -- by asking for a byte and looking at where it landed. Same probe as
// examples/heap, and trustworthy for the same reason: it is the allocator's
// answer rather than an accounting of what the code looks like it should do.
func heapTop() uintptr {
	b := make([]byte, 1)
	return uintptr(unsafe.Pointer(&b[0]))
}

func u(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// handleBuilt is one event's worth of ordinary allocating Go.
//
// Every allocation here is transient by intent -- a collector reclaims all of
// it and -gc=leaking reclaims none of it. Written the way the Go standard
// library would have you write it, not the way this project's heap budget makes
// you write it.
func handleBuilt(id uint32, x, y int32) {
	// A per-event slice of neighbours, appended into rather than sized up
	// front, so it reallocates the way `append` really does.
	var neigh []entity
	for d := uint32(0); d < 8; d++ {
		neigh = append(neigh, entity{
			id:     id ^ (d * 2654435761),
			x:      x + int32(d) - 4,
			y:      y + int32(d/3) - 1,
			kind:   uint8(d & 3),
			facing: uint8(d >> 2),
		})
	}

	// A per-event map keyed by a per-event string, which is the combination
	// this project's own heap diet found to be the expensive one.
	byKind := map[string][]uint32{}
	for _, e := range neigh {
		k := "kind-" + u(uint32(e.kind))
		byKind[k] = append(byKind[k], e.id)
	}

	// A per-event index rebuild: the retained map is cleared and refilled,
	// which allocates buckets even though the map variable itself is reused.
	for k := range index {
		delete(index, k)
	}
	for _, e := range neigh {
		index[e.id] = uint32(e.kind)
	}

	// A per-event line of formatted text, built with `+`. This is precisely
	// the shape that turned out to BE the downstream mod's whole guest heap,
	// and it stays here rather than being fixed, because a guest author should
	// not have to know that.
	s := "[churn] built id=" + u(id) + " neigh=" + u(uint32(len(neigh))) +
		" kinds=" + u(uint32(len(byKind)))
	for _, e := range neigh {
		s += "," + u(e.id&0xffff)
	}
	sink = s
	sinkN += uint32(len(s))

	// A retained slice that a real handler would cap; capped here at a size a
	// mod would consider small, so the LIVE set stays bounded and everything
	// else really is garbage.
	cluster = append(cluster, id)
	if len(cluster) > 256 {
		cluster = cluster[len(cluster)-256:]
	}
	seen++
}

// churn_events runs n events and returns a checksum, so a harness can prove two
// builds did the same work before comparing anything else about them.
//
//go:wasmexport churn_events
func churnEvents(n uint32) uint32 {
	for i := uint32(0); i < n; i++ {
		handleBuilt(i*2654435761+1, int32(i%64), int32(i/64))
	}
	return sinkN ^ uint32(seen)
}

// churn_bytes_per_event is the measurement: the bump pointer's movement across
// n events, divided by n. Under -gc=leaking this is bytes KEPT FOREVER per
// event -- in the heap, in the save and in every multiplayer join.
//
// One warm-up event runs outside the window, because the first event through
// any path grows whatever it is going to grow and that is a fixed cost.
//
//go:wasmexport churn_bytes_per_event
func churnBytesPerEvent(n uint32) uint32 {
	if n == 0 {
		return 0
	}
	handleBuilt(1, 0, 0)
	before := heapTop()
	for i := uint32(0); i < n; i++ {
		handleBuilt(i*2654435761+1, int32(i%64), int32(i/64))
	}
	return uint32((heapTop() - before) / uintptr(n))
}

// churn_heap_top exposes the bump pointer, so a driver can watch the heap grow
// across a run rather than only sampling it around a window.
//
//go:wasmexport churn_heap_top
func churnHeapTop() uint32 { return uint32(heapTop()) }

// churn_collect runs one full stop-the-world collection and returns the heap
// size afterwards, so a harness can drive collections from OUTSIDE a guest call
// -- which is the only place agents/gc.md allows one until stage C makes a step
// bounded.
//
//go:wasmexport churn_collect
func churnCollect() uint32 {
	gcCollect()
	return gcStat(0)
}

// churn_gc_tick is the pressure-gated collection a guest really puts in
// fk_on_tick: it collects only when enough has been allocated since the last
// one. It returns 1 if it collected, so a harness can count cycles without
// asking the collector.
//
//go:wasmexport churn_gc_tick
func churnGCTick() uint32 {
	if gcTick() {
		return 1
	}
	return 0
}

// churn_gc_stat exposes the collector's own view of itself by index, because a
// wasmexport cannot return a struct: 0 heap, 1 live, 2 free, 3 collections,
// 4 grows, 5 mallocs, 6 frees, 7 bytes since the last collection, 8 the
// metadata's own footprint, 9 whether a collector is compiled in at all.
// `grows` is the acceptance number -- agents/gc.md's stage-D test is "no
// doubling logged", and a collector that works holds it flat forever.
//
//go:wasmexport churn_gc_stat
func churnGCStat(which uint32) uint32 {
	if which == 9 {
		if gcEnabled() {
			return 1
		}
		return 0
	}
	return gcStat(which)
}

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("[churn] " + u(churnBytesPerEvent(200)) + " B/event, kept forever under -gc=leaking")
}

// fk_on_event is what makes this the acceptance vehicle rather than only a
// probe: subscribe it in game and every build event runs one handler's worth of
// ordinary allocating Go.
//
//go:wasmexport fk_on_event
func onEvent(id uint32, ptr uint32) {
	handleBuilt(ptr, int32(id), 0)
	if seen%1024 == 0 {
		fk.Log("[churn] events=" + u(uint32(seen)) + " heap_top=" + u(uint32(heapTop())))
	}
}

func main() {}
