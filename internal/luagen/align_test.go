package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// The congruence analysis, from the emitter's side.
//
// At -opt=3 an i32 load expands to three statements, the last of which asks a
// question at runtime that the compiler can usually answer:
//
//	if t0 % 4 == 0 then v = MEM[t0 / 4 + 1] else v = ld32(MEM, MEMSIZE, t0) end
//
// When the analysis proves the effective address a multiple of four the modulo,
// the compare and the branch all go. What follows is in two halves, and the
// second half is the one that matters: proving an address aligned when it is not
// makes MEM[t0 / 4 + 1] a FRACTIONAL table key, which reads nil and surfaces as
// arithmetic on a nil value a long way from the load that caused it.

// alignWAT is one module holding every shape the analysis reasons about, in
// both directions: shapes it must prove, and shapes that look like them and
// must come back UNKNOWN.
//
// Every function reads through i32.load so the same lowering is under test in
// all of them, and `poke` writes single bytes so a misaligned load can be given
// a value that no aligned load would produce.
const alignWAT = `(module
	(memory 1)
	(global $g (mut i32) (i32.const 0))
	(global $h (mut i32) (i32.const 8))

	(func (export "poke") (param $a i32) (param $v i32)
		(i32.store8 (local.get $a) (local.get $v)))

	;; -- must be PROVED aligned ------------------------------------------
	(func (export "shl2") (param $i i32) (result i32)
		(i32.load (i32.shl (local.get $i) (i32.const 2))))
	(func (export "mul4") (param $i i32) (result i32)
		(i32.load (i32.mul (local.get $i) (i32.const 4))))
	(func (export "maskoff") (param $a i32) (result i32)
		(i32.load (i32.and (local.get $a) (i32.const 4294967292))))
	(func (export "offset8") (param $i i32) (result i32)
		(i32.load offset=8 (i32.mul (local.get $i) (i32.const 4))))
	;; a pointer starting at a declared local -- which the spec zeroes -- and
	;; advancing by 4 forever. The wrap on the bump is invisible modulo 4
	;; because 4 divides 2^32, which is the whole reason this shape works.
	(func (export "bump4") (param $n i32) (result i32)
		(local $p i32) (local $i i32)
		(block $done (loop $top
			(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
			(local.set $p (i32.add (local.get $p) (i32.const 4)))
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br $top)))
		(i32.load (local.get $p)))
	;; a global the module only ever stores an aligned value into
	(func (export "seth") (param $v i32)
		(global.set $h (i32.mul (local.get $v) (i32.const 8))))
	(func (export "viah") (result i32) (i32.load (global.get $h)))

	;; -- must come back UNKNOWN ------------------------------------------
	;; two paths reaching one load with different residues
	(func (export "merge") (param $c i32) (result i32)
		(local $p i32)
		(block $end
			(br_if $end (local.get $c))
			(local.set $p (i32.const 2)))
		(i32.load (local.get $p)))
	;; the same pointer bump, two bytes at a time
	(func (export "bump2") (param $n i32) (result i32)
		(local $p i32) (local $i i32)
		(block $done (loop $top
			(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
			(local.set $p (i32.add (local.get $p) (i32.const 2)))
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br $top)))
		(i32.load (local.get $p)))
	;; a shift distance of 0 mod 32 is the IDENTITY -- the same trap wrap
	;; deferral fell into, and it absorbs nothing here either
	(func (export "shl0") (param $a i32) (result i32)
		(i32.load (i32.shl (local.get $a) (i32.const 32))))
	;; and-with-all-ones is the identity too
	(func (export "andall") (param $a i32) (result i32)
		(i32.load (i32.and (local.get $a) (i32.const 4294967295))))
	;; even, but not a multiple of four
	(func (export "mul2") (param $i i32) (result i32)
		(i32.load (i32.mul (local.get $i) (i32.const 2))))
	;; an aligned base pushed off by the memarg offset
	(func (export "off1") (param $i i32) (result i32)
		(i32.load offset=1 (i32.mul (local.get $i) (i32.const 4))))
	;; a load result is unknown, however aligned the address that produced it
	(func (export "chain") (param $i i32) (result i32)
		(i32.load (i32.load (i32.mul (local.get $i) (i32.const 4)))))
	;; a global the module stores an arbitrary value into
	(func (export "poison") (param $v i32) (global.set $g (local.get $v)))
	(func (export "viag") (result i32) (i32.load (global.get $g))))`

// bodyOf returns the emitted Lua for one export's function.
func bodyOf(t *testing.T, src, export string) string {
	t.Helper()
	head := `exports["` + export + `"] = F[`
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("export %q not found", export)
	}
	rest := src[i+len(head):]
	j := strings.IndexByte(rest, ']')
	idx := rest[:j]
	start := strings.Index(src, "F["+idx+"] = function")
	if start < 0 {
		t.Fatalf("body of %q (F[%s]) not found", export, idx)
	}
	end := strings.Index(src[start:], "\nend\n")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+end]
}

// The pass itself: a provably aligned load loses the runtime alignment test.
//
// This is the assertion that fails without the change -- before it, every one of
// these emitted `if t0 % 4 == 0 then ... else ld32(...) end`.
func TestAProvablyAlignedLoadDropsItsAlignmentBranch(t *testing.T) {
	src := emitBody(t, alignWAT, analysis.O3)
	for _, export := range []string{"shl2", "mul4", "maskoff", "offset8", "bump4", "viah"} {
		body := bodyOf(t, src, export)
		// A proof of alignment does two things under sharding, not one. It
		// drops the `% 4` branch as it always did -- and because dropping it
		// leaves the slow arm with no CALL in it, the whole access collapses to
		// the NO-ELSE form, whose tail is a single shared table index. That is
		// what makes a proven-aligned load cost exactly what the flat one did.
		if !strings.Contains(body, "= t1[t0 / 4 + 1]") {
			t.Errorf("%s: no inlined word load at all:\n%s", export, body)
		}
		if strings.Contains(body, "t0 % 4 == 0") {
			t.Errorf("%s: address is provably 4-aligned but the branch is still there:\n%s",
				export, body)
		}
		if strings.Contains(body, "ld32(MEM") {
			t.Errorf("%s: a proven-aligned load kept an unaligned fallback, so it "+
				"cannot take the no-else form and pays a jump the flat load did "+
				"not:\n%s", export, body)
		}
		if !strings.Contains(body, "t1 = t0 % 2097152") {
			t.Errorf("%s: the slow arm did not inline its shard select:\n%s",
				export, body)
		}
		// The bounds check is NOT part of the trade. Alignment says nothing
		// about range, and a negative multiple of four would index the table at
		// a negative key and read nil.
		if !strings.Contains(body, "if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end") {
			t.Errorf("%s: the bounds check went missing:\n%s", export, body)
		}
	}
}

// The other direction, which is the one that keeps the pass sound: every shape
// the analysis cannot prove keeps its runtime test.
//
// Each of these is a near-miss of a shape above -- an identity where a mask was
// expected, an odd multiple, a merge of two residues -- so a transfer function
// that over-claims lands here rather than in a guest.
func TestAnUnprovableAddressKeepsItsAlignmentBranch(t *testing.T) {
	src := emitBody(t, alignWAT, analysis.O3)
	for _, export := range []string{
		"merge", "bump2", "shl0", "andall", "mul2", "off1", "chain", "viag"} {
		body := bodyOf(t, src, export)
		if !strings.Contains(body, "t0 % 4 == 0") {
			t.Errorf("%s: address is NOT provably aligned, but the branch was dropped:\n%s",
				export, body)
		}
	}
}

// -opt=0, 1 and 2 do not inline the load at all, so there is nothing there for
// the analysis to change. Asserted rather than assumed, because the gate is a
// single predicate and moving it would be silent.
func TestAlignmentChangesNothingBelowOptThree(t *testing.T) {
	for _, lvl := range []analysis.Level{analysis.O0, analysis.O1, analysis.O2} {
		src := emitBody(t, alignWAT, lvl)
		if strings.Contains(src, "MEM[t0 / 4 + 1]") {
			t.Errorf("-opt=%s: the load was inlined below -opt=3", lvl)
		}
		if !strings.Contains(src, "ld32(MEM, MEMSIZE,") {
			t.Errorf("-opt=%s: the load stopped calling ld32", lvl)
		}
	}
}

// alignExpr seeds sixteen bytes and reads one export back, so an assertion can
// be written as a single expected number.
//
// Byte n gets value n+1, which makes every four-byte window distinct: a load
// that came back from the wrong offset, or that lost or gained an alignment
// test, cannot accidentally match.
func alignExpr(export, args string) string {
	return `(function()
	local poke = M.exports["poke"]
	for i = 0, 63 do poke(i, (i + 1) % 251) end
	return M.exports["` + export + `"](` + args + `)
end)()`
}

// word is what an i32.load at byte address a must return given that seeding.
func word(a int) uint32 {
	var v uint32
	for k := 3; k >= 0; k-- {
		v = v<<8 | uint32((a+k+1)%251)
	}
	return v
}

// The adversarial half: every shape the analysis handles, driven with addresses
// that are deliberately NOT aligned, at every level.
//
// A wrong "aligned" answer shows up here as a nil, not as a slightly different
// number -- MEM[t0 / 4 + 1] with a fractional key is a miss on a Lua table -- so
// these read as a crash rather than as a diff, which is exactly why they are
// worth having next to the shape assertions above.
func TestMisalignedAddressesStillReadTheRightWordAtEveryLevel(t *testing.T) {
	cases := []struct {
		export, args string
		addr         int
	}{
		// A merge of residue 0 and residue 2 -- the join has to weaken to
		// "even", not stay at "aligned" because one predecessor said so.
		{"merge", "0", 2},
		{"merge", "1", 0},
		// Bumped two bytes at a time: aligned on even trips and not on odd.
		{"bump2", "1", 2},
		{"bump2", "2", 4},
		{"bump2", "3", 6},
		// The identity shapes. A shift of 32 is a shift of 0, and a mask of all
		// ones clears nothing, so both hand the address straight through.
		{"shl0", "3", 3},
		{"shl0", "4", 4},
		{"andall", "5", 5},
		{"andall", "6", 6},
		// Even, but not a multiple of four.
		{"mul2", "1", 2},
		{"mul2", "3", 6},
		// An aligned base plus a memarg offset that is not.
		{"off1", "0", 1},
		{"off1", "2", 9},
		// The shapes that ARE proved, exercised for value as well as for form.
		{"shl2", "3", 12},
		{"mul4", "5", 20},
		{"maskoff", "22", 20},
		{"offset8", "1", 12},
		{"bump4", "3", 12},
	}
	for _, c := range cases {
		want := fmt.Sprintf("%d", word(c.addr))
		t.Run(c.export+"/"+c.args, func(t *testing.T) {
			sameAtEveryLevel(t, alignWAT, alignExpr(c.export, c.args), want)
		})
	}
}

// The module-level part, which is where the pass gets most of what it gets:
// LLVM's shadow stack lives behind a mutable global, and an analysis that gives
// up at global.get proves about a third of the loads this one proves.
//
// The claim is inductive -- the initialiser is in the class, and every
// global.set in the MODULE stores something in it -- so a single misaligned
// store anywhere has to weaken it everywhere. `poison` is that store, and it
// lives in a different function from the load that would benefit.
func TestOneMisalignedGlobalStorePoisonsEveryReadOfIt(t *testing.T) {
	sameAtEveryLevel(t, alignWAT,
		`(function()
	local poke = M.exports["poke"]
	for i = 0, 63 do poke(i, (i + 1) % 251) end
	M.exports["poison"](6)
	return M.exports["viag"]()
end)()`, fmt.Sprintf("%d", word(6)))
}

// The same global read when nothing misaligned is ever stored into it, so the
// class survives and the branch goes. Without this the test above passes for
// the wrong reason -- an analysis that proved nothing about globals at all.
func TestAnAlignedGlobalStillProvesItsLoads(t *testing.T) {
	sameAtEveryLevel(t, alignWAT,
		`(function()
	local poke = M.exports["poke"]
	for i = 0, 63 do poke(i, (i + 1) % 251) end
	M.exports["seth"](3)
	return M.exports["viah"]()
end)()`, fmt.Sprintf("%d", word(24)))
}

// The one door in the induction: persist.setglobals writes a value from
// `storage` straight into a global, and a save written by a DIFFERENT build
// reached through fk_migrate is not covered by the module's own reasoning.
//
// The guard turns that from a fractional table index deep inside guest code
// into a named error at the point of restore.
func TestRestoringAMisalignedGlobalIsRefusedByName(t *testing.T) {
	src := emitAt(t, alignWAT, analysis.O3)
	if !strings.Contains(src, "setglobals = function(t)") {
		t.Fatalf("no setglobals in the emitted persist surface")
	}
	// $h is proved 8-aligned; $g is poisoned and proves nothing, so only $h is
	// guarded -- a guard on a global nothing was proved about would be a check
	// that can never fire.
	if !strings.Contains(src, "if g1 % 8 ~= 0 then error(") {
		t.Errorf("no guard on the global whose class the emitter relied on:\n%s",
			persistBlock(src))
	}
	if strings.Contains(src, "if g0 %") {
		t.Errorf("guarded a global nothing was proved about:\n%s", persistBlock(src))
	}
}

// A module with no proved global gets no guard at all, so nothing pays for this
// who does not use it.
func TestNoGuardWhenNothingWasProvedAboutAGlobal(t *testing.T) {
	src := emitAt(t, `(module (memory 1)
		(global $g (mut i32) (i32.const 0))
		(func (export "set") (param $v i32) (global.set $g (local.get $v)))
		(func (export "f") (result i32) (i32.load (global.get $g))))`, analysis.O3)
	if strings.Contains(src, "then error(\"fklua: restored global") {
		t.Errorf("emitted a guard for a global nothing was proved about:\n%s",
			persistBlock(src))
	}
}

func persistBlock(src string) string {
	i := strings.Index(src, "  persist = {")
	if i < 0 {
		return "(no persist block)"
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n  },"); j >= 0 {
		return rest[:j]
	}
	return rest
}
