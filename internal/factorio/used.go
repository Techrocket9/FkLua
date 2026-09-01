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
// The analysis is deliberately shallow -- because the alternative to "provably
// constant" is not "probably fine", it is a missing member at runtime. Anything
// it cannot prove makes it give up on pruning entirely, which is the safe
// direction: a bigger mod, never a broken one.
//
// It is ONE LEVEL INTERPROCEDURAL and used to be none. An i32.const feeding the
// operand in the function being read is still the fast answer; when instead the
// operand is a PARAMETER THAT FUNCTION NEVER WRITES, the ids are read off that
// function's own direct call sites, which is what makes a generated wrapper the
// toolchain declined to inline provable rather than fatal. Two levels are not
// followed and no fixpoint is taken: past one, the give-up stands. See usedIDs.

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

// BulkGetName is the THIRD import that names a member id: ONE ATTRIBUTE READ
// OFF N HANDLES IN ONE CROSSING.
//
// fk.bulk_get(member, handlep, count, dstp, retp), so the id is operand 0 --
// there is no receiver, because a bulk read's receivers are the array it was
// handed. Three shapes were available and this is why it is the third:
//
//   - a member KIND, one bulk id per eligible attribute. Pruning works
//     unchanged and the member table grows by +1,533 at the GA pin -- a third
//     again, in every save, every download and every load.
//   - ONE bulk member with the TARGET id inside the ARGUMENT BLOCK. No new
//     members, and usedIDs below cannot see an i32.const that is STORED to
//     memory rather than passed to an import: a guest reading only in bulk
//     would ship all 4,268 members and nothing would say so. That is the R6
//     failure shape -- a pruning scan defeated by a call-site detail -- with the
//     detail moved one level in.
//   - a NEW IMPORT, which is this. No new member ids anywhere, and pruning
//     works BY CONSTRUCTION: the id is an ordinary i32 operand of a call to an
//     import, which is exactly the shape usedIDs was built for.
//
// A third scan is four lines and usedIDs itself does not change.
const BulkGetName = "bulk_get"

// memberOperand is the argument position holding the member id:
// fk.call(handle, member, argp, retp), and fk.call_typed with the same shape.
const memberOperand = 1

// bulkMemberOperand is the same fact for fk.bulk_get, one operand over.
const bulkMemberOperand = 0

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
// THE UNION OF ALL THREE DISPATCH IMPORTS, and it has to be a union rather than
// a choice: a guest may call one member the tier-2 way, another the typed way
// and a third in bulk in the same program, and a member reached only through
// fk.call_typed or fk.bulk_get that was pruned out would answer ERR_NO_MEMBER at
// runtime. `complete` is the AND, for the same reason -- any scan giving up
// means some id is unseen.
func UsedMembers(m *ir.Module) (ids map[int]bool, complete bool) {
	ids, complete = usedIDs(m, DispatchName, memberOperand)
	tids, tcomplete := usedIDs(m, TypedDispatchName, memberOperand)
	for id := range tids {
		ids[id] = true
	}
	bids, bcomplete := usedIDs(m, BulkGetName, bulkMemberOperand)
	for id := range bids {
		ids[id] = true
	}
	return ids, complete && tcomplete && bcomplete
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

// ONE LEVEL INTERPROCEDURAL, AND WHY THERE IS EXACTLY ONE.
//
// The scan above wants an i32.const on the import's operand IN THE FUNCTION IT
// IS READING, which is only ever true if the wrapper the guest called was
// inlined into the guest's own code. When it is not -- when `Subscribe(id)`
// survives as a real wasm function -- the id arrives at the import as
// `local.get $0`, the scan gives up, and, being all-or-nothing, the mod ships
// every descriptor there is. That happened: filed by BetterBeltBalancer (item
// 30), where LLVM declined to inline the generated `SubscribeFiltered` under
// `-gc=custom` and `fk_api_gen.lua` went from 8,425 bytes to 60,118 with a
// literal id at every call site in the guest. See agents/abi.md, "Item 30".
//
// Inlining hints hold that instance and cannot hold the class: `//go:inline`
// lowers to an LLVM `inlinehint` the inliner weighs and may decline, so pruning
// would stay one cost-model decision away from silently shipping everything. So
// the scan follows the call graph one level:
//
//   - a slot is tracked as holding PARAMETER k exactly as a slot is tracked as
//     holding a constant -- same writes, same invalidation, same clearing at
//     every control-flow boundary -- and only for a parameter the function NEVER
//     WRITES, since a reassigned one no longer holds what the caller passed;
//   - such a parameter reaching the import makes the (function, parameter) pair
//     PENDING rather than incomplete, and its ids are then read off that
//     function's own DIRECT CALL SITES.
//
// ONE LEVEL, NO FIXPOINT, DELIBERATELY. A call-site argument that is itself a
// parameter of ITS caller counts as non-constant and gives up, which is also
// what makes recursion terminate with the safe answer and no special case. Two
// out-of-line wrappers in a row are a give-up too; the shape this exists for is
// the one the generator produces, which is one.
//
// The safe direction is absolute and every rule below points at it. A function
// that ESCAPES -- named by an export, by an element segment (so reachable by
// call_indirect with anything) or as the start function -- gives up, because
// this cannot see who calls it. A call site whose argument is not provably
// constant gives up. A pending function with NO call sites constrains nothing
// and is skipped: its import call is unreachable, so no id is being missed --
// which is strictly better than today rather than a new claim.
//
// Nothing here is order-dependent: `ids` is a set and `complete` is an AND over
// the give-ups, so iterating the pending map in whatever order Go picks
// produces the same two answers.
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
	// pending is nil until a wrapper needs it, so a guest whose wrappers all
	// inlined pays nothing at all for the second pass -- not even a map.
	var pending map[uint32]map[int]bool
	for _, f := range m.Funcs {
		// Most of a guest never touches this import at all, and the parameter
		// tracking below costs a pre-pass and a slice per function. One cheap
		// look first, so a module of thousands of functions pays for the
		// handful that reach the API.
		if !callsFunc(f, dispatch) {
			continue
		}
		walkCalls(f, true, func(s *ir.Step, w *slotFacts) {
			if int(s.Callee) != dispatch {
				return
			}
			if len(s.Args) <= operand {
				complete = false
				return
			}
			if v, ok := w.konst[s.Args[operand]]; ok {
				ids[int(v)] = true
				return
			}
			// Not a constant here. It may still be a parameter this function
			// never writes, in which case the constant is at its call sites.
			k, ok := w.param[s.Args[operand]]
			if !ok {
				complete = false
				return
			}
			if pending == nil {
				pending = map[uint32]map[int]bool{}
			}
			if pending[f.Index] == nil {
				pending[f.Index] = map[int]bool{}
			}
			pending[f.Index][k] = true
		})
	}
	if len(pending) == 0 {
		return ids, complete
	}

	// The second pass: what every direct call site passes at each pending
	// argument position.
	esc := escapingFuncs(src)
	sites := map[uint32]int{}
	seen := map[pendingArg]*argValues{}
	for fn, params := range pending {
		for k := range params {
			seen[pendingArg{fn, k}] = &argValues{vals: map[uint32]bool{}, allConst: true}
		}
	}
	for _, g := range m.Funcs {
		walkCalls(g, false, func(s *ir.Step, w *slotFacts) {
			params, ok := pending[s.Callee]
			if !ok {
				return
			}
			sites[s.Callee]++
			for k := range params {
				at := seen[pendingArg{s.Callee, k}]
				if k >= len(s.Args) {
					at.allConst = false
					continue
				}
				v, ok := w.konst[s.Args[k]]
				if !ok {
					at.allConst = false
					continue
				}
				at.vals[v] = true
			}
		})
	}
	for fn, params := range pending {
		if esc[fn] {
			complete = false
			continue
		}
		if sites[fn] == 0 {
			// Nothing calls it, so its import call cannot run. An unreachable
			// wrapper constrains nothing.
			continue
		}
		for k := range params {
			at := seen[pendingArg{fn, k}]
			if !at.allConst {
				complete = false
				continue
			}
			for v := range at.vals {
				ids[int(v)] = true
			}
		}
	}
	return ids, complete
}

// callsFunc reports a DIRECT call to one function index anywhere in a body.
func callsFunc(f *ir.Func, index int) bool {
	for i := range f.Steps {
		if f.Steps[i].Op == wasm.OpCall && int(f.Steps[i].Callee) == index {
			return true
		}
	}
	return false
}

// pendingArg names one argument position of one function.
type pendingArg struct {
	fn    uint32
	param int
}

// argValues is what the call sites of one pending argument were seen passing.
// allConst is the whole verdict: one call site that could not be proven
// constant makes the union meaningless, because the id it passed is not in it.
type argValues struct {
	vals     map[uint32]bool
	allConst bool
}

// slotFacts is what the walk below knows about each slot at one instruction.
//
// konst is the value a slot last held when that was a constant. param is the
// same shape for a different fact: the PARAMETER whose incoming value a slot
// holds. Both are cleared at every control-flow boundary, for the same reason
// the peephole's forwarding table is -- a slot written on one path and read on
// another holds whichever value arrived, and this pass cannot know which.
type slotFacts struct {
	konst map[ir.Slot]uint32
	param map[ir.Slot]int
	// written[k] reports a parameter this function ASSIGNS. A wasm local changes
	// only through local.set and local.tee, so a parameter named by neither
	// still holds what the caller passed -- and one named by either does not,
	// which is why reading it can say nothing about a call site. nil when the
	// walk is not tracking parameters at all.
	written []bool
}

// walkCalls replays a function's straight-line tracking and hands every DIRECT
// call the facts as they stood at that instruction. Both passes go through it,
// so there is one statement of what "provably constant here" means.
//
// trackParams buys the second table, and with it a linear pre-pass over the
// body. The call-site pass does not need it: what a call site passes has to be
// a constant or nothing, one level being one level.
func walkCalls(f *ir.Func, trackParams bool, visit func(s *ir.Step, w *slotFacts)) {
	w := &slotFacts{konst: map[ir.Slot]uint32{}}
	if trackParams && len(f.Params) > 0 {
		w.param = map[ir.Slot]int{}
		w.written = make([]bool, len(f.Params))
		for i := range f.Steps {
			s := &f.Steps[i]
			if s.Op == wasm.OpLocalSet || s.Op == wasm.OpLocalTee {
				if int(s.Instr.LocalIndex) < len(w.written) {
					w.written[s.Instr.LocalIndex] = true
				}
			}
		}
	}

	forget := func(base ir.Slot, t wasm.ValType) {
		if base == ir.NoSlot {
			return
		}
		for k := 0; k < t.Slots(); k++ {
			delete(w.konst, base+ir.Slot(k))
			delete(w.param, base+ir.Slot(k))
		}
	}
	for i := range f.Steps {
		s := &f.Steps[i]
		if s.Op == wasm.OpI32Const {
			if s.Dst != ir.NoSlot {
				w.konst[s.Dst] = s.Instr.I32
				delete(w.param, s.Dst)
			}
			continue
		}
		if s.Op == wasm.OpLocalGet && s.Dst != ir.NoSlot && w.written != nil &&
			int(s.Instr.LocalIndex) < len(w.written) && !w.written[s.Instr.LocalIndex] {
			w.param[s.Dst] = int(s.Instr.LocalIndex)
			delete(w.konst, s.Dst)
			continue
		}
		if s.Op == wasm.OpCall {
			visit(s, w)
		}
		// Anything else invalidates the slot it wrote, and a boundary
		// invalidates everything.
		//
		// A CALL INVALIDATES ITS OWN Dst TOO, which this loop used to skip past
		// on the way to recording an id. A call's result lands on the slot its
		// first argument came from, so a constant handle left that slot holding
		// a stale constant a later call at the same stack depth could have read
		// as its member id -- a prune in the UNSAFE direction, narrow but real.
		// Visiting before invalidating costs nothing and removes it. The width
		// is the value's rather than one slot, for the same reason: an i64
		// result covers two, and a multi-value call covers its whole span.
		if isBoundary(s.Op) {
			w.konst = map[ir.Slot]uint32{}
			if w.param != nil {
				w.param = map[ir.Slot]int{}
			}
			continue
		}
		if s.Dst == ir.NoSlot {
			continue
		}
		if len(s.ResultTypes) > 1 {
			at := s.Dst
			for _, rt := range s.ResultTypes {
				forget(at, rt)
				at += ir.Slot(rt.Slots())
			}
			continue
		}
		forget(s.Dst, s.DstType)
	}
}

// escapingFuncs is every function this analysis cannot see all the callers of.
//
// An EXPORT is called by the host, an ELEMENT SEGMENT entry is callable through
// call_indirect with whatever the guest computed, and START runs at
// instantiation. Element membership escapes whether or not the module actually
// contains a call_indirect, which is more conservative than it needs to be and
// costs one scan less.
//
// START cannot in fact BE one of the pending pairs -- a start function takes no
// parameters, so nothing in it can be one -- and it is here anyway, because what
// this builds is "functions whose callers are not all in this module", and a set
// that is right only for the callers of today is the omission item 30 was.
func escapingFuncs(src *wasm.Module) map[uint32]bool {
	esc := map[uint32]bool{}
	for _, e := range src.Exports {
		esc[e.FuncIndex] = true
	}
	for _, seg := range src.Elems {
		for _, fi := range seg.Funcs {
			esc[fi] = true
		}
	}
	if src.Start >= 0 {
		esc[uint32(src.Start)] = true
	}
	return esc
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
