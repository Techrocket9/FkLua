// Command retain is the round-trip's PERSISTENT HANDLE guest.
//
// `fk_abi.lua` has always said a retained handle "lives in `storage` so it
// survives a save. A guest that stashes an entity across a save gets the entity
// back rather than a dangling index." It did not: `M.persistent_table()` and
// `M.adopt()` both existed and `fk_mod.lua` called neither, so the whole space
// was session-scoped and the promise had nothing behind it. Found by the first
// mod that ever called `fk_retain` (fklua-ports-samples, G3), where it
// presented as thirteen sensors going invalid on the first tick after a load,
// before anything had touched the world.
//
// It is a round-trip guest and not a unit test because of WHERE the defect was.
// Nothing was wrong inside `fk_abi.lua`, so no test of `fk_abi.lua` could see
// it; what was missing was two lines in the mod glue, and the thing they are
// about is a real Factorio serializing a real `LuaObject` reference into a real
// save. The host-side suite replays the control.lua protocol through a stand-in
// `storage` (internal/factorio/retain_test.go) and that is the sharp test; this
// is the one that says the engine agrees.
//
// WHAT IT CHECKS, and the third is the one a partial fix gets wrong:
//
//   - two handles retained before the save still name their objects after it,
//     which is the promise;
//
//   - a handle RELEASED before the save is still free after it, and the slot it
//     freed is the one the next retain gets. `M.adopt` rebuilds the free list
//     rather than restoring it, because a stale free list read back from a save
//     would hand out a slot that is still in use -- two guest handles aliasing
//     one object, which is corruption rather than a leak. That rebuild is
//     deterministic (ascending, from the saved table alone), so every client of
//     a multiplayer game derives the same list;
//
//   - the objects are reached by CALLING something on them, not by looking at
//     the number. A handle that resolves to nothing would still be an integer.
//
//     tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o retain.wasm ./examples/retain
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// The retained handles, in guest memory, which is what makes this a test of the
// HANDLE SPACE rather than of anything else: under --persist=table these three
// uint32s come back because linear memory came back, and they mean nothing
// unless the host's side of the arrangement came back too.
var (
	kept    [2]fkapi.Object
	freed   fkapi.Object
	regot   fkapi.Object
	seen    uint32
	stashed bool
)

// stash retains three handles to surface 1 and gives the middle one back.
//
// Three separate GetSurface calls rather than one retained three times: each
// hands back its own transient handle, so each retain allocates its own
// persistent slot -- 10, 11, 12 -- and releasing the middle one leaves a hole
// for the free list to rebuild around.
func stash() {
	var got [3]fkapi.Object
	for i := range got {
		s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1))
		if err != nil || s == nil {
			fk.Log("retain guest: game.get_surface(1) failed, so there is nothing to retain")
			return
		}
		got[i] = s.Retain()
	}
	kept[0], freed, kept[1] = got[0], got[1], got[2]
	freed.Release()
	stashed = true
	fk.Log("retain guest: stashed 2 handles and freed 1")
}

//go:wasmexport fk_on_init
func onInit() {
	// NOT here, and that is worth stating rather than leaving to be discovered:
	// `game` does not exist while control.lua is loading, and on_init runs
	// before the surfaces this asks for are worth asking about. The first tick
	// is early enough -- the save is taken at 60.
	fk.Log("retain guest: waiting for the first tick to stash")
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++
	if !stashed {
		stash()
	}

	// On the TICK rather than on the count, which is what `hello` does and what
	// run-roundtrip.sh greps for: the script asks for `tick $CHECK_TICK seen=`,
	// and a report every tenth SEEN tick lands one short of it forever.
	if tick%10 != 0 {
		return
	}

	// EVERY REPORT RE-RESOLVES, rather than reporting a remembered success. A
	// handle is only as good as the call it survives, and the failure this guest
	// exists for is one that appears at the load boundary and nowhere else.
	live := 0
	for _, o := range kept {
		if ok, err := (fkapi.LuaSurface{Object: o}).NameIs("nauvis"); err == nil && ok {
			live++
		}
	}

	// ...and the freed slot is handed to the next retain, which is the free list
	// having been REBUILT from the saved table rather than lost with it. Done
	// once and given straight back, so the number is stable on every report:
	// before the save the free list holds 11, and after it, it must again.
	slot := uint32(0)
	if s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1)); err == nil && s != nil {
		regot = s.Retain()
		slot = regot.Handle()
		regot.Release()
	}

	fk.Log("tick " + strconv.FormatUint(uint64(tick), 10) +
		" seen=" + strconv.Itoa(int(seen)) +
		" retained=" + strconv.Itoa(len(kept)) +
		" live=" + strconv.Itoa(live) +
		" reused=" + strconv.FormatUint(uint64(slot), 10))
}

func main() {}
