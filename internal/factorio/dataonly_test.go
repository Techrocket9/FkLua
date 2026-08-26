package factorio

import (
	"sort"
	"strings"
	"testing"

	luart "github.com/Techrocket9/fklua/runtime"
)

// A DATA-STAGE-ONLY MOD, which is a package with a data chunk and no control
// chunk. Factorio insists on info.json and nothing else, so a mod that is
// nothing but prototypes is an ordinary genre rather than a degenerate case --
// and reaching it used to mean compiling an empty control guest and shipping it
// to be required at every load and called from nowhere.
func dataOnlyPackage(exports ...string) *Package {
	p := dataPackage(exports...)
	p.Chunk = ""
	return p
}

// THE FILE LIST, written out rather than derived, because that is the whole
// claim: three files leave and nothing else moves.
func TestADataOnlyPackageShipsNoControlStage(t *testing.T) {
	files, err := dataOnlyPackage("fk_data").Files()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"data.lua", "fk_abi.lua", "fk_data.lua", "fk_data_module.lua",
		"info.json"}
	got := names(files)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("a data-stage-only mod ships\n  %v\nand should ship\n  %v", got, want)
	}
	// Named individually as well as by the list, because these three are the
	// point and a list assertion says only that something changed.
	for _, unwanted := range []string{"control.lua", GeneratedModuleFile, APIFile} {
		if _, ok := files[unwanted]; ok {
			t.Errorf("a mod with no control module ships %s", unwanted)
		}
	}
}

// fk_abi.lua IS NOT A CONTROL-STAGE FILE, and the obvious reading of "drop the
// control stage's files" drops it -- which produces a mod that will not load,
// with a message about a Lua module rather than about anything fklua did.
//
// Asserted against the SHIM rather than against a memorised list: fk_data.lua
// requires fk_abi.lua for the tier-2 codec, so the day it stops doing so this
// test says so instead of quietly pinning a file nobody needs.
func TestTheDataStageShimRequiresTheABI(t *testing.T) {
	// CHECKED IN BOTH DIRECTIONS, and the second one does not skip. A check that
	// skips is a check that passed: if the shim stops requiring the ABI, the
	// data-only file list has to be re-derived, and nothing else would say so.
	if !strings.Contains(luart.DataStage(), `require("fk_abi")`) {
		t.Fatalf("%s no longer requires %s; re-derive what a data-stage-only "+
			"package ships in Files()", DataStageFile, ABIFile)
	}
	files, err := dataOnlyPackage("fk_data").Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[ABIFile]; !ok {
		t.Errorf("%s requires %s and the package does not ship it: the mod will "+
			"not load", DataStageFile, ABIFile)
	}
}

// REMOVING THE CONTROL STAGE REMOVES EXACTLY THE CONTROL STAGE. Every file the
// two shapes have in common is byte-identical, which is a stronger statement
// than either file list on its own: a list says which names appear, and this
// says the bytes under the shared names did not move.
func TestADataOnlyPackageIsTheSameMinusTheControlStage(t *testing.T) {
	both, err := dataPackage("fk_settings", "fk_data").Files()
	if err != nil {
		t.Fatal(err)
	}
	only, err := dataOnlyPackage("fk_settings", "fk_data").Files()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range only {
		other, shared := both[name]
		if !shared {
			t.Errorf("%s appears only without a control module", name)
			continue
		}
		if other != body {
			t.Errorf("%s differs between the two shapes", name)
		}
	}
	var gone []string
	for name := range both {
		if _, ok := only[name]; !ok {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	want := []string{APIFile, GeneratedModuleFile, "control.lua"}
	sort.Strings(want)
	if strings.Join(gone, " ") != strings.Join(want, " ") {
		t.Errorf("dropping the control module dropped\n  %v\nand should drop\n  %v",
			gone, want)
	}
}

// The stage files are the data module's, so an included tree and [stages] reach
// a data-only mod exactly as they reach any other. This is the downstream shape:
// a stand-in mod that is prototypes and nothing else.
func TestADataOnlyPackageStillCarriesStagesAndIncludes(t *testing.T) {
	p := dataOnlyPackage("fk_data")
	p.Stages = map[string][]string{"data": {"prototypes.entity", GuestStageEntry}}
	p.Extra = map[string]string{"prototypes/entity.lua": "-- hand written\n"}
	p.extraFrom = map[string]string{"prototypes/entity.lua": "mod-data"}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	if files["prototypes/entity.lua"] != "-- hand written\n" {
		t.Errorf("the included tree did not reach a data-only mod")
	}
	body := files["data.lua"]
	for _, want := range []string{`require("prototypes.entity")`, `require("fk_data").run(2)`} {
		if !strings.Contains(body, want) {
			t.Errorf("data.lua does not contain %s:\n%s", want, body)
		}
	}
}

// THE COLLISION MESSAGE NAMES WHAT WAS ACTUALLY WRITTEN. It used to name a
// literal five files, which was true of every package there was and is false of
// two now -- and the reason to fix it rather than leave it is that the sentence
// exists to tell an author which name to stop using.
func TestTheCollisionMessageNamesTheFilesThisPackageWrites(t *testing.T) {
	p := dataOnlyPackage("fk_data")
	p.Extra = map[string]string{"info.json": "{}"}
	p.extraFrom = map[string]string{"info.json": "mod-data"}
	_, err := p.Files()
	if err == nil {
		t.Fatal("an included info.json should collide with the generated one")
	}
	for _, want := range []string{"info.json", DataStageFile, DataModuleFile, ABIFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %s:\n%v", want, err)
		}
	}
	for _, unwanted := range []string{"control.lua", GeneratedModuleFile, APIFile} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the message names %s, which this package does not write:\n%v",
				unwanted, err)
		}
	}
}
