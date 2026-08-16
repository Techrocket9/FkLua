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

// THE CALLBACK SEAM, END TO END, THROUGH THE REAL control.lua.
//
// Three members of the API are unbindable and always will be:
// `LuaCommandProcessor::add_command` and `LuaRemote::add_interface` take a Lua
// FUNCTION, and `LuaRemote::call` is the API's one variadic method. They are the
// four host skips in `api/<version>/census.json` less
// `LuaBootstrap::get_event_handler`, and between them they put an entire genre
// of mod out of reach: anything with a console command, and anything that talks
// to another mod in either direction. Reported four times by fklua-ports (its
// AD7, G6 and FTS4).
//
// The seam is that the FUNCTION DOES NOT CROSS. The host synthesises a Lua
// closure, gives that to Factorio, and dispatches back into the guest by an id
// the guest chose -- which is what `subscribe` has always done for events. The
// design is in runtime/lua/fk_mod.lua's "Commands and remote interfaces"
// section; this drives it.
//
// It is an end-to-end test on purpose. Every part of this path is a seam between
// two things that are separately correct: a tier-2 descriptor written by the
// generated guest and read by `fk_abi.lua`, a closure built in `fk_mod.lua` and
// held by Factorio, an export found by name, an argument list encoded by
// `write_varargs` and decoded by `readDyn`, and a result written by the guest and
// read back by the host. A unit test of any one of them would pass with the
// chain broken.
func TestACommandAndARemoteInterfaceReachTheGuest(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "callback.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"),
		"./examples/callback", out); err != nil {
		t.Fatalf("building the callback guest: %v", err)
	}
	dir := packageCallbackGuest(t, root, tmp, out)

	// The stubs are Factorio's shape and not a convenience: `commands` and
	// `remote` are what a real control.lua registers against, and the closures
	// they capture are the thing under test. `remote.call` looks the registered
	// function up the way the engine does, so the outbound leg really does go
	// out through the host and back in through the same trampoline.
	got, err := runCallbackHost(t, h, dir, `
-- Remote methods, called the way another mod would.
print("add " .. tostring(remote.call("fk-callback-demo", "add", 20, 22)))
print("greet " .. tostring(remote.call("fk-callback-demo", "greet", "world")))

-- THE ARITY CASE. f(1, nil, 3) is THREE arguments, and a trampoline that built
-- its array with {...} would report one -- silently, and only for a caller that
-- passed a hole. See H.write_varargs.
print("arity " .. tostring(remote.call("fk-callback-demo", "arity", 1, nil, 3)))
print("arity0 " .. tostring(remote.call("fk-callback-demo", "arity")))

-- A method that writes nothing must read back as nil rather than as the
-- previous call's result, because the result slot is a reused buffer.
print("noret " .. tostring(remote.call("fk-callback-demo", "no_return", 1)))
print("noret2 " .. tostring(remote.call("fk-callback-demo", "no_return", 1)))

-- A command typed at the console. Its handler calls back OUT through
-- fk.remote_call from inside its own dispatch, which is the re-entrant case.
commands.__invoke("fk-echo", { name = "fk-echo", tick = 77, player_index = 3,
  parameter = "hello world" })
handlers[1]({ tick = 1 })
`)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, got)
	}

	want := []string{
		// Remote methods, in and back out with a result.
		"add 42",
		"greet hello, world",
		// Three arguments including the hole, and zero arguments.
		"arity 3",
		"arity0 0",
		// Nothing written, twice, so the second is not the first's leftovers.
		"noret nil",
		"noret2 nil",
		// The command reached the guest and its CustomCommandData decoded.
		"LOG cmd fk-echo param=hello world tick=77",
		// remote.call made BY the guest, from inside the command's own dispatch.
		"LOG outbound 9",
		// A missing interface is ERR_CALL_FAILED (5), not a trap.
		"LOG missing 5",
		// ...and the OUTER invocation's arguments survived both nested calls
		// encoding their own into the same scratch region.
		"LOG still hello world",
		// One command plus seven remote invocations: six from the host and one
		// the guest made itself, which lands in the same export.
		"LOG calls 8",
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(want), len(lines), got)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, lines[i], want[i])
		}
	}
}

// A GUEST THAT EXPORTS NO fk_on_call CANNOT REGISTER ANYTHING, and says so
// rather than installing a closure that would raise when a player typed the
// command.
//
// It is the same shape `subscribe` already has -- "if not E.fk_on_event then
// return H.ERR_NO_MEMBER" -- and it matters more here, because the failure would
// otherwise surface as a Lua error inside somebody's console months later
// instead of as a status at load.
func TestRegisteringWithoutTheExportIsRefused(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "hello.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/hello",
		out); err != nil {
		t.Fatalf("building: %v", err)
	}
	dir := packageCallbackGuest(t, root, tmp, out)
	got, err := runCallbackHost(t, h, dir, `
print("registered " .. tostring(commands.__count()))
`)
	if err != nil {
		t.Fatalf("running: %v\n%s", err, got)
	}
	if !strings.Contains(got, "registered 0") {
		t.Errorf("a guest with no fk_on_call export registered a command anyway:\n%s",
			got)
	}
}

// packageCallbackGuest compiles a wasm file into a real mod directory -- the
// generated chunk plus the VERBATIM runtime/lua/fk_mod.lua and fk_abi.lua, which
// is what makes this a test of the shipped runtime rather than of a copy.
func packageCallbackGuest(t *testing.T, root, tmp, wasmPath string) string {
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
			Name: "fk-callback", Version: "0.1.0", Title: "FkLua callbacks",
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

// runCallbackHost stands up the Factorio globals control.lua touches, plus
// `commands` and `remote` behaving the way the engine's do: add_command and
// add_interface STORE the closure they are given, and remote.call looks one up
// and calls it. `__invoke` and `__count` are the test's own handles on that
// store and are not part of any API.
func runCallbackHost(t *testing.T, h *luahost.Host, dir, body string) (string, error) {
	t.Helper()
	return h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-callback",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local cmds = {}
commands = {
  add_command = function(name, help, fn) cmds[name] = fn end,
  __invoke = function(name, data) return cmds[name](data) end,
  __count = function() local n = 0 for _ in pairs(cmds) do n = n + 1 end return n end,
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
%s
`, filepath.Join(dir, "?.lua"), body))
}
