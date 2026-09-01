package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	luart "github.com/Techrocket9/fklua/runtime"
)

// A data module, as small as one can be: a memory (the codec needs one) and the
// stage hooks it is asked to export.
func dataGuest(t *testing.T, exports ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("(module (memory 1)\n")
	// The allocator the shim binds. It is enough for it to EXIST here: nothing
	// in these tests runs the module, they package it.
	b.WriteString(`  (func (export "fk_alloc") (param i32) (result i32) (i32.const 0))` + "\n")
	b.WriteString(`  (func (export "fk_free") (param i32))` + "\n")
	for _, e := range exports {
		b.WriteString(`  (func (export "` + e + `"))` + "\n")
	}
	b.WriteString(")")
	p := filepath.Join(t.TempDir(), "data.wat")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A data module that reaches the runtime API, which is what must be refused.
func fkapiDataGuest(t *testing.T) string {
	t.Helper()
	// The import comes first: wat wants every import before any other module
	// field, exactly as the binary format does.
	src := `(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_data") (drop (call $call (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0)))))`
	p := filepath.Join(t.TempDir(), "apidata.wat")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func modFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// --data-module end to end: the flag, the generated files, and the one that is
// NOT generated because the guest does not export its hook.
func TestModPackagesADataModule(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t),
		"--data-module", dataGuest(t, "fk_settings", "fk_data"),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", out}); err != nil {
		t.Fatal(err)
	}
	have := modFiles(t, filepath.Join(out, "a-mod_0.1.0"))
	for _, want := range []string{factorio.DataStageFile, factorio.DataModuleFile,
		"settings.lua", "data.lua"} {
		if !have[want] {
			t.Errorf("the packaged mod has no %s", want)
		}
	}
	for _, unwanted := range []string{"data-updates.lua", "data-final-fixes.lua"} {
		if have[unwanted] {
			t.Errorf("the guest exports no hook for %s and it was generated anyway",
				unwanted)
		}
	}
}

// --stage, which is the flag form of a manifest key.
//
// EVERY NEW MANIFEST KEY GETS A FLAG FORM, and the reason is not symmetry: one
// checkout that packages several mods drives them from one Makefile with one
// manifest describing the shipped one and flags describing the rest, which a
// key with no flag makes impossible.
func TestTheStageFlagOrdersTheChain(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t),
		"--data-module", dataGuest(t, "fk_data"),
		"--stage", "data=prototypes.entity,@guest,prototypes.sprite",
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", out}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(out, "a-mod_0.1.0", "data.lua"))
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for _, w := range []string{`require("prototypes.entity")`,
		`require("fk_data").run(2, "a-mod")`, `require("prototypes.sprite")`} {
		i := strings.Index(string(body), w)
		if i < 0 {
			t.Fatalf("data.lua does not contain %s:\n%s", w, body)
		}
		if i < at {
			t.Errorf("%s is out of order:\n%s", w, body)
		}
		at = i
	}
}

func TestAMisspelledStageFlagIsRefused(t *testing.T) {
	err := runMod([]string{tinyGuest(t),
		"--data-module", dataGuest(t, "fk_data"),
		"--stage", "data_upates=@guest",
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", t.TempDir()})
	if err == nil {
		t.Fatal("a misspelled stage key should be refused, not silently ignored")
	}
	for _, w := range factorio.StageKeys() {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the message does not list %q:\n%v", w, err)
		}
	}
	// ...and a spec with no `=` in it at all.
	if err := runMod([]string{tinyGuest(t), "--stage", "data",
		"--name", "a", "--version", "0.1.0", "--author", "s", "-o", t.TempDir()}); err == nil {
		t.Fatal("--stage with no KEY=... should be refused")
	}
}

// THE MANIFEST IS THE DEFAULT AND THE FLAG IS THE OVERRIDE, the rule every
// other key here follows -- and per KEY for [stages], so overriding one chain
// does not silently discard the other three.
func TestTheDataModuleAndStagesComeFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	guestPath := dataGuest(t, "fk_data", "fk_data_final_fixes")
	if err := os.WriteFile("fklua.toml", []byte(`
[mod]
name = "from-manifest"
version = "0.2.0"
author = "someone"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
data_module = "`+guestPath+`"

[stages]
data = ["prototypes.entity", "@guest"]
data_final_fixes = ["@guest"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := runMod([]string{tinyGuest(t), "-o", out}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(out, "from-manifest_0.2.0", "data.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `require("prototypes.entity")`) {
		t.Errorf("the manifest's chain did not reach data.lua:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(out, "from-manifest_0.2.0",
		"data-final-fixes.lua")); err != nil {
		t.Errorf("the manifest's data_final_fixes chain generated no file: %v", err)
	}

	// The flag overrides ONE key and the manifest still supplies the rest.
	out2 := filepath.Join(dir, "out2")
	if err := runMod([]string{tinyGuest(t), "--stage", "data=@guest", "-o", out2}); err != nil {
		t.Fatal(err)
	}
	body2, err := os.ReadFile(filepath.Join(out2, "from-manifest_0.2.0", "data.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body2), "prototypes.entity") {
		t.Errorf("--stage did not override the manifest's chain:\n%s", body2)
	}
	if _, err := os.Stat(filepath.Join(out2, "from-manifest_0.2.0",
		"data-final-fixes.lua")); err != nil {
		t.Errorf("--stage data=... discarded the manifest's other chains: %v", err)
	}
}

// A COLLECTOR-INFECTED DATA MODULE IS REFUSED AT PACKAGE TIME, and it used to
// package with exit 0 and no line saying so.
//
// A data module is compiled `--persist=none --gc=leaking` whatever the control
// guest uses, and nothing at any data-family stage drives a collector: there is
// no tick to pace one from and no state to survive. So the three exports that
// ARE the collector to the host say the build went wrong -- and getting it
// wrong takes no typo in Rust, where cargo's v2 resolver unifies
// `--features fk/fkgc` across every package built in one invocation. Measured
// on this repository's own example data guest built both ways: 599,178 bytes of
// generated Lua against 676,282, +12.9%, all of it parsed at every load for a
// collector nothing can pace behind an allocator the stage does not need. The
// figures come from `fklua compile --persist=none --gc=leaking` over each build
// and need no game; the collected arm cannot be measured through `fklua mod`
// any more, because this is what refuses it.
//
// The export list comes from factorio.CollectorSurface() rather than being
// spelled here, which is what stops this test and the check drifting apart --
// so the surface being empty is the vacuity hazard, asserted first.
func TestADataModuleCarryingACollectorIsRefusedAtPackageTime(t *testing.T) {
	surface := factorio.CollectorSurface()
	if len(surface) == 0 {
		t.Fatal("factorio.CollectorSurface() is empty, so this test would " +
			"package a module with nothing to refuse")
	}
	doc, _, err := runModReport(t, tinyGuest(t),
		"--data-module", dataGuest(t, append([]string{"fk_data"}, surface...)...))
	if err == nil {
		t.Fatal("a data module built with the collector was packaged happily")
	}
	// The kind is contract: a driving tool switches on it, and this refusal is
	// about the data module rather than about the gc mode of a control guest.
	if got := at(t, doc, "refusal", "kind"); got != "data_module" {
		t.Errorf("refusal.kind = %v, want \"data_module\"", got)
	}
	msg := at(t, doc, "refusal", "message").(string)
	// -gc=custom rather than -gc=leaking: the latter is a substring of the
	// `--gc=leaking` this message already carries in its "what a data module
	// is" sentence, so it would leave the Go remedy line deletable with this
	// test green. -gc=custom appears once, inside that line.
	for _, want := range append(append([]string{}, surface...),
		"--persist=none", "-gc=custom", "fk/fkgc") {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}

	// THE OTHER ARM, because a refusal that fires on everything is not a check:
	// the same module without those three exports packages.
	if err := runMod([]string{tinyGuest(t),
		"--data-module", dataGuest(t, "fk_data"),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", t.TempDir()}); err != nil {
		t.Fatalf("a collector-free data module was refused: %v", err)
	}
}

// A data module that reaches the runtime API is refused at PACKAGE time, which
// is the enforceable form of a rule that would otherwise be a comment.
//
// Two things go wrong at once without it, and only one of them is loud. The
// quiet one is an API pin stamp on a module nothing checks it against. The loud
// one is that fk_data.lua binds `fkdata` and `env` and nothing else, so an
// import of `fk` is UNBOUND at instantiation -- a mod that will not load, with a
// message about a wasm module name, at game start.
func TestADataModuleReachingTheRuntimeAPIIsRefused(t *testing.T) {
	err := runMod([]string{tinyGuest(t), "--data-module", fkapiDataGuest(t),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", t.TempDir()})
	if err == nil {
		t.Fatal("a data module importing fk.call should be refused")
	}
	for _, w := range []string{"fkdata", "runtime API"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the message does not contain %q:\n%v", w, err)
		}
	}
}

// A DATA-STAGE-ONLY MOD: the control positional left out entirely.
//
// This is the downstream shape it was asked for in -- identity entirely from
// flags, packaged from a directory with no fklua.toml in it -- because that is
// how a test harness stages a stand-in beside the mod it is standing in for.
// Before this, reaching it meant compiling an inert control guest and shipping
// a hundred kilobytes of Lua that is required at every load and called from
// nowhere.
func TestModPackagesADataStageOnlyMod(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{
		"--data-module", dataGuest(t, "fk_data"),
		"--name", "stand-in", "--version", "1.0.0", "--author", "someone",
		"-o", out}); err != nil {
		t.Fatal(err)
	}
	have := modFiles(t, filepath.Join(out, "stand-in_1.0.0"))
	// The debug map is written beside the data module it describes, which is
	// why the list is six rather than the five files this mod's Lua consists
	// of. Nothing in the game reads it; --no-map leaves it out.
	want := []string{"info.json", factorio.ABIFile, factorio.DataStageFile,
		factorio.DataModuleFile, factorio.DataMapFile, "data.lua"}
	for _, w := range want {
		if !have[w] {
			t.Errorf("a data-stage-only mod has no %s", w)
		}
	}
	// The three that describe a running program, and there is none.
	for _, unwanted := range []string{"control.lua", factorio.GeneratedModuleFile,
		factorio.APIFile} {
		if have[unwanted] {
			t.Errorf("a mod with no control module ships %s", unwanted)
		}
	}
	if len(have) != len(want) {
		t.Errorf("a data-stage-only mod ships %d files: %v", len(have), have)
	}
}

// ...and the same through the manifest, because `data_module` is a key as well
// as a flag and the positional check has to read the file to know the answer.
func TestADataStageOnlyModFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	guestPath := dataGuest(t, "fk_data")
	// gc = "collected" is in here deliberately. A MANIFEST KEY THAT CANNOT APPLY
	// IS NOT A CONTRADICTION: one checkout packaging several mods describes the
	// shipped one in fklua.toml and the rest on the command line, so refusing a
	// data-only packaging because the shipped mod is collected would make that
	// impossible. A typed --gc is refused; this is not.
	if err := os.WriteFile("fklua.toml", []byte(`
[mod]
name = "from-manifest"
version = "0.2.0"
author = "someone"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
gc = "collected"
data_module = "`+guestPath+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := runMod([]string{"-o", out}); err != nil {
		t.Fatal(err)
	}
	have := modFiles(t, filepath.Join(out, "from-manifest_0.2.0"))
	if !have["data.lua"] || have["control.lua"] {
		t.Errorf("the manifest's data_module did not package data-stage-only: %v", have)
	}
}

// WITH NEITHER MODULE THE MESSAGE IS WHAT IT ALWAYS WAS. Being handed no module
// of either kind is the same mistake it was before a data stage existed, and a
// command that grew a new shape should not have grown a new way to say that.
func TestNoModuleOfEitherKindIsStillRefused(t *testing.T) {
	err := runMod([]string{"--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", t.TempDir()})
	if err == nil {
		t.Fatal("no module at all should be refused")
	}
	if err.Error() != "no input module" {
		t.Errorf("the message is %q and has always been %q", err, "no input module")
	}
}

// The flags that describe a CONTROL guest are refused rather than ignored, one
// message naming every one that was typed. A flag whose value is silently
// discarded is this repo's most repeated failure shape.
func TestControlOnlyFlagsAreRefusedWithoutAControlModule(t *testing.T) {
	for _, flag := range []string{"--gc=collected", "--persist=packed", "--fuel=1000"} {
		err := runMod([]string{flag, "--data-module", dataGuest(t, "fk_data"),
			"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
			"-o", t.TempDir()})
		if err == nil {
			t.Errorf("%s should be refused with no control module, not ignored", flag)
			continue
		}
		name := strings.SplitN(flag, "=", 2)[0]
		for _, w := range []string{name, "no control module"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the message for %s does not contain %q:\n%v", flag, w, err)
			}
		}
	}
	// All three at once name all three, so an author fixing one is not sent
	// round the loop twice.
	err := runMod([]string{"--gc=collected", "--persist=packed", "--fuel=1000",
		"--data-module", dataGuest(t, "fk_data"),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", t.TempDir()})
	if err == nil {
		t.Fatal("three control-only flags should be refused")
	}
	for _, w := range []string{"--gc", "--persist", "--fuel"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the message does not name %s:\n%v", w, err)
		}
	}
	// ...and none of them is refused when there IS a control module.
	if err := runMod([]string{tinyGuest(t), "--persist=packed", "--fuel=1000",
		"--data-module", dataGuest(t, "fk_data"),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", t.TempDir()}); err != nil {
		t.Errorf("a control module makes the same flags legal again: %v", err)
	}
}

// A mod with no data module ships exactly what it always shipped, plus the
// debug map that sits beside the generated module. The packager's own
// byte-identity gate is in internal/factorio; this is the same property through
// the command, because that is where a default could be introduced by accident.
//
// --no-map is the arm that still ships five, which is what makes the map an
// addition rather than a change to the mod: the five Lua files are untouched
// either way, and TestNoMapSubtractsOnlyTheMap compares them byte for byte.
func TestModWithoutADataModuleShipsFiveFilesAndAMap(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", out}); err != nil {
		t.Fatal(err)
	}
	have := modFiles(t, filepath.Join(out, "a-mod_0.1.0"))
	if len(have) != 6 || !have[factorio.MapFile] {
		t.Errorf("a mod with no data module ships %d files: %v", len(have), have)
	}
	bare := t.TempDir()
	if err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "--no-map", "-o", bare}); err != nil {
		t.Fatal(err)
	}
	if got := modFiles(t, filepath.Join(bare, "a-mod_0.1.0")); len(got) != 5 {
		t.Errorf("--no-map ships %d files: %v", len(got), got)
	}
	for _, unwanted := range []string{factorio.DataStageFile, factorio.DataModuleFile,
		"settings.lua", "data.lua", "data-updates.lua", "data-final-fixes.lua"} {
		if have[unwanted] {
			t.Errorf("a mod with no data module ships %s", unwanted)
		}
	}
}

// ONE LINE BUILDS THE RAISE PREFIX AND FIVE PLACES QUOTE IT.
//
// The line that runs is fail() in the data-stage shim:
//
//	error("fklua: at the " .. sname .. " stage, " .. what, 0)
//
// and a guest's own fkdata.raise arrives through it verbatim, so the text a
// player reads in front of a library's diagnostic is that literal and nothing
// else. Five surfaces say so to the author who has to write a message that
// does not repeat the stage: the Go doc comment on Raise, the Rust doc comment
// on raise, docs/data-stage.md, and the two `init --library --data` scaffolds.
//
// A quotation that drifts from the shim is not a stale comment. It is a
// document that lies about what the player reads, and the weaker version of
// this contract has already been paid for once: FkRecipes' adoption read "the
// stage name is prefixed" as a fact rather than a text, and read
// "fklua: at the data stage, fkrecipes: at the data stage, ..." end to end
// through the real shim with nothing failing. That is this repo's recorded
// shape of two places disagreeing about one key, and this test guards it
// before it happens again.
//
// The prefix is READ OUT OF the shim rather than restated here, so the test
// cannot itself become a sixth copy that drifts.
func TestEveryStatementOfTheRaisePrefixIsTheShimsOwn(t *testing.T) {
	built := regexp.MustCompile(`error\("(fklua: at the )" \.\. sname \.\. "( stage, )" \.\. what, 0\)`)
	m := built.FindStringSubmatch(luart.DataStage())
	if m == nil {
		t.Fatalf("fail() no longer builds the prefix the way this test reads it "+
			"(%s). Five places quote that literal -- the Go doc comment on "+
			"fkdata.Raise, the Rust doc comment on fkdata::raise, "+
			"docs/data-stage.md, and the two `init --library --data` scaffolds "+
			"libraryGoDataPure and libraryRustDataLib -- and they and this test "+
			"move together, or the docs start lying about what the player reads",
			built)
	}
	// Joined around the placeholder the docs use, and around "data" for the
	// worked example every doc surface shows.
	documented := m[1] + "<stage>" + m[2]
	example := m[1] + "data" + m[2]

	root := moduleRoot(t)
	for _, f := range []string{
		filepath.Join("guest", "go", "fkdata", "fkdata.go"),
		filepath.Join("guest", "rust", "fkdata", "src", "lib.rs"),
		filepath.Join("docs", "data-stage.md"),
	} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if !strings.Contains(text, documented) {
			t.Errorf("%s does not state the prefix as the shim builds it, %q", f, documented)
		}
		if !strings.Contains(text, example) {
			t.Errorf("%s does not show the worked line beginning %q", f, example)
		}
	}

	// The scaffolds are checked IN PROCESS rather than by reading library.go,
	// because what a library author reads is the generated file.
	for _, s := range []struct{ name, text string }{
		{"the --library --data Go scaffold (libraryGoDataPure)", libraryGoDataPure("x", "x")},
		{"the --library --data Rust scaffold (libraryRustDataLib)", libraryRustDataLib("")},
	} {
		if !strings.Contains(s.text, documented) {
			t.Errorf("%s does not state the prefix as the shim builds it, %q", s.name, documented)
		}
	}
}
