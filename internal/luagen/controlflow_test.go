package luagen

import (
	"regexp"
	"strings"
	"testing"
)

// Invariant B has to hold for control flow too, and it is easiest to break
// there: a `local` emitted between labels would let a goto jump into its scope
// and Lua would reject the whole chunk.
func TestInvariantBWithControlFlow(t *testing.T) {
	cases := map[string]string{
		"block":   `(block (result i32) (i32.const 1))`,
		"loop":    `(loop (result i32) (i32.const 1))`,
		"if/else": `(if (result i32) (i32.const 1) (then (i32.const 2)) (else (i32.const 3)))`,
		"if only": `(block (result i32) (if (i32.const 1) (then (nop))) (i32.const 4))`,
		"br":      `(block (result i32) (br 0 (i32.const 5)))`,
		// br_if targeting a void block, which keeps label arity out of it.
		"br_if":  `(block (result i32) (block (br_if 0 (i32.const 1))) (i32.const 7))`,
		"nested": `(block (result i32) (loop (result i32) (block (result i32) (i32.const 8))))`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := functionBody(emit(t, `(module (func (export "f") (result i32) `+body+`))`), "f")
			seenNonLocal := false
			for _, ln := range strings.Split(src, "\n") {
				tr := strings.TrimSpace(ln)
				if tr == "" || strings.HasPrefix(tr, "--") ||
					strings.HasPrefix(tr, "F[") || tr == "end" {
					continue
				}
				if strings.HasPrefix(tr, "local ") {
					if seenNonLocal {
						t.Errorf("local after prologue: %q\n%s", tr, src)
					}
				} else {
					seenNonLocal = true
				}
			}
		})
	}
}

// Lua rejects a duplicate label outright, so every label in a function body
// must be defined exactly once. This is the bug the spec suite caught: a loop
// defined its label at the top and again at `end`.
func TestLabelsAreDefinedExactlyOnce(t *testing.T) {
	srcs := []string{
		`(module (func (export "f") (result i32) (loop (result i32) (i32.const 1))))`,
		`(module (func (export "f") (result i32)
			(block (result i32) (loop (result i32) (br 1 (i32.const 1))))))`,
		`(module (func (export "f") (param i32) (result i32)
			(if (result i32) (local.get 0) (then (i32.const 1)) (else (i32.const 2)))))`,
		`(module (func (export "f") (param i32) (result i32)
			(block (block (block (br_table 0 1 2 (local.get 0))) (return (i32.const 1)))
			(return (i32.const 2))) (i32.const 3)))`,
	}
	def := regexp.MustCompile(`::(L\d+)::`)
	for _, src := range srcs {
		body := functionBody(emit(t, src), "f")
		seen := map[string]int{}
		for _, m := range def.FindAllStringSubmatch(body, -1) {
			seen[m[1]]++
		}
		for l, n := range seen {
			if n > 1 {
				t.Errorf("label %s defined %d times; Lua rejects duplicates\n%s", l, n, body)
			}
		}
	}
}

// Every goto must have a matching label, or the chunk will not compile.
func TestEveryGotoHasALabel(t *testing.T) {
	src := emit(t, `(module (func (export "f") (param i32) (result i32)
		(block
			(loop
				(br_if 1 (local.get 0))
				(br 0)))
		(i32.const 0)))`)
	body := functionBody(src, "f")
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`::(L\d+)::`).FindAllStringSubmatch(body, -1) {
		defined[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`goto (L\d+)`).FindAllStringSubmatch(body, -1) {
		if !defined[m[1]] {
			t.Errorf("goto %s has no label\n%s", m[1], body)
		}
	}
}

// A bare mid-block `return` is a Lua syntax error; it must always be wrapped.
func TestReturnIsAlwaysWrapped(t *testing.T) {
	body := functionBody(emit(t, `(module (func (export "f") (param i32) (result i32)
		(if (local.get 0) (then (return (i32.const 1))))
		(i32.const 2)))`), "f")
	for _, ln := range strings.Split(body, "\n") {
		tr := strings.TrimSpace(ln)
		if strings.HasPrefix(tr, "return ") && !strings.HasPrefix(tr, "return v") {
			t.Errorf("unwrapped return: %q", tr)
		}
		if strings.HasPrefix(tr, "do return") && !strings.HasSuffix(tr, "end") {
			t.Errorf("`do return` must be closed on the same line: %q", tr)
		}
	}
	if !strings.Contains(body, "do return") {
		t.Errorf("an early return should be wrapped in do...end:\n%s", body)
	}
}

// br_table tiers on entry count: a chain below the limit, a hoisted dispatch
// array above it.
func TestBrTableTiers(t *testing.T) {
	small := emit(t, `(module (func (export "f") (param i32) (result i32)
		(block (block (br_table 0 1 (local.get 0))) (return (i32.const 1)))
		(i32.const 2)))`)
	if strings.Contains(small, "BT[") {
		t.Errorf("a small br_table should use a comparison chain:\n%s", small)
	}

	var labels []string
	for i := 0; i < 20; i++ {
		labels = append(labels, "0")
	}
	big := emit(t, `(module (func (export "f") (param i32) (result i32)
		(block (br_table `+strings.Join(labels, " ")+` 0 (local.get 0)))
		(i32.const 2)))`)
	if !strings.Contains(big, "BT[") {
		t.Errorf("a large br_table should use a dispatch array:\n%s", big)
	}
	// The array must be hoisted to chunk scope, never declared in the body.
	body := functionBody(big, "f")
	if strings.Contains(body, "local BT") {
		t.Errorf("the dispatch array must be hoisted out of the function body:\n%s", body)
	}
}

func TestModuleStateDeclarations(t *testing.T) {
	src := emit(t, `(module
		(memory 1)
		(data (i32.const 0) "hi")
		(global $g (mut i32) (i32.const 7))
		(table 1 funcref)
		(elem (i32.const 0) $f)
		(func $f (export "f") (result i32) (global.get $g)))`)
	for _, want := range []string{"local MEM", "local MEMSIZE", "local g0 = 7",
		"local TBL, TSIG", "st8raw"} {
		if !strings.Contains(src, want) {
			t.Errorf("module state is missing %q:\n%s", want, src)
		}
	}
}

func TestMemoryLoweringsUseTheRuntimeHelpers(t *testing.T) {
	cases := map[string]string{
		`(i32.load (i32.const 0))`:     "ld32(",
		`(i32.load8_u (i32.const 0))`:  "ld8(",
		`(i32.load16_u (i32.const 0))`: "ld16(",
		`(memory.size)`:                "MEMSIZE / 65536",
		`(memory.grow (i32.const 1))`:  "mem_grow(",
	}
	for body, want := range cases {
		src := emit(t, `(module (memory 1) (func (export "f") (result i32) `+body+`))`)
		if !strings.Contains(src, want) {
			t.Errorf("%s should lower to %s:\n%s", body, want, src)
		}
	}
}

// The static offset is added in infinite precision per spec, so it needs no
// masking -- and a zero offset should cost nothing at all.
func TestMemoryOffsetFolding(t *testing.T) {
	zero := emit(t, `(module (memory 1) (func (export "f") (param i32) (result i32)
		(i32.load (local.get 0))))`)
	if strings.Contains(zero, "v0 + 0") {
		t.Errorf("a zero offset should not be emitted:\n%s", zero)
	}
	off := emit(t, `(module (memory 1) (func (export "f") (param i32) (result i32)
		(i32.load offset=12 (local.get 0))))`)
	if !strings.Contains(off, "v0 + 12") {
		t.Errorf("a static offset should be folded into the address:\n%s", off)
	}
	if strings.Contains(off, "(v0 + 12) %") {
		t.Errorf("the offset must NOT be wrapped; it traps rather than wrapping:\n%s", off)
	}
}

// A missing table entry and a signature mismatch are different failures, and
// the spec distinguishes them.
func TestCallIndirectDistinguishesItsTraps(t *testing.T) {
	src := emit(t, `(module
		(type $t (func (result i32)))
		(table 1 funcref)
		(func (export "f") (param i32) (result i32)
			(call_indirect (type $t) (local.get 0))))`)
	if !strings.Contains(src, "trap_uninit()") {
		t.Errorf("a missing table entry should trap as undefined element:\n%s", src)
	}
	if !strings.Contains(src, "trap_indirect()") {
		t.Errorf("a signature mismatch should trap separately:\n%s", src)
	}
}

// Forwarding must stop at a basic-block boundary. Carrying a pending constant
// across a label reads a stale value once the branch has overwritten the slot.
func TestForwardingStopsAtControlFlow(t *testing.T) {
	body := functionBody(emit(t, `(module (func (export "f") (result i32)
		(i32.add (i32.const 1)
			(block (result i32) (i32.const 4) (br 0 (i32.const 8))))))`), "f")
	// The add after the label must read the block's result slot, not a folded
	// constant left over from before the branch.
	if strings.Contains(body, "(1 + 4)") || strings.Contains(body, "+ 4)") {
		t.Errorf("a constant was forwarded across a branch:\n%s", body)
	}
}

func TestUnsupportedFunctionEmitsAStub(t *testing.T) {
	// Bulk memory is the current stand-in for "not implemented yet"; i64 used
	// to serve here and no longer can, because it compiles.
	src := emit(t, `(module (memory 1)
		(table 2 funcref)
		(func (export "f") (table.copy (i32.const 0) (i32.const 1) (i32.const 1))))`)
	if !strings.Contains(src, "unsupported(") {
		t.Errorf("an unsupported function should emit a raising stub:\n%s", src)
	}
	if !strings.Contains(src, "table.copy") {
		t.Errorf("the stub should name the missing feature:\n%s", src)
	}
}

// Lua returns multiple values natively, which is also the mechanism i64 will
// use at M3.
func TestMultiResultCallCapturesEveryValue(t *testing.T) {
	src := emit(t, `(module
		(func $two (result i32 i32) (i32.const 1) (i32.const 2))
		(func (export "f") (result i32) (i32.add (call $two))))`)
	if !strings.Contains(src, ", v") {
		t.Errorf("a multi-result call must capture every value:\n%s", src)
	}
}

// An i64 global is two Lua locals, and every path that touches one -- the
// declaration, its initialiser, global.get and global.set -- has to move both.
func TestI64GlobalIsTwoLocals(t *testing.T) {
	src := emit(t, `(module
		(global $g (mut i64) (i64.const 4294967297))
		(func (export "get") (result i64) (global.get $g))
		(func (export "set") (param i64) (global.set $g (local.get 0))))`)
	for _, want := range []string{"local g0, g0h = 1, 1", "= g0, g0h", "g0, g0h ="} {
		if !strings.Contains(src, want) {
			t.Errorf("expected %q in output:\n%s", want, src)
		}
	}
}

func TestFloatGlobalsUseExactLiterals(t *testing.T) {
	src := emit(t, `(module
		(global $f f64 (f64.const 3.5))
		(func (export "f") (result f64) (global.get $f)))`)
	// Hex float form round-trips exactly; decimal would not.
	if !strings.Contains(src, "local g0 = 0x1.cp+01") {
		t.Errorf("f64 global should use an exact hex literal:\n%s", src)
	}
}
