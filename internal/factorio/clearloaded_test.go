package factorio

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE package.loaded RESET A MODELLED LOAD PERFORMS, IN ONE PLACE.
//
// Factorio re-executes control.lua on every load, and control.lua requires the
// other three Lua files a packaged mod ships. Lua's `require` is memoised, so a
// harness that models a load in ONE interpreter -- which every save/load,
// rebuild, retain, defer and paced-collection test in this package does -- must
// clear all four entries or the second `require("control")` hands back the FIRST
// session's chunk. What that produces is not an error: it is a load in which the
// module's upvalues never went away, which is precisely the thing those tests
// exist to catch, passing.
//
// The list was hand-copied into eight harnesses across five files (and a ninth
// spelling as an ipairs loop), which is the shape this repo has already been
// bitten by twice -- two commands disagreeing about one manifest key, a mirror
// checked in one direction. Nothing enforced that the eight agreed and nothing
// would have said anything if they stopped, because a harness clearing three of
// four still runs and still prints plausible numbers.
//
// So: one marker, one expansion, and the names DERIVED from the constants
// mod.go's Files() writes rather than spelled a fifth time.
const clearLoadedMarker = "--@CLEAR_LOADED@"

// packagedLuaModules is every Lua module a packaged mod ships, by require name.
//
// Derived from the production constants, not transcribed: control.lua is the
// entry point Factorio itself loads and the other three are what mod.go's
// Files() puts beside it. TestTheClearListIsEveryLuaModuleAMoDShips holds the
// other direction -- that this really is all of them.
var packagedLuaModules = []string{
	"control",
	strings.TrimSuffix(GeneratedModuleFile, ".lua"),
	strings.TrimSuffix(ABIFile, ".lua"),
	strings.TrimSuffix(APIFile, ".lua"),
}

// A whole line, so the marker's own indentation is what the expansion inherits
// and a harness stays readable at whatever depth it sits.
var clearLoadedLine = regexp.MustCompile(
	`(?m)^([ \t]*)` + regexp.QuoteMeta(clearLoadedMarker) + `[ \t]*$`)

// expandClearLoaded replaces every clearLoadedMarker line in a Lua harness with
// the package.loaded reset a load performs.
//
// Applied to the script TEXT rather than at RunString, because the harnesses
// reach the interpreter by too many routes to wrap them all -- some are consts
// fed to fmt.Sprintf, some are functions that build a stub, some are inline
// literals concatenated with a report. Expanding where the text is defined is
// one call per definition and cannot be forgotten at a call site that did not
// exist when this was written.
//
// The expansion introduces no % verb, so it is safe on either side of a Sprintf.
func expandClearLoaded(script string) string {
	return clearLoadedLine.ReplaceAllStringFunc(script, func(m string) string {
		indent := m[:len(m)-len(strings.TrimLeft(m, " \t"))]
		lines := make([]string, len(packagedLuaModules))
		for i, mod := range packagedLuaModules {
			lines[i] = indent + `package.loaded["` + mod + `"] = nil`
		}
		return strings.Join(lines, "\n")
	})
}

// The derivation, checked against what a mod really contains.
//
// packagedLuaModules is built from three constants plus "control", which is one
// hop better than a fifth transcription and is still an assumption: that those
// four ARE every Lua file in the package. A fifth one added to Files() would
// leave every harness in this package modelling a load that silently keeps the
// previous session's copy of it -- the same class of miss as the four-way
// hand-copy, one level up. This is the direction that catches it.
func TestTheClearListIsEveryLuaModuleAMoDShips(t *testing.T) {
	pkg := &Package{
		Info:  Info{Name: "clearlist", Version: "0.0.1", Title: "t", Author: "a"},
		Chunk: "return function() end",
	}
	files, err := pkg.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var shipped []string
	for name := range files {
		if strings.HasSuffix(name, ".lua") {
			shipped = append(shipped, strings.TrimSuffix(name, ".lua"))
		}
	}
	sort.Strings(shipped)
	cleared := append([]string(nil), packagedLuaModules...)
	sort.Strings(cleared)
	if strings.Join(shipped, " ") != strings.Join(cleared, " ") {
		t.Errorf("a packaged mod ships Lua modules %v and the harnesses clear "+
			"%v.\nEvery test in this package that models a load in one "+
			"interpreter relies on that reset being COMPLETE: a module left in "+
			"package.loaded is the previous session's chunk, upvalues and all, "+
			"handed to a test written to prove those upvalues went away. Add the "+
			"new module to packagedLuaModules.", shipped, cleared)
	}
}

// The reason this file exists, asserted rather than hoped for: nothing else in
// this package writes the reset by hand.
//
// A text property or it is nothing -- the same footing as the loop-guard seed
// and the S1 binding. A ninth harness that copies the four lines back in would
// otherwise work perfectly, and would go stale on the day the list changes with
// no test able to notice, which is exactly the state this replaced.
func TestNothingClearsPackageLoadedByHand(t *testing.T) {
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range srcs {
		if name == "clearloaded_test.go" {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			// A comment ABOUT the reset is fine; a line that performs it is not,
			// which the subscript is what distinguishes.
			if !strings.Contains(line, "package.loaded[") {
				continue
			}
			t.Errorf("%s:%d writes the package.loaded reset by hand:\n\t%s\n"+
				"Use the %s marker instead -- the list of modules lives in "+
				"packagedLuaModules and a second copy is a second thing to "+
				"forget to update.", name, i+1, strings.TrimSpace(line),
				clearLoadedMarker)
		}
	}
}
