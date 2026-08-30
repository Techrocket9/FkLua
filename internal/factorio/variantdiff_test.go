package factorio

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE DIFF NEVER READ A METHOD'S VARIANT GROUPS, and they decide the same thing
// `takes_table` does.
//
// Both generators branch on the group count being zero: with none, the shared
// parameters lay out as a tier-1 struct or as positional arguments; with any,
// the whole argument table crosses as ONE tier-2 value and the typed form is a
// block plus an `extra` slot. So a method that gains its FIRST group or loses
// its LAST changes the shape of every call to it -- and `diffSignature` walked
// only the top-level parameters, which reports that as an ordinary list of
// fields where the shared parameters happen to move and as NOTHING AT ALL where
// they do not.
//
// ZERO INSTANCES EXIST IN ANY COMMITTED PAIR, which is why this arrived as a
// blind spot rather than as a defect somebody hit. So the pair is SYNTHETIC --
// the `loadShapeAPI` precedent one step further, since there at least one
// committed description had the shape and here none does. A test that could only
// run against a committed pair would be a test that never runs.
func TestTheDiffNoticesAVariantGroupAppearingOrDisappearing(t *testing.T) {
	base, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	// One method, two shapes, everything else identical. The parameters are the
	// SAME in both, which is the half that makes this a blind spot rather than a
	// misclassification: with them equal, the old walk had nothing to report.
	shared := []Parameter{{Name: "name", Type: Type{Name: "string"}}}
	group := []VariantGroup{{
		Name:       "assembling-machine",
		Parameters: []Parameter{{Name: "recipe", Type: Type{Name: "string"}}},
	}}

	withGroups := func(a *API, gs []VariantGroup) *API {
		mod := *a
		mod.Classes = append([]Class(nil), a.Classes...)
		mod.Classes = append(mod.Classes, Class{
			Name: "LuaVariantProbe",
			Methods: []Method{{
				Name:          "build",
				Parameters:    shared,
				VariantGroups: gs,
				Format:        Format{TakesTable: true},
			}},
		})
		return &mod
	}

	flat, tagged := withGroups(base, nil), withGroups(base, group)
	const id = "LuaVariantProbe::build"

	t.Run("gaining the first group", func(t *testing.T) {
		d := DiffAPI(flat, tagged)
		if !hasChange(d, Breaking, id, "variant parameter groups") {
			t.Fatalf("a method that gained its first variant group was not reported "+
				"as breaking; its arguments went from a laid-out block to one "+
				"dynamic value and every compiled call site now writes the wrong "+
				"shape.\n%s", d.Markdown())
		}
		if !changeMentions(d, id, "one dynamic value") {
			t.Errorf("the finding does not say what the arguments became:\n%s",
				d.Markdown())
		}
	})

	t.Run("losing the last group", func(t *testing.T) {
		d := DiffAPI(tagged, flat)
		if !hasChange(d, Breaking, id, "variant parameter groups") {
			t.Fatalf("a method that lost its last variant group was not reported as "+
				"breaking:\n%s", d.Markdown())
		}
		if !changeMentions(d, id, "a laid-out argument block") {
			t.Errorf("the finding does not say what the arguments became:\n%s",
				d.Markdown())
		}
	})

	// A GROUP ADDED BESIDE AN EXISTING ONE IS NOT REPORTED, and that is
	// proportionality rather than an omission. The shape did not flip, so a
	// variant group's parameters are keys inside a tier-2 value the guest writes
	// BY NAME -- nothing about the wire depends on them and the engine is what
	// validates them. Reporting them costs twenty findings per committed pair
	// against a diff whose whole value is that a human reads the breaking list.
	t.Run("a second group beside the first is not a wire change", func(t *testing.T) {
		two := append(append([]VariantGroup(nil), group...),
			VariantGroup{Name: "furnace"})
		d := DiffAPI(tagged, withGroups(base, two))
		if hasKindFor(d, Breaking, id) {
			t.Errorf("adding a group beside an existing one did not flip the "+
				"binding shape and must not be breaking:\n%s", d.Markdown())
		}
	})

	// The control, and it is the half that stops the detector from being one that
	// fires on everything: identical groups are no change at all.
	t.Run("unchanged", func(t *testing.T) {
		if d := DiffAPI(tagged, withGroups(base, group)); hasKindFor(d, Breaking, id) ||
			hasKindFor(d, Additive, id) {
			t.Errorf("an unchanged method was reported:\n%s", d.Markdown())
		}
	})
}

// THE SAME FLIP ON A TABLE CONCEPT, where `typeSig` cannot see it.
//
// `mapType` makes the identical decision for a concept: any variant group and
// the whole table is a bare tier-2 value, none and it is a laid-out struct. The
// concept walk's first line is an early return when the two signatures match --
// and a signature does not carry variant groups, so a concept that gained one
// with its ordinary fields untouched reported NOTHING, which is the strongest
// form of this blind spot.
func TestTheDiffNoticesAVariantGroupOnAConceptToo(t *testing.T) {
	base, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	withGroups := func(gs []VariantGroup) *API {
		mod := *base
		mod.Concepts = append(append([]Concept(nil), base.Concepts...), Concept{
			Name: "VariantProbeSpec",
			Type: Type{
				Complex:       "table",
				Parameters:    []Parameter{{Name: "name", Type: Type{Name: "string"}}},
				VariantGroups: gs,
			},
		})
		return &mod
	}
	flat := withGroups(nil)
	tagged := withGroups([]VariantGroup{{Name: "chest"}})

	d := DiffAPI(flat, tagged)
	if !hasChange(d, Breaking, "VariantProbeSpec", "variant parameter groups") {
		t.Fatalf("a table concept that gained its first variant group was not "+
			"reported; its fields are identical, so the signature comparison sees "+
			"nothing and the binding changed completely.\n%s", d.Markdown())
	}
	if d := DiffAPI(tagged, flat); !hasChange(d, Breaking, "VariantProbeSpec",
		"variant parameter groups") {
		t.Errorf("losing the last group on a concept was not reported:\n%s",
			d.Markdown())
	}
}

// AND ONE COMMITTED PAIR REALLY DOES FLIP, WHICH THE AMENDMENT SAID DID NOT
// HAPPEN.
//
// The queue entry that asked for this detector reads "Zero instances existed in
// any shipped pair", and the detector's first run found the counter-example:
// LuaSimulation::get_widget_position is (data, data2, type) with NO variant
// groups at 2.1.14 and (type) with SIXTEEN at 2.1.16, so its whole argument
// encoding flipped between two descriptions this repo committed. The old walk was
// not silent about it -- it reported two parameters removed, which is Breaking --
// but it named the wrong cause, and had the shared parameters happened not to
// move it would have reported nothing at all.
//
// So the instance is pinned BY NAME rather than asserted away. An expectation of
// zero would have been a gate that could only ever go red on a real Factorio
// release, and it would have been wrong on the day it was written.
func TestTheOneCommittedFlipIsPinnedByName(t *testing.T) {
	vers := committedVersions(t)
	if len(vers) < 2 {
		t.Skip("need two committed descriptions")
	}
	want := map[string]bool{
		"2.1.14 -> 2.1.16: LuaSimulation::get_widget_position": true,
	}
	got := map[string]bool{}
	for i := 0; i+1 < len(vers); i++ {
		from, err := LoadAPI(filepath.Join("..", "..", "api", vers[i],
			"runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		to, err := LoadAPI(filepath.Join("..", "..", "api", vers[i+1],
			"runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range DiffAPI(from, to).Changes {
			if strings.Contains(c.Detail, "variant parameter groups") {
				got[vers[i]+" -> "+vers[i+1]+": "+c.What] = true
			}
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s no longer reports a variant-group flip; that pair is the "+
				"only real instance this detector has and losing it makes the "+
				"synthetic test the only evidence it works", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s flips its variant groups and is not in the pinned list: a "+
				"guest compiled against the older pin needs its bindings "+
				"REGENERATED rather than repinned, so this is worth reading before "+
				"it is added here", k)
		}
	}
}

// changeMentions and hasKindFor are hasChange's two neighbours: one asks about
// the sentence a finding carries and the other about a finding existing at all
// for a subject, which is what a "must not fire" control needs.
func changeMentions(d APIDiff, what, detail string) bool {
	for _, c := range d.Changes {
		if c.What == what && strings.Contains(c.Detail, detail) {
			return true
		}
	}
	return false
}

func hasKindFor(d APIDiff, kind ChangeKind, what string) bool {
	for _, c := range d.Changes {
		if c.Kind == kind && c.What == what {
			return true
		}
	}
	return false
}
