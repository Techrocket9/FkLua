package guest_test

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
)

// THE SECOND SILENT-SKIP SHAPE, and the thing stage D learned trying to make it
// quieter than a failure: THERE IS NO SUCH CHANNEL.
//
// About fifteen tests in this package skip when tinygo or wasm-opt is missing.
// Each is right to skip; the aggregate is not, because `go test` prints nothing
// for a skip without `-v` and the package reports `ok` for a run in which the
// differential corpus, the collector suite, the end-to-end mod runs and the API
// bindings check all declined. A fresh worktree lands in exactly that state and
// stage D read one as a pass.
//
// The obvious middle ground -- a passing test that writes a banner to
// os.Stderr -- was built and does not work. `go test` runs the test binary with
// its output CAPTURED and prints it only when the package fails, so a banner
// from a passing test is discarded before anyone sees it. Tried, measured, and
// the reason it is not here.
//
// So the channel is a failure, and the opt-out is `-short`, which this package
// already uses for the same purpose in callcost_test.go. That keeps the
// treatment consistent with internal/luahost's TestTheOracleIsBuilt -- absence
// is reported once, by a failure, with the remedy in the message -- while
// leaving a contributor who is working on the type checker one standard flag
// rather than a bespoke environment variable.
//
// CI is the second case and `-short` does NOT serve it, which is why there is
// now a variable after all. Blanket `-short` would also skip
// TestTheRustToolchainIsAvailable, and CI is the one environment that installs
// the Rust target -- so it is exactly where that guard is worth running. The
// reasoning for the narrower channel is on guest.NoToolchainEnv.
func TestTheGuestToolchainIsAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: not requiring a guest toolchain")
	}
	if guest.ToolchainDeclaredAbsent() {
		t.Skipf("%s: this environment declares no guest toolchain", guest.NoToolchainEnv)
	}
	if ok, why := guest.Available(); !ok {
		t.Fatalf("THE GUEST TOOLCHAIN IS MISSING, so every test in this package "+
			"that builds a guest just SKIPPED -- the differential corpus, the "+
			"collector suite, the end-to-end mod runs and the API bindings "+
			"check -- and this package's result reads like a pass.\n\n  %s\n\n"+
			"Install it, or run `go test -short ./...` to say you meant to skip "+
			"them. A skipped test prints nothing without -v, which is why this "+
			"is a failure and not a log line.", why)
	}
}

// THE SAME SHAPE, FOR THE OTHER TOOLCHAIN, and it was missing for four
// milestones after Rust became a supported guest language.
//
// The test above hard-fails on a missing TinyGo. Nothing asked the same question
// about Rust, so a machine that has never run `rustup target add
// wasm32-unknown-unknown` silently skipped EVERY Rust gate there is: the
// feature-set guard (which is the whole point of this package -- it is the thing
// that notices rustc has started emitting something new), the cross-language
// differential corpus, the Rust collector suite and its root-range and mirror
// assertions, the generated-bindings compile, and the two arms of
// TestTheTwoRustArmsAreDifferentModules. Every one of them prints nothing
// without -v, and the package reports `ok`.
//
// That is not hypothetical for this repo: RustAvailable's own history is the
// case study. It asked `rustc --print cfg --target`, which answers out of a
// built-in target spec and exits 0 with no core installed anywhere, so it was a
// false positive on every CI runner until 2026-07-31. The failure mode it
// produced was loud (a cargo build dying on E0463). The failure mode a correct
// guard produces without this test is silent, which is worse.
//
// PROBE BY DOING, which is what RustAvailable already learned: it compiles a
// two-line no_std crate (26 ms) because nothing rustc will PRINT distinguishes
// "knows the target" from "has the target's rlibs". This test inherits that for
// free by calling it.
//
// The -short opt-out is deliberately the SAME flag as the TinyGo one rather than
// a second, Rust-specific switch. A contributor working on the type checker
// should have one way to say "I meant to skip the toolchain builds", and a
// second flag is a second thing to not know about.
func TestTheRustToolchainIsAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: not requiring a guest toolchain")
	}
	if ok, why := guest.RustAvailable(); !ok {
		t.Fatalf("THE RUST GUEST TOOLCHAIN IS MISSING, so every Rust gate in this "+
			"package just SKIPPED -- the rustc feature-set guard, the "+
			"cross-language differential corpus, the Rust collector suite, the "+
			"root-range assertion and the generated-bindings compile -- and this "+
			"package's result reads like a pass.\n\n  %s\n\n"+
			"Install it, or run `go test -short ./...` to say you meant to skip "+
			"them. A skipped test prints nothing without -v, which is why this "+
			"is a failure and not a log line.", why)
	}
}
