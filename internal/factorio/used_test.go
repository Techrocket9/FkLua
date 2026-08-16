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
