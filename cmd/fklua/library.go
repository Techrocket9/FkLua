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
func initLibrary(name string, langs []string, guestModule string) error {
	if len(langs) != 1 {
		return fmt.Errorf("--library scaffolds ONE language per directory; a " +
			"two-language library is two sibling trees with a mirror test between " +
			"them (the fkipc arrangement), so run init --library twice, in " +
			"sibling directories")
	}
	pkg := libraryPackageName(name)
	var files []struct{ name, body string }
	switch langs[0] {
	case "go":
		files = []struct{ name, body string }{
			{"go.mod", libraryGoMod(name, guestModule)},
			{pkg + ".go", libraryGoPure(name, pkg)},
			{"guest.go", libraryGoGuest(pkg)},
			{pkg + "_test.go", libraryGoTest(pkg)},
		}
	case "rust":
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
	return nil
}

func libraryGoMod(name, guestModule string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// A guest library is its own Go module. The module path below is a\n")
	fmt.Fprintf(&b, "// PLACEHOLDER: rename it to the path you will publish under, or your\n")
	fmt.Fprintf(&b, "// consumers cannot `go get` it.\n")
	fmt.Fprintf(&b, "module %s\n\ngo 1.24\n", libraryPackageName(name))
	if guestModule == "" {
		fmt.Fprintf(&b, "\n// Run `go mod tidy` once to pin the FkLua guest substrate\n")
		fmt.Fprintf(&b, "// (%s), which supplies fk.\n", GuestSubstrateModule)
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
# WASM-GATED, the fkipc arrangement: on the host this crate has no
# dependencies at all, which is what lets ` + "`cargo test`" + ` run the pure half
# with no wasm target installed.
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
