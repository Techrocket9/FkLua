package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// otherAPIVersion is the committed version that is NOT the default. Both ship
// in-repo, which is exactly what made the defect below silent: two LoadAPI
// calls that both succeed and disagree.
func otherAPIVersion(t *testing.T) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(moduleRoot(t), "api"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() && e.Name() != factorio.DefaultAPIVersion {
			return e.Name()
		}
	}
	t.Skip("only one API version is committed, so no pin can be distinguished " +
		"from the default; commit a second one or this test proves nothing")
	return ""
}

// firstDivergentMember finds an id the two committed versions disagree about,
// and reports the id plus what each version calls it.
//
// DERIVED RATHER THAN HARD-CODED on purpose. An id baked into this file is one
// that quietly stops discriminating the next time either description is
// regenerated -- and a test for a silent misbinding that silently stops testing
// for it is worse than no test. Ids are dense sorted indices per version, so a
// member added anywhere shifts every later one and this search always succeeds
// while the two versions differ at all.
func firstDivergentMember(t *testing.T, other string) (id int, def, pinned string) {
	t.Helper()
	root := moduleRoot(t)
	load := func(v string) []factorio.Member {
		a, err := factorio.LoadAPI(filepath.Join(root, "api", v, "runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		return factorio.GenerateMembers(a).Members
	}
	da, pa := load(factorio.DefaultAPIVersion), load(other)
	for i := range da {
		if i >= len(pa) {
			break
		}
		if da[i].Name != pa[i].Name {
			return da[i].ID, da[i].Name, pa[i].Name
		}
	}
	t.Skipf("api/%s and api/%s assign every id the same name, so no member call "+
		"can tell them apart", factorio.DefaultAPIVersion, other)
	return 0, "", ""
}

// callingGuest writes a guest that calls exactly one member, by a constant id
// the pruning scan can see -- so the packaged table contains that member and
// nothing else, and its name says which description it came from.
func callingGuest(t *testing.T, id int) string {
	t.Helper()
	return stampedGuest(t, id)
}

// stampedGuest is callingGuest plus the pin stamps a generated binding set
// carries, which is what makes the guest's OWN version provable.
//
// Written as a .wat rather than built through a toolchain because the guard
// reads export NAMES and nothing else: a TinyGo build would take a minute to
// prove a property that lives entirely in the export section, and it could only
// ever stamp the one version this checkout's committed bindings carry. What has
// to be tested is the MISMATCH, and no real build produces one on purpose.
// TestBothGeneratorsStampTheSameName is what ties this spelling back to the
// generators' real output.
func stampedGuest(t *testing.T, id int, stamps ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	var b strings.Builder
	fmt.Fprintf(&b, `(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick")
    (drop (call $call (i32.const 1) (i32.const %d) (i32.const 0) (i32.const 64))))`, id)
	for _, s := range stamps {
		fmt.Fprintf(&b, "\n  (func (export %q) (nop))", s)
	}
	b.WriteString(")")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func packagedAPITable(t *testing.T, outDir, mod string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(outDir, mod, factorio.APIFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// THE api PIN TRAVELS TO `fklua mod` THE WAY gc DOES, and until the 2026-08-04
// audit it did not travel at all.
//
// Member ids are dense sorted indices over one version's member set, so the
// table a mod ships and the bindings its guest was compiled against are only
// meaningful as a PAIR -- internal/factorio/gen.go states it. gen-bindings and
// lock read the pin; attachAPI loaded DefaultAPIVersion unconditionally. A
// project pinned to a version other than the default therefore packaged a table
// whose ids were assigned over a DIFFERENT member set: the guest calls the wrong
// member, silently wherever the kinds line up, in a lockstep game.
//
// The assertion is on a member id the two committed descriptions name
// differently, so it cannot pass by coincidence -- both tables contain that id,
// and only the pinned one gives it the pinned name.
func TestTheAPIPinTravelsToMod(t *testing.T) {
	other := otherAPIVersion(t)
	id, defName, pinnedName := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "pinned-mod"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := runMod([]string{callingGuest(t, id), "-o", out}); err != nil {
		t.Fatal(err)
	}
	table := packagedAPITable(t, out, "pinned-mod_0.1.0")

	if !strings.Contains(table, fmt.Sprintf("application_version = %q", other)) {
		t.Errorf("the packaged member table does not come from the pinned API %s; "+
			"`fklua mod` is ignoring fklua.toml's api key while gen-bindings and "+
			"lock honour it, so the guest's ids and this table were assigned over "+
			"different member sets", other)
	}
	if !strings.Contains(table, fmt.Sprintf("name=%q", pinnedName)) {
		t.Errorf("member %d is not %q in the packaged table; the pin did not reach "+
			"attachAPI", id, pinnedName)
	}
	if strings.Contains(table, fmt.Sprintf("name=%q", defName)) {
		t.Errorf("member %d is %q in the packaged table, which is what API %s calls "+
			"it -- the default version was packaged over a project pinned to %s",
			id, defName, factorio.DefaultAPIVersion, other)
	}
}

// AN --api FLAG OVERRIDES THE MANIFEST, in that direction only -- the same rule
// gc, identity and the data stage follow. Without it there is no way to package
// a mod against a version other than the one its manifest pins, which is the
// first thing anyone does when testing a version bump.
func TestTheAPIFlagOverridesTheManifest(t *testing.T) {
	other := otherAPIVersion(t)
	id, defName, _ := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "flag-mod"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := runMod([]string{callingGuest(t, id),
		"--api=" + factorio.DefaultAPIVersion, "-o", out}); err != nil {
		t.Fatal(err)
	}
	table := packagedAPITable(t, out, "flag-mod_0.1.0")
	if !strings.Contains(table, fmt.Sprintf("name=%q", defName)) {
		t.Errorf("--api=%s did not override api = %q in %s",
			factorio.DefaultAPIVersion, other, projectFile)
	}
}

// THE NO-MANIFEST PATH KEEPS DefaultAPIVersion, which is the whole of the
// backward compatibility: every in-repo build, every example and every project
// that predates the pin travelling runs without an fklua.toml here, and must
// package byte-for-byte what it packaged before.
func TestWithNoManifestTheDefaultAPIIsPackaged(t *testing.T) {
	other := otherAPIVersion(t)
	id, defName, pinnedName := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	out := filepath.Join(dir, "out")
	if err := runMod([]string{callingGuest(t, id), "--name", "bare-mod",
		"--version", "0.1.0", "-o", out}); err != nil {
		t.Fatal(err)
	}
	table := packagedAPITable(t, out, "bare-mod_0.1.0")
	if !strings.Contains(table,
		fmt.Sprintf("application_version = %q", factorio.DefaultAPIVersion)) ||
		!strings.Contains(table, fmt.Sprintf("name=%q", defName)) {
		t.Errorf("a project with no %s packaged something other than API %s "+
			"(member %d should be %q, not %q)", projectFile,
			factorio.DefaultAPIVersion, id, defName, pinnedName)
	}
}

// -o NAMES ONE FILE, so it cannot serve two languages.
//
// Every target took `dst = out`, so with both languages selected the Go
// bindings were written and the Rust bindings written straight over them: exit
// 0, "wrote <path>" twice, Rust source in a .go-named file. The innocent
// invocation that hits it is `fklua gen-bindings -o /tmp/api.go` outside a
// project, where the language default is `all`.
func TestGenBindingsRefusesOneOutputForTwoLanguages(t *testing.T) {
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	out := filepath.Join(dir, "api.go")
	err := runGenBindings([]string{"-o", out, "--lang=all"})
	if err == nil {
		t.Fatal("-o with both languages was accepted; the Rust bindings overwrite " +
			"the Go ones and the survivor is Rust source in a .go-named file")
	}
	if !strings.Contains(err.Error(), "-o") {
		t.Errorf("the refusal does not name the flag that caused it: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("the refusal came after something was already written")
	}

	// The valid one-language invocation is unchanged -- this is a refusal, not
	// a restriction on -o.
	if err := runGenBindings([]string{"-o", out, "--lang=go"}); err != nil {
		t.Fatalf("-o with one language should still write: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "package fkapi") {
		t.Error("-o --lang=go did not write the Go bindings")
	}
}

// A GUEST BUILT AGAINST ANOTHER DESCRIPTION IS REFUSED, and before the pin
// stamp existed nothing could tell.
//
// This is the defect a downstream mod measured and worked around: the library
// packages inside the FkLua guest module -- fkipc above all -- import THAT
// module's committed fkapi, which sits at DefaultAPIVersion. A consumer that
// vendors a checkout and pins anything else links bindings from one description
// and packages a table from another, and both halves succeed on their own.
// Measured at 2.1.14 against bindings at 2.0.77: fkipc subscribed to event 207
// believing it was on_udp_packet_received and got on_train_changed_state, and
// read helpers.game_version and got LuaForce.object_name -- so its engine floor
// parsed "0.0.0" and the library went inert. The mod loaded, ran and ticked, and
// one log line about a version was the whole symptom.
//
// The guest here is stamped at the DEFAULT and the project pins the other
// version, which is that arrangement exactly.
func TestAGuestBuiltAtAnotherPinIsRefused(t *testing.T) {
	other := otherAPIVersion(t)
	id, _, _ := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "mixed-mod"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	guest := stampedGuest(t, id, factorio.PinExport(factorio.DefaultAPIVersion))
	err := runMod([]string{guest, "-o", out})
	if err == nil {
		t.Fatal("a guest built against API " + factorio.DefaultAPIVersion +
			" bindings was packaged at API " + other + ". Member ids are dense " +
			"sorted indices per description, so every call this guest makes " +
			"reaches a different member -- which is exactly the shipped, silent " +
			"failure the stamp exists to stop")
	}

	// The message has to name BOTH versions and where the packaging one came
	// from. A refusal that says only "mismatch" leaves the reader to work out
	// which of three things chose the pin they did not expect.
	msg := err.Error()
	for _, want := range []string{other, factorio.DefaultAPIVersion, projectFile,
		factorio.PinExport(factorio.DefaultAPIVersion), "--into"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it is not actionable:\n%s",
				want, msg)
		}
	}
	// And nothing may be left behind: a half-written mod directory that the
	// author then finds and runs is worse than the refusal not happening.
	if _, err := os.Stat(filepath.Join(out, "mixed-mod_0.1.0", factorio.APIFile)); err == nil {
		t.Error("the refusal came after a member table was already written")
	}
}

// THE MATCHED PIN STAYS QUIET, which is the control for the test above.
//
// Without it a guard that refused every stamped guest would pass the first half
// and break every real build -- and that is not hypothetical here, because the
// stamp and the pin are compared as MANGLED strings and a mangling that
// disagreed with the generators' would refuse everything.
func TestAGuestBuiltAtThePackagedPinIsAccepted(t *testing.T) {
	other := otherAPIVersion(t)
	id, _, pinnedName := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "matched-mod"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	guest := stampedGuest(t, id, factorio.PinExport(other))
	if err := runMod([]string{guest, "-o", out}); err != nil {
		t.Fatalf("a guest stamped at the pin it is being packaged at was refused: %v", err)
	}
	table := packagedAPITable(t, out, "matched-mod_0.1.0")
	if !strings.Contains(table, fmt.Sprintf("name=%q", pinnedName)) {
		t.Errorf("the accepted package did not carry API %s's table", other)
	}
}

// AN UNSTAMPED GUEST IS STILL PACKAGED, and this is the compatibility half of
// the design rather than a hole left open.
//
// Bindings generated before the stamp existed carry none, and a guest linking
// no generated bindings at all carries none either. Refusing those would break
// every guest built against an older checkout -- including a GA-pinned one with
// nothing wrong with it -- in order to catch a case the module cannot prove.
// `api check` exits non-zero on an incomplete scan because there the
// alternative is calling a guest clean that it could not read; here the
// alternative is refusing a build that is correct.
func TestAnUnstampedGuestIsPackagedUnchanged(t *testing.T) {
	other := otherAPIVersion(t)
	id, _, pinnedName := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "unstamped-mod"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := runMod([]string{callingGuest(t, id), "-o", out}); err != nil {
		t.Fatalf("a guest carrying no pin stamp was refused; every guest built "+
			"against bindings older than the stamp carries none, so this refuses "+
			"builds that are correct: %v", err)
	}
	if !strings.Contains(packagedAPITable(t, out, "unstamped-mod_0.1.0"),
		fmt.Sprintf("name=%q", pinnedName)) {
		t.Errorf("the unstamped package did not carry API %s's table", other)
	}
}

// TWO LINKED BINDING SETS ARE REFUSED, whatever they are pinned at.
//
// A guest may link exactly one. Two sets in one module disagree about what
// every id past their first difference means, and a packaged table can match at
// most one of them -- so the other is calling the wrong member no matter which
// pin is chosen. This is reachable in Rust, where a second fkapi is just
// another path dependency; in Go the generated package also carries
// fk_scratch_base and friends, so a second copy collides at link time first.
func TestAGuestLinkingTwoBindingSetsIsRefused(t *testing.T) {
	other := otherAPIVersion(t)
	id, _, _ := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	out := filepath.Join(dir, "out")
	guest := stampedGuest(t, id, factorio.PinExport(factorio.DefaultAPIVersion),
		factorio.PinExport(other))
	err := runMod([]string{guest, "--name", "two-mod", "--version", "0.1.0", "-o", out})
	if err == nil {
		t.Fatal("a guest linking two generated binding sets was packaged; one of " +
			"them is calling the wrong member whichever pin the table came from")
	}
	if !strings.Contains(err.Error(), other) ||
		!strings.Contains(err.Error(), factorio.DefaultAPIVersion) {
		t.Errorf("the refusal does not name both pins: %v", err)
	}
}

// --into REPINS A VENDORED CHECKOUT, which is the supported form of the recipe
// the downstream mod had to work out by hand.
//
// A consumer vendors a FkLua checkout and pins something other than the default.
// The library packages inside that checkout's guest module -- fkipc above all --
// import ITS committed fkapi, not the consumer's, so unless that copy is
// regenerated at the consumer's pin the guest calls the wrong members. There was
// no supported way to say so: the recipe was `gen-bindings -o <path>` per
// language, which is undocumented, silently Go-only if you forget Rust, and has
// to be redone after every resync because a re-extract restores upstream's file.
func TestGenBindingsIntoRepinsAVendoredCheckout(t *testing.T) {
	other := otherAPIVersion(t)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	if err := os.WriteFile(projectFile, []byte(`[mod]
name = "consumer"
version = "0.1.0"

[fklua]
api = "`+other+`"
lang = ["go", "rust"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The vendored snapshot as a resync leaves it: bindings at the DEFAULT pin,
	// which is what upstream commits.
	checkout := filepath.Join(dir, "vendor", "fklua")
	for _, p := range []string{GoBindingsPath, RustBindingsPath} {
		full := filepath.Join(checkout, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := runGenBindings([]string{"--into", checkout}); err != nil {
		t.Fatalf("--into refused a vendored checkout: %v", err)
	}

	// BOTH LANGUAGES, from the manifest's lang list -- the hand recipe's whole
	// failure mode was doing one of them.
	stamp := factorio.PinExport(other)
	for _, p := range []string{GoBindingsPath, RustBindingsPath} {
		b, err := os.ReadFile(filepath.Join(checkout, p))
		if err != nil {
			t.Fatalf("%s was not written into the checkout: %v", p, err)
		}
		if !strings.Contains(string(b), stamp) {
			t.Errorf("%s in the checkout does not carry API %s's pin stamp %q, so "+
				"`fklua mod` will refuse the guest that links it", p, other, stamp)
		}
	}

	// AND NOTHING ELSE. The crate manifest and root are static, so they say
	// nothing about the pin; writing them would turn a repin into a partial
	// overwrite of somebody's vendored snapshot.
	for _, p := range []string{RustCratePath, RustCrateLibPath} {
		if _, err := os.Stat(filepath.Join(checkout, p)); err == nil {
			t.Errorf("--into wrote %s, which is not pin-dependent and belongs to "+
				"the vendored snapshot", p)
		}
	}
	// The census is a fact about the checkout that owns api/, and this project
	// is not it.
	if _, err := os.Stat(filepath.Join(checkout, "api")); err == nil {
		t.Error("--into wrote a census into the checkout it was repinning")
	}

	// --check is then the standing gate, and it must pass on what was just
	// written and fail on a resync that restored upstream's copy.
	if err := runGenBindings([]string{"--into", checkout, "--check"}); err != nil {
		t.Fatalf("--check refused the bindings --into had just written: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, GoBindingsPath),
		[]byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runGenBindings([]string{"--into", checkout, "--check"})
	if err == nil {
		t.Fatal("--check passed a checkout whose committed bindings are not at " +
			"this project's pin, which is the state every resync restores")
	}
	// The advice has to name --into: sending the reader to the bare command
	// would regenerate THIS project's bindings, which are not what failed.
	if !strings.Contains(err.Error(), "--into") {
		t.Errorf("the refusal tells the reader to re-run something that would not "+
			"fix it: %v", err)
	}
}

// -o AND --into CANNOT BOTH SAY WHERE OUTPUT GOES. -o names one file for one
// language; --into names a checkout for every language the manifest declares.
func TestGenBindingsRefusesBothOutputFlags(t *testing.T) {
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	out := filepath.Join(dir, "api.go")
	err := runGenBindings([]string{"--lang=go", "-o", out, "--into", dir})
	if err == nil {
		t.Fatal("-o and --into were both accepted; one of them silently wins")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("the refusal came after something was already written")
	}
}
