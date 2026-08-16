package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// repoRoot is the module root, from cmd/fklua where these tests run.
func moduleRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", p, err)
	}
	return p
}

// borrowedAPIDir is a stand-in for an INSTALLED fklua's api directory: the
// description is reachable (a symlink, so this costs nothing) and the census
// beside it is a real file whose bytes and mtime can be checked afterwards.
//
// A copy rather than the checkout's own api/ because the assertion is "nothing
// was written here", and a test that proves that by writing to the real thing
// has already lost.
func borrowedAPIDir(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	src := filepath.Join(root, "api", factorio.DefaultAPIVersion)
	dst := filepath.Join(t.TempDir(), "api", factorio.DefaultAPIVersion)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "runtime-api.json"),
		filepath.Join(dst, "runtime-api.json")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(src, "census.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "census.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dst)
}

// modProject makes a working directory shaped like a mod: an fklua.toml, a
// guest tree to generate into, and NO api/ of its own -- because the API
// description is part of the compiler, not of the mod.
func modProject(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goOnlyTOML = `[mod]
name = "a-mod"
version = "0.1.0"
factorio_version = "2.0"

[fklua]
api = "` + factorio.DefaultAPIVersion + `"
lang = ["go"]
`

// BUILDING A MOD MUST NOT WRITE INTO THE COMPILER.
//
// gen-bindings resolved its two outputs against two different roots: the
// bindings relative to the working directory (right) and the census relative to
// the EXECUTABLE (wrong), so running it inside a mod project rewrote
// api/<version>/census.json in whichever FkLua checkout built the binary. It is
// a silent no-op only for as long as that census happens to be current, and a
// silent corruption the moment it is not.
func TestGenBindingsDoesNotWriteIntoTheCompilersCheckout(t *testing.T) {
	apiDir := borrowedAPIDir(t)
	census := factorio.CensusPath(apiDir, factorio.DefaultAPIVersion)
	before, err := os.ReadFile(census)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(census)
	if err != nil {
		t.Fatal(err)
	}

	proj := modProject(t, goOnlyTOML)
	t.Setenv("FKLUA_API_DIR", apiDir)
	t.Chdir(proj)

	if err := runGenBindings([]string{"--lang=go"}); err != nil {
		t.Fatal(err)
	}

	// The bindings are the mod's, and they land in the mod.
	if _, err := os.Stat(filepath.Join(proj, GoBindingsPath)); err != nil {
		t.Errorf("the bindings should be written into the project: %v", err)
	}
	// The census is the compiler's, and it is not this command's to touch.
	after, err := os.ReadFile(census)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("gen-bindings rewrote the compiler's census.json from a mod project")
	}
	st2, err := os.Stat(census)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Error("gen-bindings WROTE the compiler's census.json (mtime moved); " +
			"identical bytes only mean the census happened to be current")
	}
	// Nor does it leave an api/ tree behind in the mod as a consolation.
	if _, err := os.Stat(filepath.Join(proj, "api")); err == nil {
		t.Error("gen-bindings created an api/ directory inside the mod project")
	}
}

// The same command run where the description DOES live -- the compiler's own
// checkout -- still writes the census, because the two really are one act: a
// version bump regenerates the bindings and moves the numbers, and splitting
// them is how one of them gets forgotten.
func TestGenBindingsStillWritesTheCensusInTheCompilersOwnCheckout(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()
	dst := filepath.Join(work, "api", factorio.DefaultAPIVersion)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "api", factorio.DefaultAPIVersion, "runtime-api.json"),
		filepath.Join(dst, "runtime-api.json")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	if err := runGenBindings([]string{"--lang=go"}); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(factorio.CensusPath(filepath.Join(root, "api"),
		factorio.DefaultAPIVersion))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "census.json"))
	if err != nil {
		t.Fatalf("the census belongs beside the description, and here it is ours: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the census written here differs from the committed one")
	}
}

// `fklua init --lang go` writes lang = ["go"] and then prints
// "Next: `fklua gen-bindings`". Following that advice used to drop an unwanted
// guest/rust/ into a Go-only project, because gen-bindings defaulted to "all"
// and never read the manifest -- while `fklua lock`, which DOES read it, then
// hashed only the Go half. Two commands disagreeing about one key.
func TestGenBindingsHonoursTheProjectsLangList(t *testing.T) {
	apiDir := borrowedAPIDir(t)
	proj := modProject(t, goOnlyTOML)
	t.Setenv("FKLUA_API_DIR", apiDir)
	t.Chdir(proj)

	if err := runGenBindings(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, GoBindingsPath)); err != nil {
		t.Errorf("lang = [\"go\"] should generate Go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "guest", "rust")); err == nil {
		t.Error("lang = [\"go\"] generated guest/rust/ anyway")
	}
}

// A flag still wins. The manifest is the default, not a cage: regenerating one
// language on its own is a thing an author does, and `--lang` is how.
func TestAnExplicitLangFlagOverridesTheManifest(t *testing.T) {
	apiDir := borrowedAPIDir(t)
	proj := modProject(t, goOnlyTOML)
	t.Setenv("FKLUA_API_DIR", apiDir)
	t.Chdir(proj)

	if err := runGenBindings([]string{"--lang=rust"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, RustBindingsPath)); err != nil {
		t.Errorf("--lang=rust should generate Rust: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, GoBindingsPath)); err == nil {
		t.Error("--lang=rust generated Go too")
	}
}

// With no manifest at all -- this repo's own checkout, and the examples -- both
// languages are generated, which is what keeps `--check` able to gate both.
func TestWithNoManifestBothLanguagesAreGenerated(t *testing.T) {
	apiDir := borrowedAPIDir(t)
	proj := t.TempDir()
	t.Setenv("FKLUA_API_DIR", apiDir)
	t.Chdir(proj)

	if err := runGenBindings(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{GoBindingsPath, RustBindingsPath} {
		if _, err := os.Stat(filepath.Join(proj, p)); err != nil {
			t.Errorf("no manifest should mean both languages: %s missing", p)
		}
	}
}

// The manifest's API pin is the same key `fklua lock` hashes against. If
// gen-bindings generates from a different one, the lock records a SHA for a
// description the bindings never saw -- so the pin is honoured, and a pin this
// installation cannot satisfy is an error naming the version rather than a
// silent fall back to the default.
func TestGenBindingsGeneratesFromTheProjectsAPIPin(t *testing.T) {
	apiDir := borrowedAPIDir(t)
	proj := modProject(t, `[mod]
name = "a-mod"
version = "0.1.0"

[fklua]
api = "9.9.9"
lang = ["go"]
`)
	t.Setenv("FKLUA_API_DIR", apiDir)
	t.Chdir(proj)

	err := runGenBindings(nil)
	if err == nil {
		t.Fatal("expected an error: this installation has no API 9.9.9")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("9.9.9")) {
		t.Errorf("the error should name the pinned version, got: %v", err)
	}
}
