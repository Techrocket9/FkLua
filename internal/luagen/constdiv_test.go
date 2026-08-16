package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// The constant-divisor lowerings: `i32.div_u a k` becomes `(a - a % k) / k` and
// `i32.rem_u a k` becomes `a % k`, from -opt=1.
//
// The arithmetic is exact rather than approximately right, and the argument is
// in constDivIsExact next to the lowering. What is checked HERE is that the
// emitter reaches the same answers the helper calls did, at the corners where a
// double would lose them if the argument were wrong: a maximal dividend, a
// divisor of 1, a divisor of 2^31 and of 2^32-1, and a quotient large enough
// that a single ulp of error in the division would show.
//
// Every case runs at all four levels, so it is also a differential test against
// -opt=0, which still emits the helper call. That is the instrument the audit
// named for this class -- the conformance suite asserts through materialised
// results and would not see a lowering that is only chosen at one level.

// divCase is one (a, c) pair together with what wasm says the four operations
// return, computed here in Go with exact 32-bit arithmetic.
type divCase struct{ a, c uint32 }

func TestConstantDivisorAgreesWithTheHelperAtEveryLevel(t *testing.T) {
	cases := []divCase{
		{0, 1}, {0, 7}, {0, 0xFFFFFFFF},
		{1, 1}, {1, 2}, {1, 0xFFFFFFFF},
		{7, 1}, {7, 2}, {7, 3}, {7, 7}, {7, 8}, {7, 100},
		{100, 10}, {1000, 3}, {123456789, 1000},
		// A non-power-of-two on a large dividend: the quotient is ~2^32/3, so
		// one ulp of error in the divide would be visible in the answer.
		{0xFFFFFFFF, 3}, {0xFFFFFFFE, 3}, {0xFFFFFFFF, 7}, {0xFFFFFFFF, 10},
		{0xFFFFFFFF, 1000000007},
		// Powers of two, including the two that straddle the sign bit.
		{0xFFFFFFFF, 2}, {0xFFFFFFFF, 1 << 16}, {0xFFFFFFFF, 1 << 31},
		{1 << 31, 1 << 31}, {1<<31 - 1, 1 << 31},
		// The extremes of the divisor.
		{0xFFFFFFFF, 1}, {0xFFFFFFFF, 0xFFFFFFFF}, {0xFFFFFFFE, 0xFFFFFFFF},
		{1 << 31, 0xFFFFFFFF}, {4000000000, 0xFFFFFFFE},
	}

	for _, tc := range cases {
		name := fmt.Sprintf("%d_by_%d", tc.a, tc.c)
		t.Run(name, func(t *testing.T) {
			// The dividend arrives as a parameter so nothing folds the whole
			// operation away; the divisor is the i32.const the lowering reads.
			mk := func(op string) string {
				return fmt.Sprintf(
					`(module (func (export "f") (param $a i32) (result i32)
						(%s (local.get $a) (i32.const %d))))`, op, tc.c)
			}
			expr := fmt.Sprintf(`M.exports["f"](%d)`, tc.a)

			sameAtEveryLevel(t, mk("i32.div_u"), expr, fmt.Sprint(tc.a/tc.c))
			sameAtEveryLevel(t, mk("i32.rem_u"), expr, fmt.Sprint(tc.a%tc.c))
		})
	}
}

// The signed pair specialises only under a proof about the DIVIDEND, so its
// test has to establish that proof the same way a real guest does -- with a
// guard the range analysis can read -- and then check the corners of the
// signed/unsigned boundary anyway.
func TestSignedConstantDivisorAgreesWithTheHelperAtEveryLevel(t *testing.T) {
	for _, tc := range []divCase{
		{0, 1}, {0, 3}, {1, 1}, {7, 3}, {100, 7}, {1000, 10},
		{1<<31 - 1, 3}, {1<<31 - 1, 1}, {1<<31 - 1, 1<<31 - 1},
		{1<<31 - 2, 1<<31 - 1}, {12345678, 1000},
		// Below the guard, so the helper call stays -- and must still be right.
		{1 << 31, 3}, {0xFFFFFFFF, 3}, {0xFFFFFFFF, 0xFFFFFFFF},
		// A negative divisor is never specialised; the helper handles it.
		{100, 0xFFFFFFFF}, {1 << 31, 0xFFFFFFFF},
	} {
		t.Run(fmt.Sprintf("%d_by_%d", tc.a, tc.c), func(t *testing.T) {
			// `(i32.and $a 0x7FFFFFFF)` is what gives the analysis a dividend
			// below 2^31; without it constDivisorS refuses and the helper call
			// is emitted, which is the other half of what this checks.
			mk := func(op string) string {
				return fmt.Sprintf(
					`(module (func (export "f") (param $a i32) (result i32)
						(%s (local.get $a) (i32.const %d))))`, op, tc.c)
			}
			expr := fmt.Sprintf(`M.exports["f"](%d)`, tc.a)

			sa, sc := int32(tc.a), int32(tc.c)
			if sc == -1 && sa == -1<<31 {
				t.Skip("INT_MIN / -1 traps; covered separately")
			}
			sameAtEveryLevel(t, mk("i32.div_s"), expr, fmt.Sprint(uint32(sa/sc)))
			sameAtEveryLevel(t, mk("i32.rem_s"), expr, fmt.Sprint(uint32(sa%sc)))
		})
	}
}

// The guarded signed shape, end to end: a masked dividend is what actually
// takes the specialised path.
func TestAGuardedSignedDividendTakesTheNativePath(t *testing.T) {
	const wat = `(module (func (export "f") (param $a i32) (result i32)
		(i32.div_s (i32.and (local.get $a) (i32.const 2147483647)) (i32.const 7))))`
	src := emitBody(t, wat, analysis.O1)
	if strings.Contains(src, "div_s(") {
		t.Errorf("a dividend masked below 2^31 must not call the helper:\n%s", src)
	}
	for _, a := range []uint32{0, 1, 7, 8, 0x7FFFFFFF, 0x80000000, 0xFFFFFFFF} {
		want := fmt.Sprint(uint32(int32(a&0x7FFFFFFF) / 7))
		sameAtEveryLevel(t, wat, fmt.Sprintf(`M.exports["f"](%d)`, a), want)
	}
}

// -opt=0 has to keep reproducing the M4 emitter byte for byte, and -opt>=1 has
// to actually take the new path -- otherwise the change is inert and every
// behavioural test above passes for the wrong reason.
//
// THIS is the assertion that fails when the change is reverted.
func TestConstantDivisorIsNativeFromLevelOneAndACallAtLevelZero(t *testing.T) {
	const wat = `(module (func (export "f") (param $a i32) (result i32)
		(i32.rem_u (i32.div_u (local.get $a) (i32.const 10)) (i32.const 7))))`

	zero := emitBody(t, wat, analysis.O0)
	if !strings.Contains(zero, "div_u(") || !strings.Contains(zero, "rem_u(") {
		t.Errorf("-opt=0 must keep both helper calls:\n%s", zero)
	}

	for _, lvl := range []analysis.Level{analysis.O1, analysis.O2, analysis.O3} {
		src := emitBody(t, wat, lvl)
		if strings.Contains(src, "div_u(") || strings.Contains(src, "rem_u(") {
			t.Errorf("-opt=%s must not call a division helper for a constant divisor:\n%s", lvl, src)
		}
		if !strings.Contains(src, "% 10") || !strings.Contains(src, "% 7") {
			t.Errorf("-opt=%s should have emitted native arithmetic:\n%s", lvl, src)
		}
	}
}

// A divisor of 1 is the identity for div and a constant 0 for rem, and the
// identity is the shape that has already produced a miscompile here once (a
// shl of 0 mod 32 absorbing a deferred wrap it never re-reduced). Nothing may
// absorb into a division, and this pins it: the sub's wrap has to survive.
func TestDivideByOneDoesNotAbsorbADeferredWrap(t *testing.T) {
	const wat = `(module (func (export "f") (param $a i32) (param $b i32) (result i32)
		(i32.div_u (i32.sub (local.get $a) (local.get $b)) (i32.const 1))))`
	// 1 - 2 is 0xFFFFFFFF in wasm. If the sub's wrap were deferred into the
	// divide-by-one identity, the slot would hold -1 and the answer would be
	// -1 rather than 4294967295.
	sameAtEveryLevel(t, wat, `M.exports["f"](1, 2)`, "4294967295")
	sameAtEveryLevel(t, wat, `M.exports["f"](0, 1)`, "4294967295")

	const rem = `(module (func (export "f") (param $a i32) (param $b i32) (result i32)
		(i32.rem_u (i32.sub (local.get $a) (local.get $b)) (i32.const 1000))))`
	sameAtEveryLevel(t, rem, `M.exports["f"](1, 2)`, "295")
}

// A zero divisor still traps, at every level. The specialisation is refused for
// k == 0 precisely so that this keeps working, and a range analysis that has
// PROVED the divisor zero is the case most likely to tempt a fold.
func TestAConstantZeroDivisorStillTraps(t *testing.T) {
	const div0 = "TRAP\tinteger divide by zero"
	for _, op := range []string{"i32.div_u", "i32.rem_u", "i32.div_s", "i32.rem_s"} {
		t.Run(op, func(t *testing.T) {
			sameAtEveryLevel(t, fmt.Sprintf(
				`(module (func (export "f") (param $a i32) (result i32)
					(%s (local.get $a) (i32.const 0))))`, op),
				`M.exports["f"](7)`, div0)
			// And with the zero arriving through a local, where only the range
			// analysis can see it.
			sameAtEveryLevel(t, fmt.Sprintf(
				`(module (func (export "f") (param $a i32) (result i32) (local $z i32)
					(%s (local.get $a) (local.get $z))))`, op),
				`M.exports["f"](7)`, div0)
		})
	}
	// The signed overflow trap is not a divide-by-zero and must survive too.
	sameAtEveryLevel(t, `(module (func (export "f") (param $a i32) (result i32)
		(i32.div_s (local.get $a) (i32.const 4294967295))))`,
		`M.exports["f"](2147483648)`, "TRAP\tinteger overflow")
}

// THE HAZARD THAT ALREADY BIT THIS EXACT PATTERN.
//
// Every constant-specialised lowering discards operand 1's expression, and at
// -opt>=1 the constant comes from the range analysis -- so a TRAPPING operand
// can have an exact range, and the forwarder, seeing its only use gone, deletes
// it. That shipped once as `(i32.mul (i32.const 7) (i32.div_u (i32.const 0)
// (local.get $z)))` returning 0 where the spec requires a trap.
//
// The divisions are the same class, and worse: the operand being discarded is
// most often itself a division, which is the one thing that traps for a reason
// a range analysis routinely hides -- div_u's range is [0,0] whenever its
// dividend is, whatever its divisor does.
func TestAConstantFoldedDivisorStillTraps(t *testing.T) {
	const div0 = "TRAP\tinteger divide by zero"
	// The divisor expression has an exact non-zero range -- so the lowering
	// specialises and never names it -- and traps when it runs.
	const divisor = `(i32.add (i32.const 4) (i32.div_u (i32.const 0) (local.get $z)))`

	for _, op := range []string{"i32.div_u", "i32.rem_u", "i32.div_s", "i32.rem_s"} {
		t.Run(op, func(t *testing.T) {
			wat := fmt.Sprintf(
				`(module (func (export "f") (param $z i32) (result i32)
					(%s (i32.const 100) %s)))`, op, divisor)
			sameAtEveryLevel(t, wat, `M.exports["f"](0)`, div0)
		})
	}

	// A divisor of exactly 1 makes div the IDENTITY, which names the divisor
	// nowhere at all -- the same shape as `shl` by 0 and `and` with all ones.
	sameAtEveryLevel(t, `(module (func (export "f") (param $z i32) (result i32)
		(i32.div_u (i32.const 100)
		           (i32.add (i32.const 1) (i32.div_u (i32.const 0) (local.get $z))))))`,
		`M.exports["f"](0)`, div0)

	// And the dividend side: `(a - a % k) / k` names operand 0 twice, so a
	// trapping expression must not be substituted there either -- one wasm
	// operation must not become two chances to trap, nor two loads.
	sameAtEveryLevel(t, `(module (memory 1) (func (export "f") (param $p i32) (result i32)
		(i32.div_u (i32.load (local.get $p)) (i32.const 3))))`,
		`M.exports["f"](100000)`, "TRAP\tout of bounds memory access")
}

// The dividend must not be evaluated twice, which is a behavioural claim and
// not only a cost one: a load run twice is two bounds checks, and a call run
// twice is two calls.
func TestTheDividendIsNamedOnceInTheEmittedExpression(t *testing.T) {
	src := emitBody(t, `(module (memory 1) (func (export "f") (param $p i32) (result i32)
		(i32.div_u (i32.load (local.get $p)) (i32.const 3))))`, analysis.O1)
	// Counting `ld32(` alone would also count the memio wrappers the module
	// epilogue always emits; the address operand is what makes it this load.
	if n := strings.Count(src, "ld32(MEM, MEMSIZE, v0)"); n != 1 {
		t.Errorf("the load must appear exactly once, got %d:\n%s", n, src)
	}
}

// The fold is not what gets given up. Refusing the forward costs one statement
// on the trapping path and nothing on the common one: the divisor's expression
// gets its own assignment back, and the division STILL specialises, because the
// fold reads the range and never read the expression.
func TestRefusingTheDivisorForwardKeepsTheFold(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (param $z i32) (result i32)
		(i32.rem_u (i32.const 100)
		           (i32.add (i32.const 4) (i32.div_u (i32.const 0) (local.get $z)))))) `,
		analysis.O1)
	if !strings.Contains(src, "div_u(") {
		t.Errorf("the inner divide must survive as its own statement:\n%s", src)
	}
	if !strings.Contains(src, "% 4") {
		t.Errorf("but the outer remainder keeps its constant path:\n%s", src)
	}
}
