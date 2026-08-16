package spectest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luahost"
)

func TestLuaArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []Value
		want string
		ok   bool
	}{
		{"none", nil, "", true},
		{"one", []Value{{Type: "i32", Value: "42"}}, ", 42", true},
		{"two", []Value{{Type: "i32", Value: "1"}, {Type: "i32", Value: "4294967295"}},
			", 1, 4294967295", true},
		{"i64 expands to a (lo, hi) pair", []Value{{Type: "i64", Value: "4294967296"}}, ", 0, 1", true},
		{"f32 accepted", []Value{{Type: "f32", Value: "1065353216"}}, ", 0x1p+00", true},
		{"out of u32 range", []Value{{Type: "i32", Value: "4294967296"}}, "", false},
		{"non-numeric i32", []Value{{Type: "i32", Value: "nan:canonical"}}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := luaArgs(tc.in, Options{})
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestExpectedCheck(t *testing.T) {
	if c, ok := expectedCheck2([]Value{{Type: "i32", Value: "7"}}); !ok || c.Expr != "r == 7" {
		t.Errorf("got (%q, %v), want (r == 7, true)", c.Expr, ok)
	}
	// Multi-value results and the valueless form used by assert_trap must be
	// reported as unhandled rather than guessed at.
	for _, tc := range [][]Value{
		{},
		{{Type: "i32"}},
		{{Type: "i32", Value: "1"}, {Type: "i32", Value: "2"}},
	} {
		if _, ok := expectedCheck2(tc); ok {
			t.Errorf("expectedCheck(%v) should report unhandled", tc)
		}
	}
}

// The spec permits a range of NaN payloads for arithmetic results, so NaN is
// compared by CLASS. Everything else is compared BITWISE, which is what
// distinguishes -0.0 from +0.0 -- a plain == would call them equal.
func TestExpectedCheckFloats(t *testing.T) {
	for _, nan := range []string{"nan:canonical", "nan:arithmetic"} {
		c, ok := expectedCheck2([]Value{{Type: "f32", Value: nan}})
		if !ok || !strings.Contains(c.Expr, "r ~= r") {
			t.Errorf("%s should compare by class, got %q", nan, c.Expr)
		}
	}
	// 0 and 0x80000000 are +0.0 and -0.0; their checks must differ.
	pos, _ := expectedCheck2([]Value{{Type: "f32", Value: "0"}})
	neg, _ := expectedCheck2([]Value{{Type: "f32", Value: "2147483648"}})
	if pos.Expr == neg.Expr {
		t.Error("+0.0 and -0.0 must not produce the same check")
	}
	if c, ok := expectedCheck2([]Value{{Type: "f64", Value: "4607182418800017408"}}); !ok ||
		!strings.Contains(c.Expr, "f64_to_bits") {
		t.Errorf("f64 should compare bitwise, got %q", c.Expr)
	}
}

// wast2json writes numbers as raw bit patterns. Floats must be decoded and
// re-emitted as exact hex-float literals; decimal would round twice.
func TestLuaLiteral(t *testing.T) {
	tests := []struct {
		v    Value
		want string
	}{
		{Value{Type: "i32", Value: "42"}, "42"},
		{Value{Type: "f32", Value: "0"}, "0x0p+00"},
		{Value{Type: "f32", Value: "2147483648"}, "(-0.0)"},
		{Value{Type: "f32", Value: "2139095040"}, "(1/0)"},  // +inf
		{Value{Type: "f32", Value: "4286578688"}, "(-1/0)"}, // -inf
		{Value{Type: "f32", Value: "2143289344"}, "(0/0)"},  // NaN
		{Value{Type: "f64", Value: "4607182418800017408"}, "0x1p+00"},
		{Value{Type: "i64", Value: "1"}, "1, 0"},
	}
	for _, tc := range tests {
		got, ok := luaLiteral(tc.v, Options{})
		if !ok {
			t.Errorf("%v: reported unsupported", tc.v)
			continue
		}
		if got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.v, got, tc.want)
		}
	}
	// An i64 crosses the boundary as two Lua values.
	if got, ok := luaLiteral(Value{Type: "i64", Value: "4294967296"}, Options{}); !ok || got != "0, 1" {
		t.Errorf("i64 should expand to a (lo, hi) pair, got (%q, %v)", got, ok)
	}
}

func TestPassRate(t *testing.T) {
	o := &Outcome{Passed: 9, Failed: 1, Skipped: 100}
	// Skipped assertions are excluded: a suite that skips everything should not
	// report 100%.
	if got := o.PassRate(); got != 0.9 {
		t.Errorf("PassRate = %v, want 0.9 (skips must not count)", got)
	}
	if got := (&Outcome{}).PassRate(); got != 0 {
		t.Errorf("empty outcome should be 0, got %v", got)
	}
}

func TestTotals(t *testing.T) {
	outs := []*Outcome{
		{Total: 10, Passed: 8, Failed: 1, Skipped: 1},
		{Total: 5, Passed: 5},
	}
	total, passed, failed, skipped := Totals(outs)
	if total != 15 || passed != 13 || failed != 1 || skipped != 1 {
		t.Errorf("got (%d,%d,%d,%d), want (15,13,1,1)", total, passed, failed, skipped)
	}
}

// An end-to-end run over a hand-built corpus, including a case that must FAIL.
// A harness that cannot report failure is worse than no harness, so this pins
// both directions.
func TestRunFileDetectsFailures(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	dir := t.TempDir()
	wasm := compileTestModule(t, `(module
		(func (export "add") (param i32) (param i32) (result i32)
			(i32.add (local.get 0) (local.get 1)))
		(func (export "divs") (param i32) (param i32) (result i32)
			(i32.div_s (local.get 0) (local.get 1))))`)
	if err := os.WriteFile(filepath.Join(dir, "t.0.wasm"), wasm, 0o644); err != nil {
		t.Fatal(err)
	}

	sc := script{
		SourceFilename: "t.wast",
		Commands: []Command{
			{Type: "module", Line: 1, Filename: "t.0.wasm"},
			{Type: "assert_return", Line: 10,
				Action:   &Action{Type: "invoke", Field: "add", Args: []Value{{Type: "i32", Value: "1"}, {Type: "i32", Value: "2"}}},
				Expected: []Value{{Type: "i32", Value: "3"}}},
			{Type: "assert_return", Line: 11, // deliberately wrong
				Action:   &Action{Type: "invoke", Field: "add", Args: []Value{{Type: "i32", Value: "1"}, {Type: "i32", Value: "2"}}},
				Expected: []Value{{Type: "i32", Value: "4"}}},
			{Type: "assert_trap", Line: 12, Text: "integer divide by zero",
				Action: &Action{Type: "invoke", Field: "divs", Args: []Value{{Type: "i32", Value: "1"}, {Type: "i32", Value: "0"}}}},
			{Type: "assert_return", Line: 13, // missing export
				Action:   &Action{Type: "invoke", Field: "nope", Args: nil},
				Expected: []Value{{Type: "i32", Value: "0"}}},
		},
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "t.json")
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RunFile(h, jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 4 {
		t.Errorf("Total = %d, want 4", out.Total)
	}
	if out.Passed != 2 {
		t.Errorf("Passed = %d, want 2 (the correct add and the trap)", out.Passed)
	}
	if out.Failed != 2 {
		t.Errorf("Failed = %d, want 2 (wrong expectation and missing export)", out.Failed)
	}
	joined := strings.Join(out.Failures, "\n")
	if !strings.Contains(joined, "got 3 want 4") {
		t.Errorf("a wrong result should report both values, got:\n%s", joined)
	}
	if !strings.Contains(joined, "no such export") {
		t.Errorf("a missing export should be reported, got:\n%s", joined)
	}
}

// assert_invalid passes when the module is rejected; it must FAIL when a
// genuinely invalid module slips through.
func TestAssertInvalidRequiresRejection(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()

	// A perfectly valid module, but the script claims it should be rejected.
	valid := compileTestModule(t, `(module (func (export "f") (result i32) (i32.const 1)))`)
	if err := os.WriteFile(filepath.Join(dir, "bad.0.wasm"), valid, 0o644); err != nil {
		t.Fatal(err)
	}
	sc := script{Commands: []Command{
		{Type: "assert_invalid", Line: 5, Filename: "bad.0.wasm",
			Text: "type mismatch", ModuleType: "binary"},
	}}
	raw, _ := json.Marshal(sc)
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RunFile(h, p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Errorf("accepting a module that should be rejected must fail; got %+v", out)
	}
}

// An uncompiled function must yield SKIP, never PASS: an unimplemented feature
// reporting success is the single most dangerous failure mode a conformance
// harness can have.
func TestUnsupportedFunctionSkipsRatherThanPasses(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()

	// Result type is i32 so the harness can express the assertion, but the body
	// uses table.copy, which is not implemented. This exercises the generated
	// stub -- an unsupported feature must SKIP, never silently pass.
	mod := compileTestModule(t, `(module
		(memory 1)
		(table 2 funcref)
		(func (export "ok") (result i32) (i32.const 1))
		(func (export "wide") (result i32)
			(table.copy (i32.const 0) (i32.const 1) (i32.const 1))
			(i32.const 1)))`)
	if err := os.WriteFile(filepath.Join(dir, "s.0.wasm"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
	sc := script{Commands: []Command{
		{Type: "module", Line: 1, Filename: "s.0.wasm"},
		{Type: "assert_return", Line: 2,
			Action:   &Action{Type: "invoke", Field: "ok"},
			Expected: []Value{{Type: "i32", Value: "1"}}},
		{Type: "assert_return", Line: 3,
			Action:   &Action{Type: "invoke", Field: "wide"},
			Expected: []Value{{Type: "i32", Value: "1"}}},
	}}
	raw, _ := json.Marshal(sc)
	p := filepath.Join(dir, "s.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RunFile(h, p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Passed != 1 {
		t.Errorf("Passed = %d, want 1 (the supported function still works)", out.Passed)
	}
	if out.Failed != 0 {
		t.Errorf("Failed = %d, want 0; an unimplemented feature is not a failure: %v",
			out.Failed, out.Failures)
	}
	if out.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", out.Skipped)
	}
	if len(out.Skips) == 0 || !strings.Contains(out.Skips[0], "table.copy") {
		t.Errorf("the skip reason should name the unsupported feature: %v", out.Skips)
	}
}

// A module-level failure (as opposed to a per-function one) still skips
// everything that depended on it.
func TestUncompilableModuleSkipsRatherThanPasses(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()

	// A module-level failure, as opposed to a per-function one: multiple
	// memories are refused outright.
	mod := compileTestModule(t, `(module (memory 1) (memory 1)
		(func (export "f") (result i32) (i32.const 1)))`)
	if err := os.WriteFile(filepath.Join(dir, "s.0.wasm"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
	sc := script{Commands: []Command{
		{Type: "module", Line: 1, Filename: "s.0.wasm"},
		{Type: "assert_return", Line: 2,
			Action:   &Action{Type: "invoke", Field: "f"},
			Expected: []Value{{Type: "i32", Value: "1"}}},
	}}
	raw, _ := json.Marshal(sc)
	p := filepath.Join(dir, "s.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RunFile(h, p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Passed != 0 {
		t.Errorf("a module that will not compile must not report a pass, got %d", out.Passed)
	}
	if out.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", out.Skipped)
	}
}

// expectedCheck2 is expectedCheck with default options, so the tests read
// without threading an empty Options through every call.
func expectedCheck2(vals []Value) (check, bool) { return expectedCheck(vals, Options{}) }
