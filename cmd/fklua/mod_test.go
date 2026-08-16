package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
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
