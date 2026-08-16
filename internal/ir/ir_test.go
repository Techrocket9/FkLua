package ir

import (
	"errors"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/wasm"
)

func build(t *testing.T, wat string) *Module {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := BuildModule(m)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return im
}

// The slot layout is the contract the emitter relies on: params first, then
// declared locals, then one slot per operand-stack depth.
func TestSlotLayout(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(local $a i32)
		(i32.add (local.get 0) (local.get 1))))`)
	f := m.Funcs[0]

	if len(f.Params) != 2 || len(f.Locals) != 1 {
		t.Fatalf("expected 2 params and 1 local, got %d and %d", len(f.Params), len(f.Locals))
	}
	// Params occupy 0..1, the declared local 2, so the stack starts at 3.
	if got := f.stackBase(); got != 3 {
		t.Errorf("stackBase = %d, want 3", got)
	}
	if got := f.LocalSlot(0); got != 0 {
		t.Errorf("LocalSlot(0) = %d, want 0", got)
	}
	if got := f.LocalSlot(2); got != 2 {
		t.Errorf("LocalSlot(2) = %d, want 2 (the declared local)", got)
	}
	if f.MaxStack != 2 {
		t.Errorf("MaxStack = %d, want 2", f.MaxStack)
	}
	if f.NumSlots != 5 {
		t.Errorf("NumSlots = %d, want 5 (2 params + 1 local + 2 stack)", f.NumSlots)
	}
}

// A binary op must read the two slots below the stack top and write its result
// into the lower of them, which is what keeps the stack contiguous.
func TestBinaryOpSlotResolution(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(i32.add (local.get 0) (local.get 1))))`)
	f := m.Funcs[0]

	// local.get, local.get, i32.add, end
	if len(f.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(f.Steps))
	}
	get0, get1, add := f.Steps[0], f.Steps[1], f.Steps[2]

	if get0.Dst != 2 || get1.Dst != 3 {
		t.Errorf("local.get results at %d and %d, want 2 and 3", get0.Dst, get1.Dst)
	}
	if len(add.Args) != 2 || add.Args[0] != 2 || add.Args[1] != 3 {
		t.Errorf("i32.add args = %v, want [2 3]", add.Args)
	}
	if add.Dst != 2 {
		t.Errorf("i32.add result at %d, want 2 (reusing the first operand's slot)", add.Dst)
	}
	if add.StackDepth != 2 {
		t.Errorf("StackDepth before add = %d, want 2", add.StackDepth)
	}
}

// Operand order matters for non-commutative ops: Args[0] must be the left one.
func TestOperandOrder(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(i32.sub (local.get 0) (local.get 1))))`)
	f := m.Funcs[0]
	sub := f.Steps[2]
	if sub.Args[0] >= sub.Args[1] {
		t.Errorf("Args = %v; the left operand must be the lower slot", sub.Args)
	}
}

func TestNestedExpressionStackDepth(t *testing.T) {
	// ((a+b) * (c+d)) needs depth 4 at its peak with four locals pushed.
	m := build(t, `(module (func (export "f")
		(param i32) (param i32) (param i32) (param i32) (result i32)
		(i32.mul
			(i32.add (local.get 0) (local.get 1))
			(i32.add (local.get 2) (local.get 3)))))`)
	f := m.Funcs[0]
	if f.MaxStack != 3 {
		t.Errorf("MaxStack = %d, want 3", f.MaxStack)
	}
	if f.NumSlots != 7 {
		t.Errorf("NumSlots = %d, want 7 (4 params + 3 stack)", f.NumSlots)
	}
}

func TestUnaryAndNullaryArity(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (result i32)
		(i32.clz (local.get 0))))`)
	f := m.Funcs[0]
	clz := f.Steps[1]
	if len(clz.Args) != 1 || clz.Args[0] != 1 {
		t.Errorf("clz args = %v, want [1]", clz.Args)
	}
	if clz.Dst != 1 {
		t.Errorf("clz dst = %d, want 1 (unary reuses its operand slot)", clz.Dst)
	}

	m2 := build(t, `(module (func (export "f") (result i32) (i32.const 7)))`)
	konst := m2.Funcs[0].Steps[0]
	if len(konst.Args) != 0 {
		t.Errorf("i32.const should take no operands, got %v", konst.Args)
	}
	if konst.Dst != 0 {
		t.Errorf("i32.const dst = %d, want 0", konst.Dst)
	}
}

// local.set consumes a value and produces none; local.tee consumes and pushes
// it back. Getting these backwards would silently corrupt every stack depth
// after them.
func TestLocalSetVersusTee(t *testing.T) {
	m := build(t, `(module (func (export "f") (result i32)
		(local $a i32)
		(local.set $a (i32.const 1))
		(local.tee $a (i32.const 2))))`)
	f := m.Funcs[0]

	var set, tee *Step
	for i := range f.Steps {
		switch f.Steps[i].Op {
		case wasm.OpLocalSet:
			set = &f.Steps[i]
		case wasm.OpLocalTee:
			tee = &f.Steps[i]
		}
	}
	if set == nil || tee == nil {
		t.Fatalf("expected both local.set and local.tee, got %v", f.Steps)
	}
	if set.Dst != NoSlot {
		t.Errorf("local.set should produce nothing, got dst %d", set.Dst)
	}
	if len(set.Args) != 1 {
		t.Errorf("local.set should consume one value, got %v", set.Args)
	}
	if tee.Dst == NoSlot {
		t.Error("local.tee should leave its value on the stack")
	}
	if len(tee.Args) != 1 {
		t.Errorf("local.tee should consume one value, got %v", tee.Args)
	}
}

func TestDropConsumesWithoutProducing(t *testing.T) {
	m := build(t, `(module (func (export "f") (result i32)
		(drop (i32.const 1))
		(i32.const 2)))`)
	f := m.Funcs[0]
	var drop *Step
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpDrop {
			drop = &f.Steps[i]
		}
	}
	if drop == nil {
		t.Fatal("no drop step")
	}
	if drop.Dst != NoSlot || len(drop.Args) != 1 {
		t.Errorf("drop should consume one value and produce none, got dst=%d args=%v",
			drop.Dst, drop.Args)
	}
	// The dropped slot is reused by the next push, so the peak stays at 1.
	if f.MaxStack != 1 {
		t.Errorf("MaxStack = %d, want 1", f.MaxStack)
	}
}

// Since M5 the IR no longer refuses a function past the slot budget: it reports
// how many slots the function needs and the emitter spills the coldest of them
// to the chunk-level frame stack. The refusal that used to live here is now the
// emitter's, and only for the case spilling cannot fix.
//
// This is the test that the COUNT is still right, because everything downstream
// -- the spill plan, the local declarations, the budget arithmetic -- is derived
// from it.
func TestSlotBudgetIsReportedRatherThanEnforced(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`(module (func (export "f") (result i32)`)
	for i := 0; i < MaxSlots+5; i++ {
		sb.WriteString(" (local i32)")
	}
	sb.WriteString(" (i32.const 0)))")

	m, err := wasm.DecodeWAT(sb.String())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := BuildModule(m)
	if err != nil {
		t.Fatalf("a function past the budget must still build, for the emitter to spill: %v", err)
	}
	f := im.Funcs[0]
	if f.NumSlots != MaxSlots+5+f.MaxStack {
		t.Errorf("NumSlots = %d, want %d", f.NumSlots, MaxSlots+5+f.MaxStack)
	}
	if f.NumSlots <= MaxSlots {
		t.Errorf("NumSlots = %d should exceed MaxSlots = %d", f.NumSlots, MaxSlots)
	}
}

// The message still has to name the shape of the problem, because the one case
// spilling cannot fix -- too many parameters -- reads nothing like the one it
// can.
func TestTooManySlotsErrorNamesTheParameterCase(t *testing.T) {
	e := &TooManySlotsError{Func: "f", Needed: 300, Max: MaxSlots,
		Params: 300, Locals: 0, MaxStack: 0}
	msg := e.Error()
	for _, want := range []string{"Lua locals", "parameters", "frame stack"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should mention %q: %s", want, msg)
		}
	}
}

// The IR must reject anything the decoder let through that it cannot model,
// rather than emitting silently wrong slots.
func TestArityCoversEverySupportedOp(t *testing.T) {
	// Every op the decoder can produce must have an arity, or Build would fail
	// at compile time on a valid module.
	// Control flow, calls and returns are context-dependent and handled
	// directly by the builder, so they deliberately have no arity entry.
	ops := []wasm.Op{
		wasm.OpNop, wasm.OpDrop,
		wasm.OpLocalGet, wasm.OpLocalSet, wasm.OpLocalTee, wasm.OpI32Const,
		wasm.OpGlobalGet, wasm.OpGlobalSet, wasm.OpSelect,
		wasm.OpI32Load, wasm.OpI32Load8S, wasm.OpI32Load8U,
		wasm.OpI32Load16S, wasm.OpI32Load16U,
		wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
		wasm.OpMemorySize, wasm.OpMemoryGrow,
		wasm.OpI32Add, wasm.OpI32Sub, wasm.OpI32Mul,
		wasm.OpI32DivS, wasm.OpI32DivU, wasm.OpI32RemS, wasm.OpI32RemU,
		wasm.OpI32And, wasm.OpI32Or, wasm.OpI32Xor,
		wasm.OpI32Shl, wasm.OpI32ShrS, wasm.OpI32ShrU,
		wasm.OpI32Rotl, wasm.OpI32Rotr,
		wasm.OpI32Clz, wasm.OpI32Ctz, wasm.OpI32Popcnt,
		wasm.OpI32Extend8S, wasm.OpI32Extend16S, wasm.OpI32Eqz,
		wasm.OpI32Eq, wasm.OpI32Ne,
		wasm.OpI32LtS, wasm.OpI32LtU, wasm.OpI32LeS, wasm.OpI32LeU,
		wasm.OpI32GtS, wasm.OpI32GtU, wasm.OpI32GeS, wasm.OpI32GeU,
	}
	for _, op := range ops {
		if _, ok := arityOf(op); !ok {
			t.Errorf("no arity defined for %s", op)
		}
	}
}

func TestStackUnderflowIsDetected(t *testing.T) {
	// Hand-build a body the decoder would never produce: an add with nothing
	// on the stack. watgo validates real modules, so this can only be reached
	// by an IR bug -- which is exactly what the check is for.
	f := &wasm.Func{
		Name: "broken",
		Type: wasm.FuncType{Results: []wasm.ValType{wasm.I32}},
		Body: []wasm.Instr{{Op: wasm.OpI32Add}, {Op: wasm.OpEnd}},
	}
	_, err := Build(f, &wasm.Module{})
	if err == nil {
		t.Fatal("expected stack underflow to be detected")
	}
	var se *StackError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StackError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "underflow") {
		t.Errorf("message should say underflow: %v", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("message should name the function: %v", err)
	}
}

func TestLocalIndexOutOfRangeIsDetected(t *testing.T) {
	f := &wasm.Func{
		Name: "oob",
		Type: wasm.FuncType{Params: []wasm.ValType{wasm.I32}, Results: []wasm.ValType{wasm.I32}},
		Body: []wasm.Instr{
			{Op: wasm.OpLocalGet, LocalIndex: 7},
			{Op: wasm.OpEnd},
		},
	}
	_, err := Build(f, &wasm.Module{})
	if err == nil {
		t.Fatal("expected an out-of-range local index to be rejected")
	}
	if !strings.Contains(err.Error(), "local index 7") {
		t.Errorf("message should name the bad index: %v", err)
	}
}

func TestUnmodelledOpIsRejected(t *testing.T) {
	f := &wasm.Func{
		Name: "future",
		Body: []wasm.Instr{{Op: wasm.Op(9999)}, {Op: wasm.OpEnd}},
	}
	_, err := Build(f, &wasm.Module{})
	if err == nil {
		t.Fatal("expected an unmodelled op to be rejected")
	}
	if !strings.Contains(err.Error(), "arity") {
		t.Errorf("message should explain that no arity is defined: %v", err)
	}
}

func TestBuildModulePreservesExports(t *testing.T) {
	m := build(t, `(module
		(func (export "a") (result i32) (i32.const 1))
		(func (export "b") (result i32) (i32.const 2)))`)
	if len(m.Funcs) != 2 {
		t.Fatalf("expected 2 functions, got %d", len(m.Funcs))
	}
	if len(m.Exports) != 2 {
		t.Fatalf("expected 2 exports, got %d", len(m.Exports))
	}
	if m.Funcs[0].Index != 0 || m.Funcs[1].Index != 1 {
		t.Errorf("function indices = %d, %d; want 0, 1", m.Funcs[0].Index, m.Funcs[1].Index)
	}
}

func TestBuildModulePropagatesErrors(t *testing.T) {
	// A stack underflow is a defect in our own model rather than in the input --
	// watgo validates first -- so it has to surface as an error from whichever
	// function contains it, not be silently absorbed.
	m, err := wasm.DecodeWAT(`(module (func (export "ok") (result i32) (i32.const 0))
		(func (export "bad") (result i32) (i32.const 0)))`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Hand-break the second function's body so its add has nothing to pop.
	m.Funcs[1].Body = []wasm.Instr{{Op: wasm.OpI32Add}, {Op: wasm.OpEnd}}
	if _, err := BuildModule(m); err == nil {
		t.Fatal("BuildModule should surface a failure in any function")
	}
}

func TestFunctionWithNoResultsHasNoTrailingReturn(t *testing.T) {
	m := build(t, `(module (func (export "f") (param i32) (local.set 0 (i32.const 1))))`)
	f := m.Funcs[0]
	if len(f.Results) != 0 {
		t.Errorf("expected no results, got %v", f.Results)
	}
	if f.NumSlots != 1+f.MaxStack {
		t.Errorf("NumSlots = %d, want %d", f.NumSlots, 1+f.MaxStack)
	}
}
