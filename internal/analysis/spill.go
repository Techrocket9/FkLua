package analysis

import (
	"sort"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Spill is the plan for moving some of a function's slots out of Lua locals
// and into the chunk-level frame stack.
//
// Lua caps a function at 200 locals, and the emitter budgets 180 of them (see
// ir.MaxSlots). A function past that used to be REFUSED outright. It now keeps
// its hot slots as locals and puts the rest in FS, a chunk-level array indexed
// off a per-call base -- so the refusal becomes a slowdown on the coldest
// values in the function, which is the right trade for something that
// previously did not compile at all.
type Spill struct {
	// Index maps a spilled slot to its offset within this call's frame.
	Index map[ir.Slot]int
	// Size is how many frame entries the call reserves.
	Size int
}

// Active reports whether anything spills.
func (s *Spill) Active() bool { return s != nil && s.Size > 0 }

// At reports the frame offset of a slot, when it spilled.
func (s *Spill) At(slot ir.Slot) (int, bool) {
	if s == nil {
		return 0, false
	}
	i, ok := s.Index[slot]
	return i, ok
}

// Plan chooses which slots spill so that at most max Lua locals are declared.
//
// # What can spill
//
// Not a parameter: a parameter IS a Lua local by virtue of being in the
// function's parameter list, and there is nowhere else for the caller to put
// it. Everything else -- declared wasm locals and operand-stack slots -- can
// live in the frame table.
//
// # What spills first
//
// The coldest. Each slot is scored by how often it is named, weighted by loop
// nesting depth, and the lowest scores go. Loop weighting is what stops the
// pass from putting a loop counter in a table to save a slot that a
// once-executed initialiser could have given up instead.
func Plan(f *ir.Func, max int) *Spill {
	// One extra local holds the frame base, so the budget is one tighter than
	// it looks.
	need := f.NumSlots + 1 - max
	if need <= 0 {
		return nil
	}

	params := 0
	for _, p := range f.Params {
		params += p.Slots()
	}

	score := make([]int64, f.NumSlots)
	depth := 0
	var stack []wasm.Op
	touch := func(s ir.Slot, w int64) {
		if s >= 0 && int(s) < len(score) {
			score[s] += w
		}
	}
	for _, s := range f.Steps {
		switch s.Op {
		case wasm.OpBlock, wasm.OpIf:
			stack = append(stack, s.Op)
		case wasm.OpLoop:
			stack = append(stack, s.Op)
			depth++
		case wasm.OpEnd:
			if n := len(stack); n > 0 {
				if stack[n-1] == wasm.OpLoop {
					depth--
				}
				stack = stack[:n-1]
			}
		}
		w := loopWeight(depth)
		// A local.get names its local in the IMMEDIATE, not in Args -- Args
		// holds the stack slot it pushes. Scoring only Args would give every
		// declared local a weight of zero and spill the loop counter first,
		// which is the exact opposite of the intent.
		switch s.Op {
		case wasm.OpLocalGet, wasm.OpLocalSet, wasm.OpLocalTee:
			base := f.LocalSlot(s.Instr.LocalIndex)
			if base != ir.NoSlot {
				for j := 0; j < f.LocalType(s.Instr.LocalIndex).Slots(); j++ {
					touch(base+ir.Slot(j), w)
				}
			}
		}
		for k, a := range s.Args {
			n := 1
			if k < len(s.ArgTypes) {
				n = s.ArgTypes[k].Slots()
			}
			for j := 0; j < n; j++ {
				touch(a+ir.Slot(j), w)
			}
		}
		if s.Dst != ir.NoSlot {
			for j := 0; j < s.DstType.Slots(); j++ {
				touch(s.Dst+ir.Slot(j), w)
			}
		}
		for _, br := range append([]ir.Branch{s.Target, s.Default}, s.Targets...) {
			if br.From != ir.NoSlot {
				touch(br.From, w)
			}
			if br.To != ir.NoSlot {
				touch(br.To, w)
			}
		}
	}

	cand := make([]ir.Slot, 0, f.NumSlots)
	for s := ir.Slot(params); int(s) < f.NumSlots; s++ {
		cand = append(cand, s)
	}
	if len(cand) < need {
		// Every remaining slot is a parameter. Nothing to do here; the emitter
		// reports it, because a function with 180 parameters is a different
		// problem from a function with 180 temporaries.
		return nil
	}
	sort.Slice(cand, func(i, j int) bool {
		if score[cand[i]] != score[cand[j]] {
			return score[cand[i]] < score[cand[j]]
		}
		return cand[i] > cand[j] // deeper stack slots first, they are colder
	})

	sp := &Spill{Index: map[ir.Slot]int{}}
	for _, s := range cand[:need] {
		sp.Index[s] = sp.Size
		sp.Size++
	}
	return sp
}
