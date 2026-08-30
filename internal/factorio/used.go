package factorio

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Finding which API members a guest actually calls.
//
// The compiler already parses the guest, so it can see this rather than being
// told: every host call is `fk.call(handle, member, argp, retp)` with the
// member id as a constant the binding generator emitted. Collecting those
// constants is what lets `fklua mod` ship five members instead of the whole
// table (host_members_bound in census.json).
//
// The analysis is deliberately shallow -- it looks for an i32.const feeding the
// member operand and nothing cleverer -- because the alternative to "provably
// constant" is not "probably fine", it is a missing member at runtime. Anything
// it cannot prove makes it give up on pruning entirely, which is the safe
// direction: a bigger mod, never a broken one.

// DispatchImport is the two-level name of the generic host-call import.
const (
	DispatchModule = "fk"
	DispatchName   = "call"
)

// TypedDispatchName is the SECOND dispatch import: same handle, same member id,
// same return block, and an argument block laid out as a tier-1 struct instead
// of one tier-2 map. See Member.TypedArgs.
//
// A SECOND IMPORT RATHER THAN A FIFTH OPERAND ON fk.call, which is round 1's
// fk.subscribe widening reaching the OTHER answer for a stated reason. Widening
// is right when the new information belongs to the same call -- subscribe's name
// is a key for the registration it was already making. Here the two forms decode
// DIFFERENT BLOCKS, so a form flag would put a branch on the hot path of every
// host call in the API to serve five members, and every generated binding in
// both languages would have to pass a fifth constant: the wasm of every guest in
// existence changes, for a golden diff nobody can review. The callback seam's own
// fk.register and fk.remote_call are the precedent for a second import where the
// work behind it is different.
//
// PRUNING IS UNAFFECTED BY CONSTRUCTION, which is what makes the choice safe:
// the member id is still an i32 constant in operand 1 of its own call, so
// UsedMembers unions two scans and usedIDs itself does not change.
const TypedDispatchName = "call_typed"

// memberOperand is the argument position holding the member id:
// fk.call(handle, member, argp, retp), and fk.call_typed with the same shape.
const memberOperand = 1

// SubscribeName is the import a guest asks for an event with. Its only
// argument is the event id, so the same constant scan prunes the event table.
const SubscribeName = "subscribe"

// DefineName is the import a guest reads a `defines.*` value with. Its only
// argument is the per-build define id.
const DefineName = "define"

// UsedMembers reports which member ids a module references.
//
// `complete` is false when a call site's member id could not be proven
// constant. A caller that gets false must ship the whole table: some member is
// reached by an id this cannot see, and guessing would produce a mod that fails
// on whichever path computes one.
// THE UNION OF BOTH DISPATCH IMPORTS, and it has to be a union rather than a
// choice: a guest may call one member the tier-2 way and another the typed way
// in the same program, and a member reached only through fk.call_typed that was
// pruned out would answer ERR_NO_MEMBER at runtime. `complete` is the AND, for
// the same reason -- either scan giving up means some id is unseen.
func UsedMembers(m *ir.Module) (ids map[int]bool, complete bool) {
	ids, complete = usedIDs(m, DispatchName, memberOperand)
	tids, tcomplete := usedIDs(m, TypedDispatchName, memberOperand)
	for id := range tids {
		ids[id] = true
	}
	return ids, complete && tcomplete
}

// UsedEvents is the same scan over fk.subscribe, whose single argument is the
// event id. A guest that subscribes to two events ships two, not all of them.
func UsedEvents(m *ir.Module) (ids map[int]bool, complete bool) {
	return usedIDs(m, SubscribeName, 0)
}

// UsedDefines is the same scan over fk.define, whose single argument is the
// define id.
//
// THIS IS WHY A DEFINE READ IS AN IMPORT CALL rather than a load from a table
// the host filled in at init. The whole defines set (define_values in
// census.json) is tens of KB of generated Lua; a guest that wants four
// directions has no business shipping the rest, and the only pruning
// machinery this compiler has is a scan
// for a constant reaching an IMPORT. A memory-resident table would have been
// faster per read and unprunable, so the generated accessor caches the value on
// first use instead: one host call per define for the life of the mod, and a
// table the size of what the guest actually names.
func UsedDefines(m *ir.Module) (ids map[int]bool, complete bool) {
	return usedIDs(m, DefineName, 0)
}

func usedIDs(m *ir.Module, importName string, operand int) (ids map[int]bool, complete bool) {
	ids = map[int]bool{}
	src := m.Source
	if src == nil {
		return ids, true
	}

	// Which function index is the dispatch import, if the guest imports it at
	// all. A guest that does not is trivially complete with no members.
	dispatch := -1
	for _, im := range src.Imports {
		if im.Module == DispatchModule && im.Name == importName {
			dispatch = int(im.Index)
			break
		}
	}
	if dispatch < 0 {
		return ids, true
	}

	complete = true
	for _, f := range m.Funcs {
		// The value each slot last held, when it was a constant. Cleared at
		// every control-flow boundary for the same reason the peephole's
		// forwarding table is: a slot written on one path and read on another
		// holds whichever value arrived, and this pass cannot know which.
		konst := map[ir.Slot]uint32{}
		for i := range f.Steps {
			s := &f.Steps[i]
			switch {
			case s.Op == wasm.OpI32Const:
				if s.Dst != ir.NoSlot {
					konst[s.Dst] = s.Instr.I32
				}
				continue
			case s.Op == wasm.OpCall && int(s.Instr.FuncIndex) == dispatch:
				if len(s.Args) <= operand {
					complete = false
					continue
				}
				v, ok := konst[s.Args[operand]]
				if !ok {
					complete = false
					continue
				}
				ids[int(v)] = true
			}
			// Anything else invalidates the slot it wrote, and a boundary
			// invalidates everything.
			if isBoundary(s.Op) {
				konst = map[ir.Slot]uint32{}
				continue
			}
			if s.Dst != ir.NoSlot {
				delete(konst, s.Dst)
			}
		}
	}
	return ids, complete
}

// isBoundary mirrors the emitter's rule: control flow ends what a straight-line
// analysis can claim.
func isBoundary(op wasm.Op) bool {
	switch op {
	case wasm.OpBlock, wasm.OpLoop, wasm.OpIf, wasm.OpElse, wasm.OpEnd,
		wasm.OpBr, wasm.OpBrIf, wasm.OpBrTable, wasm.OpReturn, wasm.OpUnreachable:
		return true
	}
	return false
}
