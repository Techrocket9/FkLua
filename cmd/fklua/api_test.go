package main

import (
	"io"
	"os"
	"path/filepath"
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
