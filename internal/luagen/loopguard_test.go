package luagen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The loop guard removes a BOUNDS CHECK, so a wrong answer here is not a wrong
// number -- it is a read or a write outside the guest's memory, which in a word
// table means a nil surfacing somewhere far away or a silent write past the end.
// Every test below therefore asserts behaviour rather than text, and the
// important ones are the two that assert the guard says NO.

// walk is the shape the pass targets: a bottom-tested loop closed by i32.ne on a
// local.tee'd counter, a straight-line body, and a pointer advancing by a
// constant. `sum` reads, `fill` writes, so both access forms are covered.
const walk = `(module (memory 1)
	(func (export "sum") (param $p i32) (param $bound i32) (result i32)
		(local $i i32) (local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (i32.ne
				(local.tee $i (i32.add (local.get $i) (i32.const 1)))
				(local.get $bound))))
		(local.get $acc))
	(func (export "fill") (param $p i32) (param $bound i32) (param $v i32)
		(local $i i32)
		(loop $top
			(i32.store (local.get $p) (local.get $v))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (i32.ne
				(local.tee $i (i32.add (local.get $i) (i32.const 1)))
				(local.get $bound)))))
	(func (export "down") (param $p i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc))
	(func (export "unrolled") (param $p i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc)
				(i32.add (i32.load offset=12 (local.get $p))
				(i32.add (i32.load offset=8 (local.get $p))
				(i32.add (i32.load offset=4 (local.get $p))
						 (i32.load (local.get $p)))))))
			(local.set $p (i32.add (local.get $p) (i32.const 16)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc))
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store (local.get $a) (local.get $v)))
	(func (export "peek") (param $a i32) (result i32)
		(i32.load (local.get $a))))`

// walkPlus is `walk` with a stride-4 counter alongside, so the divisibility
// case has somewhere to live.
var walkPlus = strings.Replace(walk, `	(func (export "poke")`,
	`	(func (export "sum4") (param $p i32) (param $bound i32) (result i32)
		(local $i i32) (local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (i32.ne
				(local.tee $i (i32.add (local.get $i) (i32.const 4)))
				(local.get $bound))))
		(local.get $acc))
	(func (export "poke")`, 1)

func TestTheLoopGuardIsEmittedAndCoversItsAccesses(t *testing.T) {
	src := emitBody(t, walk, analysis.O3)
	if !strings.Contains(src, "one entry test covers") {
		t.Fatalf("expected an entry guard:\n%s", src)
	}
	if !strings.Contains(src, "<= MEMSIZE") || !strings.Contains(src, "% 4 == 0 and") {
		t.Errorf("the guard should test the span and the base's alignment:\n%s", src)
	}
	// Below -opt=3 there is no inlined access to specialise, and the output must
	// not have moved.
	for _, lvl := range []analysis.Level{analysis.O0, analysis.O1, analysis.O2} {
		if s := emitBody(t, walk, lvl); strings.Contains(s, "one entry test covers") {
			t.Errorf("-opt=%s must not emit a loop guard:\n%s", lvl, s)
		}
	}
}

// sameAsLevelZero asserts every level agrees with -opt=0, which is the property
// an optimization actually owes -- and it avoids hand-computing an expected
// value, which is its own source of wrong tests.
func sameAsLevelZero(t *testing.T, wat, expr string) {
	t.Helper()
	want := runAt(t, wat, expr, analysis.O0)
	if strings.HasPrefix(want, "TRAP") {
		t.Fatalf("-opt=0 already trapped on %s: %s", expr, want)
	}
	for _, lvl := range allLevels[1:] {
		if got := runAt(t, wat, expr, lvl); got != want {
			t.Errorf("-opt=%s: got %q, -opt=0 gave %q", lvl, got, want)
		}
	}
}

// seeded wraps a body in an immediately-invoked function, because the harness
// evaluates the expression as `return <expr>`.
func seeded(body string) string {
	return `(function() local p = M.exports["poke"] ` +
		`for i = 0, 15 do p(i * 4, i * 3 + 1) end ` + body + ` end)()`
}

// A countdown closed by a bare local.tee is the shape rustc emits, and it reads
// its trip count in the opposite direction from a count up. Getting that sign
// backwards would compute a negative span, so the guard would refuse everything
// and the pass would silently do nothing -- which is why the emitted text is
// pinned here as well as the values.
func TestACountdownLoopIsGuarded(t *testing.T) {
	// The DIRECTION has to be pinned as text, because no value can observe it.
	// Taking the difference the wrong way round makes it negative, `t0 > 0`
	// fails, and the guard is simply false for every countdown -- safe, and a
	// silent loss of the entire win. `v1` is the counter and `0` its implicit
	// bound, so counter-minus-bound is the only correct order here.
	src := emitBody(t, walk, analysis.O3)
	if !strings.Contains(src, "t0 = v1 - 0") {
		t.Errorf("a countdown takes its difference as counter minus bound:\n%s", src)
	}
	sameAsLevelZero(t, walk, seeded(`return M.exports["down"](0, 8)`))
	sameAsLevelZero(t, walk, seeded(`return M.exports["down"](16, 4)`))
	sameAsLevelZero(t, walk, seeded(`return M.exports["down"](0, 1)`))
}

// An UNROLLED loop, with several accesses at distinct offsets off one base.
//
// This is the shape both real toolchains emit for an array reduction, and it is
// the only one that can catch a word index that forgets an access's own offset:
// with a single access at offset zero the index and the base coincide, so the
// bug is invisible. Here all four accesses would read the same word.
func TestAnUnrolledLoopReadsEachOffset(t *testing.T) {
	sameAsLevelZero(t, walk, seeded(`return M.exports["unrolled"](0, 4)`))
	sameAsLevelZero(t, walk, seeded(`return M.exports["unrolled"](16, 2)`))
	if src := emitBody(t, walk, analysis.O3); !strings.Contains(src, "one entry test covers 4 access") {
		t.Errorf("all four accesses should be under one guard:\n%s", src)
	}
}

// The guarded loop computes what the unguarded one computed.
func TestAGuardedLoopComputesTheSameSum(t *testing.T) {
	sameAsLevelZero(t, walk, seeded(`return M.exports["sum"](0, 8)`))
	sameAsLevelZero(t, walk, seeded(`return M.exports["sum"](16, 4)`))
	sameAsLevelZero(t, walk, seeded(`return M.exports["sum"](0, 1)`))
	// A store loop, read back through an unguarded path.
	sameAsLevelZero(t, walk,
		`(function() M.exports["fill"](400, 3, 77) return M.exports["peek"](408) end)()`)
}

// THE SAFETY TEST. A loop that walks off the end of memory must still trap, and
// the guard is what decides whether the bounds check that catches it still runs.
//
// The last access here is at MEMSIZE exactly, so the span test fails, the guard
// is false, and every access takes the checked path. If the span arithmetic were
// wrong in the optimistic direction the trap would simply not happen: the read
// would return nil out of the word table and surface as a Lua error somewhere
// else entirely, or the write would extend the table past the guest's memory.
func TestALoopThatWalksOffTheEndStillTraps(t *testing.T) {
	// The MESSAGE matters, not just that something went wrong. A read past the
	// end of the word table yields nil and raises a Lua arithmetic error, which
	// the harness also reports as a trap -- so asserting only "it failed" would
	// pass on exactly the bug this is here to catch. `oob` is what the runtime's
	// own bounds check raises, and nothing else raises it.
	const oob = "TRAP\tout of bounds memory access"

	// 1 page = 65536 bytes, so the access at 65536 is the first out of range.
	for _, tc := range []struct{ name, expr string }{
		{"a read loop", `M.exports["sum"](65528, 4)`},
		{"a store loop", `M.exports["fill"](65528, 4, 1)`},
		// The last access STARTS exactly at MEMSIZE, so only the access WIDTH
		// separates a guard that refuses from one that waves it through. Without
		// this case a span that forgot the width refuses these others anyway and
		// the omission goes unnoticed.
		{"the last access starting exactly at MEMSIZE", `M.exports["sum"](65524, 4)`},
		// The countdown-to-zero shape, tested by a bare local.tee with no
		// comparison anywhere. This is how rustc closes an unrolled loop, and
		// its trip count is read in the opposite direction from a count up.
		{"a countdown loop", `M.exports["down"](65528, 4)`},
		{"a countdown whose last access starts at MEMSIZE", `M.exports["down"](65524, 4)`},
		// An unrolled loop, whose span reaches MaxOff past the base -- the case
		// where the largest per-access offset is load-bearing in the guard.
		{"an unrolled loop walking off the end", `M.exports["unrolled"](65520, 2)`},
		// A counter whose bound is not a whole number of steps away. The loop
		// would walk past its bound and wrap, so the trip count the span was
		// computed from would be wrong by about four billion -- the guard's
		// divisibility test is what refuses it. The wrapping itself cannot be
		// run in a test; this is the observable that stands in for it.
		{"a non-divisible stride-4 loop", `M.exports["sum4"](65528, 7)`},
	} {
		for _, lvl := range allLevels {
			if got := runAt(t, walkPlus, tc.expr, lvl); got != oob {
				t.Errorf("-opt=%s, %s: got %q, want %q -- the guard let an "+
					"out-of-range access skip its bounds check", lvl, tc.name, got, oob)
			}
		}
	}
}

// An unaligned base makes the guard false, and the general path handles it --
// a failed guard is a loop this pass could not describe, not a program about to
// fault.
func TestAnUnalignedBaseFallsToTheGeneralPath(t *testing.T) {
	// Reading at offset 2 straddles words and must still produce what the
	// unguarded emitter produces at every level.
	sameAsLevelZero(t, walk, seeded(`return M.exports["sum"](2, 3)`))
}

// An i32.ne loop whose bound is not a whole number of steps away walks past it
// and wraps. The guard's divisibility test is what refuses that, and refusing it
// is a correctness condition rather than a precision loss: the trip count the
// span was computed from would be wrong by about four billion.
func TestANonDivisibleTripCountIsRefusedAtRuntime(t *testing.T) {
	const stride4 = `(module (memory 1)
		(func (export "sum") (param $p i32) (param $bound i32) (result i32)
			(local $i i32) (local $acc i32)
			(loop $top
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (i32.ne
					(local.tee $i (i32.add (local.get $i) (i32.const 4)))
					(local.get $bound))))
			(local.get $acc))
		(func (export "poke") (param $a i32) (param $v i32)
			(i32.store (local.get $a) (local.get $v))))`
	if src := emitBody(t, stride4, analysis.O3); !strings.Contains(src, "t0 % 4 == 0 then") {
		t.Fatalf("a stride-4 counter needs a divisibility test in its guard:\n%s", src)
	}
	// bound 8 is two whole steps away, so the loop runs twice; bound 7 is not,
	// so the guard refuses it and the general path takes over.
	body := func(b string) string {
		return `(function() local p = M.exports["poke"] ` +
			`for i = 0, 7 do p(i * 4, i + 1) end return ` + b + ` end)()`
	}
	sameAsLevelZero(t, stride4, body(`M.exports["sum"](0, 8)`))
}

// A guarded STORE still owes the dirty-page mark, on both arms. Same
// hazard class as the inlined store and the substituted memcpy: a store that
// does not mark its page lands in the live table, reads back correctly all
// session, and is absent from the save.
func TestAGuardedStoreReachesTheSave(t *testing.T) {
	const script = `
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["fill"](40000, 3, 305419896)   -- through the guarded store
print("dirty " .. a.persist.flush(storage.fk_pages))

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("word " .. tostring(b.exports["peek"](40008)))
`
	want := "dirty 1\nword 305419896"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, walk, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q -- the guarded store never dirtied "+
				"its page, so the save does not carry it", lvl, got, want)
		}
	}
}

// A body with control flow in it is refused: the increments could then be
// conditional, and neither the trip count nor the stride would hold.
func TestABodyWithControlFlowIsNotGuarded(t *testing.T) {
	const branchy = `(module (memory 1)
		(func (export "f") (param $p i32) (param $bound i32) (result i32)
			(local $i i32) (local $acc i32)
			(loop $top
				(if (i32.eqz (local.get $acc))
					(then (local.set $p (i32.add (local.get $p) (i32.const 4)))))
				(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
				(local.set $p (i32.add (local.get $p) (i32.const 4)))
				(br_if $top (i32.ne
					(local.tee $i (i32.add (local.get $i) (i32.const 1)))
					(local.get $bound))))
			(local.get $acc)))`
	if src := emitBody(t, branchy, analysis.O3); strings.Contains(src, "one entry test covers") {
		t.Errorf("a loop whose body branches must not be guarded:\n%s", src)
	}
}

// twoBase exercises the multi-base conjunction: two arrays walked in step, with
// different per-base offsets so that sharing one span or one word index between
// them is observable.
const twoBase = `(module (memory 1)
	(func (export "zip") (param $p i32) (param $q i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc)
				(i32.mul (i32.load (local.get $p))
						 (i32.load offset=64 (local.get $q)))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(local.set $q (i32.add (local.get $q) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc))
	(func (export "affine") (param $arr i32) (param $n i32) (result i32)
		(local $i i32) (local $p i32) (local $acc i32)
		(loop $top
			(local.set $p (i32.add (local.get $i) (local.get $arr)))
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $i (i32.add (local.get $i) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc))
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store (local.get $a) (local.get $v))))`

const seedTwo = `local p = M.exports["poke"] ` +
	`for i = 0, 63 do p(i * 4, i * 13 + 1) end ` +
	`for i = 0, 63 do p(4096 + i * 4, i * 7 + 3) end `

// Each base must get its OWN word index. Sharing one makes both accesses read
// the same array, which these values make visible.
func TestTwoBasesReadTheirOwnArrays(t *testing.T) {
	for _, n := range []int{1, 4, 16} {
		sameAsLevelZero(t, twoBase, `(function() `+seedTwo+
			`return M.exports["zip"](0, 4096 - 64, `+itoa(n)+`) end)()`)
	}
	sameAsLevelZero(t, twoBase, `(function() `+seedTwo+
		`return M.exports["affine"](0, 8) end)()`)
	if src := emitBody(t, twoBase, analysis.O3); !strings.Contains(src, "over 2 base(s)") {
		t.Errorf("the zip loop should be guarded over two bases:\n%s", src)
	}
}

// Each base must get its own SPAN. The `q` base reaches 64 bytes further than
// `p` does, so a guard that proves p's reach and applies it to q lets q's last
// access run past the end -- which must trap rather than read nil.
func TestEachBaseGetsItsOwnSpan(t *testing.T) {
	const oob = "TRAP\tout of bounds memory access"
	// q's last access is at q + (n-1)*4 + 64, placed to land just past MEMSIZE
	// while p's own reach stays comfortably inside it.
	expr := `M.exports["zip"](0, 65472, 4)`
	for _, lvl := range allLevels {
		if got := runAt(t, twoBase, expr, lvl); got != oob {
			t.Errorf("-opt=%s: got %q, want %q -- the second base's span was not "+
				"proved separately", lvl, got, oob)
		}
	}
}

// An affine base is rebuilt from an induction variable each iteration, so the
// guard has to reconstruct its entry value from the two locals it is the sum
// of. Naming the base local instead reads whatever it held before the loop.
func TestAnAffineBaseWalksFromTheRightPlace(t *testing.T) {
	for _, start := range []int{0, 16, 64} {
		sameAsLevelZero(t, twoBase, `(function() `+seedTwo+
			`return M.exports["affine"](`+itoa(start)+`, 6) end)()`)
	}
	const oob = "TRAP\tout of bounds memory access"
	// Walking off the end must still trap.
	for _, lvl := range allLevels {
		if got := runAt(t, twoBase, `M.exports["affine"](65528, 4)`, lvl); got != oob {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, oob)
		}
	}
}

// ---------------------------------------------------------------------------
// The invariant: a guard a function READS is a guard that function SEEDS.
//
// A guard flag that is declared and read but never assigned is `false` for the
// life of the call. Nothing computes a wrong answer -- the unguarded arm is the
// code that ran before this pass existed -- so no behavioural test, no
// checksum, no conformance assertion and no differential run can see it. What
// is lost is the entire win: a dead branch and a wasted word-index increment on
// every iteration, and under --persist=packed the hoisted page mark for the
// whole loop, which is emitted inside the seed block.
//
// It is therefore a TEXT property or it is nothing, and it is the same class of
// property as the countdown's difference direction above: correct-looking
// output that silently does no work. `TestACountedLoopStillSeedsItsGuard` pins
// the one shape that produced it; the corpus test below closes the class over
// every guest this repo ships, so the next lowering that replaces a loop
// header has to answer for the guard rather than quietly drop it.

// funcStart matches the line that opens an emitted wasm function.
var funcStart = regexp.MustCompile(`^(F\[\d+\]) = function\(`)

// guardDecl matches the prologue line declaring a function's guard flags and
// word indices. It is told apart from the slot and scratch declarations beside
// it by its shape: a flag first, and `false` as the first initialiser.
var guardDecl = regexp.MustCompile(`^\s*local (lg\d+[^=]*)= false, `)

var guardFlag = regexp.MustCompile(`\blg\d+\b`)

// moduleGlobal matches a module global, which occupies `gN` -- and `gNh` for the
// high half of an i64 -- off the MODULE's global index.
//
// A guard's N is a STEP index, so the two counters are unrelated, and while both
// families were spelled `gN` they met whenever a guarded loop's header step
// index fell below the module's global count. Since the guard moved to `lgN`
// that is unrepresentable rather than merely unobserved: `\b` puts no boundary
// between `l` and `g`, so this pattern cannot match a guard name at all. The
// refusal in auditGuardSeeds is kept anyway, as the belt to that braces.
var moduleGlobal = regexp.MustCompile(`\bg\d+h?\b`)

// chunkGlobals collects the names a module's own globals occupy.
func chunkGlobals(src string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(src, "\n") {
		if !strings.HasPrefix(l, "local g") || !strings.Contains(l, " = ") {
			continue
		}
		for _, n := range moduleGlobal.FindAllString(l[:strings.Index(l, " = ")], -1) {
			out[n] = true
		}
	}
	return out
}

// auditGuardSeeds reports every way an emitted chunk's guard flags fail the
// invariant, as one human-readable line each.
func auditGuardSeeds(src string) []string {
	globals := chunkGlobals(src)
	lines := strings.Split(src, "\n")
	var bad []string

	for i := 0; i < len(lines); i++ {
		m := funcStart.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		// A function's body runs to the `end` that closes it, which is the only
		// `end` emitted at column zero: everything inside is indented.
		j := i + 1
		for j < len(lines) && lines[j] != "end" {
			j++
		}
		fn, body := m[1], lines[i:j]

		decl := -1
		for k, l := range body {
			if guardDecl.MatchString(l) {
				decl = k
				break
			}
		}
		if decl < 0 {
			i = j
			continue
		}
		rest := append(append([]string{}, body[:decl]...), body[decl+1:]...)
		text := strings.Join(rest, "\n")

		for _, g := range guardFlag.FindAllString(
			guardDecl.FindStringSubmatch(body[decl])[1], -1) {
			if globals[g] {
				bad = append(bad, fmt.Sprintf(
					"%s declares guard %s, which is also the name of a module GLOBAL: "+
						"the guard shadows it for the whole function, so a global.set "+
						"there writes the flag and a global.get reads a boolean. The "+
						"two namespaces are disjoint by construction since the guard "+
						"moved to lgN, so reaching this means a name family moved back "+
						"-- see TestNoNameFamilyCanCollideWithAnother", fn, g))
				continue
			}
			assigned := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + g + ` = `)
			read := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + g + `( then| and )`)
			if !read.MatchString(text) {
				bad = append(bad, fmt.Sprintf(
					"%s declares guard %s and never reads it", fn, g))
			}
			if !assigned.MatchString(text) {
				bad = append(bad, fmt.Sprintf(
					"%s reads guard %s and never assigns it: the guard is false for "+
						"the whole call, so its fast path is dead and its word index "+
						"is stepped for nothing", fn, g))
			}
		}

		// THE HOISTED SHARD TABLE GETS THE SAME AUDIT, for a reason worth
		// stating: it is the one guard local that holds a REFERENCE rather than
		// a number. A word index that is never seeded is a wrong index; a shard
		// table that is never seeded is `false`, and `false[3]` is an error at
		// the first guarded access -- loud, but only on the path that reaches
		// it, which in a rarely-taken branch is a crash in someone's game
		// rather than a failing test here. It is initialised to `false` so that
		// case is loud, and audited here so it does not arise.
		for _, sh := range guardShard.FindAllString(text, -1) {
			if !regexp.MustCompile(`(^|[^A-Za-z0-9_])` + sh + ` = MEM\[`).MatchString(text) {
				bad = append(bad, fmt.Sprintf(
					"%s reads shard table %s and never seeds it from MEM: every "+
						"guarded access in that loop indexes `false`", fn, sh))
			}
		}
		i = j
	}
	return bad
}

// guardShard matches a hoisted shard-table local wherever it appears.
var guardShard = regexp.MustCompile(`ls\d+_\d+`)

// countedGuarded is a loop both the counted-loop lowering and the loop guard
// claim: a bottom-tested walk over an array, with the pre-header zero check
// that lets Lua's top-tested `for` stand in for it.
//
// That combination is what the emitter got wrong. The guard was written at the
// OpLoop step, the counted loop REPLACES that step with a `for`, and the step
// was never reached -- so the declaration, the guarded arms and the word-index
// stepping were all emitted and the seed was not. Both directions are here
// because the two toolchains close a loop differently and the seed is printed
// by the same code for both.
const countedGuarded = `(module (memory 1)
	(func (export "up") (param $p i32) (param $n i32) (result i32)
		(local $i i32) (local $acc i32)
		(if (i32.eqz (local.get $n)) (then (return (i32.const 0))))
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (i32.ne
				(local.tee $i (i32.add (local.get $i) (i32.const 1)))
				(local.get $n))))
		(local.get $acc))
	(func (export "down") (param $p i32) (param $n i32) (result i32)
		(local $acc i32)
		(if (i32.eqz (local.get $n)) (then (return (i32.const 0))))
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(local.get $acc))
	(func (export "fill") (param $p i32) (param $n i32) (param $v i32)
		(local $i i32)
		(if (i32.eqz (local.get $n)) (then (return)))
		(loop $top
			(i32.store (local.get $p) (local.get $v))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (i32.ne
				(local.tee $i (i32.add (local.get $i) (i32.const 1)))
				(local.get $n)))))
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store (local.get $a) (local.get $v)))
	(func (export "peek") (param $a i32) (result i32)
		(i32.load (local.get $a))))`

func TestACountedLoopStillSeedsItsGuard(t *testing.T) {
	src := emitBody(t, countedGuarded, analysis.O3)
	if !strings.Contains(src, "for ") {
		t.Fatalf("this module is meant to exercise the counted-loop lowering "+
			"as well as the guard, and no `for` was emitted:\n%s", src)
	}
	if !strings.Contains(src, " = false, ") {
		t.Fatalf("this module is meant to exercise the guard as well, and no "+
			"guard flag was declared:\n%s", src)
	}
	// The audit runs before the marker check because it is the one that says
	// WHAT went wrong. A missing `one entry test covers` is only the visible end
	// of it: the whole seed block is the comment, the span test, the flag, the
	// word index and the hoisted page mark, and the loop keeps every other part
	// of the guard whether or not any of that was written.
	if bad := auditGuardSeeds(src); len(bad) > 0 {
		t.Errorf("%s\n%s", strings.Join(bad, "\n"), src)
	}
	if !strings.Contains(src, "one entry test covers") {
		t.Errorf("expected an entry guard:\n%s", src)
	}
	// The fast path now actually runs, so it also has to be RIGHT. Before the
	// fix these agreed too -- the guard was false and every access took the
	// checked arm -- which is exactly why the text assertion above is the one
	// that finds the bug and this is the one that keeps the fix honest.
	for _, n := range []int{1, 2, 4, 15, 16} {
		sameAsLevelZero(t, countedGuarded,
			seeded(`return M.exports["up"](0, `+itoa(n)+`)`))
		sameAsLevelZero(t, countedGuarded,
			seeded(`return M.exports["down"](0, `+itoa(n)+`)`))
	}
	sameAsLevelZero(t, countedGuarded, seeded(`return M.exports["up"](16, 4)`))
	sameAsLevelZero(t, countedGuarded, seeded(`return M.exports["down"](16, 4)`))
	sameAsLevelZero(t, countedGuarded,
		`(function() M.exports["fill"](400, 3, 77) return M.exports["peek"](408) end)()`)
}

// A counted, guarded loop that walks off the end must still trap, at the exact
// message the runtime's own bounds check raises.
//
// This is the assertion the seed made load-bearing. While the guard was dead
// every access carried its own check and this passed for the wrong reason; now
// the span arithmetic is what decides, so a span computed one iteration short
// would read nil out of the word table and surface somewhere else entirely.
func TestACountedGuardedLoopStillTraps(t *testing.T) {
	const oob = "TRAP\tout of bounds memory access"
	for _, tc := range []struct{ name, expr string }{
		{"a count-up read", `M.exports["up"](65528, 4)`},
		{"a countdown read", `M.exports["down"](65528, 4)`},
		{"a store loop", `M.exports["fill"](65528, 4, 1)`},
	} {
		for _, lvl := range allLevels {
			if got := runAt(t, countedGuarded, tc.expr, lvl); got != oob {
				t.Errorf("%s at -opt=%s: got %q, want %q", tc.name, lvl, got, oob)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The invariant: a guard's locals live in a namespace nothing else can reach.
//
// This one is NOT a text-only property, which is what separates it from the
// seed above. A guard local is declared in the function prologue, so a guard
// whose name is also a module global's SHADOWS that global for the whole
// function: a `global.set` writes the flag, a `global.get` reads a boolean, and
// the write the guest thought it made is simply gone when the function returns.
//
// It was reachable, and by an ordinary route rather than a contrived one. The
// guard was `g%d` off a loop's header STEP index and a global is `g%d` off the
// MODULE's global index -- two unrelated counters in one spelling -- so any
// guarded loop whose header step index fell below the module's global count
// collided. Step 0 is a function whose first instruction is the loop, which is
// most of the hand-written .wat in this file, and global 0 in TinyGo output is
// the SHADOW-STACK POINTER.
//
// So the assertions below are values first and text second: the fixture is
// written so the shadowed global is observable after the loop.

// guardedGlobal is that shape, minimally. `walk`'s first step is the loop, so
// its guard is named off header 0, and the module has a global at index 0 for
// it to collide with. `$sp` stands in for the shadow-stack pointer and `$mark`
// is its neighbour: only one of the two is shadowed, so a run that got both
// wrong would be a broken harness rather than this bug.
const guardedGlobal = `(module (memory 1)
	(global $sp (mut i32) (i32.const 66560))
	(global $mark (mut i32) (i32.const 7))
	(func (export "walk") (param $p i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(global.set $sp (local.get $acc))
		(global.set $mark (local.get $acc))
		(local.get $acc))
	(func (export "walk_read") (param $p i32) (param $n i32) (result i32)
		(local $acc i32)
		(loop $top
			(local.set $acc (i32.add (local.get $acc) (i32.load (local.get $p))))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(br_if $top (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
		(i32.add (local.get $acc) (global.get $sp)))
	(func (export "sp") (result i32) (global.get $sp))
	(func (export "mark") (result i32) (global.get $mark))
	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store (local.get $a) (local.get $v)))
	(func (export "peek") (param $a i32) (result i32)
		(i32.load (local.get $a))))`

func TestAGuardLocalDoesNotShadowAModuleGlobal(t *testing.T) {
	src := emitBody(t, guardedGlobal, analysis.O3)

	// The fixture has to actually contain both halves of the collision, or the
	// assertions below are about nothing.
	if !strings.Contains(src, "local g0 = 66560") {
		t.Fatalf("this module is meant to declare a module global spelled g0:\n%s", src)
	}
	if !guardDecl.MatchString(firstGuardDecl(src)) {
		t.Fatalf("this module is meant to emit a guard off header step 0:\n%s", src)
	}
	if bad := auditGuardSeeds(src); len(bad) > 0 {
		t.Errorf("%s\n%s", strings.Join(bad, "\n"), src)
	}

	// The values are what say it is a miscompile rather than an eyesore. With
	// the guard spelled g0 this reported 22/7/66560 at -opt=3 against 22/22/22
	// everywhere else: the global keeps its INITIALISER, because the write went
	// into a function-scoped local that Lua discarded on return.
	sameAsLevelZero(t, guardedGlobal, seeded(
		`local a = M.exports["walk"](0, 4) `+
			`return a .. "/" .. M.exports["mark"]() .. "/" .. M.exports["sp"]()`))

	// The read side fails louder and is worth pinning too: a shadowed
	// `global.get` yields the flag, so the add is arithmetic on a boolean and
	// the guest traps somewhere with nothing to do with the loop.
	sameAsLevelZero(t, guardedGlobal,
		seeded(`return M.exports["walk_read"](0, 4)`))
}

// firstGuardDecl returns the first guard declaration line in an emitted chunk,
// or "" -- so a fixture that emits none fails on the assertion rather than on a
// nil dereference.
func firstGuardDecl(src string) string {
	for _, l := range strings.Split(src, "\n") {
		if guardDecl.MatchString(l) {
			return l
		}
	}
	return ""
}

// nameFamily is one of the families of identifier the emitter can put in
// generated Lua, and the indices it is parameterised by.
//
// The audit below is over the FAMILIES rather than over any emitted chunk,
// because that is the form the property actually has: two families collide when
// their two unrelated counters meet, and no corpus is evidence that they never
// will. Enumerating a generous range of every index and demanding the sets be
// pairwise disjoint is a proof over every module instead.
type nameFamily struct {
	what  string
	names func() []string
}

// nameFamilies is every identifier family in emitted code. Adding a lowering
// that names something new means adding it here; that is the whole cost of the
// rule, and it is what the guard's own collision cost by not existing.
func nameFamilies() []nameFamily {
	const n = 64 // more than any real index needs to be to prove the point
	seq := func(f func(int) string) func() []string {
		return func() []string {
			out := make([]string, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, f(i))
			}
			return out
		}
	}
	return []nameFamily{
		{"a module global (global index)", seq(func(i int) string {
			return globalName(i)
		})},
		{"an i64 global's high half (global index)", seq(func(i int) string {
			return globalName(i) + "h"
		})},
		{"a slot: param, local or operand stack (slot number)", seq(func(i int) string {
			return localName(ir.Slot(i))
		})},
		{"a loop guard's flag (STEP index)", seq(func(i int) string {
			return guardName(i)
		})},
		{"a loop guard's word index (STEP index, base)", func() []string {
			var out []string
			for i := 0; i < n; i++ {
				for k := 0; k < 4; k++ {
					out = append(out, wordName(i, k))
				}
			}
			return out
		}},
		// The shard table the guard hoists beside each word index. Same
		// indexing as the word index -- a STEP index and a base -- so it is the
		// same class of hazard and is enumerated the same way.
		{"a loop guard's shard table (STEP index, base)", func() []string {
			var out []string
			for i := 0; i < n; i++ {
				for k := 0; k < 4; k++ {
					out = append(out, shardName(i, k))
				}
			}
			return out
		}},
		{"a counted loop's control variable (STEP index)", seq(func(i int) string {
			return forCtrlName(i)
		})},
		{"a promoted callee upvalue (function index)", seq(func(i int) string {
			return upvalName(uint32(i))
		})},
		{"a branch label (label index)", seq(func(i int) string {
			return labelName(ir.Label(i))
		})},
		// The relay's two families, both indexed by a per-function trampoline
		// counter -- a dense small number, so the same hazard class as a step
		// index, and `LT3` against a branch label `L3` would be exactly the
		// mistake the loop guard's `g%d` was. See funclimit.go.
		{"a relay trampoline label (trampoline index)", seq(func(i int) string {
			return relayTrampolineName(i)
		})},
		{"a relay skip label (trampoline index)", seq(func(i int) string {
			return relaySkipName(i)
		})},
		{"a scratch register", func() []string {
			return []string{"t0", "t1", "t2", "t3"}
		}},
		{"a fixed name the emitter declares", func() []string {
			// The frame base `fb` is per FUNCTION; the rest are chunk locals,
			// which a function-scoped name shadows just as destructively.
			// MEMMAX is GONE: it was a compile-time constant with one reader,
			// the memory.grow lowering, and is printed there as a numeral now.
			// S1 (shard 0) and SHBOUND (min(MEMSIZE, 2097152)) are what took
			// the slot it freed plus one. See agents/codegen.md.
			return []string{"F", "MEM", "MEMSIZE", "SHBOUND", "S1", "BT", "TBL",
				"TSIG", "IMPORTS", "FS", "FP", "FUEL", "exports", "fb"}
		}},
		{"a runtime prelude chunk local", func() []string {
			return preludeChunkLocals(prelude)
		}},
	}
}

// preludeChunkLocals is the prelude's column-zero `local` names -- the ones that
// are live at chunk scope, and so the ones a function-scoped name can shadow.
// Same rule countChunkLocals counts by, since anything inside an indented
// `do ... end` is scoped to that block.
func preludeChunkLocals(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		rest, ok := strings.CutPrefix(line, "local ")
		if !ok {
			continue
		}
		if fn, ok := strings.CutPrefix(rest, "function "); ok {
			name, _, _ := strings.Cut(fn, "(")
			out = append(out, strings.TrimSpace(name))
			continue
		}
		names, _, _ := strings.Cut(rest, "=")
		for _, name := range strings.Split(names, ",") {
			if n := strings.TrimSpace(name); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// TestNoNameFamilyCanCollideWithAnother is the impossibility proof the loop
// guard did not have.
//
// A collision between two families is not a style problem: whichever name is
// declared in the narrower scope silently wins every reference in it, and Lua
// reports nothing. The guard's `g%d` against a global's `g%d` was that, and it
// was a wrong VALUE at the default -opt level rather than a lost optimization.
//
// Prefix alone is not the property, which is why this enumerates rather than
// eyeballs: `g` and `gh` would pass an eyeball and collide on an i64 global.
func TestNoNameFamilyCanCollideWithAnother(t *testing.T) {
	fams := nameFamilies()
	seen := map[string]string{}
	for _, f := range fams {
		for _, n := range f.names() {
			if prev, ok := seen[n]; ok && prev != f.what {
				t.Errorf("%q is both %s and %s -- whichever is declared in the "+
					"narrower scope shadows the other silently, and Lua will not "+
					"say so", n, prev, f.what)
				continue
			}
			seen[n] = f.what
		}
	}

	// And the guard's own three families counted explicitly, because they are
	// the ones this test exists for: dropping them from nameFamilies would make
	// every assertion above pass by asserting less. Three since sharding -- the
	// flag, the within-shard word index, and the hoisted shard TABLE.
	guards := 0
	for _, f := range fams {
		if strings.HasPrefix(f.what, "a loop guard") {
			guards++
		}
	}
	if guards != 3 {
		t.Errorf("nameFamilies enumerates %d loop-guard families, want 3 -- the "+
			"flag, the word index and the shard table are what this test was "+
			"written to hold", guards)
	}
}

// guardCorpus is every guest program this repo ships that builds with the
// standard flags: both bench guests and every example.
//
// The census in agents/optimizer.md is taken over exactly this population, so
// the invariant is asserted over it rather than over a sample. `goroutine` is
// absent because it is a wasip1 build with different flags, not because it is
// uninteresting.
var guardCorpus = []struct{ dir, pkg string }{
	{"bench/guests/go", "."},
	{"guest/go", "./examples/hello"},
	{"guest/go", "./examples/array"},
	{"guest/go", "./examples/grow"},
	{"guest/go", "./examples/api"},
	{"guest/go", "./examples/dict"},
	{"guest/go", "./examples/heap"},
	{"guest/go", "./examples/callcost"},
	{"guest/go", "./examples/churn"},
	{"guest/go", "./examples/gcconfig"},
	{"guest/go", "./examples/retain"},
}

// guardCorpusRust is the same population on the other toolchain. rustc closes an
// unrolled loop differently from TinyGo and this pass has twice been found to
// reach one language and not the other, so a text invariant about it is worth
// asserting on both.
var guardCorpusRust = []struct {
	workspace, pkg string
	lower          bool
	// collected builds the crate with the collector on, which is a DIFFERENT
	// MODULE and not a flag on the same one: `fk`'s global allocator becomes
	// guest/rust/fkgc, so the guest carries a mark-sweep collector's own loops
	// -- span walks, bitmap tests, run threading -- and those are exactly the
	// counted loops this pass rewrites. Auditing only the leaking arm would
	// leave the collector's own emitted text unchecked, which is the same
	// one-toolchain blind spot this corpus exists to close.
	collected bool
}{
	{"guest/rust", "hello", false, false},
	{"guest/rust", "api", false, false},
	{"guest/rust", "array", false, false},
	{"guest/rust", "gctorture", false, false},
	{"guest/rust", "gctorture", false, true},
	{"guest/rust", "gcconfig", false, true},
	{"bench/guests/rust", "benchkernels", true, false},
}

// persistModes are the two modes the guard's emitted text differs between: the
// hoisted `MEMPACK.mark` for a guarded STORE is inside the seed block, so a
// missing seed costs the page mark as well as the fast path -- and that one is
// not a performance loss, it is a save that omits a write. Auto is not a mode
// the emitter ever sees; ResolvePersist has already turned it into one of these.
var persistModes = []PersistMode{PersistTable, PersistPacked}

func TestEveryGuardAGuestReadsIsAlsoSeeded(t *testing.T) {
	root := luagenRepoRoot(t)
	tmp := t.TempDir()

	// A corpus test that quietly checked nothing would pass forever, which is
	// the failure mode this repo has already been bitten by. Both halves are
	// therefore required to have found guards to audit, and the counts are
	// logged so a coverage change shows up in the run rather than in a census
	// nobody re-took.
	guards := map[string]int{}
	check := func(half, name, wasmPath string) {
		raw, err := os.ReadFile(wasmPath)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		m, err := wasm.Decode(raw)
		if err != nil {
			t.Errorf("%s: decode: %v", name, err)
			return
		}
		im, err := ir.BuildModule(m)
		if err != nil {
			t.Errorf("%s: ir: %v", name, err)
			return
		}
		for _, mode := range persistModes {
			src, err := EmitModuleWith(im, Options{Opt: analysis.O3, Persist: mode})
			if err != nil {
				t.Errorf("%s (%s): emit: %v", name, mode, err)
				continue
			}
			if mode == persistModes[0] {
				n := countGuardFlags(src)
				guards[half] += n
				t.Logf("%s: %d guarded loop(s)", name, n)
			}
			if bad := auditGuardSeeds(src); len(bad) > 0 {
				t.Errorf("%s at -opt=3 --persist=%s:\n  %s",
					name, mode, strings.Join(bad, "\n  "))
			}
		}
	}

	if ok, why := guest.Available(); !ok {
		t.Logf("skipping the TinyGo half: %s", why)
	} else {
		defer func() {
			if guards["tinygo"] == 0 {
				t.Error("the TinyGo corpus produced no guarded loops at all, so " +
					"this test asserted nothing about them")
			}
		}()
		for _, g := range guardCorpus {
			name := g.dir + " " + g.pkg
			out := filepath.Join(tmp, strings.NewReplacer("/", "-", ".", "").Replace(name)+".wasm")
			if err := guest.Build(filepath.Join(root, filepath.FromSlash(g.dir)), g.pkg, out); err != nil {
				t.Errorf("building %s: %v", name, err)
				continue
			}
			check("tinygo", name, out)
		}
	}

	if ok, why := guest.RustAvailable(); !ok {
		t.Logf("skipping the Rust half: %s", why)
		return
	}
	defer func() {
		if guards["rust"] == 0 {
			t.Error("the Rust corpus produced no guarded loops at all, so this " +
				"test asserted nothing about them -- and this pass has twice " +
				"reached one toolchain and not the other")
		}
	}()
	for _, g := range guardCorpusRust {
		name := g.workspace + " " + g.pkg
		build := guest.BuildRust
		// A SEPARATE TARGET DIR PER ARM. cargo writes both arms to the same
		// artifact path, so one shared directory would hand the second reader
		// whichever wasm was built last -- silently, with the audit still
		// passing against a module it did not build.
		cargo := filepath.Join(tmp, "cargo")
		if g.collected {
			build = guest.BuildRustCollected
			name += " (collected)"
			cargo = filepath.Join(tmp, "cargo-collected")
		}
		p, err := build(filepath.Join(root, filepath.FromSlash(g.workspace)),
			g.pkg, cargo)
		if err != nil {
			t.Errorf("building %s: %v", name, err)
			continue
		}
		if g.lower {
			// The bench crate's dependencies ship precompiled bulk-memory that
			// this compiler does not accept; scripts/bench-guests.sh lowers it
			// the same way before compiling, so the module under test here is
			// the module that benchmark measures.
			lowered, err := lowerBulkMemory(t, p, filepath.Join(tmp, g.pkg+"-lowered.wasm"))
			if err != nil {
				t.Logf("skipping %s: %v", name, err)
				continue
			}
			p = lowered
		}
		check("rust", name, p)
	}
}

// countGuardFlags counts the guard flags a chunk declares, which is one per
// guarded loop.
func countGuardFlags(src string) int {
	n := 0
	for _, l := range strings.Split(src, "\n") {
		if m := guardDecl.FindStringSubmatch(l); m != nil {
			n += len(guardFlag.FindAllString(m[1], -1))
		}
	}
	return n
}

// lowerBulkMemory runs the same wasm-opt pass scripts/bench-guests.sh runs.
func lowerBulkMemory(t *testing.T, in, out string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath("wasm-opt")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "--llvm-memory-copy-fill-lowering",
		"--enable-bulk-memory", "-O3", in, "-o", out)
	if o, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("wasm-opt: %w\n%s", err, o)
	}
	return out, nil
}

func luagenRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
