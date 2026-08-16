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

// A DICTIONARY FIELD INSIDE A STRUCT, end to end, through an event payload.
//
// Five event payloads carried a `tags` dictionary and were deferred whole --
// on_built_entity and on_robot_built_entity among them, which are the two
// events a mod that builds things subscribes to, so the first downstream
// consumer read them at hand-derived byte offsets on its most important path.
//
// The Lua side never had the gap: read_value routes K_DICT into the same walk
// an array uses, so a dict inside a struct already crossed. What is new is the
// guest decoder, and the parts of it that can be wrong -- the pair stride, the
// key and value offsets WITHIN the pair, the (ptr, count) header's width -- are
// all past the type checker. This is the only test that can see them.
//
// BOTH LANGUAGES SINCE 2026-08-03, driven by one host stub against one set of
// expectations. This used to say "Go only, deliberately: the Rust generator
// does not carry dictionary fields inside structs yet", and it did not carry
// event payload structs either -- so the fixture that would have shared this
// stub could not have been written. Both landed in the ports round, and the two
// guests agreeing line for line is what says one analysis has two renderings
// rather than two implementations.
func TestADictionaryFieldCrossesInsideAnEventPayload(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p := filepath.Join(tmp, "dict.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/dict", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkDictGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "dict",
			filepath.Join(tmp, "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkDictGuest(t, p)
	})
}

func checkDictGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageDictGuest(t, wasmPath)
	// The host encodes the whole payload eagerly through the real write_struct,
	// which is the path a real dispatch takes -- so the dictionary is written by
	// fk_abi.lua and read by the generated guest, and the two agreeing is the
	// assertion.
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_built_entity = 42, on_console_chat = 24 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-dict",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local ent = { valid = true, name = "iron-chest", object_name = "LuaEntity" }
game = {}
require("control")
handlers[42]({
  entity = ent,
  player_index = 7,
  tick = 1234,
  name = 42,
  tags = { colour = "red", count = 3, live = true },
})
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	want := []string{
		// Three pairs, each with a different tier-2 value tag, so a decoder
		// reading the value from the key's offset says MISSING rather than
		// producing a plausible wrong answer. The fourth is the BINARY tag
		// TestABinaryStringCrossesAGeneratedEventReaderByteExact sends and this
		// script does not -- absent, in both languages, rather than empty or
		// a plausible other tag's bytes.
		"LOG tags: 3 colour='red' count=3 live=true blob=MISSING",
		// The scalars placed AFTER the dictionary in the layout.
		"LOG player=7 tick=1234",
		// ...and a handle from before it.
		"LOG entity=iron-chest",
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], want[i])
		}
	}
}

// packageDictGuest compiles one built dict guest to Lua and packages it as a
// mod, returning the directory to put on package.path.
//
// Extracted so the byte-exactness test can drive the SAME guest with a
// different script: what each asserts differs, what they need built does not.
func packageDictGuest(t *testing.T, wasmPath string) string {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)
	used, complete := factorio.UsedMembers(im)
	if !complete {
		t.Fatal("a member id was not a compile-time constant, so the scan broke")
	}
	usedEv, evComplete := factorio.UsedEvents(im)
	if !evComplete {
		t.Fatal("an event id was not a compile-time constant, so the scan broke")
	}
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	var builtID int
	for _, e := range events.Events {
		if e.Name == "on_built_entity" {
			builtID = e.ID
		}
	}
	if builtID == 0 {
		t.Fatal("on_built_entity has no generated event descriptor, which is " +
			"the whole thing this test exists to check")
	}

	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-dict", Version: "0.1.0", Title: "FkLua dict fields",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, APITable: table,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
