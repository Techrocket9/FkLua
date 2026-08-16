package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// A MOD-SET CHANGE IS NOT A REBUILD, AND UNTIL 2026-08-16 ONLY A REBUILD WAS
// AUDIBLE.
//
// script.on_configuration_changed is raised for the mod SET changing -- a
// neighbour added, REMOVED, or moved to another version -- for a startup setting
// moving, and for the game version moving. None of those touches THIS mod's
// build stamp, so finish_rebuild returned on its flag and nothing else was
// registered on that hook: a guest could not observe any of it.
//
// Filed by BetterBeltBalancer, which adopts an incumbent mod's entities when the
// incumbent is uninstalled. That is a once-per-save conversion whose only honest
// trigger is this event; without it the best available one is "the first event
// of the session", which converts late and on a tick nobody chose.
//
// The three properties this pins, in the order they matter:
//
//	changed   the guest is told when the build stamp did NOT move -- the gap
//	new/load  and is NOT told on a new map, or on a load with no config change
//	rebuilt   and when both happened, fk_migrate ran FIRST, on a settled heap
//
// A fourth, in its own test below: the registration follows the EXPORT and not
// just `persisting`, so a --persist=none guest that exports the hook is wired.
//
// Against the pre-2026-08-16 runtime the `changed` leg reports told=0: the
// handler registered on that hook was finish_rebuild alone, which returns on a
// flag a mod-set change never sets.

// Word 0 counts ticks, word 1 counts fk_on_configuration_changed calls, word 2
// is set by fk_migrate, and word 3 is word 2 AS THE CONFIG HOOK SAW IT -- which
// is the ordering assertion, and it is a word rather than an argument because
// the hook deliberately takes none.
const configChangedWAT = `(module
	(memory 1)
	(func (export "fk_on_tick") (param $tick i32)
		(i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1))))
	(func (export "fk_state_version") (result i32) (i32.const 7))
	(func (export "fk_migrate") (param $old i32)
		(i32.store (i32.const 8) (i32.const 1)))
	(func (export "fk_on_configuration_changed")
		(i32.store (i32.const 4) (i32.add (i32.load (i32.const 4)) (i32.const 1)))
		(i32.store (i32.const 12) (i32.load (i32.const 8)))))`

var configChangedExports = []string{"fk_on_tick", "fk_state_version",
	"fk_migrate", "fk_on_configuration_changed"}

func TestAModSetChangeReachesTheGuestWithoutARebuild(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := packRebuild(t, configChangedWAT, luagen.PersistTable, "build-A",
		configChangedExports)
	b := packRebuild(t, configChangedWAT, luagen.PersistTable, "build-B",
		configChangedExports)

	pa, pb := filepath.Join(a, "?.lua"), filepath.Join(b, "?.lua")
	out, err := h.RunString(fmt.Sprintf(configChangedScript, pa, pa, pa, pb))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	legs := parseLegs(t, out, "new", "load", "changed", "rebuilt")

	// A NEW MAP AND AN ORDINARY LOAD ARE BOTH SILENT. Factorio raises the event
	// for neither, so a guest that acts on it must not be entered.
	if got := legs["new"]["told"]; got != "0" {
		t.Errorf("a NEW MAP told the guest its configuration changed (told=%s): "+
			"Factorio raises on_configuration_changed for an existing save whose "+
			"mod set moved, never for a save being created -- fk_on_init is that "+
			"case: %v", got, legs["new"])
	}
	if got := legs["load"]["told"]; got != "0" {
		t.Errorf("an ORDINARY LOAD told the guest its configuration changed "+
			"(told=%s): %v", got, legs["load"])
	}

	// THE GAP. Same build, so nothing is pending and finish_rebuild does
	// nothing; the guest is told anyway, which is the whole feature.
	if got := legs["changed"]["told"]; got != "1" {
		t.Errorf("THE GUEST WAS NOT TOLD ITS MOD SET CHANGED (told=%s, want 1). "+
			"on_configuration_changed carried finish_rebuild and nothing else, "+
			"and finish_rebuild returns on a flag only a BUILD STAMP move sets -- "+
			"so a neighbour being uninstalled reached no guest code at all: %v",
			got, legs["changed"])
	}
	if got := legs["changed"]["migrated"]; got != "0" {
		t.Errorf("the same-build leg ran fk_migrate (migrated=%s): the two hooks "+
			"answer different questions and this leg is the one where only the "+
			"second has an answer: %v", got, legs["changed"])
	}
	// Its own state is still underneath it: this is a notification, not a reset.
	if got := legs["changed"]["ticks"]; got != "9" {
		t.Errorf("the same-build leg lost the heap it adopted (ticks=%s, want 9 "+
			"-- six from the two earlier sessions and three from this one): %v",
			got, legs["changed"])
	}

	// BOTH AT ONCE, WHICH IS THE ORDERING. fk_migrate first, on a heap
	// finish_rebuild has already settled and republished, and then this.
	if got := legs["rebuilt"]["told"]; got != "1" {
		t.Errorf("a rebuilt guest whose mod set also changed was not told "+
			"(told=%s): %v", got, legs["rebuilt"])
	}
	if got := legs["rebuilt"]["sawmigrate"]; got != "1" {
		t.Errorf("fk_on_configuration_changed RAN BEFORE fk_migrate "+
			"(sawmigrate=%s, want 1). A guest told the world moved while its own "+
			"heap has not been settled yet is reading bytes that are about to be "+
			"replaced: the dispatch belongs after finish_rebuild: %v",
			got, legs["rebuilt"])
	}
	if got := legs["rebuilt"]["build"]; got != "build-B" {
		t.Errorf("the rebuild was not finished on the config-changed path "+
			"(build=%s): %v", got, legs["rebuilt"])
	}
}

// ...AND THE REGISTRATION FOLLOWS THE EXPORT, NOT `persisting`.
//
// The hook used to be registered under `persisting` alone, because the only
// thing on it was the persistence layer's own. A guest compiled --persist=none
// has no `storage` to keep anything in and is exactly the guest that rebuilds
// from the world when something changes, so leaving it unwired would exclude
// the case with the strongest reason to want this.
//
// What is asserted here is the REGISTRATION and not a side effect, deliberately:
// under --persist=none guest memory is a local inside the chunk and no `storage`
// key mirrors it, so there is nothing a harness can read back. A registration
// that is absent is the whole defect either way.
func TestAPersistNoneGuestStillGetsTheConfigurationHook(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packRebuild(t, configChangedWAT, luagen.PersistNone, "build-N",
		configChangedExports)
	out, err := h.RunString(fmt.Sprintf(configNoneScript, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "registered true\nraised ok"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a --persist=none guest exporting "+
			"fk_on_configuration_changed must still have the hook registered; "+
			"gating the registration on `persisting` leaves it unwired and "+
			"silent)", got, want)
	}
}

// parseLegs reads the `tag key=value ...` report lines the scripts print.
func parseLegs(t *testing.T, out string, want ...string) map[string]map[string]string {
	t.Helper()
	legs := map[string]map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) < 2 {
			continue
		}
		m := map[string]string{}
		for _, kv := range f[1:] {
			if i := strings.IndexByte(kv, '='); i > 0 {
				m[kv[:i]] = kv[i+1:]
			}
		}
		legs[f[0]] = m
	}
	for _, name := range want {
		if legs[name] == nil {
			t.Fatalf("the script never reached leg %s:\n%s", name, out)
		}
		t.Logf("%s %v", name, legs[name])
	}
	return legs
}

// Four sessions of one mod in one interpreter, with only `storage` crossing.
// on_configuration_changed is raised for the last two and for neither of the
// first two, which is what Factorio does: a new save gets on_init, a load whose
// configuration is unchanged gets on_load and nothing else.
var configChangedScript = expandClearLoaded(`defines = { events = { on_tick = 1 } }
game = {}
logged = {}
function log(s) logged[#logged + 1] = s end

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local o = {}
  for k, x in pairs(v) do o[deepcopy(k)] = deepcopy(x) end
  return o
end

local handlers = {}
local function boot(saved, dir)
  handlers = {}
  script = {
    mod_name = "fk-rebuild",
    on_init = function(f) handlers.on_init = f end,
    on_load = function(f) handlers.on_load = f end,
    on_configuration_changed = function(f) handlers.on_config = f end,
    on_event = function(ev, f) handlers[ev] = f end,
  }
  storage = saved and deepcopy(saved) or {}
  logged = {}
  package.path = dir
  --@CLEAR_LOADED@
  require("control")
end

local function ticks(n)
  for t = 1, n do handlers[defines.events.on_tick]({ tick = t }) end
end

local function report(tag)
  local m = storage.fk_mem
  local function w(i) if not m then return "nil" end return tostring(m[1][i]) end
  print(tag .. " ticks=" .. w(1) .. " told=" .. w(2) .. " migrated=" .. w(3) ..
        " sawmigrate=" .. w(4) .. " build=" .. tostring(storage.fk_build))
end

-- A NEW MAP: on_init, and Factorio raises nothing else.
boot(nil, %q)
handlers.on_init()
ticks(3)
report("new")
local save = deepcopy(storage)

-- AN ORDINARY LOAD of the same build, nothing about the configuration moved.
boot(save, %q)
handlers.on_load()
ticks(3)
report("load")
local same = deepcopy(storage)

-- THE SAME BUILD, and a NEIGHBOUR was uninstalled. Nothing about this mod
-- changed, so state_load adopts and finish_rebuild has nothing to do.
boot(same, %q)
handlers.on_load()
handlers.on_config({ mod_changes = {} })
ticks(3)
report("changed")

-- A DIFFERENT BUILD and a configuration change in the same load: fk_migrate is
-- owed first, and it lands on a FRESH heap, so the counters restart.
boot(same, %q)
handlers.on_load()
handlers.on_config({ mod_changes = {} })
report("rebuilt")
`)

// The --persist=none arm. No on_init is registered at all (the guest exports
// none and there is no state to publish), so the whole leg is: does requiring
// control.lua put a handler on the hook, and does raising it run.
var configNoneScript = expandClearLoaded(`defines = { events = { on_tick = 1 } }
game = {}
function log(s) end
storage = nil

local handlers = {}
script = {
  mod_name = "fk-rebuild",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
package.path = %q
--@CLEAR_LOADED@
require("control")

print("registered " .. tostring(handlers.on_config ~= nil))
if handlers.on_config then
  handlers.on_config({ mod_changes = {} })
  print("raised ok")
end
`)
