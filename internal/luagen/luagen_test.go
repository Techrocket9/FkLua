package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// emit compiles a WAT module and returns just the generated function bodies,
// with the (large, constant) runtime prelude stripped so assertions read
// against the interesting part.
func emit(t *testing.T, wat string) string {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModule(im)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	i := strings.Index(src, "local F = {}")
	if i < 0 {
		t.Fatalf("generated chunk has no function table:\n%s", src)
	}
	return src[i:]
}

// emitOp wraps a single binary i32 op so the lowering can be asserted directly.
func emitBin(t *testing.T, op string) string {
	t.Helper()
	return emit(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(`+op+` (local.get 0) (local.get 1))))`)
}

// These are the lowerings the in-game probe measured as fastest. Three of them
// are the opposite of what a modern Lua would suggest, so pinning them here
// stops a well-meaning "optimization" from silently regressing the emitter.
func TestMeasuredLowerings(t *testing.T) {
	tests := []struct {
		op   string
		want string
		why  string
	}{
		{"i32.add", "v2 = (v0 + v1) % 4294967296.0",
			"% measured 2.81 ns vs 3.66-5.34 for a conditional fixup"},
		{"i32.sub", "v2 = (v0 - v1) % 4294967296.0",
			"Lua's floored % wraps negatives correctly with no fixup"},
		{"i32.lt_u", "v2 = v0 < v1 and 1 or 0",
			"unsigned compares are direct -- the payoff of Invariant A"},
		{"i32.ge_u", "v2 = v0 >= v1 and 1 or 0", ""},
		{"i32.eq", "v2 = v0 == v1 and 1 or 0", ""},
		{"i32.ne", "v2 = v0 ~= v1 and 1 or 0", ""},
	}
	for _, tc := range tests {
		t.Run(tc.op, func(t *testing.T) {
			got := emitBin(t, tc.op)
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in output (%s)\ngot:\n%s", tc.want, tc.why, got)
			}
		})
	}
}

// bit32 measured slowest on every operation that has an arithmetic form, so it
// must not appear where one exists.
func TestArithmeticPreferredOverBit32(t *testing.T) {
	for _, op := range []string{"i32.add", "i32.sub", "i32.shl", "i32.shr_u", "i32.shr_s"} {
		got := emitBin(t, op)
		body := got
		for _, banned := range []string{"band(", "bor(", "bxor("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: emitted %s, but an arithmetic form exists\n%s", op, banned, body)
			}
		}
	}
	// and/or/xor genuinely have no arithmetic form for general operands, so
	// bit32 is correct there -- and must actually be used.
	for op, want := range map[string]string{
		"i32.and": "band(", "i32.or": "bor(", "i32.xor": "bxor(",
	} {
		got := emitBin(t, op)
		if !strings.Contains(got, want) {
			t.Errorf("%s with non-constant operands should use %s:\n%s", op, want, got)
		}
	}
}

// The single biggest win available: 2.88 ns against 53.99 for the general
// split, an 18.75x difference.
func TestConstantMultiplySpecialization(t *testing.T) {
	got := emit(t, `(module (func (export "f") (param i32) (result i32)
		(i32.mul (local.get 0) (i32.const 12))))`)
	if !strings.Contains(got, "* 12) % 4294967296.0") {
		t.Errorf("small constant multiply should fold to one multiply:\n%s", got)
	}
	if strings.Contains(got, "mul32(") {
		t.Errorf("should not call the general split for a small constant:\n%s", got)
	}
}

func TestLargeConstantMultiplyUsesGeneralSplit(t *testing.T) {
	// 2654435761 exceeds 2^21, so a*c could pass 2^53 and lose precision.
	got := emit(t, `(module (func (export "f") (param i32) (result i32)
		(i32.mul (local.get 0) (i32.const 2654435761))))`)
	if !strings.Contains(got, "mul32(") {
		t.Errorf("large constant must use the general split to stay exact:\n%s", got)
	}
}

func TestNonConstantMultiplyUsesGeneralSplit(t *testing.T) {
	if got := emitBin(t, "i32.mul"); !strings.Contains(got, "mul32(") {
		t.Errorf("variable multiply must use the general split:\n%s", got)
	}
}

func TestConstantShiftsAreFolded(t *testing.T) {
	tests := []struct {
		op, konst, want string
	}{
		// Mask first: a * 2^n alone can reach 2^63 and lose precision.
		{"i32.shl", "8", "v1 = (v0 % 16777216) * 256"},
		{"i32.shr_u", "8", "v1 = (v0 - v0 % 256) / 256"},
		{"i32.shl", "0", "v1 = v0"},
		{"i32.shr_u", "0", "v1 = v0"},
		// A shift distance is taken mod 32, so 33 is a shift by 1.
		{"i32.shl", "33", "v1 = (v0 % 2147483648) * 2"},
	}
	for _, tc := range tests {
		got := emit(t, `(module (func (export "f") (param i32) (result i32)
			(`+tc.op+` (local.get 0) (i32.const `+tc.konst+`))))`)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s by %s: expected %q\n%s", tc.op, tc.konst, tc.want, got)
		}
		if strings.Contains(got, "shl32(") || strings.Contains(got, "shr_u32(") {
			t.Errorf("%s by constant %s should not call the runtime helper:\n%s",
				tc.op, tc.konst, got)
		}
	}
}

func TestConstantMaskSpecializations(t *testing.T) {
	tests := []struct {
		op, konst, want, why string
	}{
		{"i32.and", "255", "v1 = v0 % 256", "and with 2^n-1 is a modulo"},
		{"i32.and", "65535", "v1 = v0 % 65536", ""},
		{"i32.and", "4294967040", "v1 = v0 - v0 % 256", "and with ~(2^n-1) is an align-down"},
		{"i32.and", "4294967295", "v1 = v0", "and with all ones is identity"},
		{"i32.and", "0", "v1 = 0", ""},
		{"i32.xor", "4294967295", "v1 = 4294967295.0 - v0", "xor with all ones is a complement"},
		{"i32.or", "0", "v1 = v0", "or with zero is identity"},
	}
	for _, tc := range tests {
		got := emit(t, `(module (func (export "f") (param i32) (result i32)
			(`+tc.op+` (local.get 0) (i32.const `+tc.konst+`))))`)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s %s: expected %q (%s)\n%s", tc.op, tc.konst, tc.want, tc.why, got)
		}
	}
}

// Invariant B: no `local` may appear after the prologue, or a later goto could
// jump into its scope and Lua would reject the chunk outright.
func TestInvariantBNoLocalsAfterPrologue(t *testing.T) {
	src := emit(t, `(module (func (export "f") (param i32) (param i32) (result i32)
		(local $a i32) (local $b i32)
		(local.set $a (i32.add (local.get 0) (local.get 1)))
		(local.set $b (i32.mul (local.get $a) (i32.const 3)))
		(i32.xor (local.get $a) (local.get $b))))`)

	// Invariant B constrains function bodies. Chunk-level declarations (F,
	// exports, the prelude's helpers) are outside any goto's reach.
	body := functionBody(src, "f")
	if body == "" {
		t.Fatalf("could not isolate the function body from:\n%s", src)
	}
	seenNonLocal := false
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") ||
			strings.HasPrefix(trimmed, "F[") || trimmed == "end" {
			continue
		}
		isLocal := strings.HasPrefix(trimmed, "local ")
		if isLocal && seenNonLocal {
			t.Errorf("local declared after the prologue, which breaks Invariant B: %q\n%s",
				trimmed, body)
		}
		if !isLocal {
			seenNonLocal = true
		}
	}
}

// Invariant B must hold for every function the emitter can produce, not just
// the one hand-picked above.
func TestInvariantBAcrossAllOps(t *testing.T) {
	binOps := []string{
		"i32.add", "i32.sub", "i32.mul", "i32.div_s", "i32.div_u",
		"i32.rem_s", "i32.rem_u", "i32.and", "i32.or", "i32.xor",
		"i32.shl", "i32.shr_s", "i32.shr_u", "i32.rotl", "i32.rotr",
		"i32.eq", "i32.ne", "i32.lt_s", "i32.lt_u", "i32.le_s", "i32.le_u",
		"i32.gt_s", "i32.gt_u", "i32.ge_s", "i32.ge_u",
	}
	unOps := []string{"i32.clz", "i32.ctz", "i32.popcnt",
		"i32.extend8_s", "i32.extend16_s", "i32.eqz"}

	check := func(t *testing.T, op, src string) {
		t.Helper()
		body := functionBody(emit(t, src), "f")
		seenNonLocal := false
		for _, ln := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(ln)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") ||
				strings.HasPrefix(trimmed, "F[") || trimmed == "end" {
				continue
			}
			if strings.HasPrefix(trimmed, "local ") {
				if seenNonLocal {
					t.Errorf("%s: local after prologue: %q\n%s", op, trimmed, body)
				}
			} else {
				seenNonLocal = true
			}
		}
	}

	for _, op := range binOps {
		check(t, op, `(module (func (export "f") (param i32) (param i32) (result i32)
			(`+op+` (local.get 0) (local.get 1))))`)
	}
	for _, op := range unOps {
		check(t, op, `(module (func (export "f") (param i32) (result i32)
			(`+op+` (local.get 0))))`)
	}
}

// Every op the IR accepts must have a lowering, or compilation fails late with
// a confusing error instead of at the decoder with a clear one.
func TestEveryOpHasALowering(t *testing.T) {
	binOps := []string{
		"i32.add", "i32.sub", "i32.mul", "i32.div_s", "i32.div_u",
		"i32.rem_s", "i32.rem_u", "i32.and", "i32.or", "i32.xor",
		"i32.shl", "i32.shr_s", "i32.shr_u", "i32.rotl", "i32.rotr",
		"i32.eq", "i32.ne", "i32.lt_s", "i32.lt_u", "i32.le_s", "i32.le_u",
		"i32.gt_s", "i32.gt_u", "i32.ge_s", "i32.ge_u",
	}
	for _, op := range binOps {
		src := emit(t, `(module (func (export "f") (param i32) (param i32) (result i32)
			(`+op+` (local.get 0) (local.get 1))))`)
		if !strings.Contains(src, "return v2") {
			t.Errorf("%s produced no result assignment:\n%s", op, src)
		}
	}
	for _, op := range []string{"i32.clz", "i32.ctz", "i32.popcnt",
		"i32.extend8_s", "i32.extend16_s", "i32.eqz"} {
		src := emit(t, `(module (func (export "f") (param i32) (result i32)
			(`+op+` (local.get 0))))`)
		if !strings.Contains(src, "return v1") {
			t.Errorf("%s produced no result assignment:\n%s", op, src)
		}
	}
}

// Forwarding must not carry a local.get past a write to that same local.
func TestForwardingRespectsLocalWriteHazard(t *testing.T) {
	src := functionBody(emit(t, `(module (func (export "f") (param i32) (result i32)
		(local $t i32)
		(local.set $t (local.get 0))
		(i32.add (local.get $t) (i32.mul (local.tee $t (i32.const 5)) (i32.const 3)))))`), "f")

	// The read of $t must be materialised into a stack slot before the tee
	// overwrites v1; if forwarding leaked through, the add would reference v1
	// directly and read 5 instead of the original value.
	iRead := strings.Index(src, "v2 = v1")
	iWrite := strings.Index(src, "v1 = 5")
	if iRead < 0 {
		t.Fatalf("expected the local read to be materialised before the write:\n%s", src)
	}
	if iWrite >= 0 && iRead > iWrite {
		t.Errorf("local read forwarded past a write to the same local:\n%s", src)
	}
}

// wasm requires declared locals to start at zero; Lua would leave them nil,
// and nil arithmetic raises rather than producing a wrong answer -- but it
// would raise in generated code, which is far harder to debug than a wrong
// declaration here.
func TestDeclaredLocalsAreZeroInitialised(t *testing.T) {
	src := emit(t, `(module (func (export "f") (result i32)
		(local $a i32) (local $b i32)
		(i32.add (local.get $a) (local.get $b))))`)
	if !strings.Contains(src, "local v0, v1 = 0, 0") {
		t.Errorf("declared locals must be zero-initialised:\n%s", src)
	}
}

func TestExportsTable(t *testing.T) {
	src := emit(t, `(module
		(func (export "one") (result i32) (i32.const 1))
		(func (export "two") (result i32) (i32.const 2)))`)
	for _, want := range []string{`exports["one"] = F[0]`, `exports["two"] = F[1]`,
		"return { funcs = F, exports = exports,", "rt = { f32_to_bits"} {
		if !strings.Contains(src, want) {
			t.Errorf("expected %q\n%s", want, src)
		}
	}
}

func TestPreludeIsIncluded(t *testing.T) {
	m, _ := wasm.DecodeWAT(`(module (func (export "f") (result i32) (i32.const 1)))`)
	im, _ := ir.BuildModule(m)
	src, err := EmitModule(im)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"local function mul32", "local P2", "local PC",
		"trap_div0", "local band, bor, bxor"} {
		if !strings.Contains(src, want) {
			t.Errorf("prelude is missing %q", want)
		}
	}
}

func TestLowMask(t *testing.T) {
	cases := map[uint32]struct {
		n  uint32
		ok bool
	}{
		1: {1, true}, 3: {2, true}, 255: {8, true}, 65535: {16, true},
		0x7FFFFFFF: {31, true},
		0:          {0, false}, 0xFFFFFFFF: {0, false}, 5: {0, false}, 256: {0, false},
	}
	for k, want := range cases {
		n, ok := lowMask(k)
		if ok != want.ok || (ok && n != want.n) {
			t.Errorf("lowMask(%#x) = (%d, %v), want (%d, %v)", k, n, ok, want.n, want.ok)
		}
	}
}

func TestHighMask(t *testing.T) {
	cases := map[uint32]struct {
		n  uint32
		ok bool
	}{
		0xFFFFFF00: {8, true}, 0xFFFF0000: {16, true}, 0xFFFFFFFE: {1, true},
		0xFFFFFFFF: {0, false}, 0: {0, false}, 0xFF: {0, false},
	}
	for k, want := range cases {
		n, ok := highMask(k)
		if ok != want.ok || (ok && n != want.n) {
			t.Errorf("highMask(%#x) = (%d, %v), want (%d, %v)", k, n, ok, want.n, want.ok)
		}
	}
}

// The peephole must only fire when the constant was produced by the immediately
// preceding step, or it could fold a value that has since been overwritten.
func TestConstPeepholeIsNarrow(t *testing.T) {
	// Here the constant is the FIRST operand, so the step before the shift is
	// local.get, not i32.const. Folding would be wrong.
	src := emit(t, `(module (func (export "f") (param i32) (result i32)
		(i32.shl (i32.const 1) (local.get 0))))`)
	if !strings.Contains(src, "shl32(") {
		t.Errorf("a non-constant shift distance must use the runtime helper:\n%s", src)
	}
}

// functionBody extracts one generated function from a chunk.
func functionBody(src, export string) string {
	// Functions are emitted in index order with a comment naming them; find the
	// assignment for the export by scanning for its exports entry first.
	idx := strings.Index(src, `exports["`+export+`"] = F[`)
	if idx < 0 {
		return src
	}
	rest := src[idx+len(`exports["`+export+`"] = F[`):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return src
	}
	marker := "F[" + rest[:end] + "] = function("
	start := strings.Index(src, marker)
	if start < 0 {
		return src
	}
	tail := src[start:]
	stop := strings.Index(tail, "\nend\n")
	if stop < 0 {
		return tail
	}
	return tail[:stop+5]
}
