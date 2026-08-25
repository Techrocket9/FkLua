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

// WHAT THE ENGINE SAID, END TO END, THROUGH THE REAL control.lua.
//
// A host call returns an i32 and never raises into wasm, so a failed binding can
// report the KIND of failure -- "the Factorio API raised" -- and nothing else.
// `fk_abi.lua` has recorded the engine's own message since the ABI existed, in
// `M.last_error`, and no import carried it: the sentence was reachable from this
// repo's own tests and from nowhere a mod could stand.
//
// What it is for is a TRIPWIRE. Factorio refuses a documented subset of
// `script.raise_event` outright, and a downstream suite that asserts only
// ok=false cannot tell "refused because that event is not raiseable" from
// "refused for some other reason" -- so the day the engine starts allowing one,
// the run goes on passing over a path that has silently become testable for
// real. Asserting the exact text is what makes that day loud.
//
// END TO END on purpose, and in both languages against ONE stub. Every part of
// this is a seam between separately-correct things: a message recorded in
// `fk_abi.lua`, an import in `fk_mod.lua` writing bytes into guest memory, and a
// hand-written wrapper reassembling them -- and the two wrappers DIFFER, because
// Go needs a package-level scratch (TinyGo's ptrtoint defeats stack promotion)
// where Rust does not. A unit test of any one of them passes with the chain
// broken.
func TestTheEngineSMessageReachesTheGuest(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p := filepath.Join(t.TempDir(), "lasterror.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"),
			"./examples/lasterror", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkLastErrorGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"),
			"lasterror", filepath.Join(t.TempDir(), "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkLastErrorGuest(t, p)
	})
}

// The message the raising leg asserts, chosen to look like an engine refusal --
// which is what a downstream tripwire is really matching on.
const luaRefusal = `on_player_mined_entity (ID 76) (76) can't be raised through script.`

// And the one the truncation leg asserts: 300 bytes, longer than either
// wrapper's 256-byte buffer, with a distinguishable head and tail so a message
// that arrived SHORT reads as one rather than as a plausible other message.
const longHead, longTail = "HEADHEAD", "TAILTAIL"

func checkLastErrorGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageLastErrorGuest(t, wasmPath)

	// `game` is shaped the way Factorio's is: a LuaObject whose __index RAISES
	// for some keys rather than returning nil. That is not a convenience -- it
	// is the exact reason the member read sits inside a pcall, and it is where
	// the message this test is about comes from.
	//
	// error(msg, 0) rather than error(msg): level 0 adds no "chunk:line:"
	// prefix, so the text the guest reads is the text written here. Factorio's
	// own refusals arrive without a prefix too.
	long := longHead + strings.Repeat("x", 300-len(longHead)-len(longTail)) + longTail
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = {} }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-lasterror",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
game = setmetatable({}, { __index = function(_, k)
  if k == "tick" then error(%q, 0) end
  if k == "ticks_played" then error(%q, 0) end
  if k == "speed" then return 1.0 end
  return nil
end })
require("control")
if handlers.on_init then handlers.on_init() end
`, filepath.Join(dir, "?.lua"), luaRefusal, long))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	want := []string{
		// Nothing has failed, so the slot is empty rather than holding
		// something a previous session left.
		"LOG empty: []",
		// Status 5 is ERR_CALL_FAILED: the API raised. The message says WHAT it
		// raised. Both are asserted, because a message with the wrong status
		// beside it would be a guest reading a stale slot -- and the status
		// travels as a NUMBER because Go's error convention prefixes "fklua: "
		// and Rust's Status::as_str does not, and one set of expectations for
		// two renderings is the whole point of driving both guests here.
		"LOG raised: st=5 len=" + fmt.Sprint(len(luaRefusal)) +
			" msg=[" + luaRefusal + "]",
		// A CALL THAT SUCCEEDED CLEARS IT. Without M.call's clear this line
		// reads back the refusal above, which is the whole defect that shape
		// prevents: a stale sentence is indistinguishable from a fresh one.
		"LOG after-ok: []",
		// 300 bytes through a 256-byte buffer. The head AND the tail, because a
		// message that arrived truncated has the right head and the wrong tail.
		"LOG long: len=300 head=" + longHead + " tail=" + longTail,
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

// packageLastErrorGuest compiles one built guest to Lua and packages it as a
// mod -- the generated chunk plus the VERBATIM runtime/lua/fk_mod.lua and
// fk_abi.lua, which is what makes this a test of the shipped runtime rather than
// of a copy of it.
func packageLastErrorGuest(t *testing.T, wasmPath string) string {
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
	report, events := factorio.GenerateMembers(a), factorio.GenerateEvents(a)
	used, complete := factorio.UsedMembers(im)
	if !complete {
		t.Fatal("a member id was not a compile-time constant, so the scan broke")
	}
	usedEv, _ := factorio.UsedEvents(im)
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-lasterror", Version: "0.1.0", Title: "FkLua last_error",
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
