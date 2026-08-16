package analysis

import (
	"sort"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Range is an inclusive interval over the Lua NUMBER a slot actually holds.
//
// That is not the same thing as the wasm value's range, and the difference is
// the whole point of the pass. When a wrap is deferred, the slot holds some
// value congruent to the wasm value modulo 2^32 rather than the wasm value
// itself, and the interval is how the consumer finds out.
//
// Signed because a deferred i32.sub can leave a negative number in the slot;
// Lua's % is floored, so `(a - b) % 2^32` recovers the right answer from one.
type Range struct{ Lo, Hi int64 }

const (
	u32Max = int64(1)<<32 - 1
	// signBit is where the signed and unsigned orders part company. A value at
	// or above it is negative as a signed i32, so a range entirely below it is
	// one whose signed compares can use Lua's own operators directly.
	signBit = int64(1) << 31
	// safeMag is the magnitude a deferred value may not exceed.
	//
	// A double holds integers exactly to 2^53. Every consumer of a deferred
	// value does at least one more add before wrapping, and the analysis has to
	// stay exact through that, so the budget stops well short: 2^48 leaves 32
	// doublings of headroom and makes an off-by-a-few-bits mistake in a
	// transfer function harmless rather than a silent miscompile.
	safeMag = int64(1) << 48
)

// FullU32 is what an i32 is worth when nothing better is known.
var FullU32 = Range{0, u32Max}

// Exact reports a range pinned to a single value, and returns it.
func (r Range) Exact() (int64, bool) { return r.Lo, r.Lo == r.Hi }

// ConstU32 reports a range pinned to a single value that is a genuine i32.
//
// The u32 guard is not paperwork: a deferred value has an exact range too when
// its inputs were exact, and that range can be negative or past 2^32. Anything
// that wants to PRINT a constant into generated Lua has to go through here.
func (r Range) ConstU32() (uint32, bool) {
	v, ok := r.Exact()
	if !ok || v < 0 || v > u32Max {
		return 0, false
	}
	return uint32(v), true
}

// FitsU32 reports a range entirely inside [0, 2^32), so the value in the slot
// IS the wasm value and no consumer has to know about deferral.
func (r Range) FitsU32() bool { return r.Lo >= 0 && r.Hi <= u32Max }

// Below reports that every value in the range is strictly below n.
func (r Range) Below(n int64) bool { return r.Lo >= 0 && r.Hi < n }

// empty reports an interval with no members, which is how an infeasible edge
// arrives out of the guard refinement.
func (r Range) empty() bool { return r.Lo > r.Hi }

// join is the least interval containing both, which is what a merge point needs.
func (r Range) join(o Range) Range {
	if o.Lo < r.Lo {
		r.Lo = o.Lo
	}
	if o.Hi > r.Hi {
		r.Hi = o.Hi
	}
	return r
}

// meet is the intersection, which is what a guard that held tells us.
func (r Range) meet(o Range) Range {
	if o.Lo > r.Lo {
		r.Lo = o.Lo
	}
	if o.Hi < r.Hi {
		r.Hi = o.Hi
	}
	return r
}

// safe reports that the range stays inside the exact-arithmetic budget.
func (r Range) safe() bool {
	return r.Lo > -safeMag && r.Hi < safeMag
}

// Wrap is the result of the range analysis for one function.
type Wrap struct {
	// Arg[i][k] is the range of operand k of step i.
	Arg [][]Range
	// Result[i] is the range of the value step i leaves in its destination.
	Result []Range
	// Elide[i] reports that step i may skip its `% 2^32`.
	Elide []bool
}

// ArgRange is Arg[i][k], guarded so the emitter can ask about an operand that
// does not exist.
func (w *Wrap) ArgRange(i, k int) Range {
	if w == nil || i >= len(w.Arg) || k >= len(w.Arg[i]) {
		return FullU32
	}
	return w.Arg[i][k]
}

// Elided reports Elide[i], guarded the same way, so a nil analysis means "the
// optimizer is off" rather than a crash.
func (w *Wrap) Elided(i int) bool {
	if w == nil || i >= len(w.Elide) {
		return false
	}
	return w.Elide[i]
}

// Ranges runs the range and wrap analysis over one function.
//
// Ranges of OPERAND-STACK slots are block-local and always were: a stack slot
// is written once and read once, deferral is only ever offered to a consumer in
// the same block, and a value that crosses a boundary can be reached from
// anywhere. Ranges of wasm LOCALS are not: they are solved to a fixpoint over
// the CFG, because a loop counter's range at the loop head is exactly the fact
// a signed compare wants and exactly the fact a block-local pass throws away.
//
// Three things make that fixpoint terminate and be worth having:
//
//   - join at every merge, so a block entered from several places gets the
//     union of what its predecessors knew;
//   - widening at the target of every retreating edge, jumping a bound that
//     grew to the next THRESHOLD rather than straight to the top, so a counted
//     loop settles on the bound the program actually has;
//   - narrowing on the edges out of a conditional branch, so the loop guard --
//     `i < n` -- is a fact inside the loop body rather than a discarded
//     comparison. Without this the widening has nothing to converge onto and
//     every counter lands at the full i32 range.
//
// It runs in two passes, because deciding whether to defer step i's wrap needs
// to know something about step j that comes LATER: whether its lowering masks.
// The first pass runs with deferral switched off and exists only to populate
// the operand ranges the second pass reads ahead into. Nothing about which
// operands are constants depends on deferral, so the lookahead is stable.
func Ranges(f *ir.Func) *Wrap {
	cons := consumers(f)
	cfg := ir.BuildCFG(f)
	return ranges(f, cfg, cons, ranges(f, cfg, cons, nil))
}

func ranges(f *ir.Func, cfg *ir.CFG, cons []int, ahead *Wrap) *Wrap {
	w := &Wrap{
		Arg:    make([][]Range, len(f.Steps)),
		Result: make([]Range, len(f.Steps)),
		Elide:  make([]bool, len(f.Steps)),
	}
	for i := range w.Result {
		w.Result[i] = FullU32
	}
	s := &solver{f: f, cfg: cfg, cons: cons, ahead: ahead, w: w, thr: thresholds(f)}
	s.solve()
	return w
}

// locals is the dataflow state: what is known about each wasm local.
//
// An ABSENT key is the full i32 range, never bottom. That is what makes the
// join at a merge point a plain intersection of key sets -- a local one
// predecessor knows nothing about is a local nothing is known about.
type locals map[uint32]Range

func (l locals) clone() locals {
	out := make(locals, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (l locals) join(o locals) locals {
	out := make(locals, len(l))
	for k, a := range l {
		if b, ok := o[k]; ok {
			out[k] = a.join(b)
		}
	}
	return out
}

func (l locals) same(o locals) bool {
	if len(l) != len(o) {
		return false
	}
	for k, a := range l {
		if b, ok := o[k]; !ok || a != b {
			return false
		}
	}
	return true
}

// tighten intersects what is known about a local with what a guard just proved.
// An empty intersection means the edge is infeasible; the range is left alone
// rather than modelled, because an unreachable block's answers are never read.
func (l locals) tighten(k uint32, r Range) {
	cur, ok := l[k]
	if !ok {
		cur = FullU32
	}
	next := cur.meet(r)
	if next.empty() {
		return
	}
	l[k] = next
}

const (
	// maxRounds caps the RPO sweeps. Widening already guarantees termination;
	// this is the belt to its braces, and hitting it drops to knowing nothing
	// rather than to whatever half-solved state the sweep was in.
	maxRounds = 60
	// maxWidenings caps how far up the threshold ladder one block may climb
	// before it goes straight to the top. Only a climb that actually loosened
	// something counts, and the ladder already bounds those per local, so this
	// is set where it cannot bite a real function: a budget small enough to fire
	// costs precision at the worst possible moment, in the innermost loop of the
	// most deeply nested function.
	maxWidenings = 512
	// maxThresholds caps the ladder itself. A function with thousands of
	// distinct constants would otherwise pay a sweep per rung.
	maxThresholds = 48
)

type solver struct {
	f     *ir.Func
	cfg   *ir.CFG
	cons  []int
	ahead *Wrap
	w     *Wrap
	thr   []int64

	in   []locals
	seen []bool
	// dirty[b] is set when b's entry state moved and it has not been re-run
	// since. It is the whole of the worklist: reverse postorder does the rest.
	dirty    []bool
	widened  []int
	overflow bool
}

func (s *solver) solve() {
	n := len(s.cfg.Blocks)
	if n == 0 {
		return
	}
	s.in = make([]locals, n)
	s.seen = make([]bool, n)
	s.dirty = make([]bool, n)
	s.widened = make([]int, n)

	// An incomplete graph means an edge is missing, so a join would be an
	// under-approximation -- the one failure mode that produces wrong code
	// rather than slow code. Fall back to knowing nothing, which is exactly the
	// block-local behaviour this pass replaced.
	if s.cfg.Complete {
		s.in[0] = entryLocals(s.f)
		s.seen[0], s.dirty[0] = true, true

		for round := 0; round < maxRounds && !s.overflow; round++ {
			if !s.sweep() {
				break
			}
			if round == maxRounds-1 {
				s.overflow = true
			}
		}
	}
	if !s.cfg.Complete || s.overflow {
		for b := range s.in {
			s.in[b] = locals{}
		}
	}

	// Record with the converged entry states. The sweeps above computed nothing
	// but `in`, so the answers the emitter reads are all written here, once,
	// from a state that no longer moves. A block the entry cannot reach is
	// recorded too, from the top, so every step has an answer.
	for b := range s.cfg.Blocks {
		entry := s.in[b]
		if entry == nil {
			entry = locals{}
		}
		s.runBlock(b, entry, true)
	}
}

// sweep runs one pass in reverse postorder and reports whether anything moved.
//
// Only blocks whose own entry state changed are re-run. Reverse postorder is
// what makes that cheap rather than merely correct: a block usually sees its
// final entry state on the first visit, so the dirty set collapses to the loops
// after one round, and re-running the whole function per round costs most of the
// compile time on a large module for nothing.
func (s *solver) sweep() bool {
	changed := false
	for _, b := range s.cfg.Order {
		if !s.seen[b] || !s.dirty[b] {
			continue
		}
		s.dirty[b] = false
		exit, g := s.runBlock(b, s.in[b], false)
		blk := &s.cfg.Blocks[b]
		for _, succ := range blk.Succs {
			out := exit
			// A conditional whose arms land in the same block has learned
			// nothing about it: control arrives whichever way the test went.
			if blk.Kind == ir.ExitCond && blk.True != blk.False {
				switch succ {
				case blk.True:
					out = narrowed(exit, g, true)
				case blk.False:
					out = narrowed(exit, g, false)
				}
			}
			if s.merge(succ, out) {
				changed = true
			}
		}
	}
	return changed
}

func (s *solver) merge(b int, st locals) bool {
	if !s.seen[b] {
		s.seen[b], s.dirty[b] = true, true
		s.in[b] = st.clone()
		return true
	}
	next := s.in[b].join(st)
	if s.cfg.Retreating[b] {
		next = s.widen(b, s.in[b], next)
	}
	if s.in[b].same(next) {
		return false
	}
	s.in[b] = next
	s.dirty[b] = true
	return true
}

// widen moves a bound that grew to the next threshold rather than letting it
// climb one iteration at a time.
//
// `next` is already the join of `old` with an incoming state, so it can only be
// wider; widening only ever loosens it further, which is what makes the result
// a post-fixpoint and therefore a sound over-approximation. The threshold
// ladder is finite and each widening moves strictly up it, so the iteration
// stops.
func (s *solver) widen(b int, old, next locals) locals {
	out := make(locals, len(next))
	climbed := false
	for k, nr := range next {
		or, ok := old[k]
		if !ok {
			continue // already the top
		}
		if nr.Lo < or.Lo {
			nr.Lo = s.rungBelow(nr.Lo)
			climbed = true
		}
		if nr.Hi > or.Hi {
			nr.Hi = s.rungAbove(nr.Hi)
			climbed = true
		}
		out[k] = nr
	}
	// Only a climb is counted. Charging the budget for every merge into a loop
	// header instead spends it on the header's OWN pre-header edge, once per
	// sweep, and a nested loop then runs out of budget while it is still making
	// progress -- which shows up as an inner counter that is somehow less well
	// known than an outer one.
	if climbed {
		s.widened[b]++
		if s.widened[b] > maxWidenings {
			return locals{}
		}
	}
	return out
}

// rungAbove is the smallest threshold at or above v.
func (s *solver) rungAbove(v int64) int64 {
	i := sort.Search(len(s.thr), func(i int) bool { return s.thr[i] >= v })
	if i == len(s.thr) {
		return u32Max
	}
	return s.thr[i]
}

// rungBelow is the largest threshold at or below v.
func (s *solver) rungBelow(v int64) int64 {
	i := sort.Search(len(s.thr), func(i int) bool { return s.thr[i] > v })
	if i == 0 {
		return 0
	}
	return s.thr[i-1]
}

// thresholds is the ladder widening climbs.
//
// The structural rungs are the two cliffs -- 2^31, above which a signed compare
// can no longer use Lua's own operator, and 2^32, above which a wrap can no
// longer be elided -- and the rung one BELOW each of them. That second rung is
// not padding. A counted loop's guard leaves `i <= bound-1` and the increment
// puts the 1 straight back, so the interval that is actually stable at the head
// of a bottom-tested loop is [0, bound-2]: land on bound-1 and the next sweep
// steps past it, and the whole ladder is climbed for nothing. LLVM rotates
// almost every counted loop into exactly that shape.
//
// The rest come from the function's own i32 constants, for the same reason and
// with the same off-by-one: `for (i = 0; i < 100; i++)` settles on [0, 100]
// written one way and [0, 99] written the other.
func thresholds(f *ir.Func) []int64 {
	base := []int64{0, 1, signBit - 2, signBit - 1, signBit, u32Max - 1, u32Max}
	set := map[int64]bool{}
	for _, v := range base {
		set[v] = true
	}
	for i := range f.Steps {
		if f.Steps[i].Op != wasm.OpI32Const {
			continue
		}
		k := int64(f.Steps[i].Instr.I32)
		set[k] = true
		if k+1 <= u32Max {
			set[k+1] = true
		}
		if k-1 >= 0 {
			set[k-1] = true
		}
		if len(set) > maxThresholds {
			set = map[int64]bool{}
			for _, v := range base {
				set[v] = true
			}
			break
		}
	}
	out := make([]int64, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// entryLocals is what is known before the first step runs.
//
// Parameters are unknown, but the spec says a DECLARED local starts at zero,
// and that is the fact a counted loop's induction variable is built on: the
// emitter already writes the zero into the prologue, so nothing has to be
// proved for it to be true.
func entryLocals(f *ir.Func) locals {
	out := locals{}
	for i, t := range f.Locals {
		if t == wasm.I32 {
			out[uint32(len(f.Params)+i)] = Range{0, 0}
		}
	}
	return out
}

// guardFacts is a conditional terminator's test, resolved back to the wasm
// locals it reads. Anything it cannot resolve is left at -1 and simply does not
// get refined.
type guardFacts struct {
	op     wasm.Op
	invert bool
	aLocal int32
	bLocal int32
	a, b   Range
}

func noGuard() guardFacts {
	return guardFacts{op: wasm.OpNop, aLocal: -1, bLocal: -1}
}

func (g guardFacts) usable() bool {
	return g.op != wasm.OpNop && (g.aLocal >= 0 || g.bLocal >= 0)
}

// narrowed is the local state on one edge out of a conditional block.
func narrowed(base locals, g guardFacts, taken bool) locals {
	if !g.usable() {
		return base
	}
	if g.invert {
		taken = !taken
	}
	ra, rb, ok := refine(g.op, g.a, g.b, taken)
	if !ok {
		return base
	}
	out := base.clone()
	if g.aLocal >= 0 {
		out.tighten(uint32(g.aLocal), ra)
	}
	if g.bLocal >= 0 {
		out.tighten(uint32(g.bLocal), rb)
	}
	return out
}

// refine is what a comparison proves about its two operands on the edge where
// it came out `taken`.
func refine(op wasm.Op, a, b Range, taken bool) (Range, Range, bool) {
	// Only genuine u32 operands are refinable. A deferred value in a slot is
	// congruent to the wasm value rather than equal to it, and an interval over
	// one says nothing about the order of the other.
	if !a.FitsU32() || !b.FitsU32() {
		return a, b, false
	}

	// `eqz` is handled before the inversion because its negation is not another
	// comparison: "not zero" is an interval with a hole in it. It is still worth
	// having, because the hole is at the END of the interval in the shape that
	// matters -- LLVM strength-reduces a counted loop into `while (n--)`, whose
	// guard is exactly this, and `n >= 1` is what lets `n - 1` drop its wrap.
	if op == wasm.OpI32Eqz {
		if taken {
			return Range{0, 0}, b, true
		}
		return Range{max64(a.Lo, 1), a.Hi}, b, true
	}

	if !taken {
		inv, ok := invertCmp(op)
		if !ok {
			return a, b, false
		}
		op = inv
	}

	switch op {
	case wasm.OpI32Eq:
		r := a.meet(b)
		return r, r, true

	case wasm.OpI32Ne:
		// Same hole, same reason it is still worth something: when the excluded
		// value sits on an endpoint, the endpoint moves.
		return excluding(a, b), excluding(b, a), true

	// Unsigned order is the order of the numbers actually in the slots, which
	// is what makes these unconditional -- the payoff of Invariant A showing up
	// somewhere other than the lowering.
	case wasm.OpI32LtU:
		return Range{a.Lo, min64(a.Hi, b.Hi-1)}, Range{max64(b.Lo, a.Lo+1), b.Hi}, true
	case wasm.OpI32LeU:
		return Range{a.Lo, min64(a.Hi, b.Hi)}, Range{max64(b.Lo, a.Lo), b.Hi}, true
	case wasm.OpI32GtU:
		return Range{max64(a.Lo, b.Lo+1), a.Hi}, Range{b.Lo, min64(b.Hi, a.Hi-1)}, true
	case wasm.OpI32GeU:
		return Range{max64(a.Lo, b.Lo), a.Hi}, Range{b.Lo, min64(b.Hi, a.Hi)}, true

	case wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS:
		return refineSigned(op, a, b)
	}
	return a, b, false
}

// refineSigned is the same for the signed comparisons, where the interval is
// over the UNSIGNED representation and so the two orders disagree above 2^31.
//
// The asymmetry below is not an oversight. `x >=s c` for a non-negative c says
// x is non-negative and at least c -- a single interval, [c, 2^31-1] -- and
// that is the fact a rotated counted loop's pre-header guard (`if n < 1 skip`)
// hands the whole loop. `x <s c` says x is below c OR negative, which is two
// intervals and therefore nothing this domain can hold, unless x is already
// known non-negative.
func refineSigned(op wasm.Op, a, b Range) (Range, Range, bool) {
	posA, posB := a.Below(signBit), b.Below(signBit)
	outA, outB := a, b

	switch op {
	case wasm.OpI32LtS: // a <s b
		if posA {
			outB = Range{max64(b.Lo, a.Lo+1), min64(b.Hi, signBit-1)}
			outA = Range{a.Lo, min64(a.Hi, outB.Hi-1)}
		}
	case wasm.OpI32LeS: // a <=s b
		if posA {
			outB = Range{max64(b.Lo, a.Lo), min64(b.Hi, signBit-1)}
			outA = Range{a.Lo, min64(a.Hi, outB.Hi)}
		}
	case wasm.OpI32GtS: // a >s b
		if posB {
			outA = Range{max64(a.Lo, b.Lo+1), min64(a.Hi, signBit-1)}
			outB = Range{b.Lo, min64(b.Hi, outA.Hi-1)}
		}
	case wasm.OpI32GeS: // a >=s b
		if posB {
			outA = Range{max64(a.Lo, b.Lo), min64(a.Hi, signBit-1)}
			outB = Range{b.Lo, min64(b.Hi, outA.Hi)}
		}
	}
	return outA, outB, true
}

// invertCmp is the comparison that holds exactly when this one does not.
//
// Integers are totally ordered, so every one of these is exact. `eqz` is
// absent on purpose: its negation says only that the operand is non-zero, which
// an interval cannot express without a hole in the middle.
func invertCmp(op wasm.Op) (wasm.Op, bool) {
	switch op {
	case wasm.OpI32LtU:
		return wasm.OpI32GeU, true
	case wasm.OpI32LeU:
		return wasm.OpI32GtU, true
	case wasm.OpI32GtU:
		return wasm.OpI32LeU, true
	case wasm.OpI32GeU:
		return wasm.OpI32LtU, true
	case wasm.OpI32LtS:
		return wasm.OpI32GeS, true
	case wasm.OpI32LeS:
		return wasm.OpI32GtS, true
	case wasm.OpI32GtS:
		return wasm.OpI32LeS, true
	case wasm.OpI32GeS:
		return wasm.OpI32LtS, true
	}
	return op, false
}

func isCompare(op wasm.Op) bool {
	switch op {
	// `ne` belongs here and was missing, which made refine's OpI32Ne case
	// unreachable: a comparison that is never RECORDED can never be resolved
	// back to a guard, so every `!=` test was discarded before it could refine
	// anything. It is the test a countdown loop closes with -- `while (--n !=
	// 0)` -- so what was lost is `n >= 1` on the back edge, the one fact that
	// makes such a loop's counter provably non-zero inside its own body.
	// Narrowing on the not-taken edge stays off: invertCmp has no entry for it,
	// because "not (a != b)" pins a to a single value only when b is exact, and
	// refine's Eq case already covers the shape that arises in practice.
	case wasm.OpI32Eq, wasm.OpI32Eqz, wasm.OpI32Ne,
		wasm.OpI32LtU, wasm.OpI32LeU, wasm.OpI32GtU, wasm.OpI32GeU,
		wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS:
		return true
	}
	return false
}

// excluding removes a single known value from an interval, which is only
// expressible when it sits on one of the two ends.
func excluding(r, k Range) Range {
	v, ok := k.Exact()
	if !ok {
		return r
	}
	if v == r.Lo {
		r.Lo++
	} else if v == r.Hi {
		r.Hi--
	}
	return r
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// runBlock walks one basic block, and is where every range actually comes from.
//
// It is the M5 block-local pass unchanged, with one entry state handed in
// instead of assumed empty: slot ranges still start fresh and still die at every
// boundary instruction, because a stack slot's deferral is a deal struck with
// one consumer in one block. Only the wasm locals cross.
//
// `record` writes the answers into the Wrap. The fixpoint sweeps do not, so the
// emitter never sees a half-solved state.
func (s *solver) runBlock(b int, entry locals, record bool) (locals, guardFacts) {
	f := s.f
	blk := &s.cfg.Blocks[b]

	local := entry.clone()
	slot := map[ir.Slot]Range{}
	// origin is the step that wrote a slot, and fromLocal the wasm local a slot
	// is currently a copy of. Both die with the slot state at a boundary; both
	// are only used to turn a terminator's condition back into a statement
	// about locals.
	origin := map[ir.Slot]int{}
	fromLocal := map[ir.Slot]uint32{}
	cmps := map[int]guardFacts{}
	g := noGuard()

	forget := func(sl ir.Slot) {
		delete(slot, sl)
		delete(origin, sl)
		delete(fromLocal, sl)
	}

	for i := blk.Start; i < blk.End; i++ {
		st := &f.Steps[i]

		args := make([]Range, len(st.Args))
		for k := range st.Args {
			args[k] = FullU32
			if k < len(st.ArgTypes) && st.ArgTypes[k] != wasm.I32 {
				continue
			}
			if r, ok := slot[st.Args[k]]; ok {
				args[k] = r
			}
		}
		if record {
			s.w.Arg[i] = args
		}

		if isBoundary(st.Op) {
			// The terminator's condition has to be read before the state that
			// explains it is dropped.
			if i == blk.End-1 && blk.Kind == ir.ExitCond {
				g = resolveGuard(st, origin, cmps, slot, fromLocal)
			}
			// Everything a branch could have been reached from is unknown, so
			// no slot survives the boundary. Locals do -- that is the fixpoint.
			slot = map[ir.Slot]Range{}
			origin = map[ir.Slot]int{}
			fromLocal = map[ir.Slot]uint32{}
			cmps = map[int]guardFacts{}
			continue
		}

		switch st.Op {
		case wasm.OpLocalSet, wasm.OpLocalTee:
			idx := st.Instr.LocalIndex
			r := FullU32
			if len(args) > 0 && f.LocalType(idx) == wasm.I32 {
				r = args[0]
			}
			// A local always holds a genuine wasm value: deferral is only ever
			// offered to an arithmetic consumer, never to a local.set. The
			// clamp is what lets the fixpoint carry the interval across a
			// boundary without having to re-derive that.
			if !r.FitsU32() {
				r = FullU32
			}
			local[idx] = r
			for sl, l := range fromLocal {
				if l == idx {
					delete(fromLocal, sl)
				}
			}
			for j, c := range cmps {
				if c.aLocal == int32(idx) {
					c.aLocal = -1
				}
				if c.bLocal == int32(idx) {
					c.bLocal = -1
				}
				cmps[j] = c
			}
			if st.Op == wasm.OpLocalTee && st.Dst != ir.NoSlot {
				slot[st.Dst] = r
				origin[st.Dst] = i
				if f.LocalType(idx) == wasm.I32 {
					fromLocal[st.Dst] = idx
				}
				if record {
					s.w.Result[i] = r
				}
			}
			continue
		}

		if st.Dst == ir.NoSlot {
			continue
		}
		if st.DstType != wasm.I32 {
			forget(st.Dst)
			if st.DstType.Slots() > 1 {
				forget(st.Dst + 1)
			}
			continue
		}

		r, elide := transfer(f, s.ahead, i, args, local, s.cons)

		// Before anything is forgotten. A binary op's result lands in its FIRST
		// operand's slot -- that is how the stack pops -- so writing the result
		// first would wipe the very association that says which local was being
		// compared, and the guard would come back empty every time.
		var facts guardFacts
		cmp := isCompare(st.Op)
		if cmp {
			facts = compareFacts(st, args, origin, fromLocal, cmps)
		}

		if record {
			s.w.Result[i] = r
			s.w.Elide[i] = elide
		}
		forget(st.Dst)
		slot[st.Dst] = r
		origin[st.Dst] = i
		if st.Op == wasm.OpLocalGet && f.LocalType(st.Instr.LocalIndex) == wasm.I32 {
			fromLocal[st.Dst] = st.Instr.LocalIndex
		}
		if cmp {
			cmps[i] = facts
		}
		// A wide value below this one would have left a stale entry in the slot
		// after it; nothing reads that, but clearing keeps the map honest.
		forget(st.Dst + 1)
	}
	return local, g
}

// compareFacts captures what a comparison is comparing, at the moment it runs.
//
// It has to be captured here rather than at the branch that uses it: a stack
// slot is recycled the moment its value is read, so by the time the terminator
// asks, the slot that held the left operand may belong to something else
// entirely -- and would answer with the wrong local rather than with none.
func compareFacts(st *ir.Step, args []Range, origin map[ir.Slot]int,
	fromLocal map[ir.Slot]uint32, cmps map[int]guardFacts) guardFacts {

	g := guardFacts{op: st.Op, aLocal: -1, bLocal: -1}
	if len(args) > 0 {
		g.a = args[0]
	}
	if len(args) > 1 {
		g.b = args[1]
	}

	// `eqz` of a comparison is that comparison inverted, which is how a
	// front end writes `if (!(a < b))`. Chaining it here means the branch sees
	// the real relation instead of "something was zero".
	if st.Op == wasm.OpI32Eqz && len(st.Args) > 0 {
		if j, ok := origin[st.Args[0]]; ok {
			if inner, ok := cmps[j]; ok && inner.op != wasm.OpI32Eqz {
				inner.invert = !inner.invert
				return inner
			}
		}
	}

	if len(st.Args) > 0 {
		if l, ok := fromLocal[st.Args[0]]; ok {
			g.aLocal = int32(l)
		}
	}
	if len(st.Args) > 1 {
		if l, ok := fromLocal[st.Args[1]]; ok {
			g.bLocal = int32(l)
		}
	}
	return g
}

// resolveGuard finds what a conditional terminator is about to branch on.
func resolveGuard(t *ir.Step, origin map[ir.Slot]int, cmps map[int]guardFacts,
	slot map[ir.Slot]Range, fromLocal map[ir.Slot]uint32) guardFacts {

	if len(t.Args) == 0 {
		return noGuard()
	}
	cond := t.Args[0]
	if j, ok := origin[cond]; ok {
		if g, ok := cmps[j]; ok {
			return g
		}
	}
	// A bare value used as a condition is a test against zero, and that is not
	// a corner case: `if (n)` compiles to it, and so does every loop LLVM
	// strength-reduces into a countdown -- which, measured on TinyGo output, is
	// most of them. Recorded as an inverted `eqz`, because the branch is taken
	// when the value is NOT zero.
	if l, ok := fromLocal[cond]; ok {
		r, known := slot[cond]
		if !known {
			r = FullU32
		}
		return guardFacts{op: wasm.OpI32Eqz, invert: true, aLocal: int32(l), bLocal: -1, a: r}
	}
	return noGuard()
}

// transfer computes step i's result range and whether its wrap may go.
func transfer(f *ir.Func, ahead *Wrap, i int, args []Range, local map[uint32]Range, cons []int) (Range, bool) {
	s := &f.Steps[i]
	a, b := argAt(args, 0), argAt(args, 1)

	switch s.Op {
	case wasm.OpI32Const:
		return Range{int64(s.Instr.I32), int64(s.Instr.I32)}, false

	case wasm.OpLocalGet:
		if r, ok := local[s.Instr.LocalIndex]; ok {
			return r, false
		}
		return FullU32, false

	case wasm.OpI32Add:
		return maybeDefer(f, ahead, i, Range{a.Lo + b.Lo, a.Hi + b.Hi}, cons)
	case wasm.OpI32Sub:
		return maybeDefer(f, ahead, i, Range{a.Lo - b.Hi, a.Hi - b.Lo}, cons)
	case wasm.OpI32Mul:
		// Only the constant-specialised lowering is an expression the wrap can
		// be lifted out of; the general path calls mul32, which needs true
		// 32-bit operands.
		if k, ok := b.ConstU32(); ok && k < 1<<21 && a.Lo >= 0 {
			return maybeDefer(f, ahead, i, Range{a.Lo * int64(k), a.Hi * int64(k)}, cons)
		}
		return FullU32, false

	case wasm.OpI32And:
		if k, ok := b.ConstU32(); ok {
			if n, isLow := lowMask(k); isLow {
				return Range{0, int64(1)<<n - 1}, false
			}
			if k == 0 {
				return Range{0, 0}, false
			}
		}
		if k, ok := a.ConstU32(); ok {
			if n, isLow := lowMask(k); isLow {
				return Range{0, int64(1)<<n - 1}, false
			}
		}
		// A bitwise and can only clear bits, so the result is bounded by the
		// smaller operand -- true of band() as much as of the % form.
		hi := a.Hi
		if b.Hi < hi {
			hi = b.Hi
		}
		if hi < 0 || hi > u32Max {
			hi = u32Max
		}
		return Range{0, hi}, false

	case wasm.OpI32ShrU:
		if k, ok := b.ConstU32(); ok && a.FitsU32() {
			n := k % 32
			return Range{a.Lo >> n, a.Hi >> n}, false
		}
		return FullU32, false

	case wasm.OpI32Shl:
		if k, ok := b.ConstU32(); ok {
			n := k % 32
			if n == 0 {
				return a, false
			}
			return Range{0, u32Max}, false
		}
		return FullU32, false

	case wasm.OpI32DivU:
		if a.FitsU32() {
			return Range{0, a.Hi}, false
		}
		return FullU32, false
	case wasm.OpI32RemU:
		if k, ok := b.ConstU32(); ok && k > 0 {
			return Range{0, int64(k) - 1}, false
		}
		return FullU32, false

	case wasm.OpI32Eqz,
		wasm.OpI32Eq, wasm.OpI32Ne,
		wasm.OpI32LtU, wasm.OpI32LeU, wasm.OpI32GtU, wasm.OpI32GeU,
		wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS,
		wasm.OpI64Eq, wasm.OpI64Ne, wasm.OpI64Eqz,
		wasm.OpI64LtS, wasm.OpI64LtU, wasm.OpI64GtS, wasm.OpI64GtU,
		wasm.OpI64LeS, wasm.OpI64LeU, wasm.OpI64GeS, wasm.OpI64GeU,
		wasm.OpF32Eq, wasm.OpF32Ne, wasm.OpF32Lt, wasm.OpF32Gt, wasm.OpF32Le, wasm.OpF32Ge,
		wasm.OpF64Eq, wasm.OpF64Ne, wasm.OpF64Lt, wasm.OpF64Gt, wasm.OpF64Le, wasm.OpF64Ge:
		return Range{0, 1}, false

	case wasm.OpI32Clz, wasm.OpI32Ctz, wasm.OpI32Popcnt:
		return Range{0, 32}, false

	case wasm.OpI32Load8U:
		return Range{0, 255}, false
	case wasm.OpI32Load16U:
		return Range{0, 65535}, false

	case wasm.OpMemorySize:
		return Range{0, 65536}, false

	case wasm.OpGlobalGet:
		// A global's declared initialiser says nothing once anything writes it,
		// and a mutable global can be written from any function.
		return FullU32, false
	}
	return FullU32, false
}

func argAt(args []Range, k int) Range {
	if k >= len(args) {
		return FullU32
	}
	return args[k]
}

// maybeDefer decides whether step i's wrap can be dropped.
//
// Two independent reasons, and they are not the same optimization:
//
//   - The result provably fits [0, 2^32) already, so the wrap was never doing
//     anything. Safe unconditionally: every consumer still sees the wasm value.
//   - The single consumer re-reduces modulo 2^32 anyway, so a value merely
//     CONGRUENT to the wasm value is enough. This is the deferral proper, and
//     it is what collapses `(p + i*4) % 2^32` from two wraps to one.
func maybeDefer(f *ir.Func, ahead *Wrap, i int, r Range, cons []int) (Range, bool) {
	if r.FitsU32() {
		return r, true
	}
	if ahead == nil || !r.safe() {
		return FullU32, false
	}
	j := cons[i]
	if j < 0 || !absorbs(f, ahead, j, f.Steps[i].Dst) {
		return FullU32, false
	}
	return r, true
}

// absorbs reports that step j's lowering reduces its operands modulo 2^32
// anyway, so handing it a value that is only congruent changes nothing.
//
// The membership rule is arithmetic, not a hunch: `(x + y) % M` depends on x
// only through x mod M, and `x % 2^n` for n <= 32 depends on x only through
// x mod 2^32 because 2^n divides 2^32. Anything that reads a bit pattern
// directly -- a shift right, a memory address, a comparison -- does not
// qualify and never will.
func absorbs(f *ir.Func, ahead *Wrap, j int, via ir.Slot) bool {
	s := &f.Steps[j]
	switch s.Op {
	case wasm.OpI32Add, wasm.OpI32Sub:
		// Both operand positions are inside the same `% 2^32`.
		return true
	case wasm.OpI32And:
		// Only the low-mask lowering, `a % 2^n`, and only for the masked side.
		k, ok := ahead.ArgRange(j, 1).ConstU32()
		if !ok {
			return false
		}
		if _, isLow := lowMask(k); !isLow {
			return false
		}
		return len(s.Args) > 0 && s.Args[0] == via
	case wasm.OpI32Shl:
		// `(a % 2^(32-n)) * 2^n` masks the shifted side first -- but only for a
		// REAL shift. A distance of 0 mod 32 lowers to the identity, which
		// masks nothing at all, so a value that is merely congruent reaches the
		// identity's own consumer raw: a negative deferred sub reads as
		// negative to an unsigned compare, and as an out-of-range address to a
		// load. The And case guards the same way, by insisting on a low mask --
		// which is what excludes and-with-all-ones, its own identity.
		k, ok := ahead.ArgRange(j, 1).ConstU32()
		if !ok || k%32 == 0 {
			return false
		}
		return len(s.Args) > 0 && s.Args[0] == via
	}
	return false
}

// consumers maps each step to the step that reads its result, or -1.
//
// A wasm operand-stack slot is written once and read once, so "the first later
// step that names this slot as an operand" is the consumer -- provided nothing
// rewrites the slot first, which happens because slots are recycled as the
// stack pops. The scan stops at a control-flow instruction: a value that
// crosses a block boundary can be read after a jump from somewhere else
// entirely, and no local decision may be made about it.
func consumers(f *ir.Func) []int {
	out := make([]int, len(f.Steps))
	for i := range out {
		out[i] = -1
	}
	for i := range f.Steps {
		dst := f.Steps[i].Dst
		if dst == ir.NoSlot {
			continue
		}
		for j := i + 1; j < len(f.Steps); j++ {
			if isBoundary(f.Steps[j].Op) {
				break
			}
			found := false
			for _, a := range f.Steps[j].Args {
				if a == dst {
					found = true
					break
				}
			}
			if found {
				out[i] = j
				break
			}
			if f.Steps[j].Dst == dst {
				break // overwritten without being read
			}
		}
	}
	return out
}

// isBoundary reports an instruction that ends a basic block, either by jumping
// or by defining a label something else can jump to.
func isBoundary(op wasm.Op) bool {
	switch op {
	case wasm.OpBlock, wasm.OpLoop, wasm.OpIf, wasm.OpElse, wasm.OpEnd,
		wasm.OpBr, wasm.OpBrIf, wasm.OpBrTable, wasm.OpReturn, wasm.OpUnreachable:
		return true
	}
	return false
}

// lowMask reports whether k is 2^n - 1.
func lowMask(k uint32) (n uint32, ok bool) {
	if k == 0 || k == 0xFFFFFFFF {
		return 0, false
	}
	if k&(k+1) != 0 {
		return 0, false
	}
	for n = 0; n < 32; n++ {
		if k == (1<<n)-1 {
			return n, true
		}
	}
	return 0, false
}
