// Command handles is the HANDLE OWNERSHIP guest, and its Rust twin under
// guest/rust/examples/handles logs the same transcript line for line.
//
// Two things had no answer on the guest side until this landed, and a mod
// holding handles across events found both. There was no way to ASK which space
// a handle is in, so a double retain was silent and an author reasoning about
// ownership had only the number to look at. And on the Rust side the retain and
// release pair had to be balanced by hand on every path: the first Rust guest
// keeping handles across events leaked on three of them -- a build that had
// retained three handles before a fourth create failed, an early return past
// the release, and a helper that returned a null handle its caller went on
// holding.
//
// The predicates answer the first in both languages. The second is where the
// two languages part: Rust gets an Object::retained guard that releases on
// Drop, and Go gets `defer o.Release()`, because Go has no destructor and
// wrapping the defer in a type would only hide it. What this guest shows is
// that the OBSERVABLE is the same either way -- guardSlot below is the Go
// idiom, and the slot it frees is the one the next retain gets.
//
// WHY THE SLOT NUMBERS ARE THE ASSERTION. There is no accessor for the size of
// the host's handle table, and adding one would be a new import for a test's
// convenience. The slot index a retain hands back is a deterministic proxy: the
// persistent free list is LIFO during play, so a released slot is the very next
// one handed out, and a release that did not happen shows up as the next retain
// taking a NEW number instead.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o handles.wasm ./examples/handles
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// surface hands back a fresh TRANSIENT handle on every call.
//
// A separate call rather than one handle used several times, and that is what
// makes the slot arithmetic below mean anything: each call wraps the object in
// its own transient handle, so each retain allocates its own persistent slot.
func surface() fkapi.Object {
	s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1))
	if err != nil || s == nil {
		fk.Log("handles: game.get_surface(1) failed, so there is nothing to retain")
		return fkapi.Object{}
	}
	return *s
}

func yn(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func num(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

// class names the one space a handle is in. The three predicates partition
// every number, so exactly one arm can match and "none" is the null handle.
func class(o fkapi.Object) string {
	switch {
	case o.Global():
		return "global"
	case o.Persistent():
		return "persistent"
	case o.Transient():
		return "transient"
	}
	return "none"
}

func predicates(o fkapi.Object) string {
	return "persistent=" + yn(o.Persistent()) +
		" transient=" + yn(o.Transient()) +
		" global=" + yn(o.Global())
}

// guardSlot is the Go idiom for what Rust's Retained does on Drop: retain,
// defer the release, and let every path out of the scope free the slot.
func guardSlot() uint32 {
	o := surface().Retain()
	defer o.Release()
	return o.Handle()
}

//go:wasmexport fk_on_init
func onInit() {
	// BOTH SPLIT CONSTANTS, asked of hand-built handles rather than of whatever
	// the host happened to hand out. 9 and 10 straddle the global boundary and
	// 1073741823 and 1073741824 straddle the transient one, so a constant that
	// is off by one in either direction moves this line.
	line := "handles: classify"
	for _, h := range []uint32{0, 1, 9, 10, 1073741823, 1073741824, 4294967295} {
		line += " " + num(h) + "=" + class(fkapi.ObjectAt(h))
	}
	fk.Log(line)

	// What the API hands back is transient. Nothing here asserts WHICH transient
	// number it is: the space is the claim.
	a := surface()
	fk.Log("handles: fresh " + predicates(a))

	// ...and a retain moves it into the persistent space, at the first slot.
	r := a.Retain()
	fk.Log("handles: retained slot=" + num(r.Handle()) + " " + predicates(r))

	// IDEMPOTENCE. Retaining a handle that is already persistent hands the same
	// number back rather than allocating a second slot onto the same object, so a
	// second retain does not LEAK. It buys no ownership either: there is still one
	// slot, and the release that pairs with the second retain frees whatever the
	// next retain took. Release a slot exactly once.
	fk.Log("handles: idempotent slot=" + num(r.Retain().Handle()))

	// The guard's slot, freed by the time this line is logged.
	g := guardSlot()
	fk.Log("handles: guard slot=" + num(g))

	// ...and the next retain gets it back. THIS is the release, observed.
	b := surface().Retain()
	fk.Log("handles: after release slot=" + num(b.Handle()))

	// The other half: a handle taken out of the guard's ownership and NOT
	// released. Rust spells it Retained::into_object; Go has no guard to take it
	// out of, so the mirror is a retain whose release the guest keeps for later.
	kept := surface().Retain()
	fk.Log("handles: kept slot=" + num(kept.Handle()))

	// So the next retain takes a NEW slot, because nothing was freed.
	c := surface().Retain()
	fk.Log("handles: after keep slot=" + num(c.Handle()))

	// A global is not a slot this guest owns: releasing one is StatusBadHandle.
	fk.Log("handles: global " + predicates(fkapi.Game.Object))

	// And the null handle is in no space at all.
	fk.Log("handles: null " + predicates(fkapi.Object{}))
}

func main() {}
