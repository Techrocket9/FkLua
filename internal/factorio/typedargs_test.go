package factorio

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// A METHOD WHOSE PARAMETER TABLE IS A DISCRIMINATED UNION GETS A SECOND, TYPED
// ARGUMENT LIST, and the population is a measurement per description rather than
// a number written down.
//
// The generator's own comment said "the four of these are set_gui_arrow,
// LuaGuiElement::add, create_entity and create_segmented_unit", and it has been
// wrong since 2.1.16: LuaSimulation::get_widget_position is a fifth. That is the
// whole reason this walks every committed description and asserts an IDENTITY
// rather than a constant -- a count in prose cannot notice a description
// growing one, and a typed form that silently stopped being emitted for a member
// would leave the tier-2 form working and say nothing.
func TestEveryVariantGroupMethodGetsATypedArgumentList(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
			if err != nil {
				t.Fatal(err)
			}
			// What the DESCRIPTION says, walked here rather than taken from the
			// generator, so the two cannot agree by sharing a bug.
			want := map[string]bool{}
			for _, c := range a.Classes {
				for _, m := range c.Methods {
					if len(m.VariantGroups) > 0 {
						want[c.Name+"::"+m.Name] = true
					}
				}
			}
			if len(want) == 0 {
				t.Fatalf("%s declares no variant-group method at all, which no "+
					"published description does -- a walk that matched nothing "+
					"passes forever", v)
			}

			r := GenerateMembers(a)
			got := map[string]bool{}
			for _, m := range r.Members {
				if len(m.TypedArgs) == 0 {
					continue
				}
				got[m.Class+"::"+m.Name] = true
				// THE SHAPE OF THE SECOND LIST IS PART OF THE CONTRACT: a
				// tier-1 block, then one OPTIONAL tier-2 slot for the variant
				// tail. fk_abi.lua's call_typed reads targs[1] and targs[2] by
				// position, so a third field or a reordering would be a silent
				// mis-decode.
				if len(m.TypedArgs) != 2 ||
					m.TypedArgs[0].Kind != KindStruct ||
					m.TypedArgs[1].Kind != KindDyn || !m.TypedArgs[1].Optional {
					t.Errorf("%s::%s typed args are %v, want a struct then an optional dyn",
						m.Class, m.Name, m.TypedArgs)
				}
			}
			for k := range want {
				if !got[k] {
					t.Errorf("%s: %s has variant groups and no typed argument list", v, k)
				}
			}
			for k := range got {
				if !want[k] {
					t.Errorf("%s: %s got a typed argument list and has no variant groups", v, k)
				}
			}
		})
	}
}

// THE VARIANT TAIL IS NOT MERELY EXTRA KEYS, which is why it is allowed to
// override the block and why it lives OUTSIDE it.
//
// A shared parameter and a variant-group parameter may carry the SAME NAME --
// create_entity's `target` does, at every committed description. So the tail is
// a namespace that overlaps the block's, and an `extra` field placed inside the
// struct would have to occupy a key of its own in a space the description
// controls.
//
// This is the measurement behind those two decisions, asserted rather than
// recorded in a comment, so the day a pin removes the overlap the reasoning is
// re-read instead of being inherited.
func TestASharedParameterCanShareANameWithAVariantOne(t *testing.T) {
	for _, v := range committedVersions(t) {
		a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		overlaps := 0
		for _, c := range a.Classes {
			for _, m := range c.Methods {
				if len(m.VariantGroups) == 0 {
					continue
				}
				shared := map[string]bool{}
				for _, p := range m.Parameters {
					shared[p.Name] = true
					if p.Name == "extra" {
						t.Errorf("%s: %s::%s has a SHARED parameter named `extra`",
							v, c.Name, m.Name)
					}
				}
				for _, g := range m.VariantGroups {
					for _, p := range g.Parameters {
						if shared[p.Name] {
							overlaps++
						}
					}
				}
			}
		}
		if overlaps == 0 {
			t.Errorf("%s: no shared parameter shares a name with a variant one. "+
				"That is not a failure of the code, it is the premise of two "+
				"decisions (the tail overrides the block, and the tail lives "+
				"outside it) no longer holding -- go and re-read them", v)
		}
	}
}

// BOTH BACKENDS EMIT THE SAME TYPED VARIANTS, compared as a count at every
// committed description and by NAME at the pin.
//
// AD5 is what happens otherwise: one generator gets the shape and the other does
// not, both test suites are green, and a mod author reports it two milestones
// later. The census carries the count for the same reason.
func TestBothBackendsEmitTheSameTypedVariants(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
			if err != nil {
				t.Fatal(err)
			}
			r := GenerateMembers(a)
			ev := GenerateEvents(a)
			g, err := GenerateGoWith(a, r, ev, "fkapi")
			if err != nil {
				t.Fatal(err)
			}
			rb, err := GenerateRust(a, r, ev)
			if err != nil {
				t.Fatal(err)
			}
			members := 0
			for _, m := range r.Members {
				if len(m.TypedArgs) > 0 {
					members++
				}
			}
			if g.TypedVariants != members || rb.TypedVariants != members {
				t.Fatalf("%d members carry a typed argument list; go emitted %d "+
					"bindings and rust %d. One member, one binding per language: "+
					"a difference is a member whose typed form was dropped in "+
					"one backend alone", members, g.TypedVariants, rb.TypedVariants)
			}
			// BY NAME as well as by count, because a count cannot say that what
			// was emitted is the member this exists for.
			if !strings.Contains(g.Source, "func (o LuaGuiElement) AddTyped(args LuaGuiElementAddArgs, extra *Value)") {
				t.Errorf("%s: the Go bindings carry no typed form of LuaGuiElement::add", v)
			}
			if !strings.Contains(rb.Source, "pub fn add_typed(&self, args: LuaGuiElementAddArgs, extra: Option<&Value>)") {
				t.Errorf("%s: the Rust bindings carry no typed form of LuaGuiElement::add", v)
			}
			// AND IT GOES THROUGH THE OTHER IMPORT. A typed binding that called
			// fk.call would hand the host a flat block to decode as one tier-2
			// value -- a garbage read with no error anywhere.
			if !strings.Contains(g.Source, "hostCallTyped(o.h, ") {
				t.Errorf("%s: no Go typed binding calls hostCallTyped", v)
			}
			if !strings.Contains(rb.Source, "fk_call_typed(self.0.0, ") {
				t.Errorf("%s: no Rust typed binding calls fk_call_typed", v)
			}
		})
	}
}

// THE TYPED LAYOUT IS IN THE PACKAGED TABLE AND IN THE ABI SIGNATURE.
//
// The table is what the host decodes with, so a member with a typed form and no
// `targs` row answers ERR_BAD_ARGS for every typed call. The signature is what
// says a guest was built against a DIFFERENT typed layout at the same pin --
// same member, same id, different wire, which is the exact class fk_api_sig_*
// exists for and which a digest over the two old blocks alone could not see.
func TestTheTypedLayoutReachesTheTableAndTheSignature(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	src, err := r.LuaSourceWith(a, ev)
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Count(src, "targs=")
	members := 0
	for _, m := range r.Members {
		if len(m.TypedArgs) > 0 {
			members++
		}
	}
	if n != members {
		t.Errorf("%d members carry a typed argument list and the table has %d "+
			"targs= rows", members, n)
	}
	if !strings.Contains(src, "targsize=") {
		t.Error("the table carries no targsize=, so the host cannot size the block")
	}

	// The signature MOVES when a typed layout does. Proved by moving one --
	// against a copy, so nothing here mutates the loaded description.
	before := APISignature(a)
	for i := range a.Classes {
		for j := range a.Classes[i].Methods {
			m := &a.Classes[i].Methods[j]
			if len(m.VariantGroups) == 0 || len(m.Parameters) < 2 {
				continue
			}
			m.Parameters = m.Parameters[:len(m.Parameters)-1]
			after := APISignature(a)
			if after == before {
				t.Errorf("dropping a shared parameter of %s::%s left the ABI "+
					"signature at %s: the digest cannot see a typed block move",
					a.Classes[i].Name, m.Name, before)
			}
			return
		}
	}
	t.Fatal("no variant-group method with two shared parameters, so nothing was proved")
}

// A MEMBER REACHED ONLY THROUGH THE TYPED IMPORT IS STILL PRUNED IN.
//
// UsedMembers scans for an i32 constant at operand 1 of fk.call. A member called
// only as <Name>Typed reaches operand 1 of fk.call_typed instead, so a scan over
// one import alone would prune it out of the shipped table and the call would
// answer ERR_NO_MEMBER -- with the generated source looking perfectly correct.
//
// A wat module rather than a compiled guest, because what is under test is the
// SCAN and a real guest would also be proving that the wrapper inlined. The
// end-to-end version of that is internal/guest's typedargs fixture, which
// packages through this same function.
func TestAMemberCalledOnlyThroughTheTypedImportIsNotPruned(t *testing.T) {
	m := buildIRFromWat(t, `(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (import "fk" "call_typed" (func $callt (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_init")
    (drop (call $call (i32.const 0) (i32.const 11) (i32.const 0) (i32.const 0)))
    (drop (call $callt (i32.const 0) (i32.const 1932) (i32.const 0) (i32.const 0)))))`)
	ids, complete := UsedMembers(m)
	if !complete {
		t.Fatal("the scan gave up on a module whose ids are both constants")
	}
	for _, want := range []int{11, 1932} {
		if !ids[want] {
			t.Errorf("member %d is missing from the used set %v", want, ids)
		}
	}
	if len(ids) != 2 {
		t.Errorf("used set is %v, want exactly the two called members", ids)
	}
}

// ...AND AN UNPROVABLE ID IN EITHER IMPORT MAKES THE WHOLE SCAN INCOMPLETE.
//
// `complete` is the AND of the two scans, not the OR: either one giving up means
// some member is reached by an id nothing can see, and shipping a pruned table
// then produces a mod that fails on whichever path computed one. The safe
// direction is a bigger mod.
func TestAnUnprovableTypedCallMakesTheScanIncomplete(t *testing.T) {
	m := buildIRFromWat(t, `(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (import "fk" "call_typed" (func $callt (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_init") (param i32)
    (drop (call $call (i32.const 0) (i32.const 11) (i32.const 0) (i32.const 0)))
    (drop (call $callt (i32.const 0) (local.get 0) (i32.const 0) (i32.const 0)))))`)
	if _, complete := UsedMembers(m); complete {
		t.Fatal("a member id that is a parameter was reported as provably constant")
	}
}

func buildIRFromWat(t *testing.T, wat string) *ir.Module {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("DecodeWAT: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	return im
}
