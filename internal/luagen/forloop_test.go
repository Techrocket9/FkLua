package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The counted-loop lowering rewrites CONTROL FLOW, which is the one part of the
// emitter where a wrong answer does not look like a wrong answer: the loop still
// runs, still terminates and still returns a number. So every case here asserts
// a VALUE at every level rather than inspecting text, which is what
// CLAUDE.md's note about the conformance suite's blind spots asks for -- the
// suite asserts materialised results and would not see a trip count that is off
// by one at -opt=1 and right at -opt=0.

// A trip count of zero is the case the two loop shapes disagree about. A
// bottom-tested loop runs its body before testing anything; Lua's `for` tests
// first. If the lowering ever forgets that, this returns 0 where wasm returns 1.
func TestATopTestedLoopThatRunsZeroTimesStillRunsZeroTimes(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $s (i32.add (local.get $s) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0")
	sameAtEveryLevel(t, wat, `M.exports["f"](1)`, "1")
	sameAtEveryLevel(t, wat, `M.exports["f"](7)`, "7")
}

// A bottom-tested loop ALWAYS runs once. Lowering it needs the pre-header guard
// to prove that, and the guard is outside the loop -- so this is the case that
// catches a lowering which trusted the shape instead of the proof.
func TestABottomTestedLoopAlwaysRunsItsBodyOnce(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0") // the guard skips it
	sameAtEveryLevel(t, wat, `M.exports["f"](1)`, "1")
	sameAtEveryLevel(t, wat, `M.exports["f"](9)`, "9")
}

// The miscompile the entry proof exists to prevent, written out.
//
// This bottom-tested loop has NO adequate pre-header guard and enters with the
// counter already past its bound, so wasm runs the body exactly once -- it tests
// afterwards -- while `for i = i, 4` with i = 10 runs it zero times. Nothing
// about the loop looks wrong; it just returns 0 instead of 1.
//
// Confirmed to fail: replacing the enterUnconditionally call with `false &&`
// reports `-opt=1: got "0", want "1"` at every level above zero.
func TestABottomTestedLoopEnteredPastItsBoundStillRunsOnce(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(local.set $i (local.get $n))
		(loop $top
			(local.set $s (i32.add (local.get $s) (i32.const 1)))
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if $top (i32.lt_u (local.get $i) (i32.const 5))))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](10)`, "1")
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "5")
	if src := emitBody(t, wat, analysis.O1); strings.Contains(src, "for v") {
		t.Errorf("a loop that cannot prove it runs once must keep its goto:\n%s", src)
	}
}

// The counter after the loop. Lua's `for` variable does not outlive its loop, so
// the outer name is stale unless the emitter puts the right value back -- and
// "the right value" is not the bound when the loop ran zero times.
func TestTheCounterHasItsWasmValueAfterTheLoop(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $i)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0")
	sameAtEveryLevel(t, wat, `M.exports["f"](5)`, "5")
}

// The shape real compiler output actually has: a countdown whose test is the
// local.tee itself, with no comparison anywhere. Until this was recognised the
// pass found nothing at all in a TinyGo guest.
func TestACountdownTestedByItsOwnTeeIsLowered(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $s i32)
		(block $done
			(br_if $done (i32.eqz (local.get $n)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (i32.const 2)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0")
	sameAtEveryLevel(t, wat, `M.exports["f"](1)`, "2")
	sameAtEveryLevel(t, wat, `M.exports["f"](6)`, "12")

	if src := emitBody(t, wat, analysis.O1); !strings.Contains(src, "for v0 = v0, 1, -1 do") {
		t.Errorf("the tee-tested countdown should lower to a descending for:\n%s", src)
	}
}

// A loop with a second way out is refused, and refusing it has to leave a loop
// that still works. This is the largest refusal class in real guest output, so
// the fallback path is not a corner.
func TestALoopWithTwoExitsStillComputesTheRightThing(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (param $stop i32) (result i32)
		(local $i i32) (local $s i32)
		(block $out
			(block $done
				(loop $top
					(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
					(br_if $out (i32.eq (local.get $i) (local.get $stop)))
					(local.set $s (i32.add (local.get $s) (i32.const 1)))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top))))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](10, 3)`, "3")
	sameAtEveryLevel(t, wat, `M.exports["f"](10, 99)`, "10")
}

// A body that writes the counter is not a counted loop. If the lowering took it
// anyway, Lua's `for` would overwrite the body's write on every iteration and
// the loop would run a different number of times.
func TestABodyThatWritesTheCounterIsNotLowered(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
		(local.get $s)))`
	// i advances by two, so a bound of 10 gives five iterations.
	sameAtEveryLevel(t, wat, `M.exports["f"](10)`, "5")
	if src := emitBody(t, wat, analysis.O1); strings.Contains(src, "for v") {
		t.Errorf("a loop whose body writes the counter must keep its goto:\n%s", src)
	}
}

// Nested counted loops, so the emitter's `for`/`end` bookkeeping is exercised
// against itself rather than against one loop at a time.
func TestNestedCountedLoopsNest(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $j i32) (local $s i32)
		(block $od
			(loop $ot
				(br_if $od (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $j (i32.const 0))
				(block $id
					(loop $it
						(br_if $id (i32.ge_u (local.get $j) (local.get $n)))
						(local.set $s (i32.add (local.get $s) (i32.const 1)))
						(local.set $j (i32.add (local.get $j) (i32.const 1)))
						(br $it)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $ot)))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0")
	sameAtEveryLevel(t, wat, `M.exports["f"](5)`, "25")
}

// -opt=0 is the bisect reference and must keep the goto loop.
func TestLevelZeroKeepsTheGotoLoop(t *testing.T) {
	src := emitBody(t, counted, analysis.O0)
	if strings.Contains(src, "for v") {
		t.Errorf("-opt=0 must not lower a counted loop:\n%s", src)
	}
	if !strings.Contains(src, "::L1::") {
		t.Errorf("-opt=0 must keep the loop label:\n%s", src)
	}
}

// --fuel charges per ITERATION, and the charge has to move inside the `for` --
// left in front of it, a lowered loop would be charged once for the whole trip.
func TestFuelIsStillChargedPerIterationInsideAFor(t *testing.T) {
	m, err := wasm.DecodeWAT(counted)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: analysis.O1, Fuel: 1 << 20})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	i := strings.Index(src, "for v1 = v1")
	if i < 0 {
		t.Fatalf("expected the loop to be lowered:\n%s", src)
	}
	rest := src[i:]
	fuel := strings.Index(rest, "FUEL = FUEL - 1")
	end := strings.Index(rest, "\n  end")
	if fuel < 0 || (end >= 0 && fuel > end) {
		t.Errorf("the fuel charge must be INSIDE the for body:\n%s", rest)
	}
}

// The multi-exit shape, which was the largest refusal class in real guest
// output until the per-iteration copy made it lowerable. The `for` gets a
// control variable of its own so the wasm local is current on the edge that
// leaves from the middle.
func TestAMultiExitLoopIsLoweredWithAPerIterationCopy(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (param $stop i32) (result i32)
		(local $i i32) (local $s i32)
		(block $out
			(block $done
				(loop $top
					(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
					(br_if $out (i32.eq (local.get $i) (local.get $stop)))
					(local.set $s (i32.add (local.get $s) (i32.const 1)))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top))))
		(local.get $i)))`
	// The counter is read after the loop, so the value on the early-exit edge
	// is observable -- which is the whole point.
	sameAtEveryLevel(t, wat, `M.exports["f"](10, 3)`, "3")
	sameAtEveryLevel(t, wat, `M.exports["f"](10, 99)`, "10")
	sameAtEveryLevel(t, wat, `M.exports["f"](0, 0)`, "0")

	src := emitBody(t, wat, analysis.O1)
	if !strings.Contains(src, "for fk") {
		t.Errorf("a multi-exit loop should get its own control variable:\n%s", src)
	}
	if !strings.Contains(src, "v2 = fk") {
		t.Errorf("and a copy into the wasm local each iteration:\n%s", src)
	}
}

// A bound computed in the preheader is loop-invariant by construction, and
// refusing it was the second largest missed category.
func TestABoundHoistedIntoThePreheaderIsInvariant(t *testing.T) {
	const wat = `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (i32.mul (local.get $n) (i32.const 3))))
				(local.set $s (i32.add (local.get $s) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $s)))`
	sameAtEveryLevel(t, wat, `M.exports["f"](4)`, "12")
	sameAtEveryLevel(t, wat, `M.exports["f"](0)`, "0")
}
