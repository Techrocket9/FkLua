package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The M4 milestone in one test: a Go program becomes a Factorio mod, and the
// mod runs.
//
// Every stage is real. TinyGo compiles guest/go/examples/hello, the decoder
// reads the wasm TinyGo actually emitted, the emitter lowers it, the packager
// writes the mod, and lua52f runs that mod's own control.lua against stand-ins
// for the four Factorio globals it touches. Nothing is stubbed in the middle,
// which is the point: every previous milestone was measured against modules the
// project wrote for itself.
//
// The guest is not a hello-world. It uses a map, a growing slice, string
// formatting, 64-bit multiplication and f64 division, so the assertions below
// are checking arithmetic the emitter has to get right rather than just that
// the pipeline connects.
func TestGoProgramBecomesARunningMod(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	root := repoRoot(t)
	tmp := t.TempDir()

	// 1. Go -> wasm, with the flags the guest substrate documents.
	wasmPath := filepath.Join(tmp, "hello.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/hello", wasmPath); err != nil {
		t.Fatalf("building the guest: %v", err)
	}

	// 2. wasm -> Lua.
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding what TinyGo emitted: %v", err)
	}
	if len(mod.Imports) == 0 {
		t.Fatal("the guest calls fk.Log, so the module must import it; " +
			"a module with no imports means the toolchain optimised the host " +
			"boundary away and this test is no longer testing it")
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			// A raising stub in a real guest is a hole in the milestone, not a
			// detail: the mod loads and then dies whenever that path is reached.
			t.Errorf("function %q did not compile: %v", f.Name, f.Unsupported)
		}
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// 3. Lua -> mod directory.
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-hello", Version: "0.1.0", Title: "FkLua hello",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	if pkg.Inert() {
		t.Fatal("the packaged mod wires no event hook, so it would load and never run")
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}

	// 4. Run the mod's own control.lua under lua52f.
	out, err := h.RunString(factorioStub(dir, 30))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	// The counts are checkable by hand over ticks 1..30, which is why the guest
	// reports them: multiples of 3 that are not multiples of 15 (8), multiples
	// of 5 that are not (4), multiples of 15 (2), and the sum 1+...+30 = 465.
	want := []string{
		"LOG hello from Go, running as Lua inside Factorio",
		"LOG guest built with TinyGo: fnv64(fklua)=449d63cef97b1fda",
		"LOG tick 10 seen=10 fizz=3 buzz=2 fizzbuzz=0 sum=55 mean=5.50",
		"LOG tick 20 seen=20 fizz=5 buzz=3 fizzbuzz=1 sum=210 mean=10.50",
		"LOG tick 30 seen=30 fizz=8 buzz=4 fizzbuzz=2 sum=465 mean=15.50",
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) != len(want) {
		t.Fatalf("expected %d log lines, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if strings.TrimSpace(got[i]) != want[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], want[i])
		}
	}
}

// A mod that never reaches an event handler is not much of a mod, but a guest
// whose fk_on_init raises takes the whole save down with it. The glue does not
// catch that today, and this pins the fact rather than the wish: an error out
// of a handler propagates to Factorio, which is what a Lua mod does too.
func TestGuestErrorsReachTheHost(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// A module whose exported handler traps, packaged the same way a guest is.
	m, err := wasm.DecodeWAT(`(module
		(import "env" "fk_log" (func $log (param i32 i32)))
		(memory 1)
		(func (export "fk_on_init") (unreachable)))`)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{Name: "fk-trap", Version: "0.1.0", Title: "t",
			Author: "t", FactorioVersion: factorio.DefaultFactorioVersion},
		Chunk: src, Exports: []string{"fk_on_init"},
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.RunString(factorioStub(dir, 0))
	if err == nil {
		t.Fatalf("a trapping handler should have raised, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("the trap should name itself; got: %v", err)
	}
}

// factorioStub renders a driver that loads a packaged mod the way Factorio
// does: require("control") with the globals control.lua touches in place.
//
// Only four are needed, which is the useful part -- it means the glue's
// dependency on the game is small enough to state. `game` is deliberately left
// nil, matching control.lua load time, so the fk_print fallback is exercised.
func factorioStub(modDir string, ticks int) string {
	return fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_tick = 1 } }
local handlers = {}
script = {
  mod_name = "fk-hello",
  on_init = function(f) handlers.on_init = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

require("control")

if handlers.on_init then handlers.on_init() end
for tick = 1, %d do
  local f = handlers[defines.events.on_tick]
  if f then f({ tick = tick }) end
end
`, filepath.Join(modDir, "?.lua"), ticks)
}

func repoRoot(t *testing.T) string {
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
