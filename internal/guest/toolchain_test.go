package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// This is the guard the trunc_sat bug should have tripped.
//
// "TinyGo emits nontrapping-fptoint unconditionally" was recorded in prose and
// never checked, so trunc_sat stayed unimplemented for three milestones while
// the flagship guest emitted it. A comment cannot fail a build; this can.
//
// Skips when TinyGo is absent, because CI has no toolchain -- which does mean
// the guard only bites locally. That is the honest limit of it.
func TestTinyGoEmitsNothingWeCannotCompile(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	for _, target := range TinyGoTargets {
		t.Run(target.Name, func(t *testing.T) {
			enabled, disabled, err := Features(root, target.Name)
			if err != nil {
				t.Fatalf("reading target: %v", err)
			}
			t.Logf("enabled: %v", enabled)
			t.Logf("disabled: %v", disabled)

			for _, g := range Check(enabled) {
				switch {
				case g.Milestone == "":
					// A toolchain upgrade started emitting something new. This
					// is the case worth waking up for.
					t.Errorf("TinyGo %s now emits %q, which FkLua cannot compile "+
						"and which is not on the roadmap at all (%s).\n"+
						"Either implement it or add it to guest.Planned with a milestone.",
						target.Name, g.Feature, target.Why)
				case target.MustBeFullySupported:
					t.Errorf("TinyGo %s emits %q, scheduled for %s, but this target "+
						"is already claimed to work (%s)",
						target.Name, g.Feature, g.Milestone, target.Why)
				default:
					t.Logf("known gap: %s needs %q (%s)", target.Name, g.Feature, g.Milestone)
				}
			}
		})
	}
}

// The corpus is converted with these features excluded. If a guest ever starts
// emitting one, the corpus stops describing the dialect we actually compile --
// a quieter failure than a missing opcode, and the reason so much of the
// testsuite was left unlisted in the first place.
func TestExcludedFeaturesStayExcluded(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	excluded := []Feature{"multivalue", "reference-types", "simd128"}

	for _, target := range TinyGoTargets {
		enabled, _, err := Features(root, target.Name)
		if err != nil {
			t.Fatalf("%s: %v", target.Name, err)
		}
		for _, e := range enabled {
			for _, x := range excluded {
				if e == x {
					t.Errorf("TinyGo %s now enables %q, which scripts/fetch-spec.sh "+
						"passes --disable-%s to wast2json. The spec corpus no longer "+
						"describes the dialect our guests emit.",
						target.Name, e, strings.TrimSuffix(string(x), "128"))
				}
			}
		}
	}
}

// No TinyGo target may enable anything in this list, since none of it has a
// sane lowering into a Lua sandbox.
func TestNoTargetEnablesUncompilableProposals(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, target := range TinyGoTargets {
		enabled, _, err := Features(root, target.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range enabled {
			switch {
			case strings.Contains(string(e), "simd"),
				strings.Contains(string(e), "atomics"),
				strings.Contains(string(e), "threads"),
				strings.Contains(string(e), "exception"),
				strings.Contains(string(e), "gc"):
				t.Errorf("TinyGo %s enables %q; there is no reasonable lowering "+
					"for it in a Lua sandbox", target.Name, e)
			}
		}
	}
}

func TestParseFeatures(t *testing.T) {
	en, dis := parseFeatures("+sign-ext,-multivalue,+nontrapping-fptoint,-reference-types")
	if len(en) != 2 || en[0] != "nontrapping-fptoint" || en[1] != "sign-ext" {
		t.Errorf("enabled = %v, want sorted [nontrapping-fptoint sign-ext]", en)
	}
	if len(dis) != 2 || dis[0] != "multivalue" || dis[1] != "reference-types" {
		t.Errorf("disabled = %v", dis)
	}
	// Junk must not become a phantom feature.
	if en, dis := parseFeatures(""); len(en) != 0 || len(dis) != 0 {
		t.Errorf("empty string produced %v / %v", en, dis)
	}
	if en, dis := parseFeatures("+,x,-"); len(en) != 0 || len(dis) != 0 {
		t.Errorf("malformed entries produced %v / %v", en, dis)
	}
}

func TestCheckClassifiesGaps(t *testing.T) {
	gaps := Check([]Feature{"sign-ext", "bulk-memory", "wobbly-proposal"})
	if len(gaps) != 2 {
		t.Fatalf("expected 2 gaps (sign-ext is supported), got %v", gaps)
	}
	byName := map[Feature]string{}
	for _, g := range gaps {
		byName[g.Feature] = g.Milestone
	}
	// Partial rather than scheduled, and the note has to say which half:
	// memory.copy and memory.fill are compiled, the segment-indexed ops are
	// not. "M10" outlived M10 by two milestones and read as work in progress.
	if ms := byName["bulk-memory"]; !strings.Contains(ms, "partial") ||
		!strings.Contains(ms, "memory.copy") {
		t.Errorf("bulk-memory's status should say what is and is not compiled, got %q", ms)
	}
	// An unknown feature must report NO milestone, so the test can tell
	// "scheduled" apart from "a toolchain started emitting something new".
	if ms, ok := byName["wobbly-proposal"]; !ok || ms != "" {
		t.Errorf("an unrecognised feature should have no milestone, got %q", ms)
	}
}

// Rust is the M8 guest and is not verified here at all: rustup is not installed,
// and rustc's default wasm32 feature set has moved between releases. Recording
// that as a real hole rather than leaving it implicit.
// rustc's default wasm32 feature set, checked against what FkLua can compile.
//
// This is the M8 half of the same job the TinyGo test does, and it found a live
// problem on its first run: rustc 1.97.1 enables bulk-memory, multivalue and
// reference-types by default, none of which FkLua supports. A stock
// `cargo build --target wasm32-unknown-unknown` produces a module that compiles
// to raising stubs.
func TestRustFeatureSetIsCoveredOrMitigated(t *testing.T) {
	if ok, why := RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	enabled, err := RustFeatures()
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) == 0 {
		t.Fatal("rustc reported no target features at all, which means the " +
			"parse broke rather than that rustc enables none")
	}
	t.Logf("rustc enables %d features for %s: %v", len(enabled), RustTarget, enabled)

	for _, g := range CheckRust(enabled) {
		if g.Milestone != "" {
			t.Errorf("rustc enables %q, which is scheduled for %s and has no "+
				"mitigation in the documented build recipe", g.Feature, g.Milestone)
			continue
		}
		t.Errorf("rustc enables %q, which FkLua neither supports nor mitigates. "+
			"A toolchain has started emitting something new: decide whether to "+
			"support it, disable it in RustFlags, or lower it in wasm-opt",
			g.Feature)
	}
}

// A STOCK Rust build compiles clean, with no flags and no post-processing.
//
// This test used to assert the opposite -- that RustFlags left memory.copy in
// place and a wasm-opt pass was needed to remove it. Both halves were true, and
// measuring what the workaround cost is what killed it: binaryen's lowering is
// a byte-at-a-time loop at 173 ns/byte against 3.5 for the native path, so the
// two instructions were implemented rather than avoided.
//
// What is worth pinning now is the ABSENCE of the workaround. The guest is
// built the way a Rust author would build it, its module really does contain
// bulk-memory instructions, and every function still compiles.
func TestAStockRustBuildNeedsNoWorkaround(t *testing.T) {
	if ok, why := RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	dir := t.TempDir()
	writeRustProbe(t, dir)

	// No RUSTFLAGS at all: exactly `cargo build --target wasm32-unknown-unknown`.
	build := exec.Command("cargo", "build", "--release", "--target", RustTarget)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, out)
	}
	built := filepath.Join(dir, "target", RustTarget, "release", "fkprobe.wasm")

	// The premise: this module really does use the feature. Without it the test
	// would pass by testing nothing, which is how a guard rots.
	if !hasBulkMemory(t, built) {
		t.Fatal("the probe emitted no memory.copy/memory.fill, so this proves " +
			"nothing -- rustc or compiler_builtins changed and the probe needs " +
			"to be made to allocate again")
	}

	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding a stock Rust module: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("function %q does not compile: %v -- a stock Rust build is "+
				"supposed to need no workaround", f.Name, f.Unsupported)
		}
	}
}

// hasBulkMemory reports whether the module contains memory.copy or memory.fill.
//
// Read from the module itself rather than from wasm2wat, so the test needs no
// WABT -- and so it is asking the same decoder the compiler uses.
//
// It looks for the OPCODES rather than for an "unsupported" marker. It used to
// do the latter, which stopped working the moment FkLua implemented the two
// instructions: a detector defined as "the compiler refused it" silently
// becomes "always false" when the compiler stops refusing.
func hasBulkMemory(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding %s: %v", filepath.Base(path), err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir for %s: %v", filepath.Base(path), err)
	}
	for _, f := range im.Funcs {
		for _, s := range f.Steps {
			switch s.Instr.Op {
			case wasm.OpMemoryCopy, wasm.OpMemoryFill:
				return true
			}
		}
	}
	return false
}

// writeRustProbe lays down a no_std crate whose two functions copy and fill
// more bytes than LLVM will inline, which is what makes it call memcpy/memset
// and so pull the feature in from compiler_builtins.
func writeRustProbe(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Cargo.toml": `[package]
name = "fkprobe"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[profile.release]
opt-level = "s"
lto = true
panic = "abort"
`,
		"src/lib.rs": `#![no_std]
use core::panic::PanicInfo;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! { loop {} }

#[no_mangle]
pub extern "C" fn bigcopy(dst: *mut u8, src: *const u8, n: usize) {
    unsafe { core::ptr::copy_nonoverlapping(src, dst, n) }
}

#[no_mangle]
pub extern "C" fn bigfill(dst: *mut u8, n: usize) {
    unsafe { core::ptr::write_bytes(dst, 7u8, n) }
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The generated Rust bindings must COMPILE, not merely parse.
//
// The Go side's history is the argument: TestGeneratedGoParses accepted `*p.h`,
// which parses fine and means `*(p.h)`, and seventy members were broken behind
// a green test. Rust's checker is stricter still -- it caught borrow errors in
// this generator that the Go backend's equivalents could only surface by
// building with TinyGo.
//
// Compiling the whole crate rather than a guest that uses it: rustc type-checks
// every item in a library regardless of what is called, so this covers every
// bound member (`rust_members_bound` in census.json) where a
// dead-code-eliminated guest would cover only what it touches.
func TestGeneratedRustBindingsCompile(t *testing.T) {
	if ok, why := RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRootDir(t)
	out, err := BuildRustLib(filepath.Join(root, "guest", "rust"), "fkapi", t.TempDir())
	if err != nil {
		t.Fatalf("the generated Rust bindings do not compile:\n%v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fkapi built against the generated bindings: %d bytes of rlib", fi.Size())
}

// TestTheRustGuardProbesByCompilingNotByPrinting pins the exact distinction
// RustAvailable got wrong.
//
// The guard used to decide the guest target was installed by running `rustc
// --print cfg --target wasm32-unknown-unknown`. That target is a BUILT-IN spec
// compiled into rustc, so printing succeeds with no rlibs installed anywhere --
// the guard passed on every CI runner and handed the Rust tests to a cargo
// build that died on a missing `core`, which is a hard failure where a skip was
// meant. Only compiling discriminates.
//
// Asserted against a target that rustc knows and this machine has not
// installed, since that is the state the guard has to detect. If the probe
// target IS installed here there is nothing to discriminate and the test says
// so rather than pretending.
func TestTheRustGuardProbesByCompilingNotByPrinting(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skipf("skipping: rustc is not installed")
	}
	const absent = "aarch64-unknown-linux-gnu"
	if err := rustHasTargetLibs(absent); err == nil {
		t.Skipf("skipping: %s is installed here, so it cannot stand in for an "+
			"absent target", absent)
	}

	// The old probe, which is the false positive. If this ever starts failing,
	// rustc changed and the guard could go back to being cheap -- but do not
	// assume that, re-measure it.
	if err := exec.Command("rustc", "--print", "cfg", "--target", absent).Run(); err != nil {
		t.Fatalf("`rustc --print cfg --target %s` failed, so it no longer "+
			"demonstrates the false positive this guard exists for: %v", absent, err)
	}

	// And the guard built on it, which must reject what the print accepted.
	ok, why := RustAvailable()
	if !ok {
		t.Logf("the real guest target is absent here too (%s), which is "+
			"consistent -- the assertion above is the one that matters", why)
	}
}

// repoRootDir walks up to the go.mod, so the test does not care where it runs.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
