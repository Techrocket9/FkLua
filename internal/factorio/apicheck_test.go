package factorio

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The guest surface, at the level the cmd tests cannot reach.
//
// `cmd/fklua/apicheck_test.go` drives the whole command against the two
// committed descriptions, which is the right gate for the status and the JSON
// document. What it cannot do is exercise a change the committed descriptions
// do not contain -- and one arm of the surface has no live instance between any
// pair of them, so it needs a fixture built here.

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// AN EVENT'S PAYLOAD PUTS ITS NAMED TYPES ON THE SURFACE, exactly as a member's
// signature does.
//
// The member loop collected concepts from args and rets; the event loop
// collected the event NAME and stopped. So a guest subscribing to an event
// whose payload names a concept that gained a field was never warned -- and the
// generated reader's offsets were computed from that concept's layout, so the
// fields after it arrive wrong while the diff says nothing at all against the
// event's own name.
//
// SYNTHETIC, AND THAT IS FORCED RATHER THAN CHOSEN: no concept named by any
// event payload moved between 2.0.77 and any committed 2.1.x, so no real pair
// can produce the finding. The test below is the half that does use real data,
// and it is what says this fixture models something.
func TestAnEventsPayloadPutsItsNamedTypesOnTheSurface(t *testing.T) {
	evs := EventReport{Events: []EventDef{
		{ID: 1, Name: "on_subscribed", Fields: []FieldSpec{
			{Name: "tick", Kind: KindU32},
			// Nested on purpose: an array of a named struct is the shape
			// on_player_built_tile really has, and it is the recursion in
			// collectTypeNames that has to reach it.
			{Name: "tiles", Kind: KindArray, Elem: &FieldSpec{
				Kind: KindStruct, TypeName: "OldTileAndPositionLike",
			}},
		}},
		{ID: 2, Name: "on_not_subscribed", Fields: []FieldSpec{
			{Name: "thing", Kind: KindStruct, TypeName: "UnsubscribedConcept"},
		}},
	}}

	s := SurfaceOf(Report{}, map[int]bool{}, true,
		map[int]bool{1: true}, true, evs,
		map[int]bool{}, true, DefineReport{})

	if !has(s.Events, "on_subscribed") {
		t.Fatalf("the subscribed event is not on the surface: %v", s.Events)
	}
	if !has(s.Concepts, "OldTileAndPositionLike") {
		t.Errorf("the payload's named type is not on the surface: %v; a guest "+
			"reading that event was compiled against that concept's layout",
			s.Concepts)
	}
	// THE CONTROL, and it is the one an implementation that collected every
	// event's payload would fail: a concept named only by an event this guest
	// does NOT subscribe to is not this guest's.
	if has(s.Concepts, "UnsubscribedConcept") {
		t.Errorf("a concept named only by an unsubscribed event reached the "+
			"surface: %v", s.Concepts)
	}

	// And it has to arrive as a FINDING, with the reason a reader needs beside
	// it. `concept` rather than `event`: the event did not change.
	d := APIDiff{From: "a", To: "b", Changes: []Change{
		{Kind: Breaking, What: "OldTileAndPositionLike",
			Detail: `field "quality" added, which moves the fields after it`},
		{Kind: Breaking, What: "UnsubscribedConcept", Detail: "concept removed"},
	}}
	res := CheckGuest(s, d)
	if len(res.Hits) != 1 {
		t.Fatalf("expected exactly one finding, got %d: %+v", len(res.Hits), res.Hits)
	}
	if res.Hits[0].What != "OldTileAndPositionLike" {
		t.Errorf("the finding names %q", res.Hits[0].What)
	}
	if res.Hits[0].Match != MatchConcept {
		t.Errorf("the finding matched as %q, want %q: the event itself did not "+
			"move, a type in its payload did", res.Hits[0].Match, MatchConcept)
	}
	if res.Ignored != 1 {
		t.Errorf("ignored %d, want 1: the unsubscribed event's concept is not "+
			"this guest's problem", res.Ignored)
	}
	if v := res.Verdict(); v != VerdictImpacted {
		t.Errorf("verdict is %q, want %q", v, VerdictImpacted)
	}
}

// ...AND THE EVENT LOOP IS NOT REDUNDANT WITH THE MEMBER LOOP, which is the
// half that runs on real data and the reason the fixture above is worth having.
//
// Some concepts are reachable ONLY through an event payload: no member takes
// one or returns one, at any pin, so the member loop can never put them on a
// surface however much of the API a guest calls. A guest subscribing to one of
// those events and nothing else depends on that concept's layout and the check
// could not name it.
//
// Derived from the committed description rather than written down, for the
// reason every id in this package is: a name baked in here is one that quietly
// stops discriminating the next time the description is regenerated.
func TestSomeConceptsAreReachableOnlyThroughAnEventPayload(t *testing.T) {
	a, err := LoadAPI(filepath.Join("..", "..", "api", DefaultAPIVersion,
		"runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := GenerateMembers(a)
	evs := GenerateEvents(a)

	fromMembers := map[string]bool{}
	for _, m := range report.Members {
		for _, f := range m.Args {
			collectTypeNames(f, fromMembers)
		}
		for _, f := range m.Rets {
			collectTypeNames(f, fromMembers)
		}
	}

	// The event carrying a concept no member signature reaches, and the concept
	// itself. Sorted so the pick is the description's and not the map's.
	var evIDs []int
	byID := map[int]EventDef{}
	for _, e := range evs.Events {
		evIDs = append(evIDs, e.ID)
		byID[e.ID] = e
	}
	sort.Ints(evIDs)
	pickID, pickName, want := 0, "", ""
	for _, id := range evIDs {
		e := byID[id]
		named := map[string]bool{}
		for _, f := range e.Fields {
			collectTypeNames(f, named)
		}
		var only []string
		for n := range named {
			if !fromMembers[n] {
				only = append(only, n)
			}
		}
		if len(only) > 0 {
			sort.Strings(only)
			pickID, pickName, want = e.ID, e.Name, only[0]
			break
		}
	}
	if pickID == 0 {
		t.Skipf("every concept an event payload names in api/%s is also reachable "+
			"from a member signature, so the event loop cannot be shown to add "+
			"anything against this description", DefaultAPIVersion)
	}

	s := SurfaceOf(report, map[int]bool{}, true,
		map[int]bool{pickID: true}, true, evs,
		map[int]bool{}, true, DefineReport{})
	if !has(s.Concepts, want) {
		t.Errorf("a guest subscribing to %s depends on %q and the surface does "+
			"not carry it: %v", pickName, want, s.Concepts)
	}
	t.Logf("%s names %q, which no member signature reaches", pickName, want)
}

// EVERY VALUE `from_source` MAY CARRY HAS A PHRASE, and the set is pinned here
// as four literals rather than read off FromSources.
//
// The failure this catches is a FIFTH rule. `from_source` is a closed set a
// script switches on and FromSourcePhrase is the same fact in the words the
// report prints, so a constant added to one and not the other ships a raw token
// (`(from: env)`) into the output a mod author reads -- which looks exactly like
// a phrase somebody wrote. Reading the set out of FromSources would not catch
// it, because a rule added to the constants and not to the slice is invisible to
// both; this list is the thing a fifth rule has to edit.
func TestEveryFromSourceHasAReportPhrase(t *testing.T) {
	want := []string{FromSourceFlag, FromSourceStamp, FromSourceManifest,
		FromSourceDefault}

	if len(FromSources) != len(want) {
		t.Errorf("FromSources carries %d values and the closed set is %d: %v",
			len(FromSources), len(want), FromSources)
	}
	for _, s := range want {
		if !ValidFromSource(s) {
			t.Errorf("%q is one of the four rules and ValidFromSource rejects it, "+
				"so VerdictDoc would refuse a document that resolved it", s)
		}
	}
	for _, s := range FromSources {
		if !has(want, s) {
			t.Errorf("FromSources carries %q, which is not in this test's list; a "+
				"fifth rule is an edit to both, or it prints as a raw token", s)
		}
	}

	seen := map[string]string{}
	for _, s := range want {
		p := FromSourcePhrase(s)
		if p == "" || p == s {
			t.Errorf("FromSourcePhrase(%q) is %q: a value with no phrase of its "+
				"own prints as the raw token, in the one output a mod author reads",
				s, p)
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("FromSourcePhrase(%q) and (%q) are both %q, so the report "+
				"cannot say which rule answered", s, prev, p)
		}
		seen[p] = s
	}

	// AND THE DEFAULT BRANCH IS LOUD. A value outside the set is a defect, and
	// the phrase for it has to read as one rather than as a fifth rule nobody
	// documented.
	const bogus = "env"
	if ValidFromSource(bogus) {
		t.Fatalf("%q is not one of the four rules", bogus)
	}
	if p := FromSourcePhrase(bogus); p == bogus {
		t.Errorf("FromSourcePhrase(%q) hands back the token unchanged, which "+
			"renders as `(from: %s)` and reads as a phrase somebody wrote", bogus, p)
	}
}

// A DOCUMENT WITH NO RESOLUTION RULE IN IT IS REFUSED, not defaulted.
//
// The command-line path always sets FromSource, so this is about the OTHER
// caller: a CheckResult assembled in Go, from CheckGuest and a surface, by a
// harness that never heard of the field. That result would have marshalled to
// `"from_source": ""` -- outside the closed set the document promises, and
// indistinguishable to a script from a value it can switch on. Defaulting the
// field instead would be worse: it would NAME a rule that did not fire, which
// is the one thing `from_source` exists to prevent.
func TestAVerdictDocumentRefusesAnUnresolvedFromSource(t *testing.T) {
	r := CheckResult{From: DefaultAPIVersion, To: DefaultAPIVersion, Guest: "g.wasm"}

	if _, err := r.VerdictDoc(); err == nil {
		t.Error("VerdictDoc built a document for a CheckResult whose FromSource " +
			"was never set, so `from_source` would ship as \"\"")
	}
	if b, err := r.JSON(); err == nil {
		t.Errorf("JSON marshalled the same result rather than refusing it:\n%s", b)
	}

	// THE CONTROL. A refusal that refuses everything would pass the two
	// assertions above and take the whole `--json` mode with it.
	r.FromSource = FromSourceStamp
	doc, err := r.VerdictDoc()
	if err != nil {
		t.Fatalf("a resolved FromSource was refused: %v", err)
	}
	if doc.FromSource != FromSourceStamp {
		t.Errorf("from_source is %q, want %q", doc.FromSource, FromSourceStamp)
	}
	if _, err := r.JSON(); err != nil {
		t.Fatalf("JSON refused a resolved result: %v", err)
	}
}

// THE REPORT SAYS SO TOO WHEN NO RULE RESOLVED `from`, rather than printing the
// heading this command had before `from_source` existed.
//
// VerdictDoc refuses that value and FromSourcePhrase's default branch is
// deliberately loud, so a Report() that dropped the phrase for an unset source
// would leave the two halves of one closed set disagreeing about the same value
// -- and the half that stayed quiet is the one a mod author READS. Quiet is the
// worse failure here: the old heading is a valid heading, so the reader has
// nothing to notice.
func TestAReportWithNoResolutionRuleSaysSoInItsHeader(t *testing.T) {
	r := CheckResult{From: DefaultAPIVersion, To: DefaultAPIVersion, Guest: "g.wasm"}

	head, _, _ := strings.Cut(r.Report(), "\n")
	// The "A -> B" shape is still at the front, as it is for a resolved one.
	if !strings.HasPrefix(head, "# api check: ") || !strings.Contains(head, " -> ") {
		t.Fatalf("the report's heading moved: %q", head)
	}
	if want := FromSourcePhrase(r.FromSource); !strings.Contains(head, want) {
		t.Errorf("the heading is %q for a CheckResult whose FromSource was never "+
			"set; it reads exactly like one an older fklua printed, while "+
			"VerdictDoc refuses the same value outright. Want %q in it", head, want)
	}

	// THE CONTROL, and the anti-vacuity tooth: a header that carried the loud
	// phrase whatever the source is would pass the assertion above and say
	// nothing. A resolved source prints ITS phrase and not the loud one.
	r.FromSource = FromSourceStamp
	head, _, _ = strings.Cut(r.Report(), "\n")
	if want := FromSourcePhrase(FromSourceStamp); !strings.Contains(head, want) {
		t.Errorf("the heading %q does not name %q for a resolved source", head, want)
	}
	if bad := FromSourcePhrase(""); strings.Contains(head, bad) {
		t.Errorf("the heading %q carries %q for a source that resolved", head, bad)
	}
}
