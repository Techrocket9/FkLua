package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The smallest thing that packages: one memory, one export.
const tinyWAT = `(module (memory 1)
  (func (export "fk_on_tick")))`

func tinyGuest(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	if err := os.WriteFile(p, []byte(tinyWAT), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The flag itself, end to end and into a ZIP -- which is the half that was
// unusable for a real mod, because copying files over the output afterwards is
// exactly what an archive cannot have done to it.
func TestModIncludesADataStageInTheZip(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "prototypes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"data.lua":            "require(\"prototypes.belt\")\n",
		"prototypes/belt.lua": "data:extend{}\n",
	} {
		if err := os.WriteFile(filepath.Join(data, filepath.FromSlash(name)),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "--include", data, "--zip", "-o", out})
	if err != nil {
		t.Fatal(err)
	}

	r, err := zip.OpenReader(filepath.Join(out, "a-mod_0.1.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	have := map[string]bool{}
	for _, f := range r.File {
		have[f.Name] = true
	}
	for _, name := range []string{"a-mod_0.1.0/data.lua",
		"a-mod_0.1.0/prototypes/belt.lua", "a-mod_0.1.0/control.lua"} {
		if !have[name] {
			t.Errorf("zip is missing %s", name)
		}
	}
}

// IDENTITY LIVES IN ONE PLACE.
//
// `init` wrote name/version/title/author into fklua.toml and `mod` took every
// one of them as a flag and never read the file, so the two disagreed by
// construction and a downstream Makefile had to sed the manifest back into
// flags. And info.json's `dependencies` -- which any data-stage mod needs, and
// which Factorio refuses to load without when a prototype references another
// mod -- could not be expressed at all.
func TestModReadsTheManifest(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, "mod-data", "prototypes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "mod-data", "data.lua"),
		[]byte("-- prototypes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "fklua.toml"), []byte(`[mod]
name = "belt-balancer"
version = "1.2.3"
title = "Belt Balancer"
author = "someone"
factorio_version = "2.0"
description = "balances belts"
data = "mod-data"
dependencies = ["base >= 2.0.0", "? nullius"]

[fklua]
api = "`+"2.0.77"+`"
lang = ["go"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	guest := tinyGuest(t)
	t.Chdir(proj)

	// Not one flag. The manifest is the whole identity.
	if err := runMod([]string{guest}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(proj, "belt-balancer_1.2.3", "info.json"))
	if err != nil {
		t.Fatalf("the mod should be named from the manifest: %v", err)
	}
	var info struct {
		Name, Version, Title, Author, Description string
		Dependencies                              []string
	}
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "belt-balancer" || info.Version != "1.2.3" ||
		info.Title != "Belt Balancer" || info.Author != "someone" ||
		info.Description != "balances belts" {
		t.Errorf("info.json did not come from the manifest: %+v", info)
	}
	if len(info.Dependencies) != 2 || info.Dependencies[0] != "base >= 2.0.0" {
		t.Errorf("dependencies did not reach info.json: %q", info.Dependencies)
	}
	// [mod] data is the DEFAULT for --include, which is the shape gen-bindings
	// settled on for lang: one code path, fed by the manifest.
	if _, err := os.Stat(filepath.Join(proj, "belt-balancer_1.2.3", "data.lua")); err != nil {
		t.Errorf("[mod] data was not included: %v", err)
	}
}

// A flag still wins, for the same reason it does in gen-bindings: the manifest
// is the default, not a cage.
func TestAModFlagOverridesTheManifest(t *testing.T) {
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "fklua.toml"), []byte(`[mod]
name = "belt-balancer"
version = "1.2.3"
author = "someone"

[fklua]
api = "2.0.77"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	guest := tinyGuest(t)
	t.Chdir(proj)

	if err := runMod([]string{guest, "--version", "9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, "belt-balancer_9.9.9")); err != nil {
		t.Errorf("--version should override the manifest: %v", err)
	}
}

// modDeps packages a guest with the given flags and reads back what info.json
// declares. `nil` and an empty list are not the same answer here: `omitempty`
// leaves the key out entirely, which is what "this mod depends on nothing"
// looks like on disk.
func modDeps(t *testing.T, dir string, args ...string) ([]string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "deps-mod_0.1.0", "info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	body, present := raw["dependencies"]
	if !present {
		return nil, false
	}
	var deps []string
	if err := json.Unmarshal(body, &deps); err != nil {
		t.Fatal(err)
	}
	return deps, true
}

func depsProject(t *testing.T, manifestDeps string) string {
	t.Helper()
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "fklua.toml"), []byte(`[mod]
name = "deps-mod"
version = "0.1.0"
author = "someone"
`+manifestDeps+`
[fklua]
api = "2.0.77"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return proj
}

// ONE MANIFEST, SEVERAL PACKAGINGS.
//
// A repo that builds many mods out of one project -- a test estate whose
// observers each need their own dependency list -- had no way to say so: every
// other identity field is a flag and `dependencies` was manifest-only, so the
// list had to be rewritten in the packaged info.json afterwards by whatever was
// driving the build.
//
// The values reach info.json verbatim and IN ORDER, because Factorio's own
// syntax lives in them and this layer does not parse it.
func TestAModTakesItsDependenciesFromTheCommandLine(t *testing.T) {
	proj := depsProject(t, "")
	guest := tinyGuest(t)
	t.Chdir(proj)

	if err := runMod([]string{guest,
		"--dependency", "base >= 2.0.0",
		"--dependency", "? quality",
		"--dependency", "! other-mod"}); err != nil {
		t.Fatal(err)
	}
	deps, present := modDeps(t, proj)
	if !present {
		t.Fatal("--dependency did not reach info.json at all")
	}
	want := []string{"base >= 2.0.0", "? quality", "! other-mod"}
	if len(deps) != len(want) {
		t.Fatalf("info.json declares %q, want %q", deps, want)
	}
	for i := range want {
		if deps[i] != want[i] {
			t.Errorf("dependency %d is %q, want %q (the list is verbatim and "+
				"ordered)", i, deps[i], want[i])
		}
	}
}

// THE FLAG REPLACES THE MANIFEST'S LIST RATHER THAN ADDING TO IT, which is what
// every other identity flag does to its key and is the only semantics that can
// express a SMALLER list than the manifest's -- see the reasoning at the
// override in runMod.
func TestADependencyFlagReplacesTheManifestList(t *testing.T) {
	proj := depsProject(t, `dependencies = ["base >= 2.0.0", "? nullius"]`)
	guest := tinyGuest(t)
	t.Chdir(proj)

	if err := runMod([]string{guest, "--dependency", "? quality"}); err != nil {
		t.Fatal(err)
	}
	deps, _ := modDeps(t, proj)
	if len(deps) != 1 || deps[0] != "? quality" {
		t.Errorf("info.json declares %q; the flag list replaces the manifest's "+
			"wholesale, so appending here would make the manifest's entries "+
			"impossible to drop", deps)
	}
}

// THE EMPTY LIST IS A THING AN AUTHOR MEANS, and it is the case the whole
// replacement semantics exists for: Factorio orders mods by their dependency
// graph, so a mod that must load BEFORE another declares no dependency on it.
// An absent key is how info.json spells that.
func TestADependencyFlagCanEmptyTheManifestList(t *testing.T) {
	proj := depsProject(t, `dependencies = ["base >= 2.0.0", "? nullius"]`)
	guest := tinyGuest(t)
	t.Chdir(proj)

	if err := runMod([]string{guest, "--dependency", ""}); err != nil {
		t.Fatal(err)
	}
	deps, present := modDeps(t, proj)
	if present {
		t.Errorf(`--dependency "" left %q in info.json; an empty list is the `+
			`absence of the key, not a key holding nothing`, deps)
	}
}

// A contradiction is refused rather than resolved: whichever way an
// implementation broke the tie, half its callers would be surprised.
func TestAnEmptyDependencyCannotBeCombinedWithARealOne(t *testing.T) {
	proj := depsProject(t, "")
	guest := tinyGuest(t)
	t.Chdir(proj)

	err := runMod([]string{guest, "--dependency", "", "--dependency", "base >= 2.0.0"})
	if err == nil {
		t.Fatal(`--dependency "" beside a real dependency was accepted`)
	}
	if !strings.Contains(err.Error(), "--dependency") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// BACK COMPATIBILITY, AND IT IS THE HALF A NEW FLAG USUALLY BREAKS: a build that
// says nothing about dependencies packages exactly the manifest's list, byte for
// byte, as it did before the flag existed.
func TestWithoutTheFlagTheManifestListIsUntouched(t *testing.T) {
	proj := depsProject(t, `dependencies = ["base >= 2.0.0", "? nullius"]`)
	guest := tinyGuest(t)
	t.Chdir(proj)

	if err := runMod([]string{guest}); err != nil {
		t.Fatal(err)
	}
	deps, present := modDeps(t, proj)
	if !present || len(deps) != 2 ||
		deps[0] != "base >= 2.0.0" || deps[1] != "? nullius" {
		t.Errorf("info.json declares %q (present=%v), want the manifest's own "+
			"two entries", deps, present)
	}
}
