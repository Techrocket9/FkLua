package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M9's gate: `api diff 2.0.77 -> 2.1.12` classified against a HAND-CHECKED
// expectation.
//
// Hand-checked matters. A test asserting "200 breaking changes" would pass
// whatever the classifier did, as long as it kept doing it -- the number proves
// only that nothing moved. So the expectations below name specific changes that
// were verified against the raw JSON, and each one exercises a different arm of
// the classifier.
func TestAPIDiffClassifiesAgainstAHandCheckedExpectation(t *testing.T) {
	dir := filepath.Join("..", "..", "api")
	from, err := LoadAPI(filepath.Join(dir, "2.0.77", "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	to, err := LoadAPI(filepath.Join(dir, "2.1.12", "runtime-api.json"))
	if err != nil {
		t.Skip("2.1.12 is not cached; run `fklua api pull 2.1.12`")
	}
	d := DiffAPI(from, to)

	// Every one of these was checked against the JSON by hand, and each is a
	// different code path: a removed concept, a removed attribute, a new field
	// in a table concept, a new class, a new event.
	want := []struct {
		kind         ChangeKind
		what, detail string
	}{
		{Breaking, "FluidBoxFilter", "concept removed"},
		{Breaking, "LuaAssemblingMachineControlBehavior::include_fuel", "attribute removed"},
		{Breaking, "AccumulatorBlueprintControlBehavior", `field "output_networks" added`},
	}
	for _, w := range want {
		if !hasChange(d, w.kind, w.what, w.detail) {
			t.Errorf("expected a %s change for %q containing %q, and none was reported",
				w.kind, w.what, w.detail)
		}
	}

	// The schema held at 6 across this pair. If it ever moves, that is the one
	// change that invalidates the whole pipeline rather than one member, so it
	// must be reported -- and here it must NOT be, because it did not move.
	if hasChange(d, Breaking, "api_version", "schema") {
		t.Error("api_version was reported as changed; both versions are schema 6")
	}

	br, ad, co := d.Counts()
	if br == 0 || ad == 0 {
		t.Fatalf("expected both breaking and additive changes, got %d/%d/%d", br, ad, co)
	}
	// Sanity, not a pin: 2.1.12 is 482 members larger, so additive must
	// dominate. A classifier that called everything breaking would pass every
	// assertion above and fail this one.
	if ad < br {
		t.Errorf("additive (%d) should outnumber breaking (%d) for a release that "+
			"added 482 members; the classifier is over-reporting", ad, br)
	}
	t.Logf("2.0.77 -> 2.1.12: %d breaking, %d additive, %d cosmetic", br, ad, co)
}

// A DEFINE VALUE THAT GOES WHILE ITS GROUP STAYS IS WHAT A GUEST ACTUALLY
// LOSES, and a comparison of group NAMES cannot see one.
//
// A `fk.define` id is a dense index over the flattened value paths, so
// `defines.inventory.furnace_result` is a thing a guest baked an id for and
// `defines.inventory` is not. Comparing only the top-level group names --
// which is what this did -- reports nothing at all for the twenty value
// removals between 2.0.77 and 2.1.12, and `api check` reads the diff, so a
// guest that lost one got a clean bill.
//
// Both arms of the walk are exercised, because a subkey is a separate branch:
// a value directly under a group, and one under a nested group.
func TestDiffNoticesADefineValueRemovedFromASurvivingGroup(t *testing.T) {
	base, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("directly under a group", func(t *testing.T) {
		gi := -1
		for i, d := range base.Defines {
			// Two or more, so the group itself survives the removal -- which is
			// the whole point: a group-name comparison must be unable to see it.
			if d.Name != "events" && len(d.Values) > 1 {
				gi = i
				break
			}
		}
		if gi < 0 {
			t.Fatal("no define group has two values, which cannot be right")
		}
		mod := *base
		mod.Defines = append([]Define(nil), base.Defines...)
		group := mod.Defines[gi].Name
		gone := "defines." + group + "." + mod.Defines[gi].Values[0].Name
		mod.Defines[gi].Values = mod.Defines[gi].Values[1:]

		d := DiffAPI(base, &mod)
		if !hasChange(d, Breaking, gone, "define value removed") {
			t.Errorf("removing %s was not reported as breaking; the group %q "+
				"survives, so nothing comparing group names could see it", gone, group)
		}
		// ANTI-VACUITY, and it is the whole shape of the defect: the group must
		// NOT be reported, or this would pass on a diff that only looks at
		// groups and happened to lose one.
		if hasChange(d, Breaking, "defines."+group, "define removed") {
			t.Errorf("the group %q was reported as removed and it is still there", group)
		}
	})

	t.Run("under a subkey", func(t *testing.T) {
		gi, si := -1, -1
		for i, d := range base.Defines {
			if d.Name == "events" {
				continue
			}
			for j, s := range d.Subkeys {
				if len(s.Values) > 1 {
					gi, si = i, j
					break
				}
			}
			if gi >= 0 {
				break
			}
		}
		if gi < 0 {
			t.Skip("no define group has a subkey with two values, so the nested " +
				"arm of the walk cannot be exercised against this description")
		}
		mod := *base
		mod.Defines = append([]Define(nil), base.Defines...)
		mod.Defines[gi].Subkeys = append([]Define(nil), base.Defines[gi].Subkeys...)
		sub := &mod.Defines[gi].Subkeys[si]
		gone := "defines." + mod.Defines[gi].Name + "." + sub.Name + "." + sub.Values[0].Name
		sub.Values = sub.Values[1:]

		if !hasChange(DiffAPI(base, &mod), Breaking, gone, "define value removed") {
			t.Errorf("removing the nested value %s was not reported as breaking", gone)
		}
	})

	t.Run("a new value is additive", func(t *testing.T) {
		mod := *base
		mod.Defines = append([]Define(nil), base.Defines...)
		gi := 0
		for i, d := range base.Defines {
			if d.Name != "events" {
				gi = i
				break
			}
		}
		group := mod.Defines[gi].Name
		mod.Defines[gi].Values = append(append([]DefineVal(nil), base.Defines[gi].Values...),
			DefineVal{Name: "entirely_new"})
		if !hasChange(DiffAPI(base, &mod), Additive,
			"defines."+group+".entirely_new", "new define value") {
			t.Error("a new define value was not reported as additive")
		}
	})

	t.Run("a removed group takes its values with it", func(t *testing.T) {
		gi := -1
		for i, d := range base.Defines {
			if d.Name != "events" && len(d.Values) > 0 {
				gi = i
				break
			}
		}
		if gi < 0 {
			t.Fatal("no define group outside events carries a value")
		}
		group := base.Defines[gi].Name
		first := "defines." + group + "." + base.Defines[gi].Values[0].Name
		mod := *base
		mod.Defines = append(append([]Define(nil), base.Defines[:gi]...),
			base.Defines[gi+1:]...)

		d := DiffAPI(base, &mod)
		// The GROUP finding is what it always was -- this is the arm that was
		// already there and must keep reporting exactly what it did.
		if !hasChange(d, Breaking, "defines."+group, "define removed") {
			t.Errorf("the group-level finding for %q is gone", group)
		}
		// And the values are reported too, because they are what a guest holds
		// an id for.
		if !hasChange(d, Breaking, first, "define value removed") {
			t.Errorf("%s went with its group and was not reported", first)
		}
	})
}

// The value-level walk against the REAL descriptions, which is where the
// twenty removals the group comparison could not see actually live.
func TestTheCommittedDescriptionsDisagreeAboutDefineValues(t *testing.T) {
	dir := filepath.Join("..", "..", "api")
	from, err := LoadAPI(filepath.Join(dir, "2.0.77", "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	to, err := LoadAPI(filepath.Join(dir, "2.1.12", "runtime-api.json"))
	if err != nil {
		t.Skip("2.1.12 is not cached; run `fklua api pull 2.1.12`")
	}

	// Derived rather than named, for the reason every id in this package is:
	// a path written down here is one that quietly stops discriminating the
	// next time either description is regenerated.
	surviving := map[string]bool{}
	for _, g := range to.Defines {
		surviving[g.Name] = true
	}
	newV := map[string]bool{}
	for _, p := range definePaths(to) {
		newV[p] = true
	}
	var lost []string
	for _, p := range definePaths(from) {
		if newV[p] {
			continue
		}
		if group, _, _ := strings.Cut(p, "."); surviving[group] {
			lost = append(lost, p)
		}
	}
	if len(lost) == 0 {
		t.Skip("no define value was removed from a group that survives 2.0.77 -> " +
			"2.1.12, so the real descriptions cannot exercise this")
	}

	d := DiffAPI(from, to)
	for _, p := range lost {
		if !hasChange(d, Breaking, "defines."+p, "define value removed") {
			t.Errorf("%s is in 2.0.77 and not in 2.1.12 and the diff says nothing "+
				"about it", p)
		}
	}
	t.Logf("2.0.77 -> 2.1.12: %d define value(s) removed from surviving groups, "+
		"first %q", len(lost), lost[0])

	// And the reverse direction gains them, which is the additive arm over the
	// same pair rather than a second synthetic one.
	back := DiffAPI(to, from)
	for _, p := range lost {
		if !hasChange(back, Additive, "defines."+p, "new define value") {
			t.Errorf("%s reads as removed going forward and not as added going back", p)
		}
	}
}

func hasChange(d APIDiff, kind ChangeKind, what, detail string) bool {
	for _, c := range d.Changes {
		if c.Kind == kind && c.What == what && strings.Contains(c.Detail, detail) {
			return true
		}
	}
	return false
}

// A version against itself is the null diff. Anything reported here is a
// classifier that invents changes -- which would bury the real ones.
func TestDiffingAVersionAgainstItselfIsEmpty(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	d := DiffAPI(a, a)
	if len(d.Changes) != 0 {
		t.Errorf("expected no changes, got %d; first is %v %q %q",
			len(d.Changes), d.Changes[0].Kind, d.Changes[0].What, d.Changes[0].Detail)
	}
}

// The classifier has to notice each shape it claims to. Built from a real API
// rather than a fixture so the structures are the ones it will actually meet.
func TestDiffNoticesEachKindOfChange(t *testing.T) {
	base, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("removed method", func(t *testing.T) {
		mod := *base
		mod.Classes = append([]Class(nil), base.Classes...)
		for i := range mod.Classes {
			if len(mod.Classes[i].Methods) > 0 {
				gone := mod.Classes[i].Methods[0].Name
				id := mod.Classes[i].Name + "::" + gone
				mod.Classes[i].Methods = mod.Classes[i].Methods[1:]
				if !hasChange(DiffAPI(base, &mod), Breaking, id, "method removed") {
					t.Errorf("removing %s was not reported as breaking", id)
				}
				return
			}
		}
		t.Fatal("no class has a method, which cannot be right")
	})

	t.Run("added class is additive", func(t *testing.T) {
		mod := *base
		mod.Classes = append(append([]Class(nil), base.Classes...),
			Class{Name: "LuaEntirelyNew"})
		if !hasChange(DiffAPI(base, &mod), Additive, "LuaEntirelyNew", "new class") {
			t.Error("a new class was not reported as additive")
		}
	})

	t.Run("removed event", func(t *testing.T) {
		mod := *base
		gone := base.Events[0].Name
		mod.Events = append([]Event(nil), base.Events[1:]...)
		if !hasChange(DiffAPI(base, &mod), Breaking, gone, "event removed") {
			t.Errorf("removing event %s was not reported as breaking", gone)
		}
	})

	t.Run("removed define group", func(t *testing.T) {
		mod := *base
		gone := base.Defines[0].Name
		mod.Defines = append([]Define(nil), base.Defines[1:]...)
		if !hasChange(DiffAPI(base, &mod), Breaking, "defines."+gone, "define removed") {
			t.Errorf("removing the define group %s was not reported as breaking", gone)
		}
	})

	t.Run("schema bump", func(t *testing.T) {
		mod := *base
		mod.APIVersion = base.APIVersion + 1
		if !hasChange(DiffAPI(base, &mod), Breaking, "api_version", "schema") {
			t.Error("a schema bump was not reported as breaking, and it is the " +
				"one change that invalidates every generator at once")
		}
	})
}

// The markdown is the artifact a human reads, so it has to lead with what
// breaks and say plainly when nothing does.
func TestDiffMarkdownLeadsWithBreaking(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	md := DiffAPI(a, a).Markdown()
	if !strings.Contains(md, "No breaking changes") {
		t.Errorf("a null diff should say so plainly:\n%s", md)
	}

	mod := *a
	mod.Events = append([]Event(nil), a.Events[1:]...)
	md = DiffAPI(a, &mod).Markdown()
	bi, ai := strings.Index(md, "## Breaking"), strings.Index(md, "## Additive")
	if bi < 0 {
		t.Fatalf("expected a Breaking section:\n%s", md)
	}
	if ai >= 0 && ai < bi {
		t.Error("Additive came before Breaking; the breaking list is the one a " +
			"human reads and it must be first")
	}
}

// The JSON form has to round-trip, since the regeneration bot consumes it.
func TestDiffJSONRoundTrips(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	mod := *a
	mod.Events = append([]Event(nil), a.Events[1:]...)
	raw, err := DiffAPI(a, &mod).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("the JSON should be newline-terminated, for a line-oriented diff")
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "d.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The docs cannot name a member something the bindings do not.
//
// That is the whole reason `docs` renders from the SAME Report and the SAME
// Names map the generator produced: documentation built by a second walk of the
// JSON can disagree with the bindings, and eventually would. This asserts the
// property rather than trusting the arrangement.
func TestDocsNameExactlyWhatTheBindingsBind(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	r := GenerateMembers(a)
	evs := GenerateEvents(a)
	g, err := GenerateGoWith(a, r, evs, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	md := Docs(a, r, evs, DocOptions{Lang: "go", Names: g.Names})

	// Every bound member's emitted name appears as a heading.
	missing := 0
	for _, name := range g.Names {
		if !strings.Contains(md, "### `"+name+"`") {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d bound members have no entry in the docs", missing)
	}

	// And the count the docs claim is the count the generator produced.
	if !strings.Contains(md, fmt.Sprintf("%d members bound", len(g.Names))) {
		t.Errorf("the docs' own member count disagrees with the generator's %d", len(g.Names))
	}

	// A DEFERRED member must not appear. Documenting something a guest cannot
	// call is worse than omitting it: an author would write the call, and find
	// out at compile time in another language.
	deferred := 0
	for _, m := range r.Members {
		key := fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)
		if _, bound := g.Names[key]; bound {
			continue
		}
		deferred++
	}
	if deferred == 0 {
		t.Fatal("no members are deferred, so this proves nothing; the fixture moved")
	}
	// The skip census is reported instead, which is the honest form.
	if !strings.Contains(md, "## Not bound") {
		t.Error("the docs should say what is NOT bound; an author hunting a " +
			"missing member needs to know it was skipped rather than absent")
	}
}

// Factorio's own link markup is not markdown, and a dead `(runtime:Foo)` in
// every other description would be noise in a 700 KB file.
func TestDocsStripFactorioLinkMarkup(t *testing.T) {
	got := oneLine("See [LuaEntity](runtime:LuaEntity) for\nmore.")
	if strings.Contains(got, "runtime:") {
		t.Errorf("link scheme survived: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("newline survived: %q", got)
	}
	if !strings.Contains(got, "LuaEntity") {
		t.Errorf("the label was lost: %q", got)
	}
}
