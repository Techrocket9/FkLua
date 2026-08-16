package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

func mod(t *testing.T, wat string) *ir.Module {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	return im
}

func TestParseNaNMode(t *testing.T) {
	for in, want := range map[string]NaNMode{
		"": NaNCanonical, "canonical": NaNCanonical, "fast": NaNCanonical,
		"exact": NaNExact, "strict": NaNExact,
	} {
		got, err := ParseNaNMode(in)
		if err != nil || got != want {
			t.Errorf("ParseNaNMode(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	if _, err := ParseNaNMode("wobbly"); err == nil {
		t.Error("an unknown mode should be rejected")
	}
	if NaNExact.String() != "exact" || NaNCanonical.String() != "canonical" {
		t.Error("mode names are used in output and must be stable")
	}
}

// The point of the diagnostics is that an author is TOLD, at compile time,
// rather than discovering it from a mismatched result later.
func TestDiagnoseFindsNaNSensitiveOps(t *testing.T) {
	cases := map[string]struct{ wat, op string }{
		"copysign": {`(module (func $c (export "f") (param f32) (param f32) (result f32)
			(f32.copysign (local.get 0) (local.get 1))))`, "f32.copysign"},
		"reinterpret": {`(module (func $r (export "f") (param f32) (result i32)
			(i32.reinterpret_f32 (local.get 0))))`, "i32.reinterpret_f32"},
		"f64 load": {`(module (memory 1) (func $l (export "f") (result f64)
			(f64.load (i32.const 0))))`, "f64.load"},
		"f32 store": {`(module (memory 1) (func $s (export "f") (param f32)
			(f32.store (i32.const 0) (local.get 0))))`, "f32.store"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ds := Diagnose(mod(t, tc.wat), Options{})
			if len(ds) != 1 {
				t.Fatalf("expected 1 diagnostic, got %d: %v", len(ds), ds)
			}
			if ds[0].Op != tc.op {
				t.Errorf("Op = %q, want %q", ds[0].Op, tc.op)
			}
			if ds[0].Detail == "" || ds[0].Remedy == "" {
				t.Error("a diagnostic must say what is lost and what to do")
			}
			if !strings.Contains(ds[0].Remedy, "--nan=exact") {
				t.Errorf("the remedy should name the flag: %q", ds[0].Remedy)
			}
		})
	}
}

// Ordinary arithmetic never observes a NaN's bits, so reporting it would be
// noise that trains people to ignore the output.
func TestDiagnoseIgnoresSafeOps(t *testing.T) {
	safe := []string{
		`(module (func (export "f") (param f64) (param f64) (result f64)
			(f64.add (local.get 0) (local.get 1))))`,
		`(module (func (export "f") (param f32) (param f32) (result i32)
			(f32.lt (local.get 0) (local.get 1))))`,
		`(module (func (export "f") (param f64) (result f64) (f64.sqrt (local.get 0))))`,
		`(module (func (export "f") (param i32) (result i32)
			(i32.add (local.get 0) (i32.const 1))))`,
		`(module (memory 1) (func (export "f") (param i32) (result i32)
			(i32.load (local.get 0))))`,
	}
	for _, wat := range safe {
		if ds := Diagnose(mod(t, wat), Options{}); len(ds) != 0 {
			t.Errorf("unexpected diagnostics for a safe module: %v", ds)
		}
	}
}

// Exact mode makes those operations faithful, so there is nothing left to warn
// about -- reporting anyway would imply the mode had not worked.
func TestDiagnoseSilentInExactMode(t *testing.T) {
	m := mod(t, `(module (func (export "f") (param f32) (param f32) (result f32)
		(f32.copysign (local.get 0) (local.get 1))))`)
	if ds := Diagnose(m, Options{NaN: NaNExact}); len(ds) != 0 {
		t.Errorf("exact mode should have nothing to report, got %v", ds)
	}
}

func TestDiagnoseCountsRepeats(t *testing.T) {
	m := mod(t, `(module (func $c (export "f") (param f32) (param f32) (result f32)
		(f32.copysign
			(f32.copysign (local.get 0) (local.get 1))
			(f32.copysign (local.get 1) (local.get 0)))))`)
	ds := Diagnose(m, Options{})
	if len(ds) != 1 {
		t.Fatalf("repeats of one op should collapse into one diagnostic, got %d", len(ds))
	}
	if ds[0].Count != 3 {
		t.Errorf("Count = %d, want 3", ds[0].Count)
	}
	if !strings.Contains(ds[0].String(), "3 times") {
		t.Errorf("the repeat count should be visible: %q", ds[0].String())
	}
}

func TestFormatDiagnostics(t *testing.T) {
	if FormatDiagnostics(nil) != "" {
		t.Error("no diagnostics should produce no output at all")
	}
	out := FormatDiagnostics([]Diagnostic{
		{Func: "f", Op: "f32.copysign", Count: 1, Detail: "d", Remedy: "r"},
	})
	for _, want := range []string{"f32.copysign", "--nan=exact"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
}

// Exact mode must not leave a plain Lua operator anywhere a boxed operand can
// reach, or the generated code raises "attempt to compare table with number".
func TestExactModeRoutesFloatOpsThroughHelpers(t *testing.T) {
	src, err := EmitModuleWith(mod(t, `(module
		(func (export "a") (param f64) (param f64) (result f64) (f64.add (local.get 0) (local.get 1)))
		(func (export "l") (param f64) (param f64) (result i32) (f64.lt (local.get 0) (local.get 1)))
		(func (export "n") (param f32) (result f32) (f32.neg (local.get 0)))
		(func (export "c") (param f32) (param f32) (result f32) (f32.copysign (local.get 0) (local.get 1))))`),
		Options{NaN: NaNExact})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"xadd(", "xlt(", "xneg(", "xcopysign("} {
		if !strings.Contains(src, want) {
			t.Errorf("exact mode should emit %s:\n%s", want, src)
		}
	}
	if !strings.Contains(src, `nan_mode = "exact"`) {
		t.Error("the module should record which mode it was built in")
	}
	if !strings.Contains(src, "boxf32 = boxf32") {
		t.Error("box constructors must be exported so a host can pass a NaN in")
	}
}

// Only a NON-canonical NaN needs boxing; boxing every float constant would slow
// the mode down for no benefit.
func TestExactModeBoxesOnlyNonCanonicalNaNConstants(t *testing.T) {
	src, err := EmitModuleWith(mod(t, `(module
		(func (export "p") (result f32) (f32.const nan:0x200000))
		(func (export "q") (result f32) (f32.const 1.5)))`), Options{NaN: NaNExact})
	if err != nil {
		t.Fatal(err)
	}
	// Count only in the generated functions: the prelude defines and uses
	// boxf32 itself.
	gen := src[strings.Index(src, "local F = {}"):]
	if !strings.Contains(gen, "boxf32(") {
		t.Errorf("a payload-carrying NaN constant should be boxed:\n%s", gen)
	}
	// One in function p, plus the export in the rt table.
	if n := strings.Count(gen, "boxf32("); n > 2 {
		t.Errorf("only the NaN constant should be boxed, saw %d:\n%s", n, gen)
	}
}

func TestCanonicalModeUsesPlainOperators(t *testing.T) {
	src, err := EmitModuleWith(mod(t, `(module (func (export "f") (param f64) (param f64) (result f64)
		(f64.add (local.get 0) (local.get 1))))`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(functionBody(src, "f"), "xadd(") {
		t.Errorf("canonical mode should use a plain operator:\n%s", src)
	}
}

func TestIsNonCanonicalNaN(t *testing.T) {
	if isNonCanonicalNaN32(0x7FC00000) {
		t.Error("the canonical f32 NaN needs no box")
	}
	if !isNonCanonicalNaN32(0x7FA00000) {
		t.Error("a payload-carrying f32 NaN needs a box")
	}
	if isNonCanonicalNaN32(0x3F800000) {
		t.Error("1.0 is not a NaN")
	}
	if isNonCanonicalNaN64(0x7FF8000000000000) {
		t.Error("the canonical f64 NaN needs no box")
	}
	if !isNonCanonicalNaN64(0x7FF4000000000001) {
		t.Error("a payload-carrying f64 NaN needs a box")
	}
}

// A diagnostic about code the guest never runs is worse than no diagnostic, so
// unreachable functions are not reported.
func TestDiagnoseIgnoresUnreachableCode(t *testing.T) {
	// $dead does the NaN-sensitive thing but nothing calls it.
	m := mod(t, `(module
		(func $dead (param f32) (param f32) (result f32)
			(f32.copysign (local.get 0) (local.get 1)))
		(func (export "live") (param i32) (result i32)
			(i32.add (local.get 0) (i32.const 1))))`)
	if ds := Diagnose(m, Options{}); len(ds) != 0 {
		t.Errorf("an unreachable function should not be reported, got %v", ds)
	}
}

// Reachable through a direct call from an export, so it must still be reported:
// the caller cannot see the operation, but its results depend on it.
func TestDiagnoseFollowsDirectCalls(t *testing.T) {
	m := mod(t, `(module
		(func $helper (param f32) (param f32) (result f32)
			(f32.copysign (local.get 0) (local.get 1)))
		(func (export "live") (param f32) (param f32) (result f32)
			(call $helper (local.get 0) (local.get 1))))`)
	ds := Diagnose(m, Options{})
	if len(ds) != 1 || ds[0].Func != "$helper" {
		t.Errorf("a callee of an export must be reported, got %v", ds)
	}
}

// The start function is a root as much as an export is: it runs at load whether
// or not anything else references it.
func TestDiagnoseTreatsStartAsARoot(t *testing.T) {
	m := mod(t, `(module
		(global $g (mut i32) (i32.const 0))
		(func $init (global.set $g (i32.reinterpret_f32 (f32.const 1))))
		(start $init)
		(func (export "get") (result i32) (global.get $g)))`)
	if ds := Diagnose(m, Options{}); len(ds) != 1 {
		t.Errorf("the start function should be a reachability root, got %v", ds)
	}
}

// Which table entry a call_indirect selects is a runtime value, so every entry
// has to be assumed reachable once a module contains one.
func TestDiagnoseIsConservativeAboutCallIndirect(t *testing.T) {
	m := mod(t, `(module
		(type $t (func (param f32) (param f32) (result f32)))
		(table 1 funcref)
		(elem (i32.const 0) $viaTable)
		(func $viaTable (type $t)
			(f32.copysign (local.get 0) (local.get 1)))
		(func (export "live") (param i32) (param f32) (param f32) (result f32)
			(call_indirect (type $t) (local.get 1) (local.get 2) (local.get 0))))`)
	ds := Diagnose(m, Options{})
	if len(ds) != 1 || ds[0].Func != "$viaTable" {
		t.Errorf("a table entry must be assumed reachable, got %v", ds)
	}
}

// Naming the entry point is what makes a diagnostic actionable: the author can
// answer "which of my hooks is affected?" but not "what is fmaximumf?".
func TestDiagnoseNamesTheReachingExport(t *testing.T) {
	m := mod(t, `(module
		(func $helper (param f32) (param f32) (result f32)
			(f32.copysign (local.get 0) (local.get 1)))
		(func (export "on_tick") (param f32) (param f32) (result f32)
			(call $helper (local.get 0) (local.get 1))))`)
	ds := Diagnose(m, Options{})
	if len(ds) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", ds)
	}
	if len(ds[0].ReachedFrom) != 1 || ds[0].ReachedFrom[0] != `export "on_tick"` {
		t.Errorf("ReachedFrom = %v, want [export \"on_tick\"]", ds[0].ReachedFrom)
	}
	if !strings.Contains(ds[0].String(), `reached from export "on_tick"`) {
		t.Errorf("the message should name the entry point: %q", ds[0].String())
	}
}

// Options.Roots is what lets `fklua mod` stay quiet about TinyGo's exported
// libm: a mod's control.lua wires only factorio.Hooks, so nothing else is an
// entry point. A bare compile has no such luxury and must assume every export.
func TestRootsRestrictEntryPoints(t *testing.T) {
	src := `(module
		(func (export "fmaximumf") (param f32) (param f32) (result f32)
			(f32.copysign (local.get 0) (local.get 1)))
		(func (export "fk_on_tick") (param i32) (result i32)
			(i32.add (local.get 0) (i32.const 1))))`

	// Bare compile: fmaximumf is exported, so a host could call it.
	if ds := Diagnose(mod(t, src), Options{}); len(ds) != 1 {
		t.Errorf("with no root restriction every export counts, got %v", ds)
	}
	// Packaged as a mod: only the wired hook is an entry point.
	if ds := Diagnose(mod(t, src), Options{Roots: []string{"fk_on_tick"}}); len(ds) != 0 {
		t.Errorf("an unwired export is not an entry point, got %v", ds)
	}
}

// A helper reached from many hooks should not bury the finding under a list.
func TestReachedFromPhraseIsTrimmed(t *testing.T) {
	cases := map[int]string{
		0: "",
		1: `, reached from export "a"`,
		2: `, reached from export "a" and export "b"`,
		4: `, reached from export "a" and 3 other entry points`,
	}
	for n, want := range cases {
		var from []string
		for i := 0; i < n; i++ {
			from = append(from, `export "`+string(rune('a'+i))+`"`)
		}
		got := Diagnostic{ReachedFrom: from}.reachedFromPhrase()
		if got != want {
			t.Errorf("n=%d: got %q, want %q", n, got, want)
		}
	}
}
