package factorio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A mod project's manifest and lockfile.
//
// `fklua.toml` is what an author writes; `fklua.lock` is what the toolchain
// derives from it. The split is the usual one and it earns its keep here for a
// specific reason: the bindings a project builds against are GENERATED, so
// "which API did this come from" is not answerable from the source tree unless
// something records it.
//
// The lock answers three questions a mod author will eventually have:
//
//   - Which Factorio API version are my bindings actually from? (`api`)
//   - Did anyone edit the generated tree by hand? (`bindings_sha256`)
//   - Would regenerating produce something different? (`api_sha256`)
//
// Deliberately NOT a dependency resolver. There is one dependency, it is a JSON
// file, and the version is written down. Anything more would be machinery in
// search of a problem.

// Project is a parsed fklua.toml.
type Project struct {
	Name            string
	Version         string
	Title           string
	Author          string
	Description     string
	FactorioVersion string
	// API is the runtime-api.json version the bindings are generated from.
	API string
	// Langs are the guest languages to generate for.
	Langs []string
	// GC is how the guest's own heap is managed: "collected" for a TinyGo
	// -gc=custom build that imports guest/go/fkgc, "leaking" for everything
	// else. `fklua mod` and `fklua compile` read it the way they read identity,
	// and --gc on the command line overrides it.
	//
	// A STRING RATHER THAN A luagen.GCMode, deliberately. internal/factorio does
	// not import internal/luagen and should not start over a config key: the
	// manifest describes a project and the compiler's enum is where the spelling
	// is validated. cmd/fklua parses it with the same luagen.ParseGCMode the
	// flag uses, so a typo in the file and a typo on the command line produce the
	// same message.
	//
	// EMPTY IS "ABSENT", NOT "leaking", and that distinction is the whole of the
	// backward compatibility here. A project written before this key existed has
	// no `gc` line, so nothing in this struct overrides the command's own
	// default and its build is byte-for-byte what it was.
	GC string
	// Data is a directory whose contents are copied into the packaged mod --
	// the DATA STAGE: data.lua, prototypes/, graphics/, locale/. The default
	// for `fklua mod --include`, which is the mechanism; see Package.Include.
	Data string
	// DataModule is a second wasm module, built from its own package or crate --
	// a Go main package, a Rust cdylib -- that runs at Factorio's SETTINGS and
	// DATA stages. The default for `fklua mod --data-module`, which is the
	// mechanism.
	//
	// NAMING BOTH LANGUAGES IS LOAD-BEARING. The main-package-only phrasing
	// this replaces was right while Go was the only language that could have a
	// data module, and `fklua init --lang rust --data` is what made it wrong:
	// a Rust project has no main package, and this sentence is copied verbatim
	// into the manifest that scaffold writes. It has five copies -- here, the
	// manifest text below, `fklua mod --help`, runModWith's data-stage comment
	// and mod.go's DataChunk -- and a test holds all five to a phrasing that
	// names both, which is also why the superseded wording is not quoted
	// anywhere in them.
	//
	// EMPTY IS "no data guest", and every project written before this key
	// existed has no line for it -- so `fklua mod` over one emits exactly what
	// it emitted before, which is the same backward-compatibility rule `gc`
	// follows and is gated by a byte-identity test.
	DataModule string
	// DataModuleAlt is the OTHER language's data artifact, written as a COMMENT
	// beside data_module and never as a key.
	//
	// WRITE-ONLY, and deliberately. A mod has one data stage, so the manifest
	// has one `data_module`; what a two-language project needs is not a second
	// key but a note saying where the other build lands, so that swapping which
	// one ships is editing a line the file already carries. ParseProject never
	// sets it -- reading a comment back would invent a second declaration out of
	// one -- so `fklua meta` reports the key and not this.
	DataModuleAlt string
	// Stages is the [stages] section: an ordered list of require paths per
	// stage, one entry of which may be "@guest".
	//
	// It is a RAMP whose destination is an empty section. A mod moving its data
	// stage into Go does it a file at a time, and this is what lets the guest
	// and the remaining hand-written Lua sit in one stage file in an order the
	// author states -- which is all data.lua has ever been. When the last
	// require goes the key goes, and an absent key with the hook exported means
	// ["@guest"].
	//
	// A DECLARED key with an empty list is not the same as an ABSENT one, so
	// this map is read for presence rather than for length.
	Stages map[string][]string
	// Scenarios is the [scenarios] section: an ordered list of require paths per
	// SCENARIO the mod ships, one entry of which may be "@control".
	//
	// A scenario's control.lua is a full control stage in its own Lua state, and
	// the base game's own convention for a mod-shipped scenario is a one-line
	// require into the mod's tree -- which is exactly the file `fklua mod`
	// already writes for the mod root. So this is a packaging key rather than a
	// second compiler: it says which scenario directories to put that line in.
	//
	// An ABSENT key generates nothing, so a project written before it existed
	// emits byte for byte what it emitted before -- the same rule `gc` and
	// `data_module` follow, gated by the same byte-identity test.
	Scenarios map[string][]string
	// Dependencies reach info.json verbatim, in Factorio's own syntax:
	// "base >= 2.0.0" for a hard dependency, "? other-mod" for an optional
	// one, "! conflicting-mod" for an incompatibility. Not parsed here --
	// Factorio is the authority on its own grammar and a half-understanding of
	// it would reject strings the game accepts.
	Dependencies []string
}

// Lock is a parsed fklua.lock: what the last generation actually used.
type Lock struct {
	API string
	// APISHA256 is over the runtime-api.json itself. If this moves, the
	// description changed underneath us -- which should be impossible for a
	// pinned version and is worth screaming about if it happens.
	APISHA256 string
	// BindingsSHA256 is over the generated tree. If this moves without the API
	// moving, someone edited generated code by hand.
	BindingsSHA256 string
	// Fklua is the compiler version that generated it.
	Fklua string
}

// ParseProject reads an fklua.toml.
//
// A hand-rolled reader rather than a TOML library, and the reason is the
// project's standing one: go.mod requires exactly watgo, and a config file with
// six keys does not justify breaking that. The subset is flat sections and
// `key = "value"` or `key = ["a", "b"]`, which is all the format above uses.
// Anything outside it is an ERROR rather than ignored -- a typo'd key silently
// doing nothing is how a pin stops pinning. The single exception is the
// reserved `[tool]` / `[tool.<name>]` namespace, which belongs to external
// tools and is skipped entirely; see the comment at the skip.
func ParseProject(src string) (Project, error) {
	var p Project
	section := ""
	for i, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		// `[tool]` AND `[tool.<name>]` BELONG TO SOMEBODY ELSE. The hard-error
		// rule above is what keeps a typo from silently doing nothing, and it
		// also means an external driver has nowhere to put its own settings --
		// which is why one such tool carried a second sidecar file next to a
		// manifest that already described the project. This is the one hole in
		// the rule, and it is a hole with a name on it: everything between such
		// a header and the next section header is skipped wholesale, so a tool
		// may keep richer TOML in there than the flat subset fklua reads, and
		// `Project.TOML()` never writes one. The prefix is `tool.` WITH the dot
		// plus the exact name `tool`, so `[tools]` and `[toolbox]` are still
		// errors.
		//
		// The one caveat is that this reader is LINE-BASED: a line inside a tool
		// section that itself looks like `[section]` ends the tool section, the
		// same way it would anywhere else. The subset's grammar is unchanged.
		if section == "tool" || strings.HasPrefix(section, "tool.") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return p, fmt.Errorf("line %d: expected `key = value`, got %q", i+1, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		if strings.HasPrefix(val, "[") {
			items, err := parseTOMLList(val)
			if err != nil {
				return p, fmt.Errorf("line %d: %w", i+1, err)
			}
			if section == "fklua" && key == "lang" {
				p.Langs = items
				continue
			}
			if section == "mod" && key == "dependencies" {
				p.Dependencies = items
				continue
			}
			// [stages] IS THE ONE SECTION WHOSE KEYS ARE ALL LISTS, and the
			// four names are the four stages Factorio has -- there is no fifth
			// to grow into, so an unknown key here is a typo rather than a
			// version this build is too old for.
			//
			// An EMPTY list is stored as a non-nil empty slice, because the
			// difference between "declared as nothing" (a stage file with no
			// requires) and "not declared" (no stage file at all) is read by
			// presence in this map and nil would erase it.
			if section == "stages" {
				if _, ok := StageHookByKey(key); !ok {
					return p, fmt.Errorf("line %d: unknown key %q in [stages]; the "+
						"stages are %s", i+1, key, strings.Join(StageKeys(), ", "))
				}
				if p.Stages == nil {
					p.Stages = map[string][]string{}
				}
				if items == nil {
					items = []string{}
				}
				p.Stages[key] = items
				continue
			}
			// [scenarios] IS THE ONE SECTION WHOSE KEYS ARE THE AUTHOR'S OWN, so
			// unlike [stages] there is nothing to check a key against here: a
			// scenario is named whatever the author calls it. What the name has to
			// be is checked at PACKAGE time, where it becomes a directory --
			// scenarioNameRE -- rather than here, so the manifest reader stays a
			// reader.
			if section == "scenarios" {
				if p.Scenarios == nil {
					p.Scenarios = map[string][]string{}
				}
				if items == nil {
					items = []string{}
				}
				p.Scenarios[key] = items
				continue
			}
			return p, fmt.Errorf("line %d: unknown list key %q in [%s]", i+1, key, section)
		}
		val = strings.Trim(val, `"`)

		switch section + "." + key {
		case "mod.name":
			p.Name = val
		case "mod.version":
			p.Version = val
		case "mod.title":
			p.Title = val
		case "mod.author":
			p.Author = val
		case "mod.description":
			p.Description = val
		case "mod.factorio_version":
			p.FactorioVersion = val
		case "mod.data":
			p.Data = val
		case "fklua.api":
			p.API = val
		case "fklua.gc":
			p.GC = val
		case "fklua.data_module":
			p.DataModule = val
		default:
			return p, fmt.Errorf("line %d: unknown key %q in [%s]", i+1, key, section)
		}
	}
	if p.Name == "" {
		return p, fmt.Errorf("[mod] name is required")
	}
	if p.API == "" {
		return p, fmt.Errorf("[fklua] api is required: pin the version the bindings come from")
	}
	if len(p.Langs) == 0 {
		p.Langs = []string{"go"}
	}
	return p, nil
}

func parseTOMLList(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("expected a [\"list\"], got %q", s)
	}
	var out []string
	for _, part := range strings.Split(strings.Trim(s, "[]"), ",") {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out, nil
}

// TOML renders a project back, for `fklua init`.
func (p Project) TOML() string {
	var b strings.Builder
	b.WriteString("# What this mod is. These become info.json.\n[mod]\n")
	fmt.Fprintf(&b, "name = %q\n", p.Name)
	fmt.Fprintf(&b, "version = %q\n", p.Version)
	if p.Title != "" {
		fmt.Fprintf(&b, "title = %q\n", p.Title)
	}
	if p.Author != "" {
		fmt.Fprintf(&b, "author = %q\n", p.Author)
	}
	fmt.Fprintf(&b, "factorio_version = %q\n", p.FactorioVersion)
	if p.Description != "" {
		fmt.Fprintf(&b, "description = %q\n", p.Description)
	}
	if p.Data != "" {
		b.WriteString("# Copied into the packaged mod: data.lua, prototypes/, graphics/, locale/.\n")
		fmt.Fprintf(&b, "data = %q\n", p.Data)
	}
	if len(p.Dependencies) > 0 {
		b.WriteString("# info.json dependencies, in Factorio's own syntax:\n")
		b.WriteString("#   \"base >= 2.0.0\"  required   \"? other\"  optional   \"! other\"  conflicts\n")
		var ds []string
		for _, d := range p.Dependencies {
			ds = append(ds, fmt.Sprintf("%q", d))
		}
		fmt.Fprintf(&b, "dependencies = [%s]\n", strings.Join(ds, ", "))
	}
	b.WriteString("\n# The API version the generated bindings come from.\n")
	b.WriteString("# PINNED: `fklua gen-bindings --check` fails if the tree drifts from it,\n")
	b.WriteString("# and `fklua api check --to <newer>` says whether an upgrade would break\n")
	b.WriteString("# anything this mod actually calls.\n[fklua]\n")
	fmt.Fprintf(&b, "api = %q\n", p.API)
	var qs []string
	for _, l := range p.Langs {
		qs = append(qs, fmt.Sprintf("%q", l))
	}
	fmt.Fprintf(&b, "lang = [%s]\n", strings.Join(qs, ", "))
	if p.GC != "" {
		// THE KEY IS WRITTEN WITH ITS REASON, because this is the one line in
		// the file that has to agree with how the guest was BUILT. `fklua mod`
		// refuses --gc=collected for a module that does not export the
		// collector surface, so a `gc = "collected"` here plus a
		// `tinygo -gc=leaking` there is a build error rather than a mod that
		// quietly does not collect -- which is the right failure and is still
		// a confusing one to meet without this comment in front of it.
		//
		// IT SAID RUST HAD NO COLLECTOR FOR FOUR ROUNDS AFTER guest/rust/fkgc
		// LANDED, while init's own printed next-steps in the same command said
		// the opposite and named the feature. The manifest is the first file a
		// new author reads, and it was telling them their scaffold was
		// misconfigured when it was not. Filed by WormholeBelts, which rewrote
		// the comment in place. Both languages are named here now, so the two
		// halves of one command cannot disagree again without this line
		// changing too.
		b.WriteString("\n# How the GUEST's own heap is managed, and it must match how the guest\n")
		b.WriteString("# was BUILT. \"collected\" means, in Go, `tinygo -gc=custom` plus an import\n")
		b.WriteString("# of guest/go/fkgc (`fklua init` scaffolded both) and, in Rust,\n")
		b.WriteString("# `cargo build --features fk/fkgc` with no import and no second flag,\n")
		b.WriteString("# because the fk crate owns the single #[global_allocator] site.\n")
		b.WriteString("# `fklua mod` refuses \"collected\" for a module that exports no collector,\n")
		b.WriteString("# so a mismatch is a build error and never a mod that silently fails to\n")
		b.WriteString("# collect.\n")
		b.WriteString("# \"leaking\" is the expert path -- correct for an allocation-disciplined\n")
		b.WriteString("# guest, and the only option for wasip1.\n")
		b.WriteString("# See agents/guests.md, \"the guest heap budget\".\n")
		fmt.Fprintf(&b, "gc = %q\n", p.GC)
	}
	if p.DataModule != "" {
		b.WriteString("\n# A SECOND wasm module, run at Factorio's settings and data stages.\n")
		b.WriteString("# It is built from its own package or crate -- a Go main package, a\n")
		b.WriteString("# Rust cdylib -- and must not import the generated fkapi bindings:\n")
		b.WriteString("# those stages have no runtime API.\n")
		b.WriteString("# fklua generates a stage file for each hook the module exports:\n")
		b.WriteString("#   fk_settings -> settings.lua           fk_data -> data.lua\n")
		b.WriteString("#   fk_data_updates -> data-updates.lua   fk_data_final_fixes -> data-final-fixes.lua\n")
		b.WriteString("# It is also compiled --persist=none and -gc=leaking whatever the control\n")
		b.WriteString("# guest uses: it runs once at load and dies with the Lua state that built\n")
		b.WriteString("# it. `fklua mod` refuses a data module that carries a collector.\n")
		if p.DataModuleAlt != "" {
			// ONE KEY, HOWEVER MANY LANGUAGES, because a mod has one data
			// stage. The other artifact is named rather than dropped, so
			// switching which language ships is editing this line instead of
			// working the path out again.
			b.WriteString("# This project declares more than one guest language and a mod has ONE\n")
			b.WriteString("# data stage, so the key names one build. The other lands at:\n")
			fmt.Fprintf(&b, "#   %s\n", p.DataModuleAlt)
		}
		fmt.Fprintf(&b, "data_module = %q\n", p.DataModule)
	}
	if len(p.Scenarios) > 0 {
		b.WriteString("\n# Scenarios this mod ships. Each key is a directory under\n")
		b.WriteString("# scenarios/, and each entry is one require in the generated\n")
		b.WriteString("# control.lua for it, with \"@control\" standing for this mod's own\n")
		b.WriteString("# control stage. A scenario's control.lua is a full control stage in\n")
		b.WriteString("# its own Lua state, so the shim is what connects it to the guest.\n")
		b.WriteString("[scenarios]\n")
		for _, name := range sortedScenarioNames(p.Scenarios) {
			var qs []string
			for _, e := range p.Scenarios[name] {
				qs = append(qs, fmt.Sprintf("%q", e))
			}
			fmt.Fprintf(&b, "%s = [%s]\n", name, strings.Join(qs, ", "))
		}
	}
	if len(p.Stages) > 0 {
		b.WriteString("\n# The order each stage file loads things in, one entry per require,\n")
		b.WriteString("# with \"@guest\" standing for this mod's own data-stage hook. A RAMP:\n")
		b.WriteString("# a key here is only needed while hand-written Lua is still in the\n")
		b.WriteString("# chain, and the destination is an empty section.\n")
		b.WriteString("[stages]\n")
		// STAGE ORDER, NOT MAP ORDER. Go randomizes a map walk, and a manifest
		// that reordered itself on every write would make every regeneration a
		// diff.
		for _, h := range StageHooks {
			chain, ok := p.Stages[h.Key]
			if !ok {
				continue
			}
			var qs []string
			for _, e := range chain {
				qs = append(qs, fmt.Sprintf("%q", e))
			}
			fmt.Fprintf(&b, "%s = [%s]\n", h.Key, strings.Join(qs, ", "))
		}
	}
	return b.String()
}

// Text renders a lockfile.
//
// Generated, and it says so: the header is the first thing a reader sees and
// the first thing a reviewer needs, because a hand-edited lock is a lock that
// lies.
func (l Lock) Text() string {
	var b strings.Builder
	b.WriteString("# Generated by fklua. Do not edit.\n")
	b.WriteString("# Regenerate with `fklua lock`; verify with `fklua gen-bindings --check`.\n")
	fmt.Fprintf(&b, "api = %q\n", l.API)
	fmt.Fprintf(&b, "api_sha256 = %q\n", l.APISHA256)
	fmt.Fprintf(&b, "bindings_sha256 = %q\n", l.BindingsSHA256)
	fmt.Fprintf(&b, "fklua = %q\n", l.Fklua)
	return b.String()
}

// ParseLock reads a lockfile back.
func ParseLock(src string) (Lock, error) {
	var l Lock
	for i, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return l, fmt.Errorf("line %d: expected `key = value`, got %q", i+1, line)
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(key) {
		case "api":
			l.API = val
		case "api_sha256":
			l.APISHA256 = val
		case "bindings_sha256":
			l.BindingsSHA256 = val
		case "fklua":
			l.Fklua = val
		default:
			return l, fmt.Errorf("line %d: unknown key %q", i+1, key)
		}
	}
	return l, nil
}

// HashFile is the sha256 of one file, hex-encoded.
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// HashTree is the sha256 over a set of files, path and contents both.
//
// Paths are included and the list is sorted, so renaming a generated file
// changes the hash and filesystem order never does. A hash that depended on
// readdir order would differ between machines and the lock would be useless.
func HashTree(paths []string) (string, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, p := range sorted {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(p), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
