package analysis

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/wasm"
)

// countedOf runs the recognizer over the first function and returns the one
// loop it found, or nil.
func countedOf(t *testing.T, wat string) *Counted {
	t.Helper()
	m := build(t, wat)
	f := m.Funcs[0]
	got := CountedLoops(f, Ranges(f))
	if len(got) > 1 {
		t.Fatalf("expected at most one counted loop, got %d", len(got))
	}
	for _, c := range got {
		return c
	}
	return nil
}

// The bottom-tested shape LLVM emits after loop rotation, which is what
// bench/wasm/count.wat contains.
func TestABottomTestedCountedLoopIsRecognised(t *testing.T) {
	c := countedOf(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (local.get $i)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
		(local.get $s)))`)
	if c == nil {
		t.Fatal("the canonical counted loop was not recognised")
	}
	if c.TopTested {
		t.Error("this shape is bottom-tested")
	}
	if c.Step != 1 {
		t.Errorf("step = %d, want 1", c.Step)
	}
	// The body sees i = 0 .. n-1, so Lua's inclusive limit is n-1 and the
	// counter lands on n.
	if c.Adjust != -1 || c.FinalAdjust != 0 {
		t.Errorf("adjust = %d, final = %d; want -1, 0", c.Adjust, c.FinalAdjust)
	}
}

// The top-tested shape, which is what bench/wasm/sum.wat contains.
func TestATopTestedCountedLoopIsRecognised(t *testing.T) {
	c := countedOf(t, `(module (memory 1) (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $s i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $s (i32.add (local.get $s) (local.get $i)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $s)))`)
	if c == nil {
		t.Fatal("the top-tested counted loop was not recognised")
	}
	if !c.TopTested {
		t.Error("this shape is top-tested")
	}
	if c.Adjust != -1 || c.FinalAdjust != 0 {
		t.Errorf("adjust = %d, final = %d; want -1, 0", c.Adjust, c.FinalAdjust)
	}
}

// A countdown against zero. LLVM strength-reduces most counted loops into this,
// so a pass that only knew `lt` would miss the shape it most needs to see.
func TestACountdownToZeroIsCounted(t *testing.T) {
	c := countedOf(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $s i32)
		(block $done
			(br_if $done (i32.eqz (local.get $n)))
			(loop $top
				(local.set $s (i32.add (local.get $s) (i32.const 3)))
				(local.set $n (i32.sub (local.get $n) (i32.const 1)))
				(br_if $top (i32.ne (local.get $n) (i32.const 0)))))
		(local.get $s)))`)
	if c == nil {
		t.Fatal("a countdown to zero is a counted loop")
	}
	if c.Step != -1 {
		t.Errorf("step = %d, want -1", c.Step)
	}
	// Body sees n .. 1; Lua's limit is 0 - (-1) = 1, and the counter lands on 0.
	if c.Adjust != 1 || c.FinalAdjust != 0 {
		t.Errorf("adjust = %d, final = %d; want 1, 0", c.Adjust, c.FinalAdjust)
	}
}

// Every refusal below would be a miscompile if it were allowed through.
func TestTheRefusals(t *testing.T) {
	cases := []struct{ name, wat string }{
		{"a body that writes the counter is not counted", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(loop $top
				(local.set $i (i32.mul (local.get $i) (i32.const 2)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n))))
			(local.get $s)))`},
		{"a bound the loop writes is not invariant", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(loop $top
				(local.set $n (i32.add (local.get $n) (i32.const 1)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n))))
			(local.get $s)))`},
		{"a continue skips the increment", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(loop $top
				(br_if $top (i32.eqz (local.get $s)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.lt_s (local.get $i) (local.get $n))))
			(local.get $s)))`},
		// A second exit is no longer a refusal on its own -- the per-iteration
		// copy handles it. This one is refused because a BOTTOM-tested loop
		// runs its body before testing anything, and nothing here proves that
		// was going to happen: $i enters at 0 and $n could be 0 too.
		{"a bottom-tested loop that cannot prove it runs once", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(block $out
				(loop $top
					(br_if $out (i32.eqz (local.get $s)))
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
			(local.get $i)))`},
		// An unconditional branch before the back edge makes everything after
		// it dead -- including the increment, so there is no trip count.
		{"an unconditional branch out kills the increment after it", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(block $out
				(loop $top
					(br_if $out (i32.ge_u (local.get $i) (local.get $n)))
					(br $out)
					(local.set $i (i32.add (local.get $i) (i32.const 1)))
					(br $top)))
			(local.get $s)))`},
		{"counting up under a test that only wraps out", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(loop $top
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br_if $top (i32.gt_s (local.get $i) (local.get $n))))
			(local.get $s)))`},
		{"a step of 4 needs a congruence proof its != test does not give", `
			(module (func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $s i32)
			(loop $top
				(local.set $i (i32.add (local.get $i) (i32.const 4)))
				(br_if $top (i32.ne (local.get $i) (local.get $n))))
			(local.get $s)))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c := countedOf(t, tc.wat); c != nil {
				t.Errorf("recognised a loop it must refuse: %+v", c)
			}
		})
	}
}

// The tripwire for defOf's linear scan, now that the loop guard has relaxed
// into branchy bodies and the counted-loop pass has not.
//
// defOf walks step INDICES and knows nothing about control flow, so it is the
// definition only when textual order is execution order. The loop guard buys its
// branchy bodies by restricting every lookup to the use's own BLOCK (defOfB);
// CountedLoops has no such restriction and therefore still must refuse a branchy
// body outright. Relaxing it without the same treatment is silent unsoundness --
// a trip count derived from a definition on a path that never ran.
func TestCountedLoopsStillRefuseABranchyBody(t *testing.T) {
	const branchy = `(module (memory 1)
		(func (export "f") (param $n i32) (param $p i32) (result i32)
			(local $i i32) (local $s i32)
			(local.set $i (local.get $n))
			(loop $top
				(local.set $s (i32.add (local.get $s) (i32.load (local.get $p))))
				(if (i32.eqz (local.get $s))
					(then (local.set $s (i32.const 1))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $i
					(i32.sub (local.get $i) (i32.const 1)))))
			(local.get $s)))`
	f := build(t, branchy).Funcs[0]
	for _, c := range CountedLoops(f, Ranges(f)) {
		for i := c.BodyStart; i < c.BodyEnd; i++ {
			switch f.Steps[i].Op {
			case wasm.OpIf, wasm.OpElse, wasm.OpBlock, wasm.OpBrTable:
				t.Fatalf("a counted loop kept a branchy body at step %d (%v) -- "+
					"defOf is still a plain linear scan there", i, f.Steps[i].Op)
			}
		}
	}
}
