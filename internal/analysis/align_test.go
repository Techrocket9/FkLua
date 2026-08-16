package analysis

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The congruence lattice, from underneath.
//
// The emitter's tests prove the pass produces the right Lua; these prove the
// lattice itself has the shape the soundness argument needs. UNKNOWN is the TOP
// and an absent key means UNKNOWN -- get that backwards and the analysis proves
// everything, looks like it works, and is wrong about most of it.

func TestUnknownIsTheTopAndProvesNothing(t *testing.T) {
	if CongTop.DividesBy(2) || CongTop.DividesBy(4) || CongTop.DividesBy(8) {
		t.Error("the top must prove nothing at all")
	}
	if CongTop.divisor() != 1 {
		t.Errorf("top divisor = %d, want 1", CongTop.divisor())
	}
	// Joining anything with the top yields the top, in both orders.
	four := cong(8, 4)
	if got := four.join(CongTop); got.DividesBy(2) {
		t.Errorf("join with the top proved %v", got)
	}
	if got := CongTop.join(four); got.DividesBy(2) {
		t.Errorf("join with the top proved %v", got)
	}
}

func TestJoinKeepsOnlyWhatBothAgreeOn(t *testing.T) {
	cases := []struct {
		a, b Cong
		div  uint32
	}{
		{cong(8, 0), cong(8, 0), 8},
		{cong(8, 0), cong(8, 4), 4}, // 0 and 4 agree modulo 4
		{cong(8, 0), cong(8, 2), 2}, // and modulo 2
		{cong(8, 0), cong(8, 1), 1}, // and not at all
		{cong(8, 4), cong(4, 0), 4},
		{cong(4, 2), cong(8, 6), 2},
	}
	for _, c := range cases {
		if got := c.a.join(c.b).divisor(); got != c.div {
			t.Errorf("join(%v, %v) divides by %d, want %d", c.a, c.b, got, c.div)
		}
		// Join is symmetric, or a merge would depend on predecessor order --
		// which would make generated code depend on graph traversal order, and
		// determinism is a correctness property here.
		if got := c.b.join(c.a).divisor(); got != c.div {
			t.Errorf("join(%v, %v) divides by %d, want %d (asymmetric)", c.b, c.a, got, c.div)
		}
	}
}

// An absent key is the top. congLocals.join is a plain intersection of key sets
// for exactly this reason: a predecessor that knows nothing about a local has to
// poison the merge, not be ignored by it.
func TestAnAbsentLocalIsUnknownNotAligned(t *testing.T) {
	known := congLocals{1: cong(8, 0)}
	empty := congLocals{}
	if got := known.join(empty); len(got) != 0 {
		t.Errorf("joining with a state that knows nothing kept %v", got)
	}
	if got := empty.join(known); len(got) != 0 {
		t.Errorf("joining with a state that knows nothing kept %v", got)
	}
}

// addrOf is the congruence the analysis derived for the LAST memory access in
// the named export -- last, so a case that loads through a loaded pointer is
// asking about the outer load rather than the one that fetched the address.
func addrOf(t *testing.T, wat, export string) Cong {
	t.Helper()
	m := build(t, wat)
	ga := Globals(m)
	fi := -1
	for _, e := range m.Source.Exports {
		if e.Name == export {
			fi = int(e.FuncIndex)
		}
	}
	if fi < 0 {
		t.Fatalf("no export %q", export)
	}
	for _, f := range m.Funcs {
		if int(f.Index) != fi {
			continue
		}
		a := Aligns(f, Ranges(f), ga)
		for i := len(f.Steps) - 1; i >= 0; i-- {
			if isMemAccess(f.Steps[i].Op) {
				return a.Addr[i]
			}
		}
		t.Fatalf("export %q has no memory access", export)
	}
	t.Fatalf("no function with index %d", fi)
	return CongTop
}

func TestTransferFunctionsProveWhatTheyClaim(t *testing.T) {
	cases := []struct {
		name string
		body string
		div  uint32
	}{
		{"const", `(i32.load (i32.const 12))`, 4},
		{"const-odd", `(i32.load (i32.const 13))`, 1},
		{"shl2", `(i32.load (i32.shl (local.get 0) (i32.const 2)))`, 4},
		{"shl3", `(i32.load (i32.shl (local.get 0) (i32.const 3)))`, 8},
		{"shl1", `(i32.load (i32.shl (local.get 0) (i32.const 1)))`, 2},
		// A distance of 0 mod 32 is the IDENTITY, and identities absorb
		// nothing -- the same shape that made a deferred wrap unsound.
		{"shl32", `(i32.load (i32.shl (local.get 0) (i32.const 32)))`, 1},
		{"shl-dynamic", `(i32.load (i32.shl (local.get 0) (local.get 0)))`, 1},
		{"mul4", `(i32.load (i32.mul (local.get 0) (i32.const 4)))`, 4},
		{"mul2", `(i32.load (i32.mul (local.get 0) (i32.const 2)))`, 2},
		{"mul3", `(i32.load (i32.mul (local.get 0) (i32.const 3)))`, 1},
		{"mask-low2", `(i32.load (i32.and (local.get 0) (i32.const 4294967292)))`, 4},
		{"mask-all", `(i32.load (i32.and (local.get 0) (i32.const 4294967295)))`, 1},
		{"mask-zero", `(i32.load (i32.and (local.get 0) (i32.const 0)))`, 8},
		{"or", `(i32.load (i32.or (i32.mul (local.get 0) (i32.const 8))
			(i32.mul (local.get 0) (i32.const 4))))`, 4},
		{"xor-odd", `(i32.load (i32.xor (i32.mul (local.get 0) (i32.const 4))
			(local.get 0)))`, 1},
		{"add-aligned", `(i32.load (i32.add (i32.mul (local.get 0) (i32.const 4))
			(i32.const 8)))`, 4},
		{"add-off", `(i32.load (i32.add (i32.mul (local.get 0) (i32.const 4))
			(i32.const 2)))`, 2},
		{"sub", `(i32.load (i32.sub (i32.mul (local.get 0) (i32.const 8))
			(i32.const 4)))`, 4},
		{"rem-8", `(i32.load (i32.rem_u (i32.mul (local.get 0) (i32.const 4))
			(i32.const 8)))`, 4},
		{"rem-6", `(i32.load (i32.rem_u (i32.mul (local.get 0) (i32.const 4))
			(i32.const 6)))`, 2},
		{"shr", `(i32.load (i32.shr_u (i32.mul (local.get 0) (i32.const 8))
			(i32.const 1)))`, 1},
		// Everything not reasoned about is UNKNOWN, and that default is what
		// makes the pass safe to extend.
		{"param", `(i32.load (local.get 0))`, 1},
		{"load", `(i32.load (i32.load (i32.const 0)))`, 1},
		{"call", `(i32.load (call 1))`, 1},
		{"div", `(i32.load (i32.div_u (local.get 0) (i32.const 4)))`, 1},
		{"clz", `(i32.load (i32.clz (local.get 0)))`, 1},
		{"select", `(i32.load (select (i32.mul (local.get 0) (i32.const 8))
			(i32.mul (local.get 0) (i32.const 4)) (local.get 0)))`, 4},
	}
	for _, c := range cases {
		wat := `(module (memory 1)
			(func (export "f") (param i32) (result i32) ` + c.body + `)
			(func (result i32) (i32.const 16)))`
		if got := addrOf(t, wat, "f").divisor(); got != c.div {
			t.Errorf("%s: divides by %d, want %d", c.name, got, c.div)
		}
	}
}

// The memarg offset is part of the effective address, and it is the difference
// between a pass that helps and one that reads four bytes from the wrong place.
func TestTheMemargOffsetIsPartOfTheAddress(t *testing.T) {
	for _, c := range []struct {
		off uint32
		div uint32
	}{{0, 8}, {4, 4}, {8, 8}, {1, 1}, {2, 2}} {
		wat := `(module (memory 1) (func (export "f") (param i32) (result i32)
			(i32.load offset=` + itoa(c.off) + ` (i32.mul (local.get 0) (i32.const 8)))))`
		if got := addrOf(t, wat, "f").divisor(); got != c.div {
			t.Errorf("offset=%d: divides by %d, want %d", c.off, got, c.div)
		}
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// The module-level part: a global's class is the initialiser joined with every
// store the module makes, and one misaligned store anywhere weakens it
// everywhere. That inductive step is what makes this a proof rather than an
// assumption about how LLVM lays out a shadow stack.
func TestAGlobalIsOnlyAsAlignedAsItsWorstStore(t *testing.T) {
	aligned := `(module (memory 1)
		(global $sp (mut i32) (i32.const 65536))
		(func (export "push") (global.set $sp
			(i32.sub (global.get $sp) (i32.const 16))))
		(func (export "f") (result i32) (i32.load (global.get $sp))))`
	if got := addrOf(t, aligned, "f").divisor(); got != 8 {
		t.Errorf("a shadow-stack global stores only 16-aligned values; divides by %d, want 8", got)
	}

	poisoned := `(module (memory 1)
		(global $sp (mut i32) (i32.const 65536))
		(func (export "push") (global.set $sp
			(i32.sub (global.get $sp) (i32.const 16))))
		(func (export "poison") (param i32) (global.set $sp (local.get 0)))
		(func (export "f") (result i32) (i32.load (global.get $sp))))`
	if got := addrOf(t, poisoned, "f").divisor(); got != 1 {
		t.Errorf("one arbitrary store must poison the class; divides by %d, want 1", got)
	}

	// A store that is aligned but to the WRONG residue is not the same thing as
	// an aligned one: 65536 and 65534 are both even and disagree modulo 4.
	skewed := `(module (memory 1)
		(global $sp (mut i32) (i32.const 65536))
		(func (export "skew") (global.set $sp (i32.const 65534)))
		(func (export "f") (result i32) (i32.load (global.get $sp))))`
	if got := addrOf(t, skewed, "f").divisor(); got != 2 {
		t.Errorf("divides by %d, want 2", got)
	}
}

// An immutable global cannot be stored to, so its initialiser is final.
func TestAnImmutableGlobalKeepsItsInitialiser(t *testing.T) {
	wat := `(module (memory 1)
		(global $base i32 (i32.const 32))
		(func (export "f") (result i32) (i32.load (global.get $base))))`
	if got := addrOf(t, wat, "f").divisor(); got != 8 {
		t.Errorf("divides by %d, want 8", got)
	}
}

// A non-i32 global is not tracked at all, and asking about one must not read
// past the end of anything.
func TestAWideGlobalIsNotTracked(t *testing.T) {
	m := build(t, `(module (memory 1)
		(global $x (mut i64) (i64.const 0))
		(func (export "f") (param i32) (result i32) (i32.load (local.get 0))))`)
	ga := Globals(m)
	if ga.At(0).DividesBy(2) {
		t.Errorf("an i64 global was given a congruence: %v", ga.At(0))
	}
	if ga.At(99).DividesBy(2) {
		t.Error("an out-of-range global index must answer with the top")
	}
}

// The fixpoint has to be a function of the module, not of map iteration order.
// Running it repeatedly on freshly built IR is the cheapest way to say so.
func TestTheGlobalFixpointIsDeterministic(t *testing.T) {
	wat := `(module (memory 1)
		(global $a (mut i32) (i32.const 64))
		(global $b (mut i32) (i32.const 6))
		(func (export "p") (param i32)
			(global.set $a (i32.mul (local.get 0) (i32.const 8)))
			(global.set $b (i32.add (global.get $b) (i32.const 4)))
			(global.set $a (i32.and (local.get 0) (i32.const 4294967280))))
		(func (export "f") (result i32) (i32.load (global.get $a))))`
	var first GlobalAlign
	for i := 0; i < 16; i++ {
		got := Globals(build(t, wat))
		if first == nil {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d: %d globals, want %d", i, len(got), len(first))
		}
		for k := range got {
			if got[k] != first[k] {
				t.Fatalf("run %d: global %d = %v, first run said %v", i, k, got[k], first[k])
			}
		}
	}
	if first[1].divisor() != 2 {
		t.Errorf("global $b starts at 6 and grows by 4, so it is even and never a "+
			"multiple of four; divides by %d", first[1].divisor())
	}
}

// A nil analysis is "the optimizer is off", not a crash, and an unsupported
// function still gets an answer for every step.
func TestTheGuardedAccessorsAnswerConservatively(t *testing.T) {
	var a *Align
	if a.AddrDividesBy(0, 4) {
		t.Error("a nil analysis must answer no")
	}
	real := &Align{Addr: []Cong{cong(8, 0)}}
	if real.AddrDividesBy(-1, 4) || real.AddrDividesBy(7, 4) {
		t.Error("an out-of-range step must answer no")
	}
	if !real.AddrDividesBy(0, 4) {
		t.Error("an in-range aligned step must answer yes")
	}
	if real.AddrDividesBy(0, 16) {
		t.Error("nothing above the tracked maximum can be proved")
	}
	if Globals(nil) != nil {
		t.Error("a nil module has no globals")
	}
	un := &ir.Func{Unsupported: errUnsupported{}, Steps: []ir.Step{{Op: wasm.OpNop}}}
	if got := Aligns(un, nil, nil); len(got.Addr) != 1 || got.Addr[0] != CongTop {
		t.Errorf("an unsupported function must still answer the top: %v", got.Addr)
	}
}

type errUnsupported struct{}

func (errUnsupported) Error() string { return "unsupported" }
