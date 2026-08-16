package ir

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/wasm"
)

func steps(t *testing.T, wat string) *Func {
	t.Helper()
	m := build(t, wat)
	return m.Funcs[0]
}

func opsOf(f *Func) []wasm.Op {
	out := make([]wasm.Op, len(f.Steps))
	for i, s := range f.Steps {
		out[i] = s.Op
	}
	return out
}

// A loop's label sits at its TOP, because that is where a branch to a loop
// lands. Defining it again at `end` produced "label already defined" and was a
// real bug caught by the spec suite.
func TestLoopEndCarriesNoLabel(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32)
		(loop (result i32) (i32.const 1))))`)
	var loopStep, endStep *Step
	for i := range f.Steps {
		switch f.Steps[i].Op {
		case wasm.OpLoop:
			loopStep = &f.Steps[i]
		case wasm.OpEnd:
			if endStep == nil {
				endStep = &f.Steps[i]
			}
		}
	}
	if loopStep == nil || loopStep.Label == NoLabel {
		t.Fatal("a loop must define a label at its top")
	}
	if endStep == nil || endStep.Label != NoLabel {
		t.Errorf("a loop's end must not redefine the label, got %v", endStep.Label)
	}
}

func TestBlockEndCarriesTheLabel(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32)
		(block (result i32) (i32.const 1))))`)
	for _, s := range f.Steps {
		if s.Op == wasm.OpEnd && s.Label != NoLabel {
			return
		}
	}
	t.Error("a block's end must define its label")
}

// A branch out of a construct that yields a value has to move the value into
// the construct's result slot first.
func TestBranchCarriesValue(t *testing.T) {
	// Two values are pushed before the branch, so the value being carried is
	// NOT already sitting in the block's result slot -- the copy is real.
	f := steps(t, `(module (func (export "f") (result i32)
		(block (result i32) (i32.const 4) (br 0 (i32.const 8)))))`)
	for _, s := range f.Steps {
		if s.Op == wasm.OpBr {
			if s.Target.From == NoSlot || s.Target.To == NoSlot {
				t.Fatalf("br into a value-yielding block must carry a copy, got %+v", s.Target)
			}
			if s.Target.From == s.Target.To {
				t.Errorf("expected a copy between distinct slots, got %+v", s.Target)
			}
			return
		}
	}
	t.Fatal("no br step emitted")
}

// A loop's label arity is its PARAMETER count, which is zero without
// multi-value, so a branch to a loop carries nothing.
func TestBranchToLoopCarriesNothing(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32)
		(local $i i32)
		(loop (result i32)
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if 0 (i32.lt_u (local.get $i) (i32.const 3)))
			(local.get $i))))`)
	for _, s := range f.Steps {
		if s.Op == wasm.OpBrIf {
			if s.Target.From != NoSlot || s.Target.To != NoSlot {
				t.Errorf("a branch to a loop must carry no value, got %+v", s.Target)
			}
			return
		}
	}
	t.Fatal("no br_if step emitted")
}

// A branch past the outermost construct leaves the function.
func TestBranchToFunctionIsReturn(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32) (br 0 (i32.const 7))))`)
	for _, s := range f.Steps {
		if s.Op == wasm.OpBr {
			if !s.Target.IsReturn() {
				t.Errorf("branch depth 0 at function level must be a return, got %+v", s.Target)
			}
			return
		}
	}
	t.Fatal("no br step emitted")
}

func TestIfGetsBothLabels(t *testing.T) {
	f := steps(t, `(module (func (export "f") (param i32) (result i32)
		(if (result i32) (local.get 0) (then (i32.const 1)) (else (i32.const 2)))))`)
	var ifStep, elseStep *Step
	for i := range f.Steps {
		switch f.Steps[i].Op {
		case wasm.OpIf:
			ifStep = &f.Steps[i]
		case wasm.OpElse:
			elseStep = &f.Steps[i]
		}
	}
	if ifStep == nil || ifStep.Label == NoLabel || ifStep.ElseLabel == NoLabel {
		t.Fatalf("an if needs both an end and an else label, got %+v", ifStep)
	}
	if ifStep.Label == ifStep.ElseLabel {
		t.Error("the else and end labels must differ")
	}
	if elseStep == nil || !elseStep.HasElse {
		t.Error("the else arm should be marked")
	}
	// Once `else` has emitted the else label, `end` must not emit it again.
	for _, s := range f.Steps {
		if s.Op == wasm.OpEnd && s.ElseLabel != NoLabel {
			t.Error("end must not redefine the else label after an explicit else")
		}
	}
}

// An if with no else still needs its else label, or the false branch has
// nowhere to land.
func TestIfWithoutElseKeepsElseLabelForEnd(t *testing.T) {
	f := steps(t, `(module (func (export "f") (param i32)
		(if (local.get 0) (then (nop)))))`)
	found := false
	for _, s := range f.Steps {
		if s.Op == wasm.OpEnd && s.ElseLabel != NoLabel {
			found = true
		}
	}
	if !found {
		t.Error("an if with no else must keep its else label for end to define")
	}
}

// wasm lets the operand stack go polymorphic after an unconditional branch.
// Rather than model that, instructions are skipped until an else or end
// restores a depth fixed by the block type.
func TestUnreachableCodeIsSkipped(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32)
		(block (result i32)
			(br 0 (i32.const 1))
			(i32.const 2)
			(i32.const 3)
			(i32.add))))`)
	adds := 0
	for _, s := range f.Steps {
		if s.Op == wasm.OpI32Add {
			adds++
		}
	}
	if adds != 0 {
		t.Errorf("unreachable instructions should produce no steps, got %d adds", adds)
	}
	if f.NumSlots > MaxSlots {
		t.Errorf("slot budget blown by unreachable code: %d", f.NumSlots)
	}
}

func TestUnreachableThenElseIsReachableAgain(t *testing.T) {
	f := steps(t, `(module (func (export "f") (param i32) (result i32)
		(if (result i32) (local.get 0)
			(then (return (i32.const 1)))
			(else (i32.const 2)))))`)
	// The else arm must still be compiled even though the then arm ended in a
	// return.
	konsts := 0
	for _, s := range f.Steps {
		if s.Op == wasm.OpI32Const {
			konsts++
		}
	}
	if konsts < 2 {
		t.Errorf("the else arm should be compiled, saw %d constants: %v", konsts, opsOf(f))
	}
}

func TestBrTableResolvesEveryTarget(t *testing.T) {
	f := steps(t, `(module (func (export "f") (param i32) (result i32)
		(block (block (block
			(br_table 0 1 2 (local.get 0)))
			(return (i32.const 10)))
			(return (i32.const 20)))
		(i32.const 30)))`)
	for _, s := range f.Steps {
		if s.Op == wasm.OpBrTable {
			// br_table's LAST label is the default, so `br_table 0 1 2` has
			// two indexed targets and 2 as the default.
			if len(s.Targets) != 2 {
				t.Errorf("expected 2 indexed targets, got %d", len(s.Targets))
			}
			if s.Default.Label == NoLabel && !s.Default.IsReturn() {
				t.Error("the default target is unresolved")
			}
			for i, tg := range s.Targets {
				if tg.Label == NoLabel && !tg.IsReturn() {
					t.Errorf("target %d unresolved", i)
				}
			}
			return
		}
	}
	t.Fatal("no br_table step emitted")
}

func TestCallResolvesArityFromCallee(t *testing.T) {
	f := steps(t, `(module
		(func $add (param i32) (param i32) (result i32) (i32.add (local.get 0) (local.get 1)))
		(func (export "f") (result i32) (call $add (i32.const 1) (i32.const 2))))`)
	// f is index 1.
	m := build(t, `(module
		(func $add (param i32) (param i32) (result i32) (i32.add (local.get 0) (local.get 1)))
		(func (export "f") (result i32) (call $add (i32.const 1) (i32.const 2))))`)
	f = m.Funcs[1]
	for _, s := range f.Steps {
		if s.Op == wasm.OpCall {
			if len(s.Args) != 2 {
				t.Errorf("call should consume 2 arguments, got %d", len(s.Args))
			}
			if s.Results != 1 {
				t.Errorf("call should produce 1 result, got %d", s.Results)
			}
			if s.Dst == NoSlot {
				t.Error("a result-producing call needs a destination slot")
			}
			return
		}
	}
	t.Fatal("no call step emitted")
}

// call_indirect pops the table index ABOVE its arguments.
func TestCallIndirectPopsTableIndexLast(t *testing.T) {
	m := build(t, `(module
		(type $t (func (param i32) (result i32)))
		(table 1 funcref)
		(func (export "f") (param i32) (result i32)
			(call_indirect (type $t) (i32.const 5) (local.get 0))))`)
	for _, s := range m.Funcs[0].Steps {
		if s.Op == wasm.OpCallIndirect {
			if len(s.Args) != 2 {
				t.Fatalf("expected 1 argument plus the table index, got %d", len(s.Args))
			}
			if s.Args[1] <= s.Args[0] {
				t.Error("the table index must be the topmost operand")
			}
			return
		}
	}
	t.Fatal("no call_indirect step emitted")
}

func TestGlobalIndexIsValidated(t *testing.T) {
	f := &wasm.Func{
		Name: "g", Type: wasm.FuncType{Results: []wasm.ValType{wasm.I32}},
		Body: []wasm.Instr{{Op: wasm.OpGlobalGet, GlobalIndex: 3}, {Op: wasm.OpEnd}},
	}
	_, err := Build(f, &wasm.Module{})
	if err == nil || !strings.Contains(err.Error(), "global index 3") {
		t.Errorf("an out-of-range global should be rejected by name, got %v", err)
	}
}

func TestMemoryOpsHaveCorrectArity(t *testing.T) {
	m := build(t, `(module (memory 1)
		(func (export "f") (param i32) (result i32)
			(i32.store (local.get 0) (i32.const 7))
			(i32.load (local.get 0))))`)
	var store, load *Step
	for i := range m.Funcs[0].Steps {
		switch m.Funcs[0].Steps[i].Op {
		case wasm.OpI32Store:
			store = &m.Funcs[0].Steps[i]
		case wasm.OpI32Load:
			load = &m.Funcs[0].Steps[i]
		}
	}
	if store == nil || len(store.Args) != 2 || store.Dst != NoSlot {
		t.Errorf("i32.store takes address+value and yields nothing, got %+v", store)
	}
	if load == nil || len(load.Args) != 1 || load.Dst == NoSlot {
		t.Errorf("i32.load takes an address and yields a value, got %+v", load)
	}
}

func TestUnsupportedFunctionSkipsBuild(t *testing.T) {
	m := build(t, `(module (memory 1) (table 2 funcref) (func (export "f") (table.copy (i32.const 0) (i32.const 1) (i32.const 1))))`)
	f := m.Funcs[0]
	if f.Unsupported == nil {
		t.Fatal("expected the function to be marked unsupported")
	}
	if len(f.Steps) != 0 {
		t.Error("an unsupported function should have no steps")
	}
}

func TestModuleCarriesItsSource(t *testing.T) {
	m := build(t, `(module (memory 1) (global $g i32 (i32.const 5))
		(func (export "f") (result i32) (global.get $g)))`)
	if m.Source == nil {
		t.Fatal("the resolved module must keep its source for the emitter")
	}
	if !m.Source.Memory.Has {
		t.Error("memory declaration lost")
	}
	if len(m.Source.Globals) != 1 || m.Source.Globals[0].InitBits != 5 {
		t.Errorf("global initialiser lost: %+v", m.Source.Globals)
	}
}

// The slot allocator is width-aware: an i64 occupies two Lua slots where every
// other type occupies one, so "stack depth -> slot" is no longer the identity
// map. These pin the layout before any i64 opcode exists to exercise it.
func TestSlotWidths(t *testing.T) {
	if wasm.I64.Slots() != 2 {
		t.Errorf("i64 must occupy 2 slots, got %d", wasm.I64.Slots())
	}
	for _, v := range []wasm.ValType{wasm.I32, wasm.F32, wasm.F64} {
		if v.Slots() != 1 {
			t.Errorf("%s must occupy 1 slot, got %d", v, v.Slots())
		}
	}
}

// With only single-slot types present the layout must be exactly what it was
// before the rework, or the 13,031 passing assertions would have moved.
func TestLocalSlotsAreIdentityWithoutI64(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (param f64) (result i32)
		(local $a i32) (local $b f32)
		(local.get 0)))`)
	f := m.Funcs[0]
	for i := range f.LocalSlots {
		if f.LocalSlots[i] != Slot(i) {
			t.Errorf("LocalSlots = %v; with no i64 it should be the identity map",
				f.LocalSlots)
			break
		}
	}
	if f.ResultSlot() != 4 {
		t.Errorf("stack should start at 4 (2 params + 2 locals), got %d", f.ResultSlot())
	}
}

// Steps must carry operand and result types, or the emitter cannot tell how
// many Lua names a value occupies.
func TestStepsCarryTypes(t *testing.T) {
	m := build(t, `(module (func (export "f") (param f64) (param f64) (result f64)
		(f64.add (local.get 0) (local.get 1))))`)
	for _, s := range m.Funcs[0].Steps {
		if s.Op != wasm.OpF64Add {
			continue
		}
		if len(s.ArgTypes) != 2 || s.ArgTypes[0] != wasm.F64 || s.ArgTypes[1] != wasm.F64 {
			t.Errorf("ArgTypes = %v, want [f64 f64]", s.ArgTypes)
		}
		if s.DstType != wasm.F64 {
			t.Errorf("DstType = %v, want f64", s.DstType)
		}
		return
	}
	t.Fatal("no f64.add step")
}

// A branch carries its value's type so the copy moves every slot the value
// occupies, not just the first.
func TestBranchCarriesType(t *testing.T) {
	m := build(t, `(module (func (export "f") (result f64)
		(block (result f64) (f64.const 1) (br 0 (f64.const 2)))))`)
	for _, s := range m.Funcs[0].Steps {
		if s.Op == wasm.OpBr {
			if s.Target.Typ != wasm.F64 {
				t.Errorf("Target.Typ = %v, want f64", s.Target.Typ)
			}
			return
		}
	}
	t.Fatal("no br step")
}

// A block's result type has to reach the IR, or the result slot is sized wrong.
func TestBlockResultTypeIsRecorded(t *testing.T) {
	m := build(t, `(module (func (export "f") (result f64)
		(block (result f64) (f64.const 1))))`)
	for _, s := range m.Funcs[0].Steps {
		if s.Op == wasm.OpBlock {
			if s.Instr.BlockResults != 1 || s.Instr.BlockType != wasm.F64 {
				t.Errorf("block result = %d/%v, want 1/f64",
					s.Instr.BlockResults, s.Instr.BlockType)
			}
			return
		}
	}
	t.Fatal("no block step")
}

// All four numeric types are legal global initialisers. Rejecting f32/f64 used
// to fail the WHOLE module, which is what made unreachable.wast unrunnable
// despite having nothing to do with globals.
func TestGlobalInitialisersOfEveryType(t *testing.T) {
	m := build(t, `(module
		(global $a (mut i32) (i32.const 7))
		(global $b (mut i64) (i64.const 4294967297))
		(global $c f32 (f32.const 1.5))
		(global $d f64 (f64.const 2.5))
		(func (export "f") (result i64) (global.get $b)))`)
	g := m.Source.Globals
	if len(g) != 4 {
		t.Fatalf("expected 4 globals, got %d", len(g))
	}
	if g[0].InitBits != 7 {
		t.Errorf("i32 global = %d, want 7", g[0].InitBits)
	}
	// 4294967297 is 0x1_0000_0001: low half 1, high half 1.
	if g[1].InitBits != 0x100000001 {
		t.Errorf("i64 global = %#x, want 0x100000001", g[1].InitBits)
	}
	if g[1].Type != wasm.I64 || g[1].Type.Slots() != 2 {
		t.Errorf("i64 global should occupy 2 slots, got %v", g[1].Type)
	}
	if g[2].Type != wasm.F32 || g[3].Type != wasm.F64 {
		t.Errorf("float global types lost: %v, %v", g[2].Type, g[3].Type)
	}
}

// global.get must be typed by the global it reads, so the emitter knows how
// many Lua names to move.
func TestGlobalGetIsTypedByTheGlobal(t *testing.T) {
	m := build(t, `(module (global $g (mut i64) (i64.const 1))
		(func (export "f") (result i64) (global.get $g)))`)
	for _, s := range m.Funcs[0].Steps {
		if s.Op == wasm.OpGlobalGet {
			if s.DstType != wasm.I64 {
				t.Errorf("DstType = %v, want i64", s.DstType)
			}
			return
		}
	}
	t.Fatal("no global.get step")
}
