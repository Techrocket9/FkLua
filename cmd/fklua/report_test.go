package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
		// A list's ELEMENTS are walked too, under the list's own name, so a
		// field set inside a list of objects is pinned like any other. Nothing
		// duplicates: every list here holds either strings or one shape of
		// object.
		//
		// IT CANNOT GO RED HERE AND THAT IS NOT AN OVERSIGHT. The only list of
		// objects this report has is jumps.<module>.relayed_functions, and the
		// tiny guest packaged below relays nothing, so every list this walk
		// meets is empty or holds strings and the recursion never reaches an
		// object through one. Renaming a field inside a relayed_functions row is
		// caught by TestTheReportSaysWhatTheRelayCost, which pins that row's six
		// names on a packaging that actually fills it; the branch is here so the
		// pin does not go blind the day a second list of objects is added.
		if l, ok := v.([]any); ok {
			for _, child := range l {
				walk(prefix, child)
			}
			return
		}
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
	// Sorted first, so a name that appears in every element of a list of objects
	// collapses to one entry.
	keys = slices.Compact(keys)
	want := []string{
		"api", "api.source", "api.table_attached", "api.table_bytes", "api.version",
		"build", "build.control_module", "build.data_module",
		"build.factorio_version", "build.factorio_version_source",
		"build.fuel", "build.gc", "build.nan", "build.opt", "build.persist",
		"hooks", "hooks.control", "hooks.data",
		"inert",
		"jumps",
		"jumps.control", "jumps.control.block_room_bytes", "jumps.control.hop_bytes",
		"jumps.control.limit_bytes", "jumps.control.relayed",
		"jumps.control.relayed_functions", "jumps.control.stations",
		"jumps.control.widest_block_bytes", "jumps.control.widest_block_function",
		"jumps.control.widest_span_after_relay_bytes",
		"jumps.control.widest_span_bytes", "jumps.control.widest_span_function",
		"jumps.data", "jumps.data.block_room_bytes", "jumps.data.hop_bytes",
		"jumps.data.limit_bytes", "jumps.data.relayed",
		"jumps.data.relayed_functions", "jumps.data.stations",
		"jumps.data.widest_block_bytes", "jumps.data.widest_block_function",
		"jumps.data.widest_span_after_relay_bytes",
		"jumps.data.widest_span_bytes", "jumps.data.widest_span_function",
		"lua_migrations", "ok",
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

// ============================================================================
// jumps: what Lua's 18-bit jump limit cost each module
// ============================================================================

// bigDataGuest writes a data module whose fk_data carries one enormous jump.
//
// `shape` picks which one. "flat" is the shape the relay CAN break up: a br_if
// near the top of a long block, everything at function-body level once emitted,
// which is what a data guest whose sections the optimizer inlined looks like.
// "blocky" is the one it cannot: the same distance, but inside a counted loop's
// body, where a body-level label may not go.
func bigDataGuest(t *testing.T, shape string, fill int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`(module (memory 1)
  (func (export "fk_alloc") (param i32) (result i32) (i32.const 0))
  (func (export "fk_free") (param i32))
`)
	switch shape {
	case "flat":
		b.WriteString(`  (func $spanner (export "fk_data") (local $s i32)
    (block $out
      (br_if $out (i32.load (i32.const 0)))`)
		for i := 0; i < fill; i++ {
			b.WriteString("\n      (local.set $s (i32.add (local.get $s) (i32.const 1)))")
		}
		b.WriteString("\n      (return))\n    (unreachable)))\n")
	case "blocky":
		b.WriteString(`  (func $blocky (export "fk_data") (local $i i32) (local $s i32) (local $n i32)
    (local.set $n (i32.const 4))
    (block $out
      (block $done
        (loop $top
          (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
          (br_if $out (i32.eq (local.get $i) (i32.const 3)))`)
		for i := 0; i < fill; i++ {
			b.WriteString("\n          (local.set $s (i32.add (local.get $s) (i32.const 1)))")
		}
		b.WriteString(`
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $top))))
    (drop (local.get $s))))
`)
	case "both", "relayed-and-blocky":
		// BOTH SHAPES IN ONE MODULE, the wide relayable one FIRST and wider
		// than the one that cannot be relayed. That is the only arrangement in
		// which "the module's widest span" and "the function that refused the
		// module" are different functions, which is what the refusal row has to
		// be right about. `fk_settings` is a real data-stage export, so the
		// first function is live without being called from the second, where
		// the optimizer could inline it away.
		b.WriteString(`  (func $spanner (export "fk_settings") (local $s i32)
    (block $out
      (br_if $out (i32.load (i32.const 0)))`)
		for i := 0; i < fill*8/3; i++ {
			b.WriteString("\n      (local.set $s (i32.add (local.get $s) (i32.const 1)))")
		}
		// One fewer closing paren than the "flat" arm: the module stays open
		// for the second function.
		b.WriteString("\n      (return))\n    (unreachable))\n")
		// "relayed-and-blocky" holds the loop body at 15,000 whatever the
		// spanner's size: the spanner crosses the limit and is relayed, the
		// loop body is under it and stays, and its block room is -167,361.
		// That is the module the relayed arm of the note has to be right
		// about, and the only arrangement in which the widest-span function
		// and the function with the least block room are different functions
		// in a module that PACKAGES.
		blockFill := fill
		if shape == "relayed-and-blocky" {
			blockFill = 15000
		}
		b.WriteString(`  (func $blocky (export "fk_data") (local $i i32) (local $s i32) (local $n i32)
    (local.set $n (i32.const 4))
    (block $out
      (block $done
        (loop $top
          (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
          (br_if $out (i32.eq (local.get $i) (i32.const 3)))`)
		for i := 0; i < blockFill; i++ {
			b.WriteString("\n          (local.set $s (i32.add (local.get $s) (i32.const 1)))")
		}
		b.WriteString(`
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $top))))
    (drop (local.get $s))))
`)
	default:
		t.Fatalf("unknown shape %q", shape)
	}
	p := filepath.Join(t.TempDir(), "data.wat")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// runModReportOut is runModReport plus what the command printed. Separate
// because captureStdout fails the test on a run error and half of what is
// asserted here is a refusal.
func runModReportOut(t *testing.T, args ...string) (map[string]any, string, error) {
	t.Helper()
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	old := os.Stdout
	os.Stdout = w
	var doc map[string]any
	var err error
	func() {
		defer func() { os.Stdout = old }()
		doc, _, err = runModReport(t, args...)
	}()
	w.Close()
	out, rerr := io.ReadAll(r)
	r.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	return doc, string(out), err
}

// TestTheReportSaysWhatTheRelayCost is the half FkRecipes asked for: a module
// the relay carries reports the span it hid, not only the span it left.
//
// The distinction is the whole feature. A relayed module's remaining span is
// about one hop whatever its size, so the number that was visible read as a
// comfortable half of the limit for a module 111% over it -- and the first
// library on this substrate quoted 83% of the limit from exactly that number.
// So the pre-relay span, the station count and the block room all have to be
// there, in the JSON and on stdout, and the `wrote` line has to be untouched.
//
// Red-proven by making jumpEntry report WidestSpanBytes: 0 (the plumbing broken
// the way it would break), which fails at "the report hides the span the relay
// carried"; and by dropping the advisory line from jumpNote, which fails at "the
// stdout says nothing about the block room".
func TestTheReportSaysWhatTheRelayCost(t *testing.T) {
	guest := bigDataGuest(t, "flat", 52000)
	doc, out, err := runModReportOut(t, "--data-module", guest)
	if err != nil {
		t.Fatalf("a module the relay should have carried was refused: %v\n%s", err, out)
	}
	j := at(t, doc, "jumps", "data").(map[string]any)
	num := func(k string) int {
		v, ok := j[k].(float64)
		if !ok {
			t.Fatalf("jumps.data.%s is %v, not a number", k, j[k])
		}
		return int(v)
	}
	limit, hop := num("limit_bytes"), num("hop_bytes")
	if limit == 0 || hop == 0 {
		t.Fatalf("the limit and the hop read %d and %d; without them a consumer "+
			"cannot turn any of the rest into a percentage", limit, hop)
	}
	if got := num("widest_span_bytes"); got <= limit {
		t.Errorf("the report hides the span the relay carried: widest_span_bytes is "+
			"%d, at or under the %d-byte limit, which is the post-relay reading and "+
			"not the one that says how near the wall this guest is", got, limit)
	}
	if j["relayed"] != true {
		t.Errorf("relayed is %v for a module carrying stations", j["relayed"])
	}
	if num("stations") == 0 {
		t.Error("stations is 0 for a relayed module")
	}
	if got := num("widest_span_after_relay_bytes"); got > limit || got == 0 {
		t.Errorf("widest_span_after_relay_bytes is %d against a limit of %d", got, limit)
	}
	if got := num("widest_block_bytes"); got == 0 || got > hop {
		t.Errorf("widest_block_bytes is %d against a hop of %d; a module that was "+
			"carried has a block inside one hop and a block of 0 means nothing was "+
			"measured", got, hop)
	}
	if got, want := num("block_room_bytes"), hop-num("widest_block_bytes"); got != want {
		t.Errorf("block_room_bytes is %d and hop - block is %d", got, want)
	}
	if got := at(t, doc, "jumps", "data", "widest_span_function"); got == "" {
		t.Error("widest_span_function is empty, so the number points at no function")
	}

	// The per-function rows, and their field names, which are contract too.
	fns := j["relayed_functions"].([]any)
	if len(fns) != 1 {
		t.Fatalf("relayed_functions carries %d rows, want the 1 function the relay "+
			"carried", len(fns))
	}
	row := fns[0].(map[string]any)
	var keys []string
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"block_room_bytes", "function", "stations",
		"widest_block_bytes", "widest_span_after_relay_bytes", "widest_span_bytes"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("a relayed_functions row's field set moved:\n got %v\nwant %v", keys, want)
	}
	if int(row["widest_span_bytes"].(float64)) != num("widest_span_bytes") {
		t.Errorf("the module's widest span and its only relayed function's disagree")
	}

	// The control module is absent, and an absent module is not a module with no
	// jump: the two constants are there and every measurement is 0.
	c := at(t, doc, "jumps", "control").(map[string]any)
	if c["limit_bytes"].(float64) == 0 || c["widest_span_bytes"].(float64) != 0 ||
		c["relayed"] != false || len(c["relayed_functions"].([]any)) != 0 {
		t.Errorf("the absent control module's entry is not constants-and-zeros: %v", c)
	}

	// And stdout, where the `wrote` line is untouched to the byte and the note
	// sits under the line naming the module it is about.
	if !strings.Contains(out, "wrote ") || !strings.Contains(out, "bytes of Lua)") {
		t.Errorf("the wrote line moved:\n%s", out)
	}
	for _, w := range []string{
		"jump span: widest",
		"% of the 655355-byte limit)",
		"station(s)",
		"widest span after the relay",
		"block room",
		"the number that bounds it is the block room and not the span",
		"//go:noinline",
		"#[inline(never)]",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("the stdout says nothing about %q:\n%s", w, out)
		}
	}
	t.Logf("%s", out)
}

// TestTheReportNamesTheBlockThatRefusedAModule is the other side: the shape the
// relay cannot help, packaged, refused, and reported.
//
// The design named this shape before the relay existed and it had never been
// observed; this is it, built from wasm rather than from hand-written Lua. It is
// the ceiling a large data guest actually meets, so a refusal has to carry the
// distance that caused it and the report has to carry it too -- a refusal is
// exactly when a driving tool needs structure.
//
// Red-proven by returning the zero ModuleJumps from emitWithDiagnostics on the
// error path (which is what it did first), failing at "the report says nothing
// about the module that was refused".
func TestTheReportNamesTheBlockThatRefusedAModule(t *testing.T) {
	guest := bigDataGuest(t, "blocky", 30000)
	doc, out, err := runModReportOut(t, "--data-module", guest)
	if err == nil {
		t.Fatalf("a span with no body-level statement in it was packaged:\n%s", out)
	}
	if at(t, doc, "ok") != false {
		t.Error("ok is not false on a refusal")
	}
	if got := at(t, doc, "refusal", "kind"); got != "emit" {
		t.Errorf("refusal.kind = %v", got)
	}
	msg := at(t, doc, "refusal", "message").(string)
	for _, w := range []string{
		"too big for Lua",
		"no body-level statement boundary",
		"one hop of the ladder covers",
		"//go:noinline",
		"widest_block_bytes",
	} {
		if !strings.Contains(msg, w) {
			t.Errorf("the refusal message does not mention %q:\n%s", w, msg)
		}
	}

	j := at(t, doc, "jumps", "data").(map[string]any)
	span := int(j["widest_span_bytes"].(float64))
	block := int(j["widest_block_bytes"].(float64))
	room := int(j["block_room_bytes"].(float64))
	hop := int(j["hop_bytes"].(float64))
	if span == 0 {
		t.Fatal("the report says nothing about the module that was refused, which is " +
			"the one case a driving tool cannot do without")
	}
	if block <= hop {
		t.Errorf("the block reads %d against a hop of %d; if it fitted, the relay "+
			"would have carried the module", block, hop)
	}
	if room >= 0 {
		t.Errorf("block_room_bytes is %d on a refusal; the room is negative exactly "+
			"when the relay cannot help", room)
	}
	if block*100 < span*99 {
		t.Errorf("the block is %d of a %d-byte span, so this fixture is no longer "+
			"the one-block shape it is here to be", block, span)
	}
	if j["relayed"] != false || int(j["stations"].(float64)) != 0 {
		t.Errorf("the refused module is reported as relayed: %v", j)
	}
	if !strings.Contains(msg, strconv.Itoa(block)) {
		t.Errorf("the refusal message does not carry the block distance %d:\n%s", block, msg)
	}
	t.Logf("span %d, block %d, hop %d, room %d", span, block, hop, room)
}

// TestAModuleUnderTheSpanLimitStillSaysWhatTheRelayCannotCarry is the hole in
// the note that only the report had covered.
//
// "No relay needed" and "the relay could carry this if it had to" are two
// different statements, and the branch that printed the first used to print
// nothing at all about the second: for a module under the threshold the line
// gave the span, the limit and a percentage, and stopped. That is the reassuring
// half of a number that is not reassuring. The fixture here is the blocky shape
// at 15,000 filler statements: its span is 76% of the limit, so no relay runs
// and nothing is refused, and its block room is already negative -- the run it
// would have to break up has nowhere in it to put a station, so the day that
// span crosses the threshold the module stops packaging with no warning that
// anything was ever coming.
//
// The report carried both numbers throughout; stdout is where an author looks,
// and CLAUDE.md and agents/codegen.md both say the two carry the same facts.
//
// Red-proven by dropping the block and the room from that branch of jumpNote,
// which fails at "the stdout says nothing about \"widest block\"" and at the
// advisory.
func TestAModuleUnderTheSpanLimitStillSaysWhatTheRelayCannotCarry(t *testing.T) {
	doc, out, err := runModReportOut(t, "--data-module", bigDataGuest(t, "blocky", 15000))
	if err != nil {
		t.Fatalf("the fixture was refused, so it is not the case under test: %v\n%s", err, out)
	}
	j := at(t, doc, "jumps", "data").(map[string]any)
	num := func(k string) int { return int(j[k].(float64)) }
	limit, hop := num("limit_bytes"), num("hop_bytes")
	span, block, room := num("widest_span_bytes"), num("widest_block_bytes"),
		num("block_room_bytes")

	// The fixture has to be BOTH THINGS AT ONCE or it proves nothing: under the
	// span limit, so no relay ran, and over the block one, so the relay could
	// not have helped.
	if span == 0 || span > limit {
		t.Fatalf("the fixture's widest span is %d against a limit of %d; this test needs "+
			"a module that is UNDER the limit", span, limit)
	}
	if num("stations") != 0 || j["relayed"] != false {
		t.Fatalf("the fixture was relayed after all (%d station(s)), which is the other "+
			"branch of the note", num("stations"))
	}
	if room >= 0 {
		t.Fatalf("the fixture's block room is %+d bytes against a hop of %d; this test "+
			"needs a module the relay could NOT carry", room, hop)
	}

	if !strings.Contains(out, "no relay needed") {
		t.Errorf("the note does not say the relay was not needed:\n%s", out)
	}
	for _, w := range []string{
		"widest block",
		"block room",
		"under the span limit and the relay could NOT carry it",
		"nowhere at body level to put a station",
		"//go:noinline",
		"#[inline(never)]",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("the stdout says nothing about %q:\n%s", w, out)
		}
	}
	for _, n := range []int{block, room} {
		if !strings.Contains(out, strconv.Itoa(n)) {
			t.Errorf("the stdout does not carry %d, which the report does:\n%s", n, out)
		}
	}
	t.Logf("span %d (%d%% of %d), block %d, hop %d, room %d\n%s",
		span, 100*span/limit, limit, block, hop, room, out)
}

// TestCompilePrintsTheJumpNoteUnderItsWroteLine is the `compile` half of the
// note, and the pin on the line it sits under.
//
// `mod`'s note has TestTheReportSaysWhatTheRelayCost; `compile`'s had nothing at
// all, so deleting the loop that prints it left this package green. The two
// claims here are the two the change makes about `compile`: the note is printed,
// and the `wrote` line above it is untouched to the byte -- which is a claim
// about a line other people's scripts parse, so it is pinned as a whole string
// and not as a `strings.Contains`.
//
// Red-proven twice. Removing the `for ... range jumpNote(jumps)` loop fails at
// "there is no second line under the wrote line". Adding one space inside the
// wrote line's format fails at "the wrote line moved", quoting both forms.
func TestCompilePrintsTheJumpNoteUnderItsWroteLine(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "chunk.lua")
	out := captureStdout(t, func() error {
		return runCompile([]string{bigDataGuest(t, "flat", 52000), "-o", dst})
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("there is no second line under the wrote line, so nothing was printed "+
			"about the jump limit:\n%s", out)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("wrote %s (%d bytes, NaN mode: canonical, -opt=3)", dst, st.Size())
	if lines[0] != want {
		t.Errorf("the wrote line moved:\n got %q\nwant %q", lines[0], want)
	}
	if !strings.HasPrefix(lines[1], "  jump span: widest ") {
		t.Errorf("the line under the wrote line is not the jump note: %q", lines[1])
	}
	for _, w := range []string{
		"% of the 655355-byte limit)",
		"station(s)",
		"widest span after the relay",
		"block room",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("the note says nothing about %q:\n%s", w, out)
		}
	}
	t.Logf("%s", out)
}

// TestARefusedModulesJumpRowIsTheRefusedFunctions is the module-level half of
// "the numbers describe the function that stopped the build".
//
// A module's jumps object reports its WIDEST function, which is the right choice
// while the module packages: that is the function nearest Lua's wall and the one
// an author steers by. It is the wrong choice on a refusal, because the widest
// function and the refused one need not be the same -- a 1.9 MB body-level span
// is carried by the relay and a 990 KB one inside a loop body is not -- and a
// refusal that reports the carried function's row says the relay had 327,647
// bytes of room on a build it had just given up on.
//
// The fixture is both shapes in one module with the relayable one first and
// wider. Red-proven by taking the `jumps.Widest = f` line out of the refusal
// path in measureAndRelay, which fails at "the refused module's row is
// $spanner's" and at the room and block assertions under it.
func TestARefusedModulesJumpRowIsTheRefusedFunctions(t *testing.T) {
	doc, out, err := runModReportOut(t, "--data-module", bigDataGuest(t, "both", 30000))
	if err == nil {
		t.Fatalf("a module with an unrelayable span in it was packaged:\n%s", out)
	}
	j := at(t, doc, "jumps", "data").(map[string]any)
	num := func(k string) int { return int(j[k].(float64)) }
	fn, _ := j["widest_span_function"].(string)
	if !strings.Contains(fn, "blocky") {
		t.Errorf("the refused module's row is %q, and the function that refused it is "+
			"$blocky; every number under this name then describes a function that "+
			"compiled", fn)
	}
	if got := num("block_room_bytes"); got >= 0 {
		t.Errorf("block_room_bytes is %+d on a refusal; the room goes negative exactly "+
			"when the relay cannot help", got)
	}
	if block, hop := num("widest_block_bytes"), num("hop_bytes"); block <= hop {
		t.Errorf("the block reads %d against a hop of %d; if it fitted, the module would "+
			"have packaged", block, hop)
	}
	t.Logf("refused on %s: span %d, block %d, hop %d, room %d", fn,
		num("widest_span_bytes"), num("widest_block_bytes"), num("hop_bytes"),
		num("block_room_bytes"))
}

// THE MODULE ROW'S BLOCK IS THE MODULE'S WORST, and the whole-diff review of
// 2026-09-08 is why this test exists: the row took its block and its room from
// the function with the WIDEST SPAN, so a module carrying a 572 KB body-level
// span in one function and a 495 KB loop body in another printed "327,625 bytes
// of block room (100%)", no advisory line, and `block_room_bytes` of +327,625
// in the report -- the relay's ceiling reported from the function nearest
// Lua's. `bigDataGuest(t, "both", 15000)` is that module: $spanner at 40,000
// statements is the widest span and under the limit, $blocky at 15,000 is the
// longest block and cannot be relayed, and nothing is refused because nothing
// has crossed.
//
// Red-proven by selecting Blockiest the way Widest is selected (by span): the
// row names $spanner, block_room_bytes reads +327,625, and the stdout carries
// neither the loop body's block nor the advisory line.
func TestTheModuleRowsBlockIsTheFunctionWithTheLeastRoom(t *testing.T) {
	doc, out, err := runModReportOut(t, "--data-module", bigDataGuest(t, "both", 15000))
	if err != nil {
		t.Fatalf("the fixture was refused, so it is not the case under test: %v\n%s", err, out)
	}
	j := at(t, doc, "jumps", "data").(map[string]any)
	num := func(k string) int { return int(j[k].(float64)) }
	spanFn, _ := j["widest_span_function"].(string)
	blockFn, _ := j["widest_block_function"].(string)
	if !strings.Contains(spanFn, "spanner") {
		t.Fatalf("the widest span is in %q; this fixture needs it in $spanner", spanFn)
	}
	if num("stations") != 0 {
		t.Fatalf("the fixture was relayed (%d station(s)); this test is the no-relay arm", num("stations"))
	}
	if !strings.Contains(blockFn, "blocky") {
		t.Errorf("widest_block_function is %q, and the longest block in the module is "+
			"$blocky's loop body", blockFn)
	}
	room, block, hop := num("block_room_bytes"), num("widest_block_bytes"), num("hop_bytes")
	if room >= 0 || block <= hop {
		t.Errorf("block_room_bytes is %+d and widest_block_bytes %d against a hop of %d; "+
			"the row is reading the widest-span function's block, and the module "+
			"holds a loop body the relay could not carry", room, block, hop)
	}
	for _, w := range []string{
		"no relay needed",
		fmt.Sprintf("widest block %d bytes in %s against", block, blockFn),
		strconv.Itoa(room),
		"the relay could NOT carry it if it crossed: the widest run in " + blockFn,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("the stdout says nothing about %q:\n%s", w, out)
		}
	}
}

// THE SAME ON THE RELAYED ARM. A module the relay carried can still hold a
// loop body it could not carry, in a function that has not crossed the limit
// yet; that module packages, its second line gives the general advice, and it
// used to stop there. `relayed-and-blocky` at 30,000 is $spanner at 80,000
// statements (1.12 MB, relayed) beside $blocky at 15,000 (495 KB, room
// -167,361). Red-proven by dropping the third line, which fails at "the stdout
// says nothing about \"could NOT carry\"".
func TestARelayedModuleStillSaysWhatTheRelayCannotCarry(t *testing.T) {
	doc, out, err := runModReportOut(t, "--data-module", bigDataGuest(t, "relayed-and-blocky", 30000))
	if err != nil {
		t.Fatalf("the fixture was refused, so it is not the case under test: %v\n%s", err, out)
	}
	j := at(t, doc, "jumps", "data").(map[string]any)
	num := func(k string) int { return int(j[k].(float64)) }
	if num("stations") == 0 || j["relayed"] != true {
		t.Fatalf("the fixture was not relayed; this test is the relayed arm")
	}
	blockFn, _ := j["widest_block_function"].(string)
	if !strings.Contains(blockFn, "blocky") {
		t.Errorf("widest_block_function is %q, want $blocky", blockFn)
	}
	if room := num("block_room_bytes"); room >= 0 {
		t.Errorf("block_room_bytes is %+d on a module holding a loop body longer than a hop", room)
	}
	for _, w := range []string{
		"relayed through",
		"the number that bounds it is the block room and not the span",
		"and the relay could NOT carry " + blockFn + " if its widest span crossed",
		"the widest run in " + blockFn + " with nowhere at body level",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("the stdout says nothing about %q:\n%s", w, out)
		}
	}
}
