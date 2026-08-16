package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
)

// THE SILENT-SKIP GUARD FOR THIS PACKAGE, and it is here for one test rather
// than fifteen because one test is all it takes.
//
// TestCollectedIsAcceptedForAGuestCarryingTheCollector has two halves and only
// the second is load-bearing: the stand-in half pins the RULE against a WAT
// export list, and the real-build half is what says the rule's required list is
// the list a genuinely collected guest has -- which no amount of WAT can
// establish, and which is precisely the thing that was wrong before the gate
// existed. Without tinygo that half skipped, in silence, and the test still
// reported PASS. `go test` prints nothing for a skip without -v, so the
// package's `ok` line looked identical either way.
//
// Same treatment as internal/guest.TestTheGuestToolchainIsAvailable and
// internal/luahost.TestTheOracleIsBuilt: the absence is reported ONCE, by
// something that FAILS, with `-short` as the way to say you meant it. A passing
// test that writes a banner was tried in stage D and does not work -- `go test`
// captures the binary's output and prints it only when the package fails.
func TestTheGuestToolchainIsAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: not requiring a guest toolchain")
	}
	if guest.ToolchainDeclaredAbsent() {
		t.Skipf("%s: this environment declares no guest toolchain", guest.NoToolchainEnv)
	}
	if ok, why := guest.Available(); !ok {
		t.Fatalf("THE GUEST TOOLCHAIN IS MISSING, so the half of "+
			"TestCollectedIsAcceptedForAGuestCarryingTheCollector that builds a "+
			"REAL collected guest just SKIPPED -- leaving only the WAT stand-in, "+
			"which cannot say that the required export list is the list a real "+
			"collector has. That is the exact gap the gate was added to close, "+
			"and the test still reported PASS.\n\n  %s\n\n"+
			"Install it, or run `go test -short ./...` to say you meant to skip "+
			"it. A skipped test prints nothing without -v, which is why this is a "+
			"failure and not a log line.", why)
	}
}

// gcModule decodes a WAT body so checkGC can be asked about a shape rather than
// about a build. The shapes below are export lists, and an export list is the
// whole of what this gate reads.
func gcModule(t *testing.T, wat string) *ir.Module {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	if err := os.WriteFile(p, []byte(wat), 0o644); err != nil {
		t.Fatal(err)
	}
	im, err := loadModule(p)
	if err != nil {
		t.Fatalf("decoding the stand-in guest: %v", err)
	}
	return im
}

// A Rust guest: wasm32-unknown-unknown, no WASI import anywhere, and the fk
// host boundary but nothing of the collector. This is the module the old gate
// waved through -- it looked for wasi_snapshot_preview1, found none, and said
// yes to a guest for which no collector exists in ANY toolchain.
const rustShapedWAT = `(module (memory 1)
  (func (export "fk_on_init"))
  (func (export "fk_on_tick"))
  (func (export "fk_alloc") (param i32) (result i32) i32.const 0)
  (func (export "fk_free") (param i32))
  (func (export "fk_scratch_base") (result i32) i32.const 0)
  (func (export "fk_scratch_size") (result i32) i32.const 0))`

// A TinyGo guest built the default way -- -gc=leaking, so _initialize is there
// and the fkgc exports are not. Same verdict, different reason to reach it: the
// author passed --gc=collected to the compiler and not -gc=custom to TinyGo.
const goLeakingWAT = `(module (memory 1)
  (func (export "_initialize"))
  (func (export "fk_on_tick"))
  (func (export "fk_alloc") (param i32) (result i32) i32.const 0)
  (func (export "fk_alloc_static") (param i32) (result i32) i32.const 0)
  (func (export "fk_free") (param i32))
  (func (export "fk_scratch_base") (result i32) i32.const 0)
  (func (export "fk_scratch_size") (result i32) i32.const 0))`

// A guest carrying the collector: the three exports control.lua binds, and
// nothing else about it matters.
const goCollectedWAT = `(module (memory 1)
  (func (export "_initialize"))
  (func (export "fk_on_tick"))
  (func (export "fk_gc_step") (param i32) (result i32) i32.const 0)
  (func (export "fk_gc_dirty_base") (result i32) i32.const 0)
  (func (export "fk_gc_dirty_cap") (result i32) i32.const 0))`

// A wasip1 guest that DOES carry the collector, which is the only shape that
// reaches the first refusal: fkgc links on wasip1, so the surface check would
// wave this through and the import is what does not.
const wasip1CollectedWAT = `(module
  (import "wasi_snapshot_preview1" "fd_write"
    (func (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "_initialize"))
  (func (export "fk_on_tick"))
  (func (export "fk_gc_step") (param i32) (result i32) i32.const 0)
  (func (export "fk_gc_dirty_base") (result i32) i32.const 0)
  (func (export "fk_gc_dirty_cap") (result i32) i32.const 0))`

// A REFUSAL NAMES BOTH SIDES AND BOTH WAYS OUT, in every arm.
//
// A gc mismatch is made of two facts -- what asked for the mode, and what the
// artefact actually is -- and a message that carries one of them tells a reader
// they lost without telling them which half to change. The first-time report
// this test comes from called the refusal "slightly cryptic"; it named the
// missing exports and how to build a collector in either language, and left the
// reader to infer that building one and not asking for one were alternatives.
//
// Structural rather than verbatim: the two labelled lines and the two numbered
// remedies are the contract, and the prose between them is free to change. What
// is NOT free is the manifest arm naming a --gc flag the author never typed --
// the first thing anybody does with a message naming a flag is search their
// command line for it.
func TestARefusedGCModeNamesBothSidesAndBothWaysOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		wat  string
	}{
		{"no collector in the module", goLeakingWAT},
		{"a collector, but on wasip1", wasip1CollectedWAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := gcModule(t, tc.wat)

			flagErr := checkGC(luagen.GCCollected, im, "")
			if flagErr == nil {
				t.Fatal("--gc=collected was accepted")
			}
			manifestErr := checkGC(luagen.GCCollected, im, "fklua.toml")
			if manifestErr == nil {
				t.Fatal("gc = \"collected\" was accepted")
			}

			for _, want := range []string{
				"what asked for it:", "what the module says:", "(1)", "(2)",
			} {
				if !strings.Contains(flagErr.Error(), want) {
					t.Errorf("the flag refusal has no %q, so it is not the "+
						"two-sides-two-fixes shape:\n%s", want, flagErr)
				}
				if !strings.Contains(manifestErr.Error(), want) {
					t.Errorf("the manifest refusal has no %q:\n%s", want, manifestErr)
				}
			}

			// The remedy has to be sayable where the mode was said. A manifest
			// arm telling an author to "pass --gc=leaking" is advice about a
			// command line that did not set the mode.
			if !strings.Contains(flagErr.Error(), "--gc=leaking") {
				t.Errorf("the flag refusal never offers --gc=leaking:\n%s", flagErr)
			}
			if !strings.Contains(manifestErr.Error(), `set gc = "leaking" in fklua.toml`) {
				t.Errorf("the manifest refusal does not say where to unsay it:\n%s",
					manifestErr)
			}
			if strings.Contains(manifestErr.Error(), "the --gc flag on this command line") {
				t.Errorf("the manifest refusal blames a flag nobody typed:\n%s",
					manifestErr)
			}
		})
	}
}

// THE PRECONDITION IS THE COLLECTOR, NOT THE TOOLCHAIN.
//
// checkGC refused --gc=collected for a wasip1 guest and accepted every other
// guest with no collector in it -- every Rust guest, since no fkgc equivalent
// exists for wasm32-unknown-unknown, and any Go guest not built -gc=custom.
// What such a module got was the collected-mode emitter gates (the inlined
// 8-byte store goes back out of line) plus a control.lua that arms a write
// barrier nothing drains, with no collector anywhere to make either mean
// something.
func TestCollectedIsRefusedForAGuestThatCarriesNoCollector(t *testing.T) {
	for _, tc := range []struct {
		name string
		wat  string
	}{
		{"a Rust guest, which has no collector in any toolchain", rustShapedWAT},
		{"a Go guest built -gc=leaking", goLeakingWAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGC(luagen.GCCollected, gcModule(t, tc.wat), "")
			if err == nil {
				t.Fatal("--gc=collected was accepted for a guest with no collector in it")
			}
			msg := err.Error()
			// The diagnostic names what is missing rather than only the rule:
			// an author who reads "refused" and not "fk_gc_step is absent" has
			// been told they lost without being told what to do.
			for _, want := range factorio.CollectorSurface() {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal never names %s:\n%s", want, msg)
				}
			}
			for _, want := range []string{"-gc=custom", "fkgc", "--gc=leaking"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal never mentions %q, so it says what is "+
						"wrong and not what to do:\n%s", want, msg)
				}
			}
		})
	}

	// And leaking is not refused for either of them -- the gate is about the
	// mode, so a control that fails here would be refusing every guest.
	for _, wat := range []string{rustShapedWAT, goLeakingWAT} {
		if err := checkGC(luagen.GCLeaking, gcModule(t, wat), ""); err != nil {
			t.Errorf("--gc=leaking was refused: %v", err)
		}
	}
}

// The positive path, on a stand-in and then on a real one. The stand-in pins
// the rule; the real build is what says the rule's export list is the list a
// collected guest actually has, which no amount of WAT can establish.
func TestCollectedIsAcceptedForAGuestCarryingTheCollector(t *testing.T) {
	if err := checkGC(luagen.GCCollected, gcModule(t, goCollectedWAT), ""); err != nil {
		t.Fatalf("--gc=collected was refused for a guest carrying the surface: %v", err)
	}

	if testing.Short() {
		t.Skip("-short: not building a guest")
	}
	if ok, why := guest.Available(); !ok {
		t.Skip(why)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "guest", "go"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "gcsave.wasm")
	if err := guest.BuildCollected(root, "./examples/gcsave", out); err != nil {
		t.Fatalf("building the collected guest: %v", err)
	}
	im, err := loadModule(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGC(luagen.GCCollected, im, ""); err != nil {
		t.Fatalf("a real -gc=custom guest was refused: %v", err)
	}
	// The same guest built the default way is the differential: if this passed
	// too, the check above would be passing for a reason other than the one it
	// claims.
	leaking := filepath.Join(t.TempDir(), "gcsave-leaking.wasm")
	if err := guest.Build(root, "./examples/gcsave", leaking); err != nil {
		t.Fatalf("building the leaking guest: %v", err)
	}
	im, err = loadModule(leaking)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGC(luagen.GCCollected, im, ""); err == nil {
		t.Fatal("the same source built -gc=leaking was accepted as collected")
	}
}
