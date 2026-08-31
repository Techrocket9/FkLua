package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/luagen"
)

// `fklua meta --json`: the project, as one JSON document, for a tool.
//
// A HUMAN-FACING TABLE IS NOT A DATA INTERFACE, and this command is the other
// half of that rule. `api list` grew a `--current` because a bot was reading the
// pin out of a printed table and a legend line broke it; `api check` grew a
// `--json` because a build script should not have to branch on prose. This is
// the same lesson applied to the thing every external driver actually wants,
// which is not one field but the WHOLE PROJECT: what the manifest says, what
// fklua will do with it, and where the guest and its artifacts live.
//
// WHAT IT IS FOR, concretely. The Factorio Mod Toolkit fork drives fklua as a
// subprocess, and to do that it had re-implemented a lenient fklua.toml reader
// plus every default this command's `effective` block computes -- title falling
// back to name, author falling back to "unknown", the engine series, and `gc`.
// A second implementation of a rule is a second chance to get it wrong, and it
// did: the consumer assumed an ABSENT `gc` key meant collected, because
// `fklua init` writes `gc = "collected"` and a project without the key looks
// newer rather than older. It means LEAKING -- the compile-flag default is
// deliberately unchanged so that an existing build never turns into a compile
// error naming a flag its author never chose (see runMod's manifest block) --
// and a driver that guessed the other way told its users their heap was
// collected while it doubled. `effective.gc` is the field this command exists
// for. It is always present, it is always one of "leaking" or "collected", and
// it is computed by the same ParseGCMode call `fklua mod` makes.
//
// WHY AN ABSENT MANIFEST IS AN ERROR HERE, where every sibling command falls
// back to flags in silence. `fklua mod` with no fklua.toml is an ordinary
// invocation: identity comes from the command line and the file is a
// convenience. This command has no command line to fall back TO -- its entire
// output is a description of a manifest -- so the silent fallback would emit a
// document full of defaults describing no project at all, and a consumer that
// ran fklua one directory too high would receive a plausible answer rather than
// a failure. So: no manifest is exit 1, and the message says which file was
// missing and what to do about it.
//
// WHY THE GUEST PATHS COME FROM THE SCAFFOLD'S OWN CONSTANTS. guest/go moved
// from guest/ and guest/rust moved from wherever it was first written, both
// times because three separate mods had already moved them by hand; the
// bindings paths are hashed BY EXACT NAME by `fklua lock`. A second spelling of
// any of those here would be a fourth place to fix on the next move and the one
// nothing would notice, because a JSON field that is merely wrong still parses.
// Every path below is built from guestDir, GoBindingsPath, rustGuestDir,
// rustCrateDir, RustGuestArtifact and RustBindingsPath, so this command cannot
// drift from `init` and `gen-bindings` without failing to compile.
//
// STDOUT IS EXACTLY ONE DOCUMENT and nothing else. Diagnostics, if there were
// ever any, go to stderr; there are none.

// metaDoc is the whole document. The top-level keys are all always present, so
// a consumer can index without checking, and the field names are stable in the
// same sense `api check --json`'s are: the presence and spelling of a key is a
// promise, and anything printed without --json (there is nothing today) is not.
type metaDoc struct {
	// Fklua is the compiler version, the same string `fklua version` prints and
	// the same one a lockfile records.
	Fklua string `json:"fklua"`
	// Manifest is the file AS WRITTEN: raw values, with an empty string, an
	// empty list or an empty object wherever the author wrote nothing.
	//
	// ONE HONEST CAVEAT, and it is `lang`. ParseProject normalizes an absent
	// `lang` to ["go"] at parse time, before this command can see the
	// difference, so the manifest block reports ["go"] for a file with no lang
	// key. Nothing is lost: the effective value is identical either way, and the
	// only fact a consumer cannot recover is whether the author typed it. Said
	// out loud here and in docs/generated-files.md rather than papered over.
	Manifest metaProject `json:"manifest"`
	// Effective is what `fklua mod` would actually use, after every default it
	// applies. Each rule mirrors runMod exactly; see effectiveBlock.
	Effective metaProject `json:"effective"`
	// Package is the identity `fklua mod` computes: the directory (or zip)
	// Factorio expects to find this mod under.
	Package metaPackage `json:"package"`
	// Guest is the per-language layout, keyed by the language, and only the
	// languages `effective.lang` names appear.
	Guest metaGuest `json:"guest"`
}

// metaProject is ONE type for both the manifest block and the effective block,
// deliberately: they are the same field set answering the same questions before
// and after the defaults, and two structs would be two places for a field to be
// added to only one of them. A reader diffing the blocks is the intended use.
type metaProject struct {
	Name            string              `json:"name"`
	Version         string              `json:"version"`
	Title           string              `json:"title"`
	Author          string              `json:"author"`
	Description     string              `json:"description"`
	FactorioVersion string              `json:"factorio_version"`
	Data            string              `json:"data"`
	Dependencies    []string            `json:"dependencies"`
	API             string              `json:"api"`
	Lang            []string            `json:"lang"`
	GC              string              `json:"gc"`
	DataModule      string              `json:"data_module"`
	Stages          map[string][]string `json:"stages"`
	Scenarios       map[string][]string `json:"scenarios"`
}

// metaPackage is what Factorio identifies the built mod by.
type metaPackage struct {
	Dir string `json:"dir"`
	Zip string `json:"zip"`
}

// metaGuest holds one entry per configured language. Absent rather than null
// for a language this project does not build: a consumer iterating the object
// gets the languages that exist, and one indexing "rust" on a Go-only project
// gets a missing key rather than an object full of paths to nothing.
type metaGuest struct {
	Go   *metaGoGuest   `json:"go,omitempty"`
	Rust *metaRustGuest `json:"rust,omitempty"`
}

type metaGoGuest struct {
	Dir      string `json:"dir"`
	Wasm     string `json:"wasm"`
	Bindings string `json:"bindings"`
}

type metaRustGuest struct {
	Dir      string `json:"dir"`
	Crate    string `json:"crate"`
	CrateDir string `json:"crate_dir"`
	Wasm     string `json:"wasm"`
	Bindings string `json:"bindings"`
}

// runMeta writes one JSON document describing the project in the working
// directory.
func runMeta(args []string) error {
	// --json IS REQUIRED, and refusing without it is the point rather than
	// pedantry. The flag is the caller's statement that it expects stdout to be
	// one JSON document, which is what leaves room for a human summary later --
	// `fklua meta` with no flag can grow into one without breaking a single
	// consumer, because no consumer was ever allowed to write it that way.
	wantJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			wantJSON = true
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	if !wantJSON {
		return fmt.Errorf("--json is required: this command is a DATA INTERFACE " +
			"and has no human-facing form. The flag is how a caller says it " +
			"expects stdout to be one JSON document, so that a human summary " +
			"could be added later without breaking anything that parses this " +
			"one. Run `fklua meta --json`")
	}

	// NO MANIFEST IS AN ERROR, where `fklua mod` and `fklua compile` fall back
	// to flags in silence. There is nothing to fall back to here: the document
	// IS the manifest plus what fklua makes of it, so a silent fallback would
	// describe a project that does not exist and a caller one directory above
	// its own project would parse the answer happily. See the file header.
	proj, ok, err := loadProject()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no %s here, and this command describes a project "+
			"rather than guessing one: every value it reports comes from that "+
			"file, so falling back to defaults would answer a question about "+
			"nothing. Run `fklua init <mod-name>`, or run this from the project "+
			"root", projectFile)
	}

	// A VALUE THE TOOLCHAIN ITSELF WOULD REJECT MUST NEVER REACH A CONSUMER.
	// This command's whole purpose is that a driver stops re-deriving fklua's
	// rules, so it cannot hand one a `gc` the compiler refuses or a `lang` no
	// generator exists for and let the driver find out at build time. Both
	// refusals are the ones the commands that DO use these values make, spelled
	// the same way so the message a caller surfaces is the message they would
	// have got from `fklua mod` or `fklua lock`.
	gc := luagen.GCLeaking
	if proj.GC != "" {
		m, err := luagen.ParseGCMode(proj.GC)
		if err != nil {
			// Named as a manifest problem, not a flag problem -- the parser's
			// own message says "--gc", and sending someone to a command line
			// they did not type is worse than saying nothing. runMod's wording,
			// exactly.
			return fmt.Errorf("%s: [fklua] gc: %w", projectFile, err)
		}
		gc = m
	}
	for _, lang := range proj.Langs {
		if lang != "go" && lang != "rust" {
			// buildLock's refusal, verbatim: the languages that have a generator
			// are the languages this document can describe a guest layout for.
			return fmt.Errorf("%s: no generator for lang %q", projectFile, lang)
		}
	}
	// AND factorio_version IS DELIBERATELY NOT A THIRD REFUSAL HERE, even though
	// Package.Validate now rejects one that names a build rather than a series.
	// The line above is drawn where this command's own reading is: `gc` and
	// `lang` are values it must INTERPRET to answer at all -- effective.gc is
	// ParseGCMode's canonical spelling and the guest block exists per language
	// -- so it cannot describe a project whose values it cannot parse. The [mod]
	// block is different: name, version and factorio_version are reported AS
	// WRITTEN, and all three are refused by Package.Validate at package time.
	// Refusing one of the three and not the other two would make this command's
	// rule about the manifest unstateable, and `fklua mod` is the command that
	// says no. A driver that wants the verdict runs it.

	doc := metaDoc{
		Fklua:     fkluaVersion,
		Manifest:  manifestBlock(proj),
		Effective: effectiveBlock(proj, gc),
	}
	// THE PACKAGE IDENTITY IS SPELLED BY THE THING THAT SPELLS IT, rather than
	// by a second `name + "_" + version` here. factorio.Package.DirName is what
	// `fklua mod` writes the directory with, so this is the same fact rather
	// than a copy of it that could go stale. A name may contain spaces (nameRE
	// allows them) and there is nothing to escape: JSON carries them.
	dir := (&factorio.Package{
		Info: factorio.Info{Name: proj.Name, Version: proj.Version},
	}).DirName()
	doc.Package = metaPackage{Dir: dir, Zip: dir + ".zip"}
	doc.Guest = guestBlock(proj)

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// One document, one trailing newline, nothing else on stdout.
	fmt.Printf("%s\n", b)
	return nil
}

// manifestBlock is the manifest as written, with nothing filled in.
//
// The only normalizing done here is nil to empty: an absent list is `[]` and an
// absent section is `{}`, never `null`. That is a shape promise rather than a
// value change -- a consumer ranging over `dependencies` should not have to
// check for null first, and JSON's null is the one value in this document that
// would mean "I could not tell you" rather than "the author wrote nothing".
func manifestBlock(p factorio.Project) metaProject {
	return metaProject{
		Name:            p.Name,
		Version:         p.Version,
		Title:           p.Title,
		Author:          p.Author,
		Description:     p.Description,
		FactorioVersion: p.FactorioVersion,
		Data:            p.Data,
		Dependencies:    listOrEmpty(p.Dependencies),
		API:             p.API,
		// ...the one field that is NOT quite as written. See metaDoc.Manifest.
		Lang:       listOrEmpty(p.Langs),
		GC:         p.GC,
		DataModule: p.DataModule,
		Stages:     sectionOrEmpty(p.Stages),
		Scenarios:  sectionOrEmpty(p.Scenarios),
	}
}

// effectiveBlock is what `fklua mod` would actually use, and every rule in it
// mirrors runMod. Where runMod's rule changes, this changes with it or the
// command is worse than useless: a consumer that trusted a stale rule here is
// exactly the consumer this command was written to retire.
func effectiveBlock(p factorio.Project, gc luagen.GCMode) metaProject {
	m := manifestBlock(p)

	// Defaults that keep info.json valid without making the author type them,
	// in runMod's own order and with its own values.
	if m.Title == "" {
		m.Title = p.Name
	}
	if m.Author == "" {
		m.Author = "unknown"
	}
	// THE DEFAULT PIN'S SERIES, NOT THE PROJECT PIN'S, and this is the one rule
	// here that surprises people -- including whoever writes the next version of
	// this comment. runMod initializes info.FactorioVersion to
	// factorio.DefaultFactorioVersion and overrides it ONLY from the manifest's
	// own `factorio_version` key or from --factorio-version; the project's `api`
	// pin never reaches it. So a project pinning a 2.1.x description with no
	// factorio_version key still declares "2.0", and its mod is refused by a 2.1
	// engine at game start. That is a real trap, and this command reports ACTUAL
	// BEHAVIOUR rather than the behaviour the two axes suggest -- a driver that
	// wants to warn about the pairing needs the value the mod will really carry.
	if m.FactorioVersion == "" {
		m.FactorioVersion = factorio.DefaultFactorioVersion
	}
	// GC IS THE FIELD THIS COMMAND EXISTS FOR. An absent key is LEAKING, never
	// collected: `fklua init` writes `gc = "collected"` into a new project, so a
	// manifest without the key looks newer rather than older and one downstream
	// driver read it that way. The mode has already been through ParseGCMode, so
	// its String() is the canonical spelling of what the compiler will do --
	// which also normalizes the aliases the parser accepts ("none" and "off" are
	// leaking, "custom" is collected).
	m.GC = gc.String()

	// Everything else has no default beyond the one ParseProject already
	// applied: name, version, description, data, api, lang, data_module, stages
	// and scenarios are the manifest's values, so effective equals raw for them.
	// They are carried rather than dropped because the two blocks are one field
	// set on purpose (see metaProject) and a consumer reading only `effective`
	// gets a complete project.
	return m
}

// guestBlock is where a guest lives and what building it produces, per language.
//
// Every path is relative to the project root and forward-slash, on every
// platform: these are wire values a driver joins onto its own paths, not
// something to hand back to the local filesystem layer that produced them.
func guestBlock(p factorio.Project) metaGuest {
	var g metaGuest
	for _, lang := range p.Langs {
		switch lang {
		case "go":
			g.Go = &metaGoGuest{
				Dir: filepath.ToSlash(guestDir),
				// THE CONVENTIONAL ARTIFACT, and it is a convention rather than
				// a computed output path: nothing in fklua builds the Go guest.
				// `init` prints `tinygo build ... -o ../../<name>.wasm` from
				// guest/go, so the wasm lands at the project root under the mod
				// name, and `fklua mod <name>.wasm` is the next line it prints.
				Wasm:     p.Name + ".wasm",
				Bindings: filepath.ToSlash(GoBindingsPath),
			}
		case "rust":
			crate := rustCrateName(p.Name)
			g.Rust = &metaRustGuest{
				Dir:      filepath.ToSlash(rustGuestDir),
				Crate:    crate,
				CrateDir: filepath.ToSlash(rustCrateDir(p.Name)),
				// EXACTLY WHAT init PRINTS AS THE PACKAGING STEP. cargo names a
				// cdylib after the [lib] name with dashes mapped to underscores
				// (RustGuestArtifact), which is the one part of this path nobody
				// guesses right, and the release profile is the one the
				// scaffolded workspace builds with.
				Wasm: filepath.ToSlash(filepath.Join(rustGuestDir, "target",
					"wasm32-unknown-unknown", "release", RustGuestArtifact(p.Name))),
				Bindings: filepath.ToSlash(RustBindingsPath),
			}
		}
	}
	return g
}

// listOrEmpty and sectionOrEmpty are the nil-to-empty rule in one place, so no
// field can be the one that forgot it and marshalled `null`.
func listOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func sectionOrEmpty(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range in {
		out[k] = listOrEmpty(v)
	}
	return out
}
