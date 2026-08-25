package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
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
		`require("fk_data").run(2)`, `require("prototypes.sprite")`} {
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

// A mod with no data module ships exactly what it always shipped. The
// packager's own byte-identity gate is in internal/factorio; this is the same
// property through the command, because that is where a default could be
// introduced by accident.
func TestModWithoutADataModuleShipsFiveFiles(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", out}); err != nil {
		t.Fatal(err)
	}
	have := modFiles(t, filepath.Join(out, "a-mod_0.1.0"))
	if len(have) != 5 {
		t.Errorf("a mod with no data module ships %d files: %v", len(have), have)
	}
	for _, unwanted := range []string{factorio.DataStageFile, factorio.DataModuleFile,
		"settings.lua", "data.lua", "data-updates.lua", "data-final-fixes.lua"} {
		if have[unwanted] {
			t.Errorf("a mod with no data module ships %s", unwanted)
		}
	}
}
