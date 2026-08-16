package analysis

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// nth returns the index of the n-th step with the given op, counting from 0.
func nth(t *testing.T, f *ir.Func, op wasm.Op, n int) int {
	t.Helper()
	seen := 0
	for i := range f.Steps {
		if f.Steps[i].Op != op {
			continue
		}
		if seen == n {
			return i
		}
		seen++
	}
	t.Fatalf("no %v #%d in %d steps", op, n, len(f.Steps))
	return -1
}

// The whole point of the fixpoint, stated as an assertion.
//
// `for (int i = 0; i < n; i++)` in the shape LLVM emits it: rotated, so the
// trip-count guard sits in front and the loop test at the bottom. Block-locally
// nothing is known about either operand at the test, and both the increment's
// wrap and the compare's 2^31 bias survive. With the guard carried across the
// back edge, neither does.
func TestCountedLoopProvesBothSidesOfItsSignedTest(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (local.get $i)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
		(local.get $s)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	// The loop test is the second i32.lt_s; the first is the entry guard.
	test := nth(t, f, wasm.OpI32LtS, 1)
	ra, rb := w.ArgRange(test, 0), w.ArgRange(test, 1)
	if !ra.Below(1 << 31) {
		t.Errorf("the counter must be provably non-negative at the loop test, got %v", ra)
	}
	if !rb.Below(1 << 31) {
		t.Errorf("the trip count must be provably non-negative at the loop test: "+
			"the pre-header guard `n < 1 -> skip` is what says so, and it is the "+
			"only thing that does. Got %v", rb)
	}

	// The increment. Its consumer is a local.set, which absorbs nothing, so an
	// elided wrap here can only mean the result was proved to fit.
	inc := nth(t, f, wasm.OpI32Add, 1)
	if !w.Elided(inc) {
		t.Errorf("i + 1 cannot overflow when i < n <= 2^31-1, so its wrap is "+
			"dead weight on every iteration. Range was %v", w.Result[inc])
	}
}

// The same win where the source has no loop at all: a guard is a fact about
// everything it dominates, and the block-local pass threw it away at the label.
func TestAGuardIsStillTrueAfterTheBranch(t *testing.T) {
	m := build(t, `(module (func $fib (export "f") (param $n i32) (result i32)
		(if (i32.lt_u (local.get $n) (i32.const 2))
			(then (return (local.get $n))))
		(i32.add
			(call $fib (i32.sub (local.get $n) (i32.const 1)))
			(call $fib (i32.sub (local.get $n) (i32.const 2))))))`)
	f := m.Funcs[0]
	w := Ranges(f)

	for k := 0; k < 2; k++ {
		sub := nth(t, f, wasm.OpI32Sub, k)
		if !w.Elided(sub) {
			t.Errorf("n >= 2 on the fall-through of `if n < 2 return`, so "+
				"n - %d cannot go negative and needs no wrap; range was %v",
				k+1, w.Result[sub])
		}
	}
}

// LLVM strength-reduces most counted loops into a countdown, whose guard is a
// bare value rather than a comparison. Measured on TinyGo output, that shape is
// more common than the comparison one.
func TestCountdownLoopKnowsItsCounterIsNonZero(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $s i32)
		(block $done
			(loop $top
				(br_if $done (i32.eqz (local.get $n)))
				(local.set $n (i32.sub (local.get $n) (i32.const 1)))
				(local.set $s (i32.add (local.get $s) (i32.const 3)))
				(br $top)))
		(local.get $s)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	dec := nth(t, f, wasm.OpI32Sub, 0)
	if !w.Elided(dec) {
		t.Errorf("n is non-zero on the edge the guard did not take, so n - 1 "+
			"cannot wrap; range was %v", w.Result[dec])
	}
}

// Soundness, which is the half that a benchmark cannot check. A counter with no
// guard on it really can reach 2^32-1, and eliding its wrap would produce a
// value the rest of the program reads as a 33-bit number.
func TestAnUnboundedCounterKeepsItsWrap(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $c i32) (result i32)
		(local $i i32)
		(loop $top
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if $top (local.get $c)))
		(local.get $i)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	inc := nth(t, f, wasm.OpI32Add, 0)
	if w.Elided(inc) {
		t.Fatalf("nothing bounds this counter, so widening must reach the top "+
			"and the wrap must stay. Range was %v", w.Result[inc])
	}
	if got := w.ArgRange(inc, 0); got != FullU32 {
		t.Errorf("the counter itself is unknown at the loop head; got %v", got)
	}
}

// A local written between a comparison and the branch that reads it makes the
// comparison a statement about a value that is no longer there. Refining on it
// anyway is the exact shape of an unsound narrowing.
func TestALocalWrittenAfterTheCompareIsNotRefined(t *testing.T) {
	m := build(t, `(module (func (export "f") (result i32)
		(local $i i32)
		(local.set $i (i32.const 100))
		(block $out
			local.get $i
			i32.const 3
			i32.lt_u
			i32.const 1
			local.set $i
			br_if $out)
		(local.get $i)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	// The last local.get is after the branch. On the not-taken edge a naive
	// refinement would conclude i >= 3, but i was overwritten with 1.
	last := -1
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpLocalGet {
			last = i
		}
	}
	r := w.Result[last]
	if r.Lo > 1 {
		t.Errorf("i is 1 here, but the range says %v -- the comparison was about "+
			"the value i had BEFORE the local.set, and refining the local on it "+
			"is a miscompile waiting for a signed compare to read it", r)
	}
}

// A merge takes the union, and a path that knows nothing poisons the result.
// Getting this backwards is how a fixpoint quietly becomes an intersection.
func TestMergeJoinsBothArms(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $c i32) (result i32)
		(local $i i32)
		(if (local.get $c)
			(then (local.set $i (i32.const 1)))
			(else (local.set $i (i32.const 7))))
		(i32.add (local.get $i) (i32.const 1))))`)
	f := m.Funcs[0]
	w := Ranges(f)

	add := nth(t, f, wasm.OpI32Add, 0)
	if got := w.ArgRange(add, 0); got != (Range{1, 7}) {
		t.Errorf("after `i = 1` or `i = 7`, i is in [1,7]; got %v", got)
	}
}

func TestMergeWithAnUnknownArmKnowsNothing(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $c i32) (param $n i32) (result i32)
		(local $i i32)
		(if (local.get $c)
			(then (local.set $i (i32.const 1)))
			(else (local.set $i (local.get $n))))
		(i32.add (local.get $i) (i32.const 1))))`)
	f := m.Funcs[0]
	w := Ranges(f)

	add := nth(t, f, wasm.OpI32Add, 0)
	if got := w.ArgRange(add, 0); got != FullU32 {
		t.Errorf("one arm assigned an unknown parameter, so nothing is known at "+
			"the merge; got %v", got)
	}
	if w.Elided(add) {
		t.Error("and therefore the wrap must stay")
	}
}

// A declared local starts at zero -- the spec says so and the emitter's
// prologue writes it -- and that is where a counted loop's induction variable
// gets its lower bound from.
func TestDeclaredLocalsStartAtZero(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $p i32) (result i32)
		(local $i i32)
		(i32.add (local.get $i) (local.get $p))))`)
	f := m.Funcs[0]
	w := Ranges(f)

	add := nth(t, f, wasm.OpI32Add, 0)
	if got := w.ArgRange(add, 0); got != (Range{0, 0}) {
		t.Errorf("a declared local starts at 0; got %v", got)
	}
	if got := w.ArgRange(add, 1); got != FullU32 {
		t.Errorf("a parameter starts at whatever the caller passed; got %v", got)
	}
}

// Widening has to terminate on shapes that are not counted loops at all. An
// irreducible graph is the case where "back edge" and "retreating edge" part
// company, and the property that keeps the iteration finite -- that reverse
// postorder is a topological order of everything else -- holds for both.
func TestIrreducibleControlFlowTerminates(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $c i32) (result i32)
		(local $i i32)
		(block $exit
			(loop $a
				(br_if $exit (i32.eqz (local.get $c)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(loop $b
					(br_if $exit (i32.eqz (local.get $i)))
					(local.set $i (i32.sub (local.get $i) (i32.const 1)))
					(br_if $b (local.get $c))
					(br $a))))
		(local.get $i)))`)
	// Reaching here at all is the assertion: a non-terminating fixpoint hangs
	// the compiler rather than failing it.
	w := Ranges(m.Funcs[0])
	if w == nil {
		t.Fatal("no analysis produced")
	}
}

// Deeply nested loops are where a widening budget that is too small shows up as
// lost precision rather than as a failure, so the inner counter is checked.
func TestNestedLoopsBothConverge(t *testing.T) {
	m := build(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $j i32) (local $s i32)
		(block $o
			(loop $ol
				(br_if $o (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $j (i32.const 0))
				(block $in
					(loop $il
						(br_if $in (i32.ge_u (local.get $j) (local.get $n)))
						(local.set $s (i32.add (local.get $s) (local.get $j)))
						(local.set $j (i32.add (local.get $j) (i32.const 1)))
						(br $il)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $ol)))
		(local.get $s)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	// adds, in step order: s + j, j + 1, i + 1.
	if !w.Elided(nth(t, f, wasm.OpI32Add, 1)) {
		t.Error("the inner counter's increment is bounded by the inner guard")
	}
	if !w.Elided(nth(t, f, wasm.OpI32Add, 2)) {
		t.Error("the outer counter's increment is bounded by the outer guard")
	}
}

// The fixpoint must not turn a wrap the emitter still needs into one it skips
// just because the value was a constant on one path.
func TestConstantLoopBoundConvergesOnTheConstant(t *testing.T) {
	m := build(t, `(module (func (export "f") (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (i32.const 100)))
				(local.set $s (i32.add (local.get $s) (local.get $i)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $s)))`)
	f := m.Funcs[0]
	w := Ranges(f)

	cmp := nth(t, f, wasm.OpI32GeU, 0)
	got := w.ArgRange(cmp, 0)
	if got.Hi > 100 {
		t.Errorf("a loop bounded by a literal settles on that literal; got %v", got)
	}
	if !w.Elided(nth(t, f, wasm.OpI32Add, 1)) {
		t.Error("i + 1 with i < 100 needs no wrap")
	}
}
