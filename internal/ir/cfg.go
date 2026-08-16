package ir

import "github.com/Techrocket9/fklua/internal/wasm"

// ExitKind is how a basic block hands control on.
type ExitKind int

const (
	// ExitFall runs straight into the following block.
	ExitFall ExitKind = iota
	// ExitJump is an unconditional goto.
	ExitJump
	// ExitCond tests a value: True is where control goes when the terminator's
	// condition operand is non-zero, False where it goes when it is zero.
	ExitCond
	// ExitTable is br_table, whose successors carry no usable condition.
	ExitTable
	// ExitLeave leaves the function -- a return, or a trap that never comes
	// back. It has no successors at all.
	ExitLeave
)

// Block is a basic block: the maximal run of steps [Start, End) that control
// enters only at Start and leaves only at End-1.
type Block struct {
	Start, End int

	Kind  ExitKind
	Succs []int
	Preds []int

	// True and False are block indices for ExitCond, and NoBlock when that edge
	// leaves the function (a br_if whose target is a return). They are always
	// members of Succs when they are not NoBlock.
	True, False int
}

// NoBlock marks the absence of a block, which for an edge means it leaves the
// function.
const NoBlock = -1

// CFG is the control-flow graph of one function.
//
// It is derived from the FLAT step list, not from the wasm block nesting,
// because that is what the emitter prints: every construct goes out at
// function-body level with function-scoped labels and gotos (Lua rejects a goto
// into a sibling block, so nesting is not available). A `loop` defines its label
// at its own step; a `block`, an `if` and an `else` define theirs at the step
// AFTER the one that writes them, which is the only subtlety in the mapping.
type CFG struct {
	Blocks []Block
	// BlockOf[i] is the block containing step i.
	BlockOf []int
	// Order is a reverse postorder over the blocks reachable from the entry.
	// Blocks absent from it were not reachable.
	Order []int
	// Retreating[b] reports a block that is the target of an edge from a block
	// no earlier than it in Order. Every cycle in the graph contains at least
	// one such edge, so a fixpoint that widens at these blocks terminates.
	Retreating []bool
	// Complete is false when a branch named a label with no definition, which
	// would mean an edge is missing and every dataflow answer computed from the
	// graph would be an under-approximation. A caller that cannot tolerate that
	// must fall back to assuming nothing.
	Complete bool
}

// Terminator returns the index of the step that ends block b.
func (c *CFG) Terminator(b int) int { return c.Blocks[b].End - 1 }

// BuildCFG derives the control-flow graph of f.
//
// A function with no steps, or one that could not be compiled, yields an empty
// graph rather than an error: every caller here treats "no information" as the
// conservative answer already.
func BuildCFG(f *Func) *CFG {
	n := len(f.Steps)
	cfg := &CFG{BlockOf: make([]int, n), Complete: true}
	for i := range cfg.BlockOf {
		cfg.BlockOf[i] = NoBlock
	}
	if n == 0 {
		return cfg
	}

	site := labelSites(f)

	// Leaders: the entry, every step a label lands on, and the step after
	// anything that branches.
	leader := make([]bool, n)
	leader[0] = true
	for _, at := range site {
		if at > 0 && at < n {
			leader[at] = true
		}
	}
	for i := range f.Steps {
		if terminates(f.Steps[i].Op) && i+1 < n {
			leader[i+1] = true
		}
	}

	for i := 0; i < n; i++ {
		if leader[i] {
			cfg.Blocks = append(cfg.Blocks, Block{Start: i, True: NoBlock, False: NoBlock})
		}
		b := len(cfg.Blocks) - 1
		cfg.BlockOf[i] = b
		cfg.Blocks[b].End = i + 1
	}

	// Resolve a label to the block it starts. A label landing past the last
	// step is a fall-out of the function, which has no block.
	blockAt := func(step int) int {
		if step < 0 || step >= n {
			return NoBlock
		}
		return cfg.BlockOf[step]
	}
	target := func(l Label) int {
		at, ok := site[l]
		if !ok {
			cfg.Complete = false
			return NoBlock
		}
		return blockAt(at)
	}
	edge := func(br Branch) int {
		if br.IsReturn() {
			return NoBlock
		}
		return target(br.Label)
	}

	for b := range cfg.Blocks {
		blk := &cfg.Blocks[b]
		t := &f.Steps[blk.End-1]
		next := blockAt(blk.End)

		switch t.Op {
		case wasm.OpBr:
			if t.Target.IsReturn() {
				blk.Kind = ExitLeave
			} else {
				blk.Kind = ExitJump
				blk.Succs = addSucc(blk.Succs, edge(t.Target))
			}

		case wasm.OpBrIf:
			blk.Kind = ExitCond
			blk.True = edge(t.Target)
			blk.False = next
			blk.Succs = addSucc(blk.Succs, blk.True)
			blk.Succs = addSucc(blk.Succs, blk.False)

		case wasm.OpIf:
			// `if` jumps to its else-label when the condition is FALSE and falls
			// through when it is true -- the inverse of br_if, and the reason
			// True/False are named rather than positional.
			blk.Kind = ExitCond
			blk.True = next
			blk.False = target(t.ElseLabel)
			blk.Succs = addSucc(blk.Succs, blk.True)
			blk.Succs = addSucc(blk.Succs, blk.False)

		case wasm.OpElse:
			// The else step first jumps past the else-arm; its own label is
			// defined at the step after it, which is a separate block.
			blk.Kind = ExitJump
			blk.Succs = addSucc(blk.Succs, target(t.Label))

		case wasm.OpBrTable:
			blk.Kind = ExitTable
			for _, br := range t.Targets {
				blk.Succs = addSucc(blk.Succs, edge(br))
			}
			blk.Succs = addSucc(blk.Succs, edge(t.Default))

		case wasm.OpReturn, wasm.OpUnreachable:
			blk.Kind = ExitLeave

		default:
			if next == NoBlock {
				blk.Kind = ExitLeave
			} else {
				blk.Kind = ExitFall
				blk.Succs = addSucc(blk.Succs, next)
			}
		}
	}

	for b := range cfg.Blocks {
		for _, s := range cfg.Blocks[b].Succs {
			cfg.Blocks[s].Preds = append(cfg.Blocks[s].Preds, b)
		}
	}

	cfg.Order = reversePostorder(cfg)
	cfg.Retreating = retreatingTargets(cfg)
	return cfg
}

// addSucc appends a successor, skipping the marker for an edge that leaves the
// function and refusing a duplicate -- br_table names the same block many times
// and a join must not count it twice.
func addSucc(dst []int, b int) []int {
	if b == NoBlock {
		return dst
	}
	for _, x := range dst {
		if x == b {
			return dst
		}
	}
	return append(dst, b)
}

// labelSites maps each label to the step at which control resumes when
// something jumps to it.
//
// The positions come straight from what the emitter prints:
//
//	loop  ->  `::L::` at its own step, because a branch to a loop goes to its head
//	else  ->  `goto Lend` then `::Lelse::`, so the label lands on the NEXT step
//	end   ->  `::Lelse::` (an if with no else) and `::Lend::`, both on the next step
//
// `block` and `if` never define a label where they are written; theirs is
// defined at the matching `end`.
func labelSites(f *Func) map[Label]int {
	site := map[Label]int{}
	for i := range f.Steps {
		s := &f.Steps[i]
		switch s.Op {
		case wasm.OpLoop:
			if s.Label != NoLabel {
				site[s.Label] = i
			}
		case wasm.OpElse:
			if s.ElseLabel != NoLabel {
				site[s.ElseLabel] = i + 1
			}
		case wasm.OpEnd:
			if s.ElseLabel != NoLabel {
				site[s.ElseLabel] = i + 1
			}
			if s.Label != NoLabel {
				site[s.Label] = i + 1
			}
		}
	}
	return site
}

// terminates reports a step after which control does not simply continue.
//
// `end` is deliberately absent: an `end` that defines a label already forces a
// leader at the next step through labelSites, and a loop's `end` -- which
// carries no label, because a loop's label sits at its top -- is a plain
// fall-through that must not be cut.
func terminates(op wasm.Op) bool {
	switch op {
	case wasm.OpIf, wasm.OpElse, wasm.OpBr, wasm.OpBrIf, wasm.OpBrTable,
		wasm.OpReturn, wasm.OpUnreachable:
		return true
	}
	return false
}

// reversePostorder returns the reachable blocks in reverse postorder, which is
// the order an iterative forward dataflow wants: every block appears after at
// least one predecessor unless the edge between them is a back edge.
func reversePostorder(c *CFG) []int {
	n := len(c.Blocks)
	seen := make([]bool, n)
	var post []int

	type frame struct{ b, k int }
	stack := []frame{{0, 0}}
	seen[0] = true
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.k < len(c.Blocks[top.b].Succs) {
			s := c.Blocks[top.b].Succs[top.k]
			top.k++
			if !seen[s] {
				seen[s] = true
				stack = append(stack, frame{s, 0})
			}
			continue
		}
		post = append(post, top.b)
		stack = stack[:len(stack)-1]
	}

	order := make([]int, len(post))
	for i, b := range post {
		order[len(post)-1-i] = b
	}
	return order
}

// retreatingTargets marks every block reached by an edge from a block no
// earlier than it in reverse postorder.
//
// In a reducible graph these are exactly the loop headers. In an irreducible
// one they are a superset, which costs precision and never costs soundness --
// and the property that matters is the one that holds either way: reverse
// postorder is a topological order of the graph with these edges removed, so
// every cycle contains at least one of them.
func retreatingTargets(c *CFG) []bool {
	pos := make([]int, len(c.Blocks))
	for i := range pos {
		pos[i] = -1
	}
	for i, b := range c.Order {
		pos[b] = i
	}
	out := make([]bool, len(c.Blocks))
	for _, b := range c.Order {
		for _, s := range c.Blocks[b].Succs {
			if pos[s] >= 0 && pos[s] <= pos[b] {
				out[s] = true
			}
		}
	}
	return out
}
