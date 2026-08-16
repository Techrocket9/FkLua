package luagen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// A compiled function that uses a scratch register must DECLARE it.
//
// This exists because the inlined i32 load shipped without it, and the failure
// mode was invisible to every other gate. `t0 = ...` in a function whose
// prologue has no `local t0, t1` is a write to a GLOBAL: it parses, it runs, it
// computes the right answer, and the whole spec suite passes at every level.
// What it does is turn each scratch access into an _ENV table lookup and
// scribble a name into the mod's global namespace.
//
// It showed up only as a performance number going the wrong way -- `chase` and
// `sum` came back 1.28x SLOWER when the change was supposed to make them
// faster. A correctness gate that cannot see a bug this large is worth adding
// rather than relying on someone benchmarking the right kernel.
func TestAFunctionUsingAScratchRegisterDeclaresIt(t *testing.T) {
	// One module per shape that reaches for a scratch, plus the inlined load.
	// A plain i32 load is the one that regressed; the others are here so the
	// test speaks to the whole class rather than the bug that prompted it.
	mods := map[string]string{
		"i32.load": `(func (export "f") (param i32) (result i32) (i32.load (local.get 0)))`,
		// The inlined STORE reaches for both t0 and t1, and arrived by the same
		// route as the load: a lowering gated at the call site rather than
		// inside needsScratch, which is exactly where the declaration is easy
		// to forget.
		"i32.store": `(func (export "f") (param i32) (param i32)
			(i32.store (local.get 0) (local.get 1)))`,
		// A store whose value is a composite expression is the case that
		// actually writes t1, rather than leaving a bare name in place.
		"i32.store composite": `(func (export "f") (param i32) (param i32)
			(i32.store (local.get 0) (i32.add (local.get 1) (i32.const 1))))`,
		"i32.shl":     `(func (export "f") (param i32) (result i32) (i32.shl (local.get 0) (i32.const 3)))`,
		"i32.lt_s":    `(func (export "f") (param i32) (result i32) (i32.lt_s (local.get 0) (i32.const 3)))`,
		"i32.load8_s": `(func (export "f") (param i32) (result i32) (i32.load8_s (local.get 0)))`,
		"no scratch":  `(func (export "f") (param i32) (result i32) (i32.add (local.get 0) (i32.const 1)))`,
	}
	// Bodies look like `F[0] = function(...)` ... `end`, and a scratch use is a
	// bare t0 or t1 anywhere inside one.
	body := regexp.MustCompile(`(?s)F\[\d+\] = function\([^)]*\)(.*?)\nend\n`)
	uses := regexp.MustCompile(`\bt[01]\b`)

	for name, fn := range mods {
		for _, lvl := range []analysis.Level{analysis.O0, analysis.O1, analysis.O2, analysis.O3} {
			src := emitAt(t, "(module (memory 1) "+fn+")", lvl)
			for _, m := range body.FindAllStringSubmatch(src, -1) {
				fnBody := m[1]
				if !uses.MatchString(fnBody) {
					continue
				}
				if !strings.Contains(fnBody, "local t0, t1") {
					t.Errorf("%s at -opt=%d uses a scratch register without "+
						"declaring it, so every t0/t1 here is a GLOBAL:\n%s",
						name, lvl, fnBody)
				}
			}
		}
	}
}

// And the inlined load is actually inlined at -opt=3 and not below.
//
// Without this, the pass could silently stop firing -- the emitted Lua would
// still be correct, the suite would still be green, and the 1.36x would just be
// gone.
func TestTheInlinedLoadFiresAtOpt3AndNotBelow(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "f") (param i32) (result i32) (i32.load (local.get 0))))`
	for _, tc := range []struct {
		lvl    analysis.Level
		inline bool
	}{{analysis.O0, false}, {analysis.O1, false}, {analysis.O2, false}, {analysis.O3, true}} {
		src := emitAt(t, wat, tc.lvl)
		// S1 is shard 0, which is where the inlined fast arm reads. The shard
		// select is not in this string BECAUSE the merged test already proved
		// the address inside the first shard -- that is the whole design.
		got := strings.Contains(src, "S1[t0 / 4 + 1]")
		if got != tc.inline {
			verb := "did not inline"
			if got {
				verb = "inlined"
			}
			t.Errorf("-opt=%d %s the load; want inline=%v", tc.lvl, verb, tc.inline)
		}
		// And whatever it does, a call to ld32 must remain reachable for the
		// unaligned case rather than being dropped.
		if tc.inline && !strings.Contains(src, "ld32(MEM, MEMSIZE, t0)") {
			t.Errorf("-opt=%d inlined the load but left no unaligned fallback", tc.lvl)
		}
	}
}
