package factorio

import (
	"sort"
	"strings"
	"testing"
)

// A package with a control guest and nothing else, for the byte-identity gate.
func plainPackage() *Package {
	return &Package{
		Info: Info{
			Name: "p", Version: "0.1.0", Title: "P", Author: "A",
			FactorioVersion: DefaultFactorioVersion,
		},
		Chunk: "return { exports = {} }",
	}
}

// dataPackage is plainPackage plus a data module exporting the named hooks.
func dataPackage(exports ...string) *Package {
	p := plainPackage()
	p.DataChunk = "return { exports = {} }"
	p.DataExports = exports
	return p
}

func names(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// THE BACK-COMPAT GATE, and it is the difference between a feature and a
// breaking change.
//
// Every mod already built with fklua has a hand-written data stage carried by
// --include, under exactly the names this feature now generates. A packager that
// started emitting into a mod that never asked for a data module would break all
// of them at once, in game, at load.
//
// Red proof: emit a stage file unconditionally (drop the `p.DataChunk != ""`
// arm of stageChains) and this reports data.lua, data-final-fixes.lua,
// data-updates.lua and settings.lua as new entries.
func TestAProjectWithNoDataModuleIsByteIdentical(t *testing.T) {
	files, err := plainPackage().Files()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"control.lua", "fk_abi.lua", "fk_api_gen.lua", "fk_module.lua",
		"info.json"}
	got := names(files)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("a mod with no data module ships\n  %v\nand always shipped\n  %v",
			got, want)
	}
}

// ...and neither does a mod that declares no data module but has [stages] with
// pure-Lua chains, which is the far end of the migration ramp and also what a
// data-stage-only test mod wants. A chain with no @guest in it needs no module.
func TestAPureLuaStageChainNeedsNoDataModule(t *testing.T) {
	p := plainPackage()
	p.Stages = map[string][]string{"data": {"prototypes.entity", "prototypes.item"}}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[DataStageFile]; ok {
		t.Errorf("a pure-Lua chain should not ship %s", DataStageFile)
	}
	body := files["data.lua"]
	for _, want := range []string{`require("prototypes.entity")`, `require("prototypes.item")`} {
		if !strings.Contains(body, want) {
			t.Errorf("data.lua does not contain %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "fk_data") {
		t.Errorf("a chain with no %s should not call the guest:\n%s", GuestStageEntry, body)
	}
}

// A stage file is generated for an EXPORTED hook and for nothing else.
//
// This is the same feature-detection discipline control.lua applies to
// fk_on_tick and the collector triple, and it matters for a reason a reader can
// see in game: a mod with only a data stage must not get an empty settings.lua,
// because Factorio loads one and an empty one is a file that says the mod has a
// settings stage when it has none.
func TestOnlyExportedStageHooksGetAStageFile(t *testing.T) {
	files, err := dataPackage("fk_data").Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["data.lua"]; !ok {
		t.Error("a guest exporting fk_data got no data.lua")
	}
	for _, h := range StageHooks {
		if h.Export == "fk_data" {
			continue
		}
		if _, ok := files[h.File]; ok {
			t.Errorf("a guest exporting only fk_data got %s", h.File)
		}
	}
	// The shim and the module travel with any data module at all.
	for _, f := range []string{DataStageFile, DataModuleFile} {
		if _, ok := files[f]; !ok {
			t.Errorf("a mod with a data module does not ship %s", f)
		}
	}
}

// All four, when all four are exported.
func TestEveryExportedStageHookGetsItsFile(t *testing.T) {
	var all []string
	for _, h := range StageHooks {
		all = append(all, h.Export)
	}
	files, err := dataPackage(all...).Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range StageHooks {
		body, ok := files[h.File]
		if !ok {
			t.Errorf("no %s for %s", h.File, h.Export)
			continue
		}
		if !strings.Contains(body, "run("+itoa(h.Stage)+")") {
			t.Errorf("%s does not run stage %d:\n%s", h.File, h.Stage, body)
		}
	}
}

// The chain is an ORDERED sequence of requires, which is all data.lua has ever
// been -- so this is a TEXT assertion, because order is not something any value
// the packager returns can carry.
func TestAStageChainRequiresInTheDeclaredOrder(t *testing.T) {
	p := dataPackage("fk_data")
	p.Stages = map[string][]string{
		"data": {"prototypes.entity", GuestStageEntry, "prototypes.sprite"},
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	body := files["data.lua"]
	want := []string{`require("prototypes.entity")`, `require("fk_data").run(2)`,
		`require("prototypes.sprite")`}
	at := -1
	for _, w := range want {
		i := strings.Index(body, w)
		if i < 0 {
			t.Fatalf("data.lua does not contain %s:\n%s", w, body)
		}
		if i < at {
			t.Errorf("%s is out of order in:\n%s", w, body)
		}
		at = i
	}
}

// The mid-migration guardrail, and the message is the point rather than the
// refusal: a mod moving its data stage into Go lands in phases, and while it
// does, the included tree still carries data.lua under exactly the name this
// generates. "Rename it" is the wrong advice there.
func TestAStageEntryFileCollidesWithAnIncludedOne(t *testing.T) {
	p := dataPackage("fk_data")
	p.Extra = map[string]string{"data.lua": "-- the author's own"}
	p.extraFrom = map[string]string{"data.lua": "mod-data"}
	_, err := p.Files()
	if err == nil {
		t.Fatal("an included data.lua should collide with the generated one")
	}
	for _, w := range []string{"data.lua", "[stages]", GuestStageEntry} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the message does not name %q:\n%v", w, err)
		}
	}
}

// A chain naming the guest for a hook the module does not export is the one
// arrangement that is certainly a mistake: the author asked for something the
// guest does not have, and the alternative is a stage file that loads and does
// nothing.
func TestAChainNamingAnUnexportedGuestHookIsRefused(t *testing.T) {
	p := dataPackage("fk_data")
	p.Stages = map[string][]string{"data_updates": {GuestStageEntry}}
	_, err := p.Files()
	if err == nil {
		t.Fatal("a chain naming @guest for an unexported hook should be refused")
	}
	if !strings.Contains(err.Error(), "fk_data_updates") {
		t.Errorf("the message does not name the missing export:\n%v", err)
	}
}

// ...and one naming the guest with no data module at all.
func TestAChainNamingTheGuestWithNoDataModuleIsRefused(t *testing.T) {
	p := plainPackage()
	p.Stages = map[string][]string{"data": {GuestStageEntry}}
	_, err := p.Files()
	if err == nil {
		t.Fatal("a chain naming @guest with no data module should be refused")
	}
	if !strings.Contains(err.Error(), "data_module") {
		t.Errorf("the message does not say how to declare one:\n%v", err)
	}
}

// A key declared as an EMPTY list is not the same as an absent one: it means a
// stage file with no requires, which is a legitimate thing to want while a
// migration is in flight and is not the same as having no stage.
func TestADeclaredButEmptyChainStillGeneratesTheFile(t *testing.T) {
	p := dataPackage("fk_data")
	p.Stages = map[string][]string{"settings": {}}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	body, ok := files["settings.lua"]
	if !ok {
		t.Fatal("a declared but empty chain generated no settings.lua")
	}
	if strings.Contains(body, "require(") {
		t.Errorf("an empty chain should require nothing:\n%s", body)
	}
}

// THE DATA GUEST MUST NOT IMPORT fkapi, and the check has two properties
// because they catch the same mistake at different distances.
//
// The import set is the direct one, and it is also the honest report of a
// harder failure underneath: fk_data.lua binds `fkdata` and `env` and nothing
// else, so any other import is UNBOUND at instantiation. The pin-stamp export is
// the one that survives dead-code elimination -- it is a //go:wasmexport, so it
// is a root by definition, and a guest that imports fkapi carries it whether or
// not it ever calls a member.
func TestTheDataGuestDoesNotImportFkapi(t *testing.T) {
	ok := []string{"fkdata.get", "fkdata.set", "env.fk_log", "env.fk_print"}
	if err := CheckDataModule(ok, []string{"fk_data", "fk_alloc"}); err != nil {
		t.Errorf("a legitimate data module was refused: %v", err)
	}

	err := CheckDataModule(append(ok, "fk.call"), []string{"fk_data"})
	if err == nil {
		t.Fatal("a data module importing fk.call should be refused")
	}
	if !strings.Contains(err.Error(), "fkdata") || !strings.Contains(err.Error(), "fk`") {
		t.Errorf("the message should name the module and point at fkdata:\n%v", err)
	}

	err = CheckDataModule(ok, []string{"fk_data", "fk_api_pin_2_0_77"})
	if err == nil {
		t.Fatal("a data module exporting an API pin stamp should be refused")
	}
	if !strings.Contains(err.Error(), "fk_api_pin_2_0_77") ||
		!strings.Contains(err.Error(), "fkapi") {
		t.Errorf("the message should name the stamp and fkapi:\n%v", err)
	}

	// The wasip1 shim is the other thing a guest can pick up by accident, and it
	// has to be refused for the same reason: nothing binds it here.
	if err := CheckDataModule(append(ok, "wasi_snapshot_preview1.fd_write"), nil); err == nil {
		t.Error("a data module importing the WASI shim should be refused")
	}
}

// StageExportNames is the reachability root set for a data module's
// diagnostics, and _initialize belongs in it: fk_data.lua calls it before the
// stage hook, and a TinyGo guest's package initialisers are the whole of its
// setup. Without it, a diagnostic in a guest's init would be attributed to
// nothing.
func TestTheStageRootSetCoversInitialiseAndEveryHook(t *testing.T) {
	have := map[string]bool{}
	for _, n := range StageExportNames() {
		have[n] = true
	}
	if !have["_initialize"] {
		t.Error("_initialize is not in the data module's root set")
	}
	for _, h := range StageHooks {
		if !have[h.Export] {
			t.Errorf("%s is not in the data module's root set", h.Export)
		}
	}
	if len(StageExportNames()) != len(StageHooks)+1 {
		t.Errorf("the root set has %d entries and there are %d hooks plus _initialize",
			len(StageExportNames()), len(StageHooks))
	}
}

// The manifest half: [stages] parses, the four keys are the four stages, and a
// typo is refused rather than silently doing nothing.
func TestTheStagesSectionParses(t *testing.T) {
	p, err := ParseProject(`
[mod]
name = "m"
version = "0.1.0"

[fklua]
api = "2.0.77"
data_module = "dist/data.wasm"

[stages]
data = ["prototypes.entity", "@guest", "prototypes.sprite"]
data_final_fixes = ["@guest"]
settings = []
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.DataModule != "dist/data.wasm" {
		t.Errorf("data_module = %q", p.DataModule)
	}
	if got := strings.Join(p.Stages["data"], "|"); got != "prototypes.entity|@guest|prototypes.sprite" {
		t.Errorf("[stages] data = %q", got)
	}
	if chain, ok := p.Stages["settings"]; !ok || len(chain) != 0 {
		t.Errorf("an empty list must be DECLARED-and-empty, not absent: %v %v", chain, ok)
	}
	if _, ok := p.Stages["data_updates"]; ok {
		t.Error("an undeclared key must be absent from the map")
	}
}

func TestAnUnknownStagesKeyIsRefused(t *testing.T) {
	_, err := ParseProject(`
[mod]
name = "m"
version = "0.1.0"

[fklua]
api = "2.0.77"

[stages]
data_upates = ["@guest"]
`)
	if err == nil {
		t.Fatal("a misspelled stage key should be refused, not silently ignored")
	}
	for _, w := range StageKeys() {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the message does not list %q:\n%v", w, err)
		}
	}
}

// A manifest round-trips, which is what makes `fklua init` and a hand-edited
// file the same file.
func TestTheStagesSectionRoundTrips(t *testing.T) {
	src := Project{
		Name: "m", Version: "0.1.0", API: "2.0.77", Langs: []string{"go"},
		FactorioVersion: "2.0",
		DataModule:      "dist/data.wasm",
		Stages: map[string][]string{
			"data":             {"prototypes.entity", GuestStageEntry},
			"data_final_fixes": {GuestStageEntry},
		},
	}.TOML()
	back, err := ParseProject(src)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if back.DataModule != "dist/data.wasm" {
		t.Errorf("data_module did not survive: %q\n%s", back.DataModule, src)
	}
	if got := strings.Join(back.Stages["data"], "|"); got != "prototypes.entity|@guest" {
		t.Errorf("[stages] data did not survive: %q\n%s", got, src)
	}
	if got := strings.Join(back.Stages["data_final_fixes"], "|"); got != "@guest" {
		t.Errorf("[stages] data_final_fixes did not survive: %q\n%s", got, src)
	}
	// STAGE ORDER, NOT MAP ORDER: Go randomizes a map walk, and a manifest that
	// reordered itself on every write would make every regeneration a diff.
	if strings.Index(src, "data =") > strings.Index(src, "data_final_fixes =") {
		t.Errorf("the section is not in stage order:\n%s", src)
	}
}
