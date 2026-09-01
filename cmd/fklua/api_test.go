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
			!strings.Contains(line, "api list --current") {
			t.Errorf("api-regen.yml:%d reads the pin out of the human table "+
				"again; `api list --current` is the one line it wants:\n\t%s",
				i+1, strings.TrimSpace(line))
		}
		if strings.Contains(line, "git diff") && strings.Contains(line, "api/") {
			t.Errorf("api-regen.yml:%d gates on `git diff` over api/, which "+
				"cannot see the untracked directory a new version arrives as; "+
				"`git status --porcelain` can:\n\t%s", i+1, strings.TrimSpace(line))
		}
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
