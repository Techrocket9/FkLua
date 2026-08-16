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
// moving, for prototypes appearing or vanishing, for a migration being applied,
// and for the game version moving. None of those touches THIS mod's build stamp,
// so finish_rebuild returned on its flag and nothing else was registered on that
// hook: a guest could not observe any of it.
//
// Filed by BetterBeltBalancer, which adopts an incumbent mod's entities when the
// incumbent is uninstalled. That is a once-per-save conversion whose only honest
// trigger is this event; without it the best available one is "the first event
// of the session", which converts late and on a tick nobody chose.
//
// The properties this pins, in the order they matter:
//
//	changed   the guest is told when the build stamp did NOT move -- the gap --
//	          and the write it makes from the hook is in `storage` before any
//	          tick runs, in BOTH persisting modes (`synced`)
//	new/load  and is NOT told on a new map, or on a load with no config change
//	added     and IS told on the load that ADDS the mod to an existing save,
//	          after fk_on_init and with no fk_migrate -- which is what Factorio
//	          does: a mod arriving IS a mod-set change, and the event runs for
//	          every mod or for none. Measured in a real Factorio 2.0.77 by the
//	          downstream mod's own suite, whose add-to-existing-save leg logs
//	          its fk_on_init line and then the hook's rebuild-from-world line,
//	          both before the benchmark's first `Running update 0`
//	rebuilt   and when a rebuild and a config change land on one load,
//	          fk_migrate ran FIRST, on a settled and republished heap
//
// Two more, in their own test below: the registration follows the EXPORT and not
// just `persisting`, so a --persist=none guest that exports the hook is wired --
// and ENTERED, which a registration check alone cannot see.
//
// Against the pre-2026-08-16 runtime the `synced`, `changed`, `added` and
// `rebuilt` legs all report told=0: the handler registered on that hook was
// finish_rebuild alone, which returns on a flag a mod-set change never sets.
// (`new` and `load` report told=0 against either runtime; they are the negative
// and cannot go red on the change alone.)
//
// The `rebuilt` leg's ordering is guaranteed TWICE and the test cannot tell the
// two apart, deliberately: enter_outermost runs finish_rebuild before any
// dispatch it opens, so even a handler that dispatched the hook first would
// still deliver fk_migrate first. The explicit call in the handler is what
// finishes a rebuild for a guest that does NOT export this hook. Reordering the
// handler therefore leaves this leg green; what it pins is the OBSERVABLE
// property, which is the one a guest author relies on.

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

// The four words, read back out of `storage` the way each persisting mode
// mirrors them. Table mode aliases the shard vector, so word N is
// fk_mem[1][N+1]; packed mode mirrors 4 KiB pages as strings, so word N is
// bytes 4N+1..4N+4 of page 1. Reading through the mirror rather than through a
// live handle is what makes the `synced` leg a statement about the SAVE.
const configTableReport = `
local function words()
  local m = storage.fk_mem
  if not m then return "nil", "nil", "nil", "nil" end
  local t = m[1]
  return tostring(t[1]), tostring(t[2]), tostring(t[3]), tostring(t[4])
end`

const configPackedReport = `
local function words()
  local p = storage.fk_pages
  if not p or not p[1] then return "nil", "nil", "nil", "nil" end
  local s = p[1]
  return tostring(string.unpack("<I4", s, 1)), tostring(string.unpack("<I4", s, 5)),
         tostring(string.unpack("<I4", s, 9)), tostring(string.unpack("<I4", s, 13))
end`

func TestAModSetChangeReachesTheGuestWithoutARebuild(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, arm := range []struct {
		name   string
		mode   luagen.PersistMode
		report string
	}{
		{"table", luagen.PersistTable, configTableReport},
		// Packed is the arm where "the hook's write reached the save" is a real
		// question: nothing aliases the live table into `storage`, so the word
		// the guest wrote is in the mirror only if the dispatch that ran the
		// hook flushed its dirty page -- and on the rebuild path only if
		// state_init had set `pages` before that dispatch closed.
		{"packed", luagen.PersistPacked, configPackedReport},
	} {
		t.Run(arm.name, func(t *testing.T) {
			a := packRebuild(t, configChangedWAT, arm.mode, "build-A", configChangedExports)
			b := packRebuild(t, configChangedWAT, arm.mode, "build-B", configChangedExports)

			pa, pb := filepath.Join(a, "?.lua"), filepath.Join(b, "?.lua")
			out, err := h.RunString(fmt.Sprintf(configChangedScript, arm.report,
				pa, pa, pa, pa, pb))
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			legs := parseLegs(t, out, "new", "load", "synced", "changed", "added", "rebuilt")

			// A NEW MAP AND AN ORDINARY LOAD ARE BOTH SILENT. Factorio raises the
			// event for neither, so a guest that acts on it must not be entered.
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
			// Its own state is still underneath it: this is a notification, not a
			// reset.
			if got := legs["changed"]["ticks"]; got != "9" {
				t.Errorf("the same-build leg lost the heap it adopted (ticks=%s, want 9 "+
					"-- six from the two earlier sessions and three from this one): %v",
					got, legs["changed"])
			}
			// ...AND THE WRITE IS IN THE SAVE BEFORE ANY TICK RUNS. `synced` is the
			// same session read the instant the hook returned, so the only thing
			// that could have carried word 1 into `storage` is the dispatch that
			// ran the hook: its dispatch_done, and under packed its page flush.
			// A hook that ran but whose write waited for the next tick to reach
			// the mirror would report told=0 here and told=1 in `changed`.
			if got := legs["synced"]["told"]; got != "1" {
				t.Errorf("THE HOOK'S WRITE DID NOT REACH `storage` UNTIL SOMETHING ELSE "+
					"RAN (told=%s right after the hook, want 1): a guest that writes "+
					"state ONLY in fk_on_configuration_changed and is then saved or "+
					"joined before its first tick would lose it: %v", got, legs["synced"])
			}
			if got := legs["synced"]["ticks"]; got != "6" {
				t.Errorf("synced read the wrong session (ticks=%s, want 6): %v",
					got, legs["synced"])
			}

			// A MOD ADDED TO AN EXISTING SAVE GETS BOTH, IN THIS ORDER. Factorio
			// raises on_init for the newcomer and then on_configuration_changed for
			// every mod, the newcomer included, on the same load. No on_load ran,
			// so nothing is pending and fk_migrate must stay silent; the hook must
			// not be swallowed by state_init having cleared the flag.
			if got := legs["added"]["told"]; got != "1" {
				t.Errorf("A MOD ADDED TO AN EXISTING SAVE WAS NOT TOLD (told=%s, want 1) "+
					"on the load that added it, after its fk_on_init: %v",
					got, legs["added"])
			}
			if got := legs["added"]["migrated"]; got != "0" {
				t.Errorf("the added-mod leg ran fk_migrate (migrated=%s): on_init "+
					"published this build's stamp, so there is no rebuild to finish: %v",
					got, legs["added"])
			}
			if got := legs["added"]["ticks"]; got != "0" {
				t.Errorf("the added-mod leg is not on a fresh heap (ticks=%s, want 0): "+
					"%v", got, legs["added"])
			}
			if got := legs["added"]["build"]; got != "build-A" {
				t.Errorf("the added-mod leg's stamp is %s, want build-A: %v",
					got, legs["added"])
			}

			// BOTH AT ONCE, WHICH IS THE ORDERING. fk_migrate first, on a heap
			// finish_rebuild has already settled and republished, and then this.
			if got := legs["rebuilt"]["told"]; got != "1" {
				t.Errorf("a rebuilt guest whose mod set also changed was not told "+
					"(told=%s): %v", got, legs["rebuilt"])
			}
			if got := legs["rebuilt"]["migrated"]; got != "1" {
				t.Errorf("the rebuilt leg did not run fk_migrate (migrated=%s): %v",
					got, legs["rebuilt"])
			}
			if legs["rebuilt"]["told"] == "1" && legs["rebuilt"]["sawmigrate"] != "1" {
				t.Errorf("fk_on_configuration_changed RAN BEFORE fk_migrate "+
					"(sawmigrate=%s, want 1). A guest told the world moved while its own "+
					"heap has not been settled yet is reading bytes that are about to be "+
					"replaced. Both enter_outermost and the handler order the two; both "+
					"have to have gone wrong to reach this line: %v",
					legs["rebuilt"]["sawmigrate"], legs["rebuilt"])
			}
			if got := legs["rebuilt"]["build"]; got != "build-B" {
				t.Errorf("the rebuild was not finished on the config-changed path "+
					"(build=%s): %v", got, legs["rebuilt"])
			}
		})
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
// Two things are asserted: that requiring control.lua REGISTERS a handler, and
// that raising it ENTERS the guest. The second is the one a registration check
// alone cannot see -- a handler that ran finish_rebuild and skipped the dispatch
// would register and raise cleanly and reach no guest code. Under --persist=none
// guest memory is a local inside the chunk and no `storage` key mirrors it, so
// the guest says it was entered the one way a packaged guest always can: through
// env.fk_log, which control.lua routes to Factorio's log().
func TestAPersistNoneGuestStillGetsTheConfigurationHook(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packRebuild(t, configNoneWAT, luagen.PersistNone, "build-N",
		[]string{"fk_on_tick", "fk_on_configuration_changed"})
	out, err := h.RunString(fmt.Sprintf(configNoneScript, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "registered true\nraised ok\nentered 1 config-changed"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a --persist=none guest exporting "+
			"fk_on_configuration_changed must still have the hook registered AND "+
			"be entered by it; gating the registration on `persisting` leaves it "+
			"unwired and silent, and a handler that skips the dispatch is a "+
			"registration that reaches nothing)", got, want)
	}
}

// The --persist=none guest: the hook logs a fixed string out of its own data
// segment. That is the only observation channel a guest with no `storage`
// mirror has, and it is the same one defines_test.go's guest uses.
const configNoneWAT = `(module
	(import "env" "fk_log" (func $log (param i32 i32)))
	(memory 1)
	(data (i32.const 256) "config-changed")
	(func (export "fk_on_tick") (param $tick i32))
	(func (export "fk_on_configuration_changed")
		(call $log (i32.const 256) (i32.const 14))))`

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

// Five sessions of one mod in one interpreter, with only `storage` crossing.
// on_configuration_changed is raised for the last three and for neither of the
// first two, which is what Factorio does: a new save gets on_init, a load whose
// configuration is unchanged gets on_load and nothing else, and a load that
// ADDS the mod gets on_init and then on_configuration_changed with no on_load
// in between.
//
// The first %s is the mode-specific `words()` reader; the five %q are the
// package directories each session loads control.lua from.
var configChangedScript = expandClearLoaded(`%s
defines = { events = { on_tick = 1 } }
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
  local w0, w1, w2, w3 = words()
  print(tag .. " ticks=" .. w0 .. " told=" .. w1 .. " migrated=" .. w2 ..
        " sawmigrate=" .. w3 .. " build=" .. tostring(storage.fk_build))
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
-- changed, so state_load adopts and finish_rebuild has nothing to do. Read once
-- the instant the hook returns, before any tick, and once after three.
boot(same, %q)
handlers.on_load()
handlers.on_config({ mod_changes = { neighbour = { old_version = "1.0.0" } } })
report("synced")
ticks(3)
report("changed")

-- THIS MOD ADDED TO A SAVE THAT ALREADY EXISTS: on_init runs for the newcomer,
-- and then on_configuration_changed runs for every mod, the newcomer included,
-- naming it with no old_version. No on_load, so nothing was ever pending.
boot(nil, %q)
handlers.on_init()
handlers.on_config({ mod_changes = { ["fk-rebuild"] = { new_version = "0.1.0" } } })
report("added")

-- A DIFFERENT BUILD and a configuration change in the same load: fk_migrate is
-- owed first, and it lands on a FRESH heap, so the counters restart.
boot(same, %q)
handlers.on_load()
handlers.on_config({ mod_changes = {} })
report("rebuilt")
`)

// The --persist=none arm. No on_init is registered at all (the guest exports
// none and there is no state to publish), so the whole leg is: does requiring
// control.lua put a handler on the hook, does raising it run, and did the guest
// say so.
var configNoneScript = expandClearLoaded(`defines = { events = { on_tick = 1 } }
game = {}
logged = {}
function log(s) logged[#logged + 1] = s end
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
  print("entered " .. #logged .. " " .. tostring(logged[1]))
end
`)
