// Package fkgc is the guest-side garbage collector for a FkLua guest built
// with TinyGo's `-gc=custom`.
//
// A guest that imports it gets a real collector for its own heap. A guest that
// does not is unchanged: under any other `-gc` this package is empty, so the
// import costs nothing and a build under the mandatory-until-now `-gc=leaking`
// is byte-identical to what it was.
//
//	import _ "github.com/Techrocket9/fklua/guest/go/fkgc"
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
//	    -o guest.wasm ./mypackage
//
// # Why this exists
//
// `-gc=leaking` makes a guest's memory an arena that only grows, and
// agents/guests.md's heap budget is the advice that follows from it: allocate
// less, and allocate it once. The reason that advice is not merely annoying is
// in agents/gc.md -- linear memory never shrinks, Lua walks the whole word
// table in one indivisible propagatemark, and the worst tick is 0.2 ms per MiB
// of linear memory whether or not the guest is using it. So the collector's
// job is stated there in a form that is not the obvious one:
//
//	The collector's job is to prevent memory.grow, not to free bytes.
//
// Everything about this allocator follows from that sentence. Allocation is
// free-list-first and bump-second, because a bump pointer that walks past the
// end of the heap grows it permanently and no later collection gives that
// back. Size classes are not an optimisation, they are the mechanism that keeps
// a fragmenting heap from climbing the ladder. A span that sweeps completely
// empty is returned to the span allocator rather than hoarded by its class, for
// the same reason.
//
// # What it is, in one paragraph
//
// A conservative, non-moving, stop-the-world mark-sweep collector over
// size-class segregated free lists. Conservative because TinyGo passes `layout`
// to `alloc` and then ignores it under `gc.custom`, so the application has no
// map of which words in a block are pointers -- see agents/gc.md section 3;
// that is forced rather than chosen, and it is why nothing moves. The heap is
// cut into 4 KiB spans, each span serves one size class, and every piece of
// collector metadata lives in one statically reserved .bss struct that is
// excluded from root scanning.
//
// # Stop-the-world, for now
//
// Collection here is a single synchronous mark and sweep. It is intended to be
// driven from a safe point BETWEEN guest calls -- Collect, or the pressure-gated
// CollectIfNeeded -- and not from inside an allocation. That is deliberate:
// TinyGo's reference `-gc=conservative` collects on allocation failure, i.e. at
// an arbitrary point inside an event handler, which agents/gc.md measures at
// about 10 ms for a 63 KB heap. Most of a frame, in a lockstep game. Breaking
// that pause into bounded steps is stage C; this package is the correct
// collector that stage C incrementalises.
//
// Allocation therefore never collects. When a size class and the span allocator
// are both empty, it grows the heap -- and because growing is the thing the
// collector exists to prevent, a guest that never calls Collect gets a slightly
// slower `-gc=leaking` and nothing else.
//
// # What a guest owes it
//
// One call, between events:
//
//	//go:wasmexport fk_on_tick
//	func onTick() {
//		fkgc.CollectIfNeeded()
//	}
//
// # What it does not do
//
//   - Finalizers. SetFinalizer is accepted and ignored; a finalizer needs a
//     precise notion of death this collector does not have.
//   - Compaction. A conservative collector cannot move what it cannot prove is
//     a pointer.
//   - wasip1. The root discovery for a parked goroutine is argued in
//     agents/gc.md section 1 and is untested under a collector; `fklua compile`
//     refuses the combination.
package fkgc
