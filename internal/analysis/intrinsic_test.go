package analysis

import "testing"

// NativeIntrinsic decides whether a whole function body may be thrown away and
// replaced by the runtime's own mem_copy/mem_fill. It selects a CANDIDATE by
// name -- and the name section is a custom section carrying no semantics -- so
// the structural check is the only thing standing between a coincidence of
// naming and a miscompile. These pin both halves.

// byteMover is an honest memcpy: a byte loop with C's signature.
const byteMover = `(module (memory 1)
	(func $%s (param $d i32) (param $s i32) (param $n i32) (result i32)
		(local $i i32)
		(block $done
			(loop $top
				(br_if $done (i32.ge_u (local.get $i) (local.get $n)))
				(i32.store8 (i32.add (local.get $d) (local.get $i))
					(i32.load8_u (i32.add (local.get $s) (local.get $i))))
				(local.set $i (i32.add (local.get $i) (i32.const 1)))
				(br $top)))
		(local.get $d))
	(func (export "f") (result i32)
		(call $%s (i32.const 0) (i32.const 8) (i32.const 4))))`

func intrinsicOf(t *testing.T, wat string) Intrinsic {
	t.Helper()
	return NativeIntrinsic(build(t, wat).Funcs[0])
}

func TestTheThreeRecognisedNames(t *testing.T) {
	for name, want := range map[string]Intrinsic{
		"memcpy":  IntrinsicCopy,
		"memmove": IntrinsicCopy,
		"memset":  IntrinsicFill,
	} {
		wat := replaceTwice(byteMover, name)
		if got := intrinsicOf(t, wat); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	// Anything else is compiled as written, however memcpy-shaped it is.
	if got := intrinsicOf(t, replaceTwice(byteMover, "copybytes")); got != NotIntrinsic {
		t.Errorf("an unrecognised name must be compiled: got %v", got)
	}
}

// The structural half. Every one of these is NAMED memcpy and must still be
// compiled, because a name carries no semantics and these are not byte movers.
func TestASameNamedFunctionMustSurviveTheShapeCheck(t *testing.T) {
	cases := []struct{ name, wat string }{
		{"it calls out, so it is not a leaf", `(module (memory 1)
			(func $h (result i32) (i32.const 3))
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (call $h)) (local.get 0)))`},
		{"it reads a global", `(module (memory 1) (global $g i32 (i32.const 7))
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (global.get $g)) (local.get 0)))`},
		{"it writes no memory, so it moves nothing", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.add (local.get 0) (local.get 2))))`},
		{"it does float arithmetic", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0)
					(i32.trunc_f64_u (f64.add (f64.const 1) (f64.const 2))))
				(local.get 0)))`},
		{"it already uses bulk memory, so it needs no help", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(memory.copy (local.get 0) (local.get 1) (local.get 2))
				(local.get 0)))`},
		{"it asks how big memory is", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result i32)
				(i32.store8 (local.get 0) (memory.size)) (local.get 0)))`},
		{"its signature is not C's -- two parameters", `(module (memory 1)
			(func $memcpy (param i32 i32) (result i32)
				(i32.store8 (local.get 0) (local.get 1)) (local.get 0)))`},
		{"its signature is not C's -- no result", `(module (memory 1)
			(func $memcpy (param i32 i32 i32)
				(i32.store8 (local.get 0) (local.get 1))))`},
		{"its signature is not C's -- an i64 parameter", `(module (memory 1)
			(func $memcpy (param i32 i32 i64) (result i32)
				(i32.store8 (local.get 0) (local.get 1)) (local.get 0)))`},
		{"its signature is not C's -- an f64 result", `(module (memory 1)
			(func $memcpy (param i32 i32 i32) (result f64)
				(i32.store8 (local.get 0) (local.get 1)) (f64.const 0)))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intrinsicOf(t, tc.wat); got != NotIntrinsic {
				t.Errorf("replaced a function that is not a byte mover: got %v", got)
			}
		})
	}
}

func TestNativeIntrinsicToleratesNothingToLookAt(t *testing.T) {
	if got := NativeIntrinsic(nil); got != NotIntrinsic {
		t.Errorf("a nil function is not an intrinsic: got %v", got)
	}
}

// replaceTwice fills byteMover's two %s slots with the same name.
func replaceTwice(format, name string) string {
	out := ""
	for i := 0; i < len(format); i++ {
		if i+1 < len(format) && format[i] == '%' && format[i+1] == 's' {
			out += name
			i++
			continue
		}
		out += string(format[i])
	}
	return out
}
