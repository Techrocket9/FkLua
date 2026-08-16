package analysis

import (
	"sort"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// loopWeight is what one call site is worth at each loop nesting depth.
//
// A guest's hot calls are the ones inside loops, and a static count treats a
// call in a triply-nested loop the same as one in a cold initialiser. Ten per
// level, capped at three levels, is the usual profile-free stand-in; the cap
// stops a deeply nested but rarely entered loop from taking the whole budget.
func loopWeight(depth int) int64 {
	if depth > 3 {
		depth = 3
	}
	w := int64(1)
	for i := 0; i < depth; i++ {
		w *= 10
	}
	return w
}

// HotCallees ranks direct-call targets by weighted call-site count and returns
// the best `budget` of them.
//
// This is what upvalue promotion spends its budget on. `F[idx](...)` measured
// 21.32 ns in Factorio against 16.82 for a call through an upvalue -- 27%, and
// a call is the one thing a recursive guest does more of than anything else.
//
// The functions still LIVE in F. They have to: call_indirect dispatches through
// the table, exports are taken from it, and a chunk caps at 200 locals so
// `local f0, f1, ... fN` is not available at any realistic function count.
// Promotion adds a second name for the hottest few, and that is all.
func HotCallees(m *ir.Module, budget int) []uint32 {
	if budget <= 0 || m == nil {
		return nil
	}
	weight := map[uint32]int64{}
	for _, f := range m.Funcs {
		if f.Unsupported != nil {
			continue
		}
		depth := 0
		var stack []wasm.Op
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
			case wasm.OpCall:
				weight[s.Callee] += loopWeight(depth)
			}
		}
	}
	if len(weight) == 0 {
		return nil
	}

	idx := make([]uint32, 0, len(weight))
	for k := range weight {
		idx = append(idx, k)
	}
	// Weight first, then index, so the choice is deterministic: two builds of
	// the same module have to produce the same chunk or nothing downstream can
	// be diffed.
	sort.Slice(idx, func(a, b int) bool {
		if weight[idx[a]] != weight[idx[b]] {
			return weight[idx[a]] > weight[idx[b]]
		}
		return idx[a] < idx[b]
	})
	if len(idx) > budget {
		idx = idx[:budget]
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	return idx
}
