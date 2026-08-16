package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// A blueprint paste is P SEPARATE dispatches in one tick, so batching cannot
// hang off the end of a dispatch.
//
// This is the measurement the downstream report's "end-of-dispatch hook" asked
// for and the reason it would not have worked. Factorio raises one
// on_built_entity per entity, each from the engine's own loop, so each is its
// own OUTERMOST dispatch: `depth` goes 0 -> 1 -> 0 P times. A hook at
// dispatch_done therefore fires P times and batches nothing.
//
// What does batch is a deferred queue flushed once per tick: the guest calls
// fk.defer() as many times as it likes, control.lua arms a ONE-SHOT on_tick,
// and the next tick dispatches fk_on_deferred exactly once and unregisters
// itself again. Zero steady-state cost -- the assertion at the end is that
// nothing is registered on on_tick once the flush has happened.
func TestManyEventsInOneTickFlushOnce(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var buildID int
	for _, e := range events.Events {
		if e.Name == "on_player_created" {
			buildID = e.ID
		}
	}
	if buildID == 0 {
		t.Fatal("expected an on_player_created event")
	}

	// Counts events at 2048 and flushes at 2052, which are words 513 and 514 of
	// the aliased table mode memory.
	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(import "fk" "defer" (func $defer (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init") (drop (call $sub (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(i32.store (i32.const 2048)
				(i32.add (i32.load (i32.const 2048)) (i32.const 1)))
			(drop (call $defer)))
		(func (export "fk_on_deferred")
			(i32.store (i32.const 2052)
				(i32.add (i32.load (i32.const 2052)) (i32.const 1)))))`, buildID)

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
	pkg := &Package{
		Info: Info{Name: "fk-defer", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_on_event", "fk_on_deferred",
			"fk_alloc", "fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1, on_player_created = 2 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-defer",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

require("control")
handlers.on_init()
-- A blueprint paste: three builds, three separate dispatches, one tick.
handlers[2]({ player_index = 1, tick = 4 })
handlers[2]({ player_index = 2, tick = 4 })
handlers[2]({ player_index = 3, tick = 4 })
print("armed " .. tostring(handlers[1] ~= nil))
print("pending " .. tostring(storage.fk_deferred))
handlers[1]({ tick = 5 })
print("events " .. tostring(storage.fk_mem[1][513]))
print("flushes " .. tostring(storage.fk_mem[1][514]))
print("still " .. tostring(handlers[1] ~= nil))
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := strings.Join([]string{
		"armed true",
		"pending true",
		"events 3",
		"flushes 1",
		"still false",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(three dispatches in one tick must flush "+
			"once, and the one-shot on_tick must unregister itself)", got, want)
	}
}

// A save taken between the defer and the flush must not lose the work.
//
// Factorio does not save event registrations -- a mod re-registers everything
// in on_load, which is why script.on_event is legal there and script.on_init is
// not. So the ARMED FLAG lives in `storage` and on_load re-arms from it. Absent
// that, a guest that defers work and is saved before the next tick comes back
// with the work pending forever and nothing registered to run it.
func TestDeferredWorkSurvivesASaveTakenBeforeItRuns(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var buildID int
	for _, e := range events.Events {
		if e.Name == "on_player_created" {
			buildID = e.ID
		}
	}

	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(import "fk" "defer" (func $defer (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init") (drop (call $sub (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(drop (call $defer)))
		(func (export "fk_on_deferred")
			(i32.store (i32.const 2052)
				(i32.add (i32.load (i32.const 2052)) (i32.const 1)))))`, buildID)

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
	pkg := &Package{
		Info: Info{Name: "fk-defer-load", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_on_event", "fk_on_deferred",
			"fk_alloc", "fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// The second `require("control")` is the load: a fresh chunk, a fresh
	// dispatcher table, and the same `storage`. Nothing but storage crosses.
	out, err := h.RunString(fmt.Sprintf(expandClearLoaded(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1, on_player_created = 2 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-defer-load",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

require("control")
handlers.on_init()
handlers[2]({ player_index = 1, tick = 4 })
-- The save happens here. Registrations do not survive it; storage does.
handlers = {}
--@CLEAR_LOADED@
require("control")
handlers.on_load()
print("rearmed " .. tostring(handlers[1] ~= nil))
handlers[1]({ tick = 5 })
print("flushes " .. tostring(storage.fk_mem[1][514]))
print("still " .. tostring(handlers[1] ~= nil))
`), filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := "rearmed true\nflushes 1\nstill false"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(deferred work armed before a save has to "+
			"be re-armed from storage in on_load)", got, want)
	}
}

// A guest has to be able to notice that a save was LOADED.
//
// Factorio's own on_load cannot touch `game` -- it runs on every client when
// joining a multiplayer game and is read-only with respect to `storage` -- so a
// guest that wants to rebuild its state from the world had nothing to hang that
// off, and the only way to notice a load was to subscribe to on_tick forever: a
// permanent per-tick cost to observe a once-per-session event. That closed off
// --persist=none plus rebuild-from-world entirely.
//
// fk_after_load is the same one-shot machinery the deferred flush uses, which is
// what off_event was built for. It fires only after a LOAD, never on a new map:
// script.on_load does not run for one, and fk_on_init already covers that.
func TestTheFirstTickAfterALoadIsAHook(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	wat := `(module
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init"))
		(func (export "fk_after_load")
			(i32.store (i32.const 2048)
				(i32.add (i32.load (i32.const 2048)) (i32.const 1)))))`

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
	pkg := &Package{
		Info: Info{Name: "fk-afterload", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_after_load", "fk_alloc",
			"fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-afterload",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

require("control")
-- A new map: on_init, no on_load, so nothing is armed.
handlers.on_init()
print("new map armed " .. tostring(handlers[1] ~= nil))
-- A load.
handlers.on_load()
print("loaded armed " .. tostring(handlers[1] ~= nil))
handlers[1]({ tick = 11 })
print("ran " .. tostring(storage.fk_mem[1][513]))
print("still " .. tostring(handlers[1] ~= nil))
-- ...and it does not fire again on the next tick, because there is no next
-- registration to fire it.
print("second tick " .. tostring(handlers[1] == nil))
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := strings.Join([]string{
		"new map armed false",
		"loaded armed true",
		"ran 1",
		"still false",
		"second tick true",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
