package guest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// M8's gate: the same guest program, through two toolchains, producing
// IDENTICAL results.
//
// Two programs that merely both work would not test what this milestone exists
// to test. TinyGo and Rust disagree about almost everything above the wasm --
// GC'd interfaces and multiple returns against ownership, traits and Result --
// so a compiler that carries both is one whose semantics are not accidentally
// shaped like either. The programs are written as line-for-line mirrors and
// their output is compared byte for byte, with only the toolchain's own name
// allowed to differ.
//
// Every stage is real on both sides: the guest's own toolchain, the decoder
// reading what it actually emitted, the emitter, and lua52f running the result.
func TestBothToolchainsAgree(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	root := repoRoot(t)
	tmp := t.TempDir()

	// Go.
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	goWasm := filepath.Join(tmp, "go-hello.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/hello", goWasm); err != nil {
		t.Fatalf("building the Go guest: %v", err)
	}

	// Rust.
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	rsWasm, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "hello",
		filepath.Join(tmp, "cargo"))
	if err != nil {
		t.Fatalf("building the Rust guest: %v", err)
	}

	goOut := runGuest(t, h, goWasm, 30)
	rsOut := runGuest(t, h, rsWasm, 30)

	// The two lines that name their own toolchain are the only licensed
	// difference, and they are normalised rather than skipped -- dropping them
	// would also drop the FNV hash, which is the one value in the whole run
	// computed by 64-bit arithmetic.
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "hello from Go,", "hello from LANG,")
		s = strings.ReplaceAll(s, "hello from Rust,", "hello from LANG,")
		s = strings.ReplaceAll(s, "built with TinyGo:", "built with LANG:")
		s = strings.ReplaceAll(s, "built with Rust:", "built with LANG:")
		return s
	}
	if norm(goOut) != norm(rsOut) {
		t.Errorf("the toolchains disagree.\n--- TinyGo ---\n%s\n--- Rust ---\n%s", goOut, rsOut)
	}
	// And the run has to have actually done something. Comparing two empty
	// strings would pass forever.
	if n := len(strings.Split(strings.TrimSpace(goOut), "\n")); n < 5 {
		t.Fatalf("expected at least 5 output lines, got %d:\n%s", n, goOut)
	}
	t.Logf("identical across both toolchains:\n%s", norm(goOut))
}

// runGuest compiles a guest module and drives it for n ticks, returning
// everything it logged.
func runGuest(t *testing.T, h *luahost.Host, wasmPath string, ticks int) string {
	t.Helper()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding %s: %v", filepath.Base(wasmPath), err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir for %s: %v", filepath.Base(wasmPath), err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("%s: function %q did not compile: %v",
				filepath.Base(wasmPath), f.Name, f.Unsupported)
		}
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatalf("emit for %s: %v", filepath.Base(wasmPath), err)
	}

	// The imports must exist BEFORE the chunk runs: fk_import resolves them at
	// instantiation and refuses to load if one is missing.
	var b strings.Builder
	b.WriteString("local seen = {}\n")
	b.WriteString("local function rec(p, n) seen[#seen+1] = { p, n } end\n")
	b.WriteString("local IMP = { env = { fk_log = rec, fk_print = rec } }\n")
	b.WriteString("local M = (function(...)\n")
	b.WriteString(chunk)
	b.WriteString("\nend)(IMP)\n")
	b.WriteString(`
local init = M.exports["_initialize"] if init then init() end
local oi = M.exports["fk_on_init"] if oi then oi() end
local ot = M.exports["fk_on_tick"]
`)
	b.WriteString("for t = 1, ")
	b.WriteString(itoa(ticks))
	b.WriteString(` do ot(t) end
local o = {}
for i, e in ipairs(seen) do o[i] = M.read_string(e[1], e[2]) end
print(table.concat(o, "\n"))
`)
	out, err := h.RunString(b.String())
	if err != nil {
		t.Fatalf("running %s: %v\n%s", filepath.Base(wasmPath), err, out)
	}
	return strings.TrimSpace(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
