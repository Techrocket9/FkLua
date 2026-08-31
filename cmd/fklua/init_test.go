package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
)

// A FRESH `fklua init` PROJECT BUILDS AND PACKAGES COLLECTED WITH NO HAND
// EDITS, which is the only assertion that can say "collected is the default"
// rather than "collected is recommended".
//
// Every piece of this was separately true before and the whole was not. init
// printed the right four lines of shell; fklua.toml had no gc key so `fklua
// mod` defaulted to leaking whatever the author had been told; and there was
// no guest source, so "buildable" was a claim about a file the author had yet
// to write. A test that checks the printed advice is a test of a string. This
// one runs the advice.
//
// WHAT IT DELIBERATELY DOES NOT DO IS REACH THE NETWORK. The scaffolded go.mod
// normally requires the published guest substrate, which is one `go mod tidy`
// for a real author and an input this repo's CI is not allowed to take. So the
// test passes init's --guest-module, which writes a `replace` onto THIS
// checkout's guest/go -- a real, documented flag whose whole purpose is
// building against a local FkLua rather than a released one. It is a flag on
// the init invocation and not an edit to anything init produced, which is the
// distinction the assertion cares about.
func TestAFreshInitProjectBuildsAndPackagesCollected(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this builds a guest with tinygo")
	}
	if ok, why := guest.Available(); !ok {
		// A CLEAN SKIP, and it is clean because TestTheGuestToolchainIsAvailable
		// in this same package FAILS for the same condition. The absence is
		// reported once, loudly, by something that fails; every other test is
		// free to skip quietly. See gc_test.go.
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	// A REAL DIRECTORY, ENTERED. init, mod and lock all work on the working
	// directory by design -- "a command that builds a mod writes only into the
	// working directory" is a rule this repo already had to learn once -- so
	// exercising them means being somewhere else and coming back.
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "init-e2e-mod"
	// THE CHECKOUT ROOT, not `<root>/guest/go`, and the difference is the whole
	// of fklua-ports' G5b. rustSubstrateDir has always accepted all three
	// layouts on the stated grounds that ONE FLAG SERVES BOTH LANGUAGES -- so
	// the checkout root is the path a person naturally has -- while the Go arm
	// wrote the flag verbatim into its `replace` and produced a project that
	// could not build: "found ... but does not contain package .../fk".
	//
	// This test passed throughout, because it handed init the one layout that
	// needs no normalisation. Passing the root is what makes it a test of
	// goSubstrateDir rather than of a coincidence.
	if err := runInit([]string{modName, "--guest-module", root}); err != nil {
		t.Fatalf("fklua init: %v", err)
	}

	// 1. THE MANIFEST SAYS COLLECTED, and it says it in a form the parser this
	//    project ships reads back. Rendering and parsing are separate code and a
	//    key that only round-trips through the writer is a key that works until
	//    somebody reads it.
	raw, err := os.ReadFile(projectFile)
	if err != nil {
		t.Fatalf("init wrote no %s: %v", projectFile, err)
	}
	proj, err := factorio.ParseProject(string(raw))
	if err != nil {
		t.Fatalf("init wrote an %s its own parser rejects: %v\n%s",
			projectFile, err, raw)
	}
	if proj.GC != "collected" {
		t.Fatalf("a new Go project is gc = %q, want \"collected\" -- collected is "+
			"the default for a NEW project, which is the whole of this change",
			proj.GC)
	}

	// 2. THE GUEST SOURCE EXISTS AND CARRIES THE COLLECTOR IMPORT. Checked
	//    before the build, because a build failure here would otherwise be
	//    reported as a toolchain problem when it is a scaffolding one.
	gcPath := filepath.Join(guestDir, "gc.go")
	body, err := os.ReadFile(gcPath)
	if err != nil {
		t.Fatalf("init scaffolded no %s, so there is nothing for -gc=custom to "+
			"link against: %v", gcPath, err)
	}
	if !strings.Contains(string(body), GuestSubstrateModule+"/fkgc") {
		t.Fatalf("%s does not import fkgc, so a -gc=custom build of it will fail "+
			"to LINK with `missing core function \"runtime.free\"` -- which names "+
			"neither this file nor the flag:\n%s", gcPath, body)
	}

	// 3. IT BUILDS, with the collector, through the same flag list this repo's
	//    own guests use. guest.CollectedBuildFlags rather than a list spelled
	//    here: two spellings of a load-bearing flag set drift, and the direction
	//    that drifts is always the unchecked one.
	wasm := filepath.Join(dir, modName+".wasm")
	if err := guest.BuildWith(filepath.Join(dir, guestDir), ".", wasm,
		guest.CollectedBuildFlags); err != nil {
		t.Fatalf("the scaffolded guest does not build collected, so `fklua init`'s "+
			"own next-steps do not work: %v", err)
	}

	// 4. IT PACKAGES WITH NO FLAGS AT ALL, and comes out COLLECTED. This is the
	//    assertion the whole test exists for: --gc is not on the command line,
	//    so the mode can only have come from fklua.toml, and checkGC would have
	//    refused it if the guest that init scaffolded did not really carry a
	//    collector. The two halves check each other.
	out := filepath.Join(dir, "out")
	if err := runMod([]string{wasm, "-o", out}); err != nil {
		t.Fatalf("packaging a fresh init project with no flags failed: %v", err)
	}

	// The collector surface reached control.lua. `fklua mod` printing
	// --gc=collected is what the command believes; this is what it emitted, and
	// they are not the same claim -- the export list is what the host binds.
	pkgDir := filepath.Join(out, modName+"_0.1.0")
	control, err := os.ReadFile(filepath.Join(pkgDir, "control.lua"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range factorio.CollectorSurface() {
		if !strings.Contains(string(control), name) {
			t.Errorf("the packaged control.lua never mentions %s, so the mod is "+
				"not wired to a collector however it was compiled", name)
		}
	}
}

// A PROJECT WITH NO gc KEY COMPILES EXACTLY AS IT DID BEFORE THE KEY EXISTED.
//
// `init` SCAFFOLDS THE GUEST WHERE `gen-bindings` AND `lock` ALREADY WRITE, in
// BOTH languages, and this is the assertion that keeps them from parting company
// a third time.
//
// THE PATHS ARE NOT NEGOTIABLE ON THE BINDINGS SIDE. GoBindingsPath and
// RustBindingsPath are hard-coded, `fklua lock` hashes both by exact name, and
// gen-bindings writes the whole Rust crate at one of them -- so the scaffold is
// the half that has to agree, and the shape it has to agree on is "the bindings
// are a DIRECT subpackage of the guest": `<guest>/fkapi`, imported as
// `<module>/fkapi`.
//
// IT HAS BEEN WRONG ONCE PER LANGUAGE. Rust scaffolded to guest-rs/ while the
// bindings went to guest/rust/fkapi, so the generated crate was orphaned outright
// and `init --lang rust` produced a project that could not call the API at all
// (fklua-ports R8). Go scaffolded to guest/ while the bindings went to
// guest/go/fkapi, which was subtler and lasted longer: it BUILT, as a subpackage
// one segment deeper than any document describes, so nothing failed and three
// independent mods (BetterBeltBalancer, nixie-tubes, qol-research) each moved
// their guest to guest/go by hand instead.
//
// So this is a test of the RELATIONSHIP rather than of two strings. Spelling the
// expected directories here would only record today's answer twice; what must
// hold is that whatever the scaffold picks, the bindings land one level inside
// it. No toolchain: it runs init and looks at the tree.
func TestTheScaffoldIsWhereTheBindingsGo(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := runInit([]string{"layout-mod", "--lang", "go,rust"}); err != nil {
		t.Fatalf("fklua init: %v", err)
	}

	for _, c := range []struct {
		lang, guest, bindings, marker string
	}{
		// The Go guest's module root, and the file gen-bindings writes.
		{"go", guestDir, GoBindingsPath, "go.mod"},
		// The Rust WORKSPACE root -- the guest crate is a member beside fkapi,
		// so the workspace is what has to contain the bindings crate.
		{"rust", rustGuestDir, RustBindingsPath, "Cargo.toml"},
	} {
		// The scaffold really put a guest there. Without this the rest of the
		// case would pass over an empty tree.
		if _, err := os.Stat(filepath.Join(c.guest, c.marker)); err != nil {
			t.Errorf("init scaffolded no %s in %s, so there is no %s guest at the "+
				"directory this test is about: %v",
				c.marker, c.guest, c.lang, err)
			continue
		}
		// `guest/go/fkapi/fkapi.go` -> `guest/go/fkapi` -> `guest/go`. For Rust
		// the crate is `guest/rust/fkapi` and the file is two deeper
		// (`src/api.rs`), so the crate directory is derived by trimming to the
		// bindings crate rather than by a fixed number of Dir() calls.
		crate := filepath.Dir(c.bindings)
		for filepath.Base(crate) != "fkapi" && crate != "." && crate != string(filepath.Separator) {
			crate = filepath.Dir(crate)
		}
		if filepath.Base(crate) != "fkapi" {
			t.Fatalf("%s bindings path %q has no fkapi directory in it; this test "+
				"assumes the generated bindings are a crate/package called fkapi",
				c.lang, c.bindings)
		}
		if got := filepath.Dir(crate); got != filepath.Clean(c.guest) {
			t.Errorf("`fklua init` scaffolds the %s guest at %q, but gen-bindings "+
				"writes %q -- so the generated bindings are not a direct subpackage "+
				"of the guest (%q is), and `fklua lock` hashes a file the guest does "+
				"not import. Move the scaffold or move GoBindingsPath/"+
				"RustBindingsPath; do not leave them one segment apart, which is the "+
				"shape three downstream mods corrected by hand",
				c.lang, c.guest, c.bindings, got)
		}
	}
}

// `init` TELLS GIT ABOUT THE ARTIFACTS ITS OWN NEXT STEPS PRODUCE.
//
// Nothing did, and the omission is not theoretical: a `<name>.wasm` sits at the
// project root because that is where init's printed `tinygo build -o ../../` line
// puts it, the cargo target tree sits under the Rust guest, and `fklua mod`
// writes `<name>_<version>/` beside them. A downstream publish flow was bitten by
// exactly the first of those, an untracked wasm at the root.
//
// AND `fklua.lock` IS NOT IN IT, which is the assertion this test would be
// pointless without: the lock is generated, so a reflexive "ignore what is
// generated" sweep takes it -- and it is the one generated file that is meant to
// be COMMITTED, because it records which API description the bindings came from
// and `fklua lock --check` in CI has to be able to read it. Checked line by line
// rather than as a substring, because the file NAMES fklua.lock in a comment
// saying precisely this and a substring check would call that comment a bug.
//
// No toolchain: it runs init and looks at the tree.
func TestInitWritesAGitignoreForItsOwnBuildOutput(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "ignore-mod"
	// Both languages in one run, so both per-language arms are covered by the
	// same file rather than by two runs that could disagree.
	if err := runInit([]string{modName, "--lang", "go,rust"}); err != nil {
		t.Fatalf("fklua init: %v", err)
	}
	body, err := os.ReadFile(gitignoreFile)
	if err != nil {
		t.Fatalf("init wrote no %s: %v", gitignoreFile, err)
	}

	patterns := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns[line] = true
	}
	for _, want := range []string{
		// The Go artifact, at the root, spelled the way init's own next step
		// spells it.
		"/" + modName + ".wasm",
		// Cargo's build tree, derived from the scaffold's own constant so the
		// two cannot drift.
		"/" + rustGuestDir + "/target/",
		// The package directory and the zip. The glob is on the VERSION, which
		// moves; the name does not.
		"/" + modName + "_*/",
		"/" + modName + "_*.zip",
	} {
		if !patterns[want] {
			t.Errorf("%s has no pattern %q, so `git status` in a fresh project is "+
				"noise the moment its own next steps are run:\n%s",
				gitignoreFile, want, body)
		}
	}
	for _, never := range []string{lockFile, "/" + lockFile} {
		if patterns[never] {
			t.Errorf("%s ignores %q. The lock is generated AND meant to be "+
				"committed -- it records which API description the bindings came "+
				"from, and `fklua lock --check` is a gate that reads it:\n%s",
				gitignoreFile, never, body)
		}
	}
}

// AN EXISTING .gitignore IS LEFT ALONE, TO THE BYTE, AND SAID SO OUT LOUD.
//
// This is deliberately NOT the per-file refusal the guest scaffold makes. Guest
// source is hand-edited and losing it to a re-run is unrecoverable; a .gitignore
// in a repository that already exists is simply the author's, and it is the
// normal state of any project someone has already started -- `git init && fklua
// init` is the ordinary order. An init that errored here would refuse every real
// project, so it is a NOTICE, and the notice has to name what the author now has
// to add themselves.
func TestInitLeavesAnExistingGitignoreAlone(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	// Content nothing in this repo would ever write, including the one pattern
	// an author might reasonably have already: if init merged or appended, this
	// file would not come back identical.
	const existing = "# mine\n/dist/\n*.log\n"
	if err := os.WriteFile(gitignoreFile, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	// captureStdout fails the test if runInit returns an error, which is half of
	// what is being asserted: an existing .gitignore does not stop init.
	out := captureStdout(t, func() error {
		return runInit([]string{"keeps-mine", "--lang", "go", "--no-guest"})
	})

	body, err := os.ReadFile(gitignoreFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != existing {
		t.Fatalf("init rewrote an existing %s. It is the author's file:\nwant %q\ngot  %q",
			gitignoreFile, existing, body)
	}
	if !strings.Contains(out, gitignoreFile) || !strings.Contains(out, "already exists") {
		t.Errorf("init said nothing about the %s it declined to write, so the "+
			"author is never told the build output is untracked:\n%s", gitignoreFile, out)
	}
	// The notice earns its line by naming a pattern, not by reporting a
	// non-event.
	if !strings.Contains(out, "/keeps-mine_*/") {
		t.Errorf("the notice does not name what to add, which is the only thing "+
			"an author can act on:\n%s", out)
	}
}

// A MOD NAME MAY CONTAIN SPACES, AND A GITIGNORE PATTERN CARRIES THEM LITERALLY.
//
// The name syntax is `^[A-Za-z0-9][A-Za-z0-9 _-]*$`: interior spaces are legal
// and every glob metacharacter (`*`, `?`, `[`, `!`, `#`) is excluded. So the
// patterns are plain concatenation, an interior space needs no backslash and no
// quoting, and the leading `/` keeps a line from ever starting with `#` or `!`.
// This test is what stops somebody adding escaping that would break the file it
// was meant to protect.
func TestAGitignoreCarriesAModNamesSpacesLiterally(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "space mod"
	if err := runInit([]string{modName, "--lang", "go", "--no-guest"}); err != nil {
		t.Fatalf("fklua init: %v", err)
	}
	body, err := os.ReadFile(gitignoreFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n/space mod.wasm\n",
		"\n/space mod_*/\n",
		"\n/space mod_*.zip\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s has no line %q -- a name with a space must appear as it "+
				"is, with nothing escaped and nothing quoted:\n%s",
				gitignoreFile, strings.TrimSpace(want), body)
		}
	}
}

// This is the backward-compatibility half and it needs no toolchain, which is
// why it is a separate test: every fklua.toml written before this change has no
// `gc` line, and if an absent key resolved to anything but "leave the command's
// own default alone" then adding the key would have silently changed what every
// existing project emits. The manifest here is otherwise complete, so what is
// being tested is the ABSENCE of one line and not the absence of a file.
func TestAProjectWithNoGCKeyKeepsTheLeakingDefault(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "legacy-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
lang = ["go"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	proj, ok, err := loadProject()
	if err != nil || !ok {
		t.Fatalf("loadProject: %v (found %v)", err, ok)
	}
	if proj.GC != "" {
		t.Fatalf("an absent gc key parsed as %q; it has to stay EMPTY, because "+
			"empty is what means \"nobody said\" and \"leaking\" is what means "+
			"somebody chose it", proj.GC)
	}

	// End to end: the leaking-only guest below has no collector surface, so if
	// an absent key had resolved to "collected" this would be a refusal rather
	// than a mod.
	out := filepath.Join(dir, "out")
	if err := runMod([]string{tinyGuest(t), "-o", out}); err != nil {
		t.Fatalf("packaging a pre-gc-key project failed, so adding the key broke "+
			"every manifest that predates it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "legacy-mod_0.1.0", "control.lua")); err != nil {
		t.Fatal(err)
	}
}

// THE MANIFEST IS THE DEFAULT AND THE FLAG IS THE OVERRIDE, in that direction
// only -- the same rule identity and the data stage already follow.
//
// The interesting direction is the one asserted here: a manifest saying
// "collected" plus an explicit --gc=leaking must produce a leaking build rather
// than a refusal. Without it, an author debugging a collector could not turn it
// off from the command line, which is the first thing anyone tries.
func TestTheGCFlagOverridesTheManifest(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "override-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
gc = "collected"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stand-in guest carries no collector, so a manifest that was NOT
	// overridden refuses -- which is what makes this test able to tell the
	// difference at all.
	if err := runMod([]string{tinyGuest(t), "-o", filepath.Join(dir, "no")}); err == nil {
		t.Fatal("gc = \"collected\" over a guest with no collector was accepted; " +
			"the manifest is not reaching checkGC and a mod would ship paying for " +
			"a collector it does not have")
	}
	if err := runMod([]string{tinyGuest(t), "--gc=leaking",
		"-o", filepath.Join(dir, "yes")}); err != nil {
		t.Fatalf("--gc=leaking did not override gc = \"collected\": %v", err)
	}
}

// A gc KEY THE COMPILER CANNOT PARSE NAMES THE FILE, not the flag.
//
// Once a mode can come from a manifest, every message about it has to say where
// it came from -- a reader whose first move is to search their command line for
// "--gc" will not find it, and will conclude the tool is confused.
func TestABadGCKeyNamesTheManifest(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "typo-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
gc = "collcted"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runMod([]string{tinyGuest(t), "-o", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("a misspelled gc key was accepted")
	}
	if !strings.Contains(err.Error(), projectFile) {
		t.Errorf("the error for a bad gc key never names %s, so it sends the "+
			"reader to a command line they did not type: %v", projectFile, err)
	}
}

// chdir enters dir and returns the way back. t.Chdir exists in Go 1.24 but
// restores only at test end, and two of the tests here want the pairing to be
// visible at the call site.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatal(err)
		}
	}
}

// The same loop on the other toolchain, and it exists because `gc =
// "collected"` became the default for a RUST project too.
//
// The Go twin above is the model and the differences are the point:
//
//   - There is NO gc.rs to check for an import. `-gc=custom` fails to LINK
//     without one on the Go side; on the Rust side `fk` owns the single
//     `#[global_allocator]` site and `--features fk/fkgc` chooses what backs it,
//     so the collector is a build flag alone and the scaffold's obligation is
//     that the flag is in the printed command and the dependency in Cargo.toml.
//   - The feature must NOT be declared in the guest's own Cargo.toml. Cargo's v2
//     resolver unifies features across a workspace build, so a declared one
//     would turn the collector on for every other crate in the same invocation
//     -- silently, and only for that invocation.
//
// It packages with NO FLAGS, so the mode can only have come from fklua.toml, and
// checkGC would have refused it if the scaffolded guest did not really carry a
// collector. The two halves check each other, exactly as in the Go twin.
func TestAFreshRustInitProjectBuildsAndPackagesCollected(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "init-rs-e2e-mod"
	if err := runInit([]string{modName, "--lang", "rust", "--guest-module",
		filepath.Join(root, "guest", "rust")}); err != nil {
		t.Fatalf("fklua init --lang rust: %v", err)
	}

	raw, err := os.ReadFile(projectFile)
	if err != nil {
		t.Fatalf("init wrote no %s: %v", projectFile, err)
	}
	proj, err := factorio.ParseProject(string(raw))
	if err != nil {
		t.Fatalf("init wrote an %s its own parser rejects: %v\n%s",
			projectFile, err, raw)
	}
	if proj.GC != "collected" {
		t.Fatalf("a new Rust project is gc = %q, want \"collected\". Rust used to "+
			"get \"leaking\" explicitly because no collector existed for it; "+
			"guest/rust/fkgc is that collector and this key is what makes the "+
			"default true rather than recommended", proj.GC)
	}

	cargo := filepath.Join(rustGuestDir, "Cargo.toml")
	body, err := os.ReadFile(cargo)
	if err != nil {
		t.Fatalf("init scaffolded no %s: %v", cargo, err)
	}
	// THE WORKSPACE NAMES BOTH MEMBERS, and the fkapi one is what R8 was about:
	// the generated bindings have to be a crate the guest can depend on, in the
	// directory gen-bindings hard-codes, or a fresh project cannot reach the API.
	if !strings.Contains(string(body), `members = ["fkapi", "`+rustCrateName(modName)+`"]`) {
		t.Errorf("%s is not the two-member workspace the generated bindings need:\n%s",
			cargo, body)
	}
	crateCargo := filepath.Join(rustCrateDir(modName), "Cargo.toml")
	crateBody, err := os.ReadFile(crateCargo)
	if err != nil {
		t.Fatalf("init scaffolded no %s: %v", crateCargo, err)
	}
	if !strings.Contains(string(crateBody), "fkapi = { workspace = true }") {
		t.Errorf("the scaffolded guest does not depend on the generated bindings, "+
			"so it can log and count ticks and cannot call Factorio:\n%s", crateBody)
	}
	if !strings.Contains(string(body), "fk = { path =") {
		t.Errorf("%s does not depend on the local fk crate --guest-module named, "+
			"so this test would be measuring crates.io rather than this "+
			"checkout:\n%s", cargo, body)
	}
	if strings.Contains(string(body), "[features]") {
		t.Errorf("%s declares a features table. The collector feature belongs on "+
			"the command line: Cargo's v2 resolver unifies features across a "+
			"workspace build, so a declared one turns the collector on for every "+
			"other crate in the same invocation:\n%s", cargo, body)
	}

	// THE BINDINGS, WHICH IS init'S OWN NEXT STEP AND USED NOT TO WORK.
	//
	// gen-bindings writes the Rust bindings to guest/rust/fkapi/src/api.rs, a
	// path it hard-codes and `fklua lock` hashes by exact name -- and it used to
	// write ONLY that, no Cargo.toml and no lib.rs, into a directory the
	// scaffolded guest (then guest-rs/) did not reference. So a project that
	// followed init's printed instructions got 2 MB of generated Rust that
	// nothing could compile and a guest that could not call the API, and the way
	// out was copying two files out of a FkLua checkout by hand
	// (fklua-ports-samples, AD9).
	//
	// Running it HERE rather than pre-baking the crate is the whole point: what
	// is under test is that init's next-steps, in init's order, produce a
	// project that builds.
	t.Setenv("FKLUA_API_DIR", filepath.Join(root, "api"))
	if err := runGenBindings([]string{"--lang=rust"}); err != nil {
		t.Fatalf("fklua gen-bindings, which is what init tells the author to run next: %v", err)
	}
	for _, f := range []string{RustBindingsPath, RustCratePath, RustCrateLibPath} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("gen-bindings wrote no %s, so guest/rust/fkapi is not a crate "+
				"and the workspace cannot load it: %v", f, err)
		}
	}

	// IT BUILDS COLLECTED, through the same helper this repo's own Rust gates
	// use rather than a cargo line spelled here -- two spellings of a
	// load-bearing flag drift, and the one that drifts is the unchecked one.
	wasm, err := guest.BuildRustCollected(rustGuestDir, rustCrateName(modName),
		filepath.Join(dir, "cargo-collected"))
	if err != nil {
		t.Fatalf("the scaffolded Rust guest does not build collected, so `fklua "+
			"init`'s own next-steps do not work: %v", err)
	}

	out := filepath.Join(dir, "out")
	if err := runMod([]string{wasm, "-o", out}); err != nil {
		t.Fatalf("packaging a fresh Rust init project with no flags failed: %v", err)
	}
	pkgDir := filepath.Join(out, modName+"_0.1.0")
	control, err := os.ReadFile(filepath.Join(pkgDir, "control.lua"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range factorio.CollectorSurface() {
		if !strings.Contains(string(control), name) {
			t.Errorf("the packaged control.lua never mentions %s, so the mod is "+
				"not wired to a collector however it was compiled", name)
		}
	}

	// AND THE OTHER DIRECTION: the same source built WITHOUT the feature must be
	// REFUSED by the same manifest. That is what makes `gc = "collected"` a
	// default rather than a decoration -- dropping the flag is caught at package
	// time instead of shipping a guest that pays for a barrier and never
	// collects.
	leak, err := guest.BuildRust(rustGuestDir, rustCrateName(modName),
		filepath.Join(dir, "cargo-leaking"))
	if err != nil {
		t.Fatalf("the scaffolded Rust guest does not build leaking: %v", err)
	}
	err = runMod([]string{leak, "-o", filepath.Join(dir, "out-leak")})
	if err == nil {
		t.Fatal("a guest built WITHOUT --features fk/fkgc packaged happily under " +
			"gc = \"collected\". The manifest and the build can then disagree " +
			"silently, which is the failure the key exists to catch")
	}
	if !strings.Contains(err.Error(), "fk/fkgc") {
		t.Errorf("the refusal does not tell a Rust author which flag they are "+
			"missing:\n%v", err)
	}
}

// ONE FLAG, THREE COMMANDS, ONE SPELLING EACH -- and they were not the same one.
//
// `init` and `docs` took `--lang go` and answered "unknown argument" to
// `--lang=go`; `gen-bindings` took `--lang=all` and answered "unknown argument"
// to `--lang all`, which is the form its own usage line prints. Reported by a
// first-time author who had used one command and typed the other's spelling.
//
// Both forms in both directions, because a refusal that names the argument and
// not the spelling sends a reader to look for a flag that is right there.
func TestLangIsSpelledBothWaysEverywhere(t *testing.T) {
	// The parser, over the shapes an argv can be. `--lang` at the end of argv
	// is the one case with nothing to consume, and it must say so rather than
	// index past the end.
	for _, tc := range []struct {
		args     []string
		want     string
		wantNext int
		wantErr  bool
	}{
		{[]string{"--lang", "go,rust"}, "go,rust", 1, false},
		{[]string{"--lang=go,rust"}, "go,rust", 0, false},
		{[]string{"--lang=all"}, "all", 0, false},
		{[]string{"--lang="}, "", 0, false}, // empty, and parseLangs refuses it
		{[]string{"--lang"}, "", 0, true},
	} {
		if !isLangArg(tc.args[0]) {
			t.Errorf("isLangArg(%q) is false", tc.args[0])
			continue
		}
		got, next, err := langArg(tc.args, 0)
		if (err != nil) != tc.wantErr {
			t.Errorf("langArg(%q): err %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if err == nil && (got != tc.want || next != tc.wantNext) {
			t.Errorf("langArg(%q) = %q, next %d; want %q, next %d",
				tc.args, got, next, tc.want, tc.wantNext)
		}
	}

	// And end to end through the command that used to reject the equals form,
	// because a helper nothing routes through is a helper that fixed nothing.
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()
	if err := runInit([]string{"equals-mod", "--lang=go,rust", "--no-guest"}); err != nil {
		t.Fatalf("fklua init --lang=go,rust: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fklua.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `lang = ["go", "rust"]`) {
		t.Errorf("--lang=go,rust did not reach the manifest:\n%s", b)
	}
}
