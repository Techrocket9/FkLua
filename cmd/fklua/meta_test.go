package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// `fklua meta --json` is the project's DATA INTERFACE, and what these tests
// hold is the two halves of that: the document's shape (every key present, no
// nulls where a list or a section belongs) and the DEFAULT RULES it reports.
//
// The second half is the reason the command exists. An external driver used to
// re-implement those rules against its own lenient reader of fklua.toml, and
// one of them it got backwards: an absent `gc` key means LEAKING, and the
// driver assumed collected because `fklua init` writes `gc = "collected"` and a
// manifest without the key looks newer rather than older. So the minimal-
// manifest test below asserts that field loudly and by itself; a second
// implementation of a rule is a second chance to get it wrong, and this
// document is what retires the second implementation.

// writeManifest drops an fklua.toml into a fresh directory and enters it,
// returning the way back. Every test here reads the working directory, because
// that is the only place `meta` looks.
func metaProjectDir(t *testing.T, manifest string) func() {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return chdir(t, dir)
}

// metaRun runs the command and returns both the raw stdout and the parsed
// document. BOTH, deliberately: some promises here are about the TEXT (an empty
// list is `[]` and not `null`) and survive no round trip through a decoder.
func metaRun(t *testing.T, args ...string) (string, metaDoc) {
	t.Helper()
	out := captureStdout(t, func() error { return runMeta(args) })
	var doc metaDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("meta --json did not print one JSON document (%v):\n%s", err, out)
	}
	return out, doc
}

// A manifest with EVERY key set: the raw block is what the author wrote, the
// effective block is the same because nothing was left to default, and the
// package identity and both guest layouts are computed from the name.
//
// The mod name carries a DASH on purpose. A cargo cdylib is named after the
// [lib] name with dashes mapped to underscores, so "meta-full-mod" is the one
// input that tells `meta-full-mod-guest` (the crate) apart from
// `meta_full_mod_guest.wasm` (the artifact) -- and getting that wrong would
// hand a driver a path to a file that is never written.
func TestMetaDescribesAFullManifestAsWrittenAndAsItWillBeUsed(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-full-mod"
version = "1.2.3"
title = "Meta Full Mod"
author = "someone"
description = "every key set"
factorio_version = "2.1"
data = "mod-data"
dependencies = ["base >= 2.0.0", "? quality"]

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
lang = ["go", "rust"]
gc = "collected"
data_module = "dist/data.wasm"

# The reserved namespace, which meta must not so much as notice. It was added
# for exactly this kind of driver, and a tool's own settings appearing in (or
# disturbing) the document would make the namespace a liability.
[tool.fmtk]
debug_port = 34197

[scenarios]
freeplay-plus = ["@control"]

[stages]
data = ["prototypes.entity", "@guest"]
`)()

	out, doc := metaRun(t, "--json")

	// The top-level key set is the contract, so it is checked as a SET rather
	// than field by field: a key that quietly stopped being emitted would pass
	// every assertion below it.
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"fklua", "manifest", "effective", "package", "guest"} {
		if _, ok := top[k]; !ok {
			t.Errorf("the document has no %q key", k)
		}
	}
	if len(top) != 5 {
		t.Errorf("the document has %d top-level keys, want 5: %v", len(top), top)
	}
	if doc.Fklua != fkluaVersion {
		t.Errorf(`"fklua" is %q, want the compiler version %q`, doc.Fklua, fkluaVersion)
	}

	// Raw is what the author wrote.
	m := doc.Manifest
	for _, c := range []struct{ field, have, want string }{
		{"name", m.Name, "meta-full-mod"},
		{"version", m.Version, "1.2.3"},
		{"title", m.Title, "Meta Full Mod"},
		{"author", m.Author, "someone"},
		{"description", m.Description, "every key set"},
		{"factorio_version", m.FactorioVersion, "2.1"},
		{"data", m.Data, "mod-data"},
		{"api", m.API, factorio.DefaultAPIVersion},
		{"gc", m.GC, "collected"},
		{"data_module", m.DataModule, "dist/data.wasm"},
	} {
		if c.have != c.want {
			t.Errorf("manifest.%s = %q, want %q", c.field, c.have, c.want)
		}
	}
	// The dependency strings reach a consumer verbatim, `>=` and all. Go's
	// marshaller escapes `>` as \u003e in the text, which is the same string
	// after any decoder and is checked here rather than assumed.
	if got := strings.Join(m.Dependencies, "|"); got != "base >= 2.0.0|? quality" {
		t.Errorf("manifest.dependencies = %q, want the two written verbatim", got)
	}
	if got := strings.Join(m.Lang, ","); got != "go,rust" {
		t.Errorf("manifest.lang = %q, want \"go,rust\"", got)
	}
	if got := strings.Join(m.Stages["data"], "|"); got != "prototypes.entity|@guest" {
		t.Errorf("manifest.stages[data] = %q, want the chain as written", got)
	}
	if got := strings.Join(m.Scenarios["freeplay-plus"], "|"); got != "@control" {
		t.Errorf("manifest.scenarios[freeplay-plus] = %q, want \"@control\"", got)
	}

	// Nothing was left to default, so effective is raw. Compared as a whole:
	// the two blocks are one struct on purpose, and a default that fired where
	// it should not have would show up nowhere else.
	rawJSON, _ := json.Marshal(doc.Manifest)
	effJSON, _ := json.Marshal(doc.Effective)
	if string(rawJSON) != string(effJSON) {
		t.Errorf("a manifest with every key set has an effective block that differs "+
			"from the raw one, so some default fired that should not have:\n"+
			"raw  %s\neffective %s", rawJSON, effJSON)
	}

	// The identity Factorio finds the mod by.
	if doc.Package.Dir != "meta-full-mod_1.2.3" {
		t.Errorf("package.dir = %q, want \"meta-full-mod_1.2.3\"", doc.Package.Dir)
	}
	if doc.Package.Zip != "meta-full-mod_1.2.3.zip" {
		t.Errorf("package.zip = %q, want \"meta-full-mod_1.2.3.zip\"", doc.Package.Zip)
	}

	// Both guests, spelled from the scaffold's own constants.
	if doc.Guest.Go == nil {
		t.Fatal("guest has no \"go\" entry for a project whose lang says go")
	}
	for _, c := range []struct{ field, have, want string }{
		{"dir", doc.Guest.Go.Dir, "guest/go"},
		{"wasm", doc.Guest.Go.Wasm, "meta-full-mod.wasm"},
		{"bindings", doc.Guest.Go.Bindings, "guest/go/fkapi/fkapi.go"},
	} {
		if c.have != c.want {
			t.Errorf("guest.go.%s = %q, want %q", c.field, c.have, c.want)
		}
	}
	if doc.Guest.Rust == nil {
		t.Fatal("guest has no \"rust\" entry for a project whose lang says rust")
	}
	for _, c := range []struct{ field, have, want string }{
		{"dir", doc.Guest.Rust.Dir, "guest/rust"},
		{"crate", doc.Guest.Rust.Crate, "meta-full-mod-guest"},
		{"crate_dir", doc.Guest.Rust.CrateDir, "guest/rust/meta-full-mod-guest"},
		// The dash-to-underscore mangling: the crate is spelled one way and the
		// file cargo writes is spelled the other.
		{"wasm", doc.Guest.Rust.Wasm,
			"guest/rust/target/wasm32-unknown-unknown/release/meta_full_mod_guest.wasm"},
		{"bindings", doc.Guest.Rust.Bindings, "guest/rust/fkapi/src/api.rs"},
	} {
		if c.have != c.want {
			t.Errorf("guest.rust.%s = %q, want %q", c.field, c.have, c.want)
		}
	}

	// The reserved namespace leaves no trace. Checked against the TEXT, because
	// a stray key would not appear in any struct field to compare.
	if strings.Contains(out, "fmtk") || strings.Contains(out, "34197") {
		t.Errorf("a [tool.fmtk] section reached the document; those sections "+
			"belong to their tools:\n%s", out)
	}
}

// THE MINIMAL MANIFEST IS WHERE EVERY DEFAULT RULE IS VISIBLE, and where the
// one that caused a real downstream bug is asserted on its own.
//
// name, version and api are the whole file. Everything else in `effective` is
// a rule this command owes its callers, and `gc` is the one a driver got
// backwards: absent means LEAKING. `fklua init` writes `gc = "collected"` into
// a new project, so a manifest without the key reads as newer rather than
// older, and the consumer that guessed collected told its users their heap was
// being reclaimed while it doubled.
func TestMetaSaysAnAbsentGCKeyMeansLeaking(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-min-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
`)()

	out, doc := metaRun(t, "--json")

	// THE FIELD THIS COMMAND EXISTS FOR.
	if doc.Effective.GC != "leaking" {
		t.Fatalf("effective.gc = %q for a manifest with NO gc key, want \"leaking\". "+
			"An absent key is not \"collected\": `fklua mod` leaves its leaking "+
			"default alone, and a consumer that reads this the other way tells "+
			"its users a doubling heap is being collected", doc.Effective.GC)
	}
	// ...and the raw block still says the author wrote nothing, so a driver can
	// tell "leaking by default" from "leaking on purpose".
	if doc.Manifest.GC != "" {
		t.Errorf("manifest.gc = %q, want the empty string the author wrote", doc.Manifest.GC)
	}

	if doc.Effective.Title != "meta-min-mod" {
		t.Errorf("effective.title = %q, want the name it falls back to", doc.Effective.Title)
	}
	if doc.Effective.Author != "unknown" {
		t.Errorf("effective.author = %q, want %q", doc.Effective.Author, "unknown")
	}
	if doc.Effective.FactorioVersion != factorio.DefaultFactorioVersion {
		t.Errorf("effective.factorio_version = %q, want the default pin's series %q",
			doc.Effective.FactorioVersion, factorio.DefaultFactorioVersion)
	}
	if got := strings.Join(doc.Effective.Lang, ","); got != "go" {
		t.Errorf("effective.lang = %q, want \"go\"", got)
	}

	// AN EMPTY LIST IS `[]` AND NOT `null`, everywhere, and it is a promise
	// about the TEXT: a decoder turns both into a nil slice, so the struct
	// cannot tell them apart and the assertion has to read the document.
	if !strings.Contains(out, `"dependencies": []`) {
		t.Errorf("dependencies is not the empty list a consumer can range over "+
			"without checking for null:\n%s", out)
	}
	if strings.Contains(out, "null") {
		t.Errorf("the document contains a null; every absent value is an empty "+
			"string, an empty list or an empty object:\n%s", out)
	}
	for _, want := range []string{`"stages": {}`, `"scenarios": {}`} {
		if !strings.Contains(out, want) {
			t.Errorf("an undeclared section is not %s:\n%s", want, out)
		}
	}

	// A Go-only project gets a go entry and NO rust key at all, rather than a
	// rust object full of paths to files nothing will write.
	if doc.Guest.Go == nil {
		t.Error("guest has no \"go\" entry for a project that defaults to go")
	}
	if doc.Guest.Rust != nil {
		t.Errorf("guest has a \"rust\" entry for a Go-only project: %+v", doc.Guest.Rust)
	}
	var top struct {
		Guest map[string]json.RawMessage `json:"guest"`
	}
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top.Guest["rust"]; ok {
		t.Error("the guest object has a \"rust\" key for a Go-only project")
	}
}

// THE ENGINE SERIES IS THE DEFAULT PIN'S, NOT THE PROJECT PIN'S, and that is a
// trap rather than a tidy rule.
//
// `--api` and `--factorio-version` are two axes: the pin says which description
// the bindings came from, the series says which engine info.json declares. They
// DEFAULT TOGETHER only in the sense that DefaultFactorioVersion is
// majorMinor(DefaultAPIVersion) -- runMod seeds info.FactorioVersion with that
// constant and overrides it from the manifest key or the flag alone, so a
// project pinning a 2.1.x description with no factorio_version key still
// declares "2.0" and is refused by a 2.1 engine at game start.
//
// meta reports ACTUAL BEHAVIOUR, so a driver can warn about the pairing. A
// document that helpfully derived the series from the pin instead would be
// describing a mod that is not the one fklua builds.
func TestMetaReportsTheEngineSeriesTheModWillReallyDeclare(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-pinned-mod"
version = "0.1.0"

[fklua]
api = "2.1.16"
`)()

	_, doc := metaRun(t, "--json")

	if doc.Manifest.API != "2.1.16" {
		t.Fatalf("manifest.api = %q, want the pin as written", doc.Manifest.API)
	}
	if doc.Effective.FactorioVersion != factorio.DefaultFactorioVersion {
		t.Errorf("effective.factorio_version = %q for a project pinned at 2.1.16 "+
			"with no factorio_version key, want %q. The pin does not reach this "+
			"field in `fklua mod`, and meta reports what the mod will really "+
			"declare rather than what the two axes suggest",
			doc.Effective.FactorioVersion, factorio.DefaultFactorioVersion)
	}
}

// NO MANIFEST IS AN ERROR, where every sibling command falls back to flags in
// silence. There is nothing to fall back to: the document IS the manifest plus
// what fklua makes of it, so a driver run one directory above its own project
// would otherwise parse a plausible description of nothing.
func TestMetaRefusesToDescribeAProjectThatIsNotThere(t *testing.T) {
	defer chdir(t, t.TempDir())()

	err := runMeta([]string{"--json"})
	if err == nil {
		t.Fatal("meta --json with no manifest succeeded, so a caller in the " +
			"wrong directory gets a document full of defaults describing no project")
	}
	if !strings.Contains(err.Error(), projectFile) {
		t.Errorf("the refusal never names %s, so a caller cannot tell which file "+
			"it needs: %v", projectFile, err)
	}
	if !strings.Contains(err.Error(), "fklua init") {
		t.Errorf("the refusal does not say how to get one: %v", err)
	}
}

// --json IS REQUIRED, and the refusal says so. The flag is the caller's
// statement that stdout is one JSON document, which is what leaves room for a
// human summary later: `fklua meta` on its own can grow into one without
// breaking anything, because nothing was ever allowed to spell it that way.
func TestMetaRefusesWithoutTheJSONFlagBecauseItIsADataInterface(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-flag-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
`)()

	err := runMeta(nil)
	if err == nil {
		t.Fatal("meta with no --json succeeded; the machine spelling has to stay " +
			"explicit or a human form can never be added")
	}
	if !strings.Contains(err.Error(), "--json") {
		t.Errorf("the refusal does not name the flag it wants: %v", err)
	}

	// And an argument it does not know is refused the way every other
	// subcommand refuses one, rather than ignored.
	err = runMeta([]string{"--json", "--jsonn"})
	if err == nil {
		t.Fatal("meta accepted an unknown flag")
	}
	if !strings.Contains(err.Error(), `unknown flag "--jsonn"`) {
		t.Errorf("the refusal does not quote the flag: %v", err)
	}
}

// A VALUE THE TOOLCHAIN WOULD REJECT MUST NEVER REACH A CONSUMER. The whole
// point of this command is that a driver stops re-deriving fklua's rules, so
// handing one a gc mode the compiler refuses -- to be discovered at build time,
// in another process -- would be worse than the sidecar reader it replaced.
//
// The message names the FILE, not a flag: ParseGCMode's own text says "--gc",
// and sending a reader to a command line they did not type is worse than saying
// nothing. It is runMod's wording exactly.
func TestMetaRefusesAGCValueTheCompilerWouldRefuse(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-badgc-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
gc = "collcted"
`)()

	err := runMeta([]string{"--json"})
	if err == nil {
		t.Fatal("meta emitted a document for a gc value `fklua mod` would refuse")
	}
	if !strings.Contains(err.Error(), projectFile) {
		t.Errorf("the refusal never names %s, so it points at a command line the "+
			"author did not type: %v", projectFile, err)
	}
	if !strings.Contains(err.Error(), "collcted") {
		t.Errorf("the refusal does not quote the value that is wrong: %v", err)
	}
}

// The same rule one key over: a lang with no generator is refused here the way
// `fklua lock` refuses it, because there is no guest layout to report for a
// language nothing can generate bindings for.
func TestMetaRefusesALangWithNoGenerator(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-badlang-mod"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
lang = ["go", "zig"]
`)()

	err := runMeta([]string{"--json"})
	if err == nil {
		t.Fatal("meta emitted a document for a lang no generator exists for")
	}
	if !strings.Contains(err.Error(), `no generator for lang "zig"`) {
		t.Errorf("the refusal is not the one `fklua lock` makes: %v", err)
	}
}

// A MALFORMED MANIFEST IS THE PARSE ERROR, as it is everywhere else: meta reads
// the file through the same loadProject every command uses, so a typo'd key
// reports itself once and identically whichever command met it.
func TestMetaReportsAMalformedManifestAsTheParseError(t *testing.T) {
	defer metaProjectDir(t, `
[mod]
name = "meta-broken-mod"
verzion = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
`)()

	err := runMeta([]string{"--json"})
	if err == nil {
		t.Fatal("meta accepted a manifest with an unknown key")
	}
	if !strings.Contains(err.Error(), "verzion") {
		t.Errorf("the refusal does not name the key that is wrong: %v", err)
	}
}
