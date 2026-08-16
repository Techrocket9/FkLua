package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The guest source `fklua init` writes, and why it writes any at all.
//
// UNTIL NOW `init` WROTE ONE FILE -- fklua.toml -- AND PRINTED A RECOMMENDATION.
// The recommendation was the right one (build it collected) and it was four
// lines of shell an author had to retype correctly against a Go module they had
// to create themselves. The gap between "recommended" and "what you get" is
// where a default actually lives: a guest that is collected by default has to
// arrive collected, not arrive leaking with advice attached.
//
// So init now scaffolds a guest that builds, imports the collector, and is
// packaged by the `gc = "collected"` key init also writes. The three files are
// the minimum that is true:
//
//	guest/go/go.mod   its own module, because //go:wasmimport is rejected outside
//	                  GOARCH=wasm and a guest cannot live in a host module. This
//	                  is the same reason guest/go is its own module in this repo.
//	guest/go/gc.go    the fkgc import. Under any -gc except custom fkgc is an
//	                  EMPTY PACKAGE, so this file costs a leaking build nothing --
//	                  and under -gc=custom it is what supplies the seven runtime
//	                  hooks TinyGo's custom-GC seam demands. Without it,
//	                  -gc=custom does not LINK, with `missing core function
//	                  "runtime.free"` from deep inside the builder. The flag alone
//	                  is not the feature.
//	guest/go/main.go  a guest that logs, allocates, and paces its collector from
//	                  fk_on_tick. It allocates on purpose: a scaffold whose guest
//	                  never allocates would make the collector it just turned on
//	                  unobservable, and the first thing an author changes is the
//	                  body of fk_on_tick.
//
// THE DEPENDENCY IS THE ONE HARD PART, and it is honest rather than clever.
// The scaffolded module imports github.com/Techrocket9/fklua/guest/go, which
// is a published module: the default flow is `go mod tidy` once, like any Go
// project. `--guest-module PATH` instead writes a `replace` onto a local
// checkout, which is what a contributor working on FkLua itself wants and what
// this repo's own init-to-build test uses so that it needs no network.

// guestDir is where the Go guest is scaffolded, and IT IS NOT A PREFERENCE: it
// is the directory `gen-bindings` writes the bindings into and `fklua lock`
// hashes by exact name.
//
// IT WAS `guest/` AND THAT WAS THE GO ARM OF R8, one round late. gen-bindings
// hard-codes GoBindingsPath = "guest/go/fkapi/fkapi.go" and lock looks for
// exactly that file, so a guest module rooted at `guest/` swallowed the
// bindings as a subpackage one segment deeper than anybody writes -- import
// path `<mod>-guest/go/fkapi`, in a directory nothing in this repo's docs ever
// described. It BUILT, which is why it survived: the defect is not a broken
// scaffold, it is a scaffold whose layout no reader and no other tool agrees
// with. THREE independent mods (BetterBeltBalancer, nixie-tubes,
// qol-research) each moved their guest to guest/go by hand and identically,
// which is as clear a report as a layout ever gets.
//
// The Rust arm already made this move for the same reason and with the same
// evidence -- see rustGuestDir below, whose header is the R8 fix -- so the two
// languages are siblings again: guest/go beside guest/rust, exactly the shape
// this repo's own tree has. TestTheScaffoldIsWhereTheBindingsGo is what stops
// them parting company a third time.
//
// NOTHING EXISTING MOVES. init refuses to overwrite fklua.toml and refuses per
// file underneath it, so no project that has already been scaffolded is
// reachable by this change; what it decides is where the NEXT one starts.
const guestDir = "guest/go"

// scaffoldGuest writes the guest source tree for a Go project.
//
// REFUSES RATHER THAN OVERWRITES, the same rule fklua.toml follows and for the
// same reason: a guest is hand-written after the first minute of its life, and
// losing one to a re-run of init is not a recoverable mistake. It refuses per
// FILE rather than per directory, so an author who deleted one file gets it
// back without being told to delete the rest.
func scaffoldGuest(modName, guestModule string) ([]string, error) {
	if err := os.MkdirAll(guestDir, 0o755); err != nil {
		return nil, err
	}
	files := []struct{ name, body string }{
		{"go.mod", guestGoMod(modName, guestModule)},
		{"gc.go", guestGCFile},
		{"main.go", guestMainFile(modName)},
	}
	var wrote []string
	for _, f := range files {
		p := filepath.Join(guestDir, f.name)
		if _, err := os.Stat(p); err == nil {
			return wrote, fmt.Errorf("%s already exists; delete it first if you "+
				"meant to start over (fklua.toml was written, so re-running init "+
				"after deleting both is what starts clean)", p)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, p)
	}
	return wrote, nil
}

// guestModulePath turns a mod name into a Go module path.
//
// A mod name is a Factorio identifier and may contain characters a module path
// may not, so it is sanitised rather than trusted -- and the result is a bare
// name rather than a domain, because a scaffolded guest is not published and a
// fake domain in a go.mod is a lie that outlives the scaffold.
func guestModulePath(modName string) string {
	var b strings.Builder
	for _, r := range modName {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "guest"
	}
	return s + "-guest"
}

// GuestSubstrateModule is the module a guest imports for fk and fkgc. Spelled
// once, here, because it appears in generated go.mod, in generated source and
// in init's printed next steps, and three spellings of a module path is three
// chances to typo one.
const GuestSubstrateModule = "github.com/Techrocket9/fklua/guest/go"

func guestGoMod(modName, guestModule string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// The guest is its own Go module, and it has to be:\n")
	fmt.Fprintf(&b, "// //go:wasmimport is rejected outside GOARCH=wasm, so these files cannot\n")
	fmt.Fprintf(&b, "// sit in a module the host toolchain also builds.\n")
	fmt.Fprintf(&b, "module %s\n\ngo 1.24\n", guestModulePath(modName))
	if guestModule == "" {
		fmt.Fprintf(&b, "\n// Run `go mod tidy` in this directory once to pin the FkLua guest\n")
		fmt.Fprintf(&b, "// substrate (%s),\n", GuestSubstrateModule)
		fmt.Fprintf(&b, "// which supplies the fk host bindings and the fkgc collector.\n")
		return b.String()
	}
	// A LOCAL CHECKOUT. Written with an explicit v0.0.0 require beside the
	// replace, because `replace` alone does not add a requirement and the build
	// would fail on a missing module rather than on a missing directory -- which
	// sends a reader to the wrong place entirely.
	fmt.Fprintf(&b, "\n// --guest-module: built against a LOCAL FkLua checkout rather than the\n")
	fmt.Fprintf(&b, "// published module. Delete both lines and run `go mod tidy` to use the\n")
	fmt.Fprintf(&b, "// released substrate instead.\n")
	fmt.Fprintf(&b, "require %s v0.0.0\n\n", GuestSubstrateModule)
	fmt.Fprintf(&b, "replace %s => %s\n", GuestSubstrateModule,
		goSubstrateDir(guestModule))
	return b.String()
}

// goSubstrateDir turns whatever --guest-module was given into the directory that
// actually holds the guest Go module, the way rustSubstrateDir already did for
// the other language.
//
// IT DID NOT EXIST, AND THE GO ARM WROTE THE FLAG VERBATIM. rustSubstrateDir's
// own header says why that is not good enough: ONE FLAG SERVES BOTH LANGUAGES,
// so a `--lang go,rust` project gets one --guest-module and needs two substrates
// out of it, and a FkLua checkout keeps them as siblings -- which means the
// natural thing to pass is the CHECKOUT ROOT. The Rust arm probes three layouts
// and finds it; the Go arm emitted `replace ... => <checkout>`, one path segment
// short of the module's own go.mod, and `go mod tidy` failed with "found ... but
// does not contain package .../fk". Reported by fklua-ports (G5b) as an init
// that scaffolds a project which does not build.
//
// THE TEST THAT WAS SUPPOSED TO COVER IT PASSED BECAUSE IT HANDED init THE
// ANSWER -- `filepath.Join(root, "guest", "go")`, the one layout needing no
// normalisation at all. It passes the checkout root now, which is the case a
// person is in.
//
// The probe asks for go.mod rather than for the directory, because a directory
// that exists and is not a module is the failure this is fixing.
func goSubstrateDir(guestModule string) string {
	if guestModule == "" {
		return ""
	}
	for _, cand := range []string{
		guestModule, // ...checkout/guest/go
		filepath.Join(filepath.Dir(guestModule), "go"), // ...checkout/guest/rust -> go
		filepath.Join(guestModule, "guest", "go"),      // ...checkout
	} {
		if b, err := os.ReadFile(filepath.Join(cand, "go.mod")); err == nil &&
			strings.Contains(string(b), "module "+GuestSubstrateModule) {
			return cand
		}
	}
	// Unresolvable: hand back what was given, so the go.mod names the path the
	// person actually typed and the error points at their own argument rather
	// than at a directory this function invented.
	return guestModule
}

// guestGCFile is guest/go/gc.go: the fkgc import, and the reason it is its own
// file rather than a line in main.go.
//
// It mirrors guest/go/examples/*/gc.go, which every example in this repo
// carries for the same reason -- the whole examples corpus has to be buildable
// under BOTH -gc settings, and an example that cannot be built with the
// collector cannot answer agents/gc.md's stage-B gate.
const guestGCFile = `package main

// The collector.
//
// Under any -gc except custom this package is EMPTY: no symbols, no state, no
// init, and every function below is a no-op that costs a leaking build nothing.
// So the import is unconditional in guest source and turning the collector on
// is genuinely one flag on each side --
//
//	tinygo build -gc=custom ...      and      fklua mod --gc=collected
//
// ...with the second of those coming from ` + "`gc = \"collected\"`" + ` in fklua.toml,
// so you do not type it.
//
// UNDER -gc=custom THIS FILE IS LOAD-BEARING AND ITS ABSENCE IS NOT A WARNING.
// TinyGo's custom-GC seam requires the application to supply seven runtime
// functions by //go:linkname; fkgc is what supplies them. Delete this import
// and a -gc=custom build fails to LINK, with ` + "`missing core function" + `
// ` + "\"runtime.free\"`" + ` from deep inside the builder -- which does not
// mention this file, or fkgc, or the flag.
import "github.com/Techrocket9/fklua/guest/go/fkgc"

// collectStep runs one bounded piece of a collection, and it belongs on a tick.
//
// A collection is cut into steps driven from the host, so the only thing the
// guest owes is a safe point: CollectIfNeeded starts one when the heap has
// grown past fkgc.SetThreshold and otherwise returns immediately. Calling it
// from fk_on_tick is right because an OUTERMOST dispatch is where the shadow
// stack is empty -- see agents/gc.md's safe-point precondition. Calling it
// from inside an event handler that the API re-entered would not be.
//
// Under -gc=leaking this is a call that returns false, and the guest is what it
// was.
func collectStep() bool { return fkgc.CollectIfNeeded() }
`

// guestMainFile is guest/go/main.go: a guest that runs, allocates and collects.
func guestMainFile(modName string) string {
	return fmt.Sprintf(`// Command %[1]s is a FkLua guest: a Go program that becomes a Factorio mod.
//
// Build it, then package it:
//
//	cd guest/go && tinygo build -target=wasm-unknown -scheduler=none \
//	    -gc=custom -opt=2 -o ../../%[1]s.wasm .
//	fklua mod %[1]s.wasm
//
// EVERY ONE OF THOSE TINYGO FLAGS IS LOAD-BEARING and none is stylistic:
// -target=wasm-unknown is the only target whose feature set FkLua compiles;
// -scheduler=none because a Factorio tick cannot block, and with a scheduler a
// parked goroutine is a busy spin inside the game loop; -gc=custom is the
// collector (see gc.go); -opt=2 rather than TinyGo's -opt=z default, which
// optimises for SIZE -- the one cost this target does not have, and worth up to
// 1.7x. The list is guest/go/fk.CollectedBuildFlags in the FkLua tree.
//
// `+"`fklua mod`"+` needs no flags: it reads fklua.toml for this mod's identity,
// its dependencies and its `+"`gc`"+` key.
//
// # Where to go next
//
// This file is the two LEGACY hooks -- fk_on_init once per save, fk_on_tick
// every tick -- and they are the whole of what a scaffold can show without
// picking a mod for you. What a real mod does instead is subscribe to events
// and decode their payloads, and the worked example of that is
// `+"`guest/go/examples/api/`"+` in an FkLua checkout: Subscribe and
// SubscribeFiltered from an initialiser, an fk_on_event switch over generated
// EventXxx ids, ReadOnXxx(ptr) to decode a payload into a generated struct,
// NameFilter, and host-side predicates like `+"`surface.NameIs(...)`"+` that
// answer a string question without copying the string into guest memory. The
// commented-out block below is that shape in miniature.
//
// `+"`guest/rust/examples/api/`"+` is the same guest in Rust. And if this mod
// has to talk to a program OUTSIDE the game, `+"`guest/go/examples/ipc/`"+` is
// the four-line wiring for that -- see the FkIPC section of the README.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// An EVENT-DRIVEN guest, which is what most mods are. Uncomment after
// `+"`fklua gen-bindings`"+` has written guest/go/fkapi/, and add the import.
//
//	func init() {
//		// AN INITIALISER, NOT fk_on_init. init() runs during _initialize,
//		// which control.lua calls on every LOAD; script.on_init fires once,
//		// when the save is CREATED. A subscription made in fk_on_init vanishes
//		// the first time the save is reloaded -- the API calls keep working
//		// and the events silently stop arriving.
//		fkapi.SubscribeFiltered(fkapi.EventOnBuiltEntity,
//			fkapi.NameFilter("iron-chest")...)
//	}
//
//	//go:wasmexport fk_on_event
//	func onEvent(id, ptr uint32) {
//		switch id {
//		case fkapi.EventOnBuiltEntity:
//			e := fkapi.ReadOnBuiltEntity(ptr)
//			ent := fkapi.LuaEntity{Object: e.Entity}
//			_ = ent
//		}
//	}

// Package state SURVIVES A SAVE under the default --persist=table: the whole
// linear memory is carried, so this map and TinyGo's allocator state come back
// as they were. --persist=none is what restores rebuild-from-nothing.
var seen = map[string]int{}

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("%[1]s: hello from Go, running as Lua inside Factorio")
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	// Something to allocate, so the collector this scaffold turned on has
	// something to do. Replace it with your mod.
	seen[bucket(tick)]++

	// ONCE EVERY TEN SECONDS, AND THAT IS THE ONLY REASON THIS SHAPE IS HERE.
	//
	// strconv plus "+" allocates every intermediate string, and a "+" chain in a
	// loop is quadratic in the pieces. Do not lift it onto a path that runs per
	// tick or per event: a downstream mod built its log lines this way, ran one
	// per entity placed, and MEASURED the result as its entire guest heap -- 64
	// MiB of linear memory made of dead strings, and an 18 ms idle worst tick,
	// because Lua's collector walks the memory's SIZE rather than its live part.
	// It costs the collected arm too, as churn the pacer has to keep up with.
	//
	// The fix downstream was a package-level [512]byte, copy() to append and
	// unsafe.String to hand the host a borrow rather than a copy -- worth
	// writing once your mod logs from a hot path, and not before. The budget it
	// is spent against ("0.2 ms of worst tick per MiB of linear memory") is in
	// agents/guests.md under "the guest heap budget".
	if tick%%600 == 0 {
		fk.Log("%[1]s: tick " + strconv.FormatUint(uint64(tick), 10) +
			" fizz=" + strconv.Itoa(seen["fizz"]) +
			" buzz=" + strconv.Itoa(seen["buzz"]) +
			" plain=" + strconv.Itoa(seen["plain"]))
	}

	// ONE BOUNDED PIECE OF A COLLECTION, at a safe point. See gc.go. Leaving
	// this out does not break anything -- it makes the guest a leaking one that
	// paid for a collector, which is the failure with no error message.
	//
	// KEEP THIS CALL UNCONDITIONAL. It is what advances a collection, so a guest
	// that only reaches it from a branch starves its own pacer on exactly the
	// ticks that allocated -- and the symptom is fkgc.Stats().Deadlines rising
	// rather than a pause, which sends you to SetBudget, which is the wrong
	// knob. A real mod hit this by calling it only from its batched-work
	// handler, which runs only when there was batched work.
	collectStep()
}

func bucket(tick uint32) string {
	switch {
	case tick%%15 == 0:
		return "fizzbuzz"
	case tick%%3 == 0:
		return "fizz"
	case tick%%5 == 0:
		return "buzz"
	}
	return "plain"
}

// TinyGo builds this as a c-shared reactor, so main never runs. It exists
// because the package must still be package main.
func main() {}
`, modName)
}

// ---------------------------------------------------------------------------
// The Rust guest, which is the same scaffold with one difference that matters.
//
// A GO GUEST NEEDS AN IMPORT AND A RUST GUEST DOES NOT. -gc=custom fails to LINK
// unless the application supplies TinyGo's seven runtime hooks, which is what
// guest/go/gc.go is for; on the Rust side `fk` owns the single
// `#[global_allocator]` site and `--features fk/fkgc` chooses what backs it. So
// there is no gc.rs, the collector is a build flag alone, and this scaffold's
// job is to put the flag in the printed command and the dependency in
// Cargo.toml.
//
// IT LIVES IN guest/rust/ AND THAT IS THE R8 FIX, not a tidy-up. It used to be
// guest-rs/, chosen so that `--lang go,rust` scaffolds both without one
// overwriting the other -- and that reasoning was sound about collisions and
// missed the thing that mattered: `fklua gen-bindings` writes the Rust bindings
// to guest/rust/fkapi/src/api.rs, a path it HARD-CODES and `fklua lock` hashes
// by exact name. So the two directories never lined up, the scaffolded guest did
// not depend on the bindings, and gen-bindings emitted no Cargo.toml or lib.rs
// for the crate it had just written 2 MB of Rust into. `fklua init --lang rust`
// followed by the `fklua gen-bindings` it printed produced a project that could
// not call the API at all, and making it build meant copying two files out of a
// FkLua checkout by hand.
//
// So: guest/rust/ is a WORKSPACE with two members -- `fkapi`, which gen-bindings
// now writes in full, and the guest crate beside it. That is the layout the
// first Rust port converged on by hand (fklua-ports-samples, AD9), and
// it is the same shape this repo's own guest/rust uses. `--lang go,rust` still
// collides with nothing, and since the Go arm made the same move the two are
// plain siblings rather than one nested inside the other: guest/go beside
// guest/rust, which is where gen-bindings writes both languages' bindings and
// what this repo's own tree looks like.
// ---------------------------------------------------------------------------

// rustGuestDir is the WORKSPACE root; rustCrateDir is the guest crate inside it.
const rustGuestDir = "guest/rust"

func rustCrateDir(modName string) string {
	return filepath.Join(rustGuestDir, rustCrateName(modName))
}

// RustSubstrateGit is where the `fk` crate comes from when there is no local
// checkout to point at.
//
// A git dependency and not a version, because `fk` is NOT PUBLISHED to
// crates.io -- and saying so in the generated file is better than a version
// requirement that would resolve to somebody else's crate of the same name. The
// Go side has a published module and takes one `go mod tidy`; this is the honest
// equivalent, and `--guest-module` is still the flow for a contributor.
const RustSubstrateGit = "https://github.com/Techrocket9/fklua"

// scaffoldRustGuest writes the Rust guest source tree. Refuses rather than
// overwrites, per file, exactly as scaffoldGuest does and for the same reason.
func scaffoldRustGuest(modName, guestModule string) ([]string, error) {
	crate := rustCrateDir(modName)
	if err := os.MkdirAll(filepath.Join(crate, "src"), 0o755); err != nil {
		return nil, err
	}
	files := []struct{ name, body string }{
		// The workspace comes first so that a failure part-way leaves the
		// directory obviously incomplete rather than subtly wrong.
		{filepath.Join(rustGuestDir, "Cargo.toml"), rustWorkspaceCargo(modName, guestModule)},
		{filepath.Join(crate, "Cargo.toml"), rustGuestCargo(modName)},
		{filepath.Join(crate, "src", "lib.rs"), rustGuestLib(modName)},
	}
	var wrote []string
	for _, f := range files {
		p := f.name
		if _, err := os.Stat(p); err == nil {
			return wrote, fmt.Errorf("%s already exists; delete it first if you "+
				"meant to start over (fklua.toml was written, so re-running init "+
				"after deleting both is what starts clean)", p)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, p)
	}
	return wrote, nil
}

// rustCrateName is the mod name as a cargo package name, sanitised the same way
// guestModulePath sanitises it into a Go module path.
//
// rustSubstrateDir turns whatever --guest-module was given into the directory
// holding the Rust workspace, or "" if there is none.
//
// ONE FLAG SERVES BOTH LANGUAGES, and it has to: a `--lang go,rust` project gets
// one --guest-module and needs two substrates out of it. A FkLua checkout keeps
// them as siblings (guest/go and guest/rust), so a path at either one finds the
// other, and a path at the checkout root finds both.
func rustCrateName(modName string) string { return guestModulePath(modName) }

func rustSubstrateDir(guestModule string) string {
	if guestModule == "" {
		return ""
	}
	for _, cand := range []string{
		guestModule, // ...checkout/guest/rust
		filepath.Join(filepath.Dir(guestModule), "rust"), // ...checkout/guest/go -> rust
		filepath.Join(guestModule, "guest", "rust"),      // ...checkout
	} {
		if _, err := os.Stat(filepath.Join(cand, "fk", "Cargo.toml")); err == nil {
			return cand
		}
	}
	return ""
}

// RustGuestArtifact is the wasm a cargo build of the scaffolded guest produces.
//
// A cdylib is named after the [lib] name with DASHES MAPPED TO UNDERSCORES and
// not after the package, and getting that wrong makes init's printed next step a
// command that cannot find its own output.
func RustGuestArtifact(modName string) string {
	return strings.ReplaceAll(rustCrateName(modName), "-", "_") + ".wasm"
}

// rustWorkspaceCargo is guest/rust/Cargo.toml: the two-member workspace.
//
// `fkapi` is a member that DOES NOT EXIST YET when init runs -- `fklua
// gen-bindings` writes it, which is the very next step init prints. That
// ordering is deliberate rather than an oversight: the alternative is for init
// to scaffold a stub crate that gen-bindings then overwrites, and a stub that
// compiles is a stub somebody ships.
func rustWorkspaceCargo(modName, guestModule string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# The guest workspace.\n#\n")
	fmt.Fprintf(&b, "# TWO MEMBERS, AND THE PATHS ARE NOT NEGOTIABLE. `fkapi` is written by\n")
	fmt.Fprintf(&b, "# `fklua gen-bindings` into guest/rust/fkapi/, a path that command hard-codes\n")
	fmt.Fprintf(&b, "# and `fklua lock` hashes by exact name -- so the crate lives there and the\n")
	fmt.Fprintf(&b, "# guest lives beside it. Run `fklua gen-bindings` before the first build:\n")
	fmt.Fprintf(&b, "# until you do, the fkapi member is a directory that does not exist.\n")
	fmt.Fprintf(&b, "[workspace]\nresolver = \"2\"\nmembers = [\"fkapi\", %q]\n\n",
		rustCrateName(modName))
	fmt.Fprintf(&b, "[workspace.dependencies]\n")
	if guestModule == "" {
		fmt.Fprintf(&b, "# `fk` is not published to crates.io, so this is a git dependency rather\n")
		fmt.Fprintf(&b, "# than a version. `--guest-module PATH` writes a path onto a local FkLua\n")
		fmt.Fprintf(&b, "# checkout instead, which is what a contributor wants.\n")
		fmt.Fprintf(&b, "fk = { git = %q }\n", RustSubstrateGit)
	} else {
		fmt.Fprintf(&b, "# --guest-module: a LOCAL FkLua checkout. Replace with\n")
		fmt.Fprintf(&b, "# fk = { git = %q } to build against the published tree.\n", RustSubstrateGit)
		fmt.Fprintf(&b, "fk = { path = %q }\n",
			filepath.ToSlash(filepath.Join(guestModule, "fk")))
	}
	fmt.Fprintf(&b, "fkapi = { path = \"fkapi\" }\n\n")
	fmt.Fprintf(&b, "# NOT PREFERENCES. panic=abort because nothing can unwind across the wasm\n")
	fmt.Fprintf(&b, "# boundary -- FkLua compiles a trap, and a mod that unwinds mid-tick has\n")
	fmt.Fprintf(&b, "# nowhere to unwind to. opt-level=\"s\" and lto because the generated Lua is\n")
	fmt.Fprintf(&b, "# parsed by the game at load, so module size is load time. lto is also what\n")
	fmt.Fprintf(&b, "# lets the event-id constants reach fk.subscribe, which is what prunes the\n")
	fmt.Fprintf(&b, "# whole event descriptor table down to the ones this guest uses.\n")
	fmt.Fprintf(&b, "[profile.release]\nopt-level = \"s\"\nlto = true\npanic = \"abort\"\ncodegen-units = 1\n")
	return b.String()
}

func rustGuestCargo(modName string) string {
	var b strings.Builder
	name := rustCrateName(modName)
	fmt.Fprintf(&b, "# The guest crate. A release build here produces the wasm `fklua mod` packages.\n")
	fmt.Fprintf(&b, "[package]\nname = %q\nversion = \"0.1.0\"\nedition = \"2021\"\n\n", name)
	fmt.Fprintf(&b, "[lib]\nname = %q\ncrate-type = [\"cdylib\"]\n\n",
		strings.ReplaceAll(name, "-", "_"))
	fmt.Fprintf(&b, "# THE COLLECTOR IS A FEATURE ON `fk` AND IS PASSED ON THE COMMAND LINE, NOT\n")
	fmt.Fprintf(&b, "# DECLARED HERE. Cargo's v2 resolver unifies features across every package\n")
	fmt.Fprintf(&b, "# built in one invocation, so a declared features = [\"fkgc\"] would turn the\n")
	fmt.Fprintf(&b, "# collector on for every other crate in the same build -- silently, and only\n")
	fmt.Fprintf(&b, "# for that invocation. Build with:\n#\n")
	fmt.Fprintf(&b, "#     cargo build --release --target wasm32-unknown-unknown --features fk/fkgc\n#\n")
	fmt.Fprintf(&b, "# ...which is what gc = \"collected\" in fklua.toml expects. Drop the flag AND\n")
	fmt.Fprintf(&b, "# set gc = \"leaking\" to opt out; changing one alone is a refusal at package\n")
	fmt.Fprintf(&b, "# time, which is the point of the key.\n")
	fmt.Fprintf(&b, "#\n# `fkapi` is the GENERATED bindings -- `fklua gen-bindings` writes the whole\n")
	fmt.Fprintf(&b, "# crate, manifest included. Without this dependency a guest can log and\n")
	fmt.Fprintf(&b, "# count ticks and cannot call the Factorio API at all.\n")
	fmt.Fprintf(&b, "[dependencies]\nfk = { workspace = true }\nfkapi = { workspace = true }\n")
	return b.String()
}

func rustGuestLib(modName string) string {
	return fmt.Sprintf(rustGuestLibTemplate, modName, RustGuestArtifact(modName), rustGuestDir)
}

const rustGuestLibTemplate = `//! %[1]s -- a FkLua guest: a Rust crate that becomes a Factorio mod.
//!
//! Build it, then package it:
//!
//!     cd %[3]s
//!     cargo build --release --target wasm32-unknown-unknown --features fk/fkgc
//!     fklua mod %[3]s/target/wasm32-unknown-unknown/release/%[2]s
//!
//! "--features fk/fkgc" IS THE COLLECTOR and it is the whole of it: the fk crate
//! owns the single #[global_allocator] site and the feature swaps its bump arena
//! for a paced conservative mark-sweep. There is no import to add and no second
//! flag -- unlike the Go side, where -gc=custom does not even LINK without the
//! fkgc import. Drop the feature and set gc = "leaking" in fklua.toml to opt
//! out; changing one alone is a refusal at package time, which is the point of
//! the key.
//!
//! "fklua mod" needs no flags: it reads fklua.toml for this mod's identity, its
//! dependencies and its gc key.
//!
//! # Subscriptions go in _initialize, not fk_on_init
//!
//! control.lua calls _initialize on every LOAD; script.on_init fires once, when
//! a save is CREATED. A subscription made in fk_on_init vanishes the first time
//! the save is reloaded -- the API calls keep working and the events silently
//! stop arriving, which is a nasty shape of bug to find.
//!
//! # Where to go next
//!
//! This file is the two LEGACY hooks plus one API call, which is the whole of
//! what a scaffold can show without picking a mod for you. What a real mod does
//! instead is subscribe to events and decode their payloads, and the worked
//! example of that is guest/rust/examples/api/ in an FkLua checkout: subscribe
//! and subscribe_filtered from _initialize, an fk_on_event match over generated
//! EVENT_* ids, read_on_xxx(ptr) to decode a payload into a generated struct,
//! name_filter, and host-side predicates that answer a string question without
//! copying the string into guest memory.
//!
//! guest/go/examples/api/ is the same guest in Go. And if this mod has to talk
//! to a program OUTSIDE the game, guest/rust/examples/ipc/ is the four-line
//! wiring for that -- see the FkIPC section of the README.

#![no_std]

extern crate alloc;

use alloc::collections::BTreeMap;
use alloc::string::{String, ToString};
use core::cell::UnsafeCell;

/// State SURVIVES A SAVE under the default --persist=table: the whole linear
/// memory is carried, so this map and the allocator's own state come back as
/// they were. --persist=none is what restores rebuild-from-nothing.
///
/// A static and not a local, which is also what makes it a ROOT: the collector
/// scans [__global_base, __heap_base), and anything reachable only from a wasm
/// local at a dispatch boundary is garbage by definition.
struct Seen(UnsafeCell<Option<BTreeMap<String, u32>>>);
unsafe impl Sync for Seen {}
static SEEN: Seen = Seen(UnsafeCell::new(None));

#[allow(clippy::mut_from_ref)]
fn seen() -> &'static mut BTreeMap<String, u32> {
    unsafe { (*SEEN.0.get()).get_or_insert_with(BTreeMap::new) }
}

#[no_mangle]
pub extern "C" fn _initialize() {
    // Subscribe to events here. See the module docs for why not fk_on_init.
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    fk::log("%[1]s: hello from Rust, running as Lua inside Factorio");

    // ...AND ONE CALL THROUGH THE GENERATED BINDINGS, so that a fresh project
    // proves the whole chain rather than only the logging half.
    //
    // fkapi is the crate "fklua gen-bindings" writes into guest/rust/fkapi --
    // manifest, lib.rs and 2 MB of api.rs -- and it is a workspace member
    // beside this one. GAME is handle 2, fixed by the ABI; every other handle is
    // reached by calling something.
    //
    // A host call NEVER raises into wasm: there are no coroutines, so an error
    // crossing that boundary could not unwind the frame it came from. It arrives
    // as a Status.
    match fkapi::GAME.tick() {
        Ok(t) => {
            let mut line = String::from("%[1]s: game.tick = ");
            push_u32(&mut line, t as u32);
            fk::log(&line);
        }
        Err(e) => fk::log(&["%[1]s: reading game.tick failed: ", e.as_str()].concat()),
    }
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    // Something to allocate, so the collector this scaffold turned on has
    // something to do. Replace it with your mod.
    *seen().entry(bucket(tick).to_string()).or_insert(0) += 1;

    if tick %% 600 == 0 {
        let mut line = String::from("%[1]s: tick ");
        push_u32(&mut line, tick);
        for k in ["fizz", "buzz", "plain"] {
            line.push(' ');
            line.push_str(k);
            line.push('=');
            push_u32(&mut line, *seen().get(k).unwrap_or(&0));
        }
        fk::log(&line);
    }

    // ONE BOUNDED PIECE OF A COLLECTION, AT A SAFE POINT.
    //
    // An OUTERMOST dispatch is where the shadow stack is empty and every live
    // reference is in a static or in the heap -- which is what makes a
    // conservative scan sound at all. Calling this from inside a handler the API
    // re-entered would not be.
    //
    // KEEP IT UNCONDITIONAL. It is what advances a collection, so a guest that
    // only reaches it from a branch starves its own pacer on exactly the ticks
    // that allocated -- and the symptom is fk::gc::deadlines() rising rather
    // than a pause, which sends you to set_budget, which is the wrong knob.
    //
    // Without the fkgc feature this is a call that returns false.
    fk::gc::collect_if_needed();
}

fn bucket(tick: u32) -> &'static str {
    match tick {
        t if t %% 15 == 0 => "fizzbuzz",
        t if t %% 3 == 0 => "fizz",
        t if t %% 5 == 0 => "buzz",
        _ => "plain",
    }
}

/// Decimal, without pulling in core::fmt's machinery -- which is a real cost in
/// a module the game parses as Lua at load.
fn push_u32(s: &mut String, mut v: u32) {
    if v == 0 {
        s.push('0');
        return;
    }
    let mut buf = [0u8; 10];
    let mut i = buf.len();
    while v > 0 {
        i -= 1;
        buf[i] = b'0' + (v %% 10) as u8;
        v /= 10;
    }
    s.push_str(core::str::from_utf8(&buf[i..]).unwrap_or("?"));
}
`
