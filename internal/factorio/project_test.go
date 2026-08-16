package factorio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest round-trips, and an unknown key is an ERROR.
//
// The second half is the one that matters. A typo'd key silently doing nothing
// is how a pin stops pinning: `apy = "2.0.77"` would leave the project
// unpinned, `--check` would compare against the default, and nothing would ever
// say so.
func TestProjectRoundTripsAndRejectsTypos(t *testing.T) {
	p := Project{
		Name: "my-mod", Version: "0.1.0", Title: "My Mod", Author: "someone",
		FactorioVersion: "2.0", API: "2.0.77", Langs: []string{"go", "rust"},
	}
	got, err := ParseProject(p.TOML())
	if err != nil {
		t.Fatalf("a manifest this package wrote must parse: %v\n%s", err, p.TOML())
	}
	if got.Name != p.Name || got.API != p.API || len(got.Langs) != 2 {
		t.Errorf("round trip lost something: %+v", got)
	}

	for _, bad := range []struct{ name, src string }{
		{"typo'd key", "[fklua]\napy = \"2.0.77\"\n"},
		{"unknown section key", "[mod]\nnmae = \"x\"\n"},
		{"no api pin", "[mod]\nname = \"x\"\n"},
		{"no name", "[fklua]\napi = \"2.0.77\"\n"},
	} {
		if _, err := ParseProject(bad.src); err == nil {
			t.Errorf("%s should be rejected, and was accepted", bad.name)
		}
	}
}

func TestLockRoundTrips(t *testing.T) {
	l := Lock{API: "2.0.77", APISHA256: "aa", BindingsSHA256: "bb", Fklua: "0.1.0"}
	got, err := ParseLock(l.Text())
	if err != nil {
		t.Fatal(err)
	}
	if got != l {
		t.Errorf("got %+v, want %+v", got, l)
	}
	if !strings.Contains(l.Text(), "Do not edit") {
		t.Error("a generated lock must say so; a hand-edited lock is a lock that lies")
	}
}

// The tree hash has to depend on the paths and not on their order, or a lock
// written on one machine would not verify on another.
func TestHashTreeIsOrderIndependentAndPathSensitive(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	for p, body := range map[string]string{a: "package a\n", b: "package b\n"} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h1, err := HashTree([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashTree([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("the hash depended on argument order, so it would differ per machine")
	}

	// Renaming a file with identical bytes must change the hash: the path is
	// part of what is being pinned.
	c := filepath.Join(dir, "c.go")
	if err := os.WriteFile(c, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3, err := HashTree([]string{c, b})
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Error("renaming a generated file did not change the hash, so the lock " +
			"would not notice a file moving")
	}

	// And editing one must.
	if err := os.WriteFile(a, []byte("package a // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h4, err := HashTree([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h1 {
		t.Error("editing generated code did not change the hash, which is the " +
			"one thing the bindings hash exists to catch")
	}
}

// The keys `fklua mod` needs, and the reason they belong in the manifest at
// all: identity used to live in two places -- `init` wrote name/version/title/
// author into fklua.toml and `mod` took every one of them as a flag and never
// read the file, so a Makefile had to sed the toml back into flags. And a
// mod's info.json `dependencies` could not be expressed anywhere, which any
// data-stage mod needs.
func TestAManifestCarriesDependenciesAndADataDirectory(t *testing.T) {
	p := Project{
		Name: "my-mod", Version: "0.1.0", Title: "My Mod", Author: "someone",
		FactorioVersion: "2.0", API: "2.0.77", Langs: []string{"go"},
		Data:         "mod-data",
		Dependencies: []string{"base >= 2.0.0", "? some-other-mod"},
	}
	got, err := ParseProject(p.TOML())
	if err != nil {
		t.Fatalf("a manifest this package wrote must parse: %v\n%s", err, p.TOML())
	}
	if got.Data != "mod-data" {
		t.Errorf("data = %q, want mod-data", got.Data)
	}
	if len(got.Dependencies) != 2 || got.Dependencies[0] != "base >= 2.0.0" ||
		got.Dependencies[1] != "? some-other-mod" {
		t.Errorf("dependencies round-tripped as %q", got.Dependencies)
	}
}
