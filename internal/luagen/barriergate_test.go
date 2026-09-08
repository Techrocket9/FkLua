package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// THE DIRTY-PAGE BARRIER IS EMITTED ONLY IN A MODE THAT CAN ARM IT.
//
// `MEMDIRTY` is `local MEMDIRTY = false` in fk_rt.lua and is turned on from
// MEMPACK.arm and MEMPACK.gc_arm, which the chunk publishes as `persist.arm`
// only under --persist=packed and `persist.gc.arm` only under --gc=collected.
// Under every other combination the flag is false for the life of the chunk and
// unreachable from the host, so each emitted
//
//	if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end
//
// is a test that can only be false, at 78 bytes a copy and two copies per store.
//
// BOTH HALVES ARE HERE because they fail in opposite directions and neither one
// alone is the property. The absence half is a size claim -- 9.7% of one shipped
// data module and 15.0% of the other. The presence half is the correctness rule
// TestEveryStoreShapeMarksItsPageUnderTheCollector enforces: a mode that CAN arm
// must still mark every store, and a gate that got its condition backwards would
// pass the first half while silently dropping a live object into the sweep.
//
// It is a TEXT property or it is nothing: nothing a mode without an `arm` can run
// observes the difference, which is exactly why the line could sit there unread.
func TestTheDirtyPageBarrierIsEmittedOnlyWhereItCanBeArmed(t *testing.T) {
	// Three routes to the barrier, one per emitting site: the general inlined
	// store's two arms, the static-fold arm (a constant address), and the loop
	// guard's hoisted mark over a proven span with its guarded store under it.
	//
	// THE LOOP IS THE ONE THE GUARD PROVES, and the count is what says so. The
	// first version of this fixture walked a loop the guard planner declined,
	// so its store went out through the general route and the "third route"
	// was never in the text; a Contains on MEMDIRTY was satisfied by the two
	// arms that were, and deleting the hoisted mark left this test green (the
	// whole-diff review of 2026-09-08 did exactly that; only
	// TestAGuardedStoreReachesTheSave noticed). `fill` is the shape
	// loopguard_test.go's `walk` fixture uses, and in a marking mode at -opt=3
	// its body carries THREE barriers: the guarded store's two arms and the
	// hoisted `if lg0 and MEMDIRTY then` over the whole span. Two would mean
	// the guard did not fire, or the hoist is gone; either is a different
	// program from the one this test is about.
	const wat = `(module (memory 1)
		(func (export "poke") (param i32 i32)
			(i32.store (local.get 0) (local.get 1)))
		(func (export "fold") (param i32)
			(i32.store (i32.const 64) (local.get 0)))
		(func (export "fill") (param $p i32) (param $bound i32) (param $v i32)
			(local $i i32)
			(loop $top
				(i32.store (local.get $p) (local.get $v))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (i32.ne
					(local.tee $i (i32.add (local.get $i) (i32.const 1)))
					(local.get $bound))))))`
	// How many MEMDIRTY tests each route emits in a marking mode at -opt=3.
	routes := []struct {
		name  string
		marks int
	}{{"poke", 2}, {"fold", 1}, {"fill", 3}}

	for _, tc := range []struct {
		p    PersistMode
		g    GCMode
		arms bool
	}{
		// Nothing here publishes an arm, so nothing here may emit a mark.
		// PersistAuto is in the list because `fklua compile` passes it to the
		// emitter unresolved and luagen.go tests `== PersistPacked`, so the
		// chunk publishes no arm for it either -- the gate has to read false
		// for exactly the same reason.
		{PersistTable, GCLeaking, false},
		{PersistNone, GCLeaking, false},
		{PersistAuto, GCLeaking, false},
		// Each of these publishes one, so each still marks every store.
		{PersistPacked, GCLeaking, true},
		{PersistTable, GCCollected, true},
		{PersistNone, GCCollected, true},
		{PersistPacked, GCCollected, true},
	} {
		for _, lvl := range allLevels {
			// The barrier is inline text, and nothing is inlined below -opt=3:
			// under it every store goes through st32, which tests MEMDIRTY in
			// the prelude where the mode cannot reach it.
			marking := tc.arms && lvl >= analysis.O3
			src := emitGC(t, wat, lvl, tc.p, tc.g)
			for _, r := range routes {
				want := 0
				if marking {
					want = r.marks
				}
				body := functionBody(src, r.name)
				if got := strings.Count(body, "MEMDIRTY"); got != want {
					why := "the mode publishes an arm, so every store marks its page " +
						"and the hoisted mark rides the guard; a missing copy is a " +
						"write the save or the sweep cannot see"
					if !marking {
						why = "nothing in this mode can ever arm MEMDIRTY, so the " +
							"test can only be false and the line is dead weight"
					}
					t.Errorf("-opt=%s --persist=%s --gc=%s: %q carries %d MEMDIRTY test(s), want %d -- %s\n%s",
						lvl, tc.p, tc.g, r.name, got, want, why, body)
				}
				// The hoisted form is its own shape, so a count that happened
				// to come out right for the wrong reason cannot pass here.
				if r.name == "fill" {
					if got := strings.Contains(body, "and MEMDIRTY then"); got != marking {
						t.Errorf("-opt=%s --persist=%s --gc=%s: the loop guard's hoisted "+
							"mark is present=%v in %q, want %v\n%s",
							lvl, tc.p, tc.g, got, r.name, marking, body)
					}
				}
			}
		}
	}
}
