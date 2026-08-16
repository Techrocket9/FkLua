package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// instantiate builds a driver that loads a compiled module with a host imports
// table and runs `body` against it as `M`.
//
// The chunk is wrapped in a vararg function rather than loaded as a chunk of
// its own, because lua52f has no loadfile and no io: everything a test runs has
// to be one source file, which is also how a Factorio mod ships.
func instantiate(t *testing.T, wat, imports, body string) (string, error) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

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

	var b strings.Builder
	b.WriteString("local M\nlocal mk = function(...)\n")
	b.WriteString(src)
	b.WriteString("\nend\nlocal ok, r = pcall(mk, " + imports + ")\n")
	b.WriteString(`if not ok then
  print("INSTANTIATE-FAILED\t" .. tostring(type(r) == "table" and (r.fk_trap or r.fk_unsupported) or r))
  return
end
M = r
`)
	b.WriteString(body)
	return h.RunString(b.String())
}

// A call to an imported function and a call to a defined one are the same
// `F[n](...)` in generated code, because imports are bound into F at their own
// index. This is the test that the index really is theirs.
func TestImportedCallReachesTheHost(t *testing.T) {
	out, err := instantiate(t, `(module
		(import "env" "host_add" (func $host_add (param i32 i32) (result i32)))
		(func $local_add (param i32 i32) (result i32)
			(i32.add (local.get 0) (local.get 1)))
		(func (export "both") (param i32 i32) (result i32)
			(i32.add (call $host_add (local.get 0) (local.get 1))
			         (call $local_add (local.get 0) (local.get 1))))
		(export "just_host" (func $host_add)))`,
		`{ env = { host_add = function(a, b) calls = (calls or 0) + 1; return (a + b) % 4294967296.0 end } }`,
		`print(M.exports.both(20, 3))
print(M.exports.just_host(1, 2))
print(calls)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "46\n3\n2"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got %q, want %q\n%s", got, want, out)
	}
}

// A missing import is caught at instantiation, not at the first call. A guest
// may not reach a rarely-used import until deep into a game, and "attempt to
// call a nil value" at that point names nothing useful.
func TestMissingImportFailsAtInstantiation(t *testing.T) {
	out, err := instantiate(t, `(module
		(import "env" "fk_log" (func $log (param i32 i32)))
		(func (export "f") (result i32) (i32.const 1)))`,
		`{ env = {} }`,
		`print("REACHED-BODY")`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "REACHED-BODY") {
		t.Errorf("instantiation succeeded without the import being supplied:\n%s", out)
	}
	for _, want := range []string{"env.fk_log", "(i32, i32) -> ()", "not supplied"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure message should mention %q:\n%s", want, out)
		}
	}
}

// A host function is on the same side of Invariant A as generated code: an i64
// crosses the boundary as an unsigned (lo, hi) pair of doubles, using Lua's
// native multiple values in both directions.
func TestImportedI64CrossesAsLoHiPair(t *testing.T) {
	out, err := instantiate(t, `(module
		(import "env" "wide" (func $wide (param i64) (result i64)))
		(func (export "f") (param i64) (result i64) (call $wide (local.get 0))))`,
		`{ env = { wide = function(lo, hi) seen = lo .. "," .. hi; return 7, 9 end } }`,
		`local lo, hi = M.exports.f(4294967295, 2)
print(seen)
print(lo .. "," .. hi)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "4294967295,2\n7,9"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// wasm has no string type, so anything textual crosses as a (pointer, length)
// pair into linear memory. read_string is what makes that pair followable from
// the host side; without it an imported fk_log has nothing to log.
func TestReadStringCrossesTheMemoryBoundary(t *testing.T) {
	out, err := instantiate(t, `(module
		(import "env" "fk_log" (func $log (param i32 i32)))
		(memory 1)
		(data (i32.const 16) "hello, mod")
		(func (export "greet") (call $log (i32.const 16) (i32.const 10))))`,
		`{ env = { fk_log = function(p, n) print("[" .. M.read_string(p, n) .. "]") end } }`,
		`M.exports.greet()
-- M.mem is the shard VECTOR, so its length is a shard count and the words are
-- one level down. Both are printed: the first says the surface still hands back
-- the memory, the second that the memory is the size it was declared.
print(#M.mem .. "," .. #M.mem[1] * 4)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "[hello, mod]\n1,65536"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A read that runs off the end of linear memory traps like any other
// out-of-bounds access, rather than returning a short string or nil bytes.
func TestReadStringTrapsOutOfBounds(t *testing.T) {
	out, err := instantiate(t, `(module
		(memory 1)
		(func (export "f") (result i32) (i32.const 1)))`,
		`nil`,
		`local ok, e = pcall(M.read_string, 65530, 10)
print(ok, type(e) == "table" and e.fk_trap or tostring(e))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "false\tout of bounds memory access"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An import-free module must not gain a chunk vararg read or an imports table:
// the overwhelming majority of modules have no imports and should pay nothing
// for the ones that do.
func TestImportFreeModuleIsUnchanged(t *testing.T) {
	src := emit(t, `(module (func (export "f") (result i32) (i32.const 1)))`)
	for _, unwanted := range []string{"IMPORTS", "fk_import("} {
		if strings.Contains(src, unwanted) {
			t.Errorf("import-free module emitted %q:\n%s", unwanted, src)
		}
	}
}

// Imports are bound before the start function runs, because a start function is
// exactly where a guest initialises -- and initialisation is the most likely
// place for it to call the host.
func TestImportsAreBoundBeforeStart(t *testing.T) {
	out, err := instantiate(t, `(module
		(import "env" "note" (func $note (param i32)))
		(func $init (call $note (i32.const 42)))
		(start $init)
		(func (export "f") (result i32) (i32.const 1)))`,
		`{ env = { note = function(v) print("start called host with " .. v) end } }`,
		`print(M.exports.f())`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "start called host with 42\n1"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The emitted binding names the import and its signature, so a host wiring the
// wrong arity is told what was expected rather than discovering it at a call
// site with no context.
func TestImportBindingCarriesItsSignature(t *testing.T) {
	src := emit(t, `(module
		(import "env" "fk_log" (func (param i32 i32)))
		(func (export "f") (result i32) (i32.const 1)))`)
	want := fmt.Sprintf("F[0] = fk_import(IMPORTS, %q, %q, %q)", "env", "fk_log", "(i32, i32) -> ()")
	if !strings.Contains(src, want) {
		t.Errorf("expected binding\n  %s\ngot:\n%s", want, src)
	}
}

// Lua caps a function at 200 locals and a chunk is a function. The prelude
// spends most of that budget, so a module with enough globals produces a chunk
// Lua itself rejects -- at the user's game start, with a message naming nothing
// about the module that caused it.
//
// The boundary is pinned against lua52f rather than against the counter, so a
// prelude that grows moves the test rather than quietly moving the cliff.
func TestChunkLocalBudgetIsEnforcedWhereLuaEnforcesIt(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	build := func(n int) string {
		var b strings.Builder
		b.WriteString("(module (memory 1) (table 1 funcref)")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, " (global $g%d (mut i32) (i32.const %d))", i, i)
		}
		b.WriteString(` (func (export "f") (result i32) (global.get 0)))`)
		return b.String()
	}
	compile := func(n int) (string, error) {
		m, err := wasm.DecodeWAT(build(n))
		if err != nil {
			t.Fatalf("%d globals: decode: %v", n, err)
		}
		im, err := ir.BuildModule(m)
		if err != nil {
			t.Fatalf("%d globals: ir: %v", n, err)
		}
		return EmitModule(im)
	}

	// Find the largest module lua52f actually accepts.
	loads := func(src string) bool {
		out, err := h.RunString("local M = (function(...)\n" + src + "\nend)()\nprint('ok')\n")
		return err == nil && strings.Contains(out, "ok")
	}
	last := -1
	// From 1, because the module reads global 0 and a module with no globals
	// would not decode.
	for n := 1; n <= 64; n++ {
		src, err := compile(n)
		if err != nil {
			break
		}
		if !loads(src) {
			t.Fatalf("%d globals: the emitter accepted a chunk lua52f rejects; "+
				"the local counter is undercounting", n)
		}
		last = n
	}
	if last < 1 {
		t.Fatal("even a module with a single global was refused")
	}
	t.Logf("chunk-local budget: %d globals fit, %d does not", last, last+1)

	// And the first one over is refused here, with a message that says why.
	_, err = compile(last + 1)
	if err == nil {
		t.Fatalf("%d globals should have been refused", last+1)
	}
	for _, want := range []string{"200", "global"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should mention %q", err.Error(), want)
		}
	}
}

// READ_STRING IS WORD-WISE NOW, so every alignment and every length has to come
// back byte-identical.
//
// fk_str used to be one string.char call and one table slot per BYTE, while its
// mirror fk_wstr had been batched to four words per string.unpack a milestone
// earlier -- the same load/store asymmetry this project already recorded once,
// when the reason sub-word accesses stayed function calls turned out to have
// been measured for stores and inherited by loads. Batching it means a head, a
// word body and a tail, so the boundaries are where it can now be wrong: a
// pointer 1, 2 or 3 bytes past a word, a length that ends mid-word, and the
// short-string path that skips the machinery entirely.
//
// The oracle is the SAME BYTES WRITTEN BACK. fk_wstr and fk_str are inverses,
// so a round trip through both is the assertion, and it does not depend on this
// test knowing what the word layout is.
func TestReadStringIsExactAtEveryAlignmentAndLength(t *testing.T) {
	out, err := instantiate(t, `(module
		(memory 1)
		(func (export "f") (result i32) (i32.const 1)))`,
		`nil`,
		`
local alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
local bad = 0
for off = 0, 7 do
  for n = 0, 40 do
    local s = string.rep(alphabet, 2):sub(1, n)
    local at = 1024 + off
    M.memio.wstr(at, s)
    local back = M.read_string(at, n)
    if back ~= s then
      bad = bad + 1
      if bad < 4 then print("off " .. off .. " n " .. n .. ": " .. back) end
    end
  end
end
print("mismatches " .. bad)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out); got != "mismatches 0" {
		t.Errorf("got:\n%s", got)
	}
}
