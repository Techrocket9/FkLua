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
	const wat = `(module (memory 1)
		(func (export "poke") (param i32 i32)
			(i32.store (local.get 0) (local.get 1)))
		(func (export "fold") (param i32)
			(i32.store (i32.const 64) (local.get 0)))
		(func (export "walk") (param i32 i32)
			(local i32)
			(block $b (loop $l
				(br_if $b (i32.ge_u (local.get 2) (local.get 1)))
				(i32.store (i32.add (local.get 0) (i32.shl (local.get 2) (i32.const 2)))
					(local.get 2))
				(local.set 2 (i32.add (local.get 2) (i32.const 1)))
				(br $l)))))`
	exports := []string{"poke", "fold", "walk"}

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
			want := tc.arms && lvl >= analysis.O3
			src := emitGC(t, wat, lvl, tc.p, tc.g)
			for _, name := range exports {
				body := functionBody(src, name)
				if got := strings.Contains(body, "MEMDIRTY"); got != want {
					verb := "emitted no barrier"
					why := "the mode publishes an arm, so a store that does not " +
						"mark its page is a write the save or the sweep cannot see"
					if got {
						verb = "emitted a barrier"
						why = "nothing in this mode can ever arm MEMDIRTY, so the " +
							"test can only be false and the line is dead weight"
					}
					t.Errorf("-opt=%s --persist=%s --gc=%s: %s %s in %q; want emitted=%v -- %s\n%s",
						lvl, tc.p, tc.g, name, verb, name, want, why, body)
				}
			}
		}
	}
}
