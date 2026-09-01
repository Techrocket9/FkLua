package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// `fklua init` and `fklua lock`.
//
// fklua.toml is what an author writes and fklua.lock is what the toolchain
// derives. The lock exists because the bindings a project builds against are
// GENERATED: "which API did this come from" is not answerable from the source
// tree unless something records it.

const projectFile = "fklua.toml"
const lockFile = "fklua.lock"

func runInit(args []string) error {
	p := factorio.Project{
		Version:         "0.1.0",
		FactorioVersion: factorio.DefaultFactorioVersion,
		API:             factorio.DefaultAPIVersion,
		Langs:           []string{"go"},
		// Every mod depends on base, so writing the key is what makes it
		// discoverable -- and a key nobody knows exists is the same as one
		// that does not.
		Dependencies: []string{"base >= 2.0.0"},
	}
	// --guest-module points the scaffolded guest at a LOCAL FkLua checkout
	// instead of the published module. Empty is the normal case; see
	// scaffold.go for why the dependency is the one part of this that cannot be
	// made hermetic by cleverness.
	guestModule := ""
	// --no-guest writes the manifest and nothing else, which is what a project
	// that already HAS a guest wants. `init` refuses to overwrite guest source
	// anyway, so this exists for the case where the guest lives somewhere else
	// entirely and a `guest/` directory would be a lie about the layout.
	noGuest := false
	// --library scaffolds a guest LIBRARY rather than a mod -- see library.go.
	// It shares init's name, --lang and --guest-module and nothing else.
	// --data picks the DATA-STAGE flavor (fkdata, the stage contract), from
	// FkRecipes' dogfood report: the control flavor misled a data library's
	// build in both languages.
	library := false
	dataFlavor := false
	apiTyped := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--library":
			library = true
		case args[i] == "--data":
			dataFlavor = true
		case isLangArg(args[i]):
			// Both spellings, for the reason in langArg (main.go).
			v, next, err := langArg(args, i)
			if err != nil {
				return err
			}
			p.Langs, i = strings.Split(v, ","), next
		case args[i] == "--api":
			if i+1 >= len(args) {
				return fmt.Errorf("--api needs a version")
			}
			i++
			p.API = args[i]
			apiTyped = true
		case args[i] == "--guest-module":
			if i+1 >= len(args) {
				return fmt.Errorf("--guest-module needs a path")
			}
			i++
			abs, err := filepath.Abs(args[i])
			if err != nil {
				return err
			}
			// Checked HERE rather than at build time. A `replace` onto a path
			// that does not exist fails inside tinygo, several layers down, in
			// a message about a module rather than about a directory.
			//
			// EITHER LANGUAGE'S SUBSTRATE COUNTS, because one flag has to serve a
			// --lang go,rust project: a checkout keeps guest/go and guest/rust as
			// siblings, so a path at either finds the other (rustSubstrateDir).
			_, goErr := os.Stat(filepath.Join(abs, "go.mod"))
			rustDir := rustSubstrateDir(abs)
			if goErr != nil && rustDir == "" {
				return fmt.Errorf("--guest-module %s: no go.mod and no fk/Cargo.toml "+
					"there. It should point at a FkLua checkout's guest/go or "+
					"guest/rust directory", args[i])
			}
			guestModule = abs
		case args[i] == "--no-guest":
			noGuest = true
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		default:
			p.Name = args[i]
		}
	}
	if p.Name == "" {
		return fmt.Errorf("usage: fklua init <mod-name> [--lang go,rust] [--api VERSION] " +
			"[--guest-module PATH] [--no-guest], or fklua init --library <name> " +
			"[--lang go|rust] [--guest-module PATH]")
	}
	if !library && dataFlavor {
		return fmt.Errorf("--data is --library's flavor flag: a MOD's data stage " +
			"is declared with [fklua] data_module or --data-module on fklua mod, " +
			"not at init")
	}
	if library {
		// A library has no manifest, no pin and no gc key, so the mod-only
		// flags are refused rather than ignored -- the same rule runMod applies
		// to --persist on a data-only packaging.
		if apiTyped {
			return fmt.Errorf("--library takes no --api: a library has no pin of " +
				"its own -- it compiles against its CONSUMER's bindings, which is " +
				"the contract the scaffold's own comments carry")
		}
		if noGuest {
			return fmt.Errorf("--library and --no-guest contradict: the library " +
				"IS the guest source")
		}
		return initLibrary(p.Name, p.Langs, guestModule, dataFlavor)
	}
	if p.Title == "" {
		p.Title = p.Name
	}

	// COLLECTED IS THE DEFAULT FOR A NEW PROJECT, and this line is the whole of
	// it. Everything else in this function and in scaffold.go exists to make the
	// line TRUE -- a `gc = "collected"` key over a guest built -gc=leaking is a
	// refusal at package time, so writing the key obliges init to scaffold a
	// guest that carries a collector.
	//
	// The compile-flag default is deliberately NOT changed, and the two are not
	// in tension: `fklua mod` with no manifest still defaults to leaking, so
	// every build that exists today is untouched. What is collected by default
	// is a PROJECT, which is the unit an author actually chooses.
	//
	// RUST GETS IT TOO, as of guest/rust/fkgc. This used to read "leaking" for
	// Rust and say why -- there was no collector for wasm32-unknown-unknown and
	// guest/rust/fk's allocator was a bump arena whose dealloc is a no-op. There
	// is one now, it needs no import at all (fk owns the single
	// #[global_allocator] site and --features fk/fkgc chooses what backs it), and
	// the same obligation follows: writing the key obliges init to scaffold a
	// guest that carries a collector, which is what scaffoldRustGuest is for.
	p.GC = "collected"

	// Refuse rather than overwrite. A manifest is hand-written and losing one
	// to a re-run of init is not a recoverable mistake.
	if _, err := os.Stat(projectFile); err == nil {
		return fmt.Errorf("%s already exists; delete it first if you meant to start over", projectFile)
	}
	if err := os.WriteFile(projectFile, []byte(p.TOML()), 0o644); err != nil {
		return err
	}
	// WHERE, not just what. `fklua init my-mod` writes into the CURRENT
	// directory and creates no my-mod/ of its own -- the name argument is the
	// mod's identity and nothing else. The README's `mkdir my-mod && cd my-mod`
	// makes the other reading easy to assume, and a first-time user reported
	// stopping to run `find` to check nothing had nested.
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = "the current directory"
	}
	fmt.Printf("wrote %s (into %s; the name argument is the mod's identity, "+
		"not a directory to create)\n", projectFile, cwd)
	fmt.Printf("  mod %s %s, Factorio %s\n", p.Name, p.Version, p.FactorioVersion)
	fmt.Printf("  bindings from API %s, for %s\n", p.API, strings.Join(p.Langs, " and "))
	fmt.Printf("  gc = %q\n", p.GC)

	// WHATEVER --no-guest SAYS. The package output accumulates in the project
	// root however the guest is built and wherever it lives, so the ignore list
	// is about the project rather than about the scaffold.
	if err := writeGitignore(p); err != nil {
		return err
	}

	scaffolded, scaffoldedRust := false, false
	if hasLang(p.Langs, "go") && !noGuest {
		wrote, err := scaffoldGuest(p.Name, guestModule)
		for _, f := range wrote {
			fmt.Printf("wrote %s\n", f)
		}
		if err != nil {
			return err
		}
		scaffolded = true
	}
	if hasLang(p.Langs, "rust") && !noGuest {
		wrote, err := scaffoldRustGuest(p.Name, rustSubstrateDir(guestModule))
		for _, f := range wrote {
			fmt.Printf("wrote %s\n", f)
		}
		if err != nil {
			return err
		}
		scaffoldedRust = true
	}

	// THE NEXT STEPS ARE THE COMMANDS THAT ACTUALLY WORK, in the order they
	// work in. This block used to print a RECOMMENDATION -- four lines of shell
	// against a Go module the author still had to create -- and the distance
	// between a recommendation and a default is exactly the distance between
	// "you should build it collected" and "it is built collected".
	fmt.Printf("\nNext:\n")
	if scaffolded || scaffoldedRust {
		if scaffolded && guestModule == "" {
			fmt.Printf("  (cd %s && go mod tidy)\n", guestDir)
		}
		fmt.Printf("  fklua gen-bindings && fklua lock\n")
		if scaffolded {
			fmt.Printf("  (cd %s && tinygo build -target=wasm-unknown -scheduler=none \\\n", guestDir)
			fmt.Printf("      -gc=custom -opt=2 -o ../../%s.wasm .)\n", p.Name)
			fmt.Printf("  fklua mod %s.wasm\n", p.Name)
		}
		if scaffoldedRust {
			// --features fk/fkgc is the collector and there is no second half:
			// no import, no -gc flag. It is passed here rather than declared in
			// the guest's Cargo.toml because Cargo's v2 resolver would unify it
			// across every crate in the same build.
			fmt.Printf("  (cd %s && cargo build --release \\\n", rustGuestDir)
			fmt.Printf("      --target wasm32-unknown-unknown --features fk/fkgc)\n")
			fmt.Printf("  fklua mod %s\n", filepath.Join(rustGuestDir, "target",
				"wasm32-unknown-unknown", "release", RustGuestArtifact(p.Name)))
		}
	} else {
		fmt.Printf("  fklua gen-bindings && fklua lock\n")
		fmt.Printf("  fklua mod <your-guest>.wasm\n")
	}
	fmt.Printf("\n`fklua mod` reads %s, so it needs no flags -- not for identity,\n", projectFile)
	fmt.Printf("not for dependencies, and not for --gc. Add `data = \"mod-data\"` under\n")
	fmt.Printf("[mod] to ship a data stage.\n")

	if p.GC == "collected" {
		// WHAT THE DEFAULT BOUGHT, in the two numbers that decide it. Both are
		// in-game measurements from agents/guests.md's grow table, and they are
		// about the GROWTH LAW rather than about reclaiming: a leaking guest
		// doubles, and nothing in FkLua can bound a doubling's grow tick.
		fmt.Printf("\nThis project is COLLECTED by default, which is the recommendation as of\n")
		if scaffolded {
			fmt.Printf("sharding stage C. %s imports the collector and %s paces it\n",
				filepath.Join(guestDir, "gc.go"), filepath.Join(guestDir, "main.go"))
			fmt.Printf("from fk_on_tick; -gc=custom above is the other half and both are needed.\n")
		}
		if scaffoldedRust {
			fmt.Printf("sharding stage C. On the Rust side there is NO import and no second\n")
			fmt.Printf("flag: --features fk/fkgc above is the whole of it, because fk owns the\n")
			fmt.Printf("single #[global_allocator] site. %s paces it from fk_on_tick.\n",
				filepath.Join(rustCrateDir(p.Name), "src", "lib.rs"))
		}
		fmt.Printf("Measured in game on the same guest at a 40 MiB heap: a worst grow tick\n")
		fmt.Printf("of 24.6 ms collected against 974.5 ms leaking. There is no heap cap and\n")
		fmt.Printf("no size at which turning it off is better -- the collector's own\n")
		fmt.Printf("metadata is 31 KiB plus about 1%% of the heap.\n")
		// THE OPT-OUT IS NAMED WITHOUT AN ENDORSEMENT, and this text used to
		// carry one: it cited the first mod outside this repo (BBB) as having
		// measured both arms and shipped leaking. That was true when it was
		// written and stopped being true on 2026-08-02, when the same mod
		// re-measured on the sharded pin and flipped -- the steady state could
		// not tell the arms apart, and the marathon doubling stall it was
		// avoiding measured 782 ms leaking against 71 ms collected. A downstream
		// citation is the one part of this message that can go stale without
		// anything failing, so what is left is the property rather than the
		// witness.
		fmt.Printf("\n`gc = \"leaking\"` in %s is the EXPERT opt-out, and it is a real\n", projectFile)
		fmt.Printf("one: a guest that allocates once and reuses reclaims nothing, and what\n")
		fmt.Printf("it buys back is the collector's own emitted code -- measured downstream\n")
		fmt.Printf("at +32.4%% of fk_module.lua and +13.7%% of the zip. It is also the only\n")
		fmt.Printf("option for wasip1. What it does NOT buy back is the growth law: a\n")
		fmt.Printf("leaking guest's memory DOUBLES, and the tick that doubles is the 974.5\n")
		fmt.Printf("ms above -- so choose it on a MEASUREMENT of your own heap over a long\n")
		fmt.Printf("session, not on a prediction about your own tidiness. The first mod\n")
		fmt.Printf("outside this repo shipped leaking on exactly that prediction and\n")
		fmt.Printf("re-measured its way back to collected.\n")
		fmt.Printf("Change the key AND build without the collector (-gc=leaking,\n")
		fmt.Printf("or dropping --features fk/fkgc); changing one alone is a refusal at\n")
		fmt.Printf("package time.\n")
		fmt.Printf("See agents/guests.md, \"the guest heap budget\".\n")
	}
	return nil
}

const gitignoreFile = ".gitignore"

// writeGitignore writes the ignore list for a fresh project, and LEAVES AN
// EXISTING ONE ALONE.
//
// Nothing else told git about the artifacts init's own next steps produce: a
// `<name>.wasm` at the project root, a cargo target tree under the Rust guest,
// and a `<name>_<version>/` package directory beside them. A project that
// packages once and commits is carrying all three.
//
// AN EXISTING .gitignore IS A NOTICE AND NOT A REFUSAL, which is deliberately
// different from the per-file refusal the guest scaffold makes. Those files are
// hand-edited source whose loss is unrecoverable, so overwriting one is a
// mistake nobody can undo; a .gitignore is simply the author's, and a repository
// that already exists normally has one. An init that errored on it would refuse
// every real project -- `git init && fklua init` is the ordinary order, and so
// is running init inside a repo that has been there for years.
func writeGitignore(p factorio.Project) error {
	if _, err := os.Stat(gitignoreFile); err == nil {
		fmt.Printf("NOTICE: %s already exists; left as it is -- worth ignoring "+
			"/%s_*/ and the built wasm there\n", gitignoreFile, p.Name)
		return nil
	}
	if err := os.WriteFile(gitignoreFile, []byte(gitignoreBody(p)), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", gitignoreFile)
	return nil
}

// gitignoreBody is the file's content, derived from the name and the languages.
//
// PLAIN CONCATENATION IS CORRECT HERE AND NOTHING NEEDS ESCAPING. A mod name is
// matched by `^[A-Za-z0-9][A-Za-z0-9 _-]*$` (factorio.nameRE), so it may contain
// interior SPACES -- which a gitignore pattern carries literally, with no quoting
// and no backslash -- and it may NOT contain any of the glob metacharacters `*`,
// `?`, `[`, `!` or `#` that would need one. The leading `/` on every line settles
// the two position-sensitive characters as well: a pattern can never begin with
// `#` (a comment) or `!` (a negation) however the name starts. Do not add
// escaping to this function later; there is nothing for it to escape.
//
// That leading `/` is also what anchors each pattern at the project root, so a
// nested directory that happens to share the mod's name is not swept up.
func gitignoreBody(p factorio.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Build output, written by the commands `fklua init` prints.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# Every pattern is anchored with a leading \"/\", so it matches at the\n")
	fmt.Fprintf(&b, "# project root only and a nested directory named like the mod is left\n")
	fmt.Fprintf(&b, "# alone.\n")
	fmt.Fprintf(&b, "#\n")
	// THE ONE GENERATED FILE THAT IS NOT HERE, said in the file itself because
	// the sweep that gets it wrong is a reflexive one: "it is generated, so
	// ignore it". The lock is what records which API description the bindings
	// came from, which is a fact a source tree cannot answer on its own -- so it
	// belongs in the commit, and `fklua lock --check` in CI is checking a file CI
	// has to be able to read.
	fmt.Fprintf(&b, "# fklua.lock is deliberately NOT ignored. It is generated and it is meant\n")
	fmt.Fprintf(&b, "# to be COMMITTED: it records which API description the bindings were\n")
	fmt.Fprintf(&b, "# generated from, and `fklua lock --check` is the CI gate that reads it.\n")
	if hasLang(p.Langs, "go") {
		fmt.Fprintf(&b, "\n# The Go guest's wasm, where init's own `tinygo build -o ../../%s.wasm`\n", p.Name)
		fmt.Fprintf(&b, "# puts it and where `fklua mod` then reads it from.\n")
		fmt.Fprintf(&b, "/%s.wasm\n", p.Name)
	}
	if hasLang(p.Langs, "rust") {
		fmt.Fprintf(&b, "\n# Cargo's build tree for the Rust guest; the release wasm is inside it.\n")
		fmt.Fprintf(&b, "/%s/target/\n", rustGuestDir)
	}
	fmt.Fprintf(&b, "\n# What `fklua mod` writes: the package directory and the zip, both named\n")
	fmt.Fprintf(&b, "# <name>_<version>. The version moves, so the glob is on it.\n")
	fmt.Fprintf(&b, "/%s_*/\n", p.Name)
	fmt.Fprintf(&b, "/%s_*.zip\n", p.Name)
	return b.String()
}

func hasLang(langs []string, want string) bool {
	for _, l := range langs {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// loadProject reads fklua.toml from the working directory, if there is one.
//
// Absent is not an error: every command that calls this works without a
// project, which is how the repo's own tests and the examples run.
func loadProject() (factorio.Project, bool, error) {
	b, err := os.ReadFile(projectFile)
	if os.IsNotExist(err) {
		return factorio.Project{}, false, nil
	}
	if err != nil {
		return factorio.Project{}, false, err
	}
	p, err := factorio.ParseProject(string(b))
	if err != nil {
		return p, true, fmt.Errorf("%s: %w", projectFile, err)
	}
	return p, true, nil
}

func runLock(args []string) error {
	check := false
	for _, a := range args {
		if a == "--check" {
			check = true
			continue
		}
		return fmt.Errorf("unknown argument %q", a)
	}
	p, ok, err := loadProject()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no %s here; run `fklua init <mod-name>` first", projectFile)
	}

	want, err := buildLock(p)
	if err != nil {
		return err
	}
	if !check {
		if err := os.WriteFile(lockFile, []byte(want.Text()), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (api %s)\n", lockFile, want.API)
		return nil
	}

	raw, err := os.ReadFile(lockFile)
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua lock`)", lockFile, err)
	}
	have, err := factorio.ParseLock(string(raw))
	if err != nil {
		return err
	}
	// Each mismatch gets its own message, because each means something
	// different and the fix differs.
	switch {
	case have.API != want.API:
		return fmt.Errorf("%s pins API %s but %s says %s; run `fklua lock`",
			lockFile, have.API, projectFile, want.API)
	case have.APISHA256 != want.APISHA256:
		return fmt.Errorf("the runtime-api.json for %s CHANGED underneath the lock "+
			"(%s -> %s). A pinned version's description should never move; "+
			"check what edited api/%s/", want.API, have.APISHA256[:12],
			want.APISHA256[:12], want.API)
	case have.BindingsSHA256 != want.BindingsSHA256:
		return fmt.Errorf("the generated bindings do not match the lock; either " +
			"someone edited generated code by hand, or `fklua gen-bindings` " +
			"was not re-run after changing the API pin")
	}
	fmt.Printf("%s is up to date (api %s)\n", lockFile, have.API)
	return nil
}

// buildLock computes what the lock should say right now.
func buildLock(p factorio.Project) (factorio.Lock, error) {
	l := factorio.Lock{API: p.API, Fklua: fkluaVersion}
	sum, err := factorio.HashFile(apiPath(p.API))
	if err != nil {
		return l, fmt.Errorf("hashing the API description: %w (run `fklua api pull %s`)", err, p.API)
	}
	l.APISHA256 = sum

	var paths []string
	for _, lang := range p.Langs {
		switch lang {
		case "go":
			paths = append(paths, GoBindingsPath)
		case "rust":
			paths = append(paths, RustBindingsPath)
		default:
			return l, fmt.Errorf("%s: no generator for lang %q", projectFile, lang)
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return l, fmt.Errorf("%s: %w (run `fklua gen-bindings`)", filepath.Base(p), err)
		}
	}
	sum, err = factorio.HashTree(paths)
	if err != nil {
		return l, err
	}
	l.BindingsSHA256 = sum
	return l, nil
}
