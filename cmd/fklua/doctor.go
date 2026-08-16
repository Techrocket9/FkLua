package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Techrocket9/fklua/internal/guest"
)

// THE VERSIONS THE README'S PREREQUISITES TABLE NAMES, and nothing else.
//
// `doctor` reports rather than decides: these strings are printed beside what
// was found so a reader can compare, and only ABSENCE is an error. A pinned
// TinyGo is what this repo measures against, not what a guest requires --
// refusing 0.42 on a version string would be a guess dressed up as a gate, and
// the thing that actually knows is the build.
//
// Kept honest by TestDoctorQuotesTheVersionsTheReadmeNames, which greps
// README.md for each of them: a prerequisites table and a diagnostic that
// disagree is the "mirror checked in one direction" failure this repo has
// already had once, and the direction that drifts is always the unchecked one.
const (
	docGoVersion      = "Go 1.26+"
	docTinyGoVersion  = "TinyGo 0.41.1"
	docBinaryen       = "binaryen"
	docRustVersion    = "Rust 1.97+"
	docFactorioVerion = "Factorio 2.1.14"
)

// runDoctor reports which of the toolchains the docs name are installed here.
//
// WHY THIS EXISTS AND WHY IT IS THIS SMALL. A first-time author's second
// question, after "what do I install", is "did I install it right", and until
// now the only instrument was to run a build and read the failure -- which for
// a missing binaryen is "could not find wasm-opt" from several layers inside
// TinyGo, and for a missing Rust target is E0463 from inside cargo. Both of
// those are already given a better message by internal/guest; nothing surfaced
// them except a build that had already started.
//
// IT ASKS internal/guest RATHER THAN RE-DERIVING THE VERDICT. guest.Available
// and guest.RustAvailable are what the build path itself consults, including
// the rustc probe that COMPILES a no_std rlib because nothing rustc will print
// distinguishes a known target from an installed one. A doctor with its own
// opinion would be a second implementation of the question, and the one that
// drifts is the one nothing runs.
//
// It installs nothing and reaches no network, so it is safe to suggest to
// somebody who has just cloned this repository.
func runDoctor(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("doctor takes no arguments")
	}

	fmt.Println("fklua doctor -- the toolchains README.md's prerequisites table names")
	fmt.Println()

	// The tool itself, which is not a guest toolchain: whoever is reading this
	// already has a built fklua, so Go's absence is worth reporting and is not
	// a reason to fail.
	doctorRow("go", goVersion(), docGoVersion, "builds the fklua tool itself")

	// The Go guest arm. wasm-opt is separate from TinyGo because it is a
	// separate install that TinyGo shells out to and hard-requires.
	tinygoOK := doctorRow("tinygo", tinygoVersion(), docTinyGoVersion,
		"a Go guest: -target=wasm-unknown, not standard Go")
	wasmOptOK := doctorRow("wasm-opt", wasmOptVersion(), docBinaryen,
		"NOT optional: TinyGo needs it for every wasm target")
	goGuest, goWhy := guest.Available()

	// The Rust guest arm.
	doctorRow("cargo", cargoVersion(), docRustVersion, "a Rust guest")
	rustGuest, rustWhy := guest.RustAvailable()
	doctorRowBool("wasm32-unknown-unknown", rustGuest, "rustup target add wasm32-unknown-unknown",
		"the Rust guest target, and having it is not knowing it")

	// The game, which building needs neither of -- the API description is
	// committed under api/<version>/.
	doctorRow("factorio", factorioVersion(), docFactorioVerion,
		"only to RUN what you built; building needs neither game nor network")

	fmt.Println()
	switch {
	case goGuest && rustGuest:
		fmt.Println("BOTH guest toolchains are complete. You need one, not both.")
	case goGuest:
		fmt.Println("The Go guest toolchain is complete. Rust is optional: " + rustWhy)
	case rustGuest:
		fmt.Println("The Rust guest toolchain is complete. Go is optional: " + goWhy)
	default:
		// THE ONLY FAILURE, and it is one guest toolchain rather than all of
		// them, because README.md says so in as many words: "You need one guest
		// toolchain, not both."
		fmt.Println("NO COMPLETE GUEST TOOLCHAIN. `fklua compile` and `fklua mod`")
		fmt.Println("work on a wasm file you already have, but nothing here can")
		fmt.Println("produce one. Pick an arm and finish it:")
		fmt.Println("  Go:   " + goWhy)
		fmt.Println("  Rust: " + rustWhy)
		if tinygoOK && !wasmOptOK {
			fmt.Println()
			fmt.Println("TinyGo is installed and binaryen is not, which is the near")
			fmt.Println("miss: `brew install binaryen`, or set WASMOPT to a wasm-opt.")
		}
		if guest.ToolchainDeclaredAbsent() {
			fmt.Println()
			fmt.Println(guest.NoToolchainEnv + "=1 is set, which is how an environment")
			fmt.Println("declares this on purpose. Unset it if that was not deliberate.")
		}
		return fmt.Errorf("no guest toolchain is installed")
	}
	return nil
}

// doctorRow prints one row and reports whether the tool was found. found is the
// version string, or "" for a tool that is not on PATH.
func doctorRow(tool, found, want, what string) bool {
	status, shown := "MISSING", "--"
	if found != "" {
		status, shown = "ok", found
	}
	fmt.Printf("  %-22s %-12s %-7s docs say %s -- %s\n", tool, shown, status, want, what)
	return found != ""
}

// doctorRowBool is doctorRow for a fact with no version to print.
func doctorRowBool(name string, ok bool, want, what string) {
	status := "MISSING"
	if ok {
		status = "ok"
	}
	fmt.Printf("  %-22s %-12s %-7s docs say %s -- %s\n", name, "", status, want, what)
}

// firstVersionLike pulls the first number out of a tool's version line, which
// every tool below prints somewhere in its first line and none of them prints
// in the same position.
//
// THE DOT IS OPTIONAL AND THAT IS THE WHOLE POINT OF THIS COMMENT. Requiring
// one is the obvious spelling and it reported binaryen MISSING on a machine
// that had it: `wasm-opt --version` prints "wasm-opt version 131" -- a bare
// integer, because binaryen versions are a single number. The verdict line
// disagreed with its own table in the first run of this command, which is
// exactly the failure a doctor exists to not have.
//
// The trailing groups are greedy in the right direction for the rest: `go
// version go1.26.5` yields 1.26.5 rather than 1, since the match starts at the
// first digit and takes every dotted component after it.
var firstVersionLike = regexp.MustCompile(`[0-9]+(\.[0-9]+)*`)

// toolVersion runs a tool's version command and returns the first dotted number
// in its output, or "" if the tool is absent or says nothing recognisable.
//
// An error is deliberately indistinguishable from an absence here: a tool that
// is on PATH and cannot report its own version is not a tool this can vouch
// for, and the remedy -- install it properly -- is the same one.
func toolVersion(bin string, args ...string) string {
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return firstVersionLike.FindString(line)
}

func goVersion() string { return toolVersion("go", "version") }

// tinygoVersion reads TinyGo's own version rather than the Go one it embeds --
// `tinygo version` prints both, and the dotted number that comes FIRST is
// TinyGo's ("tinygo version 0.41.1 darwin/arm64 (using go version go1.26.5)").
func tinygoVersion() string { return toolVersion("tinygo", "version") }

// wasmOptVersion honours the same WASMOPT override TinyGo itself reads, so a
// binaryen that is not on PATH but IS configured reports as present -- which is
// the state guest.Available() already accepts.
func wasmOptVersion() string {
	if env := os.Getenv("WASMOPT"); env != "" {
		return toolVersion(env, "--version")
	}
	return toolVersion("wasm-opt", "--version")
}

func cargoVersion() string { return toolVersion("cargo", "--version") }

// factorioVersion reads the version out of the description the installed game
// ships, which is the same file `fklua api pull --from-install` reads and the
// only place a version is stated rather than inferred from a path.
func factorioVersion() string {
	p := installedAPIPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var v struct {
		ApplicationVersion string `json:"application_version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	return v.ApplicationVersion
}

// installedAPIPath is where the game's own runtime-api.json lives, honouring
// the FACTORIO_API_JSON override `api pull --from-install` already reads.
//
// The macOS Steam path is the only one hard-coded, because it is the only one
// this repo has ever been developed on and a guessed Linux path that is wrong
// reports a MISSING game to somebody who has one. FACTORIO_API_JSON is the
// answer everywhere else, and the row says so by being optional.
func installedAPIPath() string {
	if env := os.Getenv("FACTORIO_API_JSON"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Steam", "steamapps",
		"common", "Factorio", "factorio.app", "Contents", "doc-html", "runtime-api.json")
}
