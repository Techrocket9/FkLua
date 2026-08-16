// Package ir turns a decoded wasm function into a form the Lua emitter can
// print directly: every operand resolved to a concrete Lua local slot, every
// branch resolved to a label, and the total slot count known before a single
// line is emitted.
//
// The central idea is that wasm validation already pins the operand stack's
// height and type at every program point. That makes "wasm stack position ->
// Lua slot" a pure compile-time function, so the emitted code needs no runtime
// stack, no tagging, and no dynamic dispatch -- just assignments between named
// locals and gotos between labels.
package ir

import (
	"fmt"

	"github.com/Techrocket9/fklua/internal/wasm"
)

// MaxSlots is the ceiling on declared Lua locals per function.
//
// Lua 5.2 hard-caps a function at 200 locals (measured: 199 ok, 200 ok, 201
// rejected) and 255 registers, where registers also cover expression
// temporaries and call-argument setup. 180 leaves headroom for those without
// having to model Lua's register allocator.
const MaxSlots = 180

// Slot is an index into the function's flat local space. Slots are laid out as
// params, then declared locals, then one per operand-stack depth.
type Slot int

// NoSlot marks the absence of a slot.
const NoSlot Slot = -1

// Label is a branch target. Labels are function-scoped: the emitter writes
// every one at the top level of the function body, never inside a nested Lua
// block, because Lua rejects a goto into a sibling block ("no visible label
// 'x' for <goto>"). Flat emission is what makes every label reachable from
// every branch, and it is also why control flow cannot use nested while/break.
type Label int

// NoLabel marks the absence of a label.
const NoLabel Label = -1

// Branch is one outgoing edge: where to jump, and the value copy it carries.
// wasm branches carry operands, so an edge into a construct that yields a value
// has to move that value into the construct's result slot first.
type Branch struct {
	Label Label
	// From and To describe the value copy, or NoSlot when the edge carries
	// nothing. A branch to a loop never carries a value here, because a loop's
	// label arity is its PARAMETER count, and blocks take no parameters without
	// multi-value.
	From Slot
	To   Slot
	// Typ is the carried value's type, and so how many slots the copy moves.
	Typ wasm.ValType
}

// IsReturn reports an edge that leaves the function rather than jumping.
func (b Branch) IsReturn() bool { return b.Label == NoLabel }

// Step is one instruction with its operands already resolved to slots.
type Step struct {
	Op    wasm.Op
	Instr wasm.Instr

	// Args are the operand slot BASES in wasm order: for `a - b`, Args[0] holds
	// a. A value wider than one slot occupies Args[k] and the slot after it,
	// which is why ArgTypes travels alongside.
	Args []Slot
	// ArgTypes gives each operand's type, and so its width.
	ArgTypes []wasm.ValType

	// Dst is the slot base receiving the result, or NoSlot.
	Dst Slot
	// DstType is the result's type, and so its width.
	DstType wasm.ValType
	// ResultTypes lists a call's result types, in order.
	ResultTypes []wasm.ValType

	// StackDepth is the operand-stack height before this instruction.
	StackDepth int

	// Label is this construct's own label, for block/loop/if/else/end.
	Label Label
	// ElseLabel is where an `if` jumps when its condition is false.
	ElseLabel Label
	// HasElse marks the else step itself.
	HasElse bool

	// Target is the edge taken by br and br_if.
	Target Branch
	// Targets and Default are br_table's edges. The spec requires every target
	// of a br_table to share one arity, so all edges carry the same copy.
	Targets []Branch
	Default Branch

	// Callee is the function index for a direct call; CallType is the signature
	// index for call_indirect; Results is how many values the call returns.
	Callee   uint32
	CallType uint32
	Results  int
}

// Func is a function ready for emission.
type Func struct {
	Name    string
	Index   uint32
	Params  []wasm.ValType
	Results []wasm.ValType
	Locals  []wasm.ValType

	Steps []Step

	// NumSlots is how many Lua locals the function needs in total. The emitter
	// declares exactly this many in the prologue and never declares another --
	// see Invariant B.
	NumSlots int

	// MaxStack is the peak operand-stack depth.
	MaxStack int

	// NumLabels is how many labels the body uses.
	NumLabels int

	// Mod is the decoded module, so the emitter can resolve a global's type
	// (and hence its width) from any function.
	Mod *wasm.Module

	// LocalSlots is the slot base of each wasm local. It stops being the
	// identity map as soon as an i64 local appears, since that consumes two.
	LocalSlots []Slot

	// Unsupported is non-nil when the function could not be compiled. It still
	// appears in the module and is still callable; calling it raises.
	Unsupported error
}

// LocalSlot returns the slot base holding wasm local i.
func (f *Func) LocalSlot(i uint32) Slot {
	if int(i) < len(f.LocalSlots) {
		return f.LocalSlots[i]
	}
	return NoSlot
}

// LocalType returns the declared type of wasm local i.
func (f *Func) LocalType(i uint32) wasm.ValType {
	if int(i) < len(f.Params) {
		return f.Params[i]
	}
	return f.Locals[int(i)-len(f.Params)]
}

// ResultSlot is where a returning function leaves its value.
func (f *Func) ResultSlot() Slot { return f.stackBase() }

// stackBase is the first slot used for operand-stack values: past the params
// and declared locals, which occupy more slots than their count once an i64
// appears.
func (f *Func) stackBase() Slot {
	n := Slot(0)
	for _, p := range f.Params {
		n += Slot(p.Slots())
	}
	for _, l := range f.Locals {
		n += Slot(l.Slots())
	}
	return n
}

// TooManySlotsError reports a function that cannot be made to fit Lua's local
// limit even after spilling.
//
// Since M5 the emitter moves the coldest slots to a chunk-level frame stack, so
// this is no longer raised for a function with many temporaries. What remains
// is the one case spilling cannot help: a function whose PARAMETERS alone
// exceed the budget, because a parameter is a Lua local by virtue of being in
// the parameter list and there is nowhere else for the caller to put it.
type TooManySlotsError struct {
	Func     string
	Needed   int
	Max      int
	Params   int
	Locals   int
	MaxStack int
}

func (e *TooManySlotsError) Error() string {
	return fmt.Sprintf(
		"function %q needs %d Lua locals (%d params + %d locals + %d stack slots) "+
			"but the budget is %d, and its parameters alone do not leave enough "+
			"room for the frame stack to spill into",
		e.Func, e.Needed, e.Params, e.Locals, e.MaxStack, e.Max)
}

// StackError reports an operand-stack or control-stack inconsistency. watgo
// validates before we get here, so hitting this means our own model is wrong,
// not the input.
type StackError struct {
	Func   string
	Offset int
	Op     wasm.Op
	Detail string
}

func (e *StackError) Error() string {
	return fmt.Sprintf("function %q offset %d (%s): %s", e.Func, e.Offset, e.Op, e.Detail)
}

// arity is how many operands an op pops and pushes. Ops whose effect depends on
// context -- control flow and calls -- are handled directly in the builder.
type arity struct {
	pops   int
	pushes int
	// result is the pushed value's type. A few ops (local.get, global.get,
	// select) depend on context instead and are patched up by the builder.
	result wasm.ValType
}

func arityOf(op wasm.Op) (arity, bool) {
	switch op {
	case wasm.OpNop:
		return arity{}, true
	case wasm.OpDrop, wasm.OpLocalSet, wasm.OpGlobalSet:
		return arity{pops: 1}, true
	case wasm.OpI32Const, wasm.OpMemorySize:
		return arity{pushes: 1, result: wasm.I32}, true
	case wasm.OpI64Const:
		return arity{pushes: 1, result: wasm.I64}, true
	case wasm.OpLocalGet, wasm.OpGlobalGet:
		// Type comes from the local or global; the builder fills it in.
		return arity{pushes: 1}, true
	case wasm.OpF32Const:
		return arity{pushes: 1, result: wasm.F32}, true
	case wasm.OpF64Const:
		return arity{pushes: 1, result: wasm.F64}, true
	case wasm.OpLocalTee:
		return arity{pops: 1, pushes: 1}, true
	// i64 -> i64
	case wasm.OpI64Clz, wasm.OpI64Ctz, wasm.OpI64Popcnt, wasm.OpI64Extend8S, wasm.OpI64Extend16S, wasm.OpI64Extend32S,
		wasm.OpI64Load, wasm.OpI64Load8S, wasm.OpI64Load8U,
		wasm.OpI64Load16S, wasm.OpI64Load16U, wasm.OpI64Load32S, wasm.OpI64Load32U,
		wasm.OpI64ExtendI32S, wasm.OpI64ExtendI32U,
		wasm.OpI64TruncF32S, wasm.OpI64TruncF32U,
		wasm.OpI64TruncF64S, wasm.OpI64TruncF64U,
		wasm.OpI64ReinterpretF64,
		wasm.OpI64TruncSatF32S, wasm.OpI64TruncSatF32U,
		wasm.OpI64TruncSatF64S, wasm.OpI64TruncSatF64U:
		return arity{pops: 1, pushes: 1, result: wasm.I64}, true
	// i64 -> i32
	case wasm.OpI64Eqz, wasm.OpI32WrapI64:
		return arity{pops: 1, pushes: 1, result: wasm.I32}, true
	case wasm.OpF32ConvertI64S, wasm.OpF32ConvertI64U:
		return arity{pops: 1, pushes: 1, result: wasm.F32}, true
	case wasm.OpF64ConvertI64S, wasm.OpF64ConvertI64U, wasm.OpF64ReinterpretI64:
		return arity{pops: 1, pushes: 1, result: wasm.F64}, true
	case wasm.OpF32Abs, wasm.OpF32Neg, wasm.OpF32Ceil, wasm.OpF32Floor,
		wasm.OpF32Trunc, wasm.OpF32Nearest, wasm.OpF32Sqrt,
		wasm.OpF32ConvertI32S, wasm.OpF32ConvertI32U,
		wasm.OpF32DemoteF64, wasm.OpF32ReinterpretI32, wasm.OpF32Load:
		return arity{pops: 1, pushes: 1, result: wasm.F32}, true
	case wasm.OpF64Abs, wasm.OpF64Neg, wasm.OpF64Ceil, wasm.OpF64Floor,
		wasm.OpF64Trunc, wasm.OpF64Nearest, wasm.OpF64Sqrt,
		wasm.OpF64ConvertI32S, wasm.OpF64ConvertI32U,
		wasm.OpF64PromoteF32, wasm.OpF64Load:
		return arity{pops: 1, pushes: 1, result: wasm.F64}, true
	case wasm.OpMemoryGrow,
		wasm.OpI32Clz, wasm.OpI32Ctz, wasm.OpI32Popcnt,
		wasm.OpI32Extend8S, wasm.OpI32Extend16S, wasm.OpI32Eqz,
		wasm.OpI32Load, wasm.OpI32Load8S, wasm.OpI32Load8U,
		wasm.OpI32Load16S, wasm.OpI32Load16U,
		wasm.OpI32TruncF32S, wasm.OpI32TruncF32U,
		wasm.OpI32TruncF64S, wasm.OpI32TruncF64U,
		wasm.OpI32ReinterpretF32,
		wasm.OpI32TruncSatF32S, wasm.OpI32TruncSatF32U,
		wasm.OpI32TruncSatF64S, wasm.OpI32TruncSatF64U:
		return arity{pops: 1, pushes: 1, result: wasm.I32}, true
	case wasm.OpMemoryCopy, wasm.OpMemoryFill:
		// dest, src/value, len -- and nothing back.
		return arity{pops: 3}, true
	case wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
		wasm.OpF32Store, wasm.OpF64Store,
		wasm.OpI64Store, wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32:
		return arity{pops: 2}, true
	case wasm.OpI64Add, wasm.OpI64Sub, wasm.OpI64Mul, wasm.OpI64DivS, wasm.OpI64DivU, wasm.OpI64RemS, wasm.OpI64RemU, wasm.OpI64And, wasm.OpI64Or, wasm.OpI64Xor, wasm.OpI64Shl, wasm.OpI64ShrS, wasm.OpI64ShrU, wasm.OpI64Rotl, wasm.OpI64Rotr:
		return arity{pops: 2, pushes: 1, result: wasm.I64}, true
	case wasm.OpI64Eq, wasm.OpI64Ne, wasm.OpI64LtS, wasm.OpI64LtU, wasm.OpI64GtS, wasm.OpI64GtU, wasm.OpI64LeS, wasm.OpI64LeU, wasm.OpI64GeS, wasm.OpI64GeU:
		return arity{pops: 2, pushes: 1, result: wasm.I32}, true
	case wasm.OpF32Add, wasm.OpF32Sub, wasm.OpF32Mul, wasm.OpF32Div,
		wasm.OpF32Min, wasm.OpF32Max, wasm.OpF32Copysign:
		return arity{pops: 2, pushes: 1, result: wasm.F32}, true
	case wasm.OpF64Add, wasm.OpF64Sub, wasm.OpF64Mul, wasm.OpF64Div,
		wasm.OpF64Min, wasm.OpF64Max, wasm.OpF64Copysign:
		return arity{pops: 2, pushes: 1, result: wasm.F64}, true
	case wasm.OpSelect:
		// Result type is operand 0's; the builder fills it in.
		return arity{pops: 3, pushes: 1}, true
	case wasm.OpI32Add, wasm.OpI32Sub, wasm.OpI32Mul,
		wasm.OpI32DivS, wasm.OpI32DivU, wasm.OpI32RemS, wasm.OpI32RemU,
		wasm.OpI32And, wasm.OpI32Or, wasm.OpI32Xor,
		wasm.OpI32Shl, wasm.OpI32ShrS, wasm.OpI32ShrU,
		wasm.OpI32Rotl, wasm.OpI32Rotr,
		wasm.OpI32Eq, wasm.OpI32Ne,
		wasm.OpI32LtS, wasm.OpI32LtU, wasm.OpI32LeS, wasm.OpI32LeU,
		wasm.OpI32GtS, wasm.OpI32GtU, wasm.OpI32GeS, wasm.OpI32GeU,
		wasm.OpF32Eq, wasm.OpF32Ne, wasm.OpF32Lt, wasm.OpF32Gt,
		wasm.OpF32Le, wasm.OpF32Ge,
		wasm.OpF64Eq, wasm.OpF64Ne, wasm.OpF64Lt, wasm.OpF64Gt,
		wasm.OpF64Le, wasm.OpF64Ge:
		return arity{pops: 2, pushes: 1, result: wasm.I32}, true
	}
	return arity{}, false
}

// ctrl is one entry on the control stack.
// ctrl is one entry on the control stack.
type ctrl struct {
	op        wasm.Op // OpBlock, OpLoop or OpIf
	label     Label   // where a branch to this construct lands
	elseLabel Label
	// entryLen and entrySlot capture the operand stack on entry, so `end` and
	// `else` can restore it exactly. entrySlot is where a result value lands.
	entryLen  int
	entrySlot Slot
	results   int
	resultTyp wasm.ValType
	// dead marks a construct entered while already unreachable, so its `end`
	// produces no label and does not restore reachability.
	dead bool
}

// entry is one value on the operand stack.
type entry struct {
	typ  wasm.ValType
	slot Slot
}

type builder struct {
	f   *Func
	mod *wasm.Module

	// stack is the TYPED operand stack. A slot base is derived from cumulative
	// width, not from depth, because an i64 occupies two slots while everything
	// else occupies one.
	stack []entry
	// top is the next free slot; maxSlot is its high-water mark.
	top     Slot
	base    Slot
	maxSlot Slot

	ctrl    []ctrl
	nextLbl Label
	// unreachable is set after an unconditional branch. wasm lets the operand
	// stack go polymorphic there, so instructions are skipped until an else or
	// end restores a shape fixed by the block type.
	unreachable bool
}

func (b *builder) newLabel() Label {
	l := b.nextLbl
	b.nextLbl++
	return l
}

// push places a value of type t on the operand stack and returns its slot base.
func (b *builder) push(t wasm.ValType) Slot {
	s := b.top
	b.stack = append(b.stack, entry{typ: t, slot: s})
	b.top += Slot(t.Slots())
	if b.top > b.maxSlot {
		b.maxSlot = b.top
	}
	return s
}

// pop removes the top value.
func (b *builder) pop() entry {
	e := b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	b.top = e.slot
	return e
}

func (b *builder) depth() int { return len(b.stack) }

// peek returns the value n places below the top, where 0 is the top.
func (b *builder) peek(n int) entry { return b.stack[len(b.stack)-1-n] }

// truncate restores the stack to a known length and slot position, which is how
// `else` and `end` recover a shape the block type pins down.
func (b *builder) truncate(length int, slot Slot) {
	if length < len(b.stack) {
		b.stack = b.stack[:length]
	}
	b.top = slot
}

// branchTo resolves a relative label depth into an edge, including any value
// copy the edge carries. A depth equal to the control-stack height targets the
// function itself, which is a return.
func (b *builder) branchTo(relDepth uint32, off int, op wasm.Op) (Branch, error) {
	i := len(b.ctrl) - 1 - int(relDepth)
	if i < 0 {
		if int(relDepth) == len(b.ctrl) {
			br := Branch{Label: NoLabel, From: NoSlot, To: NoSlot}
			if len(b.f.Results) > 0 && b.depth() > 0 {
				br.From = b.peek(0).slot
				br.To = b.f.ResultSlot()
				br.Typ = b.peek(0).typ
			}
			return br, nil
		}
		return Branch{}, b.stackErr(off, op, fmt.Sprintf(
			"branch depth %d exceeds the control stack (%d deep)", relDepth, len(b.ctrl)))
	}
	c := b.ctrl[i]
	br := Branch{Label: c.label, From: NoSlot, To: NoSlot}
	// A branch to a loop jumps to its head and carries the loop's PARAMETERS, of
	// which there are none without multi-value. Only block and if carry a result.
	if c.op != wasm.OpLoop && c.results > 0 {
		if b.depth() < 1 {
			return Branch{}, b.stackErr(off, op,
				"branch carries a value but the stack is empty")
		}
		br.From = b.peek(0).slot
		br.To = c.entrySlot
		br.Typ = c.resultTyp
	}
	return br, nil
}

// Build resolves a decoded function into slot- and label-addressed steps.
func Build(f *wasm.Func, mod *wasm.Module) (*Func, error) {
	out := &Func{
		Name: f.Name, Index: f.Index,
		Params: f.Type.Params, Results: f.Type.Results,
		Locals: f.Locals,
	}
	if f.Unsupported != nil {
		out.Unsupported = f.Unsupported
		return out, nil
	}

	// Lay out locals first. This is no longer the identity map: an i64 local
	// consumes two slots, so every later local shifts.
	next := Slot(0)
	for _, p := range out.Params {
		out.LocalSlots = append(out.LocalSlots, next)
		next += Slot(p.Slots())
	}
	for _, l := range out.Locals {
		out.LocalSlots = append(out.LocalSlots, next)
		next += Slot(l.Slots())
	}

	out.Mod = mod
	b := &builder{f: out, mod: mod, base: next, top: next, maxSlot: next}
	for i, in := range f.Body {
		if err := b.step(i, in); err != nil {
			return nil, err
		}
	}

	out.MaxStack = int(b.maxSlot - b.base)
	out.NumLabels = int(b.nextLbl)
	out.NumSlots = int(b.maxSlot)
	// No budget check here any more. A function past MaxSlots keeps its hot
	// slots as Lua locals and spills the rest to the chunk-level frame stack --
	// a decision the emitter makes, because it is the thing that knows how many
	// names it is about to declare. See analysis.Plan.
	return out, nil
}

func (b *builder) step(i int, in wasm.Instr) error {
	if b.unreachable {
		return b.skipUnreachable(i, in)
	}

	switch in.Op {
	case wasm.OpBlock, wasm.OpLoop:
		c := ctrl{op: in.Op, label: b.newLabel(), elseLabel: NoLabel,
			entryLen: b.depth(), entrySlot: b.top,
			results: in.BlockResults, resultTyp: in.BlockType}
		b.ctrl = append(b.ctrl, c)
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
			Label: c.label, ElseLabel: NoLabel})
		return nil

	case wasm.OpIf:
		if b.depth() < 1 {
			return b.stackErr(i, in.Op, "if has no condition on the stack")
		}
		cond := b.pop()
		c := ctrl{op: in.Op, label: b.newLabel(), elseLabel: b.newLabel(),
			entryLen: b.depth(), entrySlot: b.top,
			results: in.BlockResults, resultTyp: in.BlockType}
		b.ctrl = append(b.ctrl, c)
		b.emit(Step{Op: in.Op, Instr: in, Args: []Slot{cond.slot},
			ArgTypes: []wasm.ValType{cond.typ}, Dst: NoSlot,
			StackDepth: b.depth(), Label: c.label, ElseLabel: c.elseLabel})
		return nil

	case wasm.OpElse:
		if len(b.ctrl) == 0 || b.ctrl[len(b.ctrl)-1].op != wasm.OpIf {
			return b.stackErr(i, in.Op, "else without a matching if")
		}
		c := &b.ctrl[len(b.ctrl)-1]
		b.truncate(c.entryLen, c.entrySlot)
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
			Label: c.label, ElseLabel: c.elseLabel, HasElse: true})
		c.elseLabel = NoLabel // consumed: `end` must not emit it again
		return nil

	case wasm.OpEnd:
		if len(b.ctrl) == 0 {
			b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
				Label: NoLabel, ElseLabel: NoLabel})
			return nil
		}
		c := b.ctrl[len(b.ctrl)-1]
		b.ctrl = b.ctrl[:len(b.ctrl)-1]
		b.truncate(c.entryLen, c.entrySlot)
		if c.results > 0 {
			b.push(c.resultTyp)
		}
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
			Label: endLabel(c), ElseLabel: c.elseLabel})
		return nil

	case wasm.OpBr:
		br, err := b.branchTo(in.BranchDepth, i, in.Op)
		if err != nil {
			return err
		}
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(), Target: br})
		b.unreachable = true
		return nil

	case wasm.OpBrIf:
		if b.depth() < 1 {
			return b.stackErr(i, in.Op, "br_if has no condition on the stack")
		}
		cond := b.pop()
		br, err := b.branchTo(in.BranchDepth, i, in.Op)
		if err != nil {
			return err
		}
		b.emit(Step{Op: in.Op, Instr: in, Args: []Slot{cond.slot},
			ArgTypes: []wasm.ValType{cond.typ}, Dst: NoSlot,
			StackDepth: b.depth(), Target: br})
		return nil

	case wasm.OpBrTable:
		if b.depth() < 1 {
			return b.stackErr(i, in.Op, "br_table has no index on the stack")
		}
		idx := b.pop()
		targets := make([]Branch, 0, len(in.BranchTable))
		for _, dpt := range in.BranchTable {
			br, err := b.branchTo(dpt, i, in.Op)
			if err != nil {
				return err
			}
			targets = append(targets, br)
		}
		def, err := b.branchTo(in.BranchDefault, i, in.Op)
		if err != nil {
			return err
		}
		b.emit(Step{Op: in.Op, Instr: in, Args: []Slot{idx.slot},
			ArgTypes: []wasm.ValType{idx.typ}, Dst: NoSlot,
			StackDepth: b.depth(), Targets: targets, Default: def})
		b.unreachable = true
		return nil

	case wasm.OpReturn:
		st := Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth()}
		if len(b.f.Results) > 0 {
			if b.depth() < 1 {
				return b.stackErr(i, in.Op, "return has no value on the stack")
			}
			e := b.peek(0)
			st.Args = []Slot{e.slot}
			st.ArgTypes = []wasm.ValType{e.typ}
		}
		b.emit(st)
		b.unreachable = true
		return nil

	case wasm.OpUnreachable:
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth()})
		b.unreachable = true
		return nil

	case wasm.OpCall:
		// The immediate indexes imports and definitions together, so the
		// signature has to be looked up across both. Indexing Funcs directly
		// would silently read the wrong signature in any module with imports.
		ft, ok := b.mod.FuncTypeAt(in.FuncIndex)
		if !ok {
			return b.stackErr(i, in.Op, fmt.Sprintf(
				"call target %d out of range (%d imports + %d functions)",
				in.FuncIndex, len(b.mod.Imports), len(b.mod.Funcs)))
		}
		return b.emitCall(i, in, ft, 0)

	case wasm.OpCallIndirect:
		if int(in.TypeIndex) >= len(b.mod.Types) {
			return b.stackErr(i, in.Op, fmt.Sprintf(
				"type index %d out of range", in.TypeIndex))
		}
		// call_indirect pops the table index above the call arguments.
		return b.emitCall(i, in, b.mod.Types[in.TypeIndex], 1)
	}

	ar, ok := arityOf(in.Op)
	if !ok {
		return b.stackErr(i, in.Op,
			"no arity defined; the decoder accepted an instruction the IR does not model")
	}
	if b.depth() < ar.pops {
		return b.stackErr(i, in.Op, fmt.Sprintf(
			"stack underflow: needs %d operand(s) but depth is %d", ar.pops, b.depth()))
	}

	// Validate index immediates before anything is consumed, so a bad index
	// cannot corrupt the stack model on the way to being reported.
	switch in.Op {
	case wasm.OpLocalGet, wasm.OpLocalSet, wasm.OpLocalTee:
		if int(in.LocalIndex) >= len(b.f.LocalSlots) {
			return b.stackErr(i, in.Op, fmt.Sprintf(
				"local index %d out of range (%d params + %d locals)",
				in.LocalIndex, len(b.f.Params), len(b.f.Locals)))
		}
	case wasm.OpGlobalGet, wasm.OpGlobalSet:
		if int(in.GlobalIndex) >= len(b.mod.Globals) {
			return b.stackErr(i, in.Op, fmt.Sprintf(
				"global index %d out of range (%d declared)",
				in.GlobalIndex, len(b.mod.Globals)))
		}
	}

	st := Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth()}
	if ar.pops > 0 {
		st.Args = make([]Slot, ar.pops)
		st.ArgTypes = make([]wasm.ValType, ar.pops)
		for k := 0; k < ar.pops; k++ {
			e := b.peek(ar.pops - 1 - k)
			st.Args[k] = e.slot
			st.ArgTypes[k] = e.typ
		}
	}

	// The result type of a few ops depends on context rather than the opcode.
	rt := ar.result
	switch in.Op {
	case wasm.OpLocalGet, wasm.OpLocalTee:
		rt = b.f.LocalType(in.LocalIndex)
	case wasm.OpGlobalGet:
		rt = b.mod.Globals[in.GlobalIndex].Type
	case wasm.OpSelect:
		rt = st.ArgTypes[0]
	}

	for k := 0; k < ar.pops; k++ {
		b.pop()
	}
	if ar.pushes > 0 {
		st.Dst = b.push(rt)
		st.DstType = rt
	}
	b.emit(st)
	return nil
}

// emitCall resolves a call's operands. extraPops covers call_indirect's table
// index, which sits above the arguments.
func (b *builder) emitCall(i int, in wasm.Instr, ft wasm.FuncType, extraPops int) error {
	pops := len(ft.Params) + extraPops
	if b.depth() < pops {
		return b.stackErr(i, in.Op, fmt.Sprintf(
			"call needs %d operand(s) but depth is %d", pops, b.depth()))
	}
	st := Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
		Callee: in.FuncIndex, CallType: in.TypeIndex, Results: len(ft.Results)}
	st.Args = make([]Slot, pops)
	st.ArgTypes = make([]wasm.ValType, pops)
	for k := 0; k < pops; k++ {
		e := b.peek(pops - 1 - k)
		st.Args[k] = e.slot
		st.ArgTypes[k] = e.typ
	}
	for k := 0; k < pops; k++ {
		b.pop()
	}
	if len(ft.Results) > 0 {
		st.Dst = b.push(ft.Results[0])
		st.DstType = ft.Results[0]
		st.ResultTypes = append(st.ResultTypes, ft.Results[0])
		for _, r := range ft.Results[1:] {
			b.push(r)
			st.ResultTypes = append(st.ResultTypes, r)
		}
	}
	b.emit(st)
	return nil
}

// skipUnreachable tracks block structure through code the validator has already
// declared unreachable, so the matching else or end restores a known shape.
func (b *builder) skipUnreachable(i int, in wasm.Instr) error {
	switch in.Op {
	case wasm.OpBlock, wasm.OpLoop, wasm.OpIf:
		b.ctrl = append(b.ctrl, ctrl{op: in.Op, label: NoLabel, elseLabel: NoLabel,
			entryLen: b.depth(), entrySlot: b.top,
			results: in.BlockResults, resultTyp: in.BlockType, dead: true})
		return nil

	case wasm.OpElse:
		if len(b.ctrl) == 0 {
			return b.stackErr(i, in.Op, "else without a matching if")
		}
		c := &b.ctrl[len(b.ctrl)-1]
		if c.dead {
			return nil
		}
		// The then-arm ended unreachable; the else-arm is reachable again.
		b.unreachable = false
		b.truncate(c.entryLen, c.entrySlot)
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
			Label: c.label, ElseLabel: c.elseLabel, HasElse: true})
		c.elseLabel = NoLabel
		return nil

	case wasm.OpEnd:
		if len(b.ctrl) == 0 {
			b.unreachable = false
			b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
				Label: NoLabel, ElseLabel: NoLabel})
			return nil
		}
		c := b.ctrl[len(b.ctrl)-1]
		b.ctrl = b.ctrl[:len(b.ctrl)-1]
		if c.dead {
			return nil
		}
		b.unreachable = false
		b.truncate(c.entryLen, c.entrySlot)
		if c.results > 0 {
			b.push(c.resultTyp)
		}
		b.emit(Step{Op: in.Op, Instr: in, Dst: NoSlot, StackDepth: b.depth(),
			Label: endLabel(c), ElseLabel: c.elseLabel})
		return nil
	}
	return nil // anything else in unreachable code produces no output
}

// endLabel is the label an `end` should define.
//
// A loop's label sits at its TOP, because that is where a branch to a loop
// lands; defining it again at the end would be a duplicate, which Lua rejects
// outright ("label 'L0' already defined").
func endLabel(c ctrl) Label {
	if c.op == wasm.OpLoop {
		return NoLabel
	}
	return c.label
}

func (b *builder) emit(s Step) { b.f.Steps = append(b.f.Steps, s) }

func (b *builder) stackErr(off int, op wasm.Op, detail string) error {
	return &StackError{Func: b.f.Name, Offset: off, Op: op, Detail: detail}
}

// Module is a module with every function resolved.
type Module struct {
	Funcs   []*Func
	Exports []wasm.Export
	Source  *wasm.Module
}

// BuildModule resolves every function in a decoded module.
func BuildModule(m *wasm.Module) (*Module, error) {
	out := &Module{Exports: m.Exports, Source: m}
	for i := range m.Funcs {
		f, err := Build(&m.Funcs[i], m)
		if err != nil {
			return nil, err
		}
		out.Funcs = append(out.Funcs, f)
	}
	return out, nil
}
