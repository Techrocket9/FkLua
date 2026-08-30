package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// THE PERIODIC HOOK, driven through the real control.lua.
//
// LuaBootstrap::on_nth_tick takes a Lua function, so it bound green and could
// never fire -- it is DEFERRED in both guest backends and this is what replaces
// it. What has to be true, and what these two tests assert:
//
//   - an idle guest registers nothing, which is the standing property every
//     one-shot in this file protects;
//   - a guest is handed the PERIOD that fired, so one export serves several
//     timers;
//   - a period disarmed is UNREGISTERED, not left calling into a handler that
//     returns;
//   - zero disarms every period, which is Factorio's own nil-tick reading;
//   - and the armed set survives a save, because Factorio saves no event
//     registration -- the defect storage.fk_deferred exists to prevent, one
//     mechanism over.
//
// The wat guest arms 60 and 180 from fk_on_init, disarms 60 when it is handed an
// event, and disarms everything from the deferred flush that event asks for --
// which is how three of the four gestures are reachable from a stub that can
// only call what control.lua wired.
//
// THE SUBSCRIPTION IS IN _initialize AND THE ARM IS IN fk_on_init, which is not
// a stylistic split. _initialize runs on every load, so the event trigger is
// available after one; fk_on_init runs on a new map and never again, so the only
// thing that can register a period on a LOADED instance is the re-arm out of
// storage. Arming from _initialize would do the re-arm's work silently and the
// save test below would pass over a mechanism that had never run.
func nthTickGuest(t *testing.T, name string, eventID int) string {
	t.Helper()
	return fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(import "fk" "defer" (func $defer (result i32)))
		(import "fk" "on_nth_tick" (func $nth (param i32 i32) (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "_initialize")
			(drop (call $sub (i32.const %d))))
		(func (export "fk_on_init")
			(drop (call $nth (i32.const 60) (i32.const 1)))
			(drop (call $nth (i32.const 180) (i32.const 1))))
		(func (export "fk_on_nth_tick") (param $n i32)
			(i32.store (i32.const 2056) (local.get $n))
			(if (i32.eq (local.get $n) (i32.const 60))
				(then (i32.store (i32.const 2048)
					(i32.add (i32.load (i32.const 2048)) (i32.const 1)))))
			(if (i32.eq (local.get $n) (i32.const 180))
				(then (i32.store (i32.const 2052)
					(i32.add (i32.load (i32.const 2052)) (i32.const 1))))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(drop (call $nth (i32.const 60) (i32.const 0)))
			(drop (call $defer)))
		(func (export "fk_on_deferred")
			(drop (call $nth (i32.const 0) (i32.const 0)))))`, eventID)
}

func nthTickPackage(t *testing.T, name string, eventID int) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	im := buildIR(t, nthTickGuest(t, name, eventID))
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: name, Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"_initialize", "fk_on_init", "fk_on_event",
			"fk_on_deferred", "fk_on_nth_tick", "fk_alloc", "fk_alloc_static",
			"fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The stub's on_nth_tick table, plus the two registrations control.lua makes
// through script.on_event, so a test can count both independently.
const nthTickStub = `
function log(s) end
defines = { events = { on_tick = 1, on_player_created = 2 } }
storage = {}
local handlers = {}
local nth = {}
script = {
  mod_name = %q,
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
  on_nth_tick = function(n, f) nth[n] = f end,
}
local function armed()
  local ks = {}
  for k in pairs(nth) do ks[#ks + 1] = k end
  table.sort(ks)
  return table.concat(ks, ",")
end
`

func TestAPeriodicHookIsArmedPerPeriodAndDisarmedPerPeriod(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	events := GenerateEvents(a)
	var evID int
	for _, e := range events.Events {
		if e.Name == "on_player_created" {
			evID = e.ID
		}
	}
	if evID == 0 {
		t.Fatal("expected an on_player_created event")
	}
	dir := nthTickPackage(t, "fk-nth", evID)

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q`+nthTickStub+`
require("control")
-- AN IDLE GUEST REGISTERS NOTHING. control.lua has been loaded and nothing has
-- asked for a period, so Factorio is not calling in at all.
print("idle [" .. armed() .. "]")
handlers.on_init()
print("armed [" .. armed() .. "]")
nth[60]()
nth[60]()
nth[180]()
print("sixty " .. tostring(storage.fk_mem[1][513]))
print("oneeighty " .. tostring(storage.fk_mem[1][514]))
print("last " .. tostring(storage.fk_mem[1][515]))
-- An event disarms 60 and asks for a flush; the flush disarms everything.
handlers[2]({ player_index = 1, tick = 4 })
print("after one off [" .. armed() .. "]")
handlers[1]({ tick = 5 })
print("after all off [" .. armed() .. "]")
print("storage " .. tostring(storage.fk_nth))
`, filepath.Join(dir, "?.lua"), "fk-nth"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := strings.Join([]string{
		"idle []",
		"armed [60,180]",
		"sixty 2",
		"oneeighty 1",
		"last 180",
		"after one off [180]",
		"after all off []",
		"storage nil",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a period is armed and disarmed by name, the "+
			"guest is handed the period that fired, and zero disarms every one)",
			got, want)
	}
}

// A period armed before a save has to be armed again after it.
//
// FACTORIO SAVES NO EVENT REGISTRATION -- a mod re-registers everything from
// on_load, which is why script.on_event is legal there and script.on_init is
// not. So the armed SET lives in `storage` and the load path re-arms from it.
// Without that a guest that armed a timer, saved, and loaded would find it
// silently gone: the class of defect the deferred flush's own design notes warn
// about, and the reason that flag is in `storage` rather than in an upvalue.
//
// AND THE SET IS RE-ARMED SORTED, which the deferred flush never had to think
// about because it has exactly one registration. `pairs()` over a numeric-keyed
// table is not an order this runtime may bet on, and two peers registering the
// same periods in two orders would be asking Factorio to dispatch them in two
// orders on a tick that is a multiple of both.
func TestAnArmedPeriodSurvivesASaveAndADisarmedOneDoesNot(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	events := GenerateEvents(a)
	var evID int
	for _, e := range events.Events {
		if e.Name == "on_player_created" {
			evID = e.ID
		}
	}
	dir := nthTickPackage(t, "fk-nth-load", evID)

	// Two loads out of one `storage`, exactly as the deferred flush's own save
	// test does it: a fresh chunk, a fresh dispatcher table, and nothing but
	// `storage` crossing.
	out, err := h.RunString(fmt.Sprintf(expandClearLoaded(`
package.path = %q`+nthTickStub+`
local order = {}
script.on_nth_tick = function(n, f)
  nth[n] = f
  if f ~= nil then order[#order + 1] = n end
end

require("control")
handlers.on_init()
print("armed [" .. armed() .. "]")
-- The save happens here. Registrations do not survive it; storage does.
handlers, nth, order = {}, {}, {}
--@CLEAR_LOADED@
require("control")
handlers.on_load()
print("rearmed [" .. armed() .. "]")
print("order [" .. table.concat(order, ",") .. "]")
nth[180]()
print("oneeighty " .. tostring(storage.fk_mem[1][514]))

-- ...and a period disarmed before the save does NOT come back.
handlers[2]({ player_index = 1, tick = 4 })
handlers, nth, order = {}, {}, {}
--@CLEAR_LOADED@
require("control")
handlers.on_load()
print("after disarm [" .. armed() .. "]")
`), filepath.Join(dir, "?.lua"), "fk-nth-load"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := strings.Join([]string{
		"armed [60,180]",
		"rearmed [60,180]",
		"order [60,180]",
		"oneeighty 1",
		"after disarm [180]",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(an armed period is re-armed from storage on "+
			"load, in sorted order, and a disarmed one is not)", got, want)
	}
}

// Arming period zero is refused rather than passed through.
//
// Zero already means "every period" on the disarm side and every-zero-ticks is
// not a schedule; Factorio's own answer to on_nth_tick(0, f) is a raise, and a
// guest whose arithmetic produced a 0 should get a status rather than a mod that
// will not load. A guest with no fk_on_nth_tick export gets ERR_NO_MEMBER, which
// is what every other arming import here answers.
func TestArmingPeriodZeroIsAStatusAndSoIsAMissingExport(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	// Two guests in one: the second exports no fk_on_nth_tick at all. The status
	// each import call returns is written into memory so the stub can read it.
	wat := `(module
		(import "fk" "on_nth_tick" (func $nth (param i32 i32) (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init")
			(i32.store (i32.const 2048) (call $nth (i32.const 0) (i32.const 1)))
			(i32.store (i32.const 2052) (call $nth (i32.const 60) (i32.const 1))))
		(func (export "fk_on_nth_tick") (param $n i32)))`

	im := buildIR(t, wat)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, exports []string) string {
		pkg := &Package{
			Info: Info{Name: name, Version: "0.1.0", Title: "t", Author: "x",
				FactorioVersion: DefaultFactorioVersion},
			Chunk: chunk, Exports: exports, APITable: apiSrc,
		}
		dir, err := pkg.WriteDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// The SAME chunk packaged twice; only the export list differs, which is what
	// control.lua feature-detects on. (The wasm still defines the function; what
	// is under test is the runtime's guard, and Exports is what it reads.)
	full1 := mk("fk-nth-zero", []string{"fk_on_init", "fk_on_nth_tick",
		"fk_alloc", "fk_alloc_static", "fk_free"})

	run := func(dir, name string) string {
		out, err := h.RunString(fmt.Sprintf(`
package.path = %q`+nthTickStub+`
require("control")
handlers.on_init()
print("zero " .. tostring(storage.fk_mem[1][513]))
print("sixty " .. tostring(storage.fk_mem[1][514]))
print("armed [" .. armed() .. "]")
`, filepath.Join(dir, "?.lua"), name))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return strings.TrimSpace(out)
	}

	// ERR_BAD_ARGS is 4, OK is 0 -- the status numbering fk_abi.lua publishes.
	want := "zero 4\nsixty 0\narmed [60]"
	if got := run(full1, "fk-nth-zero"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(period 0 is ERR_BAD_ARGS on the arm side and "+
			"leaves nothing registered)", got, want)
	}
}
