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
			p.Langs, i = dedupeLangs(strings.Split(v, ",")), next
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
			"[--data] [--guest-module PATH] [--no-guest], or fklua init --library " +
			"<name> [--data] [--lang go|rust] [--guest-module PATH]")
	}
	if dataFlavor && !library && noGuest {
		// The data guest is a SECOND wasm module in the SAME tree as the
		// control one -- a main package inside guest/go's module, a member of
		// guest/rust's workspace -- so there is nowhere to put it in a project
		// whose guest lives somewhere else entirely. Refused rather than half
		// done, the way --library and --no-guest are.
		return fmt.Errorf("--data and --no-guest contradict: a data guest is a " +
			"second module in the SAME tree as the control guest (a main package " +
			"under guest/go, a member of guest/rust's workspace), so there is " +
			"nothing to scaffold it beside. Declare an existing one with [fklua] " +
			"data_module instead")
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

	// --data: the mod gets a DATA-STAGE guest per language, and the manifest
	// gets the key that names the artifact.
	//
	// ONE KEY, HOWEVER MANY LANGUAGES. `data_module` is a path to one wasm,
	// because a mod HAS one data stage -- so a two-language project's key names
	// the first language in `lang` order and the other artifact rides beside it
	// as a comment, which is exactly the choice `fklua mod <guest>.wasm` already
	// makes for the control module. Swapping languages is then editing one line
	// in a file that already tells you what to put there.
	var dataModuleLang, dataModuleAltLang string
	if dataFlavor {
		arts := dataArtifacts(p.Name, p.Langs)
		if len(arts) == 0 {
			return fmt.Errorf("--data has no language to scaffold for: lang is %q, "+
				"and a data guest is written in go or rust", strings.Join(p.Langs, ","))
		}
		p.DataModule = arts[0].path
		dataModuleLang = arts[0].lang
		if len(arts) > 1 {
			p.DataModuleAlt = arts[1].path
			dataModuleAltLang = arts[1].lang
		}
	}

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
		if dataFlavor {
			wrote, err := scaffoldGoDataGuest(p.Name, p.Langs)
			for _, f := range wrote {
				fmt.Printf("wrote %s\n", f)
			}
			if err != nil {
				return err
			}
		}
		scaffolded = true
	}
	if hasLang(p.Langs, "rust") && !noGuest {
		wrote, err := scaffoldRustGuest(p.Name, rustSubstrateDir(guestModule), dataFlavor)
		for _, f := range wrote {
			fmt.Printf("wrote %s\n", f)
		}
		if err != nil {
			return err
		}
		if dataFlavor {
			wrote, err := scaffoldRustDataGuest(p.Name, p.Langs)
			for _, f := range wrote {
				fmt.Printf("wrote %s\n", f)
			}
			if err != nil {
				return err
			}
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
		// ONE DERIVATION FOR THE PRINTED BLOCK AND THE SCAFFOLDED ONE, which is
		// the whole of why these lines are no longer written out here. A
		// `--data` project's recipe has to build the data module `data_module`
		// NAMES, which in a two-language project is the OTHER language's, and
		// that rule was taught to the scaffolded headers alone: init went on
		// printing each language's own data build, so pasting the non-key
		// block's three lines ended on `fklua mod: data module: open <path>:
		// no such file or directory` -- an open error naming a wasm nothing in
		// the block had built, the very failure the headers' own paragraph
		// says a recipe must not produce. Both now render
		// initRecipeCommands, so the block on screen and the block in the
		// author's editor cannot disagree about it.
		if scaffolded {
			printRecipe(initRecipeCommands(p.Name, "go", p.Langs, dataFlavor))
		}
		if scaffoldedRust {
			printRecipe(initRecipeCommands(p.Name, "rust", p.Langs, dataFlavor))
		}
	} else {
		fmt.Printf("  fklua gen-bindings && fklua lock\n")
		fmt.Printf("  fklua mod <your-guest>.wasm\n")
	}
	fmt.Printf("\n`fklua mod` reads %s, so it needs no flags -- not for identity,\n", projectFile)
	// `data` IS THE ASSET DIRECTORY AND `data_module` IS THE WASM, and this
	// line used to call the first one "a data stage" -- one sentence above a
	// --data project being told it has a DATA STAGE already, declared as
	// data_module. Two different things under one name in one screen of output
	// is a first-time author reading the wrong key twice, so this one says what
	// it copies rather than what stage it belongs to.
	fmt.Printf("not for dependencies, and not for --gc. Add `data = \"mod-data\"` under\n")
	fmt.Printf("[mod] to ship files alongside the guest -- graphics, locale, a\n")
	fmt.Printf("changelog, hand-written Lua -- copied into the package as they are.\n")

	if dataFlavor {
		// THE TWO CARGO INVOCATIONS ARE NOT A STYLE, said here because the
		// printed commands above are what an author copies and the reason they
		// are two is invisible in them.
		fmt.Printf("\nThis project has a DATA STAGE: a SECOND wasm module, run at Factorio's\n")
		fmt.Printf("settings and data stages, declared as `data_module` in %s.\n", projectFile)
		fmt.Printf("It is compiled --persist=none and -gc=leaking whatever the control guest\n")
		fmt.Printf("uses, because it runs once at load and dies with the Lua state that built\n")
		fmt.Printf("it -- no tick to pace a collection from, and no state to survive. `fklua\n")
		fmt.Printf("mod` REFUSES a data module that carries a collector, or that imports the\n")
		fmt.Printf("generated fkapi bindings: there is no runtime API at those stages.\n")
		if scaffoldedRust {
			// -p RATHER THAN A LINE COUNT, because how many cargo lines are
			// printed depends on which language holds the key: a project whose
			// data module is the Go one prints ONE cargo line in the Rust
			// block, and "the two cargo lines above" was then a sentence about
			// a block that did not exist. What is true in every arm is that no
			// printed cargo line builds the workspace whole.
			fmt.Printf("On the Rust side that is why every cargo line above names its package\n")
			fmt.Printf("with -p: cargo's v2 resolver unifies --features fk/fkgc across every\n")
			fmt.Printf("package built in ONE invocation, so a bare build over the workspace\n")
			fmt.Printf("would collect the data crate too.\n")
		}
		if len(p.Langs) > 1 && p.DataModuleAlt != "" {
			// WHICH BUILD THE KEY NAMES, said in the language's own name. Two
			// languages print two `fklua mod` lines, one per control guest, and
			// BOTH of them package the data module this key names -- so an
			// author who builds only the other language's half meets a missing
			// artifact whichever line they ran. "One of them" left that to be
			// worked out from a path.
			fmt.Printf("Both languages were scaffolded and a mod has ONE data stage, so\n")
			fmt.Printf("`data_module` names the %s build (%s). Every `fklua\n",
				langName(dataModuleLang), p.DataModule)
			fmt.Printf("mod` line above packages that one, whichever control guest it names,\n")
			fmt.Printf("which is why the same data build appears in both blocks: each block is\n")
			fmt.Printf("a complete recipe, so the three lines for the guest you are shipping\n")
			fmt.Printf("work on their own. To ship the %s data module instead, point the\n",
				langName(dataModuleAltLang))
			fmt.Printf("key at the path in the comment beside it (%s).\n", p.DataModuleAlt)
		}
		fmt.Printf("See docs/data-stage.md in the FkLua repo for the library.\n")
	}

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

// dataArtifacts is where a `--data` scaffold's data module lands, one entry per
// declared language, IN THE ORDER `lang` declares them.
//
// The order is the whole of the two-language rule: `data_module` takes the
// first and the rest ride beside it as comments, so which language a project
// packages by default is a fact about its own manifest rather than about an
// alphabetisation nobody chose. Unknown languages are skipped here and rejected
// where they always were, in buildLock.
//
// SKIPPING RATHER THAN REFUSING HAS ONE VISIBLE CONSEQUENCE AND IT IS LEFT
// ALONE DELIBERATELY. `--lang go,zig --data` writes `lang = ["go", "zig"]` and
// then, because only one entry survives this switch, a scaffolded note reading
// "`lang` declares one language". That sentence is wrong about the manifest
// three lines above it -- and the project it is wrong in cannot be built at
// all: `fklua gen-bindings`, which is init's own next printed step, exits with
// `no generator for lang "zig"`, and so does `fklua lock`. Nobody reaches the
// note in anger. Deciding here that a language is unknown would make this a
// SECOND place that decides which languages exist, and two of those is the
// drift shape the rest of this file is written to avoid; the one place is the
// generator table buildLock consults.
func dataArtifacts(modName string, langs []string) []dataArtifact {
	var out []dataArtifact
	for _, l := range langs {
		switch lang := strings.TrimSpace(l); lang {
		case "go":
			out = append(out, dataArtifact{lang, GoDataArtifact(modName)})
		case "rust":
			out = append(out, dataArtifact{lang, rustReleaseWasm(RustDataArtifact(modName))})
		}
	}
	return out
}

// dataRecipeCommands is the build-and-package recipe a scaffolded data guest's
// header carries, one element per command and one string per physical line,
// GENERATED FROM dataArtifacts for the same reason dataModuleKeyNote is.
//
// The three commands are: build the control guest OF THIS FILE'S OWN LANGUAGE,
// because the packaging line takes its wasm as the positional; build the data
// module `data_module` NAMES, which in a two-language project is the OTHER
// language's; and package the two. Writing the whole block in one language was
// right while a project had one, and `--lang go,rust --data` is what made it
// wrong: the key takes the FIRST language, so the non-key-holding header's
// second line built a module the packaging line never reads and the packaging
// line then opened a wasm nothing in the block had built. That is the very
// failure the block's own paragraph says a recipe must not produce.
//
// A one-language project is unaffected, because there the key's language and
// the file's language are the same one.
func dataRecipeCommands(modName, thisLang string, langs []string) [][]string {
	keyLang := thisLang
	if arts := dataArtifacts(modName, langs); len(arts) > 0 {
		keyLang = arts[0].lang
	}
	// No trailing `# ...` note on the packaging line. A Rust control artifact's
	// path is already 84 characters under the block's indent, and the sentence
	// a note would carry -- that the data module comes from the key rather than
	// from the argument -- is the paragraph directly under the block in both
	// templates.
	return [][]string{
		controlBuildCommand(modName, thisLang),
		dataBuildCommand(modName, keyLang),
		{"fklua mod " + shellArg(controlWasm(modName, thisLang))},
	}
}

// initRecipeCommands is the build-and-package block `fklua init` prints for one
// scaffolded language, and for a `--data` project it IS dataRecipeCommands --
// the same function the two data-guest headers render, from the same
// dataArtifacts fact `data_module` is filled in from.
//
// That identity is the point. The two-language rule -- the recipe builds the
// data module the KEY names, not the one this language happens to produce --
// reached the scaffolded headers first and the printed steps not at all, so
// init went on printing each language's own data build and a literal paste of
// the non-key block ended on `fklua mod: data module: open <path>: no such
// file or directory`. Two statements of one fact drifting is this repo's most
// repeated failure shape; there is one statement now.
//
// A two-language `--data` project therefore prints the SAME data build in both
// blocks, on purpose: each block is a complete recipe, so an author who runs
// only the one for the guest they are shipping still builds what the packaging
// line reads. Building it twice is a cached no-op; not building it is the open
// error above.
//
// Without --data there is no data module and the block is the two lines it has
// always been. The Rust one is deliberately NOT controlBuildCommand's: with no
// data crate in the workspace there is nothing to disambiguate, and the
// unqualified `cargo build --features fk/fkgc` is what this command has always
// printed for a one-member workspace.
func initRecipeCommands(modName, thisLang string, langs []string, data bool) [][]string {
	if data {
		return dataRecipeCommands(modName, thisLang, langs)
	}
	if thisLang == "rust" {
		return [][]string{
			{
				"(cd " + rustGuestDir + " && cargo build --release \\",
				"    --target wasm32-unknown-unknown --features fk/fkgc)",
			},
			{"fklua mod " + shellArg(controlWasm(modName, thisLang))},
		}
	}
	return [][]string{
		controlBuildCommand(modName, thisLang),
		{"fklua mod " + shellArg(controlWasm(modName, thisLang))},
	}
}

// printRecipe writes a recipe to stdout under init's own two-space indent, one
// physical line per element, which is the shape an author copies.
func printRecipe(cmds [][]string) {
	for _, cmd := range cmds {
		for _, line := range cmd {
			fmt.Printf("  %s\n", line)
		}
	}
}

// controlBuildCommand and dataBuildCommand are the two build lines, per
// language. A continuation carries the four spaces a reader sees under the
// block's own indent; the caller adds the comment marker and that indent.
//
// --features fk/fkgc IS THE COLLECTOR AND THERE IS NO SECOND HALF on the Rust
// side: no import, no -gc flag. It is a command-line flag rather than a key in
// the guest's Cargo.toml because cargo's v2 resolver would unify it across
// every crate in the same build.
//
// AND THAT IS WHY THE RUST LINE CARRIES -p. This function is reached only from
// a recipe that has a data crate in the workspace, and an unqualified
// `cargo build --features fk/fkgc` builds every member -- so the same
// unification that makes the feature a flag would hand the data module a
// collector. `fklua mod` refuses that, so it would be a build error rather
// than a silent one, but the command printed for an author to paste should not
// be the one that provokes it. A project with no data crate has nothing to
// disambiguate and gets the unqualified line; see initRecipeCommands.
func controlBuildCommand(modName, lang string) []string {
	if lang == "rust" {
		return []string{
			"(cd " + rustGuestDir + " && cargo build --release \\",
			"    --target wasm32-unknown-unknown -p " + rustCrateName(modName) +
				" --features fk/fkgc)",
		}
	}
	return []string{
		"(cd " + guestDir + " && tinygo build -target=wasm-unknown -scheduler=none \\",
		"    -gc=custom -opt=2 -o ../../" + shellArg(GoGuestArtifact(modName)) + " .)",
	}
}

func dataBuildCommand(modName, lang string) []string {
	if lang == "rust" {
		return []string{
			"(cd " + rustGuestDir + " && cargo build --release \\",
			"    --target wasm32-unknown-unknown -p " + rustDataCrateName(modName) + ")",
		}
	}
	// -gc=leaking, and it is the flag that differs from the control guest's
	// line: a data module runs once and dies with the Lua state that built it,
	// and packaging refuses one that carries a collector.
	return []string{
		"(cd " + guestDir + " && tinygo build -target=wasm-unknown -scheduler=none \\",
		"    -gc=leaking -opt=2 -o ../../" + shellArg(GoDataArtifact(modName)) + " ./data)",
	}
}

// controlWasm is where a language's CONTROL guest build lands, which is what
// the packaging line takes as its positional.
func controlWasm(modName, lang string) string {
	if lang == "rust" {
		return rustReleaseWasm(RustGuestArtifact(modName))
	}
	return GoGuestArtifact(modName)
}

// dataRecipeBlock renders that recipe as the indented comment block a header
// carries: marker is the file's doc-comment marker and indent is what the
// language's formatter renders as a code block under it.
func dataRecipeBlock(modName, thisLang string, langs []string, marker, indent string) string {
	var b strings.Builder
	for _, cmd := range dataRecipeCommands(modName, thisLang, langs) {
		for _, line := range cmd {
			b.WriteString(marker + indent + line + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// dataModuleKeyNote is the paragraph a scaffolded data guest carries about
// WHICH data module `data_module` names, GENERATED FROM dataArtifacts -- the
// same fact the printed steps above are generated from.
//
// It is generated because the sentence it replaces was a constant in a
// template and could not be right in every project: `data_module` takes the
// FIRST language in `lang` order, so a Go template asserting "it names this
// module" is false in every `--lang rust,go --data` project and a Rust one is
// false in every `--lang go,rust`. That is this repo's most repeated failure
// shape -- two statements of one fact drifting -- and the printed steps were
// taught the rule in the same change that left both scaffolded files
// asserting. One derivation cannot disagree with itself.
//
// thisLang is the language of the file the note is going into. An unknown one
// cannot happen from the scaffolders, which are only reached for a language
// dataArtifacts knows, and is answered with the rule rather than with a claim
// about a path that was never computed.
func dataModuleKeyNote(modName, thisLang string, langs []string) string {
	arts := dataArtifacts(modName, langs)
	// The FIRST match, because the key's own rule is the first in `lang` order:
	// a loop that kept the last would answer a different question from the one
	// `data_module` was filled in by.
	//
	// BELT AND BRACES, and it is worth saying which: arts can hold two entries
	// of one language only if p.Langs does, and dedupeLangs removes that at
	// the only call site that reaches the scaffolders -- so removing this
	// break changes no output any command here can produce and reddens
	// nothing. It is what would hold if a HAND-EDITED manifest reached this
	// code, which ParseProject neither refuses nor de-duplicates.
	mine := -1
	for i, a := range arts {
		if a.lang == thisLang {
			mine = i
			break
		}
	}
	if len(arts) == 0 || mine < 0 {
		return "`data_module` names ONE data module, and a project declaring " +
			"two languages gives the key the FIRST in `lang` order. Check the " +
			"key: it is the module `fklua mod` packages, whichever control " +
			"guest the command names."
	}
	if len(arts) == 1 {
		return "IN THIS PROJECT THE KEY NAMES THIS MODULE, at " + arts[0].path +
			". `lang` declares one language, so there is nothing else it could " +
			"name -- but the key is still the whole of the choice, and a " +
			"project that later declares two gives it the FIRST in `lang` order."
	}
	other := arts[1]
	if mine != 0 {
		other = arts[mine]
		return "IN THIS PROJECT THE KEY DOES NOT NAME THIS MODULE. Both " +
			"languages were scaffolded and a mod has ONE data stage, so " +
			"`data_module` takes the FIRST in `lang` order, which is " +
			langName(arts[0].lang) + ": the key holds " + arts[0].path +
			" and this module's own path rides in a comment beside it. Every " +
			"`fklua mod` line packages the key's module whichever control " +
			"guest it names, so build that one too -- or point the key at " +
			other.path + " to ship this one instead."
	}
	return "IN THIS PROJECT THE KEY NAMES THIS MODULE, at " + arts[0].path +
		". Both languages were scaffolded and a mod has ONE data stage, so " +
		"`data_module` takes the FIRST in `lang` order, which is " +
		langName(arts[0].lang) + "; the " + langName(other.lang) +
		" data module builds to " + other.path + " and rides in a comment " +
		"beside the key. Point the key there to ship that one instead."
}

// langName is a declared language spelled the way prose spells it, so a printed
// sentence reads "the Go build" rather than "the go build".
func langName(lang string) string {
	switch lang {
	case "go":
		return "Go"
	case "rust":
		return "Rust"
	}
	return lang
}

// dataArtifact is one language's data-module build: the language that produces
// it, and where that language's printed build line leaves it.
//
// The LANGUAGE travels with the path because the next-steps block has to say
// which build `data_module` names. A two-language project prints two
// `fklua mod` lines, one per control guest, and both of them package the same
// data module -- the key's -- so "one of them" is not enough to act on.
type dataArtifact struct {
	lang string
	path string
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
		// THE QUOTED FORM IN THE COMMENT AND THE BARE FORM IN THE PATTERN, and
		// the two differ on purpose. The comment QUOTES A SHELL COMMAND, which
		// is the one init prints, so a name carrying a space is quoted there
		// exactly as it is there; the pattern below is read by git, which
		// matches it literally and would take a quote as a character in the
		// filename. See shellArg.
		fmt.Fprintf(&b, "\n# The Go guest's wasm, where init's own `tinygo build -o ../../%s`\n", shellArg(GoGuestArtifact(p.Name)))
		fmt.Fprintf(&b, "# puts it and where `fklua mod` then reads it from.\n")
		fmt.Fprintf(&b, "/%s\n", GoGuestArtifact(p.Name))
		// THE DATA MODULE IS A SECOND ARTIFACT AT THE SAME PLACE, so it needs
		// its own line: the pattern above is anchored and exact, not a glob.
		// Keyed on `data_module` rather than on a flag, because the key IS the
		// statement that this project has a data stage. The Rust arm needs
		// nothing, since its whole target tree is already ignored below.
		if p.DataModule != "" {
			fmt.Fprintf(&b, "\n# The Go DATA guest's wasm, beside it, from the second `tinygo build`\n")
			fmt.Fprintf(&b, "# line init prints.\n")
			fmt.Fprintf(&b, "/%s\n", GoDataArtifact(p.Name))
		}
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

// dedupeLangs drops a language `--lang` names twice, keeping the FIRST spelling
// and the declared order.
//
// `--lang go,go` is a typo rather than a usage, and it used to reach the
// manifest whole: `lang = ["go", "go"]` made a ONE-language project look like a
// two-language one to everything downstream, which is where it stopped being
// harmless. `data_module` takes the first entry and the "other" artifact rides
// beside it as a comment, so the key and the comment named the same file, and
// the scaffolded note read "THE KEY DOES NOT NAME THIS MODULE" about a key
// holding that module's own path -- every sentence in it false. Removing the
// duplicate HERE rather than at each reader is what makes one language stay one
// language everywhere: the manifest, the printed steps, the scaffolders, the
// artifact list and the note all read `lang`.
//
// An UNKNOWN language is not this function's business and is still written
// through: it is refused where it always was, by the generators, and turning a
// typo into a different message here would be a second place that decides which
// languages exist.
func dedupeLangs(langs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range langs {
		key := strings.TrimSpace(l)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	return out
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
