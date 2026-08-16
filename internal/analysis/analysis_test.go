package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

func build(t *testing.T, wat string) *ir.Module {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	return im
}

func TestLevelParsing(t *testing.T) {
	for in, want := range map[string]Level{"0": O0, "1": O1, "2": O2, "3": O3} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("4"); err == nil {
		t.Error("level 4 should be rejected rather than clamped: a flag typo that " +
			"silently means something else is worse than an error")
	}
	if _, err := ParseLevel("fast"); err == nil {
		t.Error("a non-numeric level should be rejected")
	}
	if O0.Peephole() || O0.Slots() || O0.Upvalues() {
		t.Error("-opt=0 must disable every pass; it is the reference output")
	}
	if !O1.Peephole() || O1.Slots() {
		t.Error("O1 is the peephole and nothing more")
	}
	if !O2.Slots() || O2.Upvalues() {
		t.Error("O2 adds typed-slot promotion and nothing more")
	}
	if !O3.Upvalues() {
		t.Error("O3 adds upvalue promotion")
	}
	// The default moved to O3 at M7, once the two things blocking it were
	// measured rather than assumed: promotion backs off as the chunk fills
	// (luagen's TestPromotionLeavesTheMarginItPromises) so it cannot break a
	// build that worked, and M7's chunk state turned out to live in separate
	// required files rather than competing for the guest's 200 names.
	if !DefaultLevel.Upvalues() {
		t.Error("the default should include upvalue promotion: it is ~14% on call " +
			"dispatch and inside noise everywhere else")
	}
	if DefaultLevel != O3 {
		t.Errorf("DefaultLevel = %v, want O3", DefaultLevel)
	}
	if got := O2.String(); got != "2" {
		t.Errorf("String() = %q, want \"2\"", got)
	}
}

// The load-bearing case: a wrap whose consumer wraps again may go, because
// `(a + (x % M)) % M` and `(a + x) % M` are the same number.
func TestWrapIsDeferredIntoAConsumerThatWrapsAgain(t *testing.T) {
	m := build(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $i i32) (result i32)
			(i32.load (i32.add (local.get $p) (i32.mul (local.get $i) (i32.const 4))))))`)
	w := Ranges(m.Funcs[0])

	mul, add := -1, -1
	for i, s := range m.Funcs[0].Steps {
		switch s.Op {
		case wasm.OpI32Mul:
			mul = i
		case wasm.OpI32Add:
			add = i
		}
	}
	if mul < 0 || add < 0 {
		t.Fatal("expected a mul and an add in the address computation")
	}
	if !w.Elided(mul) {
		t.Error("the mul's wrap feeds an add that wraps, so it should be deferred")
	}
	if w.Elided(add) {
		t.Error("the add's result is a memory ADDRESS, which needs the true value; " +
			"its wrap must stay")
	}
}

// The other half of the same rule: nothing is deferred into a consumer that
// reads the value's bits rather than its residue.
func TestWrapSurvivesAConsumerThatReadsBits(t *testing.T) {
	m := build(t, `(module
		(func (export "f") (param $a i32) (param $b i32) (result i32)
			(i32.shr_u (i32.add (local.get $a) (local.get $b)) (i32.const 3))))`)
	w := Ranges(m.Funcs[0])
	for i, s := range m.Funcs[0].Steps {
		if s.Op == wasm.OpI32Add && w.Elided(i) {
			t.Error("shr_u divides, so it needs the wrapped value; the wrap must stay")
		}
	}
}

// A wrap that could never have fired is dropped outright, with no consumer
// analysis involved.
func TestWrapDroppedWhenTheResultCannotOverflow(t *testing.T) {
	m := build(t, `(module (memory 1)
		(func (export "f") (param $p i32) (result i32)
			(i32.add (i32.load8_u (local.get $p)) (i32.const 1))))`)
	w := Ranges(m.Funcs[0])
	found := false
	for i, s := range m.Funcs[0].Steps {
		if s.Op != wasm.OpI32Add {
			continue
		}
		found = true
		if !w.Elided(i) {
			t.Error("a byte load is at most 255, so +1 cannot reach 2^32")
		}
		if r := w.Result[i]; r.Lo != 1 || r.Hi != 256 {
			t.Errorf("range = [%d,%d], want [1,256]", r.Lo, r.Hi)
		}
	}
	if !found {
		t.Fatal("no add found")
	}
}

// A wasm LOCAL's range survives a block boundary; an operand-STACK slot's does
// not. The split is deliberate and it is the line the CFG fixpoint drew.
//
// A local is a Lua local with a name that outlives the block, so a fact about
// it joins at merges and converges at loop heads like any other dataflow value.
// A stack slot is written once and read once, and its range is entangled with
// whether its wrap was deferred -- a deal struck with one consumer inside one
// block. Letting one of those cross would mean a consumer somewhere else
// reading a value only CONGRUENT to the one it expects.
func TestLocalRangesCrossABlockBoundaryAndSlotRangesDoNot(t *testing.T) {
	m := build(t, `(module
		(func (export "f") (param $n i32) (result i32)
			(local $i i32)
			(local.set $i (i32.const 3))
			(block (loop
				(br_if 1 (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br 0)))
			(local.get $i)))`)
	f := m.Funcs[0]
	w := Ranges(f)
	// The counter enters the loop at 3 and the guard bounds it below n, so it
	// is in [3, 2^32-2] inside the body -- which is exactly what makes i + 1
	// unable to overflow.
	for i, s := range f.Steps {
		if s.Op != wasm.OpI32Add {
			continue
		}
		if r := w.ArgRange(i, 0); r.Lo != 3 || r.Hi != u32Max-1 {
			t.Errorf("loop counter range = [%d,%d], want [3,%d]", r.Lo, r.Hi, u32Max-1)
		}
		if !w.Elided(i) {
			t.Error("and therefore the increment needs no wrap")
		}
	}

	// The same fact carried on the operand stack instead is given up, even
	// though both arms of the branch put the same number there.
	m2 := build(t, `(module
		(func (export "g") (param $c i32) (result i32)
			(i32.add
				(block (result i32)
					(i32.const 5)
					(br_if 0 (local.get $c))
					(drop)
					(i32.const 5))
				(i32.const 1))))`)
	g := m2.Funcs[0]
	w2 := Ranges(g)
	last := -1
	for i, s := range g.Steps {
		if s.Op == wasm.OpI32Add {
			last = i
		}
	}
	if last < 0 {
		t.Fatal("no add found")
	}
	if r := w2.ArgRange(last, 0); r != FullU32 {
		t.Errorf("a block result arrives on a stack slot, whose range does not "+
			"survive the merge; got %v", r)
	}
}

func TestConstU32RejectsADeferredValue(t *testing.T) {
	if _, ok := (Range{Lo: -1, Hi: -1}).ConstU32(); ok {
		t.Error("a negative exact range is a deferred subtraction, not an i32 constant")
	}
	if _, ok := (Range{Lo: 1 << 33, Hi: 1 << 33}).ConstU32(); ok {
		t.Error("an exact range past 2^32 is a deferred sum, not an i32 constant")
	}
	if v, ok := (Range{Lo: 7, Hi: 7}).ConstU32(); !ok || v != 7 {
		t.Errorf("ConstU32() = %d, %v; want 7, true", v, ok)
	}
	if _, ok := (Range{Lo: 0, Hi: 1}).ConstU32(); ok {
		t.Error("a range with two values is not a constant")
	}
	if !(Range{Lo: 0, Hi: 5}).Below(6) || (Range{Lo: 0, Hi: 6}).Below(6) {
		t.Error("Below is strict")
	}
	if (Range{Lo: -1, Hi: 0}).FitsU32() {
		t.Error("a negative low bound does not fit an unsigned i32")
	}
}

// A nil analysis is what -opt=0 hands the emitter, and every accessor has to
// answer "nothing is known" rather than crash.
func TestNilAnalysisIsSafe(t *testing.T) {
	var w *Wrap
	if w.Elided(3) {
		t.Error("a nil analysis must elide nothing")
	}
	if r := w.ArgRange(0, 0); r != FullU32 {
		t.Error("a nil analysis must report the full range")
	}
	var fr *Frame
	if fr.Promoted() {
		t.Error("a nil frame promotes nothing")
	}
	if _, ok := fr.LoadAt(0); ok {
		t.Error("a nil frame has no loads")
	}
	if _, ok := fr.StoreAt(0); ok {
		t.Error("a nil frame has no stores")
	}
	var sp *Spill
	if sp.Active() {
		t.Error("a nil spill is inactive")
	}
	if _, ok := sp.At(0); ok {
		t.Error("a nil spill holds nothing")
	}
}

const framePrologue = `(global.set $sp (local.tee $fp (i32.sub (global.get $sp) (i32.const 16))))`

func frameModule(t *testing.T, body string) *ir.Func {
	t.Helper()
	m := build(t, fmt.Sprintf(`(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (param $x f64) (result f64) (local $fp i32)
			%s %s))`, framePrologue, body))
	return m.Funcs[0]
}

func TestFramePromotesANonEscapingSlot(t *testing.T) {
	f := frameModule(t, `
		(f64.store offset=8 (local.get $fp) (local.get $x))
		(f64.load offset=8 (local.get $fp))`)
	fr := Frames(f)
	if !fr.Promoted() {
		t.Fatal("a frame slot that is only ever stored and loaded must promote")
	}
	if len(fr.Slots) != 1 || fr.Slots[0].Type != wasm.F64 || fr.Slots[0].Offset != 8 {
		t.Fatalf("slots = %+v, want one f64 at offset 8", fr.Slots)
	}
	if len(fr.Load) != 1 || len(fr.Store) != 1 {
		t.Errorf("expected one load and one store to be rewritten, got %d and %d",
			len(fr.Load), len(fr.Store))
	}
	if fr.Extra != 1 {
		t.Errorf("Extra = %d, want 1 (an f64 is one Lua slot)", fr.Extra)
	}
}

// The case that matters most in practice, and the reason the pass finds nothing
// in TinyGo output: a frame address handed to a callee could be read or written
// by anyone.
func TestFrameRefusesWhenTheAddressEscapesIntoACall(t *testing.T) {
	m := build(t, fmt.Sprintf(`(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func $sink (param i32))
		(func (export "f") (result i32) (local $fp i32)
			%s
			(i32.store offset=8 (local.get $fp) (i32.const 7))
			(call $sink (i32.add (local.get $fp) (i32.const 8)))
			(i32.load offset=8 (local.get $fp))))`, framePrologue))
	if fr := Frames(m.Funcs[1]); fr.Promoted() {
		t.Error("a frame pointer passed to a call escapes; nothing may be promoted")
	}
}

func TestFrameRefusesADynamicOffset(t *testing.T) {
	m := build(t, fmt.Sprintf(`(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (param $k i32) (result i32) (local $fp i32)
			%s
			(i32.store (i32.add (local.get $fp) (local.get $k)) (i32.const 7))
			(i32.load offset=8 (local.get $fp))))`, framePrologue))
	if fr := Frames(m.Funcs[0]); fr.Promoted() {
		t.Error("frame + a runtime value names a slot the pass cannot identify")
	}
}

func TestFrameRefusesMixedWidthsAtOneOffset(t *testing.T) {
	m := build(t, fmt.Sprintf(`(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (result i32) (local $fp i32)
			%s
			(i64.store offset=8 (local.get $fp) (i64.const 1))
			(i32.load offset=8 (local.get $fp))))`, framePrologue))
	if fr := Frames(m.Funcs[0]); fr.Promoted() {
		t.Error("a slot written as an i64 and read as an i32 is a union; a Lua " +
			"local has no halves")
	}
}

func TestFrameRefusesOverlappingSlots(t *testing.T) {
	m := build(t, fmt.Sprintf(`(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (result i32) (local $fp i32)
			%s
			(i64.store offset=8 (local.get $fp) (i64.const 1))
			(i32.store offset=12 (local.get $fp) (i32.const 2))
			(i32.load offset=12 (local.get $fp))))`, framePrologue))
	if fr := Frames(m.Funcs[0]); fr.Promoted() {
		t.Error("offset 12 sits inside the i64 at offset 8")
	}
}

func TestFrameNeedsThePrologue(t *testing.T) {
	m := build(t, `(module (memory 1)
		(func (export "f") (param $p i32) (result i32)
			(i32.store offset=8 (local.get $p) (i32.const 1))
			(i32.load offset=8 (local.get $p))))`)
	if fr := Frames(m.Funcs[0]); fr.Promoted() {
		t.Error("without a stack-pointer frame there is no privacy argument at all")
	}
}

func TestSpillLeavesTheHottestSlotsAlone(t *testing.T) {
	// A function well past the budget, with one slot used inside a loop and the
	// rest touched once each in a straight line.
	var sb strings.Builder
	sb.WriteString(`(module (func (export "f") (result i32)`)
	const n = ir.MaxSlots + 20
	for i := 0; i < n; i++ {
		sb.WriteString(" (local i32)")
	}
	sb.WriteString(" (local $hot i32)")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, " (local.set %d (i32.const %d))", i, i)
	}
	// The hot local is written inside a loop, so its weight is 10x.
	fmt.Fprintf(&sb, ` (block (loop
		(br_if 1 (i32.ge_u (local.get %d) (i32.const 5)))
		(local.set %d (i32.add (local.get %d) (i32.const 1)))
		(br 0)))`, n, n, n)
	fmt.Fprintf(&sb, " (local.get %d)))", n)

	m := build(t, sb.String())
	f := m.Funcs[0]
	if f.NumSlots <= ir.MaxSlots {
		t.Fatalf("test module needs to exceed the budget, NumSlots = %d", f.NumSlots)
	}
	sp := Plan(f, ir.MaxSlots)
	if !sp.Active() {
		t.Fatal("a function past the budget must get a spill plan")
	}
	if got := f.NumSlots - sp.Size + 1; got > ir.MaxSlots {
		t.Errorf("%d locals would still be declared, over the %d budget", got, ir.MaxSlots)
	}
	if _, spilled := sp.At(f.LocalSlot(uint32(n))); spilled {
		t.Error("the loop-carried local is the hottest slot in the function and " +
			"must be the last thing to spill")
	}
}

func TestSpillIsNilWhenEverythingFits(t *testing.T) {
	m := build(t, `(module (func (export "f") (result i32) (i32.const 1)))`)
	if sp := Plan(m.Funcs[0], ir.MaxSlots); sp.Active() {
		t.Error("a two-slot function must not spill")
	}
}

func TestSpillRefusesWhenOnlyParametersRemain(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`(module (func (export "f")`)
	for i := 0; i < ir.MaxSlots+5; i++ {
		sb.WriteString(" (param i32)")
	}
	sb.WriteString("))")
	m := build(t, sb.String())
	if sp := Plan(m.Funcs[0], ir.MaxSlots); sp.Active() {
		t.Error("a parameter is a Lua local because it is in the parameter list; " +
			"there is nowhere else for the caller to put it")
	}
}

func TestHotCalleesPrefersCallsInsideLoops(t *testing.T) {
	m := build(t, `(module
		(func $cold)
		(func $hot)
		(func (export "f")
			(call $cold)
			(block (loop
				(call $hot)
				(br_if 1 (i32.const 1))
				(br 0)))))`)
	got := HotCallees(m, 1)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("HotCallees = %v, want [1] -- the call inside the loop", got)
	}
	if all := HotCallees(m, 10); len(all) != 2 {
		t.Errorf("with room for both, HotCallees = %v, want two entries", all)
	}
	if none := HotCallees(m, 0); none != nil {
		t.Errorf("a zero budget promotes nothing, got %v", none)
	}
	if none := HotCallees(nil, 4); none != nil {
		t.Errorf("a nil module promotes nothing, got %v", none)
	}
}

func TestHotCalleesIsDeterministic(t *testing.T) {
	m := build(t, `(module
		(func $a) (func $b) (func $c)
		(func (export "f") (call $a) (call $b) (call $c)))`)
	first := HotCallees(m, 2)
	for i := 0; i < 5; i++ {
		if got := HotCallees(m, 2); len(got) != len(first) || got[0] != first[0] || got[1] != first[1] {
			t.Fatalf("HotCallees is not deterministic: %v then %v", first, got)
		}
	}
}

func TestLoopWeightSaturates(t *testing.T) {
	if loopWeight(0) != 1 {
		t.Error("an unnested call is worth one")
	}
	if loopWeight(4) != loopWeight(3) {
		t.Error("the weight has to saturate, or one deeply nested but rarely " +
			"entered loop takes the whole budget")
	}
}

// The transfer functions are where a wrong interval becomes a wrong program, so
// each one is pinned to the bound it claims rather than exercised incidentally.
func TestResultRanges(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		op     wasm.Op
		lo, hi int64
	}{
		{"and with a low mask", "(i32.and (local.get $p) (i32.const 255))",
			wasm.OpI32And, 0, 255},
		{"and with zero", "(i32.and (local.get $p) (i32.const 0))",
			wasm.OpI32And, 0, 0},
		{"and narrows to the smaller side", "(i32.and (i32.load8_u (local.get $p)) (local.get $p))",
			wasm.OpI32And, 0, 255},
		{"shift right by a constant", "(i32.shr_u (local.get $p) (i32.const 24))",
			wasm.OpI32ShrU, 0, 255},
		{"shift right by zero is the identity", "(i32.shr_u (local.get $p) (i32.const 0))",
			wasm.OpI32ShrU, 0, u32Max},
		{"shift left stays in range", "(i32.shl (local.get $p) (i32.const 3))",
			wasm.OpI32Shl, 0, u32Max},
		{"shift left by zero passes the range through", "(i32.shl (i32.load8_u (local.get $p)) (i32.const 0))",
			wasm.OpI32Shl, 0, 255},
		{"unsigned divide cannot grow", "(i32.div_u (i32.load8_u (local.get $p)) (local.get $p))",
			wasm.OpI32DivU, 0, 255},
		{"remainder by a constant", "(i32.rem_u (local.get $p) (i32.const 10))",
			wasm.OpI32RemU, 0, 9},
		{"a comparison is a flag", "(i32.lt_u (local.get $p) (i32.const 4))",
			wasm.OpI32LtU, 0, 1},
		{"eqz is a flag", "(i32.eqz (local.get $p))", wasm.OpI32Eqz, 0, 1},
		{"popcount is bounded by the width", "(i32.popcnt (local.get $p))",
			wasm.OpI32Popcnt, 0, 32},
		{"clz is bounded by the width", "(i32.clz (local.get $p))",
			wasm.OpI32Clz, 0, 32},
		{"a 16-bit load", "(i32.load16_u (local.get $p))", wasm.OpI32Load16U, 0, 65535},
		{"memory.size in pages", "(memory.size)", wasm.OpMemorySize, 0, 65536},
		{"a global is unknown", "(global.get $g)", wasm.OpGlobalGet, 0, u32Max},
		{"a general multiply is unknown", "(i32.mul (local.get $p) (local.get $p))",
			wasm.OpI32Mul, 0, u32Max},
		{"a shift by a runtime amount is unknown", "(i32.shr_u (local.get $p) (local.get $p))",
			wasm.OpI32ShrU, 0, u32Max},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := build(t, fmt.Sprintf(`(module (memory 1) (global $g (mut i32) (i32.const 0))
				(func (export "f") (param $p i32) (result i32) %s))`, c.body))
			f := m.Funcs[0]
			w := Ranges(f)
			for i, s := range f.Steps {
				if s.Op != c.op {
					continue
				}
				if r := w.Result[i]; r.Lo != c.lo || r.Hi != c.hi {
					t.Errorf("range = [%d,%d], want [%d,%d]", r.Lo, r.Hi, c.lo, c.hi)
				}
				return
			}
			t.Fatalf("no %s in the compiled body", c.op)
		})
	}
}

// A constant that reaches an operand through a local is still a constant, and
// the emitter and the analysis have to agree about that or the recorded range
// describes code that was never emitted.
func TestConstantsTravelThroughLocals(t *testing.T) {
	m := build(t, `(module
		(func (export "f") (param $p i32) (result i32) (local $k i32)
			(local.set $k (i32.const 255))
			(i32.and (local.get $p) (local.get $k))))`)
	f := m.Funcs[0]
	w := Ranges(f)
	for i, s := range f.Steps {
		if s.Op != wasm.OpI32And {
			continue
		}
		if k, ok := w.ArgRange(i, 1).ConstU32(); !ok || k != 255 {
			t.Fatalf("operand 1 = %d, %v; a constant through a local is still a constant", k, ok)
		}
		if r := w.Result[i]; r.Hi != 255 {
			t.Errorf("result range = [%d,%d], want [0,255]", r.Lo, r.Hi)
		}
	}
}

// The masking consumers absorb a deferred value because 2^n divides 2^32; the
// address of a load does not, because it is read as a number rather than a
// residue.
func TestAbsorbingConsumers(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		defer_ bool
	}{
		{"mask absorbs", "(i32.and (i32.add (local.get $a) (local.get $b)) (i32.const 255))", true},
		{"shift left absorbs", "(i32.shl (i32.add (local.get $a) (local.get $b)) (i32.const 4))", true},
		{"a non-mask and does not", "(i32.and (i32.add (local.get $a) (local.get $b)) (i32.const 6))", false},
		{"a runtime shift does not", "(i32.shl (i32.add (local.get $a) (local.get $b)) (local.get $a))", false},
		{"a comparison does not", "(i32.lt_u (i32.add (local.get $a) (local.get $b)) (i32.const 4))", false},
		{"a drop does not", "(drop (i32.add (local.get $a) (local.get $b))) (i32.const 0)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := build(t, fmt.Sprintf(`(module
				(func (export "f") (param $a i32) (param $b i32) (result i32) %s))`, c.body))
			f := m.Funcs[0]
			w := Ranges(f)
			for i, s := range f.Steps {
				if s.Op != wasm.OpI32Add {
					continue
				}
				if got := w.Elided(i); got != c.defer_ {
					t.Errorf("deferred = %v, want %v", got, c.defer_)
				}
				return
			}
			t.Fatal("no add found")
		})
	}
}

func TestLowMask(t *testing.T) {
	for _, k := range []uint32{0, 0xFFFFFFFF, 6, 0x80000000} {
		if _, ok := lowMask(k); ok {
			t.Errorf("%#x is not 2^n - 1", k)
		}
	}
	for n := uint32(1); n < 32; n++ {
		got, ok := lowMask(1<<n - 1)
		if !ok || got != n {
			t.Errorf("lowMask(2^%d - 1) = %d, %v", n, got, ok)
		}
	}
}

func TestMemAccessOnlyClassifiesFullWidth(t *testing.T) {
	full := map[wasm.Op]uint32{
		wasm.OpI32Load: 4, wasm.OpI32Store: 4,
		wasm.OpI64Load: 8, wasm.OpI64Store: 8,
		wasm.OpF32Load: 4, wasm.OpF32Store: 4,
		wasm.OpF64Load: 8, wasm.OpF64Store: 8,
	}
	for op, width := range full {
		_, w, _, ok := memAccess(op)
		if !ok || w != width {
			t.Errorf("%s: width %d, ok %v; want %d", op, w, ok, width)
		}
	}
	// A narrowing access reads part of a slot, and a Lua local has no parts.
	for _, op := range []wasm.Op{wasm.OpI32Load8U, wasm.OpI32Store8,
		wasm.OpI64Load32S, wasm.OpI64Store16, wasm.OpI32Add} {
		if _, _, _, ok := memAccess(op); ok {
			t.Errorf("%s must not be promotable", op)
		}
	}
}

// Each clause of the prologue match earns its keep, so each is checked with the
// others intact.
func TestPrologueRejections(t *testing.T) {
	cases := map[string]string{
		"an immutable stack pointer": `(module (memory 1) (global $sp i32 (i32.const 65536))
			(func (export "f") (result i32) (local $fp i32)
				(drop (i32.sub (global.get $sp) (i32.const 16)))
				(i32.store offset=8 (local.tee $fp (i32.const 0)) (i32.const 1))
				(i32.load offset=8 (local.get $fp))))`,
		"an f64 stack pointer": `(module (memory 1) (global $sp (mut f64) (f64.const 0))
			(func (export "f") (result i32) (local $fp i32)
				(i32.store offset=8 (local.get $fp) (i32.const 1))
				(i32.load offset=8 (local.get $fp))))`,
		"too short to be a prologue": `(module (memory 1)
			(func (export "f") (result i32) (i32.const 1)))`,
	}
	for name, wat := range cases {
		t.Run(name, func(t *testing.T) {
			m := build(t, wat)
			if fr := Frames(m.Funcs[0]); fr.Promoted() {
				t.Error("promotion needs the whole prologue, not part of it")
			}
		})
	}
}

// A function with no module attached cannot have its globals inspected, so the
// pass has to decline rather than dereference nil.
func TestFramesWithoutAModule(t *testing.T) {
	if fr := Frames(&ir.Func{}); fr.Promoted() {
		t.Error("no module means no globals to check the stack pointer against")
	}
}
