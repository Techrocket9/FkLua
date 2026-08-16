// Command heap measures how much guest heap a host call keeps forever.
//
// Under -gc=leaking -- mandatory, because a collector's pause lands in a
// lockstep game loop -- nothing a guest allocates is ever reclaimed. So every
// byte the ABI allocates per host call is a byte in every save and every
// multiplayer join, and a mod that calls the API in volume (a compiler, a
// mass-builder) accumulates them at the rate it calls.
//
// The measurement is the allocator's own bump pointer, read by allocating one
// byte: the difference across N calls IS the bytes those calls kept. Nothing
// here times anything -- fklua bench does that -- and nothing here is clever;
// the number is only trustworthy because it is the allocator's answer rather
// than an accounting of what the code looks like it should do.
//
// Three probes, chosen so the ABI's share can be told from the caller's:
//
//   - a hoisted tier-2 argument, which allocates ONLY inside the ABI;
//
//   - a string return, whose Go string belongs to the caller and cannot be
//     arena'd by anyone;
//
//   - a scalar read, the control, which should allocate nothing either way.
//
//     tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o heap.wasm ./examples/heap
package main

import (
	"strconv"
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// Enough that a per-call cost dominates the one-byte probe and any rounding
// the allocator does, and few enough that the run stays quick.
const iters = 200

// Somewhere for a result to go that the optimizer cannot prove is unread.
var (
	sink     string
	sinkBool bool
	sinkObjs []fkapi.Object
)

// The name the stub's object carries. A package-level constant rather than a
// literal at each use, so the two name probes below compare against the SAME
// bytes and their difference is the mechanism rather than the string.
const wantName = "transport-belt-of-a-perfectly-ordinary-length"

func u(n int) string { return strconv.FormatUint(uint64(n), 10) }

// heapTop is the allocator's bump pointer, read the only way a guest can read
// it: by asking for a byte and looking at where it landed.
func heapTop() uintptr {
	b := make([]byte, 1)
	return uintptr(unsafe.Pointer(&b[0]))
}

func report(what string, before, after uintptr) {
	fk.Log(what + ": " + u(int(after-before)/iters) + " B/call")
}

//go:wasmexport fk_on_init
func onInit() {
	players, err := fkapi.Game.ConnectedPlayers()
	if err != nil || len(players) == 0 {
		fk.Log("no object to call, so nothing here would mean anything")
		return
	}
	first := players[0]
	surface := fkapi.LuaSurface{Object: first}
	entity := fkapi.LuaEntity{Object: first}

	// Built ONCE. A caller that rebuilds its argument every call pays for that
	// itself, in its own language, and no arena on this side can help -- so
	// hoisting it is what isolates the ABI's own share.
	args := fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("iron-chest")},
		fkapi.KeyValue{Key: fkapi.OfString("force"), Val: fkapi.OfString("player")},
		fkapi.KeyValue{Key: fkapi.OfString("bar"), Val: fkapi.OfNumber(4)},
	)

	// One warm-up call per probe, outside the window: the first call through
	// any path grows whatever it is going to grow, and that is a fixed cost
	// rather than a per-call one.
	surface.CreateEntity(args)
	b := heapTop()
	for i := 0; i < iters; i++ {
		surface.CreateEntity(args)
	}
	report("tier2 arg", b, heapTop())

	// The result is KEPT. Discarding it lets the optimizer delete the copy that
	// makes the string a Go value, and the probe then reports a cost no real
	// caller avoids -- which is how this line first read 0.
	sink, _ = entity.Name()
	b = heapTop()
	for i := 0; i < iters; i++ {
		sink, _ = entity.Name()
	}
	report("string ret", b, heapTop())

	// THE DOWNSTREAM PATTERN: read the name, compare it, throw it away.
	//
	// A guest subscribed with a CATEGORY filter -- "transport-belt-connectable"
	// -- is entered for every entity anyone builds anywhere on the map, and has
	// to read the name to discover it does not care. So the string is bought
	// before the decision that would have said not to buy it, and under
	// -gc=leaking the copy is permanent. This is the shape no downstream gate
	// can remove by writing better code, which is why it is measured here and
	// not left as an argument.
	sink, _ = entity.Name()
	sinkBool = sink == wantName
	b = heapTop()
	for i := 0; i < iters; i++ {
		n, _ := entity.Name()
		sinkBool = sinkBool != (n == wantName)
	}
	report("name cmp", b, heapTop())

	// ...and the same question asked of the host instead. The comparison
	// happens in Lua against a string the host already holds; nothing is
	// written into guest memory, so there is nothing to keep.
	sinkBool, _ = entity.NameIs(wantName)
	b = heapTop()
	for i := 0; i < iters; i++ {
		v, _ := entity.NameIs(wantName)
		sinkBool = sinkBool != v
	}
	report("name is", b, heapTop())

	// THE OTHER DOWNSTREAM PATTERN: a container return, per call.
	//
	// `out := make([]Object, n)` is a fresh allocation every call and the guest
	// reads it once. The ELEMENTS are already free -- the host writes them into
	// the marshalling arena, which the binding's own bracket reclaims -- so the
	// slice's backing array is the whole of what is kept.
	var filter fkapi.EntitySearchFilters
	sinkObjs, _ = surface.FindEntitiesFiltered(filter)
	b = heapTop()
	for i := 0; i < iters; i++ {
		sinkObjs, _ = surface.FindEntitiesFiltered(filter)
	}
	report("array ret", b, heapTop())

	// ...and into a destination the caller keeps. The first call may still
	// grow it; every call after reuses the capacity, which is what the probe
	// sees because the warm-up is outside the window.
	dst := make([]fkapi.Object, 0, 8)
	dst, _ = surface.FindEntitiesFilteredInto(dst, filter)
	b = heapTop()
	for i := 0; i < iters; i++ {
		dst, _ = surface.FindEntitiesFilteredInto(dst, filter)
	}
	report("array into", b, heapTop())
	sinkObjs = dst

	entity.Health()
	b = heapTop()
	for i := 0; i < iters; i++ {
		entity.Health()
	}
	report("scalar ret", b, heapTop())

	entity.SetHealth(1)
	b = heapTop()
	for i := 0; i < iters; i++ {
		entity.SetHealth(1)
	}
	report("scalar arg", b, heapTop())

	// The control: a member with NO argument block and NO return block, so
	// nothing on either side has an address to take. If this is not zero the
	// probe itself is allocating and no other number here can be read.
	surface.CreateGlobalElectricNetwork()
	b = heapTop()
	for i := 0; i < iters; i++ {
		surface.CreateGlobalElectricNetwork()
	}
	report("no blocks", b, heapTop())
}

func main() {}
