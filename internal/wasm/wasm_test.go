package wasm

import (
	"errors"
	"strings"
	"testing"

	"github.com/eliben/watgo"
	"github.com/eliben/watgo/wasmir"
)

// compileWAT produces binary wasm so tests can exercise Decode() without
// checking .wasm blobs into the repo.
func compileWAT(src string) ([]byte, error) {
	return watgo.CompileWATToWASM([]byte(src))
}

func TestDecodeWATSimple(t *testing.T) {
	m, err := DecodeWAT(`(module
		(func (export "add") (param $x i32) (param $y i32) (result i32)
			(i32.add (local.get $x) (local.get $y))))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Funcs) != 1 {
		t.Fatalf("expected 1 function, got %d", len(m.Funcs))
	}
	f := m.Funcs[0]
	if got := f.Type.String(); got != "(i32, i32) -> (i32)" {
		t.Errorf("type = %s", got)
	}
	want := []Op{OpLocalGet, OpLocalGet, OpI32Add, OpEnd}
	if len(f.Body) != len(want) {
		t.Fatalf("body has %d instructions, want %d: %v", len(f.Body), len(want), f.Body)
	}
	for i, op := range want {
		if f.Body[i].Op != op {
			t.Errorf("body[%d] = %v, want %v", i, f.Body[i].Op, op)
		}
	}
	if f.Body[0].LocalIndex != 0 || f.Body[1].LocalIndex != 1 {
		t.Errorf("local indices = %d, %d; want 0, 1",
			f.Body[0].LocalIndex, f.Body[1].LocalIndex)
	}
}

func TestExportLookup(t *testing.T) {
	m, err := DecodeWAT(`(module
		(func (export "a") (result i32) (i32.const 1))
		(func (export "b") (result i32) (i32.const 2)))`)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := m.FuncByExport("b")
	if !ok {
		t.Fatal("export b not found")
	}
	if f.Body[0].I32 != 2 {
		t.Errorf("wrong function resolved: const = %d, want 2", f.Body[0].I32)
	}
	if _, ok := m.FuncByExport("nope"); ok {
		t.Error("FuncByExport should report missing exports")
	}
}

// Invariant A: an i32 is an unsigned value in [0, 2^32). A negative constant in
// the source must arrive as its two's-complement bit pattern, because every
// downstream lowering assumes unsigned.
func TestI32ConstIsUnsigned(t *testing.T) {
	tests := []struct {
		src  string
		want uint32
	}{
		{"(i32.const 0)", 0},
		{"(i32.const 1)", 1},
		{"(i32.const -1)", 0xFFFFFFFF},
		{"(i32.const -2147483648)", 0x80000000},
		{"(i32.const 2147483647)", 0x7FFFFFFF},
	}
	for _, tc := range tests {
		m, err := DecodeWAT(`(module (func (export "f") (result i32) ` + tc.src + `))`)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got := m.Funcs[0].Body[0].I32; got != tc.want {
			t.Errorf("%s: got %#x, want %#x", tc.src, got, tc.want)
		}
	}
}

// The diagnostics are the reason this package exists instead of using watgo
// directly. Each case asserts the message names the instruction, the function,
// and when it will work.
//
// Since M2 an unsupported instruction disables only its own function: the
// module still decodes, and Func.Unsupported carries the reason. Failing the
// whole module would mean one i64 helper zeroes an entire spec file.
func TestUnsupportedDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantOp   string
		wantMile string
		wantText []string
	}{
		// The segment-indexed half of bulk memory is UNSCHEDULED, not pending:
		// memory.copy and memory.fill compile natively, and nothing a guest
		// toolchain emits has been seen reaching for the rest. It carried a
		// milestone label for two milestones after that milestone shipped,
		// which reads as work someone is doing.
		{
			name:     "bulk memory",
			src:      `(module (memory 1) (table 2 funcref) (func $bulk (export "f") (table.copy (i32.const 0) (i32.const 1) (i32.const 1))))`,
			wantOp:   "table.copy",
			wantMile: "",
			wantText: []string{"bulk memory", "unscheduled"},
		},
		{
			name:     "table copy",
			src:      `(module (table 1 funcref) (func $tc (export "f") (table.copy (i32.const 0) (i32.const 0) (i32.const 0))))`,
			wantOp:   "table.copy",
			wantMile: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := DecodeWAT(tc.src)
			if err != nil {
				t.Fatalf("the module should still decode: %v", err)
			}
			if len(m.Funcs) != 1 {
				t.Fatalf("expected 1 function, got %d", len(m.Funcs))
			}
			err = m.Funcs[0].Unsupported
			if err == nil {
				t.Fatal("expected the function to be marked unsupported")
			}
			msg := err.Error()
			for _, want := range tc.wantText {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q should mention %q", msg, want)
				}
			}
			if !strings.Contains(msg, tc.wantOp) {
				t.Errorf("message %q should name the offending type or instruction %q",
					msg, tc.wantOp)
			}
			var ue *UnsupportedError
			if errors.As(err, &ue) && ue.Planned != tc.wantMile {
				t.Errorf("Planned = %q, want %q", ue.Planned, tc.wantMile)
			}
		})
	}
}

// A module must keep working around an unsupported function, or incremental
// milestones show no progress at all.
func TestUnsupportedFunctionDoesNotPoisonTheModule(t *testing.T) {
	m, err := DecodeWAT(`(module
		(memory 1)
		(table 2 funcref)
		(func $ok (export "ok") (param i32) (result i32) (i32.add (local.get 0) (i32.const 1)))
		(func $wide (export "wide") (table.copy (i32.const 0) (i32.const 1) (i32.const 1))))`)
	if err != nil {
		t.Fatalf("module should decode: %v", err)
	}
	okFn, found := m.FuncByExport("ok")
	if !found {
		t.Fatal("export ok missing")
	}
	if okFn.Unsupported != nil {
		t.Errorf("the supported function should compile: %v", okFn.Unsupported)
	}
	if len(okFn.Body) == 0 {
		t.Error("the supported function should have a body")
	}
	wide, _ := m.FuncByExport("wide")
	if wide.Unsupported == nil {
		t.Error("the i64 function should be marked unsupported")
	}
	if len(wide.Body) != 0 {
		t.Error("an unsupported function should carry no body")
	}
}

// Opcodes that landed in M2 must no longer be reported as unsupported.
// Opcodes that landed in M2 and M3a must no longer report as unsupported.
func TestM2OpcodesAreSupported(t *testing.T) {
	srcs := map[string]string{
		"block":          `(module (func (export "f") (result i32) (block (result i32) (i32.const 1))))`,
		"loop":           `(module (func (export "f") (result i32) (loop (result i32) (i32.const 1))))`,
		"if":             `(module (func (export "f") (param i32) (result i32) (if (result i32) (local.get 0) (then (i32.const 1)) (else (i32.const 2)))))`,
		"br_table":       `(module (func (export "f") (param i32) (result i32) (block (block (br_table 0 1 (local.get 0))) (return (i32.const 1))) (i32.const 2)))`,
		"call":           `(module (func $c (result i32) (i32.const 1)) (func (export "f") (result i32) (call $c)))`,
		"global":         `(module (global $g (mut i32) (i32.const 7)) (func (export "f") (result i32) (global.get $g)))`,
		"memory load":    `(module (memory 1) (func (export "f") (result i32) (i32.load (i32.const 0))))`,
		"memory store":   `(module (memory 1) (func (export "f") (i32.store (i32.const 0) (i32.const 1))))`,
		"memory.size":    `(module (memory 1) (func (export "f") (result i32) (memory.size)))`,
		"f64 arithmetic": `(module (func (export "f") (result f64) (f64.add (f64.const 1) (f64.const 2))))`,
		"i64 arithmetic": `(module (func (export "f") (result i64) (i64.add (i64.const 1) (i64.const 2))))`,
		"i64 memory":     `(module (memory 1) (func (export "f") (result i64) (i64.load (i32.const 0))))`,
		"f32 conversion": `(module (func (export "f") (param f32) (result i32) (i32.trunc_f32_s (local.get 0))))`,
		"f64 memory":     `(module (memory 1) (func (export "f") (result f64) (f64.load (i32.const 0))))`,
		"select":         `(module (func (export "f") (param i32) (result i32) (select (i32.const 1) (i32.const 2) (local.get 0))))`,
		"call_indirect":  `(module (table 1 funcref) (type $t (func (result i32))) (func (export "f") (param i32) (result i32) (call_indirect (type $t) (local.get 0))))`,
	}
	for name, src := range srcs {
		t.Run(name, func(t *testing.T) {
			m, err := DecodeWAT(src)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, f := range m.Funcs {
				if f.Unsupported != nil {
					t.Errorf("%s should be supported at M2: %v", name, f.Unsupported)
				}
			}
		})
	}
}

// Things we will never support should say so, rather than implying a wait.
func TestNotPlannedDiagnostics(t *testing.T) {
	for _, op := range []string{"v128.const", "i32x4.add", "ref.null", "struct.new"} {
		ms, detail := milestoneFor(op)
		if ms != "" {
			t.Errorf("%s: reported as planned for %s, but it is not on the roadmap", op, ms)
		}
		if detail == "" {
			t.Errorf("%s: no explanation of why it is unsupported", op)
		}
	}
	e := &UnsupportedError{Op: "i32x4.add", Func: "f", Offset: 3}
	if !strings.Contains(e.Error(), "not planned") {
		t.Errorf("message should say 'not planned': %q", e.Error())
	}
}

// Imported functions occupy the function index space BEFORE module-defined
// ones, so a definition's index is its position plus the import count. Getting
// this wrong does not fail loudly -- it calls the wrong function -- which is why
// it is pinned here rather than left to the end-to-end test to notice.
func TestImportsShiftTheFunctionIndexSpace(t *testing.T) {
	m, err := DecodeWAT(`(module
		(import "env" "log" (func $log (param i32)))
		(import "env" "tick" (func $tick (result i32)))
		(func $one (result i32) (i32.const 1))
		(func $two (result i32) (call $one))
		(export "two" (func $two)))`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(m.Imports) != 2 {
		t.Fatalf("got %d imports, want 2", len(m.Imports))
	}
	if m.Imports[0].Module != "env" || m.Imports[0].Name != "log" {
		t.Errorf("import 0 is %q.%q, want env.log", m.Imports[0].Module, m.Imports[0].Name)
	}
	if got := m.Imports[1].Type.String(); got != "() -> (i32)" {
		t.Errorf("env.tick has type %s, want () -> (i32)", got)
	}

	// $one is the third entry in the function index space, not the first.
	if m.Funcs[0].Index != 2 {
		t.Errorf("first defined function has index %d, want 2", m.Funcs[0].Index)
	}
	if m.NumFuncs() != 4 {
		t.Errorf("NumFuncs is %d, want 4", m.NumFuncs())
	}

	// A call immediate is read against imports and definitions together.
	ft, ok := m.FuncTypeAt(2)
	if !ok || ft.String() != "() -> (i32)" {
		t.Errorf("FuncTypeAt(2) = %v, %v; want the type of $one", ft, ok)
	}
	if ft, ok := m.FuncTypeAt(0); !ok || ft.String() != "(i32) -> ()" {
		t.Errorf("FuncTypeAt(0) = %v, %v; want the type of the imported env.log", ft, ok)
	}
	if _, ok := m.FuncTypeAt(4); ok {
		t.Error("FuncTypeAt(4) resolved, but the index space holds 4 entries (0..3)")
	}
}

// An imported memory, table or global would shift an index space the emitter
// numbers for itself. Ignoring one produces a module that loads and silently
// reads the wrong global, so it is refused instead.
func TestNonFunctionImportsRefused(t *testing.T) {
	for _, tc := range []struct{ name, decl, kind string }{
		{"global", `(import "env" "g" (global i32))`, "global"},
		{"memory", `(import "env" "m" (memory 1))`, "memory"},
		{"table", `(import "env" "t" (table 1 funcref))`, "table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeWAT(`(module ` + tc.decl + `
				(func (export "f") (result i32) (i32.const 1)))`)
			if err == nil {
				t.Fatalf("expected an imported %s to be refused", tc.kind)
			}
			if !strings.Contains(err.Error(), tc.kind) {
				t.Errorf("message %q should name the kind %q", err.Error(), tc.kind)
			}
		})
	}
}

// Unnamed functions still need something for diagnostics to point at.
func TestUnnamedFunctionGetsSyntheticName(t *testing.T) {
	m, err := DecodeWAT(`(module (func (export "f") (result i32) (i32.const 1)))`)
	if err != nil {
		t.Fatal(err)
	}
	if m.Funcs[0].Name != "func[0]" {
		t.Errorf("Name = %q, want func[0]", m.Funcs[0].Name)
	}
}

func TestLocalsAreDecoded(t *testing.T) {
	m, err := DecodeWAT(`(module (func (export "f") (result i32)
		(local $a i32) (local $b i32)
		(local.set $a (i32.const 7))
		(local.tee $b (local.get $a))))`)
	if err != nil {
		t.Fatal(err)
	}
	f := m.Funcs[0]
	if len(f.Locals) != 2 {
		t.Fatalf("expected 2 locals, got %d", len(f.Locals))
	}
	for i, l := range f.Locals {
		if l != I32 {
			t.Errorf("local %d = %v, want i32", i, l)
		}
	}
	var sawSet, sawTee bool
	for _, in := range f.Body {
		sawSet = sawSet || in.Op == OpLocalSet
		sawTee = sawTee || in.Op == OpLocalTee
	}
	if !sawSet || !sawTee {
		t.Errorf("local.set/local.tee missing from body: %v", f.Body)
	}
}

func TestDecodeBinaryRoundTrip(t *testing.T) {
	// Decode() takes bytes; get some by compiling WAT through watgo.
	src := `(module (func (export "double") (param i32) (result i32)
		(i32.mul (local.get 0) (i32.const 2))))`
	m1, err := DecodeWAT(src)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := compileWAT(src)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Decode(bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(m1.Funcs) != len(m2.Funcs) || len(m1.Funcs[0].Body) != len(m2.Funcs[0].Body) {
		t.Fatalf("WAT and binary paths disagree: %d/%d funcs, %d/%d instrs",
			len(m1.Funcs), len(m2.Funcs), len(m1.Funcs[0].Body), len(m2.Funcs[0].Body))
	}
	if _, ok := m2.FuncByExport("double"); !ok {
		t.Error("export lost through the binary path")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("not a wasm module")); err == nil {
		t.Fatal("expected an error decoding garbage")
	}
}

func TestValTypeString(t *testing.T) {
	for vt, want := range map[ValType]string{I32: "i32", I64: "i64", F32: "f32", F64: "f64"} {
		if got := vt.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", uint8(vt), got, want)
		}
	}
	if got := ValType(99).String(); !strings.Contains(got, "99") {
		t.Errorf("unknown valtype should report its number, got %q", got)
	}
}

func TestOpString(t *testing.T) {
	if got := OpI32ShrU.String(); got != "i32.shr_u" {
		t.Errorf("got %q", got)
	}
	if got := Op(9999).String(); !strings.Contains(got, "9999") {
		t.Errorf("unknown op should report its number, got %q", got)
	}
	// Every declared op needs a name, or diagnostics degrade to numbers.
	for o := Op(0); o < numOps; o++ {
		if opNames[o] == "" {
			t.Errorf("Op(%d) has no name", uint16(o))
		}
	}
}

// The generated table is what turns unsupported instructions into readable
// diagnostics; a stale or truncated table would silently degrade them.
func TestKindNamesGenerated(t *testing.T) {
	if len(kindNames) < 400 {
		t.Errorf("kindNames has only %d entries; regenerate with scripts/gen-kindnames.py", len(kindNames))
	}
	spot := map[wasmir.InstrKind]string{
		wasmir.InstrI32Add:         "i32.add",
		wasmir.InstrLocalGet:       "local.get",
		wasmir.InstrBrIf:           "br_if",
		wasmir.InstrCallIndirect:   "call_indirect",
		wasmir.InstrI32Load8U:      "i32.load8_u",
		wasmir.InstrI32Extend8S:    "i32.extend8_s",
		wasmir.InstrMemoryGrow:     "memory.grow",
		wasmir.InstrI32WrapI64:     "i32.wrap_i64",
		wasmir.InstrF64ConvertI32S: "f64.convert_i32_s",
	}
	for kind, want := range spot {
		if got := kindNames[kind]; got != want {
			t.Errorf("kindNames[%d] = %q, want %q", uint16(kind), got, want)
		}
	}
}

// Every op we claim to support must actually be reachable from a watgo kind,
// or it is dead weight that can never fire.
func TestEverySupportedOpIsMapped(t *testing.T) {
	mapped := map[Op]bool{}
	for _, op := range supportedOps {
		mapped[op] = true
	}
	for o := Op(0); o < numOps; o++ {
		if !mapped[o] {
			t.Errorf("Op %s is declared but no watgo kind maps to it", o)
		}
	}
}
