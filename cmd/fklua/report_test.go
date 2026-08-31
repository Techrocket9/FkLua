package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// The report contract lives in report.go; these tests are its red proofs. They
// decode into map[string]any rather than a typed struct on purpose -- a typed
// unmarshal is silent about a renamed field, and the field-name SET is part of
// the contract (api check --json's rule, applied here).

func runModReport(t *testing.T, args ...string) (map[string]any, []byte, error) {
	t.Helper()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	out := t.TempDir()
	path := filepath.Join(t.TempDir(), "report.json")
	full := append(args, "--name", "rep-mod", "--version", "0.1.0",
		"--author", "someone", "-o", out, "--report", path)
	err := runMod(full)
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("no report was written (run error: %v): %v", err, rerr)
	}
	var doc map[string]any
	if derr := json.Unmarshal(b, &doc); derr != nil {
		t.Fatalf("the report is not JSON: %v\n%s", derr, b)
	}
	return doc, b, err
}

func at(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("no object at %v", path)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("no field %q on the way to %v", p, path)
		}
	}
	return cur
}

// The success path, end to end: a guest that calls one member by a constant id
// and carries the matching pin stamp packages green, and the report carries
// every verdict the prose printed.
func TestTheReportCarriesThePackagingVerdicts(t *testing.T) {
	guest := stampedGuest(t, 1, factorio.PinExport(factorio.DefaultAPIVersion))
	doc, _, err := runModReport(t, guest)
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	if at(t, doc, "ok") != true {
		t.Errorf("ok is not true")
	}
	if doc["refusal"] != nil {
		t.Errorf("refusal is not null on success: %v", doc["refusal"])
	}
	if got := at(t, doc, "api", "version"); got != factorio.DefaultAPIVersion {
		t.Errorf("api.version = %v", got)
	}
	if at(t, doc, "api", "table_attached") != true {
		t.Errorf("a guest calling a member did not attach a table")
	}
	if got := at(t, doc, "pruning", "members", "complete"); got != true {
		t.Errorf("a constant id did not prove complete")
	}
	if got := at(t, doc, "pruning", "members", "shipped"); got != float64(1) {
		t.Errorf("one member call shipped %v rows", got)
	}
	if got := at(t, doc, "pruning", "members", "total"); got == float64(0) {
		t.Errorf("the members total is 0 with a table attached")
	}
	if got := at(t, doc, "pin", "status"); got != "ok" {
		t.Errorf("pin.status = %v for a matching stamp", got)
	}
	if got := at(t, doc, "signature", "status"); got != "absent" {
		t.Errorf("signature.status = %v for a guest with no sig stamp", got)
	}
	hooks := at(t, doc, "hooks", "control").([]any)
	if len(hooks) != 1 || hooks[0] != "fk_on_tick" {
		t.Errorf("hooks.control = %v", hooks)
	}
	if at(t, doc, "outputs", "path") == "" {
		t.Errorf("outputs.path is empty")
	}
	if got := at(t, doc, "build", "factorio_version"); got != factorio.DefaultFactorioVersion {
		t.Errorf("build.factorio_version = %v", got)
	}
}

// THE HALF THE FEATURE WAS ASKED FOR: a refusal writes the report too, with a
// stable kind a tool can switch on. Red-proven during development by making the
// wrapper write only on success, which fails this test at "no report was
// written".
func TestAReportIsWrittenOnARefusal(t *testing.T) {
	guest := stampedGuest(t, 1,
		factorio.PinExport(factorio.DefaultAPIVersion),
		factorio.PinExport("9.9.9"))
	doc, _, err := runModReport(t, guest)
	if err == nil {
		t.Fatalf("two binding sets were not refused")
	}
	if at(t, doc, "ok") != false {
		t.Errorf("ok is not false on a refusal")
	}
	if got := at(t, doc, "refusal", "kind"); got != "api_pin" {
		t.Errorf("refusal.kind = %v", got)
	}
	msg := at(t, doc, "refusal", "message").(string)
	if !strings.Contains(msg, "ONE") {
		t.Errorf("the refusal message is not the human one: %q", msg)
	}
	if got := at(t, doc, "pin", "status"); got != "mismatch" {
		t.Errorf("pin.status = %v on the refusal", got)
	}
	if got := len(at(t, doc, "pin", "guest").([]any)); got != 2 {
		t.Errorf("pin.guest carries %d stamps, not the 2 the module has", got)
	}
}

// The signature check stays a WARNING: a wrong generation packages, ok is
// true, and the report says mismatch where a tool can see it.
func TestASignatureMismatchIsAStatusNotARefusal(t *testing.T) {
	guest := stampedGuest(t, 1,
		factorio.PinExport(factorio.DefaultAPIVersion),
		factorio.SigExport("000000000000"))
	doc, _, err := runModReport(t, guest)
	if err != nil {
		t.Fatalf("a signature mismatch must not refuse: %v", err)
	}
	if at(t, doc, "ok") != true {
		t.Errorf("ok is not true")
	}
	if got := at(t, doc, "signature", "status"); got != "mismatch" {
		t.Errorf("signature.status = %v", got)
	}
	if at(t, doc, "signature", "packaged") == "" {
		t.Errorf("signature.packaged is empty")
	}
}

// A list is [] and never null -- a property of the BYTES, asserted on a guest
// whose lists are all empty except the one control hook.
func TestReportListsAreNeverNull(t *testing.T) {
	_, raw, err := runModReport(t, tinyGuest(t))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	for _, w := range []string{`"guest": []`, `"data": []`, `"lua_migrations": []`} {
		if !bytes.Contains(raw, []byte(w)) {
			t.Errorf("the report does not contain %s:\n%s", w, raw)
		}
	}
	if bytes.Contains(raw, []byte(`"guest": null`)) ||
		bytes.Contains(raw, []byte(`"control": null`)) {
		t.Errorf("a list serialised as null:\n%s", raw)
	}
}

// The field-name SET is the contract. A rename that a typed unmarshal would
// swallow fails here by name; a new field fails here too, which is the point
// -- adding one is fine, and the tool-facing contract moves with intent
// rather than by accident.
func TestTheReportFieldSetIsPinned(t *testing.T) {
	doc, _, err := runModReport(t, tinyGuest(t))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	var keys []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, child := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			keys = append(keys, p)
			walk(p, child)
		}
	}
	walk("", doc)
	sort.Strings(keys)
	want := []string{
		"api", "api.source", "api.table_attached", "api.table_bytes", "api.version",
		"build", "build.control_module", "build.data_module",
		"build.factorio_version", "build.factorio_version_source",
		"build.fuel", "build.gc", "build.nan", "build.opt", "build.persist",
		"hooks", "hooks.control", "hooks.data",
		"inert", "lua_migrations", "ok",
		"outputs", "outputs.control_lua_bytes", "outputs.data_lua_bytes",
		"outputs.path", "outputs.zip",
		"pin", "pin.guest", "pin.packaged", "pin.status",
		"pruning",
		"pruning.defines", "pruning.defines.complete", "pruning.defines.shipped",
		"pruning.defines.total",
		"pruning.events", "pruning.events.complete", "pruning.events.shipped",
		"pruning.events.total",
		"pruning.members", "pruning.members.complete", "pruning.members.shipped",
		"pruning.members.total",
		"refusal",
		"signature", "signature.guest", "signature.packaged", "signature.status",
	}
	if strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Errorf("the report's field set moved:\n got %v\nwant %v", keys, want)
	}
}

// A packaging that never runs a scan must not wear the scan's warning.
// FkRecipes' dogfood report: a data-only mod's report said `complete: false`,
// which reads as "ships the full table" for a mod that has no table --
// table_attached is the disambiguator and the flags are vacuously true.
func TestADataOnlyReportIsVacuouslyComplete(t *testing.T) {
	doc, _, err := runModReport(t, "--data-module", dataGuest(t, "fk_data"))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	if at(t, doc, "api", "table_attached") != false {
		t.Errorf("a data-only mod attached a table")
	}
	for _, table := range []string{"members", "events", "defines"} {
		if got := at(t, doc, "pruning", table, "complete"); got != true {
			t.Errorf("pruning.%s.complete = %v with no scan run; the zero value "+
				"is the full-table warning and this mod has no table", table, got)
		}
	}
}

// The incomplete-pruning verdict is the one warning a driving tool exists to
// surface, and it is structured now: an id the scan cannot prove reads as
// complete=false with the full table shipped.
func TestAnUnprovableIdReadsAsIncomplete(t *testing.T) {
	// The id arrives through a local set inside a branch, which the scan's
	// control-flow rule deliberately refuses to follow.
	p := filepath.Join(t.TempDir(), "guest.wat")
	wat := `(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick") (local $id i32)
    (local.set $id (i32.const 3))
    (block
      (local.set $id (i32.const 4)))
    (drop (call $call (i32.const 1) (local.get $id) (i32.const 0) (i32.const 64))))
)`
	if err := os.WriteFile(p, []byte(wat), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, _, err := runModReport(t, p)
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	if got := at(t, doc, "pruning", "members", "complete"); got != false {
		t.Errorf("an unprovable id read as complete")
	}
	shipped := at(t, doc, "pruning", "members", "shipped").(float64)
	total := at(t, doc, "pruning", "members", "total").(float64)
	if shipped != total || total == 0 {
		t.Errorf("an incomplete scan shipped %v of %v rows; the full table is "+
			"the contract", shipped, total)
	}
}
