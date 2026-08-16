package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// Event dispatch has to survive being re-entered.
//
// Factorio raises some events SYNCHRONOUSLY from inside the API call that
// caused them -- create_entity{raise_built=true}, entity.die() and friends -- so
// a guest handler that calls the API can be called again before it returns.
// Two things a dispatch owns used to be destroyed by that:
//
//   - the scratch buffer, a single address, which the inner dispatch encoded
//     its own event over. A handler reads its fields lazily from the pointer it
//     was handed rather than copying them out, so the outer handler carried on
//     reading the inner event's data.
//   - the transient handle space, which dispatch_done released wholesale. An
//     entity the outer event handed the guest stopped resolving halfway through
//     the handler -- and because clear_transient also restarts the id counter,
//     it could come back pointing at a DIFFERENT object, which is a desync
//     rather than an error.
//
// The guest here is the smallest thing that can tell: it records what its event
// buffer says before and after a host call, and the host call raises the event
// the guest is subscribed to.
func TestANestedDispatchLeavesTheOuterOneIntact(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	// reload_script stands in for any API call that raises an event: it takes
	// nothing, returns nothing, and the stub below makes it re-raise on_tick.
	callID := full.MemberIndex()["LuaGameScript::reload_script/0"]
	if callID == 0 {
		t.Fatal("expected a CALL entry for LuaGameScript::reload_script")
	}
	var tickID int
	for _, e := range events.Events {
		if e.Name == "on_tick" {
			tickID = e.ID
		}
	}
	if tickID == 0 {
		t.Fatal("expected an on_tick event")
	}

	// Reads its tick field, calls the host, reads it again. Handle 2 is `game`.
	// The global is what tells the outer dispatch from the inner one, since
	// both arrive at the same export.
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(memory 1)
		(global $depth (mut i32) (i32.const 0))
		;; A bump allocator, which is all control.lua needs of one: it allocates
		;; the event scratch and never frees. Starts past the two words the
		;; handler records into.
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		;; The event buffers outlive the call that asked for them, so control.lua
		;; takes them through this rather than fk_alloc -- see event_buffer in
		;; fk_mod.lua. Here the two are the same bump; in a real guest fk_alloc
		;; is an arena and this one is not.
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init") (drop (call $sub (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(global.set $depth (i32.add (global.get $depth) (i32.const 1)))
			(if (i32.eq (global.get $depth) (i32.const 1))
				(then
					(i32.store (i32.const 2048)
						(i32.load (i32.add (local.get $ptr) (i32.const 4))))
					(drop (call $call (i32.const 2) (i32.const %d)
						(i32.const 0) (i32.const 0)))
					(i32.store (i32.const 2052)
						(i32.load (i32.add (local.get $ptr) (i32.const 4))))))
			(global.set $depth (i32.sub (global.get $depth) (i32.const 1)))))`,
		tickID, callID)

	im := buildIR(t, wat)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	// Table mode, so `storage.fk_mem` IS the guest's word table and the test can
	// read what the handler recorded without the guest needing a way to say it.
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-reentrant", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk:    chunk,
		Exports:  []string{"fk_on_init", "fk_on_event", "fk_alloc", "fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// The stub takes a transient handle the way an event field would, raises
	// on_tick re-entrantly, and then asks whether its handle still works --
	// which is the question a guest holding event.entity across an API call is
	// really asking.
	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-reentrant",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

local H = require("fk_abi")
local held, survived, raised = nil, "not asked", false
game = {
  reload_script = function()
    if raised then return end        -- the inner dispatch must not recurse
    raised = true
    held = H.transient({ object_name = "LuaEntity" })
    handlers[1]({ tick = 99 })       -- Factorio, re-entering the handler
    survived = H.get(held) ~= nil and "yes" or "no"
  end,
}

require("control")
handlers.on_init()
handlers[1]({ tick = 7 })
-- 2048 and 2052 as word indices into the aliased memory.
print("before " .. tostring(storage.fk_mem[1][513]))
print("after " .. tostring(storage.fk_mem[1][514]))
print("handle " .. survived)
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// before proves the tick field is where the guest thinks it is, so `after`
	// is a real comparison rather than two reads of the same wrong offset.
	want := "before 7\nafter 7\nhandle yes"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the inner dispatch encoded over the outer "+
			"one's buffer, or released the handles it was still using)", got, want)
	}
}
