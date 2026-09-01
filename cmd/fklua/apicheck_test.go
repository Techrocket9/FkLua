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
	code, out, _ = checkRunErr(t, args...)
	return code, out
}

// checkRunErr is checkRun plus what the refusal SAID, which is the half stdout
// cannot show: an exit 2 prints nothing there by construction, so a test that
// only reads stdout cannot tell one refusal from another.
func checkRunErr(t *testing.T, args ...string) (code int, out, msg string) {
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
		code, msg = ee.code, ee.Error()
	default:
		t.Fatalf("api check returned an error carrying no status: %v", runErr)
	}
	return code, string(raw), msg
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
	defs := factorio.GenerateDefines(from)
	for _, m := range report.Members {
		s := factorio.SurfaceOf(report, map[int]bool{m.ID: true}, true,
			map[int]bool{}, true, evs, map[int]bool{}, true, defs)
		if len(factorio.CheckGuest(s, diff).Hits) == 0 {
			return m.ID, m.Class + "::" + m.Name
		}
	}
	t.Skipf("every member of api/%s is affected by %s, so the clean control "+
		"cannot be built", factorio.DefaultAPIVersion, to)
	return 0, ""
}

// impactedDefine finds a `defines.*` VALUE the guest can read by id at the
// default pin whose dotted path is in the breaking list -- and whose GROUP
// survives, so the finding can only have come from the value-level walk.
//
// Derived from the two descriptions for the same reason every other id in this
// file is: a path written down here is one that quietly stops discriminating
// the next time either description is regenerated.
func impactedDefine(t *testing.T, from *factorio.API, to string) (id int, what string) {
	t.Helper()
	b := loadTo(t, to)
	surviving := map[string]bool{}
	for _, g := range b.Defines {
		surviving[g.Name] = true
	}
	breaking := map[string]bool{}
	for _, c := range factorio.DiffAPI(from, b).Breaking() {
		breaking[c.What] = true
	}
	for _, d := range factorio.GenerateDefines(from).Defines {
		group, _, _ := strings.Cut(d.Path, ".")
		if surviving[group] && breaking["defines."+d.Path] {
			return d.ID, "defines." + d.Path
		}
	}
	t.Skipf("no define value of api/%s is itself a breaking change in %s whose "+
		"group survives, so no guest can be built that the check must report",
		factorio.DefaultAPIVersion, to)
	return 0, ""
}

// cleanDefine finds a define value both versions have, for the control. Without
// it a check that reported every breaking change would pass every assertion
// about the impacted case.
func cleanDefine(t *testing.T, from *factorio.API, to string) (id int, what string) {
	t.Helper()
	survives := map[string]bool{}
	for _, d := range factorio.GenerateDefines(loadTo(t, to)).Defines {
		survives[d.Path] = true
	}
	for _, d := range factorio.GenerateDefines(from).Defines {
		if survives[d.Path] {
			return d.ID, "defines." + d.Path
		}
	}
	t.Skipf("api/%s and %s share no define value, so the clean control cannot "+
		"be built", factorio.DefaultAPIVersion, to)
	return 0, ""
}

// definingGuest writes a guest that reads exactly one define, by a constant id
// the pruning scan can see, and calls no member at all -- so its member and
// event surfaces are empty and any finding it produces has to be the define.
//
// A .wat for the reason stampedGuest gives: the property lives entirely in the
// import and the constant, and a TinyGo build would spend a minute pinning the
// answer to whichever defines this checkout's committed bindings happen to
// name.
func definingGuest(t *testing.T, id int) string {
	t.Helper()
	return defineGuestSrc(t, fmt.Sprintf("(i32.const %d)", id))
}

// computedDefineGuest reads a define whose id arrives in a parameter, so the
// constant scan cannot say which one. That is the unproven case for defines,
// and it is a separate arm from the member one: `complete` is the AND of three
// scans and dropping any of them from it reads as a pass.
func computedDefineGuest(t *testing.T) string {
	t.Helper()
	return defineGuestSrc(t, "(local.get 0)")
}

func defineGuestSrc(t *testing.T, idExpr string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "defguest.wat")
	src := fmt.Sprintf(`(module
  (import "fk" "define" (func $define (param i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick") (param i32)
    (drop (call $define %s))))
`, idExpr)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
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

// A GUEST THAT READS A DEFINE THE UPGRADE TOOK AWAY IS IMPACTED, and until
// this existed it was CLEAN.
//
// `UsedDefines` has been recovering exactly which `defines.*` values a compiled
// guest reads since the pruner needed it, and the check never consumed it -- so
// a guest holding an id for a value a release removed was told its upgrade was
// safe. That is the one answer this feature exists to make impossible, and it
// is the quiet direction: the define resolves to 0 at load and the guest reads
// a wrong constant rather than failing.
func TestAPICheckNamesADefineTheUpgradeRemoved(t *testing.T) {
	from, to := checkVersions(t)
	id, what := impactedDefine(t, from, to)

	code, out := checkRun(t, definingGuest(t, id), "--to", to, "--json")
	if code != 1 {
		t.Errorf("a guest reading %s, which %s removes, exits %d, want 1:\n%s",
			what, to, code, out)
	}
	v := decodeVerdict(t, out)
	if v.Verdict != factorio.VerdictImpacted {
		t.Fatalf("verdict is %q, want %q:\n%s", v.Verdict, factorio.VerdictImpacted, out)
	}
	if v.Surface.Defines != 1 {
		t.Errorf("the surface counts %d define(s) for a guest that reads one; "+
			"the scan did not reach the guest at all", v.Surface.Defines)
	}
	if v.Surface.Members != 0 {
		t.Errorf("this guest calls no member and the surface counts %d, so the "+
			"finding below might not be the define's", v.Surface.Members)
	}
	found := false
	for _, f := range v.Findings {
		if f.What != what {
			continue
		}
		found = true
		if f.Match != factorio.MatchDefine {
			t.Errorf("finding %q matched as %q, want %q", f.What, f.Match,
				factorio.MatchDefine)
		}
		if f.Kind != "breaking" {
			t.Errorf("finding %q has kind %q; only breaking changes are "+
				"cross-referenced", f.What, f.Kind)
		}
		if f.Detail == "" {
			t.Errorf("finding %q says what moved and not what happened to it", f.What)
		}
	}
	if !found {
		t.Errorf("%s is not among the findings: %+v", what, v.Findings)
	}
	if v.Ignored == 0 {
		t.Error("every breaking change in the release was reported as touching a " +
			"one-define guest, which is what a check that reports everything " +
			"looks like")
	}
}

// THE CONTROL: a guest reading a define BOTH versions have is clean. Without it
// a check that reported every breaking change would pass the test above.
func TestAPICheckIsCleanForAGuestReadingASurvivingDefine(t *testing.T) {
	from, to := checkVersions(t)
	id, what := cleanDefine(t, from, to)

	code, out := checkRun(t, definingGuest(t, id), "--to", to, "--json")
	if code != 0 {
		t.Errorf("a guest reading only %s, which %s keeps, exits %d, want 0:\n%s",
			what, to, code, out)
	}
	v := decodeVerdict(t, out)
	if v.Verdict != factorio.VerdictClean {
		t.Errorf("verdict is %q, want %q:\n%s", v.Verdict, factorio.VerdictClean, out)
	}
	if len(v.Findings) != 0 {
		t.Errorf("a clean verdict carries findings: %+v", v.Findings)
	}
	if v.Surface.Defines != 1 {
		t.Errorf("the surface counts %d define(s) for a guest that reads one, so "+
			"the clean answer is over an empty question", v.Surface.Defines)
	}
	// ANTI-VACUITY. A diff with no removed define in it makes every such guest
	// clean, and a clean answer over an empty question proves nothing.
	if v.BreakingTotal == 0 {
		t.Errorf("api/%s and api/%s have no breaking change between them",
			factorio.DefaultAPIVersion, to)
	}
	if !v.Complete {
		t.Errorf("the scan of a guest with one constant define id reads as "+
			"incomplete: %+v", v)
	}
}

// A COMPUTED DEFINE ID IS AN UNPROVEN SCAN, exactly as a computed member id is.
// `complete` is the AND of three scans and dropping any one of them from it
// reads as a pass over a guest nothing could see.
func TestAPICheckWillNotCallAGuestWithAComputedDefineIDClean(t *testing.T) {
	_, to := checkVersions(t)

	code, out := checkRun(t, computedDefineGuest(t), "--to", to, "--json")
	v := decodeVerdict(t, out)
	if v.Complete {
		t.Fatalf("a guest whose define id is computed reads as a complete scan, "+
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

// AN EMPTY --from IS THE SAME REFUSAL AS --from WITH NO VALUE, because it is
// the same mistake: `--from "$PIN"` with PIN unset is what a shell hands over.
//
// It bites harder here than it would in a command with one default, because
// three rules sit BEHIND the flag. Reading "was a flag typed" off an empty
// string collides with a value a caller can pass, so an empty --from would fall
// through to the guest's own stamp -- or, one arm down, to the manifest -- and
// ANSWER, with `from_source` telling a caller that believes it named a version
// that the module or the project chose. That is the filed defect's own shape
// one level down, which is why the flag carries a bool beside its value.
func TestAPICheckRefusesAnEmptyFrom(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)

	pinned := t.TempDir()
	if err := os.WriteFile(filepath.Join(pinned, projectFile), []byte(`[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both places the sentinel would have been read as silence, so a fix that
	// closed one of them is not enough to pass.
	for _, tc := range []struct {
		name  string
		guest string
	}{
		{"a stamped guest, where the stamp would have answered",
			stampedGuest(t, id, factorio.PinExport(other))},
		{"an unstamped guest, where the manifest would have",
			callingGuest(t, id)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := chdir(t, pinned)
			defer back()

			code, out, msg := checkRunErr(t, tc.guest, "--from", "",
				"--to", other, "--json")
			if code != 2 {
				t.Fatalf("exit %d, want 2: `--from \"\"` is a flag whose version "+
					"went missing, not a request for the resolver's other rules\n%s",
					code, out)
			}
			if out != "" {
				t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
			}
			if !strings.Contains(msg, "--from needs a version") {
				t.Errorf("the refusal is not the one `--from` with no value at all "+
					"gets, and it is the same mistake:\n%s", msg)
			}
		})
	}
}

// THE DEFAULT IS THE LAST RULE, THE ONE THAT ANSWERS WHEN NOTHING ELSE DOES,
// and the document echoes what it resolved to. A caller that passed no version
// has nothing else to learn it from, and the constant has moved under this repo
// twice.
//
// Run from an empty directory, because the manifest rule sits between the guest
// and the default: an fklua.toml dropped into this package's own directory
// would otherwise answer, and the failure would read as "the default is wrong"
// rather than "the default never fired".
func TestAPICheckEchoesTheVersionsItResolved(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := cleanMember(t, from, to)
	guest := callingGuest(t, id)

	back := chdir(t, t.TempDir())
	defer back()

	_, out := checkRun(t, guest, "--to", to, "--json")
	v := decodeVerdict(t, out)
	if v.From != factorio.DefaultAPIVersion {
		t.Errorf("from is %q for an unstamped guest with no manifest; the last "+
			"rule is this binary's default, %q, and the document is where a "+
			"caller finds out which rule answered", v.From,
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
	// `from_source` is deliberately in this list. A caller that passed no
	// --from now gets a different `from` than it used to -- the guest's own pin
	// stamp rather than this binary's default -- and the document is the only
	// place it can find out which rule answered.
	wantKeys(t, "the verdict document", doc, "from", "to", "from_source", "guest",
		"verdict", "complete", "exit_code", "surface", "breaking_total", "ignored",
		"findings")

	var surface map[string]json.RawMessage
	if err := json.Unmarshal(doc["surface"], &surface); err != nil {
		t.Fatal(err)
	}
	wantKeys(t, "surface", surface, "members", "events", "defines", "concepts")

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

// WHICH DESCRIPTION THE GUEST'S IDS ARE INDICES INTO is a fact the module
// carries, and until this landed `--from` ignored it.
//
// Filed by WormholeBelts (FKLUA-GAPS item 9). Member, event and define ids are
// dense per-description indices, and `--from` defaulted to this BINARY's
// DefaultAPIVersion -- a fact about fklua and about nothing else. So a guest
// stamped at another pin, calling a member that exists only there, was told it
// touches NOTHING: surface members 0, verdict clean, exit 0. The other
// direction is as quiet and worse, since an id that resolves under both
// descriptions names two different members and the check reports a change to
// one the guest never called.
//
// The member here is DERIVED to be one the default description does not have an
// id for at all, which is what makes the assertion below discriminate: under
// the old resolution the surface is empty, under this one it is the member.

// memberBeyondTheDefault finds a member of `other` whose id is past every id the
// default description assigns, so a guest calling it has an EMPTY surface when
// its ids are decoded against the default and a one-member surface when they are
// decoded against the description they actually came from.
func memberBeyondTheDefault(t *testing.T, from *factorio.API, other string) (id int, what string) {
	t.Helper()
	last := -1
	for _, m := range factorio.GenerateMembers(from).Members {
		if m.ID > last {
			last = m.ID
		}
	}
	ms := factorio.GenerateMembers(loadTo(t, other)).Members
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].ID > last {
			return ms[i].ID, ms[i].Class + "::" + ms[i].Name
		}
	}
	t.Skipf("api/%s assigns no id past api/%s's last (%d), so a guest calling "+
		"one cannot be built and this test would pass against either resolution",
		other, factorio.DefaultAPIVersion, last)
	return 0, ""
}

// A STAMPED GUEST IS READ AT ITS OWN PIN, with no --from at all.
func TestAPICheckResolvesFromTheGuestsOwnPinStamp(t *testing.T) {
	from, other := checkVersions(t)
	id, what := memberBeyondTheDefault(t, from, other)
	guest := stampedGuest(t, id, factorio.PinExport(other))

	_, out := checkRun(t, guest, "--to", factorio.DefaultAPIVersion, "--json")
	v := decodeVerdict(t, out)
	if v.From != other {
		t.Errorf("from is %q for a guest stamped %s; the stamp is the FACT about "+
			"which description its ids were assigned over, and %q is a fact about "+
			"this binary", v.From, other, factorio.DefaultAPIVersion)
	}
	if v.FromSource != factorio.FromSourceStamp {
		t.Errorf("from_source is %q, want %q", v.FromSource, factorio.FromSourceStamp)
	}
	// THE REPRODUCTION, as an assertion. Member %d exists at %s and at no id of
	// the default description, so decoding against the default reports a guest
	// that calls a member as touching nothing at all.
	if v.Surface.Members != 1 {
		t.Errorf("the surface counts %d member(s) for a guest calling %s (id %d); "+
			"its ids were decoded against %s, where that id names nothing",
			v.Surface.Members, what, id, v.From)
	}
}

// AN EXPLICIT --from THAT CONTRADICTS THE STAMP IS A REFUSAL, not a preference.
//
// The flag says which description to decode against and the module says which
// one its ids came from; when they disagree exactly one of them is a fact, and
// answering the flag's question produces a surface that is a fiction rather than
// a wrong number. Exit 2, because nothing was checked.
//
// AND IT SAYS WHEN THE VERSION IT NAMES IS A GUESS. `PinExport` writes every
// character outside [0-9A-Za-z] as `_` and has no inverse, so a stamp this
// checkout has no description for can only be READ BACK -- and this refusal
// tells the reader to type that reading (`PASS THE STAMP'S VERSION: --from X`),
// which is advice about a spelling nothing here knows. The committed arm is the
// control and the anti-vacuity tooth in one: an emptied caveat makes its
// `not present` assertion fail, because every message contains the empty string.
func TestAPICheckRefusesAFromThatContradictsTheStamp(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)

	for _, tc := range []struct {
		name    string
		stamp   string
		names   string
		guessed bool
	}{
		{"a committed version", factorio.PinExport(other), other, false},
		// No api/9.9.9 directory, so "9.9.9" here is the mangled export name
		// with its dots put back rather than a version this checkout carries.
		{"a version with no description", factorio.PinExport("9.9.9"), "9.9.9", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest := stampedGuest(t, id, tc.stamp)

			code, out, msg := checkRunErr(t, guest, "--from", factorio.DefaultAPIVersion,
				"--to", other, "--json")
			if code != 2 {
				t.Fatalf("exit %d, want 2: this run checked nothing, and a 0 or 1 "+
					"here reads as a verdict over a surface nothing could "+
					"decode\n%s", code, out)
			}
			if out != "" {
				t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
			}
			// BOTH SIDES NAMED, which is checkAPIPin's shape: a reader has to
			// know which of the two said what, or the refusal is not actionable.
			for _, want := range []string{tc.names, factorio.DefaultAPIVersion,
				tc.stamp, "the --from flag"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q:\n%s", want, msg)
				}
			}
			// THE CAVEAT'S ACTIONABLE TAIL BELONGS HERE, because `--from` is
			// exactly what this refusal is about: a caller who typed a spelling
			// this checkout cannot check can type another one.
			if got := strings.Contains(msg, guessCaveat(false, true)); got != tc.guessed {
				t.Errorf("the refusal names API %s and says it is a guess: %v, "+
					"want %v -- a reading this checkout cannot check must say so, "+
					"and one it looked up must not\n%s", tc.names, got, tc.guessed, msg)
			}
		})
	}
}

// AN EXPLICIT --from THAT AGREES WITH THE STAMP IS THE CONTROL for the refusal
// above, and it is the one a same-pin gate types. Without it a resolver that
// refused every stamped guest carrying a --from would pass the test above and
// break the harness the flag exists for.
func TestAPICheckAcceptsAFromThatMatchesTheStamp(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)
	guest := stampedGuest(t, id, factorio.PinExport(other))

	code, out := checkRun(t, guest, "--from", other, "--to", other, "--json")
	if code != 0 {
		t.Fatalf("--from %s on a guest stamped %s exits %d:\n%s", other, other, code, out)
	}
	v := decodeVerdict(t, out)
	if v.From != other || v.FromSource != factorio.FromSourceFlag {
		t.Errorf("from/from_source are %q/%q, want %q/%q", v.From, v.FromSource,
			other, factorio.FromSourceFlag)
	}
	if v.Surface.Members != 1 {
		t.Errorf("the surface counts %d member(s) for a guest calling one",
			v.Surface.Members)
	}
}

// TWO STAMPS IS A REFUSAL, because there is no description that decodes both.
//
// A guest may link exactly one generated binding set. Two of them in one module
// disagree about what every id past their first difference means, so whichever
// description is named, at most one half of the guest is being read correctly.
// `fklua mod` refuses the same arrangement for the same reason.
//
// It names both versions, so it carries the guess caveat's NAMING half when
// EITHER of them had to be read back out of its export name -- the second arm --
// and not when both were looked up. It never carries the caveat's ACTIONABLE
// TAIL: this refusal returns before `--from` is read, so telling the reader to
// pass a spelling buys them a second run printing the same words.
func TestAPICheckRefusesAGuestLinkingTwoBindingSets(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)

	// THE TAIL DERIVED RATHER THAN SPELLED A SECOND TIME, so this cannot pass on
	// a guessCaveat whose two forms are the same string -- which is what the
	// split it exists to hold would look like if it were undone.
	tail := strings.TrimPrefix(guessCaveat(false, true), guessCaveat(false, false))
	if tail == "" || tail == guessCaveat(false, true) {
		t.Fatalf("the guess caveat's actionable tail is %q against a naming half "+
			"of %q; the assertions below read it by difference", tail,
			guessCaveat(false, false))
	}

	for _, tc := range []struct {
		name    string
		second  string
		names   string
		guessed bool
		args    []string
	}{
		{"both committed", factorio.PinExport(other), other, false, nil},
		{"one with no description", factorio.PinExport("9.9.9"), "9.9.9", true, nil},
		// --from CHANGES NOTHING HERE, which is why the tail must not be here:
		// `len(pins) > 1` returns before the flag is read.
		{"one with no description, --from typed", factorio.PinExport("9.9.9"),
			"9.9.9", true, []string{"--from", "9.9.9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest := stampedGuest(t, id,
				factorio.PinExport(factorio.DefaultAPIVersion), tc.second)

			args := append([]string{guest}, tc.args...)
			args = append(args, "--to", other, "--json")
			code, out, msg := checkRunErr(t, args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: one of the two binding sets is being "+
					"decoded as the other whatever description is named\n%s", code, out)
			}
			if out != "" {
				t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
			}
			if !strings.Contains(msg, tc.names) ||
				!strings.Contains(msg, factorio.DefaultAPIVersion) {
				t.Errorf("the refusal does not name both pins:\n%s", msg)
			}
			if got := strings.Contains(msg, guessCaveat(false, false)); got != tc.guessed {
				t.Errorf("the refusal says a version it named is a guess: %v, want "+
					"%v -- it names %s and %s\n%s", got, tc.guessed,
					factorio.DefaultAPIVersion, tc.names, msg)
			}
			// THE INERT INSTRUCTION. This refusal ignores --from, so a message
			// ending in "pass the real spelling as --from" contradicts its own
			// body two lines above and spends the reader's next attempt.
			if strings.Contains(msg, tail) {
				t.Errorf("the refusal tells the reader to pass --from, which it "+
					"returns before reading:\n%s", msg)
			}
		})
	}
}

// A STAMP THIS CHECKOUT HAS NO DESCRIPTION FOR IS A REFUSAL that names the
// command which fixes it. The ids cannot be decoded at all, and falling back to
// the default would answer the question this whole resolution exists to stop
// being answered wrongly.
//
// The version it names is a GUESS by construction -- this is the branch where no
// committed description carries it -- so the caveat is always here.
func TestAPICheckRefusesAStampItHasNoDescriptionFor(t *testing.T) {
	_, other := checkVersions(t)
	guest := stampedGuest(t, 1, factorio.PinExport("9.9.9"))

	// THE CAVEAT IS A SENTENCE AND ITS ABSENCE IS EMPTINESS, so an emptied one
	// would make every `contains` below pass while saying nothing.
	if guessCaveat(false, true) == "" || guessCaveat(true, true) != "" {
		t.Fatalf("guessCaveat is %q for a read-back version and %q for a looked-up "+
			"one; the refusals below assert its presence by substring",
			guessCaveat(false, true), guessCaveat(true, true))
	}

	code, out, msg := checkRunErr(t, guest, "--to", other, "--json")
	if code != 2 {
		t.Fatalf("exit %d, want 2: nothing here could decode the guest's ids\n%s",
			code, out)
	}
	if out != "" {
		t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
	}
	// THE ACTIONABLE TAIL IS HERE, unlike the two-stamp refusal: this one names
	// `api pull` and a spelling typed as --from is a second way out of it.
	for _, want := range []string{"9.9.9", "api pull", guessCaveat(false, true)} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it is not actionable:\n%s",
				want, msg)
		}
	}
}

// AN UNSTAMPED GUEST WITH NO MANIFEST RESOLVES TO THE DEFAULT, byte for byte as
// it always did. That is the whole of the compatibility: bindings generated
// before the stamp existed carry none, and a guest linking no generated bindings
// carries none either, so an absent stamp is silence here exactly as it is for
// `fklua mod`.
func TestAnUnstampedGuestWithNoManifestResolvesToTheDefault(t *testing.T) {
	from, to := checkVersions(t)
	id, _ := cleanMember(t, from, to)
	guest := callingGuest(t, id)

	// A directory with no fklua.toml in it, so the manifest rule cannot fire
	// and the assertion below is about the last resort rather than about
	// whatever happens to be beside the test binary.
	back := chdir(t, t.TempDir())
	defer back()

	_, out := checkRun(t, guest, "--to", to, "--json")
	v := decodeVerdict(t, out)
	if v.From != factorio.DefaultAPIVersion {
		t.Errorf("from is %q for an unstamped guest with no manifest, want the "+
			"default %q", v.From, factorio.DefaultAPIVersion)
	}
	if v.FromSource != factorio.FromSourceDefault {
		t.Errorf("from_source is %q, want %q", v.FromSource, factorio.FromSourceDefault)
	}
}

// AN UNSTAMPED GUEST TAKES THE PROJECT'S OWN PIN, which is the intent when the
// module carries no fact. gen-bindings and lock have always read `[fklua] api`;
// this is the same key answering the same question one command over, and without
// it a pinned project's own harness asks about a description nothing in the
// project chose.
func TestAnUnstampedGuestResolvesFromTheManifest(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := cleanMember(t, from, other)
	guest := callingGuest(t, id)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(`[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	back := chdir(t, dir)
	defer back()

	_, out := checkRun(t, guest, "--to", factorio.DefaultAPIVersion, "--json")
	v := decodeVerdict(t, out)
	if v.From != other {
		t.Errorf("from is %q beside a manifest pinning %q; the default answered "+
			"over a version the project chose", v.From, other)
	}
	if v.FromSource != factorio.FromSourceManifest {
		t.Errorf("from_source is %q, want %q", v.FromSource,
			factorio.FromSourceManifest)
	}
}

// A MANIFEST THAT IS THERE AND UNREADABLE IS THE FOURTH REFUSAL, and it says
// what this command was asking it and how to answer without it.
//
// The arrangement is ordinary rather than exotic: a manifest a newer fklua wrote
// carries a key an older one does not know, and every key it does not know is a
// hard error by design. Under that, `api check` on an UNSTAMPED guest exits 2
// carrying loadProject's own words -- a sentence about a file the caller never
// mentioned, naming neither that the manifest was consulted for `[fklua] api`
// nor that `--from` answers without it. Falling through to the default instead
// is not the alternative: that is the defect this resolver exists to close.
// The second arm is the reason checkFrom's manifest rule carries no
// `proj.API != ""` guard: `[fklua] api` is REQUIRED, so a manifest with no pin
// in it does not arrive as an empty pin, it arrives here. A guard for the state
// that cannot happen would be dead today and worse than dead if the key ever
// became optional, since it would hand the question to the default without a
// word.
func TestAPICheckRefusesAnUnreadableManifest(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := cleanMember(t, from, other)
	guest := callingGuest(t, id)

	for _, tc := range []struct {
		name     string
		manifest string
		names    string
	}{
		{"a key this fklua is older than", `[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "` + other + `"
some_future_key = "written by an fklua this one is older than"
`, "some_future_key"},
		{"no pin at all, which is an error rather than an empty one", `[mod]
name = "pinned"
version = "0.1.0"
`, "api is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, projectFile),
				[]byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			back := chdir(t, dir)
			defer back()

			code, out, msg := checkRunErr(t, guest, "--to",
				factorio.DefaultAPIVersion, "--json")
			if code != 2 {
				t.Fatalf("exit %d, want 2: the rule that would have answered could "+
					"not be read, so nothing here chose the description the ids were "+
					"decoded against\n%s", code, out)
			}
			if out != "" {
				t.Errorf("a run that checked nothing printed a verdict:\n%s", out)
			}
			// THE TWO SIDES AND THE WAY OUT. loadProject's own words are one of
			// them and are not enough on their own: a refusal that names only
			// the file has not said why this command opened it.
			for _, want := range []string{
				projectFile,
				tc.names,
				"[fklua] api",
				"--from",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q, so it names neither "+
						"what was consulted nor how to answer without it:\n%s",
						want, msg)
				}
			}
		})
	}
}

// THE FOUR RULES ARE FOUR, and `from_source` is a CLOSED SET.
//
// Driven end to end rather than asserted about the constants, because the
// failure this catches is a resolver that collapsed two rules into one -- which
// leaves every constant in place and every other test in this file green, since
// each of those asserts one rule at a time.
func TestEveryFromSourceIsReachableAndDistinct(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)
	stamped := stampedGuest(t, id, factorio.PinExport(other))
	bare := callingGuest(t, id)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(`[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "`+other+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The default's arm needs a directory with no manifest in it, and the
	// manifest's arm needs the one written above; everything else is the same
	// question asked four ways.
	seen := map[string]string{}
	for _, tc := range []struct {
		want string
		cwd  string
		args []string
	}{
		{factorio.FromSourceFlag, dir, []string{bare, "--from", other}},
		{factorio.FromSourceStamp, dir, []string{stamped}},
		{factorio.FromSourceManifest, dir, []string{bare}},
		{factorio.FromSourceDefault, t.TempDir(), []string{bare}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			back := chdir(t, tc.cwd)
			defer back()
			_, out := checkRun(t, append(tc.args,
				"--to", factorio.DefaultAPIVersion, "--json")...)
			v := decodeVerdict(t, out)
			if v.FromSource != tc.want {
				t.Errorf("from_source is %q, want %q", v.FromSource, tc.want)
			}
			if prev, dup := seen[v.FromSource]; dup {
				t.Errorf("%q also answered for %q, so two rules are indistinguishable "+
					"to a caller", v.FromSource, prev)
			}
			seen[v.FromSource] = tc.want
		})
	}
	if len(seen) != 4 {
		t.Errorf("%d of the 4 from_source values were reached: %v", len(seen), seen)
	}
}

// THE REPORT'S HEADER NAMES WHICH RULE ANSWERED, and nothing else in this file
// reads it.
//
// Every other assertion about `from_source` goes through the JSON document, so
// collapsing Report() back to `# api check: A -> B` leaves the whole suite
// green -- and the report is the output a mod author actually reads, where the
// question "which description was this decoded against" is exactly as live as
// it is for a script. The phrase comes from FromSourcePhrase rather than being
// spelled again here: the document and the report must not be able to disagree
// about which of the four answered.
func TestTheReportHeaderNamesWhereFromCameFrom(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)

	empty := t.TempDir()
	for _, tc := range []struct {
		name  string
		want  string
		guest string
	}{
		{"stamp", factorio.FromSourceStamp,
			stampedGuest(t, id, factorio.PinExport(other))},
		{"default", factorio.FromSourceDefault, callingGuest(t, id)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No fklua.toml, so the default arm is the last rule answering and
			// not the manifest rule wearing its name.
			back := chdir(t, empty)
			defer back()

			_, human := checkRun(t, tc.guest, "--to", factorio.DefaultAPIVersion)
			head, _, _ := strings.Cut(human, "\n")
			// THE "A -> B" SHAPE IS STILL AT THE FRONT. The phrase is appended
			// rather than spliced between the versions, which is what lets a
			// reader scan for the pair where it has always been.
			if !strings.HasPrefix(head, "# api check: ") ||
				!strings.Contains(head, " -> ") {
				t.Fatalf("the report's heading moved: %q", head)
			}
			if want := factorio.FromSourcePhrase(tc.want); !strings.Contains(head, want) {
				t.Errorf("the heading %q does not name where `from` came from "+
					"(%q); the version alone cannot say, because the four rules "+
					"routinely resolve to the same string", head, want)
			}
		})
	}
}

// A STAMP THAT DISAGREES WITH THE MANIFEST IS SAID OUT LOUD, and it is a NOTICE
// rather than a refusal.
//
// `api check` answers a question about a GUEST -- which description are this
// module's ids indices into -- and the stamp is the fact that answers it, so
// proceeding from the stamp is right even when the project pinned something
// else. Answering SILENTLY is not: `fklua mod` refuses that exact pairing rather
// than choosing, so a caller that reads a clean verdict here and then packages
// walks into a refusal, and one that reads it as covering the build the project
// would make has read it wrong. Stderr, because stdout carries the verdict.
//
// BOTH ARMS, and the second is the one a real caller reaches. The notice is a
// function of (stamp, manifest): `fklua mod`'s refusal reads those two and has
// never heard of `--from`, so the arrangement is exactly as present when the
// caller typed `--from` naming the stamp's own version as when it typed nothing
// -- and `--from PIN --to PIN` is what a downstream harness types, as a same-pin
// gate. Attaching the notice to the untyped rule instead left that arm silent.
func TestAStampThatDisagreesWithTheManifestIsNoticed(t *testing.T) {
	from, other := checkVersions(t)
	id, _ := memberBeyondTheDefault(t, from, other)
	guest := stampedGuest(t, id, factorio.PinExport(other))

	// The manifest pins the DEFAULT and the guest is stamped `other`, so the
	// two name different descriptions and the stamp is the one that answers.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(`[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		args   []string
		source string
	}{
		{"no --from at all", nil, factorio.FromSourceStamp},
		// THE FLAG NAMES THE STAMP'S OWN VERSION, which is legal and is the
		// shape a same-pin gate types. The stamp still disagrees with the
		// manifest, so `fklua mod` still refuses the pairing.
		{"--from naming the stamp", []string{"--from", other}, factorio.FromSourceFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := chdir(t, dir)
			defer back()

			args := append([]string{guest}, tc.args...)
			args = append(args, "--to", factorio.DefaultAPIVersion, "--json")
			var code int
			var out string
			msg := captureStderr(t, func() {
				code, out = checkRun(t, args...)
			})

			// THE VERDICT IS UNCHANGED. A notice that moved the status or put a
			// word on stdout would be a refusal wearing a notice's clothes.
			if code != 0 {
				t.Errorf("exit %d, want 0: the stamp answered and the guest is "+
					"clean against it; a divergence with the manifest is not this "+
					"check's refusal to make\n%s", code, out)
			}
			v := decodeVerdict(t, out)
			if v.From != other || v.FromSource != tc.source {
				t.Errorf("from/from_source are %q/%q, want %q/%q: the stamp is the "+
					"fact and the manifest is the intent", v.From, v.FromSource,
					other, tc.source)
			}

			// BOTH SIDES NAMED, and the consequence: `fklua mod` refuses this
			// pairing.
			for _, want := range []string{other, factorio.DefaultAPIVersion,
				factorio.PinExport(other), projectFile, "fklua mod", "refuses"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the notice does not mention %q:\n%s", want, msg)
				}
			}

			// THE NOTICE NAMES THE RULE THAT ACTUALLY ANSWERED, and it is a THIRD
			// output saying so beside the report's header and the document's
			// `from_source`. The two arms here resolve through DIFFERENT rules and
			// both reach this notice, so a sentence that spells one of them is
			// wrong on the other -- two lines above a header, and one JSON field
			// away, that say otherwise. Asserted in both directions: the phrase
			// for the rule that fired is present and no other one is.
			for _, src := range factorio.FromSources {
				phrase := factorio.FromSourcePhrase(src)
				if got := strings.Contains(msg, phrase); got != (src == tc.source) {
					t.Errorf("the notice names the resolution rule %q: %v, want %v "+
						"-- %q is what answered, and the verdict beside it says so\n%s",
						phrase, got, src == tc.source, tc.source, msg)
				}
			}

			if strings.Contains(out, "NOTICE") {
				t.Errorf("the notice landed on stdout, where the verdict lives:\n%s", out)
			}
		})
	}
}

// THE NOTICE MAY NOT PROMISE OUTPUT THAT NEVER ARRIVES. checkFrom prints it
// before either description is loaded, so a `--to` this checkout carries no
// description for produces the notice and then a refusal: nothing on stdout, no
// verdict, and no report. A sentence pointing at "the verdict below" is then a
// claim about a run's output made by a function that cannot know what follows
// it -- the same shape as attributing a resolution rule the caller did not use.
//
// The arrangement is ordinary rather than exotic: `--to` names the version a mod
// author is considering, which is routinely one they have not pulled yet.
func TestTheDivergenceNoticePromisesNoVerdictItMayNotProduce(t *testing.T) {
	_, other := checkVersions(t)
	guest := stampedGuest(t, 1, factorio.PinExport(other))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFile), []byte(`[mod]
name = "pinned"
version = "0.1.0"

[fklua]
api = "`+factorio.DefaultAPIVersion+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	back := chdir(t, dir)
	defer back()

	var code int
	var out, refusal string
	notice := captureStderr(t, func() {
		code, out, refusal = checkRunErr(t, guest, "--to", "9.9.9", "--json")
	})

	// THE ARRANGEMENT, asserted rather than assumed -- the check below is
	// vacuous against a run that produced a verdict after all.
	if code != 2 || out != "" {
		t.Fatalf("exit %d with %d byte(s) on stdout, want 2 and nothing: this run "+
			"is the one where no verdict follows the notice\n%s", code, len(out), out)
	}
	if !strings.Contains(refusal, "9.9.9") {
		t.Fatalf("the refusal does not name the version it could not load:\n%s", refusal)
	}
	// ANTI-VACUITY: the notice has to have fired, or "it says nothing about a
	// verdict" is true of the empty string.
	if !strings.Contains(notice, "NOTICE") || !strings.Contains(notice, projectFile) {
		t.Fatalf("no divergence notice on a stamped guest beside a manifest "+
			"pinning another version:\n%s", notice)
	}
	if strings.Contains(notice, "below") {
		t.Errorf("the notice points at something below it, and this run has "+
			"nothing below it but a refusal:\n%s", notice)
	}
}
