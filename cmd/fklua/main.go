// Command fklua is the FkLua toolchain.
//
// `compile` turns one wasm module into a Lua chunk; `mod` packages that chunk
// as a mod Factorio will load. `bench` and `spectest` are the two gates:
// generated Lua has to stay within the M0 ratios and the conformance pass rate
// may rise but never fall.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/bench"
	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/spectest"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// fkluaVersion is stamped into a project lockfile, so a lock records WHICH
// compiler generated the bindings it pins -- a regeneration by a different
// version is a real difference even when the API did not move.
const fkluaVersion = "0.1.0"

const usage = `fklua -- WebAssembly to Lua 5.2 for Factorio

Usage:
  fklua compile IN.wasm [-o OUT.lua] [--nan=canonical|exact] [--opt=0..3]
            [--persist=MODE] [--gc=leaking|collected]
  fklua mod [IN.wasm] [-o DIR] [--zip] [--nan=MODE] [--opt=N] [--include DIR]...
            [--persist=table|packed|auto|none] [--fuel=N]
            [--gc=leaking|collected] [--api=VERSION] [--factorio-version X.Y]
            [--data-module DATA.wasm] [--stage KEY=a,@guest,b]...
            [--name NAME] [--version X.Y.Z] [--title T] [--author A]
            [--description D] [--dependency DEP]...
                                     (identity defaults to fklua.toml's [mod])
      IN.wasm           the CONTROL guest, and it is optional when this mod has
                        a data module ([fklua] data_module or --data-module).
                        A data-stage-only mod ships no control.lua,
                        fk_module.lua or fk_api_gen.lua, and --persist, --gc and
                        --fuel are refused there rather than ignored
      --dependency DEP  repeatable, and the list REPLACES [mod] dependencies
                        rather than adding to it -- so one manifest can package
                        several mods with different lists. ` + "`--dependency \"\"`" + `
                        alone is an empty list, and mixing it with a real value
                        is refused
  fklua api pull <version> | --from-install
  fklua api list [--current]        (--current: just the pin, one line, for a script)
  fklua api diff <from> <to> [--breaking] [--json PATH]
  fklua api check GUEST.wasm --to <version> [--from <version>] [--json]
                        exit 0 nothing this guest uses breaks, 1 something does
                        or the scan could not see everything, 2 the check could
                        not be run. --json writes one verdict object to stdout
  fklua init <mod-name> [--lang go,rust] [--api VERSION]
  fklua lock [--check]
  fklua meta --json     one JSON document describing this project, for tools.
                        Top-level keys: fklua (this version), manifest (the
                        file as written), effective (what ` + "`fklua mod`" + ` would
                        really use after every default -- title falls back to
                        name, author to "unknown", factorio_version to the
                        default pin's series, lang to ["go"], and an ABSENT gc
                        key means "leaking"), package (the identity
                        <name>_<version>, plus the zip name), and guest (the
                        per-language directory, generated bindings path and
                        conventional wasm artifact). --json is required: this
                        is a data interface with no human-facing form. It reads
                        fklua.toml and errors without one rather than guessing
  fklua docs [--lang go|rust] [--api VERSION] [-o DIR]
  fklua gen-bindings [--lang go,rust|all] [-o FILE] [--into DIR] [--check]
                                                       (default: fklua.toml's lang)
      -o FILE    one language's bindings to one file
      --into DIR repin a vendored FkLua checkout's committed bindings to this
                 project's pin -- what the library packages in its guest module
                 import, and what fklua mod refuses a mismatch of
  fklua bench [--runs N] [--json PATH]   run the M0 kernels under lua52f
  fklua bench --opt [--runs N]           compile the wasm kernels at each -opt
  fklua spectest [--filter S] [-v] [--nan=MODE] [--opt=N] [--gc=MODE]
  fklua doctor                           are the toolchains the docs name installed?
  fklua version

` + "`--gc=collected`" + ` says the guest was built with a collector, so its heap no
longer only grows -- TinyGo ` + "`-gc=custom`" + ` plus an import of guest/go/fkgc, or
for a Rust guest ` + "`cargo build --features fk/fkgc`" + ` and no source change at all.
It is the RECOMMENDED default for a new project and ` + "`fklua init`" + ` writes
` + "`gc = \"collected\"`" + ` into fklua.toml; the compile-flag default stays leaking so
that an existing build is not turned into a compile error naming a flag its author
never chose. There is no heap cap: collector metadata is ~31 KiB plus about 1% of
the heap. It is CHECKED -- a module that does not export the collector's pacing
surface is refused -- and it is refused for a wasip1 guest, which is the one arm
with no collector. See agents/gc.md.

` + "`--api`" + ` and ` + "`--factorio-version`" + ` are TWO AXES and they are not the same
question. ` + "`--api`" + ` picks the DESCRIPTION the bindings and the packaged member
table come from -- a build-time fact, defaulting to the general-availability
release rather than to the newest description shipped in api/ or to whatever is
installed here. ` + "`--factorio-version`" + ` is info.json's declaration about the
ENGINE the mod will run on, defaulting to the pin's major.minor series because
that is usually right; it is what to override when the two come apart, and it is
what the in-game gates in scripts/ pass. A guest that wants to know which engine
it is actually running on asks at RUN TIME -- helpers.game_version -- which is
how fkipc's version floor works, so a GA-pinned mod gets the whole IPC library
on a newer engine with no rebuild.

` + "`--data-module`" + ` gives the mod a SETTINGS and DATA stage written in Go or
Rust: a second wasm module, compiled from its own main package, that reaches
data.raw through the fkdata library. fklua writes a stage file for each hook the
module exports -- fk_settings, fk_data, fk_data_updates, fk_data_final_fixes --
and ` + "`--stage KEY=a,@guest,b`" + ` (or ` + "`[stages]`" + ` in fklua.toml) says what
order that file loads things in while hand-written Lua is still in the chain.
A data module must not import the generated fkapi bindings: those stages have no
runtime API, and packaging refuses one that does. See docs/data-stage.md.

` + "`bench`" + ` measures FkLua-style generated Lua against the Lua a mod author
would write by hand. ` + "`spectest`" + ` runs the official WebAssembly conformance
suite under lua52f and is the primary correctness gate: the pass rate recorded
in testdata/spec/PASSRATE may rise, never fall.

` + "`--opt`" + ` selects the optimization level, default 3. Level 0 disables every
pass and reproduces the M4 emitter exactly, which is what makes it the
reference when a miscompile has to be bisected against the optimizer. The
conformance suite must be green at EVERY level.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// `-h` ANYWHERE IS A REQUEST FOR HELP, not an unknown flag.
	//
	// Every subcommand parses its own arguments and rejects what it does not
	// know, so `fklua init my-mod --help` -- which is the first thing anybody
	// types -- came back "unknown argument \"--help\"" and exit 1. Handled here,
	// before dispatch, because there is one usage text and no subcommand has a
	// help of its own to print instead.
	//
	// The two flag spellings only, never a bare `help`: this scans flag VALUES
	// as well as flags, and `--description help` is a thing somebody could
	// plausibly write.
	for _, a := range os.Args[2:] {
		if a == "-h" || a == "--help" {
			fmt.Print(usage)
			return
		}
	}

	switch os.Args[1] {
	case "api":
		if err := runAPI(os.Args[2:]); err != nil {
			os.Exit(reportExit("fklua api", err))
		}
	case "docs":
		if err := runDocs(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua docs: %v\n", err)
			os.Exit(1)
		}
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua init: %v\n", err)
			os.Exit(1)
		}
	case "lock":
		if err := runLock(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua lock: %v\n", err)
			os.Exit(1)
		}
	case "meta":
		if err := runMeta(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua meta: %v\n", err)
			os.Exit(1)
		}
	case "bench":
		if err := runBench(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua bench: %v\n", err)
			os.Exit(1)
		}
	case "compile":
		if err := runCompile(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua compile: %v\n", err)
			os.Exit(1)
		}
	case "gen-bindings":
		if err := runGenBindings(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua gen-bindings: %v\n", err)
			os.Exit(1)
		}
	case "mod":
		if err := runMod(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua mod: %v\n", err)
			os.Exit(1)
		}
	case "spectest":
		if err := runSpectest(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua spectest: %v\n", err)
			os.Exit(1)
		}
	case "doctor":
		if err := runDoctor(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "fklua doctor: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("fklua " + fkluaVersion)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runBench(args []string) error {
	runs := 5
	jsonPath := ""
	optMode := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--opt":
			optMode = true
		case "--runs":
			if i+1 >= len(args) {
				return fmt.Errorf("--runs needs a value")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &runs); err != nil {
				return fmt.Errorf("bad --runs value %q", args[i])
			}
		case "--json":
			if i+1 >= len(args) {
				return fmt.Errorf("--json needs a path")
			}
			i++
			jsonPath = args[i]
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	h, err := luahost.Find()
	if err != nil {
		return err
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	if optMode {
		return runOptBench(h, root, runs)
	}
	dir := filepath.Join(root, "bench", "kernels")

	fmt.Printf("lua52f: %s\nkernels: %s\nruns per measurement: %d (median)\n\n", h.Bin, dir, runs)

	results, err := bench.Run(h, dir, bench.M0Kernels, runs)
	if err != nil {
		return err
	}

	report(results, bench.M0Kernels)

	if mismatches := bench.CheckChecksums(results); len(mismatches) > 0 {
		fmt.Println("\nCHECKSUM MISMATCH")
		for _, m := range mismatches {
			fmt.Println("  " + m)
		}
		return fmt.Errorf("variants disagree on the answer; timings are meaningless until fixed")
	}

	if jsonPath != "" {
		if err := writeJSON(jsonPath, results); err != nil {
			return err
		}
		fmt.Printf("\nwrote %s\n", jsonPath)
	}

	fails := bench.Gate(results, bench.M0Kernels)
	fmt.Println()
	if len(fails) > 0 {
		fmt.Println("M0 GATE: FAILED")
		for _, f := range fails {
			fmt.Println("  " + f)
		}
		return fmt.Errorf("%d kernel(s) over their ratio limit", len(fails))
	}
	fmt.Println("M0 GATE: PASSED -- generated Lua is within the ratio limits")
	return nil
}

// runOptBench compiles the wasm kernels at every optimization level and times
// what came out.
//
// Separate from the M0 gate on purpose. The M0 kernels are hand-written Lua
// standing in for generated code: they establish the ceiling and they do not
// move when the emitter improves. This one runs the actual compiler, so it is
// the only thing that can say whether a pass paid for itself.
func runOptBench(h *luahost.Host, root string, runs int) error {
	dir := filepath.Join(root, "bench", "wasm")
	levels := []int{0, 1, 2, 3}

	fmt.Printf("lua52f: %s\nkernels: %s\nruns per measurement: %d (median)\n", h.Bin, dir, runs)
	fmt.Printf("ratios are against -opt=0, so below 1.00 is faster\n\n")

	compile := func(path string, level int) (string, error) {
		im, err := loadModule(path)
		if err != nil {
			return "", err
		}
		lvl, err := analysis.ParseLevel(strconv.Itoa(level))
		if err != nil {
			return "", err
		}
		return luagen.EmitModuleWith(im, luagen.Options{Opt: lvl})
	}

	results, err := bench.RunOpt(h, dir, bench.OptKernels, levels, runs, compile)
	if err != nil {
		return err
	}
	fmt.Print(bench.ReportOpt(results, bench.OptKernels))

	if mismatches := bench.CheckChecksums(results); len(mismatches) > 0 {
		fmt.Println("CHECKSUM MISMATCH")
		for _, m := range mismatches {
			fmt.Println("  " + m)
		}
		return fmt.Errorf("levels disagree on the answer; an optimization that " +
			"computes something else is not an optimization")
	}
	fmt.Println("checksums agree at every level")
	return nil
}

func report(results []bench.Result, kernels []bench.Kernel) {
	byKernel := map[string][]bench.Result{}
	for _, r := range results {
		byKernel[r.Kernel] = append(byKernel[r.Kernel], r)
	}

	for _, k := range kernels {
		rs := byKernel[k.Name]
		if len(rs) == 0 {
			continue
		}
		limit := "informational"
		if k.MaxRatio > 0 {
			limit = fmt.Sprintf("limit %.0fx", k.MaxRatio)
		}
		fmt.Printf("%s  (%s)\n    %s\n", k.Name, limit, k.Why)

		sort.Slice(rs, func(i, j int) bool { return rs[i].NsPerOp < rs[j].NsPerOp })
		for _, r := range rs {
			ratio := ""
			if r.Variant != k.Baseline {
				mark := "  "
				if k.MaxRatio > 0 && r.Ratio > k.MaxRatio {
					mark = " !"
				}
				ratio = fmt.Sprintf("%s%.2fx", mark, r.Ratio)
			} else {
				ratio = "   (baseline)"
			}
			fmt.Printf("    %-9s %9.2f ns/op  %12s   %s ops\n",
				r.Variant, r.NsPerOp, ratio, humanInt(r.Ops))
		}
		fmt.Println()
	}
}

func writeJSON(path string, results []bench.Result) error {
	payload := map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"note": "ns/op is wall time net of interpreter startup, median of N runs. " +
			"Ratios compare FkLua-style generated Lua against hand-written Lua.",
		"results": results,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// exitError is a status a subcommand chose deliberately, rather than the
// blanket 1 an error gets.
//
// It exists because `api check` answers a QUESTION rather than performing a
// task: "nothing breaks" and "something breaks" are both successful runs, and
// only the third case -- the check could not be run -- is a failure in the
// ordinary sense. A caller that cannot tell those apart has to parse prose to
// find out whether its build is safe.
//
// An empty msg means the command has already said everything it has to say on
// stdout, so nothing is printed to stderr.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// reportExit prints whatever an error still has to say and returns the status
// to leave with. An error that named no code is the usual 1.
func reportExit(prefix string, err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, ee.msg)
		}
		return ee.code
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	return 1
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

// runSpectest executes the WebAssembly conformance suite under lua52f.
//
// Pass rate is the project's primary correctness metric, and it is compared
// against testdata/spec/PASSRATE: it may rise, never fall. That file is
// updated deliberately, in the same commit as the change that earns it.
func runSpectest(args []string) error {
	filter := ""
	verbose := false
	update := false
	nan := luagen.NaNCanonical
	opt := analysis.DefaultLevel
	gc := luagen.GCLeaking
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--opt=") {
			l, err := analysis.ParseLevel(strings.TrimPrefix(args[i], "--opt="))
			if err != nil {
				return err
			}
			opt = l
			continue
		}
		if strings.HasPrefix(args[i], "--gc=") {
			m, err := luagen.ParseGCMode(strings.TrimPrefix(args[i], "--gc="))
			if err != nil {
				return err
			}
			gc = m
			continue
		}
		switch args[i] {
		case "--filter":
			if i+1 >= len(args) {
				return fmt.Errorf("--filter needs a value")
			}
			i++
			filter = args[i]
		case "-v", "--verbose":
			verbose = true
		case "--update-passrate":
			update = true
		case "--nan=exact", "--nan=canonical", "--nan=fast", "--nan=strict":
			m, err := luagen.ParseNaNMode(strings.TrimPrefix(args[i], "--nan="))
			if err != nil {
				return err
			}
			nan = m
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	h, err := luahost.Find()
	if err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "testdata", "spec")

	if nan != luagen.NaNCanonical {
		fmt.Printf("NaN mode: %s\n\n", nan)
	}
	if opt != analysis.DefaultLevel {
		fmt.Printf("optimization: -opt=%s\n\n", opt)
	}
	if gc != luagen.GCLeaking {
		fmt.Printf("gc: --gc=%s\n\n", gc)
	}
	outs, err := spectest.RunDirWith(h, dir, spectest.Options{NaN: nan, Opt: opt, GC: gc})
	if err != nil {
		return err
	}
	if len(outs) == 0 {
		fmt.Println("no converted suite files under testdata/spec; run scripts/fetch-spec.sh")
		return nil
	}

	shown := 0
	for _, o := range outs {
		if filter != "" && !strings.Contains(o.File, filter) {
			continue
		}
		shown++
		fmt.Println(o)
		if verbose || o.Failed > 0 {
			limit := len(o.Failures)
			if !verbose && limit > 15 {
				limit = 15
			}
			for _, f := range o.Failures[:limit] {
				fmt.Println("    " + f)
			}
			if limit < len(o.Failures) {
				fmt.Printf("    ... and %d more (pass -v for all)\n", len(o.Failures)-limit)
			}
		}
		if verbose && len(o.Skips) > 0 {
			for _, s := range o.Skips {
				fmt.Println("    skip: " + s)
			}
		}
	}
	if shown == 0 {
		return fmt.Errorf("filter %q matched no suite files", filter)
	}

	total, passed, failed, skipped := spectest.Totals(outs)
	rate := 0.0
	if passed+failed > 0 {
		rate = float64(passed) / float64(passed+failed) * 100
	}
	fmt.Printf("\ntotal: %d assertions, %d passed, %d failed, %d skipped (%.2f%%)\n",
		total, passed, failed, skipped, rate)

	// The recorded baseline describes the default mode; a run in another mode
	// is informational and must not move it.
	if nan != luagen.NaNCanonical {
		if failed > 0 {
			return fmt.Errorf("%d assertion(s) failed", failed)
		}
		return nil
	}

	ratePath := filepath.Join(dir, "PASSRATE")
	if update {
		body := fmt.Sprintf("%d/%d\n", passed, passed+failed)
		if err := os.WriteFile(ratePath, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d/%d)\n", ratePath, passed, passed+failed)
		return nil
	}

	prev, err := os.ReadFile(ratePath)
	if err != nil {
		fmt.Printf("\nno %s yet; record this baseline with --update-passrate\n", ratePath)
		if failed > 0 {
			return fmt.Errorf("%d assertion(s) failed", failed)
		}
		return nil
	}
	var wantPass, wantRun int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(prev)), "%d/%d", &wantPass, &wantRun); err != nil {
		return fmt.Errorf("malformed %s: %q", ratePath, string(prev))
	}
	if passed < wantPass {
		return fmt.Errorf("pass rate regressed: %d passing, previously %d. "+
			"The suite is the primary correctness gate; it may rise, never fall", passed, wantPass)
	}
	if passed > wantPass {
		fmt.Printf("pass rate improved: %d -> %d. Record it with --update-passrate\n",
			wantPass, passed)
	}
	if failed > 0 {
		return fmt.Errorf("%d assertion(s) failed", failed)
	}
	return nil
}

// checkGC refuses --gc=collected for a module the collector cannot be honestly
// claimed for, with a diagnostic that names the reason rather than the rule.
//
// THE PRECONDITION IS THE COLLECTOR, NOT THE TOOLCHAIN, and for a while this
// function had it backwards: it refused wasip1 and accepted everything else,
// including every Rust guest and every Go guest built the default -gc=leaking
// way.
//
// THAT IT IS SURFACE-BASED IS WHY THE RUST COLLECTOR NEEDED NOTHING HERE. When
// guest/rust/fkgc landed, a collected Rust guest began exporting the same three
// functions a collected Go guest exports and this function accepted it with no
// line changing -- which is what asking a toolchain-agnostic question buys. The
// only edit it forced was to the DIAGNOSTIC below, which used to tell an author
// that no Rust collector existed.
// Such a module got the collected-mode emitter gates and a control.lua arming a
// write barrier, with no collector anywhere to make either mean anything.
// factorio.CollectorSurface() is the toolchain-agnostic form of the question.
//
// The two refusals are ordered, and the order is the point: a wasip1 guest CAN
// carry the surface -- fkgc links there -- so the surface check would wave it
// through, and the hazard wasip1 names is specific and measured.
//
// wasip1 is the second case, and agents/gc.md section 1 is the argument. Root discovery
// there goes through a second path: a goroutine's stack is an ordinary heap
// allocation reachable from internal/task.currentTask, which a conservative
// collector that scans every reachable block's contents finds without knowing
// goroutines exist. The evidence says it works. What has not happened is anyone
// RUNNING it under a collector, and one of the two hazards is measured and real
// -- a task's stackState holds csp = stack + stackSize, a pointer one past the
// end of a block, and TestTheCollectorKeepsWhatIsReachable records that this
// collector does not retain through one of those.
//
// So the gate is not "it cannot work". It is that shipping an untested second
// root-discovery path is how a soundness bug gets into a lockstep game, and the
// acceptance vehicle for stages B to D is a -scheduler=none event handler.
//
// WHERE THE MODE CAME FROM IS PART OF THE DIAGNOSTIC, since `gc` became an
// fklua.toml key. Both refusals below open by naming "--gc=collected", which is
// exactly right when somebody typed it and actively misleading when they did
// not: the first thing a reader does with a message naming a flag is search
// their command line for it, and a manifest-supplied mode is not there. `from`
// is the empty string for a flag and the manifest path otherwise.
func checkGC(gc luagen.GCMode, im *ir.Module, from string) error {
	if gc != luagen.GCCollected || im.Source == nil {
		return nil
	}
	said, saidWhere, unsay := "--gc=collected", "the --gc flag on this command line",
		"pass --gc=leaking instead"
	if from != "" {
		said = "gc = \"collected\" (from " + from + ")"
		saidWhere = from + ", key gc"
		unsay = "set gc = \"leaking\" in " + from
	}
	for _, imp := range im.Source.Imports {
		if imp.Module != "wasi_snapshot_preview1" {
			continue
		}
		return fmt.Errorf(
			"%s is refused for a wasip1 guest.\n"+
				"  what asked for it: %s\n"+
				"  what the module says: it imports %s.%s, which only a wasip1 "+
				"build does -- so this guest carries TinyGo's asyncify scheduler\n"+
				"A parked goroutine's stack is findable in principle -- it is an "+
				"ordinary heap allocation reachable from internal/task.currentTask "+
				"-- but that path has never been run under a collector, and a "+
				"task's csp is a pointer ONE PAST THE END of its stack block, "+
				"which this collector does not retain through. See agents/gc.md "+
				"section 1.\n"+
				"Two ways to reconcile:\n"+
				"  (1) BUILD IT FOR wasm-unknown, with -target=wasm-unknown "+
				"-scheduler=none. A mod event handler cannot block anyway, so this "+
				"is the shape a guest normally has.\n"+
				"  (2) KEEP wasip1 AND DROP THE MODE: %s.",
			said, saidWhere, imp.Module, imp.Name, unsay)
	}
	if missing := missingCollectorSurface(im); len(missing) > 0 {
		// BOTH SIDES, NAMED, AND THEN THE TWO WAYS OUT -- which is the shape the
		// rest of this CLI's refusals use, and which this one only half had.
		//
		// It said what the module was missing and how to build a collector in
		// either language, and it left the reader to work out that "how to build
		// a collector" and "how to stop asking for one" were alternatives. A
		// first-time author read it as slightly cryptic and they were right: the
		// two facts a mismatch is made of -- what asked, and what the artefact
		// is -- were a clause apart in one sentence, and only the manifest arm
		// ever said them as a pair.
		//
		// The manifest arm keeps its extra line, because there the mismatch is
		// between two things the SAME PROJECT owns: an author who changes the
		// tinygo flag and not the key, or the key and not the flag, is the only
		// way those can disagree, so the remedy is "these two, and they must
		// match" rather than "one of these".
		disagree := ""
		if from != "" {
			disagree = fmt.Sprintf("\n  THE MANIFEST AND THE BUILD DISAGREE, and "+
				"they are both yours: %s says gc = \"collected\" and the guest it "+
				"packages was built without a collector. Changing one alone lands "+
				"back here.", from)
		}
		return fmt.Errorf(
			"%s is refused for a guest that carries no collector.\n"+
				"  what asked for it: %s\n"+
				"  what the module says: it does not export %s, and those "+
				"exports ARE the collector as far as the host can tell -- so it "+
				"was built WITHOUT one%s\n"+
				"control.lua binds all of them or none: it writes the pages the "+
				"guest dirtied into the buffer fk_gc_dirty_base/fk_gc_dirty_cap "+
				"describe and drives fk_gc_step from a one-shot on_tick. Without "+
				"them the mode is not inert -- it still takes the inlined 8-byte "+
				"store back out of line, and still emits the write barrier's "+
				"arming surface -- so the guest pays for a collector it does not "+
				"have and nothing ever collects.\n"+
				"Two ways to reconcile, and exactly one of them is what you meant:\n"+
				"  (1) BUILD THE GUEST WITH ITS COLLECTOR.\n"+
				"      Go:   -gc=custom in place of -gc=leaking, plus "+
				"`import _ \"github.com/Techrocket9/fklua/guest/go/fkgc\"` -- which "+
				"is the guest/go/gc.go `fklua init` scaffolds. The flag alone does "+
				"not link.\n"+
				"      Rust: `cargo build --release --target "+
				"wasm32-unknown-unknown -p <guest> --features fk/fkgc`, and NO "+
				"source change: guest/rust/fk owns the single #[global_allocator] "+
				"site and that feature swaps its bump arena for guest/rust/fkgc.\n"+
				"      Either way, rebuild the wasm and package that.\n"+
				"  (2) STOP ASKING FOR ONE: %s. Without the collector a Rust "+
				"guest's dealloc is a no-op and a Go guest's -gc=leaking never "+
				"frees, so --gc=leaking is what honestly describes the artefact "+
				"you have.\n"+
				"See agents/guests.md, \"the guest heap budget\", item 0.",
			said, saidWhere, strings.Join(missing, ", "), disagree, unsay)
	}
	return nil
}

// missingCollectorSurface reports which of the collector's exports this module
// does not have.
//
// The names come from factorio.CollectorSurface() rather than from a list here,
// because control.lua's binding of them and this refusal are the same fact:
// two spellings of it would drift the way factorio.Hooks and fk_mod.lua already
// did once, and the direction that drifts is always the unchecked one.
func missingCollectorSurface(im *ir.Module) []string {
	have := make(map[string]bool, len(im.Exports))
	for _, e := range im.Exports {
		have[e.Name] = true
	}
	var missing []string
	for _, name := range factorio.CollectorSurface() {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// runCompile compiles one wasm module to Lua and reports anything the emitted
// code cannot reproduce exactly.
func runCompile(args []string) error {
	var in, out string
	nan := luagen.NaNCanonical
	opt := analysis.DefaultLevel
	persist := luagen.PersistNone
	gc := luagen.GCLeaking
	gcFromFlag := false
	gcFrom := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o":
			if i+1 >= len(args) {
				return fmt.Errorf("-o needs a path")
			}
			i++
			out = args[i]
		case strings.HasPrefix(args[i], "--nan="):
			m, err := luagen.ParseNaNMode(strings.TrimPrefix(args[i], "--nan="))
			if err != nil {
				return err
			}
			nan = m
		case strings.HasPrefix(args[i], "--persist="):
			m, err := luagen.ParsePersistMode(strings.TrimPrefix(args[i], "--persist="))
			if err != nil {
				return err
			}
			persist = m
		case strings.HasPrefix(args[i], "--gc="):
			m, err := luagen.ParseGCMode(strings.TrimPrefix(args[i], "--gc="))
			if err != nil {
				return err
			}
			gc, gcFromFlag = m, true
		case strings.HasPrefix(args[i], "--opt="):
			l, err := analysis.ParseLevel(strings.TrimPrefix(args[i], "--opt="))
			if err != nil {
				return err
			}
			opt = l
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		default:
			if in != "" {
				return fmt.Errorf("expected one input module, got %q and %q", in, args[i])
			}
			in = args[i]
		}
	}
	if in == "" {
		return fmt.Errorf("no input module")
	}

	// `gc` travels from the manifest here too, and only `gc`. `compile` emits a
	// chunk rather than a mod, so it has no identity to fill and no data stage
	// to include -- but the collector is a property of the MODULE, and a chunk
	// compiled with a different --gc than the mod built from the same wasm is a
	// chunk whose 8-byte stores and write-barrier surface do not match it.
	// Reading one key is what stops `compile` and `mod` disagreeing about the
	// same guest in the same directory.
	if proj, ok, err := loadProject(); err != nil {
		return err
	} else if ok && proj.GC != "" && !gcFromFlag {
		m, err := luagen.ParseGCMode(proj.GC)
		if err != nil {
			return fmt.Errorf("%s: [fklua] gc: %w", projectFile, err)
		}
		gc, gcFrom = m, projectFile
	}

	im, err := loadModule(in)
	if err != nil {
		return err
	}
	if err := checkGC(gc, im, gcFrom); err != nil {
		return err
	}
	opts := luagen.Options{NaN: nan, Opt: opt, Persist: persist, GC: gc}
	src, err := emitWithDiagnostics(im, opts)
	if err != nil {
		return err
	}

	if out == "" {
		fmt.Print(src)
		return nil
	}
	if err := os.WriteFile(out, []byte(src), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes, NaN mode: %s, -opt=%s)\n", out, len(src), nan, opt)
	return nil
}

// attachAPI generates the member table this guest needs, pruned to the members
// it actually calls, and says what it did.
//
// The full table is about a megabyte of Lua at the 2.1.14 pin, for every member
// the host binds (`host_members_bound` in census.json). A guest that calls five
// of them has no business shipping the rest -- in every save, in every
// download, and in Factorio's parse time at load. The counts this function
// PRINTS are all derived from the tables it just generated, which is why they
// are the ones to trust; the magnitude here is only the reason the pruning
// exists.
//
// The VERSION is a parameter rather than the default, because the ids in this
// table only mean anything paired with the bindings the guest was compiled
// against -- see runMod's apiVersion. It is printed with every line this
// function emits, so a project whose pin does not match its bindings says so in
// its own build output instead of in game.
//
// AND PRINTING IT WAS NOT ENOUGH, which is what checkAPIPin below is for. A
// version in build output is only a warning to whoever reads build output, and
// the mismatch this guards against produced no other symptom at all: the
// downstream instance in pin.go shipped, ran, logged and ticked while calling
// entirely different members. The pairing is now PROVEN from the guest's own
// exports and refused when it fails, on the same principle as every other
// refusal in this CLI -- a stub that raises in game is worse than a build that
// stops here.
func attachAPI(pkg *factorio.Package, im *ir.Module, version string, pin pinSource) error {
	used, complete := factorio.UsedMembers(im)
	usedEv, evComplete := factorio.UsedEvents(im)
	usedDef, defComplete := factorio.UsedDefines(im)
	// THE HOOK PAYLOAD IS PRUNED BY AN EXPORT, not by a constant scan, and it is
	// the only thing in this table that is. There is no id to find: Factorio
	// raises on_configuration_changed and the guest never asks for it, so what
	// says whether the layout can ever be used is whether the guest exports the
	// hook at all. A guest that does not can never be handed one, and packaging
	// the layout anyway would be bytes in every save for a dispatch that cannot
	// happen.
	wantsConfChanged := false
	for _, e := range pkg.Exports {
		if e == factorio.ConfChangedHook {
			wantsConfChanged = true
			break
		}
	}
	if complete && evComplete && defComplete && !wantsConfChanged &&
		len(used) == 0 && len(usedEv) == 0 && len(usedDef) == 0 {
		// Nothing to attach. The packager still writes an empty table, because
		// control.lua requires the file unconditionally.
		//
		// ...unless the guest exports the configuration-changed hook, whose
		// payload layout is the one entry here that no call site can prove.
		return nil
	}

	a, err := factorio.LoadAPI(apiPath(version))
	if err != nil {
		return fmt.Errorf("loading the API description for %s: %w", version, err)
	}
	// AFTER the early return above, and that placement is the whole scope of
	// this guard: a guest that calls no member, subscribes to no event and
	// reads no define gets no table, and a table that does not exist cannot
	// disagree with anything. It is the same line `compile` sits on -- it emits
	// a bare chunk and never attaches a table, so there is no version for it to
	// be wrong about.
	//
	// AND AFTER LoadAPI, so the comparison is against the description's own
	// `application_version` rather than against a directory name. `api pull`
	// files a description under the version the FILE claims, so the two agree;
	// comparing the thing the ids were actually assigned over is one fewer
	// assumption, and it is free here.
	if err := checkAPIPin(im, a.ApplicationVersion, pin); err != nil {
		return err
	}
	// ...AND THE OTHER HALF, which the pin cannot reach: whether the guest was
	// built against THESE bindings or against an older generation of the same
	// description. A warning rather than a refusal -- see warnAPISignature.
	warnAPISignature(im, a, pin)
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)
	defs := factorio.GenerateDefines(a)
	full, fullEv, fullDef := len(report.Members), len(events.Events), len(defs.Defines)

	if !wantsConfChanged {
		events = events.WithoutConfChanged()
	}
	if evComplete {
		events = events.Only(usedEv)
		if len(events.Events) > 0 {
			fmt.Printf("API %s: %d events subscribed, of %d\n", version, len(events.Events), fullEv)
		}
	} else {
		fmt.Printf("API %s: all %d events -- an event id was not a compile-time constant\n",
			version, fullEv)
	}

	// The whole defines set is ~45 KB of paths. A guest naming four directions
	// ships four, by the same scan and the same "cannot prove it, ship it all"
	// rule the members follow.
	if defComplete {
		defs = defs.Only(usedDef)
		if len(defs.Defines) > 0 {
			fmt.Printf("API %s: %d defines read, of %d\n", version, len(defs.Defines), fullDef)
		}
	} else {
		fmt.Printf("API %s: all %d defines -- a define id was not a compile-time constant\n",
			version, fullDef)
	}
	report.Defines = defs

	if complete {
		report = report.Only(used)
		fmt.Printf("API %s: %d members, pruned from %d\n", version, len(report.Members), full)
	} else {
		// A member id the compiler could not prove constant means some call is
		// reached by an id it cannot see. Shipping everything is the only safe
		// answer, and saying so matters: a guest hitting this is usually doing
		// something its bindings could express directly.
		fmt.Printf("API %s: all %d members -- a member id was not a compile-time "+
			"constant, so the table cannot be pruned\n", version, full)
	}

	src, err := report.LuaSourceWith(a, events)
	if err != nil {
		return fmt.Errorf("generating the member table: %w", err)
	}
	pkg.APITable = src
	return nil
}

// checkAPIPin refuses a package whose guest was built against a different
// description than the table about to be attached was generated from.
//
// THE DEFECT THIS CLOSES, in the words of the downstream mod that measured it:
// a FkLua guest links a generated binding set, `fklua mod` packages a member
// table, and until now nothing made the two agree. The library packages that
// live inside the FkLua guest module -- fkipc above all -- import THAT module's
// own committed fkapi, so a consumer that vendors a FkLua checkout and pins
// anything other than the default gets bindings at one version and a table at
// another, by construction and with no error anywhere. Measured at pin 2.1.14
// against committed bindings at 2.0.77: fkipc subscribed to event 207 believing
// it was on_udp_packet_received and got on_train_changed_state, and read
// helpers.game_version and got LuaForce.object_name, so its engine-floor gate
// parsed "0.0.0" and the library went inert. The mod loaded, ran, logged and
// ticked. One log line about a version was the entire symptom.
//
// SILENCE WHEN THERE IS NO STAMP, and that is deliberate rather than a gap.
// Bindings generated before the stamp existed carry none, and a guest that
// links no generated bindings at all carries none either. Refusing those would
// break every guest built against an older checkout -- including a GA-pinned
// one that has nothing wrong with it -- to catch a case this cannot prove.
// `api check` exits non-zero on an incomplete scan because there the alternative
// is reporting "clean" for a guest it could not read; here the alternative is
// refusing a build that is correct, so the two go opposite ways for one reason.
// What makes it converge anyway is that the stamp ships WITH the bindings: a
// guest regenerated at any pin, including the default, gets one.
// pinSource is where the resolved API pin came from: the phrase to blame in a
// refusal, and the manifest that said so when a manifest did.
//
// TWO FIELDS rather than one phrase, because deriving "was it the manifest" by
// comparing the phrase against a rebuilt string would be two places spelling
// one fact -- this repo's most-repeated failure shape, and here it would fail
// SILENTLY, quietly downgrading the advice to the flag form while everything
// still worked.
type pinSource struct {
	what string // "fklua.toml, [fklua] api", "the --api flag ...", the default
	file string // the manifest that chose it, or "" when nothing did
}

// warnAPISignature says when a guest was built against a DIFFERENT GENERATION of
// the bindings than the table about to be attached comes from.
//
// THE DEFECT, as BetterBeltBalancer reported it (FKLUA-GAPS item 18): `fklua
// mod` packages a wasm built against old bindings with a fresh member table AT
// THE SAME PIN without complaint. The pin stamp proves both halves came from one
// DESCRIPTION and cannot prove they came from one GENERATION, and at one pin the
// ids move whenever the generator grows -- a member kind added, an operator's
// write half emitted, three global functions appended, a handle variant over an
// attribute. Every id then resolves to a different member, silently wherever the
// kinds line up, with the first symptom in a player's game.
//
// A WARNING RATHER THAN A REFUSAL, and the reasoning is worth having beside the
// code because the pin's own check goes the other way. The digest is
// CONSERVATIVE IN THE WRONG DIRECTION: a generator change that only APPENDS
// members leaves every existing id meaning exactly what it meant -- the three
// global functions were appended after every class precisely so that they would
// -- and a whole-table digest cannot tell that from a renumbering. Refusing
// would stop builds that are correct, which is what `checkAPIPin`'s
// silence-on-absent rule and this repo's "a check whose repair cannot be run
// from the consumer's checkout gets reverted rather than satisfied" both point
// at. The pin keeps refusing the case that is ALWAYS wrong; this names the case
// that MAY be, loudly, with the repair.
//
// THE LOCK HASH WAS THE OTHER CANDIDATE AND IT IS THE WRONG INSTRUMENT. It
// answers whether the generated bindings TREE matches what `fklua lock` last
// recorded -- which is `fklua lock --check`'s question -- and the wasm is in
// neither. An author who regenerated, re-locked and did not rebuild has a
// current lock and a stale wasm, which is exactly the reported defect and
// exactly what a lock-hash comparison cannot see.
//
// AN ABSENT STAMP IS SILENCE, as an absent pin is: bindings older than the stamp
// carry none, and a guest linking no generated bindings carries none either.
func warnAPISignature(im *ir.Module, a *factorio.API, from pinSource) {
	sigs := factorio.GuestSigs(im)
	if len(sigs) == 0 {
		return
	}
	want := factorio.SigExport(factorio.APISignature(a))
	if len(sigs) == 1 && sigs[0] == want {
		return
	}
	repin := "fklua gen-bindings"
	if from.file != "" {
		repin = "fklua gen-bindings (this project pins " + a.ApplicationVersion +
			" in " + from.file + ")"
	}
	fmt.Fprintf(os.Stderr,
		"WARNING: this guest was built against a DIFFERENT GENERATION of the "+
			"API %s bindings\n"+
			"  what the module says: it exports %s\n"+
			"  what this generator produces: %s\n"+
			"Member, event and define ids are dense sorted indices over one\n"+
			"GENERATION's table, and they move whenever the generator grows -- so a\n"+
			"guest compiled against older bindings can call different members than\n"+
			"the table being packaged names, silently wherever the kinds line up.\n"+
			"An id that only MOVED because members were appended after it is still\n"+
			"correct, which is why this is a warning and not a refusal.\n"+
			"Regenerate and REBUILD THE GUEST: %s, then build again.\n",
		a.ApplicationVersion, strings.Join(sigs, " and "), want, repin)
}

func checkAPIPin(im *ir.Module, version string, from pinSource) error {
	pins := factorio.GuestPins(im)
	if len(pins) == 0 {
		return nil
	}
	want := factorio.PinExport(version)
	if len(pins) == 1 && pins[0] == want {
		return nil
	}

	// The ids are what actually break, so the paragraph that says so is shared
	// by both refusals rather than written twice with a word different.
	const why = "Member, event and define ids are DENSE SORTED INDICES over one " +
		"description's\nset, so a member added or removed anywhere shifts every " +
		"later id. The table\nthis would package answers the guest's calls with " +
		"DIFFERENT MEMBERS -- silently\nwherever the kinds line up, in a lockstep " +
		"game. That is why this stops here\nrather than warning: the symptom in " +
		"the field was one log line about a version."

	if len(pins) > 1 {
		var named []string
		for _, p := range pins {
			named = append(named, pinVersionName(p))
		}
		return fmt.Errorf(
			"this guest links %d generated binding sets, at APIs %s.\n"+
				"  what the module says: it exports %s\n"+
				"%s\n"+
				"A guest may link exactly ONE. Two sets in one module disagree "+
				"about what every\nid past their first difference means, and a "+
				"packaged table can only match one\nof them -- so whichever is "+
				"not %s is calling the wrong member.\n"+
				"Regenerate every binding set this guest imports at ONE pin: "+
				"`fklua gen-bindings`\nfor the project's own, and "+
				"`fklua gen-bindings --into DIR` for the committed copy\ninside a "+
				"vendored FkLua checkout. Then rebuild the guest.",
			len(pins), strings.Join(named, " and "), strings.Join(pins, " and "),
			why, version)
	}

	guest := pinVersionName(pins[0])
	repin := "pass --api=" + guest
	if from.file != "" {
		repin = "set api = \"" + guest + "\" in " + from.file
	}
	return fmt.Errorf(
		"this guest was built against API %s bindings, and this package is being "+
			"made at API %s.\n"+
			"  what asked for %s: %s\n"+
			"  what the module says: it exports %s, which every generated binding "+
			"set carries\n    to name the description its ids were assigned over\n"+
			"%s\n"+
			"Two ways to reconcile:\n"+
			"  (1) REGENERATE THE BINDINGS THIS GUEST IMPORTS at %s, and rebuild it.\n"+
			"      For the project's own bindings that is `fklua gen-bindings`. For\n"+
			"      the committed bindings inside a vendored FkLua checkout -- which\n"+
			"      is where fkipc and every other library package in the guest\n"+
			"      module gets its fkapi -- it is `fklua gen-bindings --into DIR`.\n"+
			"  (2) PACKAGE AT THE PIN THE GUEST WAS BUILT AT: %s.",
		guest, version, version, from.what, pins[0], why, version, repin)
}

// pinVersionName turns a stamp export name back into a version to say out loud.
//
// BY MATCHING RATHER THAN BY UNMANGLING. PinExport replaces every character
// outside [0-9A-Za-z] with '_' and is therefore not injective, so there is no
// inverse to write; what there is instead is a short list of versions this
// checkout has committed, and mangling each of those answers the question
// exactly. A guest built against a description this checkout does not carry
// falls back to the raw export name, which is still enough to act on and is
// honest about what is known.
func pinVersionName(stamp string) string {
	if ents, err := os.ReadDir(apiDir()); err == nil {
		for _, e := range ents {
			if e.IsDir() && factorio.PinExport(e.Name()) == stamp {
				return e.Name()
			}
		}
	}
	return strings.TrimPrefix(stamp, factorio.PinExportPrefix) +
		" (no description for it is committed here)"
}

// heapBytes is the module's declared initial linear memory, which is what
// --persist=auto decides on. A module with no memory reports zero and lands on
// the table side, where it has nothing to persist anyway.
func heapBytes(im *ir.Module) uint64 {
	if im.Source == nil || !im.Source.Memory.Has {
		return 0
	}
	return uint64(im.Source.Memory.Min) * 65536
}

// buildIDDomain is what this hash is FOR, written into the preimage so that no
// other sha256 in this project can ever produce a value that means something
// here. The trailing version number is the construction's own: if a later field
// is folded in, bumping it is what keeps a stamp computed by an older fklua from
// coinciding with one computed by a newer over different inputs.
const buildIDDomain = "fklua/build-id/v2\x00"

// buildID identifies a guest build by the CONTENT of the module it was compiled
// from AND by the API version it was PACKAGED against.
//
// It is what a save records so a later load can tell whether the heap in it was
// laid out by the build now running. Hashing the whole module rather than, say,
// only the data segments is the conservative choice on purpose: a change
// anywhere in the guest can move how the heap is interpreted even when the
// segments themselves are byte-identical, and being wrong in that direction
// corrupts a save rather than merely resetting one.
//
// THE API PIN IS PART OF THE BUILD, and until this it was not part of the
// stamp -- so one wasm packaged against two --api pins produced two mods with
// one identity, and same_build() adopted a heap across them. The package
// carries pin-derived facts the HEAP depends on. API.event_scratch is the
// largest subscribed event's payload, computed from the PACKAGED event table,
// and it is the size a cached buffer in the guest heap was allocated at; a
// define id the guest resolves once and caches is a per-build number living in
// that same heap. And member, event and define ids are all dense sorted indices
// over one version's set (internal/factorio/gen.go), so a pin change shifts
// them as a CLASS -- which makes a cross-pin adopt unsound in general rather
// than in the one or two places it can be pointed at today. P12's size guard in
// fk_mod.lua closes one symptom of it and is kept for the case this cannot
// reach (fk_migrate_adopt, which hands over another build's heap deliberately).
//
// THE FOLD IS UNAMBIGUOUS BY THE WIDTH OF ITS FIELDS, not by a separator, and
// that is the whole of the collision argument:
//
//	sha256( domain || sha256(module) || pin )
//
// The domain is a compile-time constant of known length; the module contributes
// exactly 32 bytes, at a constant offset, whatever it contains; the pin is
// everything after. So there is exactly one way to read a preimage back into
// (module digest, pin), and two builds hash equal only if their module digests
// and their pins are both equal.
//
// A wasm that happens to CONTAIN the version bytes -- or the domain tag, or a
// separator -- cannot move any of those boundaries, because the module's bytes
// never appear in this preimage at all. Only its digest does. The obvious
// alternative, sha256(module || sep || pin), does not have that property for
// free: its boundary is FOUND by scanning content, so its injectivity rests on
// an assumption about which field may contain sep -- and the pin is
// user-supplied (a manifest key or an --api flag), which is the side of that
// assumption one does not want to be defending.
//
// It changes the value for every build, including one at the default pin, so
// the first repackage after this lands looks like a rebuild to a save written
// before it. That is a one-time reset down the designed path -- fk_migrate on a
// fresh heap, or a logged discard -- and it is unavoidable: any stamp that can
// tell two pins apart necessarily differs from one that could not.
func buildID(raw []byte, apiVersion string) string {
	module := sha256.Sum256(raw)
	h := sha256.New()
	h.Write([]byte(buildIDDomain))
	h.Write(module[:])
	h.Write([]byte(apiVersion))
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// loadModuleID is loadModule plus the build identity of the bytes it read,
// which is a fact about the module AND the pin it is being packaged against --
// so the caller has to have resolved that pin first. See runMod, where the
// manifest/flag/default resolution deliberately sits above this call.
func loadModuleID(path, apiVersion string) (*ir.Module, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	im, err := decodeModule(path, raw)
	if err != nil {
		return nil, "", err
	}
	return im, buildID(raw, apiVersion), nil
}

// loadModule decodes a module from .wasm or .wat and resolves every function.
func loadModule(path string) (*ir.Module, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeModule(path, raw)
}

func decodeModule(path string, raw []byte) (*ir.Module, error) {
	var mod *wasm.Module
	var err error
	if strings.HasSuffix(path, ".wat") {
		mod, err = wasm.DecodeWAT(string(raw))
	} else {
		mod, err = wasm.Decode(raw)
	}
	if err != nil {
		return nil, err
	}
	return ir.BuildModule(mod)
}

// emitWithDiagnostics renders a module and writes everything the emitted code
// cannot reproduce exactly to stderr.
func emitWithDiagnostics(im *ir.Module, opts luagen.Options) (string, error) {
	src, err := luagen.EmitModuleWith(im, opts)
	if err != nil {
		return "", err
	}

	// Functions the compiler could not handle at all are reported first: they
	// are a harder problem than a NaN bit pattern, and silently emitting a
	// raising stub without saying so would be indefensible.
	unsupported := 0
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			unsupported++
			fmt.Fprintf(os.Stderr, "warning: %v\n", f.Unsupported)
		}
	}
	if unsupported > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d function(s) will raise if called\n\n", unsupported)
	}

	if ds := luagen.Diagnose(im, opts); len(ds) > 0 {
		fmt.Fprint(os.Stderr, luagen.FormatDiagnostics(ds))
	}
	return src, nil
}

// runMod compiles a module and packages it as a mod Factorio will load.
func runMod(args []string) error {
	var in, outDir string
	var include []string
	zip := false
	info := factorio.Info{FactorioVersion: factorio.DefaultFactorioVersion}
	nan := luagen.NaNCanonical
	opt := analysis.DefaultLevel
	persist := luagen.PersistTable
	// WHETHER --persist WAS TYPED, for the reason gcFromFlag exists a few lines
	// down and for one more: a packaging with no CONTROL module has nothing to
	// persist, so a typed --persist is a contradiction while an untyped default
	// is simply never reached. Refused below, beside --gc and --fuel.
	persistFromFlag := false
	gc := luagen.GCLeaking
	// WHETHER --gc WAS TYPED, which is a different question from what it is.
	// The manifest supplies `gc` and the flag overrides it, so "leaking" has to
	// be distinguishable from "nobody said" -- and the zero value of GCMode is
	// GCLeaking, so the value alone cannot tell them apart. Every other
	// manifest-backed field is a string whose empty value carries that
	// information for free; this one is an enum and has to carry it separately.
	gcFromFlag := false
	gcFrom := ""
	fuel := 0
	fuelFromFlag := false
	// WHICH API DESCRIPTION THE PACKAGED TABLES COME FROM, and it has to be the
	// one the guest's bindings were generated against. Member ids are dense
	// sorted indices over a version's member set (internal/factorio/gen.go), so
	// a member added or removed between versions shifts every later id: bindings
	// from 2.1.12 packaged against 2.0.77's table call the WRONG MEMBER, silently
	// wherever the kinds line up, in a lockstep game. This read the default
	// unconditionally while gen-bindings and lock honoured the pin, so a pinned
	// project was mispaired by construction -- the same "two commands disagreeing
	// about one manifest key" class the gc key's history records, one key over.
	// Found by the 2026-08-04 audit.
	apiVersion := factorio.DefaultAPIVersion
	apiFromFlag := false
	// WHERE THE PIN CAME FROM, carried for the refusal in checkAPIPin and for
	// nothing else -- the same reason checkGC carries `gcFrom`. A reader whose
	// guest and package disagree about the version has to know which of three
	// things chose the one they did not expect, and by the time the mismatch is
	// provable the three have collapsed into one number.
	apiPin := pinSource{what: "fklua's default pin"}
	// WHICH ENGINE SERIES info.json DECLARES, and it is the OTHER axis: the pin
	// above says which description the tables came from, this says which
	// Factorio the mod claims to run on. They default together -- the series is
	// majorMinor of the pin -- because a mod built against a description usually
	// runs on that description's series, and they come apart the moment the
	// default pin is GA and the machine in front of you is not. A 2.1 engine
	// refuses a mod declaring "2.0" outright, at game start.
	//
	// TRACKED AS A SEPARATE BOOLEAN FOR THE SAME REASON --gc IS. The manifest is
	// the default and the flag is the override, which needs "the flag was typed"
	// to be a different question from "the value equals the default" -- and this
	// one has a non-empty default, so the value cannot carry that by itself. It
	// used to be inferred from `info.FactorioVersion == DefaultFactorioVersion`,
	// which made `--factorio-version 2.0` against a manifest saying 2.1 lose to
	// the manifest: the flag was silently discarded for naming the default.
	fvFromFlag := false
	// THE DEPENDENCY LIST, AND WHETHER THE COMMAND LINE SAID ANYTHING ABOUT IT.
	// Same shape as --gc and --factorio-version and for the same reason: an
	// empty list is a thing an author can mean, so "the flag was given" cannot
	// be recovered from the value. See the override below.
	var deps []string
	depsFromFlag := false
	// THE DATA STAGE. A second wasm module, compiled from its own main package,
	// that runs at Factorio's settings and data stages -- see
	// internal/factorio/stage.go. It travels the way every other manifest-backed
	// setting here does: the file is the default, the flag is the override, and
	// "was the flag typed" is a boolean rather than a comparison against a
	// default, because the empty string is a meaningful value ("no data guest").
	dataModule := ""
	dataFromFlag := false
	// The [stages] chains. Nil until something declares one, because an ABSENT
	// key and a key declared as an empty list mean different things and a map
	// that invented entries would erase the difference.
	var stages map[string][]string
	var scenarios map[string][]string

	str := func(i *int, flag string, dst *string) error {
		if *i+1 >= len(args) {
			return fmt.Errorf("%s needs a value", flag)
		}
		*i++
		*dst = args[*i]
		return nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch {
		case args[i] == "-o":
			err = str(&i, "-o", &outDir)
		case args[i] == "--name":
			err = str(&i, "--name", &info.Name)
		case args[i] == "--version":
			err = str(&i, "--version", &info.Version)
		case args[i] == "--title":
			err = str(&i, "--title", &info.Title)
		case args[i] == "--author":
			err = str(&i, "--author", &info.Author)
		case args[i] == "--description":
			err = str(&i, "--description", &info.Description)
		case args[i] == "--factorio-version":
			err = str(&i, "--factorio-version", &info.FactorioVersion)
			fvFromFlag = err == nil
		case args[i] == "--dependency":
			var d string
			if err = str(&i, "--dependency", &d); err == nil {
				deps = append(deps, d)
				depsFromFlag = true
			}
		case args[i] == "--zip":
			zip = true
		case args[i] == "--include":
			var dir string
			if err = str(&i, "--include", &dir); err == nil {
				include = append(include, dir)
			}
		case args[i] == "--data-module":
			err = str(&i, "--data-module", &dataModule)
			dataFromFlag = err == nil
		case args[i] == "--stage":
			// KEY=a,@guest,b. A flag form for a manifest key, and the reason is
			// the multi-project one rather than symmetry: one checkout that
			// packages several mods drives them from one Makefile with one
			// manifest describing the shipped one and flags describing the rest,
			// which is what a key with no flag would make impossible.
			var spec string
			if err = str(&i, "--stage", &spec); err == nil {
				var key string
				var chain []string
				key, chain, err = parseStageFlag(spec)
				if err == nil {
					if stages == nil {
						stages = map[string][]string{}
					}
					stages[key] = chain
				}
			}
		case strings.HasPrefix(args[i], "--nan="):
			nan, err = luagen.ParseNaNMode(strings.TrimPrefix(args[i], "--nan="))
		case strings.HasPrefix(args[i], "--opt="):
			opt, err = analysis.ParseLevel(strings.TrimPrefix(args[i], "--opt="))
		case strings.HasPrefix(args[i], "--persist="):
			persist, err = luagen.ParsePersistMode(strings.TrimPrefix(args[i], "--persist="))
			persistFromFlag = err == nil
		case strings.HasPrefix(args[i], "--gc="):
			gc, err = luagen.ParseGCMode(strings.TrimPrefix(args[i], "--gc="))
			gcFromFlag = err == nil
		case strings.HasPrefix(args[i], "--api="):
			apiVersion = strings.TrimPrefix(args[i], "--api=")
			apiFromFlag = true
			apiPin = pinSource{what: "the --api flag on this command line"}
			if apiVersion == "" {
				err = fmt.Errorf("--api needs a version, such as --api=%s",
					factorio.DefaultAPIVersion)
			}
		case strings.HasPrefix(args[i], "--fuel="):
			fuel, err = strconv.Atoi(strings.TrimPrefix(args[i], "--fuel="))
			if err == nil && fuel < 0 {
				err = fmt.Errorf("--fuel cannot be negative")
			}
			fuelFromFlag = err == nil
		case strings.HasPrefix(args[i], "-"):
			err = fmt.Errorf("unknown flag %q", args[i])
		default:
			if in != "" {
				return fmt.Errorf("expected one input module, got %q and %q", in, args[i])
			}
			in = args[i]
		}
		if err != nil {
			return err
		}
	}
	// THE INPUT MODULE IS CHECKED BELOW THE MANIFEST, and it used to be checked
	// here. Whether a control module is required at all depends on whether this
	// mod has a DATA module, and `data_module` is a manifest key as well as a
	// flag -- so the question cannot be asked until fklua.toml has been read.
	// The only invocations whose diagnostic moves are ones that were already
	// wrong twice over: a malformed fklua.toml, or a contradictory --dependency
	// list, now reports itself before "no input module" does.

	// --dependency REPLACES [mod] dependencies, it does not add to them.
	//
	// Every other identity flag overrides its manifest key, and a list is no
	// different -- but a list has one shape a scalar does not, and it is the one
	// that decides the semantics: a mod whose load-bearing property is that it
	// depends on NOTHING. Factorio sorts mods by their dependency graph, so an
	// observer that must run before the mod it observes has to declare no
	// dependency on it, and an APPENDING flag could never say that however many
	// values it took. Replacement expresses both directions; appending expresses
	// one. So a repo with one manifest and several packagings drives the whole
	// list from the command line, and the manifest goes on describing the mod it
	// is the manifest OF.
	//
	// The empty list is spelled `--dependency ""` rather than a second flag,
	// because two flags disagreeing about one manifest key is this repo's most
	// repeated failure shape and there is no reason to open another instance of
	// it. An empty string is not a dependency Factorio's grammar can express, so
	// nothing legal is displaced -- and mixing it with a real value is a
	// contradiction rather than a list, so it is refused rather than resolved.
	if depsFromFlag {
		for _, d := range deps {
			if d != "" {
				continue
			}
			if len(deps) > 1 {
				return fmt.Errorf(`--dependency "" says the list is empty and ` +
					`cannot be combined with another --dependency`)
			}
			deps = nil
		}
		info.Dependencies = deps
	}

	// THE MANIFEST IS THE DEFAULT AND THE FLAG IS THE OVERRIDE, the same rule
	// gen-bindings follows. `init` writes the identity into fklua.toml and this
	// command used to take every field as a flag and never read the file, so
	// the two disagreed by construction -- a downstream Makefile sed'd the toml
	// back into flags to keep them in step. Each field is filled only if the
	// command line left it empty, so a flag never has to fight the file.
	proj, hasProject, err := loadProject()
	if err != nil {
		return err
	}
	if hasProject {
		fill := func(dst *string, from string) {
			if *dst == "" {
				*dst = from
			}
		}
		fill(&info.Name, proj.Name)
		fill(&info.Version, proj.Version)
		fill(&info.Title, proj.Title)
		fill(&info.Author, proj.Author)
		fill(&info.Description, proj.Description)
		// The engine series travels like `gc` and `api`: manifest is the
		// default, flag is the override, and "the flag was typed" is a boolean
		// rather than a comparison against the default -- see fvFromFlag.
		if proj.FactorioVersion != "" && !fvFromFlag {
			info.FactorioVersion = proj.FactorioVersion
		}
		// Verbatim, in Factorio's own syntax. Not parsed: the game is the
		// authority on its own grammar, and a half-understanding of it would
		// reject strings the game accepts.
		//
		// `--dependency` travels like `gc`, `api` and the engine series: the
		// manifest is the default and the flag is the override, and "the flag
		// was given" is a boolean because the flag's value can legitimately be
		// the empty list. See the override above.
		if !depsFromFlag {
			info.Dependencies = proj.Dependencies
		}
		if proj.Data != "" {
			include = append(include, proj.Data)
		}
		// `gc` travels the same way, and the same way round: the manifest is the
		// default and the flag is the override.
		//
		// AN ABSENT KEY LEAVES THE LEAKING DEFAULT ALONE, which is the whole of
		// the backward compatibility. Every fklua.toml written before this key
		// existed has no `gc` line, so `fklua mod` over it emits exactly what it
		// emitted before -- the flag default is deliberately unchanged, because
		// --gc=collected is REFUSED for a module that exports no collector and
		// flipping it would turn every existing `tinygo -gc=leaking` build into
		// a compile error naming a flag its author never chose.
		if proj.GC != "" && !gcFromFlag {
			m, err := luagen.ParseGCMode(proj.GC)
			if err != nil {
				// Named as a manifest problem, not a flag problem. The parser's
				// own message says "--gc", and sending someone to a command line
				// they did not type is worse than saying nothing.
				return fmt.Errorf("%s: [fklua] gc: %w", projectFile, err)
			}
			gc, gcFrom = m, projectFile
		}
		// And `api` travels the same way, for the reason at its declaration: the
		// pin exists so the guest's bindings and the packaged member table come
		// from ONE version, and gen-bindings and lock have always read it. A
		// manifest cannot omit it -- ParseProject requires the key -- so the
		// no-manifest path is the only one that keeps DefaultAPIVersion, which is
		// exactly the path every in-repo build and every pre-manifest project
		// takes.
		if proj.API != "" && !apiFromFlag {
			apiVersion = proj.API
			apiPin = pinSource{what: projectFile + ", [fklua] api", file: projectFile}
		}
		// And so does the data module, and so does every [stages] key the flags
		// did not name -- per key rather than wholesale, so `--stage data=...`
		// overrides one chain without silently discarding the other three.
		if proj.DataModule != "" && !dataFromFlag {
			dataModule = proj.DataModule
		}
		for key, chain := range proj.Stages {
			if stages == nil {
				stages = map[string][]string{}
			}
			if _, typed := stages[key]; !typed {
				stages[key] = chain
			}
		}
		// [scenarios] TRAVELS THE SAME WAY AND HAS NO FLAG, which is [stages]'
		// own arrangement rather than an oversight. A scenario is a directory of
		// authored assets that the manifest names alongside them; there is no
		// case where one checkout packages several mods with different scenario
		// sets out of one tree, which is what earned --dependency its flag.
		scenarios = proj.Scenarios
	}

	// A DATA-STAGE-ONLY MOD HAS NO CONTROL MODULE, and Factorio has never
	// required one: info.json is the only file it insists on, and a mod that is
	// nothing but prototypes -- a compatibility shim, a stand-in, a mod whose
	// whole job is data.raw -- is an ordinary genre rather than a degenerate
	// case. Until this, `fklua mod` demanded a control guest whatever the mod
	// was, so the shape had to be reached by compiling an empty one: a hundred
	// kilobytes of Lua that is required at every load and called from nowhere.
	//
	// WITH NEITHER MODULE THE MESSAGE IS WHAT IT ALWAYS WAS. The command takes a
	// module, and being handed none of either kind is the same mistake it was
	// before a data stage existed.
	if in == "" && dataModule == "" {
		return fmt.Errorf("no input module")
	}
	// ...AND THE FLAGS THAT DESCRIBE A CONTROL GUEST ARE REFUSED RATHER THAN
	// IGNORED. A data module is compiled --persist=none and -gc=leaking whatever
	// else is asked for -- it runs once and dies with the Lua state that built
	// it, so there is nothing to save and no tick to pace a collector from --
	// and --fuel guards a loop in a program that is not here. A flag whose value
	// is silently discarded is this repo's most repeated failure shape, and the
	// refusal is the same one `checkGC` makes for the same reason.
	//
	// THE FLAG, NOT THE MANIFEST KEY, and the distinction is the one gcFromFlag
	// already exists to draw. `gc = "collected"` in fklua.toml is a statement
	// about the mod that manifest is the manifest OF; one checkout packaging
	// several mods drives the rest from flags, and refusing a data-only
	// packaging because the shipped mod's key is set would make that impossible.
	// A default that cannot apply is not a contradiction; a typed flag is.
	if in == "" {
		var typed []string
		if gcFromFlag {
			typed = append(typed, "--gc")
		}
		if persistFromFlag {
			typed = append(typed, "--persist")
		}
		if fuelFromFlag {
			typed = append(typed, "--fuel")
		}
		if len(typed) > 0 {
			verb, noun := "describe", "the flags"
			if len(typed) == 1 {
				verb, noun = "describes", "the flag"
			}
			return fmt.Errorf("%s %s how a CONTROL guest is compiled and this "+
				"packaging has no control module. A data module is always "+
				"--persist=none and -gc=leaking: it runs once and dies with the Lua "+
				"state that built it. Drop %s, or pass a control module too",
				andList(typed), verb, noun)
		}
	}
	if info.Name == "" {
		return fmt.Errorf("--name is required (or [mod] name in %s); Factorio "+
			"identifies a mod by it", projectFile)
	}
	if info.Version == "" {
		return fmt.Errorf("--version is required, as three numbers such as 0.1.0 "+
			"(or [mod] version in %s)", projectFile)
	}
	// Defaults that keep info.json valid without making the author type them.
	if info.Title == "" {
		info.Title = info.Name
	}
	if info.Author == "" {
		info.Author = "unknown"
	}
	if outDir == "" {
		outDir = "."
	}

	// THE CONTROL GUEST, and every step of it is gated on there being one. An
	// empty `in` is a data-stage-only mod (see the refusal above), and what
	// follows is the whole of what a control module costs: a build stamp, the
	// persist decision, the collector check, the emit, the exported hook list
	// and the pruned member table. Not one of them has anything to say about a
	// mod that is declarative from end to end -- and attachAPI in particular
	// must not be reached, because it is what would demand an fk_api_pin export
	// from a module that by design carries none.
	var im *ir.Module
	var src string
	pkg := &factorio.Package{Info: info, Stages: stages, Scenarios: scenarios}
	if in != "" {
		// BELOW the pin resolution above, and it has to be: the build stamp is a
		// fact about the module AND the version this package is built against, so
		// a stamp taken before the manifest was read would identify the build by a
		// pin it is not being packaged with. See buildID.
		var id string
		im, id, err = loadModuleID(in, apiVersion)
		if err != nil {
			return err
		}
		// Resolve --persist=auto HERE rather than inside the emitter, so the
		// choice can be printed. An automatic decision the author cannot see is
		// one they cannot correct, and this one turns on a proxy (heap size) for
		// something the compiler cannot know (write locality).
		if persist == luagen.PersistAuto {
			heap := heapBytes(im)
			persist = luagen.ResolvePersist(persist, heap)
			fmt.Printf("--persist=auto chose %s for a %d KiB heap (threshold %d KiB)\n",
				persist, heap/1024, luagen.AutoThresholdBytes/1024)
		}
		// A mod's control.lua wires only the hooks in factorio.Hooks, so those are
		// the only entry points its guest code can be reached through. Without
		// this, diagnostics name TinyGo's exported libm -- fmaximumf and friends
		// -- which the mod never calls and the author never wrote.
		if err := checkGC(gc, im, gcFrom); err != nil {
			return err
		}
		src, err = emitWithDiagnostics(im, luagen.Options{NaN: nan, Opt: opt, Persist: persist, BuildID: id,
			GC: gc, Fuel: fuel, Roots: hookNames()})
		if err != nil {
			return err
		}
		pkg.Chunk = src
		for _, e := range im.Exports {
			pkg.Exports = append(pkg.Exports, e.Name)
		}
		if err := attachAPI(pkg, im, apiVersion, apiPin); err != nil {
			return err
		}
	}
	// THE DATA STAGE. A second module through the same pipeline, and the flags
	// it is NOT given are as deliberate as the ones it is.
	//
	// No --persist: it runs once and dies with the Lua state that built it, so
	// there is nothing to save. No --gc: same reason, and a collector's pacing
	// surface is driven from a tick this stage does not have. No --api and no
	// member table: it calls no runtime API, which is checked rather than
	// assumed. What it DOES share is the NaN mode and the -opt level, because
	// those are properties of the emitter rather than of the mod's lifecycle,
	// and two modules in one mod compiled at two levels would be a confusing
	// thing to debug.
	var dataModuleSize int
	if dataModule != "" {
		dim, err := loadModule(dataModule)
		if err != nil {
			return err
		}
		var imports, exports []string
		for _, im := range dim.Source.Imports {
			imports = append(imports, im.Module+"."+im.Name)
		}
		for _, e := range dim.Exports {
			exports = append(exports, e.Name)
		}
		if err := factorio.CheckDataModule(imports, exports); err != nil {
			return fmt.Errorf("%s: %w", dataModule, err)
		}
		dsrc, err := emitWithDiagnostics(dim, luagen.Options{
			NaN: nan, Opt: opt, Persist: luagen.PersistNone, GC: luagen.GCLeaking,
			Roots: factorio.StageExportNames(),
		})
		if err != nil {
			return err
		}
		pkg.DataChunk = dsrc
		pkg.DataExports = exports
		dataModuleSize = len(dsrc)
	}
	// The data stage. Merged into Files() BEFORE either writer runs, so a
	// directory and a zip carry the same bytes -- copying files over the output
	// afterwards is what --zip could never do.
	for _, dir := range include {
		if err := pkg.Include(dir); err != nil {
			return err
		}
	}

	var path string
	if zip {
		path, err = pkg.WriteZip(outDir)
	} else {
		path, err = pkg.WriteDir(outDir)
	}
	if err != nil {
		return err
	}

	// TWO LINES BECAUSE THERE ARE TWO SHAPES, and the control one is untouched
	// to the byte -- it is what every build in and outside this repo greps. The
	// data-only line names neither --persist nor --gc, because a line reporting
	// a mode nothing was compiled in is how a reader comes to believe a flag did
	// something; the two are refused above rather than reported here.
	if in == "" {
		fmt.Printf("wrote %s (data stage only, no control module; NaN mode: %s, -opt=%s)\n",
			path, nan, opt)
	} else {
		fmt.Printf("wrote %s (%d bytes of Lua, NaN mode: %s, -opt=%s, --persist=%s, --gc=%s)\n",
			path, len(src), nan, opt, persist, gc)
	}
	// SAY WHICH ENGINE THE MOD CLAIMS, beside the `API <version>:` lines that
	// say which description it was built from. The two are separate axes and
	// they no longer default to the same series in every project, so the one
	// place both are visible at once is here -- and the failure they guard
	// against is a refusal at GAME START ("Incompatible Factorio version"),
	// which is the worst place to learn about a two-character string.
	fvSrc := "the " + apiVersion + " pin's series"
	switch {
	case fvFromFlag:
		fvSrc = "--factorio-version"
	case hasProject && proj.FactorioVersion != "":
		fvSrc = projectFile
	}
	fmt.Printf("  info.json declares Factorio %s (from %s)\n", info.FactorioVersion, fvSrc)
	if n := len(pkg.Extra); n > 0 {
		fmt.Printf("  included %d file(s) from %s\n", n, strings.Join(include, ", "))
	}
	if dataModule != "" {
		fmt.Printf("  data module %s (%d bytes of Lua)\n", dataModule, dataModuleSize)
	}
	// HAND-WRITTEN LUA, NAMED WHERE IT IS CARRIED PAST THE COMPILER. A mod's own
	// state is the guest's heap and the heap is migrated by fk_migrate;
	// migrations/*.lua is not FkLua's state-migration mechanism and will not
	// become one. What it keeps is the status of inline assembly -- permitted,
	// marked, minimised, never generated -- and this line is the mark, so a
	// repository can grep its own build output for the count instead of
	// remembering to look. JSON migrations are a prototype-rename TABLE rather
	// than a program and are deliberately not counted: there is nothing there a
	// compiler could have replaced.
	if migs := pkg.LuaMigrations(); len(migs) > 0 {
		fmt.Printf("  %d hand-written Lua migration(s): %s\n", len(migs),
			strings.Join(migs, ", "))
	}

	// Say what was actually connected. A guest that misspells fk_on_tick
	// otherwise gets a mod that loads, does nothing, and explains nothing.
	found, absent := pkg.Wiring()
	for _, h := range found {
		fmt.Printf("  wired %-12s -> %s\n", h.Export, h.What)
	}
	// The same for the data stage, and a mod with a data module that exports
	// nothing gets told so: it would otherwise ship a module that is parsed at
	// no stage and called from nowhere.
	if dataModule != "" {
		dfound, dabsent := pkg.DataWiring()
		for _, h := range dfound {
			fmt.Printf("  wired %-20s -> %s\n", h.Export, h.File)
		}
		if len(dfound) == 0 {
			fmt.Println("\nThe data module exports no stage hook, so no stage file was")
			fmt.Println("generated and it will never run. Export one of:")
			for _, h := range dabsent {
				fmt.Printf("  %-20s %s\n", h.Export, h.What)
			}
		}
	}
	// A DATA-ONLY MOD IS NOT INERT, it is declarative, and telling its author to
	// export an event hook would be advice to write the control guest they
	// deliberately did not write. `Inert()` reads the control exports and a
	// package with no control module has none, so the warning has to be gated on
	// the shape rather than on the answer. What stands in for it is the data
	// wiring block above: a data module exporting no stage hook IS the mod that
	// loads and does nothing, and that is what it says.
	if in != "" && pkg.Inert() {
		fmt.Println("\nThis guest exports no event hook, so the mod will load and then never")
		fmt.Println("be called again. Export one of:")
		for _, h := range absent {
			if h.Event {
				fmt.Printf("  %-12s %s\n", h.Export, h.What)
			}
		}
	}
	return nil
}

// andList writes a list the way a sentence does: "a", "a and b", "a, b and c".
// `strings.Join(x, " and ")` reads as a chant past two, and this message can
// carry three.
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// parseStageFlag reads `--stage KEY=a,@guest,b`.
//
// The key is checked against the four stages here rather than left to the
// packager, so a typo is one message naming the four rather than a stage file
// that is silently not generated.
func parseStageFlag(spec string) (string, []string, error) {
	key, list, ok := strings.Cut(spec, "=")
	if !ok {
		return "", nil, fmt.Errorf("--stage takes KEY=a,%s,b -- the stages are %s",
			factorio.GuestStageEntry, strings.Join(factorio.StageKeys(), ", "))
	}
	key = strings.TrimSpace(key)
	if _, known := factorio.StageHookByKey(key); !known {
		return "", nil, fmt.Errorf("--stage %s: unknown stage; the stages are %s",
			key, strings.Join(factorio.StageKeys(), ", "))
	}
	// An empty list is legal and means a stage file with no requires, which is
	// different from not declaring the key at all -- so the slice is non-nil
	// even when there is nothing in it.
	chain := []string{}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			chain = append(chain, part)
		}
	}
	return key, chain, nil
}

// hookNames is the set of exports a packaged mod actually wires, which is what
// makes it the right reachability root set for a mod's diagnostics.
func hookNames() []string {
	names := make([]string, 0, len(factorio.Hooks))
	for _, h := range factorio.Hooks {
		names = append(names, h.Export)
	}
	return names
}

// apiDir is the directory holding the committed API descriptions.
func apiDir() string {
	if p := os.Getenv("FKLUA_API_DIR"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(exe))
		if p := filepath.Join(root, "api"); fileExists(filepath.Join(p,
			factorio.DefaultAPIVersion, "runtime-api.json")) {
			return p
		}
	}
	return "api"
}

// apiPath locates a committed runtime-api.json.
//
// Relative to the executable's module root rather than the working directory:
// `fklua mod` is run from wherever the guest is, and the API description is
// part of the compiler, not of the guest.
func apiPath(version string) string {
	if p := os.Getenv("FKLUA_API_DIR"); p != "" {
		return filepath.Join(p, version, "runtime-api.json")
	}
	if exe, err := os.Executable(); err == nil {
		// bin/fklua -> the repo root beside it.
		root := filepath.Dir(filepath.Dir(exe))
		if p := filepath.Join(root, "api", version, "runtime-api.json"); fileExists(p) {
			return p
		}
	}
	return filepath.Join("api", version, "runtime-api.json")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// GoBindingsPath is where the generated guest package is committed.
//
// Inside guest/go, which is its own Go module: //go:wasmimport is rejected
// outside GOARCH=wasm, so keeping it there is what stops it breaking
// `go build ./...` for the compiler itself.
const GoBindingsPath = "guest/go/fkapi/fkapi.go"

// RustBindingsPath is the Rust equivalent. Committed for the same reason: a
// build needs neither the game nor the network.
const RustBindingsPath = "guest/rust/fkapi/src/api.rs"

// RustCratePath and RustCrateLibPath are the other two files of the generated
// crate, and they exist because api.rs on its own IS NOT A CRATE.
//
// This command hard-codes the path above -- `fklua lock` hashes it by that exact
// name -- and wrote nothing else, so a project that ran `fklua init --lang rust`
// and then the `fklua gen-bindings` init told it to run ended up with 2 MB of
// generated Rust that nothing could compile: no manifest, no lib.rs declaring
// the module, and a scaffolded guest in a different directory that did not
// depend on it. Making it build meant copying two files out of a FkLua checkout
// by hand, which init's own next-steps did not mention (fklua-ports-samples,
// AD9; its converged layout is what this now scaffolds).
//
// They are generated rather than scaffolded by `init` because they belong to
// the bindings: a project regenerating against a new API pin gets all three
// together, and a project that never ran init still gets a crate.
const (
	RustCratePath    = "guest/rust/fkapi/Cargo.toml"
	RustCrateLibPath = "guest/rust/fkapi/src/lib.rs"
)

// rustCrateCargo and rustCrateLib are the two static files above. They are the
// bytes this repo's own guest/rust/fkapi carries, so `--check` in a FkLua
// checkout compares them against themselves.
const rustCrateCargo = `[package]
name = "fkapi"
version = "0.1.0"
edition = "2021"
description = "Generated Factorio API bindings. Regenerate with ` + "`fklua gen-bindings`" + `."

[lib]
crate-type = ["rlib"]
`

const rustCrateLib = `//! Generated Factorio API bindings for Rust.
//!
//! ` + "`api.rs`" + ` is generated by ` + "`fklua gen-bindings`" + ` and committed, so a build
//! needs neither the game nor the network. Do not edit it: ` + "`--check`" + ` fails if
//! it drifts from ` + "`api/<version>/runtime-api.json`" + `.
#![no_std]

extern crate alloc;

mod api;
pub use api::*;
`

// runGenBindings writes -- or checks -- the generated guest bindings.
//
// --check is the CI gate. The bindings are committed as golden files so any
// regeneration produces a reviewable diff, and a stale checkout is a build
// failure rather than something a guest author discovers as a missing method.
func runGenBindings(args []string) error {
	lang, out, into, check := "", "", "", false
	for i := 0; i < len(args); i++ {
		switch {
		case isLangArg(args[i]):
			v, next, err := langArg(args, i)
			if err != nil {
				return err
			}
			lang, i = v, next
		case args[i] == "--check":
			check = true
		case args[i] == "-o":
			if i+1 >= len(args) {
				return fmt.Errorf("-o needs a path")
			}
			i++
			out = args[i]
		case args[i] == "--into":
			if i+1 >= len(args) {
				return fmt.Errorf("--into needs a directory")
			}
			i++
			into = args[i]
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
	}

	// THE MANIFEST IS THE DEFAULT AND THE FLAG IS THE OVERRIDE.
	//
	// `lang` and `api` are fklua.toml keys, and `fklua lock` has always read
	// them; this command did not, so `fklua init --lang go` printed "Next:
	// `fklua gen-bindings`" and following that advice dropped an unwanted
	// guest/rust/ into a Go-only project -- which `lock` then refused to hash.
	// Two commands disagreeing about one key is worse than either behaviour.
	version := factorio.DefaultAPIVersion
	proj, hasProject, err := loadProject()
	if err != nil {
		return err
	}
	if hasProject {
		version = proj.API
		if lang == "" {
			lang = strings.Join(proj.Langs, ",")
		}
	}
	if lang == "" {
		lang = "all"
	}
	langs, err := parseLangs(lang)
	if err != nil {
		return err
	}
	// ONE PATH CANNOT HOLD TWO LANGUAGES. Every target took `dst = out`, so with
	// both languages selected the Go bindings were written to the path and the
	// Rust bindings straight over them: exit 0, "wrote <path>" printed twice, and
	// Rust source left in a .go-named file. It is not an exotic invocation -- a
	// bare `fklua gen-bindings -o /tmp/api.go` outside a project defaults to
	// `all` and lands here. Refusal only; a valid one-language `-o` is unchanged.
	if out != "" && len(langs) > 1 {
		return fmt.Errorf("-o names one file but --lang selects %d languages "+
			"(%s); run once per language with -o", len(langs), langList(langs))
	}
	// --into IS THE SUPPORTED WAY TO REPIN A VENDORED CHECKOUT, and it exists
	// because the alternative was a recipe nobody wrote down.
	//
	// The library packages inside guest/go and guest/rust -- fkipc above all --
	// import their OWN module's fkapi, which is committed here at
	// DefaultAPIVersion. A consumer that vendors this checkout and pins anything
	// else therefore has to regenerate that committed copy at its own pin or its
	// guest calls the wrong members; checkAPIPin now refuses the package that
	// results, and a refusal whose remedy is "hand-edit somebody else's tree"
	// would be a poor one. So the remedy is a flag: the pin and the language
	// list come from THIS project's manifest, and the files land in the checkout
	// named here.
	//
	// It is not -o with a directory. -o names ONE FILE for ONE language and
	// this names a checkout for every language the manifest declares, so
	// conflating them would give -o a second meaning that depends on --lang.
	if out != "" && into != "" {
		return fmt.Errorf("-o names one file and --into names a checkout to write "+
			"the committed bindings of; they cannot both say where output goes "+
			"(-o %s, --into %s)", out, into)
	}

	a, err := factorio.LoadAPI(apiPath(version))
	if err != nil {
		return fmt.Errorf("API %s: %w (run `fklua api pull %s`)", version, err, version)
	}
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)

	// Every language this tree declares, every time. The census taught this:
	// two artifacts generated by separate commands is how one of them goes
	// stale, and the whole point of `--check` is that it cannot.
	type target struct {
		lang, path, src string
		emitted         int
		summary         string
	}
	var targets []target

	// The three ways a target file's path can be chosen, in one place so that
	// "which of -o, --into and the committed path wins" is answered once rather
	// than once per language.
	dstOf := func(committed string) string {
		switch {
		case out != "":
			return out
		case into != "":
			return filepath.Join(into, committed)
		}
		return committed
	}
	// What to tell a reader to re-run. `--check` failing under --into and
	// telling them to run the bare command would send them to regenerate the
	// WRONG TREE -- this project's own bindings are not what just failed.
	rerun := "fklua gen-bindings"
	if into != "" {
		rerun += " --into " + into
	}

	if langs["go"] {
		g, err := factorio.GenerateGoWith(a, report, events, "fkapi")
		if err != nil {
			return err
		}
		dst := dstOf(GoBindingsPath)
		targets = append(targets, target{"go", dst, g.Source, g.Emitted,
			fmt.Sprintf("%s, %d Into variants", deferSummary(g), g.IntoVariants)})
	}
	if langs["rust"] {
		r, err := factorio.GenerateRust(a, report, events)
		if err != nil {
			return err
		}
		dst := dstOf(RustBindingsPath)
		targets = append(targets, target{"rust", dst, r.Source, r.Emitted,
			fmt.Sprintf("%d deferred, %d Into variants", r.Deferred, r.IntoVariants)})
		// ...AND THE REST OF THE CRATE. Only when the bindings are going to
		// their own committed path: `-o` means "write the source somewhere I
		// named", and dropping a Cargo.toml next to it would be writing files
		// nobody asked for. See RustCratePath.
		//
		// --into is excluded for the same reason and a sharper one: those two
		// files are STATIC, so they say nothing about the pin, and the checkout
		// being repinned already has its own. Writing them would turn a repin
		// into a partial overwrite of somebody's vendored snapshot, and under
		// `--check` it would fail a consumer whose FkLua is simply a different
		// version from the one running -- which is not what this checks.
		if out == "" && into == "" {
			targets = append(targets,
				target{"rust", RustCratePath, rustCrateCargo, r.Emitted, "the crate manifest"},
				target{"rust", RustCrateLibPath, rustCrateLib, r.Emitted, "the crate root"})
		}
	}

	for _, t := range targets {
		if check {
			have, err := os.ReadFile(t.path)
			if err != nil {
				return fmt.Errorf("%s: %w (run `%s`)", t.path, err, rerun)
			}
			if string(have) != t.src {
				return fmt.Errorf("%s is out of date; run `%s`", t.path, rerun)
			}
			if t.path == RustCratePath || t.path == RustCrateLibPath {
				fmt.Printf("%s is up to date (%s)\n", t.path, t.summary)
			} else {
				fmt.Printf("%s is up to date (%d members bound, %s)\n",
					t.path, t.emitted, t.summary)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(t.path, []byte(t.src), 0o644); err != nil {
			return err
		}
		if t.path == RustCratePath || t.path == RustCrateLibPath {
			fmt.Printf("wrote %s: %s\n", t.path, t.summary)
			continue
		}
		fmt.Printf("wrote %s: %d members bound, %s, %d bytes\n",
			t.path, t.emitted, t.summary, len(t.src))
	}
	// AND THE FULL DEFERRAL REPORT, every group, plus the accounting line that
	// says where every shape the description models ended up. See
	// printDeferrals: the headline above names one group, and the fourteen it
	// does not name are where two of the campaign's findings were hiding.
	gb, err := factorio.GenerateGoWith(a, report, events, "fkapi")
	if err != nil {
		return err
	}
	rbind, err := factorio.GenerateRust(a, report, events)
	if err != nil {
		return err
	}
	printDeferrals(a, report, gb, rbind)
	// NO CENSUS UNDER --into. The census is a fact about the checkout that owns
	// `api/<version>/`, and --into writes into a checkout that is being repinned
	// FROM somewhere else -- writing one there would file this project's numbers
	// under somebody else's tree, which is the same "building a mod must never
	// write into the compiler" rule censusPass already enforces one level up.
	if into != "" {
		return nil
	}
	return censusPass(version, check)
}

// langArg reads --lang in BOTH spellings and reports the index to continue from.
//
// ONE FLAG, THREE COMMANDS, AND IT PARSED TWO DIFFERENT WAYS. `init` and `docs`
// took `--lang go` and answered "unknown argument" to `--lang=go`; this command
// took `--lang=all` and answered "unknown argument" to `--lang all` -- which is
// the form its own usage line prints. Nobody reads three argument loops before
// typing a flag they have already used elsewhere, so whichever spelling a user
// learned first was wrong somewhere, and the refusal named the argument rather
// than the spelling.
//
// BOTH, rather than picking one, because both are already in this repo's own
// tests (`init_test.go` writes `--lang go,rust`, `genbindings_test.go` writes
// `--lang=go`) and neither was ever documented as the wrong one.
//
// Scoped to --lang deliberately. The rest of the CLI has a split that is
// consistent BY FLAG and therefore not a trap: paths and names take a space
// (-o, --api, --guest-module, --name), compile modes take an equals (--opt=,
// --gc=, --persist=, --nan=, --fuel=), and each of those is spelled the same way
// in every command that has it. --lang was the only one that was not.
func langArg(args []string, i int) (string, int, error) {
	if v, ok := strings.CutPrefix(args[i], "--lang="); ok {
		return v, i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("--lang needs a value")
	}
	return args[i+1], i + 1, nil
}

// isLangArg reports whether args[i] is --lang in either spelling.
func isLangArg(arg string) bool {
	return arg == "--lang" || strings.HasPrefix(arg, "--lang=")
}

// parseLangs turns a comma-separated list into a set, refusing anything with
// no generator rather than silently emitting less than asked for.
func parseLangs(list string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, l := range strings.Split(list, ",") {
		switch l = strings.TrimSpace(l); l {
		case "all":
			out["go"], out["rust"] = true, true
		case "go", "rust":
			out[l] = true
		case "":
		default:
			return nil, fmt.Errorf("no generator for lang %q; use go, rust or all "+
				"(C is not generated yet)", l)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--lang names no language")
	}
	return out, nil
}

// langList names a language set in a stable order, for a diagnostic. A map's
// walk order is randomized and an error message that reads differently on
// consecutive runs is one nobody can grep for.
func langList(langs map[string]bool) string {
	var names []string
	for _, l := range []string{"go", "rust"} {
		if langs[l] {
			names = append(names, l)
		}
	}
	return strings.Join(names, ", ")
}

// censusPass rewrites -- or checks -- the committed census beside EVERY API
// description this working directory owns, whatever pin was invoked.
//
// It rides along with gen-bindings on purpose. The two are the same act: a
// version bump regenerates the bindings and moves the numbers, and splitting
// them into two commands is how one of them gets forgotten. That is also why
// there is no `fklua census` subcommand: a second command writing the same file
// is the split this file's own header argues against.
//
// IT IS WRITTEN ONLY WHERE ITS INPUT LIVES. The bindings resolve against the
// working directory and the census used to resolve against the EXECUTABLE, so
// `fklua gen-bindings` inside a mod project rewrote api/<version>/census.json in
// whichever FkLua checkout built the binary -- invisible for exactly as long as
// that census was current. One command, one root: a working directory with no
// api/ of its own owns no description, and the census is not its to write.
//
// AND EVERY DESCRIPTION THE ROOT OWNS, not only the invoked pin, which is the
// 2026-08-24 fix. A census is taken by whatever generation last ran against its
// description, and only the default pin's ever ran -- so the moment the
// generators grew a row, every OTHER committed description's census was stale,
// with no command anywhere that could repair it: regenerating from a mod project
// is refused by the paragraph above, and regenerating from the checkout wrote
// the default pin's file and nothing else. A downstream mod moving its pin onto
// one of those versions then failed `gen-bindings --check` against a file it
// could not write and upstream could not refresh (BetterBeltBalancer, gap 24;
// it hit this the day the index-assign member kind added `index_setter_members`,
// and had to revert the pin move).
//
// So ownership is per DESCRIPTION rather than per invocation. The checkout owns
// three descriptions and leaves all three current; a mod project owns none and
// leaves all of them alone. Staleness is then not a thing to remember, which is
// the only form of "remember to re-run it" this repo has ever made stick.
//
// The cost is one full generation per extra description on a command that
// already does two, and it is paid where the descriptions are: about 0.3 s per
// version in this checkout, nothing at all in a mod project.
func censusPass(pin string, check bool) error {
	owned := ownedAPIVersions()
	if len(owned) == 0 {
		return censusNotOurs(pin, check)
	}

	var stale []string
	for _, v := range owned {
		path := factorio.CensusPath("api", v)
		a, err := factorio.LoadAPI(filepath.Join("api", v, "runtime-api.json"))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		c, err := factorio.TakeCensus(a)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		next, err := c.JSON()
		if err != nil {
			return err
		}
		old, loadErr := factorio.LoadCensus(path)

		if check {
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", path, loadErr)
				stale = append(stale, v)
				continue
			}
			lines := c.Diff(old)
			if len(lines) == 0 {
				fmt.Printf("%s is up to date\n", path)
				continue
			}
			for _, l := range lines {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", v, l)
			}
			stale = append(stale, v)
			continue
		}

		if loadErr == nil {
			if lines := c.Diff(old); len(lines) > 0 {
				fmt.Printf("census moved (%s):\n", v)
				for _, l := range lines {
					fmt.Printf("  %s\n", l)
				}
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, next, 0o644); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		// Every stale version in one error, because they go stale TOGETHER --
		// one generator row moves all of them -- and a message naming the first
		// would have a reader fix it and run again to meet the second.
		return fmt.Errorf("the census is out of date for %s; run `fklua gen-bindings` "+
			"here, which refreshes every census this checkout owns",
			strings.Join(stale, ", "))
	}
	return nil
}

// ownedAPIVersions lists the descriptions committed under THIS working
// directory, sorted, so that a pass over them is the same on every machine.
//
// A version directory with no runtime-api.json in it is not owned: `api pull`
// writes the description first, and half a pull is not a thing to take a census
// of.
func ownedAPIVersions() []string {
	ents, err := os.ReadDir("api")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && fileExists(filepath.Join("api", e.Name(), "runtime-api.json")) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// censusNotOurs is what a mod project gets: a notice, never a failure.
//
// A STALE CENSUS IN THE COMPILER IS NOT A DEFECT OF THE MOD, and until
// 2026-08-24 `--check` here failed on one. Nothing downstream reads a census --
// not `mod`, not `lock`, not either generator -- so it is an FkLua-internal
// consistency artifact and a mod's own gate has no business failing on it. The
// failure was also unfixable by construction: the write half of this function
// declines to write from a mod project, so no invocation in that directory could
// ever make the check pass (BetterBeltBalancer, gap 24).
//
// It is still SAID, at notice level and only when it is really stale, because
// this is the one place a downstream author learns that the toolchain they are
// building against is behind its own numbers -- which is how that gap got
// reported in the first place. It names the checkout and the command, so the
// report can be made without a round trip.
func censusNotOurs(pin string, check bool) error {
	path := factorio.CensusPath(apiDir(), pin)
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	root, _ := filepath.Abs(filepath.Dir(filepath.Dir(path)))

	a, err := factorio.LoadAPI(apiPath(pin))
	if err != nil {
		return nil // gen-bindings already loaded it; nothing to add here.
	}
	c, err := factorio.TakeCensus(a)
	if err != nil {
		return nil
	}
	old, loadErr := factorio.LoadCensus(path)
	if loadErr == nil && len(c.Diff(old)) == 0 {
		return nil
	}

	verb := "not writing"
	if check {
		verb = "not checking"
	}
	fmt.Printf("%s a census: this directory owns no API description, and building "+
		"a mod does not write into the compiler.\n", verb)
	fmt.Printf("  NOTICE: %s is behind the generator that just ran. Nothing here "+
		"reads it and your build is unaffected; it is the compiler's own "+
		"bookkeeping. Refresh it with `fklua gen-bindings` run in %s.\n", abs, root)
	return nil
}

// deferSummary says what is missing AND what would fix the most of it.
//
// It read "awaiting struct support" for two milestones after structs landed,
// which is the failure mode of writing the reason as a literal: the number
// stayed honest while the sentence next to it did not. This derives both.
func deferSummary(g factorio.GoBindings) string {
	if g.Deferred == 0 {
		return "nothing deferred"
	}
	top, n := "", 0
	for k, v := range g.DeferredBy {
		// Ties broken by name so the output does not depend on map order.
		if v > n || (v == n && k < top) {
			top, n = k, v
		}
	}
	// Not "because it <reason>": the reasons are a mix of verb phrases and noun
	// phrases, and only half of them read as a sentence after "it".
	return fmt.Sprintf("%d deferred, the largest group (%d) being: %s",
		g.Deferred, n, top)
}

// printDeferrals lists EVERY deferral group, not just the largest.
//
// The one-line summary above is a headline and was, for four milestones, the
// whole report -- so of 27 deferrals an author saw the 13 that were multi-return
// members and nothing at all about the other 14. Four of those were the `tags`
// setters that fklua-ports' fluid-memory-storage reported as F-TAGS, described
// in its findings as generating "silently": they were counted, and counted
// where nobody could read it is the same experience as not counted.
//
// The other lesson is F-IDX's and it is not fixed by printing more: class
// operators were absent from the deferral list entirely because no generator
// looked at them, so no amount of reporting on what the generator TRIED would
// have shown them. That is what census.json's operators_bound row is for, and
// what the accounting line below states -- every shape the API description
// models is either bound, deferred with a reason, or named here as a decision.
func printDeferrals(a *factorio.API, r factorio.Report,
	g factorio.GoBindings, rb factorio.RustBindings) {
	list := func(label string, total int, by map[string]int) {
		if total == 0 {
			fmt.Printf("  %s: none\n", label)
			return
		}
		keys := make([]string, 0, len(by))
		for k := range by {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("  %s: %d\n", label, total)
		for _, k := range keys {
			fmt.Printf("    %4d  %s\n", by[k], k)
		}
	}
	fmt.Println("deferrals, in full:")
	list("host member table", len(r.Skipped), r.Reasons)
	// OMISSIONS ARE NOT DEFERRALS and are printed apart from them. A deferral
	// costs a member; an omission is a field the description says carries no
	// value, dropped from a struct whose member still binds. Printed because
	// the alternative is that it appears nowhere at all: every gate is green
	// either way, and the whole reason it is safe to drop the field is a
	// sentence in the description that nobody re-reads.
	list("struct fields omitted", len(r.Omitted), r.OmittedBy)
	for _, o := range r.Omitted {
		fmt.Printf("      %s::%s (%s)\n", o.Owner, o.Field, o.Type)
	}
	list("go members", g.Deferred, g.DeferredBy)
	list("go event payloads", g.EventsDeferred, g.EventDeferBy)
	list("rust members", rb.Deferred, rb.DeferredBy)
	list("rust event payloads", rb.EventsDeferred, rb.EventDeferBy)
	// STRING-ENUM CONSTANTS ARE NOT MEMBERS and are printed apart from them,
	// which is the reporting half of the census's off-by-one. They were counted
	// in the member deferrals, so `go members: 19` was 18 members plus one
	// literal and the three census rows did not reconcile. What a nameless
	// literal costs is one spelling of a string the guest can still write out;
	// what a deferred member costs is a call.
	list("go string-enum constants", g.LiteralsDeferred, g.LiteralDeferBy)
	list("rust string-enum constants", rb.LiteralsDeferred, rb.LiteralDeferBy)

	// NAME COLLISIONS BY IDENTITY, because a count cannot say whose decision it
	// is. A collision is one member losing a name to another and somebody has to
	// choose which -- `memberRename` in gen.go is where the two standing ones are
	// chosen -- so an unlisted one prints with its name and its would-be
	// spelling rather than sitting as a number in a census diff. Silent when
	// there are none, like every other line here.
	for _, b := range []struct {
		lang           string
		collide, stale []string
	}{
		{"go", g.Collisions, g.StaleRenames},
		{"rust", rb.Collisions, rb.StaleRenames},
	} {
		for _, c := range b.collide {
			fmt.Printf("  %s name collision with NO rename row: %s\n", b.lang, c)
		}
		for _, s := range b.stale {
			fmt.Printf("  %s STALE rename row: %s\n", b.lang, s)
		}
	}

	// THE ACCOUNTING LINE. Everything api.go models, reconciled against what
	// came out, so a shape that reaches neither list is visible as a number
	// rather than as an absence somebody has to notice.
	methods, attrs, reads, writes, ops := 0, 0, 0, 0, 0
	for _, c := range a.Classes {
		methods += len(c.Methods)
		attrs += len(c.Attributes)
		ops += len(c.Operators)
		for _, at := range c.Attributes {
			if at.ReadType != nil {
				reads++
			}
			if at.WriteType != nil {
				writes++
			}
		}
	}
	bound := map[int]int{}
	for _, m := range r.Members {
		bound[m.Kind]++
	}
	nOps := bound[factorio.MemberIndex] + bound[factorio.MemberLen] + bound[factorio.MemberSelf]
	fmt.Println("what the description models, and where each of it went:")
	fmt.Printf("  %4d methods -> %d call members\n", methods, bound[factorio.MemberCall])
	fmt.Printf("  %4d attributes (%d readable, %d writable) -> %d get, %d set, %d predicates\n",
		attrs, reads, writes, bound[factorio.MemberGet], bound[factorio.MemberSet],
		bound[factorio.MemberGetEq])
	fmt.Printf("  %4d class operators -> %d members\n", ops, nOps)
	// THE GLOBAL FUNCTIONS, which read "-> 0 members: they are not on a class
	// and fk.call takes a handle" for eight milestones. That was a decision and
	// it was written down here and in census.json, which is the only reason it
	// could be re-taken rather than rediscovered. The kind's branch in M.invoke
	// runs BEFORE the handle is resolved, so "fk.call takes a handle" stopped
	// being an obstacle the moment somebody asked for one of the three.
	fmt.Printf("  %4d global functions -> %d members, on no class: the binding is\n",
		len(a.GlobalFunctions), bound[factorio.MemberGlobalFunc])
	fmt.Println("       package-level and the handle operand is unread. See agents/abi.md.")
	// THE HANDLE VARIANTS, WHICH THIS LINE DID NOT MENTION. An attribute typed
	// LuaCustomTable gets a SECOND member returning the handle, so the four
	// lines above summed to 4784 against a member table of 4842 and the
	// difference was nowhere -- the same "a shape that reaches no line of the
	// report" this whole block was written to prevent, met from inside.
	fmt.Printf("  %4d of those readable attributes are a LuaCustomTable and get a\n",
		bound[factorio.MemberGetHandle])
	fmt.Println("       SECOND, handle-returning member beside the materialising one.")
	// ...AND THE METHOD HALF, which is the same gap and was the half kind 7 left
	// open: eleven members RETURN a LuaCustomTable and each materialised its
	// whole result per call. On its own line for the reason the one above has
	// one, and counted separately in the census for the same reason -- folding
	// the two would make the day this landed read as an attribute count moving.
	fmt.Printf("  %4d METHODS return a LuaCustomTable and get the same twin, which is\n",
		bound[factorio.MemberCallHandle])
	fmt.Println("       what makes an index lookup on the result reachable at all.")
	// THE INDEX OPERATORS' WRITE HALF, which is the one member count here the
	// description cannot be asked for: an operator carries a read_type and never
	// a write_type, so what `obj[k] = v` exists for is PROSE, read through an
	// allowlist. On its own line for the reason kind 7 has one -- a kind in no
	// bucket is a kind nobody adds up.
	fmt.Printf("  %4d of the index operators have a WRITE half (obj[k] = v), from the\n",
		bound[factorio.MemberIndexSet])
	fmt.Println("       allowlist in gen.go: the description declares no write side for one.")
	// And the sum, stated, because a decomposition nobody adds up is a list.
	fmt.Printf("  %4d members in the table, and %d + %d + %d + %d + %d + %d + %d + %d + %d is the same number.\n",
		len(r.Members), bound[factorio.MemberCall], bound[factorio.MemberGet],
		bound[factorio.MemberSet], bound[factorio.MemberGetEq], nOps,
		bound[factorio.MemberGetHandle], bound[factorio.MemberIndexSet],
		bound[factorio.MemberGlobalFunc], bound[factorio.MemberCallHandle])
	// The identity the census carries, printed where the numbers are. It closes
	// exactly, in both languages, and TestTheCensusMemberArithmeticCloses is
	// what keeps it closing -- it did NOT close until the string-enum constants
	// stopped being counted as members.
	fmt.Printf("  every one of them is bound or deferred: go %d + %d, rust %d + %d.\n",
		g.Emitted, g.Deferred, rb.Emitted, rb.Deferred)
}
