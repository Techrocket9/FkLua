package guest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildFlags are the TinyGo flags a guest must be built with, and the reasons
// are not stylistic:
//
//   - -target=wasm-unknown is the only target whose feature set FkLua can
//     compile. TestTinyGoEmitsNothingWeCannotCompile checks that claim against
//     TinyGo's own target JSON rather than against this comment.
//   - -scheduler=none because a Factorio tick cannot block. With a scheduler,
//     a parked goroutine becomes a busy spin inside the game loop.
//   - -gc=leaking because a collector's pauses land in a lockstep game loop,
//     where one client stalling desyncs everyone.
//   - -opt=2 because TinyGo's default is -opt=z, which optimises for SIZE, and
//     size is the one cost this target does not have: the day-0 probe measured
//     Factorio parsing 4 MB of Lua in 106 ms and a generated chunk never
//     appears in a save. Measured against -opt=z through the real compiler:
//     real_names 0.577x, real_grid 0.771x, pure_sum 0.770x, pure_dot 0.847x,
//     real_entities 0.958x. pure_prng is ~2% slower and is the only kernel
//     that does not gain. -opt=0 and -opt=1 are NOT substitutes: -opt=0 fails
//     to build under -scheduler=none, and -opt=1 leaves most of the win.
//
// Kept in step with guest/go/fk.BuildFlags, which is what a guest author reads.
var BuildFlags = []string{
	"-target=wasm-unknown",
	"-scheduler=none",
	"-gc=leaking",
	"-opt=2",
}

// CollectedBuildFlags are BuildFlags with the collector turned on: the same
// four flags with -gc=custom in place of -gc=leaking, and nothing else.
//
// -gc=custom is the supported seam for plugging an external collector into
// TinyGo (src/runtime/gc_custom.go). It requires the application to provide
// seven functions by //go:linkname, which is what guest/go/fkgc does -- so a
// guest built with these flags and WITHOUT that import does not link, with
// `missing core function "runtime.free"` from deep inside the builder. That is
// the trap here, and it is the same shape as wasip1's -buildmode=c-shared: the
// flag alone is not the feature.
//
// Everything else is deliberately identical, because the stage-B allocation
// measurement is only meaningful if the two arms differ in one flag. Kept in
// step with guest/go/fk.CollectedBuildFlags.
var CollectedBuildFlags = []string{
	"-target=wasm-unknown",
	"-scheduler=none",
	"-gc=custom",
	"-opt=2",
}

// WASIBuildFlags are the flags for a wasip1 guest, which is what buys
// goroutines. Each is load-bearing for a different reason:
//
//   - -target=wasip1 brings the asyncify scheduler, which rewrites the module
//     into a resumable state machine INSIDE the wasm. That is what makes
//     goroutines work with no host coroutines, which Lua 5.2 does not have.
//   - -buildmode=c-shared is NOT optional and is the trap. wasip1 defaults to
//     building a COMMAND, exporting `_start`: it runs main and terminates, and
//     calling an export afterwards is out of contract. The symptom is
//     "//go:wasmexport function called before runtime initialization" from the
//     guest's own runtime, which reads like an ordering bug in the host and is
//     not. A mod needs a REACTOR, which exports `_initialize`.
//
// The gc is left at TinyGo's wasip1 default (precise) rather than forced to
// leaking: asyncify already costs what it costs, and a guest reaching for
// goroutines is not the guest optimising for a tick budget.
var WASIBuildFlags = []string{
	"-target=wasip1",
	"-buildmode=c-shared",
}

// Available reports whether a guest can be built here, and why not when it
// cannot.
//
// wasm-opt is checked separately from TinyGo because it is a separate install
// (binaryen) that TinyGo shells out to and hard-requires for wasm targets. Its
// absence produces "could not find wasm-opt" from deep inside a build, which
// does not tell an unlucky reader to `brew install binaryen`.
func Available() (bool, string) {
	if _, err := Root(); err != nil {
		return false, "tinygo is not installed: " + err.Error()
	}
	if _, err := wasmOpt(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// NoToolchainEnv is how an environment DECLARES that it carries no guest
// toolchain on purpose, which is the one thing that stops the availability
// guards being a failure.
//
// It exists because those guards were written for a fresh worktree -- where a
// missing TinyGo means fifteen tests skip and the package still says `ok` --
// and CI is the other case: .github/workflows/ci.yml does not install TinyGo,
// says so, and gives its reasons. Absence there is declared, reviewed and in
// version control, not the silent gap the guards exist to expose. Until this
// existed the two collided and took the `go` job down on every push, which
// took `spectest` with it as a SKIP, because it declares `needs: [go, lua52f]`.
//
// Deliberately a declaration rather than a sniff at `CI`: a future job that
// does install TinyGo must still be guarded, and inferring the answer from the
// environment would quietly exempt it. And deliberately not `-short`, which is
// the guards' own opt-out but is too blunt here -- it would also skip
// TestTheRustToolchainIsAvailable, and CI installs the Rust target, so CI is
// exactly where that one is worth asserting. It was a false positive on every
// runner until 2026-07-31 and nothing noticed.
//
// It needs no stale-declaration check. Setting it on a machine that HAS a
// toolchain changes nothing -- Available reports true and the guard passes on
// its own -- so the variable can only ever speak for an absence that is real.
const NoToolchainEnv = "FKLUA_NO_GUEST_TOOLCHAIN"

// ToolchainDeclaredAbsent reports whether this environment has declared that it
// carries no guest toolchain. The name lives here, next to Available, so the
// two guards that read it cannot drift apart on the spelling.
func ToolchainDeclaredAbsent() bool {
	return os.Getenv(NoToolchainEnv) == "1"
}

// wasmOpt finds the binaryen optimiser TinyGo requires for wasm targets,
// honouring the same WASMOPT override TinyGo itself reads.
func wasmOpt() (string, error) {
	if env := os.Getenv("WASMOPT"); env != "" {
		return env, nil
	}
	path, err := exec.LookPath("wasm-opt")
	if err != nil {
		return "", fmt.Errorf("wasm-opt is not installed, and TinyGo requires it " +
			"for every wasm target: `brew install binaryen`")
	}
	return path, nil
}

// Build compiles a guest package with TinyGo and writes a wasm module to out.
//
// dir is the guest module's root -- the directory holding its go.mod -- and pkg
// is the package to build relative to it.
func Build(dir, pkg, out string) error {
	return BuildWith(dir, pkg, out, BuildFlags)
}

// BuildCollected compiles a guest with the collector enabled. The package must
// import guest/go/fkgc, which supplies the -gc=custom hooks; without it the
// link fails rather than producing a guest that quietly does not collect.
func BuildCollected(dir, pkg, out string) error {
	return BuildWith(dir, pkg, out, CollectedBuildFlags)
}

// BuildWith compiles a guest with an explicit flag set. Callers should use
// BuildFlags or CollectedBuildFlags rather than assembling their own -- the
// flags are load-bearing and the reasons are on the variables.
func BuildWith(dir, pkg, out string, flags []string) error {
	if ok, why := Available(); !ok {
		return fmt.Errorf("cannot build guest: %s", why)
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	args := append(append([]string{"build"}, flags...), "-o", abs, pkg)
	cmd := exec.Command("tinygo", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tinygo %s: %w\n%s",
			strings.Join(args, " "), err, output)
	}
	return nil
}
