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
-- A MOD-DEFINED EVENT, raised the way its publisher would raise it. The guest
-- asked the publisher for the id during _initialize and subscribed to the
-- NUMBER; nothing about 240 was in the wasm.
handlers[240]({ payload = "from-runtime-id", tick = 3 })
-- ...and the other spelling, a custom-event prototype's NAME.
handlers["fk-demo-custom-event"]({ payload = "from-prototype", tick = 4 })
-- The name the engine refused registered nothing, so nothing is dispatchable
-- under it -- which is the ROLLBACK, not merely the refusal.
print("absent registered " .. tostring(handlers["fk-absent-event"] ~= nil))

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
		// THE THIRD REGISTER KIND, from _initialize: the guest asked a publisher
		// for a runtime-minted id and subscribed to it, subscribed to a
		// custom-event prototype by name, and was refused a name this game has no
		// prototype for -- 3 is ERR_NO_MEMBER, which is what a subscription to
		// something absent has always answered.
		"LOG modevent id 240 st 0",
		"LOG modevent named st 0",
		// The engine's OWN WORDS reach the log and the mod keeps running, which is
		// what the pcall around a named registration is for.
		"LOG fklua: script.on_event refused the event name fk-absent-event: " +
			"Unknown event name: fk-absent-event. The guest will not receive it. " +
			"The mod keeps running.",
		"LOG modevent absent st 3",
		// TWICE, which is the ROLLBACK: the host cleared its own dispatcher list
		// when the engine refused, so a second attempt at the same name is refused
		// too rather than appending to a list nothing dispatches.
		"LOG fklua: script.on_event refused the event name fk-absent-event: " +
			"Unknown event name: fk-absent-event. The guest will not receive it. " +
			"The mod keeps running.",
		"LOG modevent absent2 st 3",
		// Both spellings deliver, and the payload is one tier-2 value.
		"LOG modevent runtime payload=from-runtime-id args=1",
		"LOG modevent named payload=from-prototype args=1",
		"absent registered false",
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
		// ...and so is a missing METHOD on an interface that IS there, which is
		// the other half of the Lua guard idiom and is why neither half is needed
		// here: the status answers both without copying every interface name in
		// the save into the guest.
		"LOG nomethod 5",
		// ...and the OUTER invocation's arguments survived both nested calls
		// encoding their own into the same scratch region.
		"LOG still hello world",
		// One command, seven remote invocations (six from the host and one the
		// guest made itself; the two that fail never reach the guest) and two
		// mod-defined events -- ALL TEN through the one
		// fk_on_call export, which is the seam's own claim about the third kind
		// needing no second entry point.
		"LOG calls 10",
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

// THE SIMULATION BRIDGE, to the exact extent it can be verified without a
// graphical client.
//
// A SimulationDefinition's `init` is run as a SILENT CONSOLE COMMAND inside the
// simulation, so it can never `require` and can never load a compiled module --
// that is a property of the entry point rather than a gap in the compiler. What
// FkLua owes it is the one-line bridge: a simulation that lists the mod in
// `SimulationDefinition.mods` (documented as "an array of mods whose runtime
// scripts will be loaded for this simulation") and calls into the mod's REMOTE
// SEAM keeps the whole screenplay in the guest.
//
// WHAT THIS PINS is that the init string is EXECUTABLE and REACHES THE SEAM: the
// exact text a SimulationDefinition would carry, loaded as a chunk with no
// `require` available to it, against a state where the guest has registered its
// interface. What it cannot pin is that a real simulation's Lua state has that
// interface in it, which needs a client that renders one -- and a headless
// Factorio never runs a simulation at all (measured: zero mentions across
// --dump-data, --create and --benchmark, and no flag in --help). The prototype
// half IS verified in a real 2.0.77: a factoriopedia_simulation carrying `mods`
// and `init` loads and reaches data-raw-dump.json verbatim.
//
// AND THE ENGINE DOES NOT VALIDATE THE STRING AT LOAD -- measured, an init with
// an unbalanced parenthesis loads with exit 0 and is stored as written. So "the
// prototype loads" is no evidence at all that the init will run, which is
// exactly why this test evaluates the string rather than trusting the dump.
func TestASimulationsInitStringReachesTheRemoteSeam(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "callback.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"),
		"./examples/callback", out); err != nil {
		t.Fatalf("building the callback guest: %v", err)
	}
	dir := packageCallbackGuest(t, root, tmp, out)

	// The literal a SimulationDefinition would carry. It goes into the Lua as a
	// long string so the quotes inside it are the ones a prototype would hold,
	// unescaped and unchanged.
	got, err := runCallbackHost(t, h, dir, `
-- A console command has no require, which is the whole reason the bridge is a
-- remote call rather than a module load. The chunk is given an environment with
-- require REMOVED, so a recipe that quietly depended on one fails here rather
-- than in somebody's main menu.
local sandbox = { remote = remote }
-- The result is ASSIGNED rather than returned, because a console command
-- discards what its last expression evaluates to -- which is one of the
-- restrictions that makes this an init STRING rather than a function.
local init = [[greeting = remote.call("fk-callback-demo", "greet", "simulation")]]
local f, lerr = load(init, "simulation-init", "t", sandbox)
print("SIM loaded " .. tostring(f ~= nil) .. " " .. tostring(lerr))
local ok, err_ = pcall(f)
print("SIM ran " .. tostring(ok) .. " " .. tostring(err_))
print("SIM reached " .. tostring(sandbox.greeting))
print("SIM norequire " .. tostring(sandbox.require == nil))
`)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, got)
	}
	// The tail, because the guest's own registration lines come first: what is
	// under test is the four lines the init string produced.
	want := []string{
		"LOG loaded true nil",
		"LOG ran true nil",
		"LOG reached hello, simulation",
		"LOG norequire true",
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "SIM ") {
			lines = append(lines, "LOG "+strings.TrimPrefix(l, "SIM "))
		}
	}
	if len(lines) != len(want) {
		t.Fatalf("expected %d SIM lines, got %d:\n%s", len(want), len(lines), got)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s\n(the init string a simulation "+
				"would carry has to be executable as a bare chunk and has to reach "+
				"the guest through the remote seam)", i+1, lines[i], want[i])
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
  -- A NAME THIS GAME HAS NO PROTOTYPE FOR RAISES, which is the engine's own
  -- behaviour and is what the register kind's pcall is for. Measured on 2.0.77
  -- during the custom-input round, as: Unknown event name: NAME. Modelled here
  -- the way remote.call's "no such interface" already is.
  on_event = function(ev, f)
    if type(ev) == "string" and ev:sub(1, 10) == "fk-absent-" then
      error("Unknown event name: " .. ev, 0)
    end
    handlers[ev] = f
  end,
}
local cmds = {}
commands = {
  add_command = function(name, help, fn) cmds[name] = fn end,
  __invoke = function(name, data) return cmds[name](data) end,
  __count = function() local n = 0 for _ in pairs(cmds) do n = n + 1 end return n end,
}
-- The PUBLISHER of a mod-defined event: the shape an LTN-style hub has, where
-- the id is minted at runtime and handed out through a remote interface. 240 is
-- an arbitrary number standing in for what generate_event_name() returned.
local ifaces = { ["fk-event-publisher"] = { event_id = function() return 240 end } }
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
