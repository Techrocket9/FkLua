package analysis

import "testing"

// These pin the loop guard's CONTRACT at the analysis boundary, independent of
// how luagen chooses to emit it. internal/luagen has the behavioural tests --
// values, traps, the dirty-page mark -- and they are the ones that would
// catch a miscompile; these say what the analysis promises, which is what a
// future emitter change gets to rely on.

// guardOf runs the recogniser over the first function and returns the single
// loop it found, or nil.
func guardOf(t *testing.T, wat string) *LoopGuard {
	t.Helper()
	m := build(t, wat)
	got := LoopGuards(m.Funcs[0])
	if len(got) > 1 {
		t.Fatalf("expected at most one guarded loop, got %d", len(got))
	}
	for _, g := range got {
		return g
	}
	return nil
}

// The canonical shape: a bottom-tested countdown closed by a bare local.tee,
// a straight-line body, and a pointer advancing by a constant.
func TestLoopGuardDescribesTheCanonicalLoop(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc)
					(i32.load offset=4 (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 8)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g == nil {
		t.Fatal("the canonical guardable loop was not recognised")
	}
	if g.Step != -1 || !g.ExactTrips || !g.BoundIsConst || g.BoundConst != 0 {
		t.Errorf("countdown to an implicit zero: step=%d exact=%v constBound=%v/%d",
			g.Step, g.ExactTrips, g.BoundIsConst, g.BoundConst)
	}
	if len(g.Bases) != 1 {
		t.Fatalf("one base expected, got %d", len(g.Bases))
	}
	b := g.Bases[0]
	if b.Stride != 8 {
		t.Errorf("stride = %d, want 8", b.Stride)
	}
	// offset=4 on the memarg is part of the address, so the span must reach
	// offset + width past the base, not just the base.
	if b.MaxEnd != 8 {
		t.Errorf("maxEnd = %d, want 8 (offset 4 + width 4)", b.MaxEnd)
	}
	if len(g.Steps) != 1 || g.HasStore {
		t.Errorf("one guarded LOAD expected: steps=%d hasStore=%v", len(g.Steps), g.HasStore)
	}
	if b.Inc < 0 {
		t.Error("the base advances, so Inc should name the step that advances it")
	}
}

// A store is guarded too, and the guard must know -- that is what lets it widen
// the dirty-page range once for the whole span instead of once per store.
func TestLoopGuardNoticesAStore(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $n i32)
			(loop $top
				(i32.store (local.get $p) (i32.const 7))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))))`)
	if g == nil {
		t.Fatal("a store loop is guardable")
	}
	if !g.HasStore {
		t.Error("HasStore must be set, or the guard will not mark the span dirty")
	}
}

// Each refusal below would be an out-of-bounds access if it were allowed.
func TestLoopGuardRefusals(t *testing.T) {
	cases := []struct{ name, wat string }{
		{"an increment outside the LATCH may not run every iteration", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(if (i32.eqz (local.get $acc))
					(then (local.set $p (i32.add (local.get $p) (i32.const 4)))))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
		{"a nested loop runs the latch many times per outer iteration", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32) (local $k i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $k (i32.const 3))
				(loop $inner
					(local.set $acc (i32.add (local.get $acc) (i32.const 1)))
					(br_if $inner (local.tee $k (i32.sub (local.get $k) (i32.const 1)))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
		{"a base written twice has no single stride", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
		{"a counter written twice has no trip count", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $n (i32.mul (local.get $n) (i32.const 3)))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
		{"an unaligned stride cannot keep the base 4-aligned", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 6)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
		{"an unaligned memarg offset likewise", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load offset=2 (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},

		{"memory.grow in the body", `
			(module (memory 1) (func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(drop (memory.grow (i32.const 1)))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if g := guardOf(t, tc.wat); g != nil {
				t.Errorf("guarded a loop it must refuse: %+v", g)
			}
		})
	}
}

// TWO bases is the shape a dot product has -- two arrays walked in step -- and
// it is what the multi-base conjunction exists for. Each gets its own span, its
// own alignment test and its own word index.
func TestTwoBasesEachGetTheirOwnSpan(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $q i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc)
					(i32.add (i32.load offset=8 (local.get $p)) (i32.load (local.get $q)))))
				(local.set $p (i32.add (local.get $p) (i32.const 16)))
				(local.set $q (i32.add (local.get $q) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g == nil {
		t.Fatal("two bases walked in step is the shape this pass exists for")
	}
	if len(g.Bases) != 2 || len(g.Steps) != 2 {
		t.Fatalf("bases=%d accesses=%d, want 2 and 2", len(g.Bases), len(g.Steps))
	}
	// Each base's span is its OWN reach. Sharing one would under-cover the base
	// with the larger offset and over-constrain the other.
	byStride := map[int64]GuardBase{}
	for _, b := range g.Bases {
		byStride[b.Stride] = b
	}
	if byStride[16].MaxEnd != 12 {
		t.Errorf("the offset-8 base should reach 12, got %d", byStride[16].MaxEnd)
	}
	if byStride[4].MaxEnd != 4 {
		t.Errorf("the offset-0 base should reach 4, got %d", byStride[4].MaxEnd)
	}
	// Each access must point at the base it actually hangs off.
	for st, a := range g.Steps {
		if a.Off == 8 && g.Bases[a.Base].Stride != 16 {
			t.Errorf("step %d: offset 8 pointed at the wrong base", st)
		}
	}
}

// An AFFINE base -- recomputed each iteration as `index + arrayStart` rather
// than advanced in place -- is what both toolchains emit for an indexed array,
// and it is the shape that kept pure_dot out of this pass.
func TestAnAffineBaseIsRecognised(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $arr i32) (param $n i32) (result i32)
			(local $i i32) (local $p i32) (local $acc i32)
			(loop $top
				(local.set $p (i32.add (local.get $i) (local.get $arr)))
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $i (i32.add (local.get $i) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g == nil {
		t.Fatal("an affine base is the shape a real guest indexes an array with")
	}
	if len(g.Bases) != 1 {
		t.Fatalf("one base expected, got %d", len(g.Bases))
	}
	b := g.Bases[0]
	if !b.Affine {
		t.Fatal("the base is a sum, so it should be marked affine")
	}
	// Its stride is the INDUCTION VARIABLE's, not its own -- it has none of its
	// own, being rewritten from scratch each iteration.
	if b.Stride != 4 {
		t.Errorf("stride = %d, want the induction variable's 4", b.Stride)
	}
	if b.AffineIV == b.AffineInv {
		t.Error("the walking half and the invariant half must be different locals")
	}
}

// A BRANCHY body is admitted now, provided both increments live in the latch --
// the block the back edge leaves from, which by construction runs exactly once
// per completed iteration. An access in an earlier block happens at MOST once,
// which is fine: the span covers an access that happens, not one that must.
func TestABranchyBodyIsGuardedWhenItsIncrementsAreInTheLatch(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(if (i32.eqz (local.get $acc)) (then (local.set $acc (i32.const 1))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g == nil {
		t.Fatal("a branchy body whose increments are in the latch is guardable")
	}
	if len(g.Bases) != 1 || g.Bases[0].Stride != 4 {
		t.Fatalf("bases=%d stride=%d, want 1 and 4", len(g.Bases), g.Bases[0].Stride)
	}
}

// A definition reached only through a SIBLING arm is refused.
//
// defOf is a backward linear scan, so the nearest textually-preceding writer of
// a slot can sit in a branch that never executed on the path to the use.
// Operand-stack values normally die at a block boundary, so the way to actually
// produce one is a block that yields a value -- here an `if (result i32)` whose
// two arms offer different bases. Restricting every lookup to the use's own
// block is what makes a branchy body safe, and without it this loop is guarded
// against whichever arm happens to be textually last.
func TestADefinitionFromAnotherBlockIsRefused(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $q i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc)
					(i32.load (if (result i32) (i32.eqz (local.get $acc))
						(then (local.get $p))
						(else (local.get $q))))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g != nil {
		t.Errorf("an address whose definition is in a sibling arm must be "+
			"refused: %+v", g)
	}
}

// Past the base cap the extra accesses are SKIPPED, not the loop refused. A
// skipped access keeps its own bounds check, so the guard still only claims what
// it proved -- and the loop keeps the benefit for the bases it could describe.
func TestPastTheBaseCapTheExtraAccessesAreSkipped(t *testing.T) {
	g := guardOf(t, `(module (memory 1) (func (export "f")
		(param $a i32) (param $b i32) (param $c i32) (param $d i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc)
				(i32.add (i32.add (i32.load (local.get $a)) (i32.load (local.get $b)))
						 (i32.add (i32.load (local.get $c)) (i32.load (local.get $d))))))
			(local.set $a (i32.add (local.get $a) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc)))`)
	if g == nil {
		t.Fatal("the loop should still be guarded for the bases it can describe")
	}
	if len(g.Bases) > maxGuardBases {
		t.Errorf("bases = %d, over the cap of %d", len(g.Bases), maxGuardBases)
	}
	if len(g.Steps) != len(g.Bases) {
		t.Errorf("accesses = %d but bases = %d; the access past the cap should "+
			"have been skipped, not counted", len(g.Steps), len(g.Bases))
	}
}

// A loop with no memory access at all has nothing to guard, so there is no
// reason to pay for an entry test.
func TestALoopWithNoAccessIsNotGuarded(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.const 3)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	if g != nil {
		t.Errorf("nothing to guard, but got %+v", g)
	}
}

// GuardedAccessOffset re-derives what the recogniser already computed. The two
// must agree: a disagreement is a wrong table index rather than a refused loop,
// so they share one implementation and this says so.
func TestGuardedAccessOffsetAgreesWithTheRecogniser(t *testing.T) {
	m := build(t, `(module (memory 1)
		(func (export "f") (param $p i32) (param $n i32) (result i32)
			(local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc)
					(i32.add (i32.load offset=12 (local.get $p))
							 (i32.load (local.get $p)))))
				(local.set $p (i32.add (local.get $p) (i32.const 16)))
				(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
			(local.get $acc)))`)
	gs := LoopGuards(m.Funcs[0])
	if len(gs) != 1 {
		t.Fatalf("expected one guarded loop, got %d", len(gs))
	}
	for _, g := range gs {
		if len(g.Steps) != 2 {
			t.Fatalf("expected both loads guarded, got %d", len(g.Steps))
		}
		seen := map[int64]bool{}
		for st := range g.Steps {
			a, ok := GuardedAccessOffset(g, st)
			if !ok {
				t.Fatalf("step %d: the recogniser took it but recorded nothing", st)
			}
			if a.Base < 0 || a.Base >= len(g.Bases) {
				t.Fatalf("step %d: base index %d out of range", st, a.Base)
			}
			if a.Off+a.Width > g.Bases[a.Base].MaxEnd {
				t.Errorf("step %d: reaches %d past its base but the span proves only %d "+
					"-- this access is not covered", st, a.Off+a.Width, g.Bases[a.Base].MaxEnd)
			}
			seen[a.Off] = true
		}
		if !seen[0] || !seen[12] {
			t.Errorf("offsets = %v, want 0 and 12", seen)
		}
	}
}

// A base derived FROM the counter, which is how rustc indexes an array: the
// loop counter walks and both array starts do not, so each address is their
// sum. Refusing this -- on the theory that a counter-shaped base means two
// strides -- is what kept Rust's pure_dot out of the pass while TinyGo's went
// through, and there is no second stride: the base's stride simply IS the
// counter's.
func TestABaseDerivedFromTheCounterIsAllowed(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $a i32) (param $b i32) (param $n i32) (result f64)
			(local $i i32) (local $acc f64)
			(loop $top
				(local.set $acc (f64.add (local.get $acc)
					(f64.mul (f64.load (i32.add (local.get $i) (local.get $a)))
							 (f64.load (i32.add (local.get $i) (local.get $b))))))
				(br_if $top (i32.ne
					(local.tee $i (i32.add (local.get $i) (i32.const 8)))
					(local.get $n))))
			(local.get $acc)))`)
	if g == nil {
		t.Fatal("a base derived from the counter is guardable")
	}
	if len(g.Bases) != 2 || len(g.Steps) != 2 {
		t.Fatalf("bases=%d accesses=%d, want 2 and 2", len(g.Bases), len(g.Steps))
	}
	for _, b := range g.Bases {
		if !b.Affine {
			t.Error("each base is a sum, so each should be affine")
		}
		if b.Stride != 8 {
			t.Errorf("stride = %d, want the counter's 8", b.Stride)
		}
	}
	// The 8-byte width is what makes this kernel's accesses guardable at all.
	for _, a := range g.Steps {
		if a.Width != 8 {
			t.Errorf("width = %d, want 8", a.Width)
		}
	}
}

// The counter used AS a base, rather than derived from, is still refused.
func TestTheCounterItselfIsNotABase(t *testing.T) {
	g := guardOf(t, `(module (memory 1)
		(func (export "f") (param $n i32) (result i32)
			(local $i i32) (local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $i))))
				(br_if $top (i32.ne
					(local.tee $i (i32.add (local.get $i) (i32.const 4)))
					(local.get $n))))
			(local.get $acc)))`)
	if g != nil {
		t.Errorf("the counter as a base is still refused: %+v", g)
	}
}
