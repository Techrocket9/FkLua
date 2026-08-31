// Command callcost is the fixture for profiling what a host call costs through
// a REAL compiled guest, rather than through the Lua ABI on its own.
//
// internal/factorio/abicost_test.go measures the host half -- decode, dispatch,
// encode -- with hand-written Lua standing in for the guest. That is a fair
// thing to want and it is not what a mod author pays: the guest's own encode is
// compiled wasm running as interpreted Lua, and `fk_mod.lua`'s dispatch wrapper
// (pcall, depth, transient release, globals sync, packed flush) is paid once per
// EVENT rather than once per call. The first downstream mod measured ~12 µs per
// host call end to end against ~1.5-3 µs for a plain Lua-to-C++ API call, and
// nothing here could say where the rest went.
//
// One probe per shape, selected by the tick the driver passes, so the driver can
// difference them against a dispatch that makes no host call at all.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o callcost.wasm ./examples/callcost
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

var (
	obj      = fkapi.ObjectAt(2) // `game`, fixed by the ABI
	thing    fkapi.Object
	args     fkapi.Value
	sink     string
	sinkBool bool
	sinkObjs []fkapi.Object
	dst      []fkapi.Object
	filter   fkapi.EntitySearchFilters
	// The bulk read's two sides, both allocated once in init for the reason
	// `args` and `dst` are: a probe that grew a slice would be measuring make().
	bulkObjs []fkapi.Object
	bulkDst  []fkapi.BulkOptUint64
	bulkN    int
)

// How many handles the bulk probes read in one crossing. Two sizes, because the
// whole claim about a bulk form is that its ONE fixed cost amortizes -- a single
// size would say nothing about the shape of that curve, and N=4 is where a
// caller most doubts it.
const (
	bulkSmall = 4
	bulkLarge = 256
)

// The name the stub's object carries. One constant for both name probes, so
// their difference is the mechanism rather than the string.
const wantName = "transport-belt-of-a-perfectly-ordinary-length"

func init() {
	// Built once, outside every probe: a caller that rebuilds its argument each
	// call pays for that in its own language, and mixing the two in one number
	// is how a profile stops saying anything.
	args = fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("iron-chest")},
		fkapi.KeyValue{Key: fkapi.OfString("force"), Val: fkapi.OfString("player")},
	)
	thing = fkapi.ObjectAt(2)
	// The destination the Into probe reuses, sized past what the stub returns
	// so that the steady state is a reuse rather than a grow. Allocated HERE,
	// outside every probe, for the same reason `args` is.
	dst = make([]fkapi.Object, 0, 16)
	// The handle array a search would have written, and the destination the
	// bulk read fills. Both are the guest's own memory and neither is touched
	// inside a probe.
	bulkObjs = make([]fkapi.Object, bulkLarge)
	for i := range bulkObjs {
		bulkObjs[i] = thing
	}
	bulkDst = make([]fkapi.BulkOptUint64, bulkLarge)
}

//go:wasmexport fk_on_tick
func onTick(which uint32) {
	switch which {
	case 0:
		// The baseline: a dispatch that makes no host call. Everything the
		// other probes cost above this is the call itself.
	case 1:
		// No argument block and no return block: pure dispatch through the
		// handle table and the member entry.
		fkapi.LuaGameScript{Object: obj}.ReloadScript()
	case 2:
		// One scalar in, one scalar out: two blocks and two field codecs.
		fkapi.LuaEntity{Object: thing}.Health()
	case 3:
		// A string return, which goes through the scratch region on the host
		// side and a Go string copy on the guest side.
		sink, _ = fkapi.LuaEntity{Object: thing}.Name()
	case 4:
		// A tier-2 map argument, hoisted: writeDyn on the guest side, read_dyn
		// on the host side, and the arena underneath both.
		fkapi.LuaSurface{Object: thing}.CreateEntity(args)
	case 5:
		// THE DOWNSTREAM NAME PATTERN. Read the name and compare it: the string
		// crosses into guest memory (fk_alloc, fk_wstr, a Go string copy) and is
		// thrown away one line later.
		n, _ := fkapi.LuaEntity{Object: thing}.Name()
		sinkBool = n == wantName
	case 6:
		// ...and the same question asked HOST-side. The (ptr, len) of a Go
		// string constant goes out -- its bytes are in the data section, so
		// nothing is marshalled -- and a bool comes back.
		sinkBool, _ = fkapi.LuaEntity{Object: thing}.NameIs(wantName)
	case 7:
		// THE ANSWER THE DOWNSTREAM SHAPE USUALLY GETS. A guest subscribed to a
		// CATEGORY filter is entered for entities it does not want, so "no" is
		// the common case -- and a length that does not match settles it host-
		// side without decoding a byte out of guest memory. Probe 6 is the
		// matching answer, which has to decode; the pair is the fast path.
		sinkBool, _ = fkapi.LuaEntity{Object: thing}.NameIs("iron-chest")
	case 8:
		// A container return, allocating a fresh slice on every call.
		sinkObjs, _ = fkapi.LuaSurface{Object: thing}.FindEntitiesFiltered(filter)
	case 9:
		// ...and into a destination whose capacity is already there. Same host
		// call, same member id, same blocks: only the guest's slice differs.
		dst, _ = fkapi.LuaSurface{Object: thing}.FindEntitiesFilteredInto(dst, filter)
	case 10:
		// A BULK ATTRIBUTE READ of four handles in ONE crossing. Divide by four
		// to compare against probe 2, which is the same attribute read one
		// handle at a time: this is where a caller finds out whether one
		// crossing has paid for itself yet.
		bulkN, _ = fkapi.LuaEntityUnitNumberBulk(bulkObjs[:bulkSmall],
			bulkDst[:bulkSmall])
	case 11:
		// ...and of 256, which is the size a poll actually is. Divide by 256.
		bulkN, _ = fkapi.LuaEntityUnitNumberBulk(bulkObjs[:bulkLarge],
			bulkDst[:bulkLarge])
	}
	_ = bulkN
}

func main() {}
