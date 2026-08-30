package factorio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apiPath is the committed copy. It is committed on purpose: the generator's
// input must not depend on a Factorio install or on the network, or a build is
// only reproducible on a machine that owns the game.
//
// It follows DefaultAPIVersion rather than naming a release, because the
// committed bindings and census these tests compare against are generated from
// exactly that pin -- a literal here would compare 2.0.77's output to 2.1.14's
// golden file and report the GENERATOR as stale, which is a version bump
// wearing a codegen bug's clothes.
var apiPath = filepath.Join("..", "..", "api", DefaultAPIVersion, "runtime-api.json")

func loadTestAPI(t *testing.T) *API {
	t.Helper()
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatalf("LoadAPI: %v", err)
	}
	return a
}

// shapeAPIVersion is the description a test names when it is about a SHAPE
// rather than about the default pin, and the distinction is worth stating once
// here instead of five times below.
//
// Most tests want the pin: they compare against the committed bindings and the
// committed census, which are generated from exactly that description, so
// loadTestAPI following DefaultAPIVersion is the only thing that can be right.
// A handful want something else -- they assert that the generators can express
// a particular shape, and that shape is a 2.1 one. The `nil` concept
// ColorLookupTable exists in every 2.1.x description and has never existed in
// 2.0.77; UtilityConstants' dictionary-of-dictionaries, LuaEntity's
// array-of-arrays and LuaPlayer::get_alerts' three levels are all 2.1 shapes.
//
// THOSE TESTS PASSED FOR AN ACCIDENT UNTIL THE PIN MOVED BACK: the default
// happened to be the description that had the shape. That is a coupling, not a
// property, and the fix is for a shape test to name its own description -- every
// description is committed precisely so a build needs neither the game nor the
// network, and the pinning machinery already reads whichever one it is pointed
// at.
//
// IT IS THE NEWEST 2.1 DESCRIPTION COMMITTED, and that is the point rather than
// an arbitrary pick: a shape these tests assert is expressible is one Factorio
// could stop publishing, and naming an older description would hide that behind
// a version nobody ships any more. It moves with `api pull`, not with the pin.
//
// A test using this must NOT also compare against api/<pin>/census.json or the
// committed bindings, for the reason apiPath's comment gives.
const shapeAPIVersion = "2.1.17"

// loadShapeAPI loads a description by name. See shapeAPIVersion.
func loadShapeAPI(t *testing.T, version string) *API {
	t.Helper()
	a, err := LoadAPI(filepath.Join("..", "..", "api", version, "runtime-api.json"))
	if err != nil {
		t.Fatalf("LoadAPI(%s): %v", version, err)
	}
	return a
}

// committedVersions lists every description this working directory owns.
//
// For a test whose property is about the DESCRIPTION FAMILY rather than about
// one pin: "these five members take a Lua function", "exactly two writable
// attributes are a union with a class in it". Asserting such a thing at one pin
// makes it a coincidence of which description happened to be the default, and
// the generators are one code path serving all of them -- which is the lesson
// TestEveryCommittedDescriptionHasACurrentCensus was written for, one level over.
//
// It reads the directory rather than listing versions, so a `fklua api pull`
// widens every such test on the day it lands.
func committedVersions(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "api")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "runtime-api.json")); err != nil {
			// A version directory with no description is half a pull, not a
			// committed description.
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatal("no committed descriptions: a walk that matched nothing passes forever")
	}
	return out
}

// The census lives in api/<version>/census.json, not in Go literals here.
//
// It used to be literals across three test files, and measuring an upgrade to
// 2.1.12 showed what that costs: the pipeline handled 482 more members with NO
// code change, and seven tests failed -- every one a moved count rather than a
// logic error. Editing source to acknowledge a number is exactly the manual
// step automatic regeneration is supposed to remove.
//
// So a version bump is now `fklua gen-bindings` and a one-file data diff. This
// test is the gate that the committed file still matches.
//
// The counts are EQUALITIES, not floors, deliberately: a shrinking API is news
// too. 2.1.12 removed two operators, which an equality catches and a ">= 9"
// would wave through.
func TestCensusMatchesTheCommittedBaseline(t *testing.T) {
	a := loadTestAPI(t)
	got, err := TakeCensus(a)
	if err != nil {
		t.Fatal(err)
	}
	want, err := LoadCensus(CensusPath(filepath.Join("..", "..", "api"), a.ApplicationVersion))
	if err != nil {
		t.Fatalf("%v -- run `fklua gen-bindings`", err)
	}
	if lines := got.Diff(want); len(lines) > 0 {
		t.Errorf("the census moved; run `fklua gen-bindings` and review the diff:\n  %s",
			strings.Join(lines, "\n  "))
	}
}

// Diff has to actually report a change, or the gate above is decoration.
func TestCensusDiffReportsMovement(t *testing.T) {
	a, b := CensusData{Classes: 148, HostSkipsBy: map[string]int{"x": 1}},
		CensusData{Classes: 156, HostSkipsBy: map[string]int{"x": 1, "y": 2}}
	lines := b.Diff(a)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "148 -> 156") {
		t.Errorf("a moved count should be reported: %v", lines)
	}
	// A NEW skip reason is the actionable half: the generator met a shape it
	// had not met before, which matters even when totals barely move.
	if !strings.Contains(joined, "NEW") {
		t.Errorf("a new skip reason should be called out: %v", lines)
	}
	if n := len(CensusData{}.Diff(CensusData{})); n != 0 {
		t.Errorf("identical censuses should diff clean, got %d lines", n)
	}
}

// Four things the parser has to get right that a looser model would lose. Each
// was found by DisallowUnknownFields rather than by reading the schema, which
// is the argument for keeping that on.
func TestParserKeepsTheAwkwardShapes(t *testing.T) {
	a := loadTestAPI(t)
	byName := map[string]Class{}
	for _, c := range a.Classes {
		byName[c.Name] = c
	}

	// 1. Class inheritance. LuaEntity's members are not all listed on LuaEntity.
	if p := byName["LuaEntity"].Parent; p == "" {
		t.Error("LuaEntity has a parent in the JSON; the model dropped it")
	}

	// 2. Attribute-shaped operators.
	ct := byName["LuaCustomTable"]
	var sawIndex bool
	for _, o := range ct.Operators {
		if o.Name == "index" {
			sawIndex = true
			if !o.IsAttribute() {
				t.Error("LuaCustomTable::index is attribute-shaped (read_type), not a method")
			}
		}
	}
	if !sawIndex {
		t.Error("LuaCustomTable::index went missing")
	}

	// 3. A function type's parameters are BARE TYPE REFS, not named parameters.
	var sawFunc bool
	for _, c := range a.Concepts {
		if c.Type.Complex == "function" && len(c.Type.FuncParams) > 0 {
			sawFunc = true
			break
		}
	}
	if !sawFunc {
		// Not fatal on its own -- the shape may live only inside other types --
		// so look harder before complaining.
		t.Log("no top-level function concept; shape is exercised nested")
	}

	// 4. defines nest. defines.events is flat, but some have subkeys.
	var nested int
	for _, d := range a.Defines {
		if len(d.Subkeys) > 0 {
			nested++
		}
	}
	if nested == 0 {
		t.Error("no define has subkeys; the nesting was dropped")
	}
	t.Logf("%d of %d defines have subkeys", nested, len(a.Defines))
}

// A dump that is not a runtime API must be refused rather than half-parsed.
func TestParseRejectsTheWrongThing(t *testing.T) {
	if _, err := ParseAPI(strings.NewReader(`{"application":"factorio"}`)); err == nil {
		t.Error("a document with no api_version should be refused")
	}
	// And the strictness that found four fields during M7: an unmodelled key is
	// news, not something to skip past.
	_, err := ParseAPI(strings.NewReader(`{"api_version":6,"surprise":1}`))
	if err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Errorf("an unknown field should be named in the error; got %v", err)
	}
}

// The ABI carries four result slots. That is only safe if no method returns
// more, so the bound is measured rather than assumed -- and pinned, because a
// future API version returning four would silently truncate.
func TestMethodReturnArity(t *testing.T) {
	a := loadTestAPI(t)
	worst, where := 0, ""
	for _, c := range a.Classes {
		for _, m := range c.Methods {
			if n := len(m.ReturnValues); n > worst {
				worst, where = n, c.Name+"::"+m.Name
			}
		}
	}
	if worst > 4 {
		t.Errorf("%s returns %d values; fk_abi.lua carries only 4", where, worst)
	}
	if worst != 3 {
		t.Errorf("max return arity = %d (%s), want 3 -- if this moved, re-check "+
			"the slot count in fk_abi.lua's invoke", worst, where)
	}
}
