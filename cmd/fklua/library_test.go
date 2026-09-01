package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The library scaffold's job is the COMPOSITION CONTRACT: the rules a library
// author meets nowhere else are in the generated comments, and these tests pin
// that they are -- a template that drops one silently is a template that
// teaches the shipped violation back.

func TestInitLibraryScaffoldsTheGoContract(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib"}); err != nil {
		t.Fatal(err)
	}
	// A library is not a mod: no manifest, no gc key, nothing to package.
	if _, err := os.Stat(projectFile); err == nil {
		t.Errorf("--library wrote %s; a library has nothing to package", projectFile)
	}
	pure, err := os.ReadFile("my_lib.go")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := os.ReadFile("guest.go")
	if err != nil {
		t.Fatal(err)
	}
	// The contract, each rule by its load-bearing phrase.
	for _, w := range []string{
		"ROUTE, NEVER OWN",
		"ONE export per name",
		"THE CONSUMER'S BINDINGS, NEVER YOUR OWN",
		"PIN-TRANSPARENT",
		"IDS STAY INLINE",
		"JOIN SAFETY PASSES THROUGH",
	} {
		if !strings.Contains(string(pure), w) {
			t.Errorf("the pure file does not carry %q", w)
		}
	}
	if !strings.Contains(string(guest), "//go:build tinygo.wasm") {
		t.Errorf("guest.go is not build-gated; the host-testable split is the layout's whole point")
	}
	if !strings.Contains(string(guest), "guest/go/fk") {
		t.Errorf("guest.go does not import fk")
	}
	// No hook export anywhere in the scaffold: the rule enforced on the
	// template itself, not just stated by it.
	for name, body := range map[string][]byte{"my_lib.go": pure, "guest.go": guest} {
		if strings.Contains(string(body), "wasmexport") {
			t.Errorf("%s exports a hook; a library routes, never owns", name)
		}
	}
}

// The scaffold's pure half builds and tests on the HOST with the ordinary Go
// toolchain, which is the property the file split exists to keep -- and this
// runs `go` itself, in init's own order, because what is under test is that
// init's printed next step works.
func TestInitLibraryGoHalfTestsOnTheHost(t *testing.T) {
	root := moduleRoot(t) // before chdir: it resolves from the CWD
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib",
		"--guest-module", root}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	// The scaffold's replace points at the checkout, so nothing is fetched.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the scaffolded library does not test on the host: %v\n%s", err, out)
	}
}

// The Rust scaffold's pure half tests on the host with no wasm target, because
// its fk dependency is wasm-gated -- the fkipc arrangement, applied by the
// template.
func TestInitLibraryRustHalfTestsOnTheHost(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not installed")
	}
	root := moduleRoot(t) // before chdir: it resolves from the CWD
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib", "--lang", "rust",
		"--guest-module", root}); err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile(filepath.Join("src", "lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"#![cfg_attr(not(test), no_std)]",
		"#[inline(always)]",
		"never vendor one",
		"#[global_allocator]",
		"fk/fkgc",
	} {
		if !strings.Contains(string(lib), w) {
			t.Errorf("lib.rs does not carry %q", w)
		}
	}
	cmd := exec.Command("cargo", "test", "--quiet")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the scaffolded crate does not test on the host: %v\n%s", err, out)
	}
}

// The --data flavor carries the DATA contract (FkRecipes' dogfood ask: the
// control flavor misled a data library's build in both languages), and its
// pure half tests on the host, in init's own printed order.
func TestInitLibraryDataGoTestsOnTheHost(t *testing.T) {
	root := moduleRoot(t) // before chdir: it resolves from the CWD
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib", "--data",
		"--guest-module", root}); err != nil {
		t.Fatal(err)
	}
	pure, err := os.ReadFile("my_lib.go")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := os.ReadFile("guest.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"PLAN, THEN EMIT", "ROUTE, NEVER OWN", "NEVER fkapi",
		"PREFIX FROM ModName", "DIAGNOSE WITH Raise",
		"a refusal built here carries none",
	} {
		if !strings.Contains(string(pure), w) {
			t.Errorf("the data pure file does not carry %q", w)
		}
	}
	if !strings.Contains(string(guest), "guest/go/fkdata") {
		t.Errorf("the data guest half does not import fkdata")
	}
	if strings.Contains(string(guest), "guest/go/fk\"") {
		t.Errorf("the data guest half imports fk; a data library is fkdata-only")
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the scaffolded data library does not test on the host: %v\n%s", err, out)
	}
}

func TestInitLibraryDataRustTestsOnTheHost(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not installed")
	}
	root := moduleRoot(t) // before chdir: it resolves from the CWD
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib", "--data", "--lang", "rust",
		"--guest-module", root}); err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile(filepath.Join("src", "lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"PLAN, THEN EMIT", "NEVER fkapi", "mod_name", "raise",
		"#![cfg_attr(not(test), no_std)]",
		"a refusal built here carries none",
	} {
		if !strings.Contains(string(lib), w) {
			t.Errorf("the data lib.rs does not carry %q", w)
		}
	}
	toml, err := os.ReadFile("Cargo.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toml), "fkdata") || strings.Contains(string(toml), "\nfk = ") {
		t.Errorf("the data Cargo.toml should depend on fkdata and not fk:\n%s", toml)
	}
	cmd := exec.Command("cargo", "test", "--quiet")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the scaffolded data crate does not test on the host: %v\n%s", err, out)
	}
}

// One language per directory, and the mod-only flags are refused rather than
// ignored -- runMod's own rule for --persist on a data-only packaging.
func TestInitLibraryRefusals(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"--library", "my-lib", "--lang", "go,rust"}); err == nil ||
		!strings.Contains(err.Error(), "sibling") {
		t.Errorf("two languages were not refused with the sibling advice: %v", err)
	}
	if err := runInit([]string{"--library", "my-lib", "--api", "2.1.17"}); err == nil ||
		!strings.Contains(err.Error(), "CONSUMER") {
		t.Errorf("--api was not refused with the consumer's-pin reason: %v", err)
	}
	if err := runInit([]string{"--library", "my-lib", "--no-guest"}); err == nil {
		t.Errorf("--no-guest was not refused")
	}
	// --data belongs to --library; a MOD's data stage is declared elsewhere,
	// and the refusal says where.
	if err := runInit([]string{"my-mod", "--data"}); err == nil ||
		!strings.Contains(err.Error(), "data_module") {
		t.Errorf("--data without --library was not refused with the redirect: %v", err)
	}
	// Refuse rather than overwrite, per file: a half-written library is
	// recoverable, an overwritten one is not.
	if err := os.WriteFile("go.mod", []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"--library", "my-lib"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("an existing go.mod was overwritten: %v", err)
	}
}
