package guest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The Rust feature guard.
//
// TinyGo's guard reads a target's feature string and that is sufficient,
// because TinyGo compiles everything it links from source with that string.
// **Rust is different in a way that matters, and the difference was measured
// rather than assumed:**
//
//   - rustc 1.97.1 enables SIX features by default for wasm32-unknown-unknown,
//     and FkLua supports three of them. bulk-memory, multivalue and
//     reference-types are all on.
//   - `-C target-feature=-bulk-memory` does NOT remove them. It governs the
//     current crate's codegen; `copy_nonoverlapping` lowers to a call to
//     `memcpy`, which comes from compiler_builtins -- a PRECOMPILED rlib built
//     with bulk-memory on. A no_std crate built with the feature disabled still
//     contains memory.copy and memory.fill.
//
// So the declared feature set is a statement of what LLVM MAY emit, and the
// shipped rlibs decide what it does emit. Reading the feature set alone would
// be a guard that passes while the module it blessed raises at runtime -- which
// is what FkLua does with an unsupported instruction: it compiles a stub that
// raises when called, so the mod loads and dies later.
//
// THAT WAS THE STATE BEFORE memory.copy AND memory.fill WERE IMPLEMENTED.
// Measuring the cost of the workaround is what made implementing them obvious:
// binaryen's lowering emits a byte-at-a-time loop, which in a word-table memory
// runs at 173 ns/byte against 3.5 for a word-wise runtime helper -- 49x. So the
// two instructions are now compiled natively and the workaround is gone.
//
// What a Rust guest needs today is one line:
//
//	cargo build --release --target wasm32-unknown-unknown
//
// A stock build -- bulk-memory enabled, no RUSTFLAGS, no wasm-opt pass --
// compiles through fklua with no warnings. RustFlags below is still worth
// passing, but as belt-and-braces rather than as a requirement.

// RustTarget is the wasm target the Rust guest commits to.
const RustTarget = "wasm32-unknown-unknown"

// RustFlags is a defensive target-feature string, no longer a requirement.
//
// bulk-memory is compiled natively now, so it is not listed. multivalue and
// reference-types are: neither reaches a module in practice -- multivalue needs
// a multi-return signature, which the C ABI a guest exports does not have, and
// reference-types needs an externref, which no FkLua binding produces -- but
// turning them off costs nothing and removes the way they could.
const RustFlags = "-C target-feature=-multivalue,-reference-types"

// RustLoweringPass is binaryen's byte-loop lowering for memory.copy and
// memory.fill.
//
// KEPT FOR THE TEST THAT MEASURES WHAT IT COSTS, and for nothing else. A guest
// does not need it: 173 ns/byte against 3.5 for the native path is why the
// instructions were implemented instead.
const RustLoweringPass = "--llvm-memory-copy-fill-lowering"

// RustAvailable reports whether a Rust wasm toolchain is installed.
func RustAvailable() (bool, string) {
	if _, err := exec.LookPath("rustc"); err != nil {
		return false, "rustc is not installed: https://rustup.rs"
	}
	// cargo, because NOTHING HERE SHELLS OUT TO rustc TO BUILD A GUEST. Every
	// build path in this package -- BuildRust, BuildRustCollected, BuildRustLib,
	// and every `cargo test -p` a gate runs -- invokes cargo, so a machine with
	// rustc and no cargo passed this guard and died later inside a build, which
	// is the exact failure mode the 2026-07-31 rlib probe was written to remove
	// and this line was the half of it nobody noticed.
	//
	// A LookPath rather than a probe-by-doing, and that is not a lapse from the
	// rule below: the rule is about a question whose ANSWER CAN LIE. rustc will
	// happily describe a target whose rlibs are absent, so asking it is
	// worthless and only compiling discriminates. Whether an executable named
	// cargo is on PATH has no such gap -- it is the same class as Available()'s
	// TinyGo and wasm-opt checks, which are LookPaths for the same reason. If a
	// present cargo can still fail to build, it fails for a reason rustc's own
	// probe below already covers.
	if _, err := exec.LookPath("cargo"); err != nil {
		return false, "cargo is not installed (rustc alone is not enough -- " +
			"every Rust build here goes through cargo): https://rustup.rs"
	}
	out, err := exec.Command("rustc", "--print", "target-list").Output()
	if err != nil {
		return false, fmt.Sprintf("rustc --print target-list: %v", err)
	}
	if !strings.Contains(string(out), RustTarget) {
		return false, "rustc does not know " + RustTarget
	}
	// Knowing the target is not having its rlibs, and NOTHING rustc will PRINT
	// discriminates the two. wasm32-unknown-unknown is a BUILT-IN target spec
	// compiled into rustc, so `--print cfg --target` answers out of that spec
	// and exits 0 with no core installed anywhere on the machine. This guard
	// asked exactly that question until 2026-07-31 and was therefore a false
	// positive on every machine that had never run `rustup target add` --
	// including every CI runner, where it waved the Rust tests through into a
	// cargo build that died on E0463 and took the whole `go` job with it.
	//
	// The probe that discriminates is COMPILING something. A no_std rlib is the
	// cheapest thing that needs core and nothing else: no panic handler, no
	// linker, 26 ms measured.
	if err := rustHasTargetLibs(RustTarget); err != nil {
		return false, "run: rustup target add " + RustTarget
	}
	return true, ""
}

// rustHasTargetLibs compiles a trivial no_std crate for a target. It fails with
// E0463 ("can't find crate for `core`") unless that target's rlibs are really
// installed, which is the fact RustAvailable needs and cannot get by asking
// rustc to describe itself.
//
// Parameterised on the target only so the guard's own regression test can aim
// it at one that is not installed; RustAvailable always passes RustTarget.
func rustHasTargetLibs(target string) error {
	dir, err := os.MkdirTemp("", "fk-rust-probe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// wrapping_add rather than an empty crate, so the probe genuinely resolves
	// something out of core instead of resting on the injected extern crate.
	src := filepath.Join(dir, "probe.rs")
	const probe = "#![no_std]\npub fn f(x: u32) -> u32 { x.wrapping_add(1) }\n"
	if err := os.WriteFile(src, []byte(probe), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("rustc", "--crate-type=lib", "--target", target,
		"--out-dir", dir, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rustc probe for %s: %w\n%s", target, err, out)
	}
	return nil
}

// RustFeatures reads the features rustc enables by default for the guest
// target.
//
// This is rustc's own answer rather than a table in this file, for the reason
// the package exists: the set has moved between releases and will move again.
func RustFeatures() ([]Feature, error) {
	out, err := exec.Command("rustc", "--print", "cfg", "--target", RustTarget).Output()
	if err != nil {
		return nil, fmt.Errorf("rustc --print cfg: %w", err)
	}
	var fs []Feature
	for _, line := range strings.Split(string(out), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), `target_feature="`)
		if !ok {
			continue
		}
		fs = append(fs, Feature(strings.TrimSuffix(v, `"`)))
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i] < fs[j] })
	return fs, nil
}

// RustMitigated is the set of features FkLua does not support but which the
// documented build recipe removes from the emitted module.
//
// Being here is a claim with a test behind it: TestRustBuildRecipeRemovesWhatItClaims
// builds a guest that uses the feature and checks the module afterwards. A
// feature listed here without that evidence would be a guard that lies.
var RustMitigated = map[Feature]string{
	// memory.copy and memory.fill are compiled natively. The rest of the
	// proposal is not: memory.init and data.drop need a PASSIVE data segment,
	// which the decoder refuses outright rather than stubbing, and no guest
	// toolchain has been seen emitting one. That is the honest scope of this
	// entry -- it is not a claim to support all of bulk-memory.
	"bulk-memory":     "memory.copy/fill are compiled; memory.init is not",
	"bulk-memory-opt": "compiled natively",
	// Disabled by RustFlags, and absent in practice anyway.
	"multivalue":             RustFlags,
	"reference-types":        RustFlags,
	"call-indirect-overlong": "absorbed by the decoder",
}

// CheckRust reports every feature rustc enables that FkLua neither supports nor
// has a documented mitigation for.
//
// A gap here means rustc started emitting something new, which is the event
// this whole package exists to catch.
func CheckRust(enabled []Feature) []Gap {
	var gaps []Gap
	for _, f := range enabled {
		if Supported[f] || RustMitigated[f] != "" {
			continue
		}
		gaps = append(gaps, Gap{Feature: f, Milestone: Planned[f]})
	}
	return gaps
}

// RustCollectorFeature is the one flag that turns the collector on, and it is
// the Rust analogue of swapping -gc=leaking for -gc=custom.
//
// It is `fk`'s feature and not an example's, which is a decision with a reason
// on it. Cargo's v2 resolver unifies features across every package built in one
// invocation, so an example that DECLARED this in its own Cargo.toml would turn
// the collector on for every other example in a workspace-wide `cargo build` --
// silently, and only for that invocation, which is exactly the shape of
// build-dependent non-determinism CLAUDE.md rules out. Passed on the command
// line against a single -p there is nothing to unify.
// TestNoRustExampleDeclaresTheCollectorFeature holds the other half.
const RustCollectorFeature = "fk/fkgc"

// BuildRust compiles a Rust guest crate to wasm and returns the module path.
//
// One command and no post-processing, which is the whole recipe. Release mode
// is not optional: the workspace's release profile carries panic=abort, and a
// debug build would try to unwind across a boundary that cannot unwind.
func BuildRust(workspace, pkg, outDir string) (string, error) {
	return buildRust(workspace, pkg, outDir, false)
}

// BuildRustCollected compiles a Rust guest with the collector enabled: the same
// crate, the same command, one --features flag.
//
// The Go pair it mirrors is Build/BuildCollected, and the two differ in exactly
// the same way -- one build knob, nothing else -- for the same reason: an A/B
// between the two arms is only a measurement if that is all that changed.
//
// THERE IS NO IMPORT TO ADD, which is where the two toolchains genuinely
// diverge. A Go guest must `import _ ".../fkgc"` or -gc=custom fails to link
// with `missing core function "runtime.free"`; a Rust guest needs nothing,
// because `fk` owns the single #[global_allocator] site and the feature chooses
// what backs it. So this flag alone IS the feature here, and every example in
// the corpus builds both ways with no source change at all.
func BuildRustCollected(workspace, pkg, outDir string) (string, error) {
	return buildRust(workspace, pkg, outDir, true)
}

func buildRust(workspace, pkg, outDir string, collected bool) (string, error) {
	args := []string{"build", "--release", "--target", RustTarget, "-p", pkg}
	if collected {
		args = append(args, "--features", RustCollectorFeature)
	}
	cmd := exec.Command("cargo", args...)
	cmd.Dir = workspace
	// CARGO_TARGET_DIR so a test never writes into the checked-out tree, and so
	// concurrent tests do not fight over one target/ lock.
	//
	// IT ALSO KEEPS THE TWO ARMS APART, and that is not merely hygiene. Cargo
	// rebuilds when the feature set changes, but it writes the artifact to the
	// SAME path -- so two arms sharing a target dir hand the second reader
	// whichever wasm was built last, with no error anywhere. That is the stage-C
	// cache lesson in its Rust form. Callers pass a distinct outDir per arm, and
	// TestTheTwoRustArmsAreDifferentModules asserts the two really differ.
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		feat := ""
		if collected {
			feat = " --features " + RustCollectorFeature
		}
		return "", fmt.Errorf("cargo build -p %s%s: %w\n%s", pkg, feat, err, out)
	}
	// A cdylib's artifact is named after the [lib] name with dashes mapped to
	// underscores, not after the package.
	name := strings.ReplaceAll(pkg, "-", "_") + ".wasm"
	return filepath.Join(outDir, RustTarget, "release", name), nil
}

// BuildRustLib compiles a Rust library crate and returns its rlib path.
//
// Separate from BuildRust because a cdylib and an rlib land under different
// names, and because a library is the right unit for a compile gate: rustc
// type-checks every item in one, where a cdylib guest only pulls in what it
// calls.
func BuildRustLib(workspace, pkg, outDir string) (string, error) {
	cmd := exec.Command("cargo", "build", "--release",
		"--target", RustTarget, "-p", pkg)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cargo build -p %s: %w\n%s", pkg, err, out)
	}
	name := "lib" + strings.ReplaceAll(pkg, "-", "_") + ".rlib"
	return filepath.Join(outDir, RustTarget, "release", name), nil
}
