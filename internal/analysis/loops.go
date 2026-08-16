package analysis

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Counted describes a loop whose trip count is fixed when the loop is entered,
// so it can be emitted as Lua's numeric `for` instead of a label, an increment,
// a compare and a goto.
//
// The prize is one VM instruction where there were four. Lua compiles a numeric
// for to a single FORLOOP opcode that increments, tests and jumps in one
// dispatch, with the counter held in a register the loop owns. Measured on the
// bench kernels under bin/lua52f, hand-editing the emitted goto loop into this
// shape: `count` 0.858x and `sum` 0.893x against an A/A noise floor of 1.3%.
//
// The analysis answers a question and rewrites nothing, which is the same split
// every other pass in this package keeps. What it hands the emitter is a set of
// step indices to treat specially and the pieces of the `for` header; building
// the Lua is luagen's job.
type Counted struct {
	// Header is the OpLoop step. The emitter prints the `for` here instead of
	// the loop's label.
	Header int
	// Body is [BodyStart, BodyEnd): the steps that go inside the `for`.
	BodyStart, BodyEnd int
	// Close is the step that ends the loop -- the br_if that takes the back
	// edge (bottom-tested) or the unconditional br (top-tested). The emitter
	// prints `end` here.
	Close int
	// Exit is the step whose taken edge leaves the loop, and ExitTo is where it
	// goes. For a bottom-tested loop Exit == Close and the edge is the one NOT
	// taken, so ExitTo is whatever follows. For a top-tested loop Exit is the
	// guard at the top and the emitter must jump to ExitTo after the `for`.
	Exit   int
	ExitTo ir.Branch
	// TopTested distinguishes the two shapes.
	TopTested bool
	// ExitFallsThrough reports that the step right after the loop's `end` is
	// the one defining ExitTo's label, so the emitter can let control fall into
	// it instead of printing a goto immediately above it.
	ExitFallsThrough bool

	// Drop lists steps the `for` subsumes: the increment and the exit test,
	// with everything that feeds them. The emitter skips these.
	Drop map[int]bool

	// Local is the induction variable's wasm local index, Slot its slot. The
	// emitter names it as the `for` control variable, which shadows the outer
	// name for the body -- exactly the scoping the body wants, since nothing in
	// the body writes it.
	Local uint32
	Slot  ir.Slot

	// Step is +1 or -1. Wider constant steps are recognisable but need a
	// congruence proof for the != tests LLVM emits with them; see the file
	// comment on `ne`.
	Step int64

	// LimitFrom is the step whose operand k = LimitArg holds the loop bound, so
	// the emitter can print whatever expression the peephole gave that operand.
	LimitFrom, LimitArg int
	// Adjust is added to the bound to get Lua's inclusive `for` limit.
	Adjust int64
	// FinalAdjust is added to the bound to get the counter's value after the
	// loop, which the emitter assigns because Lua's `for` variable does not
	// outlive its loop.
	FinalAdjust int64
	// Materialise reports that something outside the loop reads the counter, so
	// that assignment actually has to be emitted.
	Materialise bool
	// ExtraExits reports a way out of the loop other than its own test.
	ExtraExits bool
	// CopyEachIteration asks the emitter to give the `for` a control variable of
	// its own and copy it into the wasm local at the top of the body, instead of
	// naming the loop variable after the local. It costs one OP_MOVE per
	// iteration -- measured as no detected change -- and makes the local current
	// at every point in the body, so an exit from the middle needs no separate
	// materialisation.
	CopyEachIteration bool
}

// CountedLoops finds every counted loop in a function.
//
// Everything it refuses is refused for a reason that would otherwise be a
// miscompile, and the restrictions are deliberately blunt because this rewrites
// control flow -- the one part of the emitter where a wrong answer does not
// look like a wrong answer:
//
//   - ONE branch to the header. A `continue` in a bottom-tested loop skips the
//     increment, so the trip count is no longer the header's to predict; in a
//     top-tested one it would need a label inside the `for` body, which is
//     legal but buys nothing here.
//   - ONE write to the induction local inside the loop, and it is the
//     increment. A body that also writes the counter is not counted.
//   - ONE exit edge. Lua's `for` variable does not survive its loop, so the
//     outer name is stale on any other way out, and materialising it on each
//     would need liveness this pass does not have.
//   - The bound is loop-invariant: a constant, or a local nothing in the loop
//     writes.
//   - Step +1 or -1 only. For an ORDERED test any constant step is exact, but
//     LLVM pairs a wider step with `i32.ne`, and `ne` is only equivalent to a
//     numeric for when the counter hits the bound exactly -- a congruence fact,
//     not a range fact. At +-1 divisibility is free and the whole question goes
//     away.
func CountedLoops(f *ir.Func, w *Wrap) map[int]*Counted {
	cfg := ir.BuildCFG(f)
	if !cfg.Complete {
		// A missing edge means every fact below is an under-approximation.
		return nil
	}
	out := map[int]*Counted{}
	for i := range f.Steps {
		if f.Steps[i].Op != wasm.OpLoop {
			continue
		}
		if c := analyseLoop(f, cfg, w, i); c != nil {
			out[i] = c
		}
	}
	return out
}

// loopEnd is the index of the `end` closing the loop that opens at step h.
func loopEnd(f *ir.Func, h int) int {
	depth := 0
	for i := h + 1; i < len(f.Steps); i++ {
		switch f.Steps[i].Op {
		case wasm.OpBlock, wasm.OpLoop, wasm.OpIf:
			depth++
		case wasm.OpEnd:
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

func analyseLoop(f *ir.Func, cfg *ir.CFG, w *Wrap, h int) *Counted {
	label := f.Steps[h].Label
	end := loopEnd(f, h)
	if end < 0 {
		return nil
	}

	// The back edge has to be the last step in the loop, or the body would not
	// be the contiguous run the `for` needs.
	var back int = -1
	branches := 0
	for i := h + 1; i < end; i++ {
		for _, br := range branchesOf(f.Steps[i]) {
			if br.Label == label {
				branches++
				back = i
			}
		}
	}
	if branches != 1 || back != end-1 {
		return nil
	}
	bs := f.Steps[back]
	if bs.Op != wasm.OpBr && bs.Op != wasm.OpBrIf {
		return nil
	}
	// A back edge carrying a value would need the copy emitted before `end`,
	// and a loop label takes no parameters anyway.
	if bs.Target.From != ir.NoSlot {
		return nil
	}

	c := &Counted{Header: h, Close: back, BodyStart: h + 1, BodyEnd: back,
		Drop: map[int]bool{}, TopTested: bs.Op == wasm.OpBr}

	// Find the exit test. A bottom-tested loop leaves by falling out of its own
	// br_if; a top-tested one leaves by a br_if somewhere inside, and for a
	// counted loop that must be the only other way out.
	var test int
	if c.TopTested {
		// The FIRST conditional branch is the bound test -- that is what makes a
		// loop top-tested. Later ones are extra ways out, which the per-iteration
		// copy handles; an unconditional one would make the rest of the body
		// dead, taking the increment with it.
		test = -1
		for i := h + 1; i < back; i++ {
			switch f.Steps[i].Op {
			case wasm.OpBrIf:
				if test < 0 {
					test = i
				} else {
					c.ExtraExits = true
				}
			case wasm.OpBrTable:
				if test < 0 {
					return nil // a table dispatch is not a loop bound
				}
				c.ExtraExits = true
			case wasm.OpBr:
				return nil
			}
		}
		if test < 0 {
			return nil
		}
		c.Exit, c.ExitTo = test, f.Steps[test].Target
		if c.ExitTo.From != ir.NoSlot {
			return nil
		}
		// Everything before the guard is the guard's own operands; the body is
		// what follows it.
		c.BodyStart = test + 1
	} else {
		test = back
		c.Exit = back
		for i := h + 1; i < back; i++ {
			switch f.Steps[i].Op {
			case wasm.OpBrIf, wasm.OpBrTable:
				// An extra way out. The counter has to be current on that edge,
				// which CopyEachIteration below arranges.
				c.ExtraExits = true
			case wasm.OpBr:
				// An unconditional branch before the back edge makes everything
				// after it dead, and a dead increment is not a trip count.
				return nil
			}
		}
	}
	// A loop with more than one way out cannot use the wasm local as the `for`
	// variable directly: Lua scopes that name to the loop, so the outer one is
	// whatever it was before the loop on any edge that leaves from the middle.
	//
	// The fix is a `for` variable of the pass's own and one copy per iteration,
	// which makes the wasm local current at every point in the body -- so every
	// exit path is right without knowing where the exits are. Measured on
	// `count`, where the body is small enough for one extra OP_MOVE to show if
	// it were going to: 0.844x with the copy against 0.847x without, an A/A
	// floor of 1.9%. No detected cost.
	c.CopyEachIteration = c.ExtraExits

	// What the test branches on. Two shapes, and the second is the one real
	// compiler output actually contains:
	//
	//	(br_if $top (i32.lt_s (local.get $i) (local.get $n)))   a comparison
	//	(br_if $top (local.tee $i (i32.sub (local.get $i) 1)))  a bare value
	//
	// The second has no comparison step at all -- a br_if keeps looping while
	// its operand is non-zero, which IS `!= 0`, and the counter reaches the
	// branch straight out of a local.tee. Every countdown TinyGo emits looks
	// like that, so a pass that only knew the first shape would find nothing in
	// a real guest, which is exactly what it did until this was added.
	cmp := defOf(f, test, 0, h)
	if cmp < 0 {
		return nil
	}
	var rel rel
	var ivGet, limGet int
	var limRange Range
	if r, ok := countedRel[f.Steps[cmp].Op]; ok {
		rel = r
		limRange = w.ArgRange(cmp, 1)
		ivGet = defOf(f, cmp, 0, h)
		limGet = defOf(f, cmp, 1, h)
		c.LimitFrom, c.LimitArg = cmp, 1
		// The bound has to be loop-invariant.
		//
		// A def of -1 means defOf walked back past the header without finding
		// one, so the bound was computed BEFORE the loop -- which makes it
		// invariant by construction, provided nothing in the loop overwrites
		// the slot it sits in. That case is not exotic: LLVM hoists a loop
		// bound into the preheader whenever it can, and refusing it was the
		// second largest category of missed loops in a real guest.
		if limGet < 0 {
			if writesSlot(f, h+1, end, f.Steps[cmp].Args[1]) {
				return nil
			}
		} else {
			switch f.Steps[limGet].Op {
			case wasm.OpI32Const:
			case wasm.OpLocalGet:
				if writesLocal(f, h+1, end, f.Steps[limGet].Instr.LocalIndex) {
					return nil
				}
			default:
				return nil
			}
		}
	} else {
		rel = relNe
		ivGet = cmp
		limGet = -1
		// The bound is an implicit zero, which no step holds -- so it cannot be
		// read back off the comparison the way the other shape's is.
		c.LimitFrom, c.LimitArg = -1, 0
		limRange = Range{Lo: 0, Hi: 0}
	}

	// A top-tested guard branches OUT when it is true, so the relation that
	// keeps the loop running is its complement.
	if c.TopTested {
		rel = rel.negate()
	}

	// The tested value is the counter -- read by a local.get, or produced by
	// the local.tee that increments it.
	if ivGet < 0 {
		return nil
	}
	switch f.Steps[ivGet].Op {
	case wasm.OpLocalGet, wasm.OpLocalTee:
	default:
		return nil
	}
	c.Local = f.Steps[ivGet].Instr.LocalIndex
	if f.LocalType(c.Local) != wasm.I32 {
		return nil
	}
	c.Slot = f.LocalSlot(c.Local)

	// The single write to the counter, and it must be `i = i + k`.
	inc := -1
	for i := h + 1; i < end; i++ {
		s := f.Steps[i]
		if (s.Op == wasm.OpLocalSet || s.Op == wasm.OpLocalTee) && s.Instr.LocalIndex == c.Local {
			if inc >= 0 {
				return nil
			}
			inc = i
		}
	}
	if inc < 0 {
		return nil
	}
	// A local.tee both increments and hands the new value to the test, so when
	// the test read the counter from a tee that tee IS the increment. Any other
	// tee is a write whose value goes somewhere this pass has not accounted for.
	if f.Steps[inc].Op == wasm.OpLocalTee && inc != ivGet {
		return nil
	}
	add := defOf(f, inc, 0, h)
	if add < 0 {
		return nil
	}
	// Both forms of a step of one. `i32.sub n 1` is not a stylistic variant of
	// `i32.add n -1`: under Invariant A the slot holds an UNSIGNED value, so the
	// add's interval is `n + 4294967295`, which leaves u32 and gets clamped to
	// the full range -- taking the `n != 0` the guard proved with it. The sub
	// keeps the interval, and the sub is what LLVM emits for a countdown, which
	// is the shape this pass most needs to see.
	var dir int64
	switch f.Steps[add].Op {
	case wasm.OpI32Add:
		dir = 1
	case wasm.OpI32Sub:
		dir = -1
	default:
		return nil
	}
	addIV := defOf(f, add, 0, h)
	if addIV < 0 || f.Steps[addIV].Op != wasm.OpLocalGet ||
		f.Steps[addIV].Instr.LocalIndex != c.Local {
		return nil
	}
	kStep := defOf(f, add, 1, h)
	if kStep < 0 || f.Steps[kStep].Op != wasm.OpI32Const {
		return nil
	}
	switch k := f.Steps[kStep].Instr.I32; k {
	case 1:
		c.Step = dir
	case 0xFFFFFFFF:
		c.Step = -dir
	default:
		return nil
	}

	// The body has to see PRE-increment values, because that is what Lua's `for`
	// variable holds while the body runs. Nothing may read the counter after the
	// increment except the test, which goes away with it.
	//
	// This is not the same as "the increment comes last", and the two shapes
	// disagree about where it sits: a bottom-tested loop increments and then
	// tests, a top-tested one tests at the top and increments at the end. Asking
	// about reads rather than about position is what covers both.
	for j := inc + 1; j < c.BodyEnd; j++ {
		if j == ivGet || j == cmp || j == test {
			continue
		}
		s := f.Steps[j]
		if (s.Op == wasm.OpLocalGet || s.Op == wasm.OpLocalTee) && s.Instr.LocalIndex == c.Local {
			return nil
		}
	}

	// The counter is read AFTER the increment by the test, so a bottom-tested
	// loop compares the incremented value and a top-tested one the current one.
	// That is the whole difference between the two bound adjustments below.
	if !rel.ok(c.Step) {
		return nil
	}
	c.Adjust, c.FinalAdjust = rel.adjust(c.Step)

	// A bottom-tested loop runs its body before it tests anything; Lua's `for`
	// tests first. They agree only when at least one iteration was going to
	// happen, and the rotated shape's pre-header guard is what says so.
	if !c.TopTested && !enterUnconditionally(f, w, c, limRange, add) {
		return nil
	}

	// Lua's `for` variable does not outlive its loop, so the outer name is
	// stale afterwards. When nothing reads it there is nothing to fix; when
	// something does, assigning the bound has to be right for a zero-trip loop
	// too, and that needs to know where the counter came in.
	// The loop's own `end` emits nothing (a loop's label is at its top), so the
	// step after it is what control reaches by falling out.
	if c.TopTested && end+1 < len(f.Steps) {
		nxt := f.Steps[end+1]
		c.ExitFallsThrough = nxt.Op == wasm.OpEnd && nxt.Label == c.ExitTo.Label &&
			nxt.ElseLabel == ir.NoLabel
	}

	c.Materialise = readAfterLoop(f, cfg, c, end)
	if c.Materialise && !materialisationIsExact(f, c, limRange) {
		return nil
	}

	// Everything feeding the test and the increment goes away with them.
	//
	// Negative indices are skipped rather than stored: the bare-value shape has
	// no comparison step and no bound step, so limGet is -1, and a -1 in this
	// set indexes f.Steps out of range in every consumer.
	for _, s := range []int{test, cmp, ivGet, limGet, inc, add, addIV, kStep} {
		if s >= 0 {
			c.Drop[s] = true
		}
	}
	if c.TopTested {
		c.Drop[c.Close] = true
	}

	// Nothing dropped may be needed by anything kept: these steps exist only to
	// serve the loop's own bookkeeping.
	if usedOutside(f, c, h, end) {
		return nil
	}
	return c
}

// countedRel is the relation that keeps the loop running, by comparison opcode.
type rel int

const (
	relLt rel = iota
	relLe
	relGt
	relGe
	relNe
	relEq
)

var countedRel = map[wasm.Op]rel{
	wasm.OpI32LtU: relLt, wasm.OpI32LtS: relLt,
	wasm.OpI32LeU: relLe, wasm.OpI32LeS: relLe,
	wasm.OpI32GtU: relGt, wasm.OpI32GtS: relGt,
	wasm.OpI32GeU: relGe, wasm.OpI32GeS: relGe,
	wasm.OpI32Ne: relNe,
}

func (r rel) negate() rel {
	switch r {
	case relLt:
		return relGe
	case relLe:
		return relGt
	case relGt:
		return relLe
	case relGe:
		return relLt
	}
	// The complement of `ne` is `eq`, which cannot keep a counted loop running
	// for more than one iteration. ok() rejects it.
	return relEq
}

// ok reports a relation that can keep a loop running in the direction the step
// moves. Counting up while the test says "keep going while i > n" terminates
// only by wrapping, which is not a counted loop.
func (r rel) ok(step int64) bool {
	switch r {
	case relLt, relLe:
		return step > 0
	case relGt, relGe:
		return step < 0
	case relNe:
		return true
	}
	return false
}

// adjust turns the wasm bound into Lua's inclusive `for` limit, and into the
// counter's value once the loop has finished.
//
// The two shapes agree here, which is worth stating because it looks like they
// should not. A bottom-tested loop tests the INCREMENTED counter and a
// top-tested one tests the value the body is about to use -- but "the body runs
// with v while v+step satisfies the test" and "the body runs with v while v
// satisfies the test" pick out the same set of v, so both are `for i = init,
// bound - step`. Where they genuinely differ is the entry obligation, and that
// is handled at the call site, not here.
func (r rel) adjust(step int64) (limit, final int64) {
	switch r {
	case relLt, relGt, relNe:
		// Running while v is strictly on one side: the last value the body
		// sees is one step short of the bound, and the counter lands on it.
		return -step, 0
	case relLe, relGe:
		// Running through the bound inclusive: the body sees it, and the
		// counter lands one step past.
		return 0, step
	}
	return 0, 0
}

// enterUnconditionally reports that the loop provably runs at least once.
//
// A bottom-tested loop runs its body before testing anything, so Lua's `for` --
// which tests first -- is only equivalent when at least one iteration was going
// to happen. The rotated shape LLVM emits always carries a pre-header guard
// saying exactly that, and the range fixpoint has already narrowed the bound on
// the guard's not-taken edge, so the fact is usually there to be read.
//
// The counter's entry value is not, so it is established structurally instead:
// a DECLARED local that nothing writes before the loop still holds the zero the
// spec says it starts at. A parameter, or a local written on the way in, is
// unknown and refuses the lowering.
// It has two sources for the counter and needs only one of them, because the
// two directions are proved by opposite ends of an interval. Counting UP wants
// an upper bound on the entry value, which a loop-wide range cannot give -- its
// high end is the loop's maximum, not its first value -- so that case leans on
// the structural zero. Counting DOWN wants a lower bound, and the loop-wide
// range's low end is exactly that: `n != 0` on the guard's not-taken edge is
// what the fixpoint already recorded, and it is what makes the countdown LLVM
// emits for most counted loops recognisable at all.
//
// Reading the counter at its INCREMENT is sound here specifically because the
// loop is bottom-tested: the body runs before anything is tested, so the
// increment runs at least once and its operand range contains the entry value.
func enterUnconditionally(f *ir.Func, w *Wrap, c *Counted, lim Range, add int) bool {
	if !lim.safe() {
		return false
	}
	iv := w.ArgRange(add, 0)
	if init, ok := entryValue(f, c); ok {
		iv = Range{Lo: init, Hi: init}
	}
	if !iv.safe() {
		return false
	}
	if c.Step > 0 {
		return iv.Hi < lim.Lo
	}
	return iv.Lo > lim.Hi
}

// materialisationIsExact reports that assigning the bound to the counter after
// the loop is right even when the loop ran zero times -- which happens only if
// the counter entered equal to the bound, and then the bound IS its value.
// It may only use the STRUCTURAL entry value, never the loop-wide range: the
// case it has to be right about is the loop running zero times, and then the
// increment never ran and its operand range says nothing about a value that
// never reached it.
func materialisationIsExact(f *ir.Func, c *Counted, lim Range) bool {
	init, ok := entryValue(f, c)
	if !ok {
		return false
	}
	if !lim.safe() {
		return false
	}
	if c.Step > 0 {
		return init <= lim.Lo
	}
	return init >= lim.Hi
}

// entryValue is the counter's value when the loop is entered, when that is
// knowable without a dataflow pass: a declared local starts at zero and keeps
// it until something writes it.
func entryValue(f *ir.Func, c *Counted) (int64, bool) {
	if int(c.Local) < len(f.Params) {
		return 0, false
	}
	for i := 0; i < c.Header; i++ {
		s := f.Steps[i]
		if (s.Op == wasm.OpLocalSet || s.Op == wasm.OpLocalTee) && s.Instr.LocalIndex == c.Local {
			return 0, false
		}
	}
	return 0, true
}

// readAfterLoop reports the counter being read on some path OUT of the loop.
// When nothing reads it, the emitter can skip materialising it altogether --
// and with the materialisation goes the whole obligation to know what the
// counter ends up as, which is what lets a loop whose trip count might be zero
// be lowered at all.
//
// It has to be asked of the CFG rather than of the step list. A read at a LOWER
// index is normally unreachable from the exit and irrelevant -- and a counter
// that is a parameter is nearly always read just before its own loop, so
// treating "outside" as "any index outside the range" refuses exactly the
// countdown this pass exists to catch. An enclosing loop can genuinely carry
// control back to such a read, which is why the answer comes from reachability
// and not from a comparison of indices.
func readAfterLoop(f *ir.Func, cfg *ir.CFG, c *Counted, end int) bool {
	// The header step BEGINS a block, so the loop's own first block starts at
	// exactly c.Header. Excluding it would put the whole loop body one BFS hop
	// away and report every in-loop read of the counter as a read after it --
	// which is not wrong, only useless: it would demand a materialisation for
	// every loop and refuse the ones that cannot prove it.
	inLoop := func(b int) bool {
		blk := cfg.Blocks[b]
		return blk.Start >= c.Header && blk.Start < end
	}

	seen := make([]bool, len(cfg.Blocks))
	var queue []int
	for b := range cfg.Blocks {
		if !inLoop(b) {
			continue
		}
		for _, s := range cfg.Blocks[b].Succs {
			if s != ir.NoBlock && !inLoop(s) && !seen[s] {
				seen[s] = true
				queue = append(queue, s)
			}
		}
	}
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		blk := cfg.Blocks[b]
		for i := blk.Start; i < blk.End; i++ {
			s := f.Steps[i]
			if (s.Op == wasm.OpLocalGet || s.Op == wasm.OpLocalTee) &&
				s.Instr.LocalIndex == c.Local {
				return true
			}
		}
		for _, s := range blk.Succs {
			if s != ir.NoBlock && !seen[s] {
				seen[s] = true
				queue = append(queue, s)
			}
		}
	}
	return false
}

// branchesOf lists a step's outgoing edges.
func branchesOf(s ir.Step) []ir.Branch {
	switch s.Op {
	case wasm.OpBr, wasm.OpBrIf:
		return []ir.Branch{s.Target}
	case wasm.OpBrTable:
		return append(append([]ir.Branch{}, s.Targets...), s.Default)
	}
	return nil
}

// defOf finds the step that wrote operand k of step i, searching backwards and
// stopping at the loop header: a definition from outside the loop is not part
// of the loop's own bookkeeping and must not be dropped with it.
//
// # IT IS A LINEAR SCAN, AND THAT IS ONLY SOUND IN A STRAIGHT-LINE BODY
//
// This walks step INDICES, which is textual order, and knows nothing about
// control flow. In a body with no branches textual order IS execution order, so
// the nearest preceding writer of a slot is the definition. In a body with
// branches it need not be: the nearest textually-preceding writer can sit in a
// sibling arm that never executed on the path reaching the use, and then every
// answer derived from it is about the wrong step.
//
// Both consumers rely on that, and both enforce it, but neither says so HERE --
// which is why this comment exists. `analyseLoop` refuses a `continue` and any
// second branch to the header; `analyseGuard` refuses every block, if, else and
// branch in the body outright. Relaxing either of those without first making
// this control-flow aware is not a missed optimization, it is silent
// unsoundness: for the counted-loop pass a wrong trip count, and for the loop
// guard a wrong ADDRESS SPAN, which means a bounds check hoisted off an access
// it does not cover.
//
// The fix, when someone wants branchy bodies (measured at 0.950x on
// `real_entities` -- see agents/optimizer.md), is to require
// `cfg.BlockOf[def] == cfg.BlockOf[use]` rather than to widen this.
// TestDefOfIsOnlyUsedUnderAStraightLineBody is the tripwire.
func defOf(f *ir.Func, i, k, h int) int {
	if k >= len(f.Steps[i].Args) {
		return -1
	}
	want := f.Steps[i].Args[k]
	for j := i - 1; j > h; j-- {
		if f.Steps[j].Dst == want {
			return j
		}
	}
	return -1
}

// writesSlot reports any step in [lo, hi) writing an operand-stack slot. It is
// what makes a bound computed before the loop safe to name inside it.
func writesSlot(f *ir.Func, lo, hi int, s ir.Slot) bool {
	for i := lo; i < hi; i++ {
		if f.Steps[i].Dst == s && s != ir.NoSlot {
			return true
		}
	}
	return false
}

func writesLocal(f *ir.Func, lo, hi int, idx uint32) bool {
	for i := lo; i < hi; i++ {
		s := f.Steps[i]
		if (s.Op == wasm.OpLocalSet || s.Op == wasm.OpLocalTee) && s.Instr.LocalIndex == idx {
			return true
		}
	}
	return false
}

// usedOutside reports a dropped step whose result something kept still reads.
// The bookkeeping steps write operand-stack slots that only the test and the
// increment consume, so this is normally false -- but a local.tee or a shared
// subexpression could break that, and silently dropping a step something reads
// leaves a nil in a slot rather than an error at the point of the mistake.
func usedOutside(f *ir.Func, c *Counted, h, end int) bool {
	for i := h + 1; i < end; i++ {
		if c.Drop[i] {
			continue
		}
		for _, a := range f.Steps[i].Args {
			for d := range c.Drop {
				if d < i && f.Steps[d].Dst == a && f.Steps[d].Dst != ir.NoSlot {
					// Something kept reads a dropped step's slot, unless a
					// later kept step rewrote it first.
					rewritten := false
					for j := d + 1; j < i; j++ {
						if !c.Drop[j] && f.Steps[j].Dst == a {
							rewritten = true
							break
						}
					}
					if !rewritten {
						return true
					}
				}
			}
		}
	}
	return false
}
