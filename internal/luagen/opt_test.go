package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// allLevels is what every behavioural test here runs at.
//
// One level being green says nothing about another: -opt=0 and -opt=3 share the
// lowerings but not the code that decides where a value lives or whether a step
// is emitted at all. The conformance suite makes the same demand of the
// compiler as a whole; this makes it of one module at a time, where a failure
// says which construct broke.
var allLevels = []analysis.Level{analysis.O0, analysis.O1, analysis.O2, analysis.O3}

// emitAt compiles a module at one level and returns the generated Lua from the
// function table onwards.
func emitAt(t *testing.T, wat string, lvl analysis.Level) string {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: lvl})
	if err != nil {
		t.Fatalf("emit at -opt=%s: %v", lvl, err)
	}
	return src
}

// emitBody is emitAt with the (large, constant) runtime prelude stripped, so a
// failure message shows the code the assertion is about.
func emitBody(t *testing.T, wat string, lvl analysis.Level) string {
	t.Helper()
	src := emitAt(t, wat, lvl)
	if i := strings.Index(src, "local F = {}"); i >= 0 {
		return src[i:]
	}
	return src
}

// runAt instantiates a module at one level and evaluates a Lua expression
// against it, returning what the expression printed.
func runAt(t *testing.T, wat, expr string, lvl analysis.Level) string {
	t.Helper()
	return runAtMode(t, wat, expr, lvl, NaNCanonical)
}

// runAtMode is runAt in a chosen NaN mode. The two modes share no lowering for
// a float operation -- canonical uses Lua's own operators, exact routes through
// a helper that can take a boxed table -- so a behaviour proved in one says
// nothing about the other.
func runAtMode(t *testing.T, wat, expr string, lvl analysis.Level, nan NaNMode) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: lvl, NaN: nan})
	if err != nil {
		t.Fatalf("emit at -opt=%s: %v", lvl, err)
	}
	var b strings.Builder
	b.WriteString("local M = (function(...)\n")
	b.WriteString(src)
	b.WriteString("\nend)()\n")
	b.WriteString("local ok, r = pcall(function() return " + expr + " end)\n")
	b.WriteString(`if ok then print(tostring(r)) else
  print("TRAP\t" .. tostring(type(r) == "table" and (r.fk_trap or r.fk_unsupported) or r))
end
`)
	out, err := h.RunString(b.String())
	if err != nil {
		t.Fatalf("run at -opt=%s: %v", lvl, err)
	}
	return strings.TrimSpace(out)
}

// sameAtEveryLevel is the workhorse: a module has to compute the same thing
// however hard the optimizer worked on it.
func sameAtEveryLevel(t *testing.T, wat, expr, want string) {
	t.Helper()
	for _, lvl := range allLevels {
		got := runAt(t, wat, expr, lvl)
		if got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// -opt=0 is the reference. It must produce exactly what the M4 emitter did, or
// it stops being useful for bisecting a miscompile against the optimizer.
func TestLevelZeroKeepsTheM4Lowerings(t *testing.T) {
	src := emitBody(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $i i32) (result i32)
			(i32.load (i32.add (local.get $p) (i32.mul (local.get $i) (i32.const 4))))))`,
		analysis.O0)
	for _, want := range []string{
		"v3 = (v1 * 4) % 4294967296.0",
		"v2 = (v0 + v3) % 4294967296.0",
		"v2 = ld32(MEM, MEMSIZE, v2)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("-opt=0 should still emit %q:\n%s", want, src)
		}
	}
}

func TestSignedCompareKeepsItsScratchFormAtLevelZero(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(i32.lt_s (local.get 0) (local.get 1))))`, analysis.O0)
	if !strings.Contains(src, "t0 = v0 if t0 >= 2147483648.0") {
		t.Errorf("-opt=0 must keep the two-scratch sign fixup:\n%s", src)
	}
}

// The peephole's headline result: a straight-line run of stack operations
// becomes one Lua expression, and the redundant wrap in the middle goes.
func TestPeepholeCollapsesAnAddressComputation(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "f") (param $p i32) (param $i i32) (result i32)
			(i32.load (i32.add (local.get $p) (i32.mul (local.get $i) (i32.const 4))))))`
	src := emitBody(t, wat, analysis.O1)
	want := "return ld32(MEM, MEMSIZE, ((v0 + (v1 * 4)) % 4294967296.0))"
	if !strings.Contains(src, want) {
		t.Errorf("expected %q:\n%s", want, src)
	}
	// One wrap where -opt=0 emitted two: the inner one fed an add that reduces
	// modulo 2^32 anyway.
	if got, was := strings.Count(src, "% 4294967296.0"),
		strings.Count(emitBody(t, wat, analysis.O0), "% 4294967296.0"); got != was-1 {
		t.Errorf("wraps = %d, want %d (one fewer than -opt=0)", got, was-1)
	}
}

// A comparison feeding a branch never becomes 0 or 1 at all.
func TestComparisonFoldsIntoItsBranch(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $a i32) (param $b i32) (result i32)
		(block (br_if 0 (i32.ge_u (local.get $a) (local.get $b))) (return (i32.const 1)))
		(i32.const 2)))`, analysis.O1)
	if !strings.Contains(src, "if v0 >= v1 then goto") {
		t.Errorf("expected the compare folded into the branch:\n%s", src)
	}
	if strings.Contains(src, "and 1 or 0") {
		t.Errorf("the 0/1 materialisation should be gone:\n%s", src)
	}
}

// `if` jumps to its else-label when the condition is FALSE, so it needs the
// inverted comparison rather than a `not`.
func TestIfFoldsTheInvertedComparison(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $a i32) (result i32)
		(if (result i32) (i32.eqz (local.get $a))
			(then (i32.const 1)) (else (i32.const 2)))))`, analysis.O1)
	if !strings.Contains(src, "if v0 ~= 0 then goto") {
		t.Errorf("expected the inverted condition inline:\n%s", src)
	}
}

func TestSignedComparesAgainstConstantsFoldTheBias(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $a i32) (result i32)
		(i32.lt_s (local.get $a) (i32.const 10))))`, analysis.O1)
	// 10 + 2^31 is a compile-time constant, so only one side is biased.
	if !strings.Contains(src, "2147483658") {
		t.Errorf("the constant side of the bias should be folded:\n%s", src)
	}
	if strings.Contains(src, "t0 =") {
		t.Errorf("the scratch-register form should be gone at -opt=1:\n%s", src)
	}
}

func TestSignedCompareOfProvablySmallValuesIsDirect(t *testing.T) {
	src := emitBody(t, `(module (memory 1)
		(func (export "f") (param $p i32) (result i32)
			(i32.lt_s (i32.load8_u (local.get $p)) (i32.const 10))))`, analysis.O1)
	if strings.Contains(src, "2147483648.0") {
		t.Errorf("a byte is never negative as a signed i32, so no bias is needed:\n%s", src)
	}
}

// counted is the canonical loop, in the shape LLVM emits it: rotated, so the
// trip-count guard is in front and the test is at the bottom.
const counted = `(module (func (export "f") (param $n i32) (result i32)
	(local $i i32) (local $s i32)
	(block $done
		(br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
		(loop $top
			(local.set $s (i32.add (local.get $s) (local.get $i)))
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
	(local.get $s)))`

// What the loop-header fixpoint is for, pinned as text rather than as a ratio.
//
// It used to assert the two hot lines directly -- `if v1 < v0 then` for a
// compare that had lost its 2^31 bias, and `v1 = v1 + 1` for an increment that
// had lost its wrap. The counted-loop lowering removes BOTH LINES, which is a
// stronger result and not a regression, so the assertion moved to what is left:
// a numeric `for` whose bound is the bare parameter. A biased compare or a
// wrapped increment could not be expressed in that header at all.
func TestCountedLoopHasNoBiasAndNoWrapOnItsHotLines(t *testing.T) {
	src := emitBody(t, counted, analysis.O1)
	if !strings.Contains(src, "for v1 = v1, v0 - 1 do") {
		t.Errorf("the counted loop should become a numeric for over the bare "+
			"bound -- both operands are provably non-negative once the entry "+
			"guard is carried across the back edge:\n%s", src)
	}
	if strings.Contains(src, "2147483648.0") && !strings.Contains(src,
		"if (v0 + 2147483648.0)") {
		t.Errorf("no bias should survive except the pre-header guard's:\n%s", src)
	}
	// The counter is never assigned at all any more -- the `for` owns it. The
	// two wraps that remain are the pre-header guard's signed-compare bias and
	// the accumulator's, since `s + i` genuinely can overflow.
	if strings.Contains(src, "v1 = v1 + 1") {
		t.Errorf("the for owns the counter; the increment should be gone:\n%s", src)
	}
	if strings.Count(src, "% 4294967296.0") != 2 {
		t.Errorf("only the guard's and the accumulator's wraps should remain:\n%s", src)
	}

	// And the same module at -opt=0 still has all of it, because level 0 is the
	// reference the optimizer gets bisected against.
	zero := emitBody(t, counted, analysis.O0)
	if !strings.Contains(zero, "% 4294967296.0") || !strings.Contains(zero, "t0 =") {
		t.Errorf("-opt=0 must keep the wrap and the scratch-register signed "+
			"compare:\n%s", zero)
	}
	if strings.Contains(zero, "for ") {
		t.Errorf("-opt=0 must keep the goto loop:\n%s", zero)
	}
}

func TestCountedLoopComputesTheSameSumAtEveryLevel(t *testing.T) {
	// 0+1+...+999 = 499500, and the counter must not run one short or one long.
	sameAtEveryLevel(t, counted, `M.exports["f"](1000)`, "499500")
	// A non-positive trip count must skip the loop entirely; the entry guard is
	// the only thing that says so and it is also what the analysis leans on.
	sameAtEveryLevel(t, counted, `M.exports["f"](0)`, "0")
	sameAtEveryLevel(t, counted, `M.exports["f"](4294967295)`, "0")
}

// The soundness half. A counter nothing bounds really can reach 2^32-1, and an
// elided wrap there leaves a 33-bit number where the rest of the program
// expects an i32.
func TestAnUnboundedCounterKeepsItsWrapInTheOutput(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $c i32) (result i32)
		(local $i i32)
		(loop $top
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if $top (local.get $c)))
		(local.get $i)))`, analysis.O1)
	if !strings.Contains(src, "% 4294967296.0") {
		t.Errorf("nothing bounds this counter, so the wrap must stay:\n%s", src)
	}
}

// Every arithmetic and comparison lowering, checked to agree across levels.
// The peephole rewrites where each operand comes from, and a width or sign
// mistake in one op shows up nowhere else.
func TestOperationsAgreeAtEveryLevel(t *testing.T) {
	cases := []struct {
		name string
		wat  string
		expr string
		want string
	}{
		{"i32 wrap on add", `(module (func (export "f") (result i32)
			(i32.add (i32.const 4294967295) (i32.const 3))))`,
			`M.exports["f"]()`, "2"},
		{"signed compare across zero", `(module (func (export "f") (result i32)
			(i32.lt_s (i32.const 4294967295) (i32.const 1))))`,
			`M.exports["f"]()`, "1"},
		{"signed compare both negative", `(module (func (export "f") (param i32) (param i32) (result i32)
			(i32.gt_s (local.get 0) (local.get 1))))`,
			`M.exports["f"](4294967295, 4294967294)`, "1"},
		{"shift left masks first", `(module (func (export "f") (param i32) (result i32)
			(i32.shl (local.get 0) (i32.const 31))))`,
			`M.exports["f"](3)`, "2147483648"},
		{"rotate keeps its scratch form", `(module (func (export "f") (param i32) (result i32)
			(i32.rotl (local.get 0) (i32.const 4))))`,
			`M.exports["f"](2415919104)`, "9"},
		{"sign extension", `(module (func (export "f") (param i32) (result i32)
			(i32.extend8_s (local.get 0))))`,
			`M.exports["f"](255)`, "4294967295"},
		{"select is not short-circuit", `(module (func (export "f") (param i32) (result i32)
			(select (i32.const 7) (i32.const 9) (local.get 0))))`,
			`M.exports["f"](0)`, "9"},
		{"i64 add through two slots", `(module (func (export "f") (result i64)
			(i64.add (i64.const 4294967295) (i64.const 1))))`,
			`select(2, M.exports["f"]())`, "1"},
		{"i64 compare", `(module (func (export "f") (result i32)
			(i64.lt_s (i64.const -1) (i64.const 0))))`,
			`M.exports["f"]()`, "1"},
		{"f64 arithmetic", `(module (func (export "f") (result f64)
			(f64.div (f64.const 1) (f64.const 4))))`,
			`M.exports["f"]()`, "0.25"},
		{"f32 rounding after every op", `(module (func (export "f") (result f32)
			(f32.add (f32.const 0.1) (f32.const 0.2))))`,
			`string.format("%.9g", M.exports["f"]())`, "0.300000012"},
		{"abs of negative zero", `(module (func (export "f") (result f64)
			(f64.abs (f64.const -0.0))))`,
			`1.0 / M.exports["f"]()`, "inf"},
		{"memory round trip", `(module (memory 1) (func (export "f") (param i32) (result i32)
			(i32.store (i32.const 16) (local.get 0))
			(i32.load (i32.const 16))))`,
			`M.exports["f"](4000000000)`, "4000000000"},
		{"a load whose address is a load", `(module (memory 1)
			(func (export "f") (result i32)
				(i32.store (i32.const 8) (i32.const 32))
				(i32.store (i32.const 32) (i32.const 99))
				(i32.load (i32.load (i32.const 8)))))`,
			`M.exports["f"]()`, "99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sameAtEveryLevel(t, c.wat, c.expr, c.want)
		})
	}
}

// The traps the peephole must not delete or reorder. Each of these was a real
// failure the conformance suite caught while the pass was being written.
func TestTrapsSurviveThePeephole(t *testing.T) {
	cases := []struct {
		name string
		wat  string
		want string
	}{
		{"a dropped divide still traps", `(module (func (export "f")
			(drop (i32.div_u (i32.const 1) (i32.const 0)))))`,
			"TRAP\tinteger divide by zero"},
		{"a dropped load still traps", `(module (memory 1) (func (export "f")
			(drop (i32.load (i32.const 100000)))))`,
			"TRAP\tout of bounds memory access"},
		{"a select arm still traps", `(module (func (export "f") (result i32)
			(select (i32.const 1) (i32.div_u (i32.const 1) (i32.const 0)) (i32.const 1))))`,
			"TRAP\tinteger divide by zero"},
		{"and with zero still evaluates its operand", `(module (func (export "f") (result i32)
			(i32.and (i32.div_u (i32.const 1) (i32.const 0)) (i32.const 0))))`,
			"TRAP\tinteger divide by zero"},
		{"or with all ones still evaluates its operand", `(module (func (export "f") (result i32)
			(i32.or (i32.div_u (i32.const 1) (i32.const 0)) (i32.const 4294967295))))`,
			"TRAP\tinteger divide by zero"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sameAtEveryLevel(t, c.wat, `M.exports["f"]()`, c.want)
		})
	}
}

// A store between a load and its consumer changes what the load would see, so
// the load cannot move across it.
func TestAForwardedLoadDoesNotCrossAStore(t *testing.T) {
	sameAtEveryLevel(t, `(module (memory 1)
		(func (export "f") (result i32)
			(i32.store (i32.const 16) (i32.const 1))
			(i32.add (i32.load (i32.const 16))
				(block (result i32) (i32.store (i32.const 16) (i32.const 100)) (i32.const 0)))))`,
		`M.exports["f"]()`, "1")
}

// The same hazard through a local rather than through memory: the M4 emitter
// had a test for exactly this, and generalised forwarding has to keep it.
func TestAForwardedLocalDoesNotCrossAWriteToIt(t *testing.T) {
	sameAtEveryLevel(t, `(module
		(func (export "f") (result i32) (local $x i32)
			(local.set $x (i32.const 5))
			(i32.add (local.get $x)
				(block (result i32) (local.set $x (i32.const 100)) (i32.const 0)))))`,
		`M.exports["f"]()`, "5")
}

// A global written by a callee invalidates a pending read of it.
func TestAForwardedGlobalDoesNotCrossACall(t *testing.T) {
	sameAtEveryLevel(t, `(module
		(global $g (mut i32) (i32.const 5))
		(func $bump (global.set $g (i32.const 100)))
		(func (export "f") (result i32)
			(i32.add (global.get $g) (block (result i32) (call $bump) (i32.const 0)))))`,
		`M.exports["f"]()`, "5")
}

// Typed-slot promotion, end to end: the frame store and load disappear and the
// answer does not change.
func TestTypedSlotPromotion(t *testing.T) {
	wat := `(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (param $x f64) (result f64) (local $fp i32)
			(global.set $sp (local.tee $fp (i32.sub (global.get $sp) (i32.const 16))))
			(f64.store offset=8 (local.get $fp) (f64.mul (local.get $x) (f64.const 3)))
			(global.set $sp (i32.add (local.get $fp) (i32.const 16)))
			(f64.load offset=8 (local.get $fp))))`

	// Scoped to the FUNCTION BODY, not the whole chunk. The module epilogue
	// names ld_f64 and st_f64 in the memio bridge the host-call ABI marshals
	// through, so a chunk-wide search reports "not promoted" for a function
	// that was.
	one := functionBody(emitBody(t, wat, analysis.O1), "f")
	two := functionBody(emitBody(t, wat, analysis.O2), "f")
	if !strings.Contains(one, "st_f64(") {
		t.Errorf("-opt=1 should still go through memory:\n%s", one)
	}
	if strings.Contains(two, "st_f64(") || strings.Contains(two, "ld_f64(") {
		t.Errorf("-opt=2 should have promoted the slot out of memory:\n%s", two)
	}
	if !strings.Contains(emitBody(t, wat, analysis.O2), "promoted shadow-stack slots") {
		t.Errorf("-opt=2 should say what it promoted:\n%s", two)
	}
	sameAtEveryLevel(t, wat, `M.exports["f"](2.5)`, "7.5")
}

// A promoted slot starts at zero. The frame's memory did not: it holds whatever
// the previous call left, and a Lua local would hold nil.
func TestPromotedSlotsAreZeroInitialised(t *testing.T) {
	sameAtEveryLevel(t, `(module (memory 1) (global $sp (mut i32) (i32.const 65536))
		(func (export "f") (result i32) (local $fp i32)
			(global.set $sp (local.tee $fp (i32.sub (global.get $sp) (i32.const 16))))
			(global.set $sp (i32.add (local.get $fp) (i32.const 16)))
			(i32.load offset=8 (local.get $fp))))`,
		`M.exports["f"]()`, "0")
}

// Upvalue promotion keeps the functions in F -- call_indirect and the export
// table both need them there -- and adds a second name for the hot ones.
func TestUpvaluePromotion(t *testing.T) {
	wat := `(module
		(func $add (param i32) (param i32) (result i32)
			(i32.add (local.get 0) (local.get 1)))
		(func (export "f") (param $n i32) (result i32)
			(local $s i32) (local $i i32)
			(block (loop
				(br_if 1 (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $s (call $add (local.get $s) (local.get $i)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br 0)))
			(local.get $s)))`

	two := emitBody(t, wat, analysis.O2)
	three := emitBody(t, wat, analysis.O3)
	if strings.Contains(two, "local fu0") {
		t.Errorf("-opt=2 must not spend chunk locals on upvalues:\n%s", two)
	}
	if !strings.Contains(three, "local fu0") || !strings.Contains(three, "fu0 = F[0]") {
		t.Errorf("-opt=3 should declare and bind the hot callee:\n%s", three)
	}
	if !strings.Contains(three, "F[0] = function") {
		t.Error("the function must still LIVE in F: call_indirect dispatches through it")
	}
	// The binding has to come after every definition, or a forward call reads nil.
	if strings.Index(three, "fu0 = F[0]") < strings.Index(three, "F[1] = function") {
		t.Error("upvalues must be bound after the last definition")
	}
	sameAtEveryLevel(t, wat, `M.exports["f"](10)`, "45")
}

// Frame-stack spilling: the function that used to be refused outright now
// compiles, runs, and gives back its frame.
func TestFrameStackSpilling(t *testing.T) {
	const n = ir.MaxSlots + 20
	var sb strings.Builder
	sb.WriteString(`(module (func (export "f") (param $p i32) (result i32)`)
	for i := 0; i < n; i++ {
		sb.WriteString(" (local i32)")
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, " (local.set %d (i32.add (local.get $p) (i32.const %d)))", i+1, i)
	}
	sb.WriteString(" (i32.const 0)")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, " (local.get %d) (i32.add)", i+1)
	}
	sb.WriteString("))")
	wat := sb.String()

	src := emitBody(t, wat, analysis.O0)
	if !strings.Contains(src, "local FS, FP = {}, 0") {
		t.Fatalf("a function past the budget should declare the frame stack:\n%s", src)
	}
	if !strings.Contains(src, "local fb = FP FP = FP +") {
		t.Error("the spilling function should take a frame")
	}
	if !strings.Contains(src, "FP = fb return") {
		t.Error("the frame has to be given back before the return")
	}
	// Reset at the entry point, because a trap unwinds past the epilogue.
	if !strings.Contains(src, `exports["f"] = function(...) FP = 0 return`) {
		t.Error("an entry point must reset FP")
	}

	want := 0
	for i := 0; i < n; i++ {
		want += 7 + i
	}
	sameAtEveryLevel(t, wat, `M.exports["f"](7)`, fmt.Sprint(want))
}

// Spilling is a capability rather than an optimization, so it has to work with
// every pass switched off -- and the frame stack must not appear at all in a
// module that does not need it.
func TestNoFrameStackWhenNothingSpills(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (result i32) (i32.const 1)))`, analysis.O3)
	if strings.Contains(src, "local FS") {
		t.Error("a module that does not spill must not pay two chunk locals for FS and FP")
	}
	if strings.Contains(src, "FP = 0 return") {
		t.Error("exports should not be wrapped when there is no frame pointer to reset")
	}
}

// The one case spilling cannot fix: a parameter is a Lua local because it is in
// the parameter list, and there is nowhere else for the caller to put it.
func TestTooManyParametersIsStillRefused(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`(module (func (export "f")`)
	for i := 0; i < ir.MaxSlots+5; i++ {
		sb.WriteString(" (param i32)")
	}
	sb.WriteString("))")
	m, err := wasm.DecodeWAT(sb.String())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	_, err = EmitModuleWith(im, Options{Opt: analysis.O2})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "parameters") {
		t.Errorf("the message should name the parameter case: %v", err)
	}
}

// Exact NaN mode routes every float operation through a helper, and the
// peephole has to substitute into those the same way.
func TestExactModeAgreesWithTheOptimizer(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	wat := `(module (memory 1)
		(func (export "f") (result i32)
			(f32.store (i32.const 0) (f32.const nan:0x200000))
			(i32.load (i32.const 0))))`
	for _, lvl := range allLevels {
		m, _ := wasm.DecodeWAT(wat)
		im, _ := ir.BuildModule(m)
		src, err := EmitModuleWith(im, Options{Opt: lvl, NaN: NaNExact})
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		out, err := h.RunString("local M = (function(...)\n" + src +
			"\nend)()\nprint(M.exports[\"f\"]())\n")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := strings.TrimSpace(out); got != "2141192192" {
			t.Errorf("-opt=%s in exact mode: got %q, want the payload preserved", lvl, got)
		}
	}
}

// A trap the optimizer can see the VALUE of must still happen.
//
// Every constant-specialised lowering prints `u32(k)` where operand 1's
// expression would go, and at -opt>=1 the constant comes from the range
// analysis rather than from an i32.const. A trapping operand can have an exact
// range -- div_u by anything is [0,0] when its dividend is zero -- so the
// consumer folded the constant, the forwarder deleted the divide, and the trap
// disappeared at the default optimization level. Present from M5, found at M5a.
func TestAConstantFoldedOperandStillTraps(t *testing.T) {
	const div0 = "TRAP\tinteger divide by zero"

	for _, tc := range []struct{ name, op string }{
		// The result is a constant, so the whole expression folds and every
		// mention of the divide goes with it.
		{"mul", `(i32.mul (i32.const 7) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"and", `(i32.and (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"or", `(i32.or (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"xor", `(i32.xor (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		// A shift by a constant zero is the identity, which names its shift
		// amount nowhere.
		{"shl", `(i32.shl (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"shr_u", `(i32.shr_u (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"shr_s", `(i32.shr_s (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"rotl", `(i32.rotl (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
		{"rotr", `(i32.rotr (i32.const 5) (i32.div_u (i32.const 0) (local.get $z)))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wat := `(module (func (export "f") (param $z i32) (result i32) ` + tc.op + `))`
			sameAtEveryLevel(t, wat, `M.exports["f"](0)`, div0)
		})
	}
}

// And the fold itself is not what got reverted. Refusing the forward costs one
// statement on the trapping path and nothing at all on the common one: the
// divide gets its own assignment back, and the multiply STILL specialises,
// because the fold reads the range and never read the expression.
func TestRefusingTheForwardKeepsTheFold(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $z i32) (result i32)
		(i32.mul (i32.const 7) (i32.div_u (i32.const 0) (local.get $z)))))`,
		analysis.O1)
	if !strings.Contains(src, "div_u(") {
		t.Errorf("the divide must survive as its own statement:\n%s", src)
	}
	if strings.Contains(src, "mul32(") {
		t.Errorf("but the multiply keeps its constant path:\n%s", src)
	}

	// The ordinary case is untouched: nothing here can trap, so nothing is
	// refused and the specialisation is the whole lowering.
	plain := emitBody(t, `(module (func (export "f") (param $c i32) (result i32)
		(i32.mul (local.get $c) (i32.const 8))))`, analysis.O1)
	if strings.Contains(plain, "mul32(") {
		t.Errorf("constant-multiply specialisation is not optional:\n%s", plain)
	}
}

// Promotion must back off as the chunk fills, and it must leave the margin it
// says it leaves.
//
// This is what makes -opt=3 safe to default to: a guest that compiles at -opt=2
// can never fail at -opt=3 because a hot callee took the last name. Lua's limit
// is 200, and the emitter refuses the module past it -- so a promotion pass that
// merely tried to be careful would turn an optimization into a compile error on
// somebody else's guest.
//
// It also pins trailingChunkLocals, which cannot be measured where it is used:
// `local exports` is emitted after promotion has already chosen. Add another
// trailing chunk local without updating the constant and the landed count
// crosses the margin here.
func TestPromotionLeavesTheMarginItPromises(t *testing.T) {
	// Globals are the cheapest way to crowd a chunk: one local each, and real
	// guests do emit them. Sweeping means the property is checked as promotion
	// backs off, not only at one arbitrary fill level.
	for _, globals := range []int{0, 10, 20, 26, 30, 32} {
		var sb strings.Builder
		sb.WriteString("(module\n")
		for g := 0; g < globals; g++ {
			fmt.Fprintf(&sb, "(global $g%d (mut i32) (i32.const %d))\n", g, g)
		}
		const funcs = 40
		for f := 0; f < funcs; f++ {
			fmt.Fprintf(&sb, "(func $f%d (param i32) (result i32) local.get 0 i32.const %d i32.add)\n", f, f)
		}
		sb.WriteString(`(func (export "run") (result i32) (local $a i32)` + "\n")
		for f := 0; f < funcs; f++ {
			fmt.Fprintf(&sb, "(local.set $a (call $f%d (local.get $a)))\n", f)
		}
		sb.WriteString("(local.get $a)))")

		// emitAt, not emitBody: the budget is the WHOLE chunk, and emitBody
		// drops the prelude, which is the ~167 locals that make this tight.
		src := emitAt(t, sb.String(), analysis.O3)
		got := countChunkLocals(src)
		limit := maxChunkLocals - upvalueMargin
		if got > limit {
			t.Errorf("%d globals: chunk landed at %d locals, over the %d the margin promises "+
				"(Lua's hard limit is %d)", globals, got, limit, maxChunkLocals)
		}
		// And the backoff has to be real rather than promotion simply never
		// firing: with room, something must actually be promoted.
		if globals == 0 && !strings.Contains(src, "local fu") {
			t.Error("an empty chunk should have room to promote something")
		}
	}
}

// A float comparison consumed by `if` must still be false for a NaN.
//
// `if` jumps to its else-label when the condition is FALSE, so it asks for the
// NEGATED comparison -- and negating a float comparison by swapping its
// operator is wrong, because `lt` and `ge` are both false when an operand is a
// NaN. The swap left the negated test false too, so the then-arm ran on a NaN
// where wasm takes the else-arm: a wrong answer at every level from -opt=1,
// which is to say at the default. Exact mode never had it, because it negates
// the helper's 0/1 result rather than the operator.
//
// The conformance suite could not see this. It asserts float comparisons
// through their materialised 0/1 result, which is the un-negated path, and
// if.wast branches on i32 conditions.
func TestFloatComparisonsInAnIfAreFalseForNaN(t *testing.T) {
	// (if (op x 1.0) (then (return 1))) (return 0) -- so the answer IS the
	// comparison, taken through the branch rather than materialised.
	const shape = `(module (func (export "f") (param %[1]s) (result i32)
		(if (%[1]s.%[2]s (local.get 0) (%[1]s.const 1.0))
			(then (return (i32.const 1))))
		(i32.const 0)))`

	// ne is the one that is TRUE for a NaN; every other comparison is false.
	for _, tc := range []struct {
		op                     string
		nan, half, one, double string
	}{
		{"lt", "0", "1", "0", "0"},
		{"le", "0", "1", "1", "0"},
		{"gt", "0", "0", "0", "1"},
		{"ge", "0", "0", "1", "1"},
		{"eq", "0", "0", "1", "0"},
		{"ne", "1", "1", "0", "1"},
	} {
		for _, width := range []string{"f32", "f64"} {
			for _, arg := range []struct{ lua, want string }{
				{"0/0", tc.nan}, {"0.5", tc.half}, {"1.0", tc.one}, {"2.0", tc.double},
			} {
				wat := fmt.Sprintf(shape, width, tc.op)
				expr := fmt.Sprintf(`M.exports["f"](%s)`, arg.lua)
				for _, lvl := range allLevels {
					for _, nan := range []NaNMode{NaNCanonical, NaNExact} {
						got := runAtMode(t, wat, expr, lvl, nan)
						if got != arg.want {
							t.Errorf("%s.%s(%s, 1.0) at -opt=%s --nan=%s: got %q, want %q",
								width, tc.op, arg.lua, lvl, nan, got, arg.want)
						}
					}
				}
			}
		}
	}
}

// A wrap may only be deferred to a consumer that re-reduces modulo 2^32, and a
// shift by 0 mod 32 lowers to the IDENTITY, which re-reduces nothing.
//
// The deferred value is merely congruent to the wasm value -- here a negative
// number where the wasm value is near 2^32 -- so handing it to an identity puts
// it in front of a consumer that reads the bit pattern. An unsigned compare
// then answers about the wrong number; an address would trap on memory the
// guest owns.
func TestShiftByZeroDoesNotAbsorbADeferredWrap(t *testing.T) {
	// 0 - 1 is 4294967295, which is NOT below 5. With the wrap deferred and the
	// shift an identity, the compare sees -1 and says it is.
	for _, dist := range []string{"0", "32", "64"} {
		wat := fmt.Sprintf(`(module (func (export "f") (param i32) (param i32) (result i32)
			(i32.lt_u
				(i32.shl (i32.sub (local.get 0) (local.get 1)) (i32.const %s))
				(i32.const 5))))`, dist)
		sameAtEveryLevel(t, wat, `M.exports["f"](0, 1)`, "0")
	}

	// The optimization itself must survive the guard: a REAL shift masks its
	// operand first, so the sub's wrap is still deferred into it.
	const real = `(module (func (export "f") (param i32) (param i32) (result i32)
		(i32.shl (i32.sub (local.get 0) (local.get 1)) (i32.const 4))))`
	src := emitBody(t, real, analysis.O1)
	if strings.Contains(src, "% 4294967296.0") {
		t.Errorf("a shift by 4 masks with %% 2^28 first, so the sub's wrap should "+
			"still be deferred into it:\n%s", src)
	}
	sameAtEveryLevel(t, real, `M.exports["f"](0, 1)`, "4294967280")
}
