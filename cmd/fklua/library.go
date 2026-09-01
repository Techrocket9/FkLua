package main

// `fklua init --library`: scaffold a GUEST LIBRARY rather than a mod.
//
// A library is a Go package or a Rust crate that a consumer's guest imports and
// that `fklua mod` then compiles into the consumer's one wasm -- the fklog /
// fkipc shape, made available to a third party. It is NOT a mod: there is no
// fklua.toml to write, no gc key, no identity, nothing to package. What a
// library scaffold is FOR is the composition contract, which exists as prose
// in this repo and nowhere a third-party author would meet it: the rules below
// are baked into the generated files as the comments an author reads first,
// because the recorded history of every one of them is that a library author
// discovers it after shipping a violation.
//
// THE CONTRACT THE TEMPLATES CARRY, and where each rule earned its place:
//   - ROUTE, NEVER OWN. A wasm module has one export per name, so a library
//     that exports fk_on_tick takes the export away from the mod that imports
//     it. The convention is fkipc's: the consumer owns the hooks and routes in,
//     and a library's entry points return whether they handled the call.
//   - IDS STAY INLINE. The packager prunes the member/event/define tables by
//     scanning for compile-time-constant ids; a wrapper that stops inlining
//     silently ships the full tables (~55 KB-1 MB per load). Rust wrappers
//     carrying an id are #[inline(always)]; Go wrappers stay small.
//   - THE CONSUMER'S BINDINGS, NEVER YOUR OWN. fkapi is generated per API pin
//     and two binding sets in one module are refused at package time. A
//     library that depends on `fk` alone is pin-transparent; one that needs
//     fkapi imports the CONSUMER's copy and inherits the consumer's pin.
//   - JOIN SAFETY PASSES THROUGH. Never store an outbound call's outcome,
//     never keep anything computed under a load hook, never iterate a hash map
//     to decide what to write. The consumer cannot see a library break these.
//   - THE PURE HALF IS HOST-TESTABLE. Host imports are rejected off-target, so
//     the state machine lives in ordinary code and the host-import glue lives
//     behind the build gate -- which is also what makes the library testable
//     with plain `go test` / `cargo test`.
//
// ONE LANGUAGE PER SCAFFOLD, refused otherwise rather than guessed at: a
// two-language library is two sibling trees with a mirror test between them
// (the fkipc arrangement), and inventing that layout here would be scaffolding
// a shape no third-party library has used yet -- the exact mistake init made
// once with guest-rs/. Run it twice, in sibling directories.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// libraryPackageName turns a library name into a Go package / Rust crate
// identifier: lowercase, '-' to '_', anything else dropped.
func libraryPackageName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r == '-':
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "lib"
	}
	return s
}

// initLibrary is `fklua init --library NAME`: writes the scaffold into the
// CURRENT directory, exactly as init-for-a-mod does, and refuses to overwrite
// anything that exists.
func initLibrary(name string, langs []string, guestModule string, data bool) error {
	if len(langs) != 1 {
		return fmt.Errorf("--library scaffolds ONE language per directory; a " +
			"two-language library is two sibling trees with a mirror test between " +
			"them (the fkipc arrangement), so run init --library twice, in " +
			"sibling directories")
	}
	pkg := libraryPackageName(name)
	var files []struct{ name, body string }
	switch {
	case langs[0] == "go" && data:
		files = []struct{ name, body string }{
			{"go.mod", libraryGoMod(name, guestModule, "fkdata")},
			{pkg + ".go", libraryGoDataPure(name, pkg)},
			{"guest.go", libraryGoDataGuest(pkg)},
			{pkg + "_test.go", libraryGoDataTest(pkg)},
		}
	case langs[0] == "go":
		files = []struct{ name, body string }{
			{"go.mod", libraryGoMod(name, guestModule, "fk")},
			{pkg + ".go", libraryGoPure(name, pkg)},
			{"guest.go", libraryGoGuest(pkg)},
			{pkg + "_test.go", libraryGoTest(pkg)},
		}
	case langs[0] == "rust" && data:
		files = []struct{ name, body string }{
			{"Cargo.toml", libraryCargoTomlData(pkg, guestModule)},
			{filepath.Join("src", "lib.rs"), libraryRustDataLib(name)},
		}
	case langs[0] == "rust":
		files = []struct{ name, body string }{
			{"Cargo.toml", libraryCargoToml(pkg, guestModule)},
			{filepath.Join("src", "lib.rs"), libraryRustLib(name)},
		}
	default:
		return fmt.Errorf("--library supports go or rust, not %q", langs[0])
	}
	var wrote []string
	for _, f := range files {
		if dir := filepath.Dir(f.name); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if _, err := os.Stat(f.name); err == nil {
			return fmt.Errorf("%s already exists; delete it first if you meant to "+
				"start over", f.name)
		}
		if err := os.WriteFile(f.name, []byte(f.body), 0o644); err != nil {
			return err
		}
		wrote = append(wrote, f.name)
	}
	for _, f := range wrote {
		fmt.Printf("wrote %s\n", f)
	}
	fmt.Printf("\nA guest LIBRARY, not a mod: there is no fklua.toml because there is\n")
	fmt.Printf("nothing to package -- a consumer imports this and their `fklua mod`\n")
	fmt.Printf("compiles it into their wasm. The composition contract is in the\n")
	fmt.Printf("generated comments; the pure half is testable on the host today:\n")
	if langs[0] == "go" {
		fmt.Printf("    go test ./...\n")
	} else {
		fmt.Printf("    cargo test\n")
	}
	if data {
		fmt.Printf("\nDATA flavor: consumers call this from their own data guest's stage\n")
		fmt.Printf("hooks (fk_data and friends); see docs/data-stage.md in the FkLua repo.\n")
	}
	return nil
}

func libraryGoMod(name, guestModule, dep string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// A guest library is its own Go module. The module path below is a\n")
	fmt.Fprintf(&b, "// PLACEHOLDER: rename it to the path you will publish under, or your\n")
	fmt.Fprintf(&b, "// consumers cannot `go get` it. If this directory is a SUBDIRECTORY of\n")
	fmt.Fprintf(&b, "// your repository (the sibling go/ and rust/ arrangement this scaffold\n")
	fmt.Fprintf(&b, "// itself suggests for a two-language library), the module path must end\n")
	fmt.Fprintf(&b, "// with that directory -- <repo-path>/go -- or Go cannot resolve it.\n")
	fmt.Fprintf(&b, "module %s\n\ngo 1.24\n", libraryPackageName(name))
	if guestModule == "" {
		fmt.Fprintf(&b, "\n// Run `go mod tidy` once to pin the FkLua guest substrate\n")
		fmt.Fprintf(&b, "// (%s), which supplies %s.\n", GuestSubstrateModule, dep)
		return b.String()
	}
	fmt.Fprintf(&b, "\n// --guest-module: built against a LOCAL FkLua checkout rather than the\n")
	fmt.Fprintf(&b, "// published module. Delete both lines and run `go mod tidy` to use the\n")
	fmt.Fprintf(&b, "// released substrate instead. NOTE a `replace` does not travel to your\n")
	fmt.Fprintf(&b, "// consumers: publishing this library means depending on a real version.\n")
	fmt.Fprintf(&b, "require %s v0.0.0\n\n", GuestSubstrateModule)
	fmt.Fprintf(&b, "replace %s => %s\n", GuestSubstrateModule, goSubstrateDir(guestModule))
	return b.String()
}

func libraryGoPure(name, pkg string) string {
	return `// Package ` + pkg + ` is a FkLua guest library: a Go package a mod's guest
// imports, compiled into the CONSUMER's wasm by their own ` + "`fklua mod`" + `.
//
// THE COMPOSITION CONTRACT, in the order it bites:
//
//   - ROUTE, NEVER OWN. Never export a hook (fk_on_tick, fk_on_event,
//     fk_on_init, fk_alloc, ...): a wasm module has ONE export per name, so an
//     export here takes it away from the consuming mod. The consumer owns the
//     hooks and routes in -- OnEvent below returns whether this library
//     handled the call, which is what lets several libraries share one mod.
//   - THE CONSUMER'S BINDINGS, NEVER YOUR OWN. Depending on ` + "`fk`" + ` alone keeps
//     this library PIN-TRANSPARENT: no API version can break it. If it ever
//     needs the generated fkapi bindings, import the CONSUMER's copy (the
//     canonical module path) and never vendor one -- two binding sets in one
//     module are refused at package time, and ids are pin-relative.
//   - IDS STAY INLINE. The packager prunes the member/event/define tables by
//     scanning for compile-time-constant ids. Keep any wrapper that carries an
//     id small enough to inline, never loop over ids, and write calls out --
//     the failure is silent and costs every consumer the full tables in every
//     save and download.
//   - JOIN SAFETY PASSES THROUGH. Never store whether an outbound host call
//     succeeded (that is a fact about one peer), never keep anything computed
//     under a load hook, and never iterate a Go map to decide what to write.
//     A consumer cannot see this library break these rules; the symptom is a
//     multiplayer desync with their mod's name on it.
//
// The pure half lives here and is testable with plain ` + "`go test`" + `; everything
// that touches a host import lives in guest.go behind the build gate.
package ` + pkg + `

// Lib is the library's state. Everything here lives in the guest heap and
// therefore in every save: allocate on a schedule you chose, not per tick.
type Lib struct {
	ticks uint32
}

// OnTick is an example entry point the consumer calls from its own fk_on_tick
// export. Pure, so it is host-testable.
func (l *Lib) OnTick() uint32 {
	l.ticks++
	return l.ticks
}

// OnEvent is the routing convention: the consumer's fk_on_event calls every
// library it imports in turn, and the return says whether this one handled
// the event -- so libraries compose without owning the export.
func (l *Lib) OnEvent(id uint32, ptr uint32) bool {
	_, _ = id, ptr
	return false
}
`
}

func libraryGoGuest(pkg string) string {
	return `//go:build tinygo.wasm

// The guest-only half: everything that touches a host import lives behind
// this build gate, which is what keeps the pure half testable with plain
// ` + "`go test`" + ` on the host (//go:wasmimport is rejected off-target).
//
// To call the Factorio API from here, import the consumer's generated
// bindings by their canonical path -- and read the contract in the package
// doc first: that import makes this library PIN-COUPLED, and it must never
// ship its own copy of fkapi.
package ` + pkg + `

import "github.com/Techrocket9/fklua/guest/go/fk"

// Announce is an example of guest-only glue over the pure state.
func (l *Lib) Announce() {
	fk.Log("` + pkg + `: tick counter is running")
}
`
}

func libraryGoTest(pkg string) string {
	return `package ` + pkg + `

import "testing"

// The pure half runs on the host, which is the property the file layout
// exists to keep: a library whose logic needs a wasm target to test is a
// library whose logic does not get tested.
func TestOnTickCounts(t *testing.T) {
	var l Lib
	if l.OnTick() != 1 || l.OnTick() != 2 {
		t.Fatal("OnTick does not count")
	}
}
`
}

func libraryCargoToml(pkg, guestModule string) string {
	dep := `fk = { git = "` + RustSubstrateGit + `" }`
	note := `# The fk dependency resolves from the FkLua repository. ONE SOURCE RULE:
# if your consumer uses a vendored FkLua checkout (a path dependency), they
# must [patch] this git source onto their path -- two fk crates in one build
# is a duplicate #[global_allocator] link error.`
	if guestModule != "" {
		if d := rustSubstrateDir(guestModule); d != "" {
			dep = `fk = { path = "` + filepath.ToSlash(filepath.Join(d, "fk")) + `" }`
			note = `# --guest-module: fk resolves from a LOCAL FkLua checkout. Before
# publishing, switch this to the git source (and read the one-source rule in
# src/lib.rs): a path into your machine travels to nobody.`
		}
	}
	return `[package]
name = "` + pkg + `"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["rlib"]

` + note + `
#
# WASM-GATED, the fkipc arrangement: on the host nothing below COMPILES,
# which is what lets ` + "`cargo test`" + ` run the pure half with no wasm toolchain
# installed. Cargo still RESOLVES the graph, so a git source is cloned once
# even for a host test run: the first run needs the network, later ones do
# not.
[target.'cfg(target_family = "wasm")'.dependencies]
` + dep + `

# NEVER declare fk/fkgc here. Cargo unifies features across the consumer's
# whole build, so a library that turned the collector on would turn it on for
# every crate in the graph -- whether the mod is collected is the CONSUMER's
# decision.
`
}

func libraryRustLib(name string) string {
	pkg := libraryPackageName(name)
	return `//! A FkLua guest library: a crate a mod's guest imports, compiled into the
//! CONSUMER's wasm by their own ` + "`fklua mod`" + `.
//!
//! THE COMPOSITION CONTRACT, in the order it bites:
//!
//! - ROUTE, NEVER OWN. Never export a hook (` + "`fk_on_tick`" + `, ` + "`fk_on_event`" + `,
//!   ` + "`fk_alloc`" + `, ...): a wasm module has ONE export per name, so a
//!   ` + "`#[no_mangle]`" + ` export here takes it away from the consuming mod. The
//!   consumer owns the hooks and routes in; entry points return whether this
//!   library handled the call.
//! - THE CONSUMER'S BINDINGS, NEVER YOUR OWN. Depending on ` + "`fk`" + ` alone keeps
//!   this library PIN-TRANSPARENT. If it ever needs the generated ` + "`fkapi`" + `,
//!   depend on the CONSUMER's copy and never vendor one -- two binding sets
//!   in one module are refused at package time, and ids are pin-relative.
//! - IDS STAY INLINE, AND IN RUST THAT IS AN ATTRIBUTE. Any wrapper that
//!   carries a member/event/define id is ` + "`#[inline(always)]`" + ` -- not
//!   ` + "`#[inline]`" + ` -- or whether the id reaches the import as a constant
//!   becomes rustc's cost heuristic's decision, per call site, and the
//!   consumer silently ships the full tables.
//! - JOIN SAFETY PASSES THROUGH. Never store whether an outbound host call
//!   succeeded, never keep anything computed under a load hook, never iterate
//!   a HashMap to decide what to write. The symptom of breaking these is a
//!   multiplayer desync with the CONSUMER's name on it.
//! - ONE ` + "`fk`" + ` SOURCE. Never declare the ` + "`fk/fkgc`" + ` feature (the consumer's
//!   graph decides), and document that a consumer on a vendored checkout must
//!   ` + "`[patch]`" + ` this crate's git source onto their path -- two ` + "`fk`" + ` crates is
//!   a duplicate ` + "`#[global_allocator]`" + ` link error.
//!
//! The pure half is below and runs under plain ` + "`cargo test`" + `; everything that
//! touches a host import lives behind ` + "`cfg(target_family = \"wasm\")`" + `.

// no_std in the consumer's wasm, std under ` + "`cargo test`" + ` -- the conditional is
// what lets the ordinary host test harness link while the shipped crate stays
// allocator-disciplined.
#![cfg_attr(not(test), no_std)]

/// The library's state. Everything here lives in the guest heap and therefore
/// in every save: allocate on a schedule you chose, not per tick.
#[derive(Default)]
pub struct Lib {
    ticks: u32,
}

impl Lib {
    /// An example entry point the consumer calls from its own fk_on_tick
    /// export. Pure, so it is host-testable.
    pub fn on_tick(&mut self) -> u32 {
        self.ticks += 1;
        self.ticks
    }

    /// The routing convention: the consumer's fk_on_event calls every
    /// library it imports in turn, and the return says whether this one
    /// handled the event -- so libraries compose without owning the export.
    pub fn on_event(&mut self, _id: u32, _ptr: u32) -> bool {
        false
    }
}

/// Guest-only glue over the pure state; host builds never see it.
#[cfg(target_family = "wasm")]
impl Lib {
    pub fn announce(&self) {
        fk::log("` + pkg + `: tick counter is running");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The pure half runs on the host, which is the property the layout
    /// exists to keep.
    #[test]
    fn on_tick_counts() {
        let mut l = Lib::default();
        assert_eq!(l.on_tick(), 1);
        assert_eq!(l.on_tick(), 2);
    }
}
`
}

// ---------------------------------------------------------------------------
// The --data flavor: a DATA-STAGE library.
//
// FkRecipes' dogfood report: the control-flavored scaffold (fk dependency,
// OnTick/OnEvent examples) misled a data library's build in both languages,
// and both halves needed hand-rewriting. A data library's contract differs in
// every load-bearing way -- fkdata not fk, NEVER fkapi (refused at package
// time), the consumer's STAGE exports as the routing surface, the
// plan-then-Emit split, Raise as the diagnostic exit, ModName as the prefix
// source -- so it is a template of its own rather than a sentence bolted onto
// the control one.
// ---------------------------------------------------------------------------

func libraryGoDataPure(name, pkg string) string {
	return `// Package ` + pkg + ` is a FkLua DATA-STAGE guest library: a Go package a
// mod's DATA guest imports, compiled into the consumer's data module by their
// own ` + "`fklua mod`" + `.
//
// THE DATA-LIBRARY CONTRACT, in the order it bites:
//
//   - PLAN, THEN EMIT. Everything before Emit is ordinary values in
//     DECLARATION ORDER, host-testable with plain ` + "`go test`" + ` -- the planner
//     and its validation live here; Emit (guest.go) is the only
//     fkdata-touching call. Never iterate a Go map to decide what to emit:
//     the data stage crosses sorted, and a map walk is a per-client order.
//   - ROUTE, NEVER OWN. The stage exports (fk_settings, fk_data,
//     fk_data_updates, fk_data_final_fixes) are the CONSUMER's; their data
//     guest calls into this library from its own hooks.
//   - NEVER fkapi. There is no runtime API at these stages and ` + "`fklua mod`" + `
//     REFUSES a data module that imports it -- which is also what keeps this
//     library pin-transparent: no API version can break it.
//   - PREFIX FROM ModName. Setting and prototype names are global namespaces
//     and a same-type collision between two mods is silent last-writer-wins,
//     so everything emitted derives its prefix from fkdata.ModName() rather
//     than taking a parameter that can drift from the packaged mod.
//   - DIAGNOSE WITH Raise. A guest panic surfaces in the player's game as an
//     opaque trap with your message lost in the log; fkdata.Raise stops the
//     load with YOUR diagnostic, stage-prefixed like every host failure.
//     Because the host adds the stage ("fklua: at the <stage> stage, " goes
//     in front of your text verbatim), a refusal built here carries none.
package ` + pkg + `

// Plan collects what to emit, in declaration order. Pure, so every rule the
// planner enforces is testable on the host.
type Plan struct {
	items []string
}

// Item queues one item prototype by its UNPREFIXED name; Emit derives the
// prefix from the packaged mod.
func (p *Plan) Item(name string) {
	p.items = append(p.items, name)
}

// Items is the queued names, in declaration order.
func (p *Plan) Items() []string {
	return p.items
}
`
}

func libraryGoDataGuest(pkg string) string {
	return `//go:build tinygo.wasm

// The guest-only half: the ONLY file that touches fkdata, which is what keeps
// the planner host-testable. To surface a validation failure from here, use
// fkdata.Raise: your message arrives stage-prefixed where a panic would be an
// opaque trap.
package ` + pkg + `

import "github.com/Techrocket9/fklua/guest/go/fkdata"

// Emit lands the plan in data.raw. Call it from the consumer's own fk_data
// export. The prefix comes from the packaged mod's name -- see the contract
// in the package doc.
func (p *Plan) Emit() {
	prefix := fkdata.ModName() + "-"
	for _, name := range p.items {
		fkdata.Extend(fkdata.Obj(
			fkdata.KVs("type", fkdata.Str("item")),
			fkdata.KVs("name", fkdata.Str(prefix+name)),
			fkdata.KVs("icon", fkdata.Str("__core__/graphics/empty.png")),
			fkdata.KVs("icon_size", fkdata.Num(1)),
			fkdata.KVs("stack_size", fkdata.Num(50)),
		))
	}
}
`
}

func libraryGoDataTest(pkg string) string {
	return `package ` + pkg + `

import "testing"

// The planner runs on the host, which is the property the file split exists
// to keep: emission order is declaration order, never a map walk.
func TestItemsKeepDeclarationOrder(t *testing.T) {
	var p Plan
	p.Item("gear")
	p.Item("axle")
	got := p.Items()
	if len(got) != 2 || got[0] != "gear" || got[1] != "axle" {
		t.Fatalf("declaration order was not kept: %v", got)
	}
}
`
}

func libraryCargoTomlData(pkg, guestModule string) string {
	dep := `fkdata = { git = "` + RustSubstrateGit + `" }`
	note := `# The fkdata dependency resolves from the FkLua repository. ONE SOURCE
# RULE: if your consumer uses a vendored FkLua checkout (path dependencies),
# they must [patch] this git source onto their path -- two copies of the
# substrate crates in one build is a duplicate-symbol link error.`
	if guestModule != "" {
		if d := rustSubstrateDir(guestModule); d != "" {
			dep = `fkdata = { path = "` + filepath.ToSlash(filepath.Join(d, "fkdata")) + `" }`
			note = `# --guest-module: fkdata resolves from a LOCAL FkLua checkout. Before
# publishing, switch this to the git source; a path into your machine
# travels to nobody.`
		}
	}
	return `[package]
name = "` + pkg + `"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["rlib"]

` + note + `
#
# WASM-GATED, so the planner tests on the host with no wasm toolchain
# installed (cargo still fetches a git source once to resolve the graph).
# NEVER add fkapi here: there is no runtime API at the data stages and
# fklua mod refuses a data module that imports it.
[target.'cfg(target_family = "wasm")'.dependencies]
` + dep + `
`
}

func libraryRustDataLib(string) string {
	return `//! A FkLua DATA-STAGE guest library: a crate a mod's DATA guest imports,
//! compiled into the consumer's data module by their own ` + "`fklua mod`" + `.
//!
//! THE DATA-LIBRARY CONTRACT, in the order it bites:
//!
//! - PLAN, THEN EMIT. Everything before ` + "`emit`" + ` is ordinary values in
//!   DECLARATION ORDER, host-testable under plain ` + "`cargo test`" + `; the emit
//!   impl below is the only fkdata-touching code. Never iterate a HashMap to
//!   decide what to emit: the data stage crosses sorted, and a map walk is a
//!   per-client order.
//! - ROUTE, NEVER OWN. The stage exports (` + "`fk_settings`" + `, ` + "`fk_data`" + `, ...)
//!   are the CONSUMER's; their data guest calls into this library.
//! - NEVER fkapi. There is no runtime API at these stages and ` + "`fklua mod`" + `
//!   REFUSES a data module that imports it -- which is also what keeps this
//!   library pin-transparent.
//! - PREFIX FROM mod_name. Setting and prototype names are global namespaces
//!   and a same-type collision is silent last-writer-wins, so everything
//!   emitted derives its prefix from ` + "`fkdata::mod_name()`" + `.
//! - DIAGNOSE WITH raise. A panic surfaces as an opaque trap with your
//!   message lost in the log; ` + "`fkdata::raise`" + ` stops the load with YOUR
//!   diagnostic, stage-prefixed like every host failure. It never returns.
//!   Because the host adds the stage ("fklua: at the <stage> stage, " goes
//!   in front of your text verbatim), a refusal built here carries none.

// no_std in the consumer's wasm, std under ` + "`cargo test`" + `.
#![cfg_attr(not(test), no_std)]

extern crate alloc;

use alloc::string::String;
use alloc::vec::Vec;

/// Collects what to emit, in declaration order. Pure, so every rule the
/// planner enforces is testable on the host.
#[derive(Default)]
pub struct Plan {
    items: Vec<String>,
}

impl Plan {
    /// Queues one item prototype by its UNPREFIXED name; emit derives the
    /// prefix from the packaged mod.
    pub fn item(&mut self, name: &str) {
        self.items.push(String::from(name));
    }

    /// The queued names, in declaration order.
    pub fn items(&self) -> &[String] {
        &self.items
    }
}

/// The only fkdata-touching code; host builds never see it. Call from the
/// consumer's own fk_data export.
#[cfg(target_family = "wasm")]
impl Plan {
    pub fn emit(&self) {
        let prefix = fkdata::mod_name();
        for name in &self.items {
            let full = alloc::format!("{prefix}-{name}");
            fkdata::extend(&[fkdata::obj(&[
                ("type", fkdata::str_("item")),
                ("name", fkdata::str_(&full)),
                ("icon", fkdata::str_("__core__/graphics/empty.png")),
                ("icon_size", fkdata::num(1.0)),
                ("stack_size", fkdata::num(50.0)),
            ])]);
        }
    }
}

#[cfg(test)]
mod data_tests {
    use super::*;

    /// Emission order is declaration order, never a map walk.
    #[test]
    fn items_keep_declaration_order() {
        let mut p = Plan::default();
        p.item("gear");
        p.item("axle");
        assert_eq!(p.items(), ["gear", "axle"]);
    }
}
`
}
