package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
)

// captureStdout runs f with os.Stdout redirected and returns what it printed.
// The outputs under test here are a line or two, so nothing can fill the pipe
// buffer and f is run inline rather than behind a goroutine.
func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := f()
	w.Close()
	os.Stdout = old
	out, readErr := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out)
}

// A HUMAN-FACING TABLE IS NOT A DATA INTERFACE, AND IT LOOKS EXACTLY LIKE ONE.
//
// The weekly api-regen bot read the pin with `api list | awk '/^\*/ {print $2}'`.
// That is correct against the starred row and also matches the legend line
// printed under the table -- "* is the version the committed bindings are
// generated from" -- so the step wrote two lines into $GITHUB_OUTPUT:
//
//	current=2.0.77
//	is
//
// GitHub rejects the second (`Invalid format 'is'`) and fails the step, which
// is what every scheduled run did from the day the legend was added. Nothing
// could have caught it: the legend was a presentation change, and the awk was
// in another file in another language.
//
// So `--current` is the machine-readable half, and what it owes its one caller
// is ONE line with nothing around it.
func TestAPIListCurrentIsOneMachineReadableLine(t *testing.T) {
	out := captureStdout(t, func() error { return runAPIList([]string{"--current"}) })

	if want := factorio.DefaultAPIVersion + "\n"; out != want {
		t.Fatalf("api list --current printed %q, want exactly %q", out, want)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("api list --current printed %d lines, want 1: %q", n, out)
	}
	// The decoration that did the damage: a leading mark, or a second column.
	if body := strings.TrimSuffix(out, "\n"); strings.ContainsAny(body, " \t*") {
		t.Errorf("api list --current printed decoration, so a caller has to "+
			"parse it again: %q", out)
	}
}

func TestAPIListRefusesAnArgumentItDoesNotKnow(t *testing.T) {
	err := runAPIList([]string{"--currrent"})
	if err == nil {
		t.Fatal("api list accepted a misspelled flag, so a typo in the bot " +
			"would print the whole table and be parsed as the pin")
	}
	if !strings.Contains(err.Error(), "--current") {
		t.Errorf("the refusal does not name the flag that exists: %v", err)
	}
}

// THE TWO DEFECTS THAT TOOK THE BOT DOWN ARE TEXT PROPERTIES OR THEY ARE
// NOTHING. Neither is reachable by building anything: one lives in an awk
// program and one in a git invocation, both inside a workflow that runs weekly
// on a schedule and whose failure arrives as an email.
//
// The second is the worse of the two and had never fired. `git diff` does not
// report untracked files, and a Factorio version this repo has never seen
// arrives as a whole new UNTRACKED directory -- so the gate that exists to
// notice a new release was the one thing structurally unable to see one, and
// would have reported "nothing to do" and exited GREEN on exactly the week it
// was written for. That is this repo's own recorded shape: a skipped gate reads
// exactly like a pass.
func TestTheRegenBotDoesNotReadTheHumanTableOrAskGitDiffForANewDirectory(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "api-regen.yml"))
	if err != nil {
		t.Fatal(err)
	}

	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // the comments describe both defects on purpose
		}
		if strings.Contains(line, "api list") &&
			!strings.Contains(line, "api list --current") &&
			!strings.Contains(line, "api list --diff-base") {
			t.Errorf("api-regen.yml:%d reads a version out of the human table "+
				"again; `api list --current` and `api list --diff-base` are the "+
				"one-line forms:\n\t%s", i+1, strings.TrimSpace(line))
		}
		if strings.Contains(line, "git diff") && strings.Contains(line, "api/") {
			t.Errorf("api-regen.yml:%d gates on `git diff` over api/, which "+
				"cannot see the untracked directory a new version arrives as; "+
				"`git status --porcelain` can:\n\t%s", i+1, strings.TrimSpace(line))
		}
	}
}

// --diff-base NAMES THE RELEASE BEFORE THE ONE BEING DIFFED, by version number,
// and is held to --current's contract: one line, nothing around it.
//
// The fixture is directory names alone, because that is all the answer reads,
// and it carries the two cases a string comparison gets wrong: 2.1.9 against
// 2.1.10, and the pulled version itself already sitting on disk, which is
// where the bot asks from.
func TestAPIListDiffBaseNamesThePreviousRelease(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"2.0.77", "2.1.9", "2.1.10", "2.1.20", "2.1.22", "notes"} {
		if err := os.Mkdir(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FKLUA_API_DIR", dir)

	for _, c := range []struct{ version, want, why string }{
		{"2.1.22", "2.1.20", "the pulled version is on disk and must not be its own base"},
		{"2.1.21", "2.1.20", "a version between two committed ones"},
		{"2.1.11", "2.1.10", "2.1.10 is newer than 2.1.9, which a string sort denies"},
		{"2.0.78", "2.0.77", "a 2.0.x patch diffs against 2.0.x, not the newest 2.1"},
		{"2.2.0", "2.1.22", "a new series diffs against the previous series' newest"},
		{"1.1.110", factorio.DefaultAPIVersion, "nothing older: the pin"},
	} {
		for _, args := range [][]string{
			{"--diff-base", c.version}, {"--diff-base=" + c.version},
		} {
			out := captureStdout(t, func() error { return runAPIList(args) })
			if out != c.want+"\n" {
				t.Errorf("api list %v printed %q, want exactly %q (%s)",
					args, out, c.want+"\n", c.why)
			}
		}
	}

	for _, args := range [][]string{
		{"--diff-base", "latest"},
		{"--diff-base"},
		{"--diff-base", "2.1.22", "--current"},
	} {
		if err := runAPIList(args); err == nil {
			t.Errorf("api list %v was accepted; the bot would diff against "+
				"whatever it printed", args)
		}
	}
}

// THE BOT DIFFS AGAINST THE RELEASE BEFORE THE ONE IT PULLED, NOT THE PIN.
//
// The pin answers whether moving the default would cost anything, and it was
// the same question as "what changed in this release" only while every
// release was a 2.0.x patch. The first PR the bot opened, for 2.1.22, carried
// 295 breaking entries against 2.0.77, nearly all of them the 2.0-to-2.1 gap,
// where 2.1.20 to 2.1.22 is 51; the review the workflow promises is reading
// that list and nothing else. A text property for the reason the two above it
// are: nothing built reaches what a workflow passes to a command.
func TestTheRegenBotDiffsAgainstThePreviousReleaseNotThePin(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "api-regen.yml"))
	if err != nil {
		t.Fatal(err)
	}
	asks := false
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // the comments name the old baseline on purpose
		}
		if strings.Contains(line, "api list --diff-base") {
			asks = true
		}
		if strings.Contains(line, "api list --current") || strings.Contains(line, "steps.before") {
			t.Errorf("api-regen.yml:%d reaches for the pin, which the bot has no "+
				"use for since its diff stopped being against it:\n\t%s",
				i+1, strings.TrimSpace(line))
		}
	}
	if !asks {
		t.Error("api-regen.yml never asks `api list --diff-base`, so its diff has " +
			"some other baseline than the release before the one it pulled")
	}
}

// A RELEASE THE GENERATORS CANNOT ABSORB MUST STILL PRODUCE A PR, and since
// 2026-08-24 the regenerate step is a place that can fail.
//
// gen-bindings takes a census of every description the checkout owns, so the
// version this job has just pulled is one of its inputs -- which is the point
// (a pulled version now arrives WITH a census, where 2.1.12 sat committed
// without one for two pins) and is also a new way for the step to go red. The
// Test step below it has been `continue-on-error` from the start for exactly
// this reasoning: a red run means the generators met a description they could
// not handle, which is the interesting failure and the one a human most needs
// to see, so the PR carries it rather than being blocked by it. Leaving the
// regenerate step able to abort the job would put the most interesting release
// of the year in an email nobody reads.
//
// It is a text property for the same reason the two above are: nothing that can
// be built reaches a workflow's failure semantics.
func TestTheRegenBotCannotBeBlockedByARegenerationItCannotDo(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "api-regen.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)

	// The step that runs the generators over every committed description.
	lines := strings.Split(text, "\n")
	regen := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "run:") && strings.Contains(line, "gen-bindings") {
			regen = i
		}
	}
	if regen < 0 {
		t.Fatal("api-regen.yml no longer regenerates anything; the census of the " +
			"version it pulls is what that step is now also for")
	}

	// Its own step block, back to the previous `- name:`.
	start := 0
	for i := regen; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "- name:") {
			start = i
			break
		}
	}
	block := strings.Join(lines[start:regen+1], "\n")
	if !strings.Contains(block, "continue-on-error: true") {
		t.Errorf("api-regen.yml:%d the regenerate step can abort the job. It reads "+
			"the freshly pulled description now, so a release the generators cannot "+
			"absorb would produce no PR at all -- which is the week the bot exists "+
			"for:\n%s", start+1, block)
	}
	if !strings.Contains(text, "steps.regen.outcome == 'failure'") {
		t.Error("api-regen.yml opens a NON-draft PR after a regeneration that " +
			"failed; the draft condition must carry steps.regen.outcome the way " +
			"it carries steps.tests.outcome")
	}
}

// THE BOT'S DRAFT FLAG MEANS SOMETHING ONLY IF ITS TEST STEP CAN PASS.
//
// The PR is a draft when the Test step fails, on the stated grounds that a red
// suite means the generators could not absorb the new description. That step
// ran bare `go test ./...` on a runner with no oracle, no Rust wasm target and
// no TinyGo, so three availability guards failed on every run and the first PR
// the bot ever opened (2.1.22) was a draft for that reason alone. A flag that is
// always set is a gate that cannot fail, read from the other side.
//
// So the step must be the repo's own entry point, which builds the oracle, and
// must provision what ci.yml's `go` job provisions: the declaration that TinyGo
// is absent, and the Rust target. Both halves are asked of ci.yml too, so a
// change of spelling there is a failure here rather than a silent divergence.
func TestTheRegenBotsTestStepCanPassOnItsRunner(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	read := func(name string) []string {
		b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		// The comments name the old line on purpose, so they are blanked rather
		// than matched -- and blanked rather than dropped, so an index is still
		// a line number.
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				lines[i] = ""
			}
		}
		return lines
	}
	regen, ci := read("api-regen.yml"), read("ci.yml")

	declare := guest.NoToolchainEnv + ": '1'"
	target := "rustup target add wasm32-unknown-unknown"
	for _, want := range []string{declare, target} {
		if !strings.Contains(strings.Join(ci, "\n"), want) {
			t.Errorf("ci.yml no longer contains %q; this test mirrors ci.yml's "+
				"`go` job and needs updating with it", want)
		}
	}

	// The Test step's own block: from its `- name:` to the next one.
	at := -1
	for i, line := range regen {
		if strings.TrimSpace(line) == "id: tests" {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("api-regen.yml has no step with `id: tests`; the draft condition " +
			"reads steps.tests.outcome")
	}
	start, end := at, len(regen)
	for start > 0 && !strings.HasPrefix(strings.TrimSpace(regen[start]), "- name:") {
		start--
	}
	for i := at + 1; i < len(regen); i++ {
		if strings.HasPrefix(strings.TrimSpace(regen[i]), "- name:") {
			end = i
			break
		}
	}
	block := strings.Join(regen[start:end], "\n")

	if !strings.Contains(block, "run: make test") {
		t.Errorf("api-regen.yml:%d the Test step is not `make test`. Bare `go test` "+
			"has no oracle here, and TestTheOracleIsBuilt fails every run:\n%s",
			start+1, block)
	}
	if !strings.Contains(block, declare) {
		t.Errorf("api-regen.yml:%d the Test step does not declare %s, so the TinyGo "+
			"guard fails on a runner that was never meant to have TinyGo:\n%s",
			start+1, declare, block)
	}
	if !strings.Contains(strings.Join(regen[:start], "\n"), target) {
		t.Errorf("api-regen.yml installs no wasm32 Rust target before the Test step, "+
			"so TestTheRustToolchainIsAvailable fails every run; it wants `%s`", target)
	}
}

// THE USAGE LINE NAMES EVERY SUBCOMMAND `fklua api` DISPATCHES, and so does the
// unknown-subcommand line beside it.
//
// The two disagreed: `fklua api` with no arguments printed `pull|list|diff`
// while the line one return below already named `check` -- so the command's own
// help omitted the one subcommand a script calls, and nothing noticed, because
// every other test drives `api check` by calling it rather than by asking what
// exists. This repo's most-recorded shape one command over: two places
// describing one thing, free to drift.
//
// THE LIST IS DERIVED FROM THE DISPATCH, by parsing runAPI's own switch, rather
// than being a fourth spelling of it here. A test carrying its own list is a
// third place to forget.
func TestTheAPIUsageNamesEverySubcommandItDispatches(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout, so runAPI's dispatch cannot be read: %v", err)
	}
	subs := dispatchedSubcommands(t, filepath.Join(root, "cmd", "fklua", "api.go"))

	// ANTI-VACUITY. A parse that found nothing, or that walked the wrong
	// function, would pass every assertion below by asserting nothing at all.
	if len(subs) < 4 {
		t.Fatalf("runAPI dispatches %d subcommand(s) (%v); it has had four since "+
			"`api check` landed, so either this parse is reading the wrong thing "+
			"or one was removed", len(subs), subs)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"an unknown subcommand", []string{"nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runAPI(tc.args)
			if err == nil {
				t.Fatalf("runAPI(%v) returned no error", tc.args)
			}
			for _, s := range subs {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("%q does not name the %q subcommand, which runAPI "+
						"dispatches", err.Error(), s)
				}
			}
		})
	}
}

// dispatchedSubcommands reads the string cases of the switch inside runAPI: what
// `fklua api` will actually run, as the compiler sees it.
func dispatchedSubcommands(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var subs []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runAPI" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", lit.Value, err)
				}
				subs = append(subs, v)
			}
			return true
		})
	}
	return subs
}
