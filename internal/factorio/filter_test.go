package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// The filter list a guest builds, as tier-2 dynamic values in its own memory.
//
// [ { filter = "name", name = "foo" } ] -- the shape Factorio wants and the
// commonest one by far. 16 bytes per value, tag at 0 and payload at 8; a
// key/value pair is two of them, so 32.
const filterWat = `
	;; the array
	(i32.store (i32.const 0x100) (i32.const 5))      ;; DYN_ARR
	(i32.store (i32.const 0x108) (i32.const 0x120))
	(i32.store (i32.const 0x10c) (i32.const 1))
	;; element 0, a map of two pairs
	(i32.store (i32.const 0x120) (i32.const 6))      ;; DYN_MAP
	(i32.store (i32.const 0x128) (i32.const 0x140))
	(i32.store (i32.const 0x12c) (i32.const 2))
	;; "filter" -> "name"
	(i32.store (i32.const 0x140) (i32.const 3))
	(i32.store (i32.const 0x148) (i32.const 0x200))
	(i32.store (i32.const 0x14c) (i32.const 6))
	(i32.store (i32.const 0x150) (i32.const 3))
	(i32.store (i32.const 0x158) (i32.const 0x206))
	(i32.store (i32.const 0x15c) (i32.const 4))
	;; "name" -> "foo"
	(i32.store (i32.const 0x160) (i32.const 3))
	(i32.store (i32.const 0x168) (i32.const 0x206))
	(i32.store (i32.const 0x16c) (i32.const 4))
	(i32.store (i32.const 0x170) (i32.const 3))
	(i32.store (i32.const 0x178) (i32.const 0x20a))
	(i32.store (i32.const 0x17c) (i32.const 3))
`

// filterGuest packages a guest whose fk_on_init subscribes the way `body` says.
func filterGuest(t *testing.T, name, body string) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var minedID int
	for _, e := range events.Events {
		if e.Name == "on_player_mined_entity" {
			minedID = e.ID
		}
	}
	if minedID == 0 {
		t.Fatal("expected an on_player_mined_entity event")
	}

	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32 i32) (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 4096))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init") %s)
		(func (export "fk_on_event") (param $id i32) (param $ptr i32))
		(data (i32.const 0x200) "filternamefoo"))`,
		fmt.Sprintf(body, minedID))

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
		Info: Info{Name: name, Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_on_event", "fk_alloc",
			"fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The stub is Factorio's contract for filters: script.on_event takes them as a
// third argument and set_event_filter replaces them afterwards.
const filterStub = `
package.path = %q
function log(s) end
defines = { events = { on_tick = 1, on_player_mined_entity = 2 } }
storage = {}
local handlers, filters = {}, {}
script = {
  mod_name = "t",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f, flt) handlers[ev] = f; filters[ev] = flt end,
  set_event_filter = function(ev, flt) filters[ev] = flt end,
}

require("control")
handlers.on_init()
local f = filters[2]
if f == nil then
  print("filters none")
else
  print("filters " .. #f)
  print("kind " .. tostring(f[1].filter))
  print("name " .. tostring(f[1].name))
end
`

// A guest that cares about one prototype should be entered only for that one.
//
// Factorio applies script.on_event's filter list in C++ before the handler
// runs, so an unfiltered subscription costs a guest a dispatch plus a host call
// plus a string crossing to read entity.name and reject -- for every build and
// mine event on the map. fk.subscribe now takes a pointer to a tier-2 dynamic
// value, which is the codec that already carries this exact shape (an array of
// string-keyed maps), and control.lua hands the decoded table straight to
// Factorio.
func TestASubscriptionCarriesItsEventFilter(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := filterGuest(t, "fk-filter", filterWat+
		"(drop (call $sub (i32.const %d) (i32.const 0x100)))")

	out, err := h.RunString(fmt.Sprintf(filterStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "filters 1\nkind name\nname foo"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Zero means unfiltered, and it has to stay the default: every guest compiled
// before filters existed passes one argument, so Lua hands `subscribe` a nil.
func TestAnUnfilteredSubscriptionStaysUnfiltered(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := filterGuest(t, "fk-nofilter",
		"(drop (call $sub (i32.const %d) (i32.const 0)))")

	out, err := h.RunString(fmt.Sprintf(filterStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out); got != "filters none" {
		t.Errorf("got %q, want %q", got, "filters none")
	}
}

// Two subscriptions to one event have to agree on ONE filter list, because
// script.on_event takes one per registration and this runtime keeps one
// dispatcher per event holding a list of handlers.
//
// The only merge that cannot lose an event is the union, and a subscriber that
// asked for no filter at all makes the whole registration unfiltered. Erring
// toward receiving MORE is the only safe direction: a guest can ignore an event
// it did not want and cannot act on one it never got. The reverse -- letting
// the filtered subscription win -- would silently stop delivering to the
// unfiltered handler, which is the failure mode the report's "fail closed"
// note is about.
func TestAnUnfilteredSubscriptionWidensAFilteredOne(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := filterGuest(t, "fk-widen", filterWat+`
		(drop (call $sub (i32.const %[1]d) (i32.const 0x100)))
		(drop (call $sub (i32.const %[1]d) (i32.const 0)))`)

	out, err := h.RunString(fmt.Sprintf(filterStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out); got != "filters none" {
		t.Errorf("got %q, want %q\n(an unfiltered subscriber must widen the "+
			"registration, or it stops receiving what it asked for)", got, "filters none")
	}
}
