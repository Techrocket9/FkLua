package luagen

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// forwarding records, for every step, what its operands should read and whether
// the step needs to be emitted at all.
//
// At -opt=0 it carries exactly the M4 rule: a local.get or an i32.const is
// substituted into the instruction that consumes it, and nothing else moves.
// From -opt=1 any single-expression step may be substituted, which turns a
// straight-line run of stack operations into one Lua expression:
//
//	v6 = (v2 * 4) % 4294967296.0          v3 = (v3 + ld32(MEM, MEMSIZE,
//	v5 = (v0 + v6) % 4294967296.0    -->        (v0 + v2 * 4) % 4294967296.0
//	v5 = ld32(MEM, MEMSIZE, v5)                )) % 4294967296.0
//	v4 = (v3 + v5) % 4294967296.0
//	v3 = v4
//
// It is sound because a wasm operand-stack slot is written once and read once,
// so nothing can observe the value that was not written. The hazards are all
// about what happens BETWEEN the two, and each has a rule below: a write to
// something the expression reads, a side effect, a second trap in one
// expression, an operand named twice, and a basic-block boundary.
type forwarding struct {
	elided []bool // steps whose result was forwarded and need no output
	args   [][]string
	// raw[i][k] is the same operand without the brackets sub() adds. It is what
	// a position that already stands alone -- an assignment's right-hand side,
	// a call argument -- prints, so generated Lua stays readable.
	raw [][]string
	// dupable[i][k] reports an operand that is a bare name or numeral, and so
	// may be substituted into a lowering that names it more than once.
	dupable [][]bool
	konst   [][]*uint32 // -opt=0 constant tracking
	// condFrom[i] is the comparison step whose BOOLEAN was folded into branch
	// step i, or -1. Folding it saves materialising 0 or 1 into a slot and
	// testing it again -- three VM instructions on every branch in every loop.
	condFrom []int
	// retExpr is the expression the fall-through return prints instead of
	// naming the result slot, when the last thing the function did was compute
	// it. Worth its own field because it is one VM move per call, and a call is
	// the one thing a recursive guest does more of than anything else.
	retExpr string
}

type pendingFwd struct {
	def      int    // step that produced the value
	expr     string // embeddable Lua text: a name, a numeral, a call, or bracketed
	raw      string // the same text with no outer brackets
	localIdx int32  // wasm local read, or -1 for a constant (-opt=0 only)
	konst    *uint32
	dup      bool
	traps    bool
	depth    int
	deps     fwdDeps
}

// fwdDeps is everything a pending expression reads. A pending dies as soon as
// anything it reads is written, which is what lets it be moved at all.
type fwdDeps struct {
	slots   map[ir.Slot]bool
	locals  map[uint32]bool
	globals map[uint32]bool
	mem     bool
}

func (d *fwdDeps) addSlot(s ir.Slot) {
	if d.slots == nil {
		d.slots = map[ir.Slot]bool{}
	}
	d.slots[s] = true
}

func (d *fwdDeps) merge(o fwdDeps) {
	for s := range o.slots {
		d.addSlot(s)
	}
	for l := range o.locals {
		if d.locals == nil {
			d.locals = map[uint32]bool{}
		}
		d.locals[l] = true
	}
	for g := range o.globals {
		if d.globals == nil {
			d.globals = map[uint32]bool{}
		}
		d.globals[g] = true
	}
	d.mem = d.mem || o.mem
}

// maxFwdDepth caps how deeply expressions nest.
//
// Lua's parser is recursive and capped at LUAI_MAXCCALLS = 200 nesting levels,
// and every nesting level also costs a VM register, on top of the up-to-180
// locals a function already declares against a 255-register ceiling. Eight is
// past where the win is: a wasm expression tree deeper than that is rare, and
// the first few levels carry nearly all of the benefit.
const maxFwdDepth = 8

func forward(b *builder, f *ir.Func) *forwarding {
	if b != nil && b.opt.Peephole() {
		return forwardPeephole(b, f)
	}
	return forwardM4(b, f)
}

// forwardM4 is the -opt=0 rule, kept verbatim so that level 0 is a genuine
// reference implementation rather than an approximation of one.
func forwardM4(b *builder, f *ir.Func) *forwarding {
	fw := newForwarding(f)
	avail := map[ir.Slot]*pendingFwd{}

	for i := range f.Steps {
		s := &f.Steps[i]

		// Resolve operands first: a result may reuse an operand's slot, so
		// reading has to happen before writing.
		for k, slot := range s.Args {
			if p, ok := avail[slot]; ok {
				fw.args[i][k] = p.expr
				fw.raw[i][k] = p.raw
				fw.konst[i][k] = p.konst
				fw.elided[p.def] = true
				delete(avail, slot)
			} else {
				fw.args[i][k] = b.slotName(slot)
				fw.raw[i][k] = b.slotName(slot)
			}
		}

		// A write to a local invalidates every pending read of that local.
		switch s.Op {
		case wasm.OpLocalSet, wasm.OpLocalTee:
			for slot, p := range avail {
				if p.localIdx == int32(s.Instr.LocalIndex) {
					delete(avail, slot)
				}
			}
		}

		// Forwarding must not cross a basic-block boundary.
		//
		// A pending value is only safe to substitute if control reaches the
		// consumer from exactly one place. Any control-flow instruction breaks
		// that: a label can be entered from elsewhere, and a branch copies a
		// value into a slot that may still hold a pending constant. That second
		// case is not hypothetical -- it is what
		//
		//     (i32.add (i32.const 4) (br 0 (i32.const 8)))
		//
		// does: the branch writes the block's result slot, which already had
		// `4` pending, and the add after the label would read 4 instead of 8.
		if isControlFlow(s.Op) {
			avail = map[ir.Slot]*pendingFwd{}
			continue
		}

		if s.Dst == ir.NoSlot {
			continue
		}
		delete(avail, s.Dst)

		if s.DstType.Slots() > 1 {
			// A wide value occupies several Lua names; the forwarding table
			// holds one expression, so it cannot represent one.
			continue
		}
		switch s.Op {
		case wasm.OpLocalGet:
			name := b.slotName(f.LocalSlot(s.Instr.LocalIndex))
			avail[s.Dst] = &pendingFwd{
				def:      i,
				expr:     name,
				raw:      name,
				localIdx: int32(s.Instr.LocalIndex),
				dup:      true,
			}
		case wasm.OpI32Const:
			v := s.Instr.I32
			avail[s.Dst] = &pendingFwd{
				def:      i,
				expr:     u32(v),
				raw:      u32(v),
				localIdx: -1,
				konst:    &v,
				dup:      true,
			}
		}
	}
	return fw
}

func newForwarding(f *ir.Func) *forwarding {
	fw := &forwarding{
		elided:   make([]bool, len(f.Steps)),
		args:     make([][]string, len(f.Steps)),
		raw:      make([][]string, len(f.Steps)),
		dupable:  make([][]bool, len(f.Steps)),
		konst:    make([][]*uint32, len(f.Steps)),
		condFrom: make([]int, len(f.Steps)),
	}
	for i := range f.Steps {
		n := len(f.Steps[i].Args)
		fw.args[i] = make([]string, n)
		fw.raw[i] = make([]string, n)
		fw.dupable[i] = make([]bool, n)
		fw.konst[i] = make([]*uint32, n)
		fw.condFrom[i] = -1
		for k := range fw.dupable[i] {
			fw.dupable[i][k] = true
		}
	}
	return fw
}

// forwardPeephole is the -opt=1 rule: any step that lowers to one expression is
// a candidate.
func forwardPeephole(b *builder, f *ir.Func) *forwarding {
	fw := newForwarding(f)
	avail := map[ir.Slot]*pendingFwd{}

	for i := range f.Steps {
		s := &f.Steps[i]

		// A promoted load reads a Lua local, so it neither traps nor depends on
		// memory -- which is the difference between a promoted store/load pair
		// collapsing into one expression and it not.
		promoted, isPromoted := b.fr.LoadAt(i)

		// One trap per emitted expression. Lua does not fix the evaluation
		// order of an operator's operands, so two operations that can trap
		// inside one expression would make WHICH trap the guest sees depend on
		// something the language does not promise.
		//
		// A division whose divisor is a known non-zero constant is not one of
		// them any more: its lowering is arithmetic, with no zero check to
		// fail. Asking here rather than inside stepTraps keeps that op-only
		// function op-only -- the answer depends on the level and on the range
		// analysis, exactly as the choice of lowering does, and the two have to
		// agree or an expression that traps would be forwarded as one that
		// cannot. They agree because both ask constDivisor/constDivisorS.
		trapUsed := stepTraps(s.Op) && !isPromoted && !b.constDivIsNative(i, s.Op)
		deps := fwdDeps{}
		maxDepth := 0
		from := make([]int, len(s.Args))
		for k := range from {
			from[k] = -1
		}

		for k, slot := range s.Args {
			fw.args[i][k] = b.slotName(slot)
			fw.raw[i][k] = b.slotName(slot)
			// Every slot the operand occupies, not just its base. An i64
			// operand is a (lo, hi) pair, and a pending expression naming both
			// halves has to die when EITHER is overwritten -- which is exactly
			// what happens when the high half is reused by the next stack
			// value, and it produced a wrong answer rather than a crash.
			for n := 0; n < argSlots(s, k); n++ {
				deps.addSlot(slot + ir.Slot(n))
			}

			p, ok := avail[slot]
			if !ok {
				continue
			}
			if p.traps && trapUsed {
				continue
			}
			if p.traps && mayNotEvaluate(s.Op, k) {
				continue
			}
			if !p.dup && duplicatesOperand(s.Op, k) {
				continue
			}
			if p.depth+1 > maxFwdDepth {
				continue
			}
			fw.args[i][k] = p.expr
			fw.raw[i][k] = p.raw
			fw.dupable[i][k] = p.dup
			fw.konst[i][k] = p.konst
			fw.elided[p.def] = true
			from[k] = p.def
			delete(avail, slot)
			trapUsed = trapUsed || p.traps
			deps.merge(p.deps)
			if p.depth > maxDepth {
				maxDepth = p.depth
			}
		}

		// Fold a comparison into the branch that consumes it, which is what
		// turns two statements and a slot write into one `if a < b then`.
		if j, ok := foldedCond(b, f, fw, i, from); ok {
			fw.condFrom[i] = j
		}

		invalidate(avail, f, i)
		if fs, ok := b.fr.StoreAt(i); ok {
			for slot, p := range avail {
				for n := 0; n < fs.Type.Slots(); n++ {
					if p.deps.slots[fs.Base+ir.Slot(n)] {
						delete(avail, slot)
						break
					}
				}
			}
		}

		// The fall-through return is not a step, so the value it hands back
		// has no consumer to be forwarded into. Catch it at the function-level
		// end, which is the only place the straight line reaches it.
		//
		// "Last step" is load-bearing, not belt and braces. A LOOP's end also
		// carries no label -- the label sits at its top, because that is where
		// a branch to a loop lands -- so a check on the label alone matches a
		// loop end too, and hands the fall-through return a value computed one
		// iteration ago.
		if i == len(f.Steps)-1 &&
			s.Op == wasm.OpEnd && s.Label == ir.NoLabel && s.ElseLabel == ir.NoLabel &&
			len(f.Results) == 1 && f.Results[0].Slots() == 1 {
			if p, ok := avail[f.ResultSlot()]; ok {
				fw.retExpr = p.raw
				fw.elided[p.def] = true
			}
		}

		if isControlFlow(s.Op) {
			avail = map[ir.Slot]*pendingFwd{}
			continue
		}
		if s.Dst == ir.NoSlot || s.DstType.Slots() > 1 {
			continue
		}

		e, ok := stepExpr(b, f, i, fw)
		if !ok {
			continue
		}
		own := deps
		switch s.Op {
		case wasm.OpLocalGet:
			own.locals = map[uint32]bool{s.Instr.LocalIndex: true}
		case wasm.OpGlobalGet:
			own.globals = map[uint32]bool{s.Instr.GlobalIndex: true}
		}
		own.mem = own.mem || (readsMemory(s.Op) && !isPromoted)
		if isPromoted {
			for n := 0; n < promoted.Type.Slots(); n++ {
				own.addSlot(promoted.Base + ir.Slot(n))
			}
		}

		avail[s.Dst] = &pendingFwd{
			def:      i,
			expr:     e.sub(),
			raw:      e.text,
			localIdx: -1,
			dup:      e.dup,
			traps:    trapUsed,
			depth:    maxDepth + 1,
			deps:     own,
		}
	}
	return fw
}

// foldedCond reports the comparison step whose boolean can be folded into
// branch step i, having already been substituted as its condition operand.
func foldedCond(b *builder, f *ir.Func, fw *forwarding, i int, from []int) (int, bool) {
	s := &f.Steps[i]
	var k int
	switch s.Op {
	case wasm.OpIf, wasm.OpBrIf:
		k = 0
	case wasm.OpSelect:
		k = 2
	default:
		return 0, false
	}
	// The producer has to be the step that was just substituted as this
	// operand: only then is its own operand text still the right thing to
	// build a boolean out of.
	if k >= len(from) || from[k] < 0 {
		return 0, false
	}
	if _, ok := condExpr(b, f, from[k], fw, false); !ok {
		return 0, false
	}
	return from[k], true
}

// invalidate drops every pending expression that step i could have changed.
func invalidate(avail map[ir.Slot]*pendingFwd, f *ir.Func, i int) {
	s := &f.Steps[i]

	drop := func(keep func(*pendingFwd) bool) {
		for slot, p := range avail {
			if !keep(p) {
				delete(avail, slot)
			}
		}
	}

	switch s.Op {
	case wasm.OpLocalSet, wasm.OpLocalTee:
		idx := s.Instr.LocalIndex
		drop(func(p *pendingFwd) bool { return !p.deps.locals[idx] })
	case wasm.OpGlobalSet:
		idx := s.Instr.GlobalIndex
		drop(func(p *pendingFwd) bool { return !p.deps.globals[idx] })
	}

	if sideEffects(s.Op) {
		// A store or a call can change memory, and a call can change any
		// global. A pending that can trap also dies here: moving it past an
		// effect would reorder the trap against something observable.
		drop(func(p *pendingFwd) bool {
			return !p.deps.mem && len(p.deps.globals) == 0 && !p.traps
		})
	}

	if s.Dst != ir.NoSlot {
		w := s.Dst
		wide := s.DstType.Slots() > 1
		drop(func(p *pendingFwd) bool {
			if p.deps.slots[w] {
				return false
			}
			return !wide || !p.deps.slots[w+1]
		})
		delete(avail, s.Dst)
	}
}

// mayNotEvaluate reports an operand position whose lowering evaluates the
// operand zero times, or only on one arm of a branch.
//
// wasm evaluates every operand exactly once, eagerly. `drop` throwing its
// operand away is the obvious case, and it is not a theoretical one: the
// conformance suite's no_dce tests exist precisely to check that
// `(drop (i32.div_u 1 0))` still traps. `select` is the subtler one -- it
// lowers to an if/else, so an operand substituted into one arm would be
// evaluated only when that arm is taken, while wasm evaluated it either way.
//
// The third case is the one that is easy to get wrong. Every CONSTANT-
// SPECIALISED lowering prints `u32(k)` where operand 1's expression would have
// gone, and so names it nowhere -- multiply by a small constant, the shifts and
// rotates, and each of and/or/xor's identity cases. At -opt>=1 the constant
// comes from the RANGE ANALYSIS rather than from an i32.const, and a trapping
// operand can have an exact range:
//
//	(i32.mul (i32.const 7) (i32.div_u (i32.const 0) (local.get $z)))
//
// div_u's range is [0,0] because its dividend is, so the multiply takes its
// constant path, and forwarding then deletes the divide that was about to trap.
// Refusing the forward costs one statement and nothing else: the divide emits
// its own assignment and traps, and the multiply still folds the constant,
// because the fold reads the range and never read the expression.
//
// Only a trapping expression is refused here. Deleting a pure one is a
// legitimate side effect of forwarding into a drop, and is the only dead-code
// elimination the emitter does.
func mayNotEvaluate(op wasm.Op, k int) bool {
	switch op {
	case wasm.OpDrop:
		return true
	case wasm.OpSelect:
		return k < 2
	case wasm.OpI32And, wasm.OpI32Or:
		// `and` with 0 lowers to the constant 0 and names NEITHER operand, so
		// position 0 goes as well. Refused for every constant rather than just
		// that one, because the cost of the extra caution is one forward.
		return k == 0 || k == 1
	case wasm.OpI32Mul, wasm.OpI32Xor,
		wasm.OpI32Shl, wasm.OpI32ShrU, wasm.OpI32ShrS,
		wasm.OpI32Rotl, wasm.OpI32Rotr:
		return k == 1
	case wasm.OpI32DivU, wasm.OpI32RemU, wasm.OpI32DivS, wasm.OpI32RemS:
		// The constant-divisor lowerings print `u32(k)` where the divisor's
		// expression would have gone. Exactly the class above, and it is the
		// class the ORIGINAL bug was in -- the operand deleted here is itself
		// most often a division, which is the one thing that traps for a reason
		// a range can hide:
		//
		//	(i32.rem_u (local.get $a)
		//	           (i32.add (i32.const 3)
		//	                    (i32.div_u (i32.const 0) (local.get $z))))
		//
		// The add's range is [3,3], so rem_u takes its constant path and never
		// names the add -- and without this line the forwarder would delete the
		// inner divide that was about to trap.
		return k == 1
	}
	return false
}

// stepTraps reports an op whose lowering can raise a wasm trap.
func stepTraps(op wasm.Op) bool {
	switch op {
	case wasm.OpI32DivU, wasm.OpI32DivS, wasm.OpI32RemU, wasm.OpI32RemS,
		wasm.OpI64DivU, wasm.OpI64DivS, wasm.OpI64RemU, wasm.OpI64RemS,
		wasm.OpI32TruncF32S, wasm.OpI32TruncF32U,
		wasm.OpI32TruncF64S, wasm.OpI32TruncF64U,
		wasm.OpI64TruncF32S, wasm.OpI64TruncF32U,
		wasm.OpI64TruncF64S, wasm.OpI64TruncF64U,
		wasm.OpCall, wasm.OpCallIndirect, wasm.OpUnreachable:
		return true
	}
	if isLoad(op) || isStore(op) {
		return true
	}
	return false
}

func isLoad(op wasm.Op) bool {
	switch op {
	case wasm.OpI32Load, wasm.OpI32Load8S, wasm.OpI32Load8U,
		wasm.OpI32Load16S, wasm.OpI32Load16U,
		wasm.OpI64Load, wasm.OpI64Load8S, wasm.OpI64Load8U,
		wasm.OpI64Load16S, wasm.OpI64Load16U, wasm.OpI64Load32S, wasm.OpI64Load32U,
		wasm.OpF32Load, wasm.OpF64Load:
		return true
	}
	return false
}

func isStore(op wasm.Op) bool {
	switch op {
	case wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
		wasm.OpI64Store, wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32,
		wasm.OpF32Store, wasm.OpF64Store:
		return true
	}
	return false
}

// argSlots is how many Lua locals operand k of a step occupies.
func argSlots(s *ir.Step, k int) int {
	if k < len(s.ArgTypes) {
		return s.ArgTypes[k].Slots()
	}
	return 1
}
