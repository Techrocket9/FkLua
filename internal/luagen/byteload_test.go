package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// bytes is a module exercising every sub-word load at every alignment, plus a
// writer to seed memory and a word reader to check nothing was disturbed.
const bytesWat = `(module (memory 1)
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store8 (local.get $a) (local.get $v)))
	(func (export "l8u")  (param $a i32) (result i32) (i32.load8_u  (local.get $a)))
	(func (export "l8s")  (param $a i32) (result i32) (i32.load8_s  (local.get $a)))
	(func (export "l16u") (param $a i32) (result i32) (i32.load16_u (local.get $a)))
	(func (export "l16s") (param $a i32) (result i32) (i32.load16_s (local.get $a)))
	(func (export "l8off") (param $a i32) (result i32) (i32.load8_u offset=3 (local.get $a)))
	(func (export "l16off") (param $a i32) (result i32) (i32.load16_u offset=5 (local.get $a))))`

// seedBytes fills [0, 24) with a pattern that makes a wrong byte position, a
// wrong word, or a swapped byte pair all produce different answers.
const seedBytes = `local p = M.exports["poke"] ` +
	`for i = 0, 23 do p(i, (i * 37 + 11) % 256) end `

// ALIGNMENT is the whole risk here: the expansion picks a byte out of a word by
// position, so an off-by-one in the position, the word index or the divisor
// shows up only at some alignments. Every one is checked against -opt=0.
func TestInlinedByteLoadsMatchAtEveryAlignment(t *testing.T) {
	for _, export := range []string{"l8u", "l8s", "l16u", "l16s"} {
		for a := 0; a < 8; a++ {
			expr := fmt.Sprintf(`(function() %s return M.exports[%q](%d) end)()`,
				seedBytes, export, a)
			sameAsLevelZero(t, bytesWat, expr)
		}
	}
	// A memarg offset is folded into the address, so it must land on the same
	// byte as an equivalent bare address.
	for a := 0; a < 5; a++ {
		sameAsLevelZero(t, bytesWat,
			fmt.Sprintf(`(function() %s return M.exports["l8off"](%d) end)()`, seedBytes, a))
		sameAsLevelZero(t, bytesWat,
			fmt.Sprintf(`(function() %s return M.exports["l16off"](%d) end)()`, seedBytes, a))
	}
}

// Sign extension is the part the unsigned form does not exercise.
func TestInlinedSignedByteLoadsExtend(t *testing.T) {
	// 0x80 is the first negative byte; 0x8000 the first negative half-word.
	expr8 := `(function() M.exports["poke"](40, 0x80) return M.exports["l8s"](40) end)()`
	sameAsLevelZero(t, bytesWat, expr8)
	if got := runAt(t, bytesWat, expr8, analysis.O3); got != "4294967168" {
		t.Errorf("l8s(0x80) = %q, want 4294967168 (0xFFFFFF80)", got)
	}
	expr16 := `(function() local p = M.exports["poke"] p(44, 0x00) p(45, 0x80) ` +
		`return M.exports["l16s"](44) end)()`
	sameAsLevelZero(t, bytesWat, expr16)
	if got := runAt(t, bytesWat, expr16, analysis.O3); got != "4294934528" {
		t.Errorf("l16s(0x8000) = %q, want 4294934528 (0xFFFF8000)", got)
	}
}

// The bounds check is kept -- this buys the CALL, not the check. A 16-bit load
// one byte from the end must trap on the whole access rather than reading the
// first byte and failing on the second, which is what the spec requires and what
// the single leading check delivers.
func TestAnInlinedByteLoadStillTraps(t *testing.T) {
	const oob = "TRAP\tout of bounds memory access"
	for _, tc := range []struct{ name, expr string }{
		{"a byte past the end", `M.exports["l8u"](65536)`},
		{"a half-word straddling the end", `M.exports["l16u"](65535)`},
		{"a signed byte past the end", `M.exports["l8s"](65536)`},
		{"a negative address", `M.exports["l8u"](-1)`},
	} {
		for _, lvl := range allLevels {
			if got := runAt(t, bytesWat, tc.expr, lvl); got != oob {
				t.Errorf("-opt=%s, %s: got %q, want %q", lvl, tc.name, got, oob)
			}
		}
	}
}

// The expansion is -opt=3 only, and it must declare the scratch registers it
// uses. A bare `t3 = ...` with no `local t3` is a write to a GLOBAL: it parses,
// it runs, it computes the right answer, and it turns every scratch access into
// an _ENV lookup -- which is how the inlined i32 load once shipped a 1.28x
// SLOWDOWN past a green spec suite.
func TestInlinedByteLoadsAreLevelThreeAndDeclareTheirScratch(t *testing.T) {
	src := emitBody(t, bytesWat, analysis.O3)
	if !strings.Contains(src, "t1 = P2[8 * t1]") {
		t.Fatalf("-opt=3 should expand the byte load:\n%s", src)
	}
	// l16u needs four; every function using one must declare it.
	for _, fn := range []string{"l16u", "l16s"} {
		body := functionBody(src, fn)
		if strings.Contains(body, "t3") && !strings.Contains(body, "local t0, t1, t2, t3") {
			t.Errorf("%s uses t3 without declaring it:\n%s", fn, body)
		}
	}
	for _, lvl := range []analysis.Level{analysis.O0, analysis.O1, analysis.O2} {
		if s := emitBody(t, bytesWat, lvl); strings.Contains(s, "t1 = P2[8 * t1]") {
			t.Errorf("-opt=%s must keep the ld8/ld16 call:\n%s", lvl, s)
		}
	}
}
