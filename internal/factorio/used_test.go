package factorio

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

func buildIR(t *testing.T, wat string) *ir.Module {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	return im
}

// A guest that calls five members should ship five, not the whole table.
func TestUsedMembersFindsConstantIDs(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(memory 1)
		(func (export "f") (param $h i32) (result i32)
			(drop (call $call (local.get $h) (i32.const 12) (i32.const 0) (i32.const 64)))
			(drop (call $call (local.get $h) (i32.const 3000) (i32.const 0) (i32.const 64)))
			(call $call (local.get $h) (i32.const 12) (i32.const 0) (i32.const 64))))`)

	ids, complete := UsedMembers(im)
	if !complete {
		t.Error("every id here is a literal; the scan should be complete")
	}
	if len(ids) != 2 || !ids[12] || !ids[3000] {
		t.Errorf("ids = %v, want {12, 3000}", ids)
	}
}

// A guest that computes a member id gets the WHOLE table. The alternative to
// "provably constant" is not "probably fine" -- it is a member missing at
// runtime on whichever path computes one.
func TestAComputedIDForcesTheFullTable(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(func (export "f") (param $h i32) (param $which i32) (result i32)
			(call $call (local.get $h) (local.get $which) (i32.const 0) (i32.const 0))))`)

	_, complete := UsedMembers(im)
	if complete {
		t.Error("the member id comes from a parameter; the scan cannot be complete")
	}
}

// A guest that never reaches the API imports nothing and needs no table at all.
func TestAGuestWithNoHostCallsUsesNoMembers(t *testing.T) {
	im := buildIR(t, `(module (func (export "f") (result i32) (i32.const 1)))`)
	ids, complete := UsedMembers(im)
	if !complete || len(ids) != 0 {
		t.Errorf("ids = %v complete = %v; want empty and complete", ids, complete)
	}
}

// An id that crossed a branch is not provably the one that arrives. Being wrong
// here means dropping a member the guest really calls, so the scan gives up
// rather than guessing.
func TestAnIDAcrossAControlFlowBoundaryIsNotAssumed(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(func (export "f") (param $h i32) (param $c i32) (result i32)
			(local $mid i32)
			(local.set $mid (i32.const 7))
			(if (local.get $c) (then (local.set $mid (i32.const 9))))
			(call $call (local.get $h) (local.get $mid) (i32.const 0) (i32.const 0))))`)

	ids, complete := UsedMembers(im)
	if complete {
		t.Errorf("the id is written on two paths; the scan claimed completeness with %v", ids)
	}
}

// Pruning keeps ids EXACTLY as they were. The guest baked them in when its
// bindings were generated, so renumbering to close the gaps would point every
// call at the wrong member -- the worst possible way to save a few kilobytes.
func TestPruningPreservesIDs(t *testing.T) {
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	if len(full.Members) < 100 {
		t.Fatal("expected the full table")
	}
	keep := map[int]bool{7: true, 1500: true, len(full.Members): true}
	small := full.Only(keep)

	if len(small.Members) != 3 {
		t.Fatalf("kept %d members, want 3", len(small.Members))
	}
	for _, m := range small.Members {
		if !keep[m.ID] {
			t.Errorf("kept member %d that was not asked for", m.ID)
		}
		// The same entry, unchanged.
		orig := full.Members[m.ID-1]
		if orig.Name != m.Name || orig.Class != m.Class || orig.Kind != m.Kind {
			t.Errorf("member %d changed identity: %v vs %v", m.ID, m, orig)
		}
	}
	// And the skip report survives: what a build could not express is still
	// worth reporting even when this guest never asked for it.
	if len(small.Skipped) != len(full.Skipped) {
		t.Error("pruning dropped the skip report")
	}
}

// The size argument, made concrete. A pruned table is a rounding error next to
// the full one, and the generated Lua indexes by the original id.
func TestAPrunedTableIsTiny(t *testing.T) {
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	fullSrc, err := full.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	small := full.Only(map[int]bool{12: true, 500: true, 2000: true, 3000: true, 3400: true})
	smallSrc, err := small.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(smallSrc) > len(fullSrc)/100 {
		t.Errorf("five members render as %d bytes against %d for all of them; "+
			"pruning is supposed to be the difference between a mod carrying the "+
			"API and carrying what it calls", len(smallSrc), len(fullSrc))
	}
	t.Logf("full %d bytes (%d members) -> pruned %d bytes (5 members)",
		len(fullSrc), len(full.Members), len(smallSrc))
}

// A GENERATED WRAPPER THE TOOLCHAIN DECLINED TO INLINE, which is the shape the
// scan used to give up on and item 30 is about. The id is at the CALL SITES,
// where the guest wrote it, and the wrapper passes it straight to the import.
//
// Two call sites passing different literals, so a scan that found only one of
// them would fail here rather than passing on a single-id coincidence.
func TestAnIDThroughAWrapperTheToolchainLeftOutOfLineIsStillProvable(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func (export "f") (result i32)
			(drop (call $wrap (i32.const 5)))
			(call $wrap (i32.const 9))))`)

	ids, complete := UsedEvents(im)
	if !complete {
		t.Fatal("the id reaches fk.subscribe as this wrapper's own parameter and " +
			"every call site passes a literal; one level of call graph proves it")
	}
	if len(ids) != 2 || !ids[5] || !ids[9] {
		t.Errorf("ids = %v, want {5, 9} -- the union over the wrapper's call sites", ids)
	}
}

// The same, at an operand that is not the first, so nothing about this rests on
// fk.subscribe's single-argument shape. fk.call(handle, member, argp, retp).
func TestAWrapperIsSeenThroughAtAnyOperandPosition(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(func $wrap (param $h i32) (param $m i32) (result i32)
			(call $call (local.get $h) (local.get $m) (i32.const 0) (i32.const 64)))
		(func (export "f") (param $h i32) (result i32)
			(drop (call $wrap (local.get $h) (i32.const 12)))
			(call $wrap (local.get $h) (i32.const 3000))))`)

	ids, complete := UsedMembers(im)
	if !complete {
		t.Fatal("the member id is the wrapper's second parameter and both call " +
			"sites pass a literal; the handle being unknown is not the question")
	}
	if len(ids) != 2 || !ids[12] || !ids[3000] {
		t.Errorf("ids = %v, want {12, 3000}", ids)
	}
}

// A wrapper this analysis cannot see all the callers of. An EXPORT is called by
// the host and an ELEMENT SEGMENT entry is reachable through call_indirect with
// whatever a guest computed, so in both the union over the call sites we CAN see
// is not the whole story and the only safe answer is the whole table.
//
// START is the third escape and is deliberately not a case here: a start
// function takes no parameters, so it can never be one of the pending pairs in
// the first place. escapingFuncs lists it anyway, because the set it builds is
// "functions whose callers are not all in this module", and a set that is right
// only for the callers of today is the omission this whole item is about.
func TestAWrapperThatEscapesGivesUp(t *testing.T) {
	body := `
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func (export "f") (result i32)
			(call $wrap (i32.const 5)))`
	for _, tc := range []struct{ name, extra string }{
		{"exported", `(export "wrap" (func $wrap))`},
		{"in the function table", "(table 1 funcref) (elem (i32.const 0) $wrap)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := buildIR(t, "(module "+body+" "+tc.extra+")")
			ids, complete := UsedEvents(im)
			if complete {
				t.Errorf("the wrapper is %s, so its callers are not all in this "+
					"module; the scan claimed completeness with %v", tc.name, ids)
			}
		})
	}
}

// ONE call site that cannot be proven constant loses the whole argument, not
// just its own id: the union is missing whatever that site passed, and pruning
// to it would drop an event the guest really subscribes to.
func TestOneUnprovableCallSiteLosesTheWholeArgument(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func (export "f") (param $which i32) (result i32)
			(drop (call $wrap (i32.const 5)))
			(call $wrap (local.get $which))))`)

	ids, complete := UsedEvents(im)
	if complete {
		t.Errorf("one call site computes its id, so the union is incomplete; "+
			"the scan claimed otherwise with %v", ids)
	}
}

// A parameter the wrapper ASSIGNS no longer holds what the caller passed, so
// reading it says nothing at all about the call sites. This is the one rule
// that makes "a parameter is the caller's argument" true rather than plausible.
func TestAWrapperThatReassignsItsParameterGivesUp(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(local.set $e (i32.const 77))
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func (export "f") (result i32)
			(call $wrap (i32.const 5))))`)

	ids, complete := UsedEvents(im)
	if complete {
		t.Errorf("the wrapper overwrites its parameter, so its call sites do not "+
			"say what reaches the import; the scan claimed completeness with %v", ids)
	}
}

// ONE LEVEL, and the second is deliberately not taken. A call-site argument
// that is itself a parameter of ITS caller is not a constant, so a wrapper
// calling a wrapper -- both left out of line -- gives up. Following further
// would want a fixpoint, and the shape this exists for is the one the generator
// produces, which is one deep.
func TestTwoOutOfLineWrappersDeepIsOneLevelTooFar(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $inner (param $e i32) (result i32)
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func $outer (param $e i32) (result i32)
			(call $inner (local.get $e)))
		(func (export "f") (result i32)
			(call $outer (i32.const 5))))`)

	ids, complete := UsedEvents(im)
	if complete {
		t.Errorf("the id passes through two out-of-line wrappers and the scan "+
			"follows one; it claimed completeness with %v", ids)
	}
}

// A wrapper NOTHING CALLS cannot reach the import at runtime, so it constrains
// nothing and the rest of the module is still provable. Strictly better than
// giving up: it is the one pending shape where there is demonstrably no id to
// miss.
func TestAWrapperNothingCallsConstrainsNothing(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
		(func (export "f") (result i32)
			(drop (call $sub (i32.const 42) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
			(i32.const 0)))`)

	ids, complete := UsedEvents(im)
	if !complete {
		t.Fatal("nothing calls the wrapper, so its import call is unreachable " +
			"and no id is being missed")
	}
	if len(ids) != 1 || !ids[42] {
		t.Errorf("ids = %v, want {42} -- the one subscription that can run", ids)
	}
}

// A wrapper that calls ITSELF passes its own parameter at that call site, which
// is not a constant, so the union is incomplete and the scan gives up. Recursion
// needs no special case and cannot loop: there is no fixpoint to iterate.
func TestARecursiveWrapperGivesUpAndTerminates(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(func $wrap (param $e i32) (result i32)
			(drop (call $sub (local.get $e) (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))
			(call $wrap (local.get $e)))
		(func (export "f") (result i32)
			(call $wrap (i32.const 5))))`)

	ids, complete := UsedEvents(im)
	if complete {
		t.Errorf("the self-call passes the parameter through, which is not a "+
			"constant; the scan claimed completeness with %v", ids)
	}
}

// A STALE CONSTANT MUST NOT BE PROVEN AS AN ID, which is the one change in this
// round that could prune MORE rather than less -- so it gets its own proof.
//
// A slot is normally only readable as an operand after some step whose Dst is
// that slot, and a Dst clears the slot in either scan, so a stale constant can
// never be reached. The exception is a MULTI-VALUE result: Step.Dst records
// result 0 alone, so results 1..n are real stack values with no Dst of their
// own and the walk sees no write for them. That is the shape below, and it is
// the ONLY observable one -- measured, not assumed. An i64 single result (Dst
// at N, two slots wide) leaves N+1 stale in the old scan and cannot be read,
// because anything that later pushes to N+1 writes it; and a dispatch call's
// own Dst is its FIRST ARGUMENT's slot while the member operand sits one above
// it, so the id a later call reads is never the slot an earlier one left.
//
// What the old scan does here is the failure this repo cannot ship: it reports
// complete=true and an id NOTHING CALLED, so the table is pruned to a member
// the guest never asked for while the one it really calls is dropped and
// answers ERR_NO_MEMBER in game.
func TestAStaleConstantUnderAMultiValueResultIsNotAnID(t *testing.T) {
	im := buildIR(t, `(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(func $two (result i32 i32) (i32.const 1) (i32.const 2))
		(func (export "f") (result i32)
			(drop (i32.add (i32.const 100) (i32.const 222)))
			(call $two)
			(call $call (i32.const 0) (i32.const 0))))`)

	ids, complete := UsedMembers(im)
	if ids[222] {
		t.Errorf("222 was proven as a member id, and nothing calls member 222: "+
			"it is a constant left on the second result's slot, which the walk "+
			"saw no Dst for. ids = %v", ids)
	}
	if complete {
		t.Errorf("the member id is a multi-value call's second result, which is "+
			"not a constant; the scan claimed completeness with %v -- and a "+
			"complete=true here PRUNES the member the guest really calls", ids)
	}
}
