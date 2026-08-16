package analysis

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// LoopGuard is a loop whose i32 memory accesses can all be proved in range and
// 4-aligned by ONE test evaluated when the loop is entered.
//
// This is where the time in a real guest's hot loop actually goes. Measured by
// hand-editing the TinyGo `pure_sum` loop and timing it interleaved under
// lua52f against a 0.5% A/A floor: hoisting the alignment test alone is 0.934x,
// hoisting the bounds check alone is 0.773x, and hoisting both is **0.637x**.
// For comparison the loop construct itself -- label, increment, compare, goto,
// which the counted-loop pass replaces with one FORLOOP -- is worth 0.950x on
// the same loop. Per-access memory overhead outweighs loop overhead by about
// seven to one, which is the opposite of what the M0 kernels suggest.
//
// # The guard is a RUNTIME test, and that is the whole design
//
// An earlier plan for this needed a static congruence proof that the counter
// hits its bound exactly, because the hot loop is 4x-unrolled and tested with
// `i32.ne`. That proof is hard: it wants the residue of a value the congruence
// analysis does not publish, and the direction fact lives on a local the range
// analysis cannot connect to the bound.
//
// None of it is necessary. The guard runs once, at loop entry, where every
// quantity it needs is a live Lua value: the counter and the bound are in
// scope, so divisibility, direction, alignment and the whole address span are
// just arithmetic. What has to be proved STATICALLY is only the loop's SHAPE --
// that the body runs a predictable number of times and that each address
// advances by a fixed amount -- and that is a structural question with no
// dataflow in it at all.
type LoopGuard struct {
	// Header is the OpLoop step. The guard is emitted immediately before that
	// step's label, so the back edge jumps past it and it runs exactly once per
	// entry to the loop.
	Header int

	// Ctr is the local the exit test advances, Step how far per iteration.
	Ctr  uint32
	Step int64
	// Bound is the value the counter is tested against: a local, or a constant
	// when BoundIsConst.
	Bound        uint32
	BoundConst   int64
	BoundIsConst bool
	// ExactTrips reports an `i32.ne` test, whose trip count is exact only when
	// the bound is reachable by whole steps -- a divisibility the guard checks.
	// The alternative is an ordered test, which needs Step 1 to give a trip
	// count without a rounding term.
	ExactTrips bool

	// Bases are the locals the guarded addresses hang off, in a deterministic
	// order. A loop reading two arrays in step -- a dot product, a zip, a blend
	// -- has two, and the guard is their conjunction.
	Bases []GuardBase
	// Steps maps each guarded access step to which base it uses and where.
	Steps map[int]GuardAccess
	// HasStore reports a guarded STORE, which is what lets the guard widen the
	// dirty-page range once for the whole span instead of once per store.
	HasStore bool
}

// GuardBase is one address base and everything the guard must prove about it.
type GuardBase struct {
	// Local is the base itself, and its value at loop entry is what the guard
	// tests -- at loop entry the local still holds it.
	//
	// For an AFFINE base it is the induction variable instead, because the base
	// local has no meaningful entry value: the loop rewrites it from scratch
	// every iteration, so before the first one it holds whatever the last
	// unrelated use left there. Nothing names it; the guard reconstructs the
	// base from AffineIV and AffineInv, and everything else here walks with the
	// induction variable. That is why substituting one for the other is a
	// no-op rather than a bug.
	Local uint32
	// Stride is how far the base advances per iteration; zero when it is
	// loop-invariant.
	Stride int64
	// MaxEnd is the far end of this base's span from the base: the largest
	// (offset + width) over its accesses. Kept as one number because a loop may
	// mix widths off the same base and only the furthest reach matters.
	MaxEnd int64
	// Inc is the step after which the word index advances -- the base's own
	// increment, or for an AFFINE base the increment of the induction variable
	// it is derived from. -1 when the base is loop-invariant.
	Inc int

	// Affine describes a base recomputed each iteration as `IV + Invariant`
	// rather than advanced in place. rustc and TinyGo both emit it for an
	// indexed array: the index walks and the array's start does not, so the
	// address is their sum. Its stride is the induction variable's, and its
	// value at loop entry is IV + Invariant with both still holding their entry
	// values -- which is exactly where the guard reads them.
	Affine    bool
	AffineIV  uint32
	AffineInv uint32
}

// GuardAccess is one specialised access: which base, how far past it, how wide.
type GuardAccess struct {
	Base  int // index into Bases
	Off   int64
	Width int64
}

// maxGuardOffset caps a guarded access's constant offset.
//
// An i32.const operand is UNSIGNED under Invariant A, so `p + -4` arrives as
// `p + 4294967292` and would make the span calculation nonsense. Refusing
// anything large keeps the arithmetic to numbers a struct or an unrolled body
// actually produces.
const maxGuardOffset = 1 << 16

// LoopGuards finds every loop in a function whose accesses one entry test can
// cover.
//
// The structural requirements are blunt on purpose. This removes a BOUNDS
// CHECK, so a loop admitted here on a wrong premise does not compute a wrong
// answer -- it reads or writes outside the guest's memory, which in a word
// table means a nil arriving somewhere far away, or a silent write past the
// end. Everything below exists to make the trip count and the address stride
// facts about the shape rather than guesses:
//
//   - ONE branch to the header, and it closes the loop. A `continue` would skip
//     an increment, and then neither the trip count nor the stride holds.
//   - A STRAIGHT-LINE body -- no block, if, else, or any other branch. That is
//     what makes "the increment happens once per iteration" true by inspection
//     rather than by a dominance argument, and it is what the unrolled loops
//     this targets look like.
//   - The counter is written exactly once, by its own increment.
//   - The base is written exactly once by `base + c`, or not at all.
//   - Every guarded access hangs off the SAME base. A second base is expressible
//     -- the guard is a conjunction -- but nothing measured needs it yet.
//   - The stride and every offset are multiples of 4, checked statically, so the
//     only alignment fact left for runtime is the base's own.
func LoopGuards(f *ir.Func) map[int]*LoopGuard {
	if f == nil || f.Unsupported != nil {
		return nil
	}
	cfg := ir.BuildCFG(f)
	if !cfg.Complete {
		// A missing edge means every block answer below is an under-estimate,
		// and the whole point of them is to be exact.
		return nil
	}
	prev := gcfg
	gcfg = cfg
	defer func() { gcfg = prev }()

	out := map[int]*LoopGuard{}
	for i := range f.Steps {
		if f.Steps[i].Op != wasm.OpLoop {
			continue
		}
		if g := analyseGuard(f, i); g != nil {
			out[i] = g
		}
	}
	return out
}

// gcfg carries the CFG through the guard analysis so every definition lookup can
// be restricted to a single basic block.
//
// This is what makes a branchy body safe to admit. `defOf` is a backward LINEAR
// SCAN over step indices: in a straight-line body textual order IS execution
// order, so the nearest preceding writer of a slot is the definition, but in a
// branchy one it can sit in a sibling arm that never executed on the path
// reaching the use. Every guard fact goes through it -- the exit test, every
// increment, and every access ADDRESS -- so widening the body without this makes
// address computation unsound before dominance is even reached.
//
// Within one block textual order is execution order again, so a definition found
// in the SAME block as its use is the real one. A definition anywhere else is
// refused rather than trusted.
var gcfg *ir.CFG

func defOfB(f *ir.Func, i, k, h int) int {
	d := defOf(f, i, k, h)
	if d < 0 || gcfg == nil {
		return d
	}
	if gcfg.BlockOf[d] != gcfg.BlockOf[i] {
		return -1
	}
	return d
}

func analyseGuard(f *ir.Func, h int) *LoopGuard {
	end := loopEnd(f, h)
	if end < 0 || end-1 <= h {
		return nil
	}
	back := end - 1

	// One branch to this header, and it is the last step. Anything else and the
	// body is not a straight run that executes each increment once.
	label := f.Steps[h].Label
	for i := h + 1; i < end; i++ {
		for _, br := range branchesOf(f.Steps[i]) {
			if br.Label == label && i != back {
				return nil
			}
		}
	}
	bs := f.Steps[back]
	if bs.Op != wasm.OpBrIf || bs.Target.Label != label || bs.Target.From != ir.NoSlot {
		return nil
	}
	// The body may branch. What must be true instead is that every increment
	// runs exactly once per completed iteration, and that is a property of the
	// LATCH -- the block the back edge leaves from.
	//
	// The latch executes exactly once per completed iteration by construction:
	// the only way back to the header is the back edge, there is exactly one of
	// those, and it is in the latch. So a write in the latch happens once an
	// iteration. Accesses in earlier blocks happen at MOST once, which is fine --
	// the span covers an access that happens, not one that must.
	//
	// Two things still have to go. A nested LOOP would let the latch run many
	// times per outer iteration while `writesOf` still reported a single write,
	// and memory.grow would move MEMSIZE under a span already proved.
	for i := h + 1; i < back; i++ {
		switch f.Steps[i].Op {
		case wasm.OpLoop, wasm.OpBrTable, wasm.OpMemoryGrow:
			return nil
		}
	}
	if !gcfg.Complete {
		return nil
	}
	latch := gcfg.BlockOf[back]

	g := &LoopGuard{Header: h, Steps: map[int]GuardAccess{}}

	// The exit test, in the three shapes real toolchains produce:
	//
	//	br_if $top (i32.ne (local.tee $i ...) (local.get $n))   TinyGo, counting up
	//	br_if $top (i32.lt_u (local.tee $i ...) (local.get $n))
	//	br_if $top (local.tee $i (i32.sub (local.get $i) 8))    rustc, counting down
	//
	// The third has no comparison step at all: a br_if continues while its
	// operand is non-zero, which IS `!= 0`, and the counter reaches the branch
	// straight out of its own local.tee. That is how rustc closes an unrolled
	// loop, and not recognising it is the only reason this pass reached TinyGo's
	// pure_sum and not Rust's.
	cmp := defOfB(f, back, 0, h)
	if cmp < 0 {
		return nil
	}
	tee, boundArg := -1, -1
	switch f.Steps[cmp].Op {
	case wasm.OpI32Ne, wasm.OpI32LtU, wasm.OpI32LtS:
		g.ExactTrips = f.Steps[cmp].Op == wasm.OpI32Ne
		// The counter is whichever side is a local.tee; the other is the bound.
		for k := 0; k < 2; k++ {
			d := defOfB(f, cmp, k, h)
			if d >= 0 && f.Steps[d].Op == wasm.OpLocalTee {
				tee, boundArg = d, 1-k
				break
			}
		}
	case wasm.OpLocalTee:
		// A bare value as the condition: the loop runs while it is non-zero, so
		// the bound is an implicit zero that no step holds.
		g.ExactTrips = true
		tee, boundArg = cmp, -1
		g.BoundIsConst, g.BoundConst = true, 0
	default:
		return nil
	}
	if tee < 0 {
		return nil
	}
	g.Ctr = f.Steps[tee].Instr.LocalIndex
	if f.LocalType(g.Ctr) != wasm.I32 {
		return nil
	}
	// Step may be negative: rustc counts DOWN to zero, TinyGo counts up. Which
	// direction the difference is taken in follows from the sign, and the guard
	// prints it accordingly.
	step, ok := incrementOf(f, tee, g.Ctr, h)
	if !ok || step == 0 {
		return nil
	}
	g.Step = step
	// An ordered test gives an exact trip count only at a step of one; anything
	// wider needs a rounding term the guard would have to compute. A countdown
	// is always tested for equality with zero, so it is never in this case.
	if !g.ExactTrips && g.Step != 1 {
		return nil
	}
	if g.Step < 0 && !g.ExactTrips {
		return nil
	}
	if writesLocalExcept(f, h+1, end, g.Ctr, tee) {
		return nil
	}
	// The counter's own write needs no latch check: it is IMPLIED. The exit test
	// is resolved with defOfB from the back edge, which already requires the
	// same block, and the tee is resolved from the test the same way -- so both
	// are in the latch by construction. An explicit check here would be dead
	// code that reads as load-bearing, which is worse than none: a later change
	// that made it reachable would find it already "tested".
	_ = latch

	// The bound: a loop-invariant local, or a constant. The bare-value shape has
	// already supplied its implicit zero.
	if boundArg < 0 {
		return finishGuard(f, g, h, end, back, latch)
	}
	bd := defOfB(f, cmp, boundArg, h)
	switch {
	case bd < 0:
		// Computed before the loop; invariant provided the slot it sits in is
		// not rewritten inside.
		return nil
	case f.Steps[bd].Op == wasm.OpLocalGet:
		g.Bound = f.Steps[bd].Instr.LocalIndex
		if writesLocal(f, h+1, end, g.Bound) {
			return nil
		}
	case f.Steps[bd].Op == wasm.OpI32Const:
		g.BoundIsConst = true
		g.BoundConst = int64(f.Steps[bd].Instr.I32)
	default:
		return nil
	}

	return finishGuard(f, g, h, end, back, latch)
}

// baseKey identifies an address base up to how it happens to be spelled.
//
// The same base arrives in two syntactic shapes in real output, and they have to
// be recognised as one thing or the guard proves a span for half a loop's
// accesses and leaves the rest uncovered. In `pure_dot`:
//
//	v16 = (v1 + v9) % 2^32   v2 = v16   t0 = v16      the first access
//	t0 = ((v2 + 8) % 2^32)                            every later one
//
// The first reads the SUM straight out of the peephole; the later ones read the
// local the sum was stored into. Canonicalising both to the pair (IV, invariant)
// makes them one base.
type baseKey struct {
	local   uint32
	iv, inv uint32
	affine  bool
}

// canonBase resolves a base local: if the loop assigns it once as `IV +
// invariant`, the pair is its identity; otherwise it is itself.
func canonBase(f *ir.Func, local uint32, h, end int) baseKey {
	if sets := writesOf(f, h+1, end, local); len(sets) == 1 {
		if _, ok := incrementOf(f, sets[0], local, h); !ok {
			// Not advanced in place: the loop rebuilds it from an
			// induction variable and something invariant.
			if add := defOfB(f, sets[0], 0, h); add >= 0 && f.Steps[add].Op == wasm.OpI32Add {
				x, y := defOfB(f, add, 0, h), defOfB(f, add, 1, h)
				if x >= 0 && y >= 0 &&
					f.Steps[x].Op == wasm.OpLocalGet && f.Steps[y].Op == wasm.OpLocalGet {
					if iv, inv, ok := affinePair(f, f.Steps[x].Instr.LocalIndex,
						f.Steps[y].Instr.LocalIndex, h, end); ok {
						return baseKey{iv: iv, inv: inv, affine: true}
					}
				}
			}
		}
	}
	return baseKey{local: local}
}

// resolveBase describes a memory access's address as a canonical base plus a
// constant byte offset.
func resolveBase(f *ir.Func, i, h, end int) (baseKey, int64, bool) {
	d := defOfB(f, i, 0, h)
	if d < 0 {
		return baseKey{}, 0, false
	}
	switch f.Steps[d].Op {
	case wasm.OpLocalGet, wasm.OpLocalTee:
		// A tee is how the peephole spells "store it and keep using it", which
		// is exactly what a loop does with a base it is about to read through:
		// `v2 = v16` and the access on the same value. Either way the address is
		// the local's current value.
		return canonBase(f, f.Steps[d].Instr.LocalIndex, h, end), 0, true
	case wasm.OpI32Add:
		x, y := defOfB(f, d, 0, h), defOfB(f, d, 1, h)
		if x < 0 || y < 0 {
			return baseKey{}, 0, false
		}
		xs, ys := f.Steps[x], f.Steps[y]
		if xs.Op == wasm.OpLocalGet && ys.Op == wasm.OpI32Const {
			return canonBase(f, xs.Instr.LocalIndex, h, end), int64(ys.Instr.I32), true
		}
		if ys.Op == wasm.OpLocalGet && xs.Op == wasm.OpI32Const {
			return canonBase(f, ys.Instr.LocalIndex, h, end), int64(xs.Instr.I32), true
		}
		// A sum of two locals IS the affine base, read before it was stored.
		if xs.Op == wasm.OpLocalGet && ys.Op == wasm.OpLocalGet {
			if iv, inv, ok := affinePair(f, xs.Instr.LocalIndex, ys.Instr.LocalIndex, h, end); ok {
				return baseKey{iv: iv, inv: inv, affine: true}, 0, true
			}
		}
	}
	return baseKey{}, 0, false
}

// affinePair decides which of two summed locals walks and which does not.
func affinePair(f *ir.Func, a, bb uint32, h, end int) (iv, inv uint32, ok bool) {
	aw := len(writesOf(f, h+1, end, a)) > 0
	bw := len(writesOf(f, h+1, end, bb)) > 0
	switch {
	case aw && !bw:
		return a, bb, true
	case bw && !aw:
		return bb, a, true
	}
	return 0, 0, false
}

// finishGuard collects the accesses and the bases they hang off, which is the
// same work whatever shape the exit test took.
func finishGuard(f *ir.Func, g *LoopGuard, h, end, back, latch int) *LoopGuard {
	maxEnd := map[baseKey]int64{}
	var order []baseKey
	at := map[int]baseKey{}

	for i := h + 1; i < back; i++ {
		s := f.Steps[i]
		w, ok := accessWidth(s.Op)
		if !ok {
			continue
		}
		key, off, ok := resolveBase(f, i, h, end)
		if !ok {
			// SKIPPED, not refused, and the difference is what admits a real
			// loop. An access this pass cannot describe simply stays out of
			// Steps: the emitter then gives it its own full bounds check and its
			// own watermark update, exactly as if no guard existed. The guard
			// only ever claims something about the accesses it specialises.
			//
			// `real_entities` is the case that needs it. Its hot loop reads and
			// writes `totals[kind]` through an address computed from a loaded
			// byte -- undescribable -- alongside two ordinary reads off the
			// entity pointer. Refusing the loop for the former threw away the
			// latter, which is where the time is.
			continue
		}
		off += int64(s.Instr.MemOffset)
		// Alignment is settled statically for the offset and the stride, so the
		// only fact left for runtime is the base's own. A 4-multiple is enough
		// for both widths: the inlined 8-byte access gates on `t0 % 4 == 0`,
		// because it reads its two words separately.
		if off < 0 || off >= maxGuardOffset || off%4 != 0 {
			continue
		}
		if _, known := maxEnd[key]; !known {
			if len(order) >= maxGuardBases {
				continue
			}
			order = append(order, key)
		}
		if off+w > maxEnd[key] {
			maxEnd[key] = off + w
		}
		if isStore(s.Op) {
			g.HasStore = true
		}
		at[i] = key
		g.Steps[i] = GuardAccess{Off: off, Width: w}
	}
	if len(order) == 0 {
		return nil
	}

	// A base whose walk this pass cannot describe is DROPPED, along with its
	// accesses -- the same principle as an undescribable address. Refusing the
	// whole loop for one bad base throws away every good one, and
	// `real_entities` is exactly that shape: two ordinary reads off the entity
	// pointer beside a `totals[kind]` pair whose base is reassigned twice.
	kept := make([]baseKey, 0, len(order))
	for _, key := range order {
		b := GuardBase{MaxEnd: maxEnd[key], Inc: -1}
		var walker uint32 // the local whose increment moves this base
		if key.affine {
			b.Affine, b.AffineIV, b.AffineInv = true, key.iv, key.inv
			b.Local, walker = key.iv, key.iv
			if f.LocalType(key.iv) != wasm.I32 || f.LocalType(key.inv) != wasm.I32 {
				continue
			}
			if writesLocal(f, h+1, end, key.inv) {
				continue
			}
		} else {
			b.Local, walker = key.local, key.local
			if f.LocalType(key.local) != wasm.I32 {
				continue
			}
		}
		// The counter doubling as a base ITSELF is refused: nothing measured
		// needs it and the arithmetic is not obviously the same.
		//
		// A base DERIVED from the counter is a different thing and is allowed.
		// rustc indexes both arrays off the loop counter -- `base = counter +
		// arrayStart` -- so the base's stride simply IS the counter's, which is
		// exactly the affine case and involves no second stride at all.
		if walker == g.Ctr && !key.affine {
			continue
		}
		sets := writesOf(f, h+1, end, walker)
		if len(sets) > 1 {
			continue // no single stride
		}
		if len(sets) == 1 {
			c, ok := incrementOf(f, sets[0], walker, h)
			if !ok || c < 0 || c%4 != 0 {
				continue
			}
			// In the latch, so it runs exactly once per completed iteration.
			// Anywhere else and the base advances a number of times the span
			// cannot predict.
			if gcfg.BlockOf[sets[0]] != latch {
				continue
			}
			b.Stride, b.Inc = c, sets[0]
		}
		g.Bases = append(g.Bases, b)
		kept = append(kept, key)
	}
	if len(g.Bases) == 0 {
		return nil
	}
	// Drop the accesses whose base did not survive; they keep their own checks.
	for i := range g.Steps {
		found := false
		for _, key := range kept {
			if key == at[i] {
				found = true
			}
		}
		if !found {
			delete(g.Steps, i)
		}
	}
	if len(g.Steps) == 0 {
		return nil
	}

	// Point each access at its base, now that the list is fixed.
	for i, a := range g.Steps {
		idx := -1
		for k, key := range kept {
			if key == at[i] {
				idx = k
			}
		}
		if idx < 0 {
			return nil
		}
		a.Base = idx
		g.Steps[i] = a
	}

	// Every access must come BEFORE its base's word index advances, or its
	// addresses are one stride further along than the span accounts for. Both
	// real guests read through the pointer and then bump it, which is the shape
	// this describes.
	for i, a := range g.Steps {
		if inc := g.Bases[a.Base].Inc; inc >= 0 && i > inc {
			return nil
		}
	}
	return g
}

// maxGuardBases caps the conjunction. Each base costs a chunk of guard
// arithmetic and one word-index local, and two is what a loop reading two
// arrays in step needs; nothing measured wants more.
const maxGuardBases = 3

// accessWidth reports the memory operations this pass covers, and how many
// bytes each touches.
//
// The 8-byte accesses are in because their inlined form gates on 4-alignment,
// not 8 -- it reads the two words separately -- so one guard covers both widths
// with the same runtime test. The sub-word ones are out: they are never
// alignment-limited, and their inlined form is already a direct table read.
func accessWidth(op wasm.Op) (int64, bool) {
	switch op {
	case wasm.OpI32Load, wasm.OpI32Store:
		return 4, true
	case wasm.OpF64Load, wasm.OpI64Load:
		return 8, true
	}
	return 0, false
}

func isStore(op wasm.Op) bool {
	return op == wasm.OpI32Store
}

// addressOf describes a memory access's address operand as a local plus a
// constant, which is the only shape whose span over a loop this pass can
// compute.
func addressOf(f *ir.Func, i, h int) (local uint32, off int64, ok bool) {
	d := defOfB(f, i, 0, h)
	if d < 0 {
		return 0, 0, false
	}
	switch f.Steps[d].Op {
	case wasm.OpLocalGet:
		return f.Steps[d].Instr.LocalIndex, 0, true
	case wasm.OpI32Add:
		a := defOfB(f, d, 0, h)
		b := defOfB(f, d, 1, h)
		if a < 0 || b < 0 {
			return 0, 0, false
		}
		if f.Steps[a].Op == wasm.OpLocalGet && f.Steps[b].Op == wasm.OpI32Const {
			return f.Steps[a].Instr.LocalIndex, int64(f.Steps[b].Instr.I32), true
		}
		if f.Steps[b].Op == wasm.OpLocalGet && f.Steps[a].Op == wasm.OpI32Const {
			return f.Steps[b].Instr.LocalIndex, int64(f.Steps[a].Instr.I32), true
		}
	}
	return 0, 0, false
}

// incrementOf reads `local = local +/- constant` written by the set/tee at
// step w, returning the signed per-iteration advance.
func incrementOf(f *ir.Func, w int, local uint32, h int) (int64, bool) {
	add := defOfB(f, w, 0, h)
	if add < 0 {
		return 0, false
	}
	var dir int64
	switch f.Steps[add].Op {
	case wasm.OpI32Add:
		dir = 1
	case wasm.OpI32Sub:
		dir = -1
	default:
		return 0, false
	}
	src := defOfB(f, add, 0, h)
	if src < 0 || f.Steps[src].Op != wasm.OpLocalGet ||
		f.Steps[src].Instr.LocalIndex != local {
		return 0, false
	}
	k := defOfB(f, add, 1, h)
	if k < 0 || f.Steps[k].Op != wasm.OpI32Const {
		return 0, false
	}
	v := int64(f.Steps[k].Instr.I32)
	if v >= 1<<31 {
		v -= 1 << 32 // a negative constant, as a signed step
	}
	return dir * v, true
}

// writesOf lists the steps in [lo, hi) that write a local.
func writesOf(f *ir.Func, lo, hi int, idx uint32) []int {
	var out []int
	for i := lo; i < hi; i++ {
		s := f.Steps[i]
		if (s.Op == wasm.OpLocalSet || s.Op == wasm.OpLocalTee) && s.Instr.LocalIndex == idx {
			out = append(out, i)
		}
	}
	return out
}

func writesLocalExcept(f *ir.Func, lo, hi int, idx uint32, except int) bool {
	for _, w := range writesOf(f, lo, hi, idx) {
		if w != except {
			return true
		}
	}
	return false
}

// GuardedAccessOffset reports where a guarded access sits: which base, how far
// past it in BYTES, and how wide.
//
// It reads what the recogniser already decided rather than re-deriving it. An
// earlier version re-derived, on the theory that two implementations agreeing is
// a check -- but they cannot disagree usefully here: a mismatch is a wrong table
// index, not a refused loop, so the only safe arrangement is one source.
func GuardedAccessOffset(g *LoopGuard, step int) (GuardAccess, bool) {
	a, ok := g.Steps[step]
	return a, ok
}
