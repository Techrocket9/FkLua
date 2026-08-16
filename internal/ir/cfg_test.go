package ir

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/wasm"
)

// tiles asserts the invariant everything else here depends on: the blocks
// partition the step list, in order, with no gap and no overlap.
func tiles(t *testing.T, f *Func, c *CFG) {
	t.Helper()
	next := 0
	for i, b := range c.Blocks {
		if b.Start != next {
			t.Fatalf("block %d starts at %d, want %d", i, b.Start, next)
		}
		if b.End <= b.Start {
			t.Fatalf("block %d is empty: [%d,%d)", i, b.Start, b.End)
		}
		next = b.End
	}
	if next != len(f.Steps) {
		t.Fatalf("blocks cover %d steps, function has %d", next, len(f.Steps))
	}
	for i := range f.Steps {
		b := c.BlockOf[i]
		if b < 0 || i < c.Blocks[b].Start || i >= c.Blocks[b].End {
			t.Fatalf("step %d maps to block %d, which does not contain it", i, b)
		}
	}
}

func cfgOf(t *testing.T, wat string) (*Func, *CFG) {
	t.Helper()
	f := steps(t, wat)
	c := BuildCFG(f)
	if !c.Complete {
		t.Fatal("every label a branch names is defined by construction; an " +
			"incomplete graph means the label sites are wrong, and a missing " +
			"edge makes every dataflow answer an under-approximation")
	}
	tiles(t, f, c)
	return f, c
}

func TestStraightLineIsOneBlock(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param i32) (result i32)
		(i32.add (local.get 0) (i32.const 1))))`)
	if len(c.Blocks) != 1 {
		t.Fatalf("straight-line code is one block, got %d", len(c.Blocks))
	}
	if c.Blocks[0].Kind != ExitLeave {
		t.Errorf("the last block falls out of the function, got kind %v", c.Blocks[0].Kind)
	}
	if len(c.Order) != 1 || c.Order[0] != 0 {
		t.Errorf("order = %v, want [0]", c.Order)
	}
	if c.Retreating[0] {
		t.Error("nothing loops here, so nothing retreats")
	}
	_ = f
}

// The shape the whole pass exists for. The loop's head must be its own block,
// the back edge must land on it, and it must be marked as a widening point --
// without that last part a fixpoint over it does not terminate.
func TestLoopHeadIsABlockWithABackEdge(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $i)))`)

	head := -1
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpLoop {
			head = c.BlockOf[i]
			if c.Blocks[head].Start != i {
				t.Fatalf("a loop's label is defined at its own step, so the loop "+
					"must START a block: step %d, block starts at %d",
					i, c.Blocks[head].Start)
			}
		}
	}
	if head < 0 {
		t.Fatal("no loop found")
	}
	if !c.Retreating[head] {
		t.Error("the loop head is the target of the back edge and must be a " +
			"widening point; a fixpoint that does not widen here never stops")
	}
	if len(c.Blocks[head].Preds) < 2 {
		t.Errorf("the loop head is entered from the preheader AND the back edge, "+
			"got %d predecessor(s)", len(c.Blocks[head].Preds))
	}
	if c.Blocks[head].Kind != ExitCond {
		t.Errorf("the loop head ends in the br_if guard, got kind %v", c.Blocks[head].Kind)
	}
	// The guard's true edge leaves the loop; its false edge is the body, which
	// is the block that follows in step order.
	if c.Blocks[head].False != head+1 {
		t.Errorf("br_if falls through to the body when its condition is false: "+
			"False = %d, want %d", c.Blocks[head].False, head+1)
	}
	if c.Blocks[head].True == c.Blocks[head].False {
		t.Error("the guard's two edges must be distinguishable")
	}
}

// `if` and `br_if` disagree about which way their condition sends control, and
// getting it backwards would narrow the loop guard onto the wrong edge -- a
// silent miscompile rather than a slow one.
func TestIfAndBrIfPointOppositeWays(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $c i32) (result i32)
		(if (local.get $c) (then (return (i32.const 1))))
		(i32.const 2)))`)

	for i := range f.Steps {
		if f.Steps[i].Op != wasm.OpIf {
			continue
		}
		b := c.BlockOf[i]
		if c.Blocks[b].Kind != ExitCond {
			t.Fatalf("an if ends its block conditionally, got kind %v", c.Blocks[b].Kind)
		}
		if c.Blocks[b].True != b+1 {
			t.Errorf("if falls through into the THEN arm when its condition holds: "+
				"True = %d, want %d", c.Blocks[b].True, b+1)
		}
		if c.Blocks[b].False == b+1 {
			t.Error("if jumps to the else-label when its condition is false, so " +
				"the false edge cannot be the fall-through")
		}
	}
}

func TestBrTableEdgesAreAllPresentAndDeduplicated(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $i i32) (result i32)
		(block $a (block $b (block $c
			(br_table $a $b $c $a (local.get $i)))
			(return (i32.const 10)))
			(return (i32.const 20)))
		(i32.const 30)))`)

	for i := range f.Steps {
		if f.Steps[i].Op != wasm.OpBrTable {
			continue
		}
		b := c.BlockOf[i]
		if c.Blocks[b].Kind != ExitTable {
			t.Fatalf("br_table kind = %v", c.Blocks[b].Kind)
		}
		// Three distinct labels named four times.
		if n := len(c.Blocks[b].Succs); n != 3 {
			t.Errorf("br_table names 3 distinct labels; got %d successors (%v). "+
				"A duplicate would be joined twice, which is harmless here and "+
				"is not harmless once an edge carries a refinement",
				n, c.Blocks[b].Succs)
		}
		for _, s := range c.Blocks[b].Succs {
			if s == b {
				t.Error("no self-edge expected from this br_table")
			}
		}
	}
}

// An unconditional branch makes the code after it reachable only by label. The
// graph has to say so, or a dataflow join picks up a state that never arrives.
func TestCodeAfterAnUnconditionalBranchIsUnreachable(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32)
		(block $done
			(loop $top
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $i)))`)

	inOrder := map[int]bool{}
	for _, b := range c.Order {
		inOrder[b] = true
	}
	if len(inOrder) == len(c.Blocks) {
		t.Fatalf("this function has an infinite loop, so the loop's `end` is "+
			"unreachable; every one of its %d blocks was reached", len(c.Blocks))
	}
	_ = f
}

func TestReturnAndUnreachableHaveNoSuccessors(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $c i32) (result i32)
		(if (local.get $c) (then (return (i32.const 1))) (else (unreachable)))
		(i32.const 2)))`)

	sawReturn, sawTrap := false, false
	for i := range f.Steps {
		switch f.Steps[i].Op {
		case wasm.OpReturn:
			sawReturn = true
		case wasm.OpUnreachable:
			sawTrap = true
		default:
			continue
		}
		b := c.BlockOf[i]
		if c.Blocks[b].Kind != ExitLeave {
			t.Errorf("%v ends its block by leaving, got kind %v", f.Steps[i].Op, c.Blocks[b].Kind)
		}
		if len(c.Blocks[b].Succs) != 0 {
			t.Errorf("%v has no successors, got %v", f.Steps[i].Op, c.Blocks[b].Succs)
		}
	}
	if !sawReturn || !sawTrap {
		t.Fatal("expected both a return and an unreachable in this function")
	}
}

// An empty or uncompilable function must still yield a usable graph: every
// caller treats "no information" as the conservative answer, and returning a
// nil or malformed CFG would turn that into a crash.
func TestEmptyFunctionYieldsAnEmptyGraph(t *testing.T) {
	c := BuildCFG(&Func{})
	if len(c.Blocks) != 0 || len(c.Order) != 0 {
		t.Errorf("empty function: blocks=%v order=%v", c.Blocks, c.Order)
	}
	if !c.Complete {
		t.Error("nothing is missing from a graph with no edges")
	}
}

// A branch to a label that is never defined would silently drop an edge, and a
// join over the surviving edges is an UNDER-approximation -- the one failure
// mode that produces wrong code rather than slow code. The flag is how the
// analysis knows to stop trusting the graph.
func TestMissingLabelSiteIsReported(t *testing.T) {
	f := steps(t, `(module (func (export "f") (result i32)
		(block $a (br $a))
		(i32.const 1)))`)
	// Corrupt the label definition the way a future emitter change might.
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpEnd && f.Steps[i].Label != NoLabel {
			f.Steps[i].Label = Label(999)
			break
		}
	}
	if c := BuildCFG(f); c.Complete {
		t.Error("a branch to an undefined label must mark the graph incomplete")
	}
}

func TestNestedLoopsBothWiden(t *testing.T) {
	f, c := cfgOf(t, `(module (func (export "f") (param $n i32) (result i32)
		(local $i i32) (local $j i32) (local $s i32)
		(block $outer
			(loop $o
				(br_if $outer (i32.ge_u (local.get $i) (local.get $n)))
				(local.set $j (i32.const 0))
				(block $inner
					(loop $in
						(br_if $inner (i32.ge_u (local.get $j) (local.get $n)))
						(local.set $s (i32.add (local.get $s) (local.get $j)))
						(local.set $j (i32.add (local.get $j) (i32.const 1)))
						(br $in)))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $o)))
		(local.get $s)))`)

	heads := 0
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpLoop && c.Retreating[c.BlockOf[i]] {
			heads++
		}
	}
	if heads != 2 {
		t.Errorf("both loop heads must be widening points, got %d", heads)
	}
}
