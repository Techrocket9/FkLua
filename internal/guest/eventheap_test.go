package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The message the event carries, and the argument the remote method carries.
// Longer than the 4 KiB string scratch region ON PURPOSE: that is the cliff.
// Below it the host writes into the region and fk_mod.lua reclaims the whole
// thing at the outermost dispatch; above it the host falls back to fk_alloc,
// and until this test existed nothing ever gave those bytes back.
const bigPayload = 5000

// WHAT A HOST-INITIATED DISPATCH KEEPS FOREVER.
//
// TestAHostCallKeepsNoHeap asks this of a GUEST-initiated call and gets 0,
// because every generated binding brackets the marshalling arena itself. This
// asks it of the other direction -- an event Factorio raised, and a remote
// method another mod called -- where there is no binding to take a bracket
// because nothing on the guest side made the call.
//
// It is end to end for the reason the callback test is: the arena lives in the
// generated guest runtime, the fallback that reaches it lives in fk_abi.lua,
// and the bracket that releases it lives in fk_mod.lua. A unit test of any one
// of the three passes with the chain broken, and the leak is silent in every
// other instrument -- the answers stay right, nothing errors, and the only tell
// is a bump pointer nobody was reading.
//
// The gate is ZERO rather than "less than before", for the reason the heap
// test's own gates are: "a little, forever" is the whole complaint. A dispatch
// carrying a payload has no legitimate byte to keep, because the guest either
// copies what it wants into its own memory or does not want it.
func TestAHostInitiatedDispatchKeepsNoHeap(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "eventheap.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"),
		"./examples/eventheap", out); err != nil {
		t.Fatalf("building the eventheap guest: %v", err)
	}
	dir, _ := packageEventHeapGuest(t, root, tmp, out)

	got, err := runEventHeapHost(t, h, dir, bigPayload)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, got)
	}

	// The payload really crossed. Without these two the B/dispatch lines would
	// be measuring a string the host never wrote, which is trivially free.
	for _, want := range []string{
		"LOG event msg " + strconv.Itoa(bigPayload),
		"LOG call arg " + strconv.Itoa(bigPayload),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in the run, so the payload never arrived:\n%s",
				want, got)
		}
	}

	kept := map[string]int{}
	re := regexp.MustCompile(`^LOG (.+): (-?\d+) B/dispatch$`)
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		kept[m[1]] = n
	}
	for _, k := range []string{"event string", "call string"} {
		v, ok := kept[k]
		if !ok {
			t.Fatalf("no measurement for %q; whole run:\n%s", k, got)
		}
		t.Logf("%-13s %5d B/dispatch", k, v)
		if v != 0 {
			t.Errorf("a host-initiated dispatch carrying a %d-byte string kept "+
				"%d B/dispatch of guest heap (%s). The string does not fit the "+
				"4 KiB scratch region, so the host falls back to fk_alloc -- and "+
				"only the outermost dispatch is in a position to give that back, "+
				"because nothing on the guest side made this call",
				bigPayload, v, k)
		}
	}
}

// A GUEST THAT EXPORTS NEITHER STILL RUNS, and this is the half that says the
// two new exports are optional rather than required.
//
// fk_mod.lua feature-detects fk_arena_mark/fk_arena_release as a PAIR, exactly
// as it feature-detects fk_alloc and the scratch region: a guest compiled
// against an older substrate exports none of them and gets precisely the
// behaviour it had, leak included. Making the bracket mandatory would turn every
// mod already in the wild into one that stops loading, which is a far worse
// failure than the one being fixed.
//
// examples/hello is that guest because it imports `fk` and not `fkapi`, so it
// carries none of the marshalling surface at all -- which is what the export
// check below asserts rather than assumes. Without that check this test would
// go on passing while silently exercising the bracketed path, the day hello
// grows an API call.
func TestAGuestWithoutTheArenaBracketStillDispatches(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "hello.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/hello",
		out); err != nil {
		t.Fatalf("building: %v", err)
	}
	dir, exports := packageEventHeapGuest(t, root, tmp, out)
	for _, e := range exports {
		if e == "fk_arena_mark" || e == "fk_arena_release" {
			t.Fatalf("examples/hello exports %q, so this test is exercising the "+
				"BRACKETED path and the optional half is untested. Pick a guest "+
				"that does not import fkapi", e)
		}
	}
	got, err := runEventHeapHost(t, h, dir, 16)
	if err != nil {
		t.Fatalf("a guest with no arena bracket failed to run: %v\n%s", err, got)
	}
	if strings.TrimSpace(got) == "" {
		t.Errorf("the guest produced no output at all:\n%s", got)
	}
}

// packageEventHeapGuest compiles a wasm file into a real mod directory -- the
// generated chunk plus the VERBATIM runtime/lua/fk_mod.lua and fk_abi.lua,
// which is what makes this a test of the shipped runtime rather than of a copy.
func packageEventHeapGuest(t *testing.T, root, tmp, wasmPath string) (string, []string) {
	t.Helper()
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
	used, _ := factorio.UsedMembers(im)
	usedEv, _ := factorio.UsedEvents(im)
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-eventheap", Version: "0.1.0", Title: "FkLua dispatch heap probe",
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
	return dir, pkg.Exports
}

// runEventHeapHost stands up the globals control.lua touches and then raises
// on_console_chat and calls the guest's remote method, warm-up plus window
// times each -- the counts the guest's own `warm` and `iters` expect.
//
// The event is raised through the dispatcher control.lua registered, not
// through a shortcut: the encode this is measuring happens on that path and
// nowhere else.
func runEventHeapHost(t *testing.T, h *luahost.Host, dir string, payload int) (string, error) {
	t.Helper()
	return h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
-- on_console_chat's own defines value. Any number does; what matters is that
-- the guest's subscribe resolved through this table and the raise below uses
-- the same key, which is what a real engine guarantees.
defines = { events = { on_tick = 1, on_console_chat = 23 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-eventheap",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local cmds = {}
commands = {
  add_command = function(name, help, fn) cmds[name] = fn end,
}
local ifaces = {}
remote = {
  add_interface = function(name, fns) ifaces[name] = fns end,
  call = function(iface, fname, ...)
    local i = ifaces[iface]
    if i == nil or i[fname] == nil then
      error("no such interface or method: " .. iface .. "." .. fname, 0)
    end
    return i[fname](...)
  end,
}
game = {}
require("control")
if handlers.on_init then handlers.on_init() end

local msg = string.rep("p", %d)
local raise = handlers[defines.events.on_console_chat]
for i = 1, %d do
  if raise then
    raise({ name = defines.events.on_console_chat, tick = i,
            player_index = 1, message = msg })
  end
  if remote and ifaces["fk-eventheap"] then
    remote.call("fk-eventheap", "send", msg)
  end
end
`, filepath.Join(dir, "?.lua"), payload, eventHeapRuns))
}

// warm + iters from guest/go/examples/eventheap. Spelled once here rather than
// inline, because the guest reports at exactly warm+iters and a mismatch would
// produce no measurement at all rather than a wrong one.
const eventHeapRuns = 3 + 50
