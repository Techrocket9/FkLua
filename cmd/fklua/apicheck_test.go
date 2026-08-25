package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// `api check` IS A GATE A SCRIPT READS, so its status and its `--json` document
// are the interface and the table is not.
//
// Everything here works from a hand-written .wat calling one member by a
// constant id, for the reason `stampedGuest` gives one file over: the property
// under test lives entirely in the module's import and constant, and a TinyGo
// build would spend a minute proving it while pinning the answer to whichever
// members this checkout's committed bindings happen to name. The member ids are
// DERIVED from the two descriptions at test time -- an id baked in here is one
// that quietly stops discriminating the next time either is regenerated.

// checkRun invokes `api check` and reports the status a shell would see plus
// what landed on stdout. Nothing here calls os.Exit: the status is carried back
// as a value precisely so it can be asserted.
func checkRun(t *testing.T, args ...string) (code int, out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := runAPICheck(args)
	w.Close()
	os.Stdout = old
	raw, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}

	var ee *exitError
	switch {
	case runErr == nil:
		code = 0
	case errors.As(runErr, &ee):
		code = ee.code
	default:
		t.Fatalf("api check returned an error carrying no status: %v", runErr)
	}
	return code, string(raw)
}

// checkVersions is the pair the whole file works against: this binary's own pin
// and one other committed description. They have to differ or nothing below
// discriminates anything.
func checkVersions(t *testing.T) (from *factorio.API, to string) {
	t.Helper()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	to = otherAPIVersion(t)
	a, err := factorio.LoadAPI(filepath.Join(moduleRoot(t), "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a, to
}

func loadTo(t *testing.T, to string) *factorio.API {
	t.Helper()
	b, err := factorio.LoadAPI(filepath.Join(moduleRoot(t), "api", to, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// impactedMember finds a member the guest can call by id at the default pin
// whose OWN identity is in the breaking list, so the finding it produces is a
// member match rather than a class or concept one.
func impactedMember(t *testing.T, from *factorio.API, to string) (id int, what string) {
	t.Helper()
	diff := factorio.DiffAPI(from, loadTo(t, to))
	breaking := map[string]bool{}
	for _, c := range diff.Breaking() {
		breaking[c.What] = true
	}
	for _, m := range factorio.GenerateMembers(from).Members {
		key := m.Class + "::" + m.Name
		if breaking[key] {
			return m.ID, key
		}
	}
	t.Skipf("no member of api/%s is itself a breaking change in %s, so no guest "+
		"can be built that the check must report", factorio.DefaultAPIVersion, to)
	return 0, ""
}

// cleanMember finds a member whose whole surface -- itself, its class and every
// named type in its signature -- survives the upgrade. Without this control a
// check that reported every breaking change would pass every assertion about
// the impacted case.
func cleanMember(t *testing.T, from *factorio.API, to string) (id int, what string) {
	t.Helper()
	diff := factorio.DiffAPI(from, loadTo(t, to))
	report := factorio.GenerateMembers(from)
	evs := factorio.GenerateEvents(from)
	for _, m := range report.Members {
		s := factorio.SurfaceOf(report, map[int]bool{m.ID: true}, true,
			map[int]bool{}, true, evs)
		if len(factorio.CheckGuest(s, diff).Hits) == 0 {
			return m.ID, m.Class + "::" + m.Name
		}
	}
	t.Skipf("every member of api/%s is affected by %s, so the clean control "+
		"cannot be built", factorio.DefaultAPIVersion, to)
	return 0, ""
}

func decodeVerdict(t *testing.T, out string) factorio.CheckVerdict {
	t.Helper()
	var v factorio.CheckVerdict
	// UNMARSHALLED RATHER THAN GREPPED, deliberately. A substring assertion
	// passes against output that is not JSON at all, which is the one failure a
	// document promised to a script must not be able to have.
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json did not print a JSON document: %v\n%s", err, out)
	}
	return v
}

// A GUEST THAT TOUCHES NOTHING AFFECTED EXITS 0 AND SAYS `clean`. That status is
// the whole of what a build harness needs in order to package one guest for two
// engine series instead of building it twice.
func TestAPICheckIsCleanAndExitsZeroForAnUnaffectedGuest(t *testing.T) {
	from, to := checkVersions(t)
	id, what := cleanMember(t, from, to)

	code, out := checkRun(t, callingGuest(t, id), "--to", to, "--json")
	if code != 0 {
		t.Errorf("a guest calling only %s exits %d, want 0:\n%s", what, code, out)
	}
	v := decodeVerdict(t, out)
	if v.Verdict != factorio.VerdictClean {
		t.Errorf("verdict is %q, want %q", v.Verdict, factorio.VerdictClean)
	}
	if len(v.Findings) != 0 {
		t.Errorf("a clean verdict carries findings: %+v", v.Findings)
	}
	// ANTI-VACUITY. A diff with nothing breaking in it makes every guest clean,
	// and a clean answer over an empty question proves nothing at all.
	if v.BreakingTotal == 0 {
		t.Errorf("api/%s and api/%s have no breaking change between them, so "+
			"this guest being clean says nothing", factorio.DefaultAPIVersion, to)
	}
	if v.Ignored != v.BreakingTotal {
		t.Errorf("a clean verdict ignored %d of %d breaking changes; every one "+
			"of them should be ignored", v.Ignored, v.BreakingTotal)
	}
	if !v.Complete {
		t.Errorf("the scan of a guest with one constant member id reads as "+
			"incomplete: %+v", v)
	}
	if v.ExitCode != code {
		t.Errorf("the document says exit_code %d and the process exited %d",
			v.ExitCode, code)
	}
}

// A GUEST THAT CALLS SOMETHING REMOVED EXITS 1 AND NAMES IT, with the reason it
// reaches this guest beside it.
func TestAPICheckExitsOneAndNamesWhatBreaks(t *testing.T) {
	from, to := checkVersions(t)
	id, what := impactedMember(t, from, to)

	code, out := checkRun(t, callingGuest(t, id), "--to", to, "--json")
	if code != 1 {
		t.Errorf("a guest calling %s, which %s breaks, exits %d, want 1:\n%s",
			what, to, code, out)
	}
	v := decodeVerdict(t, out)
	if v.Verdict != factorio.VerdictImpacted {
		t.Fatalf("verdict is %q, want %q:\n%s", v.Verdict, factorio.VerdictImpacted, out)
	}
	found := false
	for _, f := range v.Findings {
		if f.What != what {
			continue
		}
		found = true
		if f.Kind != "breaking" {
			t.Errorf("finding %q has kind %q; only breaking changes are "+
				"cross-referenced", f.What, f.Kind)
		}
		if f.Match != factorio.MatchMember {
			t.Errorf("finding %q matched as %q, want %q: it is the guest's own "+
				"member that moved, not its class or a type in its signature",
				f.What, f.Match, factorio.MatchMember)
		}
		if f.Detail == "" {
			t.Errorf("finding %q says what moved and not what happened to it", f.What)
		}
	}
	if !found {
		t.Errorf("%s is not among the findings: %+v", what, v.Findings)
	}
	if v.Ignored == 0 {
		t.Errorf("every breaking change in the release was reported as touching " +
			"a one-member guest, which is what a check that reports everything " +
			"looks like")
	}
}

// A SCAN THAT COULD NOT SEE EVERYTHING IS NOT A PASS, and it is a THIRD verdict
// rather than a flavour of clean: the guest may call anything at all, so the
// honest answer is that nothing was proven.
func TestAPICheckWillNotCallAnUnreadableGuestClean(t *testing.T) {
	_, to := checkVersions(t)

	// The member id arrives in a parameter, so the constant scan the pruner and
	// this check share cannot say which member is called.
	p := filepath.Join(t.TempDir(), "computed.wat")
	if err := os.WriteFile(p, []byte(`(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick") (param i32)
    (drop (call $call (i32.const 1) (local.get 0) (i32.const 0) (i32.const 64)))))
`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := checkRun(t, p, "--to", to, "--json")
	v := decodeVerdict(t, out)
	if v.Complete {
		t.Fatalf("a guest whose member id is computed reads as a complete scan, "+
			"so this test is not exercising the case it names:\n%s", out)
	}
	if v.Verdict != factorio.VerdictUnproven {
		t.Errorf("verdict is %q, want %q", v.Verdict, factorio.VerdictUnproven)
	}
	if code != 1 {
		t.Errorf("an unproven check exits %d, want 1: a harness reading 0 would "+
			"package a guest nothing was able to check", code)
	}
}

// 2 IS "THE CHECK COULD NOT BE RUN", and it exists because 1 and 2 were one
// status: "your guest is fine" and "you misspelled the version" both exited 1,
// and only a human reading stderr could tell them apart.
//
// Nothing is written to stdout in any of these, so a caller that captured
// stdout and got bytes knows it has a verdict.
func TestAPICheckExitsTwoWhenItCannotRun(t *testing.T) {
	_, to := checkVersions(t)
	guest := callingGuest(t, 0)
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"a flag it does not know", []string{guest, "--to", to, "--reasonable"}},
		{"no guest", []string{"--to", to}},
		{"no --to", []string{guest}},
		{"--to with no value", []string{guest, "--to"}},
		{"a version this installation does not have",
			[]string{guest, "--to", "9.9.9"}},
		{"a module that is not there",
			[]string{filepath.Join(dir, "absent.wasm"), "--to", to}},
		// `api diff --json` takes a PATH and this one does not, so somebody will
		// type one. It must not be swallowed as a second guest module.
		{"--json handed a path", []string{guest, "--to", to, "--json",
			filepath.Join(dir, "out.json")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := checkRun(t, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2: this run checked nothing, so reporting "+
					"it as 1 would read as `something breaks`", code)
			}
			if out != "" {
				t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
			}
		})
	}
}

// THE --from DEFAULT IS THIS BINARY'S OWN PIN, and the document echoes what it
// resolved to. A caller that passed no version has nothing else to learn it
// from, and the constant has moved under this repo twice.
func TestAPICheckEchoesTheVersionsItResolved(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := cleanMember(t, from, to)
	guest := callingGuest(t, id)

	_, out := checkRun(t, guest, "--to", to, "--json")
	v := decodeVerdict(t, out)
	if v.From != factorio.DefaultAPIVersion {
		t.Errorf("--from defaulted to %q; it is the binary's pin, %q, and the "+
			"document is where a caller finds that out", v.From,
			factorio.DefaultAPIVersion)
	}
	if v.To != to {
		t.Errorf("to is %q, want %q", v.To, to)
	}
	if v.Guest != guest {
		t.Errorf("guest is %q, want %q: a harness checking many guests in one "+
			"loop has nothing else to tell the documents apart by", v.Guest, guest)
	}

	// And an explicit --from is honoured, which is the half a downstream
	// harness has to pass because the default is not stable across releases.
	_, out = checkRun(t, guest, "--from", to, "--to", factorio.DefaultAPIVersion, "--json")
	v = decodeVerdict(t, out)
	if v.From != to || v.To != factorio.DefaultAPIVersion {
		t.Errorf("--from/--to were not honoured: got %s -> %s", v.From, v.To)
	}
}

// THE FIELD NAMES ARE THE CONTRACT, so they are pinned as a SET rather than by
// reading the ones a test happens to care about: unmarshalling into the typed
// struct is silent about a field that was renamed, removed or added, which is
// exactly the drift a consumer discovers in production.
//
// Adding a field is a change to this list. That is the point: a document a
// script reads should not grow one by accident.
func TestTheCheckVerdictDocumentHasTheFieldsItPromises(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := impactedMember(t, from, to)

	_, out := checkRun(t, callingGuest(t, id), "--to", to, "--json")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json did not print a JSON object: %v\n%s", err, out)
	}
	wantKeys(t, "the verdict document", doc, "from", "to", "guest", "verdict",
		"complete", "exit_code", "surface", "breaking_total", "ignored", "findings")

	var surface map[string]json.RawMessage
	if err := json.Unmarshal(doc["surface"], &surface); err != nil {
		t.Fatal(err)
	}
	wantKeys(t, "surface", surface, "members", "events", "concepts")

	var findings []map[string]json.RawMessage
	if err := json.Unmarshal(doc["findings"], &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("the impacted guest produced no findings, so the finding shape " +
			"below is not being checked")
	}
	wantKeys(t, "a finding", findings[0], "what", "kind", "match", "detail")
}

func wantKeys(t *testing.T, what string, got map[string]json.RawMessage, keys ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if _, ok := got[k]; !ok {
			t.Errorf("%s lost the field %q", what, k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s grew the field %q; a document a script reads should not "+
				"gain one without this list moving", what, k)
		}
	}
}

// AN EMPTY LIST IS `[]` AND NOT `null`, because a caller that has to handle both
// is a caller parsing the same thing twice -- and it is a property of the bytes
// rather than of the decoded value, since encoding/json reads both as an empty
// slice and would never notice.
func TestACleanVerdictCarriesAnEmptyFindingsArray(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := cleanMember(t, from, to)

	_, out := checkRun(t, callingGuest(t, id), "--to", to, "--json")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(doc["findings"])); got != "[]" {
		t.Errorf("findings is %s on a clean verdict, want []", got)
	}
}

// THE HUMAN TABLE IS PRESENTATION AND THE DOCUMENT IS THE INTERFACE, and the
// two must not arrive together: a caller parsing stdout would have to find
// where one ends. The table is unchanged in the mode that prints it.
func TestAPICheckPrintsOneThingOrTheOther(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := cleanMember(t, from, to)
	guest := callingGuest(t, id)

	_, jsonOut := checkRun(t, guest, "--to", to, "--json")
	if strings.Contains(jsonOut, "# api check:") {
		t.Errorf("--json printed the human table as well:\n%s", jsonOut)
	}

	_, human := checkRun(t, guest, "--to", to)
	if !strings.HasPrefix(human, fmt.Sprintf("# api check: %s -> %s",
		factorio.DefaultAPIVersion, to)) {
		t.Errorf("the human report's heading moved:\n%s", human)
	}
	if strings.Contains(human, `"verdict"`) {
		t.Errorf("the default mode printed the machine document:\n%s", human)
	}
}
