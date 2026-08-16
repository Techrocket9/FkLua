package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// A REBUILD THAT KEEPS THE MOD'S VERSION GETS NO on_configuration_changed, AND
// UNTIL 2026-08-07 THAT WAS THE ONLY PLACE A REBUILD WAS EVER HANDLED.
//
// Factorio raises on_configuration_changed when the mod SET changes -- for one
// mod, when its VERSION moves. A build stamp moves for a great deal less: a dev
// rebuild, a --gc or --persist change, a repackage against another --api pin.
// The commit that folded the pin into the stamp moved every stamp in existence
// without touching a single mod version.
//
// Every save/load harness in this repo called on_config() on every load --
// persist_test.go's session(), bufpin_test.go's C leg, buildstamp_test.go's
// crossPinScript -- so the path with no hook was untested through two
// milestones, and what it left behind was worse than a reset:
//
//   - the save is PERMANENTLY SELF-INCONSISTENT. state_load declined, so nothing
//     republished storage.fk_mem: `storage` still holds the previous build's
//     heap while the guest runs on the fresh one _initialize built. The guest's
//     writes reach neither the save nor the multiplayer CRC.
//   - every later load declines again, because the stamp it compares against was
//     never republished either.
//   - fk_migrate and fk_migrate_adopt NEVER FIRE. Both live in the hook that did
//     not fire, so a guest whose entire answer to a rebuild is "tell me and I
//     will rescan the world" is never told.
//
// This file is the hook half. buildstamp_test.go's TestASameVersionRebuildIsStill-
// Handled is the same defect over two real --api pins, and
// TestAJoiningPeerStaysByteIdenticalToTheServer's stale arm is the multiplayer
// consequence that made it a desync rather than a data-loss bug.

// The migratable guest, at word 0 a tick counter and at word 1 the state version
// its migrate hook was handed. Multiplying by 100 is what makes the two hooks
// distinguishable in one number: on a FRESH heap the counter is 0 and 0*100 is
// still 0, and on an ADOPTED one the previous session's five ticks become 500.
const rebuildWAT = `(module
	(memory 1)
	(func (export "fk_on_tick") (param $tick i32)
		(i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1))))
	(func (export "fk_state_version") (result i32) (i32.const 7))
	(func (export "@@HOOK@@") (param $old i32)
		(i32.store (i32.const 4) (local.get $old))
		(i32.store (i32.const 0) (i32.mul (i32.load (i32.const 0)) (i32.const 100)))))`

// ...and the same guest with no hook at all, which is the shape every TinyGo and
// Rust guest in this repo has: the discard is logged and nothing is dispatched.
const rebuildNoHookWAT = `(module
	(memory 1)
	(func (export "fk_on_tick") (param $tick i32)
		(i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1)))))`

// packRebuild packages one guest under one build identity. Two calls with two
// ids is what a recompile looks like to same_build(), which is a string
// comparison on the stamp and cannot see why it moved.
func packRebuild(t *testing.T, wat string, mode luagen.PersistMode, id string,
	exports []string) string {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Persist: mode, BuildID: id})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	pkg := &Package{
		Info: Info{
			Name: "fk-rebuild", Version: "0.1.0", Title: "Rebuild",
			Author: "FkLua", FactorioVersion: DefaultFactorioVersion,
		},
		Chunk:   chunk,
		Exports: exports,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatalf("WriteDir: %v", err)
	}
	return dir
}

// THE HOOKS FIRE ON A SAME-VERSION REBUILD, AND fk_migrate_adopt REALLY GETS THE
// OLD HEAP ON THAT PATH TOO.
//
// Three sessions of one guest per arm, with only `storage` crossing and
// on_configuration_changed NEVER CALLED -- which is the whole experiment. The
// two established tests over these same two hooks
// (TestMigrateIsToldAboutTheRebuildAndGetsAFreshHeap and
// TestMigrateAdoptReallyGetsTheOldHeap) call it on every load, so between them
// they covered one of the two ways a rebuild reaches a running game.
//
//	one    build A, a new map, five ticks. The save.
//	two    build B loads it. No on_config. The hook must fire, the stamp must be
//	       republished, and the three ticks after it must land in the save.
//	three  build B loads what two wrote. An ordinary load: it adopts, and nothing
//	       is logged or dispatched. This is the leg that says the save was left
//	       CONSISTENT rather than merely handled once.
//
// Against the unfixed runtime every arm reports leg two as the FIRST session's
// counter with the FIRST session's stamp -- `mem0 5 mem1 0 build build-A` for
// both hooked arms, and leg three identical to it, forever.
func TestARebuiltGuestIsToldWithoutOnConfigurationChanged(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, arm := range []struct {
		name string
		wat  string
		// exports is what `fklua mod` wires; the hook has to be in it or
		// control.lua cannot see it.
		exports []string
		// mem0 and mem1 after leg two, and after leg three.
		two, three [2]string
		// logged says whether the discard notice fired, which is the ONLY
		// channel a guest with no hook has.
		twoLogged string
	}{
		{
			// The notification. The heap is fresh, so the hook multiplies a zero
			// and the counter is this session's three alone -- the old bytes were
			// never underneath it, which is the whole of what fk_migrate promises.
			name:    "fk_migrate",
			wat:     strings.Replace(rebuildWAT, "@@HOOK@@", "fk_migrate", 1),
			exports: []string{"fk_on_tick", "fk_migrate", "fk_state_version"},
			two:     [2]string{"3", "7"},
			three:   [2]string{"6", "7"},
			// The guest asked to be told and was, by dispatch rather than by log.
			twoLogged: "0",
		},
		{
			// The opt-in that really hands the bytes over. state_load's gate lets
			// fk_migrate_adopt through, so the five ticks ARE underneath the hook
			// -- 500 -- and the three after it make 503. That is the arm the fix
			// had to be careful about: the flag is recorded before the gate, not
			// after it, or this hook would never fire at all on a same-version
			// rebuild while the heap it is about is sitting right there.
			name:      "fk_migrate_adopt",
			wat:       strings.Replace(rebuildWAT, "@@HOOK@@", "fk_migrate_adopt", 1),
			exports:   []string{"fk_on_tick", "fk_migrate_adopt", "fk_state_version"},
			two:       [2]string{"503", "7"},
			three:     [2]string{"506", "7"},
			twoLogged: "0",
		},
		{
			// No hook: the heap is discarded and the author is told by name. This
			// is the shape every Go and Rust guest in this repo has.
			name:      "no hook",
			wat:       rebuildNoHookWAT,
			exports:   []string{"fk_on_tick"},
			two:       [2]string{"3", "0"},
			three:     [2]string{"6", "0"},
			twoLogged: "1",
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			a := packRebuild(t, arm.wat, luagen.PersistTable, "build-A", arm.exports)
			b := packRebuild(t, arm.wat, luagen.PersistTable, "build-B", arm.exports)
			legs := runRebuildScript(t, h, tableRebuildReport, a, b)

			if legs["one"]["mem0"] != "5" || legs["one"]["build"] != "build-A" {
				t.Fatalf("leg one did not establish the save: %v", legs["one"])
			}
			if got := legs["two"]["mem0"]; got != arm.two[0] {
				t.Errorf("THE SAVE IS NOT TRACKING THE GUEST AFTER A SAME-VERSION "+
					"REBUILD (mem0=%s, want %s). 5 is what the first session left "+
					"in storage.fk_mem: state_load declined and nothing "+
					"republished the mirror, so `storage` holds build-A's heap "+
					"while the guest runs on a different table entirely. Its "+
					"writes reach neither the save nor the multiplayer CRC: %v",
					got, arm.two[0], legs["two"])
			}
			if got := legs["two"]["mem1"]; got != arm.two[1] {
				t.Errorf("THE GUEST WAS NOT TOLD ABOUT THE REBUILD (mem1=%s, want "+
					"%s -- the state version the hook is handed). Both hooks lived "+
					"in on_configuration_changed, which Factorio raises for a "+
					"VERSION change and not for a rebuild that keeps it: %v",
					got, arm.two[1], legs["two"])
			}
			if got := legs["two"]["build"]; got != "build-B" {
				t.Errorf("A DECLINED LOAD LEFT THE SAVE CARRYING THE STAMP IT "+
					"DECLINED (build=%s, want build-B), so every later load "+
					"declines again and a joining client reads the same stale "+
					"stamp: %v", got, legs["two"])
			}
			if got := legs["two"]["logged"]; got != arm.twoLogged {
				t.Errorf("leg two logged %s rebuild notices, want %s: %v",
					got, arm.twoLogged, legs["two"])
			}

			// LEG THREE IS THE CONSISTENCY HALF. Handling it once is not enough if
			// what it leaves behind declines again next time.
			if got := legs["three"]["mem0"]; got != arm.three[0] {
				t.Errorf("THE NEXT LOAD DID NOT ADOPT (mem0=%s, want %s -- leg "+
					"two's state plus three more ticks). A load that handles a "+
					"rebuild has to leave a save an ordinary load can adopt: %v",
					got, arm.three[0], legs["three"])
			}
			if legs["three"]["logged"] != "0" {
				t.Errorf("an ordinary load of leg two's save handled a rebuild "+
					"again (%v), so the stamp leg two wrote is still not this "+
					"build's", legs["three"])
			}
		})
	}
}

// PACKED MODE DECLINES THE SAME WAY AND IS WORSE OFF, because the mirror it
// needs is not an alias.
//
// Under --persist=table a declined load at least leaves `storage.fk_mem` holding
// a coherent heap somebody once wrote. Under packed, `pages` is a local that
// state_load never set, so sync_memory's `if pages then P.flush(pages) end`
// flushes NOTHING for the life of the session -- the guest runs, the save keeps
// build A's pages, and the size mirror keeps build A's size. It is the same
// defect one mode over, and it is here because that is the shape this repo keeps
// finding: a guard written for the first instance of a pattern.
func TestARebuiltPackedGuestRepublishesItsPages(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	exports := []string{"fk_on_tick"}
	a := packRebuild(t, rebuildNoHookWAT, luagen.PersistPacked, "build-A", exports)
	b := packRebuild(t, rebuildNoHookWAT, luagen.PersistPacked, "build-B", exports)
	legs := runRebuildScript(t, h, packedRebuildReport, a, b)

	if legs["one"]["mem0"] != "5" {
		t.Fatalf("leg one did not establish the save: %v", legs["one"])
	}
	if got := legs["two"]["mem0"]; got != "3" {
		t.Errorf("A DECLINED PACKED LOAD FLUSHED NOTHING FOR THE WHOLE SESSION "+
			"(page 0 word 0 = %s, want 3). state_load returned before it set "+
			"`pages`, so sync_memory had nothing to flush and the save still "+
			"carries build A's pages after a session that ran three ticks over "+
			"them: %v", got, legs["two"])
	}
	if got := legs["two"]["build"]; got != "build-B" {
		t.Errorf("the packed decline left the stamp at %s, want build-B: %v",
			got, legs["two"])
	}
	if got := legs["three"]["mem0"]; got != "6" {
		t.Errorf("the next packed load did not adopt (page 0 word 0 = %s, want "+
			"6): %v", got, legs["three"])
	}
}

// THE DECLINE ITSELF STILL WRITES NOTHING FROM on_load, WHICH IS THE HALF THE
// FIX HAD TO NOT BREAK.
//
// state_load runs from script.on_load, which Factorio runs on EVERY PEER THAT
// LOADS THE STATE -- including a client joining a game in progress, and on no
// other peer. A write to `storage` there is CLAUDE.md's named desync. So the
// obvious repair for everything above -- have state_load republish the stamp
// itself -- is the one repair that is not available, and what it records instead
// is an UPVALUE, which is the one thing on_load may write.
//
// persist_test.go's TestOnLoadDoesNotWriteToStorage asserts the same property
// over a load that ADOPTS. This is the declining load, which is the arm that
// grew new work: it is now the arm that decides something has to happen later.
func TestADecliningLoadStillWritesNothingFromOnLoad(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, arm := range []struct{ name, wat string }{
		// Both gate arms, because they take different routes through state_load:
		// the discarding one returns at the gate, and fk_migrate_adopt falls
		// through it and goes on to adopt the heap, the handle space and the
		// buffer caches -- all reads of `storage` and writes to upvalues, and
		// every one of them a line that could have been a write the wrong way.
		{"discards", rebuildNoHookWAT},
		{"adopts", strings.Replace(rebuildWAT, "@@HOOK@@", "fk_migrate_adopt", 1)},
	} {
		t.Run(arm.name, func(t *testing.T) {
			exports := []string{"fk_on_tick"}
			if arm.name == "adopts" {
				exports = []string{"fk_on_tick", "fk_migrate_adopt", "fk_state_version"}
			}
			a := packRebuild(t, arm.wat, luagen.PersistTable, "build-A", exports)
			b := packRebuild(t, arm.wat, luagen.PersistTable, "build-B", exports)
			out, err := h.RunString(fmt.Sprintf(frozenLoadScript,
				filepath.Join(a, "?.lua"), filepath.Join(b, "?.lua")))
			if err != nil {
				t.Fatalf("A DECLINING LOAD WROTE TO `storage` FROM on_load: %v\n"+
					"That runs on a joining multiplayer client and on no other "+
					"peer, so the write lands on one peer and the game desyncs "+
					"from the tick after the join. The decline has to be recorded "+
					"in an UPVALUE and acted on at a replicated point.\n%s",
					err, out)
			}
			if got := strings.TrimSpace(out); got != "clean" {
				t.Errorf("got %q, want \"clean\"", got)
			}
		})
	}
}

// The load half by hand, with `storage` frozen: any assignment raises. The first
// session is run against build A to get a real save into the table, and the
// frozen copy is then loaded by build B -- so same_build() is false and this is
// the declining path rather than the ordinary one.
var frozenLoadScript = expandClearLoaded(`defines = { events = { on_tick = 1 } }
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
script = {
  mod_name = "fk-rebuild",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

local function boot(st, dir)
  handlers = {}
  storage = st
  package.path = dir
  --@CLEAR_LOADED@
  require("control")
end

boot({}, %q)
handlers.on_init()
for tick = 1, 5 do handlers[1]({ tick = tick }) end

-- The save, frozen -- AS A PROXY OVER AN EMPTY TABLE, which is the only shape
-- that actually catches anything. __newindex fires only for a key the table does
-- not already have, so freezing a POPULATED copy catches a write to fk_deferred
-- and lets a write to fk_build straight through -- and fk_build is the one key
-- this whole file is about. Confirmed by mutation: with the populated form,
-- replacing state_load's upvalue write with a direct storage write left
-- this test green when state_load's upvalue write was replaced by a write of
-- P.build straight into storage.fk_build.
--
-- So the backing store holds the save and the proxy holds nothing, which makes
-- every assignment a new key by construction. Nothing on the load path iterates
-- storage -- state_load and after_load read named fields and nothing else -- so
-- __index alone is enough on the read side.
local real = {}
for k, v in pairs(storage) do real[k] = deepcopy(v) end
local frozen = setmetatable({}, {
  __index = real,
  __newindex = function(_, k)
    error("on_load wrote storage." .. tostring(k), 0)
  end,
})

boot(frozen, %q)
handlers.on_load()
print("clean")
`)

// runRebuildScript drives three sessions of one mod through one interpreter and
// parses the three report lines. `report` is the mode-specific reader for word 0
// and word 1, because table mode aliases the word table into `storage` and
// packed mode mirrors it as 4 KiB strings.
func runRebuildScript(t *testing.T, h *luahost.Host, report, dirA, dirB string) map[string]map[string]string {
	t.Helper()
	pa, pb := filepath.Join(dirA, "?.lua"), filepath.Join(dirB, "?.lua")
	out, err := h.RunString(fmt.Sprintf(rebuildScript, report, pa, pb, pb))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
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
	for _, name := range []string{"one", "two", "three"} {
		if legs[name] == nil {
			t.Fatalf("the script never reached leg %s:\n%s", name, out)
		}
		t.Logf("%s %v", name, legs[name])
	}
	return legs
}

// Table mode: storage.fk_mem IS the guest's shard vector, so word N is
// fk_mem[1][N+1].
const tableRebuildReport = `
local function words()
  local m = storage.fk_mem
  if not m then return "nil", "nil" end
  return tostring(m[1][1]), tostring(m[1][2])
end`

// Packed mode: the mirror is one string per 4 KiB page, so word 0 is bytes 1..4
// of page 1. Word 1 is not read -- the packed arm has no migrate hook, so there
// is nothing there to read -- but the reader returns a pair either way so the
// script below is the same script.
const packedRebuildReport = `
local function words()
  local p = storage.fk_pages
  if not p or not p[1] then return "nil", "nil" end
  return tostring(string.unpack("<I4", p[1], 1)), "0"
end`

// Three sessions of one mod in one interpreter, with only `storage` crossing.
//
// on_configuration_changed IS NEVER CALLED, which is the whole point: this is
// what a rebuild that keeps the mod's version looks like to the engine, and
// every other harness in this repo calls it unconditionally.
//
// The dir argument models the repackage. Every package.loaded entry a mod ships
// is cleared because a load re-executes every one of them, and a stale one would
// be the previous build still in the room -- see clearloaded_test.go.
var rebuildScript = expandClearLoaded(`%s
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

local function session(saved, ticks, dir)
  local handlers = {}
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

  if saved then
    if handlers.on_load then handlers.on_load() end
    -- AND NOTHING HERE. Factorio raises on_configuration_changed for a mod-set
    -- change; a rebuild that keeps the version is not one.
  else
    if handlers.on_init then handlers.on_init() end
  end
  for tick = 1, ticks do
    local f = handlers[defines.events.on_tick]
    if f then f({ tick = tick }) end
  end
  return storage
end

local function report(tag)
  local w0, w1 = words()
  print(tag .. " mem0=" .. w0 .. " mem1=" .. w1 ..
        " build=" .. tostring(storage.fk_build) ..
        " logged=" .. tostring(#logged))
  for _, s in ipairs(logged) do print("LOG " .. s) end
end

local save = session(nil, 5, %q)
report("one")

local two = session(save, 3, %q)
report("two")

session(two, 3, %q)
report("three")
`)
