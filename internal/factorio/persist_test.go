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

// A guest that accumulates state: it counts the ticks it has seen in linear
// memory and keeps a running total in a mutable global, so a round trip has to
// carry both kinds of state or one of the two numbers comes back wrong.
const counterWAT = `(module
	(memory 1)
	(global $total (mut i32) (i32.const 0))
	(func (export "fk_on_tick") (param $tick i32)
		(i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1)))
		(global.set $total (i32.add (global.get $total) (local.get $tick))))
	(func (export "fk_seen") (result i32) (i32.load (i32.const 0)))
	(func (export "fk_total") (result i32) (global.get $total)))`

func packAt(t *testing.T, wat string, mode luagen.PersistMode) string {
	return packBuild(t, wat, mode, "build-one", nil)
}

// packBuild packages a module under a chosen build identity, which is how a
// test models a REBUILD: same source, different id, exactly as recompiling a
// guest produces.
func packBuild(t *testing.T, wat string, mode luagen.PersistMode, id string, exports []string) string {
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
	if exports == nil {
		exports = []string{"fk_on_tick", "fk_seen", "fk_total"}
	}
	pkg := &Package{
		Info: Info{
			Name: "fk-counter", Version: "0.1.0", Title: "Counter",
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

// saveLoadStub drives a packaged mod through a genuine save/load boundary.
//
// The important part is that the second session gets a FRESH Lua state and a
// fresh `require` -- a reloaded control.lua, a rebuilt module, everything the
// guest had in upvalues gone. Only `storage` crosses, which is exactly what
// Factorio carries: it serializes that table and nothing else.
//
// Copying `storage` between the two sessions models the serialization without
// performing it. A deep copy is what makes the test honest: sharing the table
// would let a live reference sneak across the boundary and pass a mod that
// would break in game.
func saveLoadStub(modDir string, firstTicks, secondTicks int) string {
	return fmt.Sprintf(expandClearLoaded(`package.path = %q
defines = { events = { on_tick = 1 } }

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local out = {}
  for k, x in pairs(v) do out[deepcopy(k)] = deepcopy(x) end
  return out
end

logged = {}
function log(s) logged[#logged + 1] = s end

-- One session: load control.lua from scratch, fire on_init or on_load, run
-- some ticks, and hand back whatever ended up in storage.
--
-- The dir argument lets a later session load a DIFFERENT build of the same mod,
-- which is how a recompile is modelled.
local function session(saved, ticks, dir)
  local handlers = {}
  script = {
    mod_name = "fk-counter",
    on_init = function(f) handlers.on_init = f end,
    on_load = function(f) handlers.on_load = f end,
    on_configuration_changed = function(f) handlers.on_config = f end,
    on_event = function(ev, f) handlers[ev] = f end,
  }
  storage = saved and deepcopy(saved) or {}
  if dir then package.path = dir end
  -- Every module, because control.lua requires the others and a real load
  -- re-executes every one of them. See clearloaded_test.go.
  --@CLEAR_LOADED@
  local ok, err = pcall(require, "control")
  if not ok then error(err, 0) end

  if saved then
    if handlers.on_load then handlers.on_load() end
    -- Factorio fires this after on_load and before the first tick whenever the
    -- mod set changed, which includes this mod's own version moving.
    if handlers.on_config then handlers.on_config() end
  else
    if handlers.on_init then handlers.on_init() end
  end
  for tick = 1, ticks do
    local f = handlers[defines.events.on_tick]
    if f then f({ tick = tick }) end
  end
  return storage, _G.M
end

local first = session(nil, %d)
local second = session(first, %d)
`), filepath.Join(modDir, "?.lua"), firstTicks, secondTicks)
}

// The M6 promise, at the smallest scale that can express it: state a guest
// accumulated before a save is still there after the load, and it keeps
// accumulating from where it left off rather than from zero.
func TestGuestStateSurvivesASaveInTableMode(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistTable)

	// 5 ticks, save, load, 3 more. Memory counts 8 ticks; the global sums the
	// tick numbers each session saw: (1..5) + (1..3) = 15 + 6 = 21.
	script := saveLoadStub(dir, 5, 3) + `
-- fk_mem is the shard VECTOR now, so word 1 is fk_mem[1][1].
print("seen " .. tostring(second.fk_mem[1][1]))
print("total " .. tostring(second.fk_globals[1]))
`
	out, err := h.RunString(script)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Memory is carried by aliasing; the global is carried by the explicit
	// copy-back after each call. They fail independently, so both are named.
	want := "seen 8\ntotal 21"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the second session should have continued "+
			"from where the first stopped, not restarted from zero)", got, want)
	}
}

// And the negative: --persist=none is unchanged by M6. Nothing reaches
// `storage` at all, so the second session starts from the data segments.
func TestNoneModeTouchesNothing(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistNone)
	script := saveLoadStub(dir, 5, 3) + `
local n = 0
for _ in pairs(second) do n = n + 1 end
print("storage keys " .. n)
`
	out, err := h.RunString(script)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := strings.TrimSpace(out), "storage keys 0"; got != want {
		t.Errorf("got %q, want %q -- a --persist=none mod must not write to "+
			"storage, or it pays a save cost for state it never reads back", got, want)
	}
}

// on_load must not write to `storage`. Factorio runs it on every client joining
// a multiplayer game, and a write there is a desync waiting to happen -- one
// that shows up as a corrupted save days later rather than as an error.
//
// THE FREEZE IS A PROXY OVER AN EMPTY TABLE, and the shape is the whole test.
// __newindex fires only for a key the table does NOT already have, so freezing a
// POPULATED copy of the save -- which is what this did until 2026-08-07 -- lets
// every write to a key the load path was already going to touch straight
// through, silently. That is nearly all of them: fk_mem, fk_build, fk_handles,
// fk_bufs and fk_deferred are all present in a save taken after four ticks, so
// the assertion covered only a write to a key no session had ever created.
// Confirmed by mutation both ways -- see the subtest below.
//
// rebuild_test.go's frozenLoadScript found the same thing first and is the
// precedent this is rebuilt on: the backing store holds the save, the proxy
// holds nothing, __index reaches through for the reads, and every assignment is
// a new key by construction. Nothing on the load path iterates `storage` --
// state_load and after_load read named fields and nothing else -- so __index
// alone is enough on the read side.
func TestOnLoadDoesNotWriteToStorage(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistTable)
	script := saveLoadStub(dir, 4, 0) + expandClearLoaded(`
-- Re-run the load half with storage frozen: any assignment raises.
local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local o = {}
  for k, x in pairs(v) do o[deepcopy(k)] = deepcopy(x) end
  return o
end
local real = {}
for k, v in pairs(first) do real[k] = deepcopy(v) end
local frozen = setmetatable({}, {
  __index = real,
  __newindex = function(_, k)
    error("on_load wrote storage." .. tostring(k), 0)
  end,
})

local handlers = {}
script = {
  mod_name = "fk-counter",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
storage = frozen
-- Every module, for saveLoadStub's reason: this is the same load modelled by
-- hand.
--@CLEAR_LOADED@
require("control")
handlers.on_load()
print("clean")
`)
	out, err := h.RunString(script)
	if err != nil {
		t.Fatalf("on_load wrote to storage: %v", err)
	}
	if got := strings.TrimSpace(out); got != "clean" {
		t.Errorf("got %q", got)
	}

	// THE POSITIVE CONTROL, because "nothing raised" is what a freeze that
	// cannot fire also reports. It writes an EXISTING key -- fk_mem, which any
	// table-mode save has -- which is the write the populated form let through
	// and the whole reason this test was rebuilt. Bolted onto the same script so
	// it exercises the same proxy over the same save rather than a fresh one
	// built to be catchable.
	t.Run("the freeze catches a write to a key the save already has", func(t *testing.T) {
		out, err := h.RunString(script + `
storage.fk_mem = "clobbered"
print("NOT CAUGHT")
`)
		if err == nil {
			t.Fatalf("the frozen storage did not raise on a write to fk_mem, a "+
				"key the save ALREADY HAS -- so this file's on_load assertion is "+
				"vacuous again and passes whatever the load path writes.\n%s", out)
		}
		if !strings.Contains(err.Error(), "on_load wrote storage.fk_mem") {
			t.Errorf("raised, but not from the freeze: %v", err)
		}
	})
}

// A guest that can migrate: fk_migrate rewrites the counter it finds so the
// test can see that it ran AND that it was handed the old heap to work on.
const migratableWAT = `(module
	(memory 1)
	(func (export "fk_on_tick") (param $tick i32)
		(i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1))))
	(func (export "fk_state_version") (result i32) (i32.const 7))
	(func (export "fk_migrate") (param $old i32)
		(i32.store (i32.const 4) (local.get $old))
		(i32.store (i32.const 0) (i32.mul (i32.load (i32.const 0)) (i32.const 100)))))`

// The same guest, exporting the ADOPTING half instead. Identical body: what
// differs is only which export control.lua finds, and therefore whether the old
// build's linear memory is underneath it.
var adoptingWAT = strings.Replace(migratableWAT,
	`(func (export "fk_migrate") (param $old i32)`,
	`(func (export "fk_migrate_adopt") (param $old i32)`, 1)

// twoBuilds runs session one on build A and session two on build B -- which is
// exactly what a mod author does when they recompile and their users load an
// existing save.
func twoBuilds(t *testing.T, wat string, exports []string, firstTicks, secondTicks int) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := packBuild(t, wat, luagen.PersistTable, "build-A", exports)
	b := packBuild(t, wat, luagen.PersistTable, "build-B", exports)
	script := fmt.Sprintf(`%s
local second2 = select(1, session(first, %d, %q))
`, saveLoadStub(a, firstTicks, 0), secondTicks, filepath.Join(b, "?.lua"))
	out, err := h.RunString(script + `
print("mem0 " .. tostring(storage.fk_mem[1][1]))
print("mem1 " .. tostring(storage.fk_mem[1][2]))
print("build " .. tostring(storage.fk_build))
print("state " .. tostring(storage.fk_state))
print("warned " .. tostring(#logged > 0))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.TrimSpace(out)
}

// The dangerous case, handled safely. A rebuilt guest with no fk_migrate never
// sees the old heap: it was not adopted in on_load, and on_configuration_changed
// discards it and republishes a fresh one.
//
// Losing state cleanly beats running a guest on bytes laid out by a different
// build. In a lockstep game the second option is not "slightly wrong data", it
// is every client desyncing on whatever the garbage decodes as.
func TestARebuiltGuestWithoutMigrateStartsClean(t *testing.T) {
	got := twoBuilds(t, counterWAT, nil, 5, 3)
	want := strings.Join([]string{
		"mem0 3", // the 5 ticks before the rebuild are gone, not merged
		"mem1 0", // and fk_migrate never ran, so it never wrote its marker here
		"build build-B",
		"state 0",
		"warned true", // and the author is told, by name
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// fk_migrate IS A NOTIFICATION, ON A FRESH HEAP -- and it used to be an
// adoption, which is why no guest could use it.
//
// Adopting means replacing the module's ENTIRE linear memory with the saved
// one, and linear memory is not just the heap: it is .data and .rodata too. A
// rebuilt guest refers to its string constants, its type descriptors and its
// static buffers by compiled-in ADDRESS, and every one of those now points at
// whatever the previous build put there. The first thing fk_migrate did was
// therefore already undefined -- it would send the host a string read out of
// somebody else's rodata -- so the hook offered a choice between losing state
// silently and corrupting it silently, and the first downstream mod exported
// nothing and rebuilt from the world instead.
//
// Split in two, the safe half keeps the obvious name: the guest is told which
// state version wrote the save, on the heap _initialize just built, which is
// all a rebuild-from-world needs.
func TestMigrateIsToldAboutTheRebuildAndGetsAFreshHeap(t *testing.T) {
	exports := []string{"fk_on_tick", "fk_migrate", "fk_state_version"}
	got := twoBuilds(t, migratableWAT, exports, 5, 3)
	want := strings.Join([]string{
		// 0 * 100 + 3: the counter was NOT carried over, so what fk_migrate
		// multiplied was the fresh zero. The old heap was never underneath it.
		"mem0 3",
		"mem1 7", // and it was still handed the previous state version
		"build build-B",
		"state 7",
		"warned false", // no warning: the guest asked to be told and was
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// And the opt-in that really does hand over the old bytes, under a name that
// says so. For a guest whose state is a fixed versioned region it interprets
// itself -- hand-written wasm, a repr(C) blob -- and which can therefore
// survive reading its own constants from the wrong build. A Go or Rust guest is
// not that; see the fk_migrate section of agents/guests.md.
func TestMigrateAdoptReallyGetsTheOldHeap(t *testing.T) {
	exports := []string{"fk_on_tick", "fk_migrate_adopt", "fk_state_version"}
	got := twoBuilds(t, adoptingWAT, exports, 5, 3)
	want := strings.Join([]string{
		// 5 ticks, then migrate multiplied by 100, then 3 more: the old heap
		// was really there to be read.
		"mem0 503",
		"mem1 7",
		"build build-B",
		"state 7",
		"warned false",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Reloading the SAME build must not look like a migration. A mod that warned
// and reset on every ordinary load would be worse than no persistence at all.
func TestTheSameBuildIsNotAMigration(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistTable)
	out, err := h.RunString(saveLoadStub(dir, 5, 3) + `
-- fk_mem is the shard VECTOR now, so word 1 is fk_mem[1][1].
print("seen " .. tostring(second.fk_mem[1][1]))
print("warned " .. tostring(#logged > 0))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := strings.TrimSpace(out), "seen 8\nwarned false"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// serialize renders storage in a canonical order so two of them can be compared
// as text. `pairs` order is insertion order in Factorio's Lua and is stable, but
// relying on that to compare two tables built by different code paths would be
// testing the wrong thing.
const serializeLua = `
local function ser(v, out)
  if type(v) ~= "table" then out[#out + 1] = tostring(v) return end
  local keys = {}
  for k in pairs(v) do keys[#keys + 1] = k end
  table.sort(keys, function(a, b) return tostring(a) < tostring(b) end)
  out[#out + 1] = "{"
  for _, k in ipairs(keys) do
    out[#out + 1] = tostring(k) .. "="
    ser(v[k], out)
    out[#out + 1] = ","
  end
  out[#out + 1] = "}"
end
local function dump(t) local o = {} ser(t, o) return table.concat(o) end
`

// save -> load -> save is byte-identical: adopting a heap must not perturb it.
//
// This is the half of the M6 gate that can be checked without a real Factorio
// save cycle. If loading changed anything at all -- a rebuilt data segment
// landing on top of live state, a global reset to its initialiser -- the second
// dump would differ, and in a lockstep game that difference is a desync on the
// first tick after a join.
func TestSaveLoadSaveIsIdentical(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistTable)
	out, err := h.RunString(saveLoadStub(dir, 7, 0) + serializeLua + `
-- session two ran ZERO ticks, so nothing but the load itself touched state.
print(dump(first) == dump(second) and "identical" or "DIFFERS")
print(dump(first))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	if lines[0] != "identical" {
		t.Errorf("a load perturbed the saved state:\n%s", out)
	}
	// And it is not identical because both are empty.
	if !strings.Contains(lines[1], "fk_mem=") {
		t.Errorf("nothing was saved, so the comparison proved nothing:\n%s", lines[1])
	}
}

// The same guest, run twice from the same starting state, must reach the same
// state. Factorio is lockstep: every client runs this code and any divergence
// desyncs the game.
func TestTwoRunsFromTheSameStateAgree(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// Only table mode is checkable this way, and the reason is worth stating:
	// under --persist=none the guest's state lives in Lua upvalues that nothing
	// outside the chunk can reach, so comparing `storage` would compare two
	// empty tables and pass whatever the guest did. A test that cannot fail is
	// worse than no test.
	dir := packAt(t, counterWAT, luagen.PersistTable)
	out, err := h.RunString(saveLoadStub(dir, 3, 0) + serializeLua + `
-- Two independent sessions from the same save, same tick count. These are the
-- two clients.
local a = session(first, 25, nil)
local b = session(first, 25, nil)
print(dump(a) == dump(b) and "agree" or "DIVERGE")
print(dump(a) == dump(first) and "UNCHANGED" or "moved")
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "agree" {
		t.Errorf("two clients diverged from identical input (%s)", lines[0])
	}
	// And they agree on something the 25 ticks actually changed, rather than on
	// a state neither of them touched.
	if len(lines) < 2 || lines[1] != "moved" {
		t.Errorf("the 25 ticks changed nothing, so agreement proved nothing: %v", lines)
	}
}

// Packed mode has to be INDISTINGUISHABLE from table mode to the guest. The
// only thing that changes is the shape of what lands in storage: one string per
// 4 KiB page instead of one entry per word.
func TestPackedModeCarriesTheSameStateAsTableMode(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistPacked)
	out, err := h.RunString(saveLoadStub(dir, 5, 3) + `
-- Word 0 lives in the first four bytes of page 1.
local w0 = string.unpack("<I4", second.fk_pages[1], 1)
print("seen " .. tostring(w0))
print("total " .. tostring(second.fk_globals[1]))
print("pages " .. tostring(#second.fk_pages))
print("mem   " .. tostring(second.fk_mem))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Same 8 and 21 as the table-mode test: 5 ticks, save, load, 3 more.
	// A 64 KiB guest heap is 16 pages of 4 KiB. And nothing put the word table
	// itself in storage -- that is the entire point of the mode.
	want := "seen 8\ntotal 21\npages 16\nmem   nil"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The dirty-page set decides which pages get rebuilt, so an unmarked write
// it recorded would be silently dropped at the next save. This writes to a page
// far from page 0 and checks it survives.
func TestPackedFlushCoversEveryPageTheGuestTouched(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// Writes one word into page 0 and one into page 9, per tick.
	const scattered = `(module
		(memory 1)
		(func (export "fk_on_tick") (param $tick i32)
			(i32.store (i32.const 0) (local.get $tick))
			(i32.store (i32.const 40000) (i32.add (local.get $tick) (i32.const 1000)))))`
	dir := packAt(t, scattered, luagen.PersistPacked)
	out, err := h.RunString(saveLoadStub(dir, 4, 0) + `
print("page0 " .. tostring(string.unpack("<I4", second.fk_pages[1], 1)))
-- byte 40000 is in page 9 (40000 // 4096), at offset 40000 - 9*4096 = 3136,
-- which is 1-based position 3137.
print("page9 " .. tostring(string.unpack("<I4", second.fk_pages[10], 3137)))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := strings.TrimSpace(out), "page0 4\npage9 1004"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a page the set missed comes back stale)", got, want)
	}
}

// Packed saves should be much smaller than table saves. This is the whole
// reason the mode exists, so it is asserted rather than assumed.
func TestPackedStorageIsSmallerThanTableStorage(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	count := func(mode luagen.PersistMode) int {
		dir := packAt(t, counterWAT, mode)
		out, err := h.RunString(saveLoadStub(dir, 2, 0) + `
local function entries(v)
  if type(v) ~= "table" then return 1 end
  local n = 0
  for _, x in pairs(v) do n = n + entries(x) end
  return n
end
print(entries(first))
`)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		var n int
		fmt.Sscan(strings.TrimSpace(out), &n)
		return n
	}
	tbl, pk := count(luagen.PersistTable), count(luagen.PersistPacked)
	if pk >= tbl/100 {
		t.Errorf("packed storage is %d entries against table's %d; the mode exists "+
			"to make that ratio large", pk, tbl)
	}
	t.Logf("storage entries: table %d, packed %d (%.0fx fewer)",
		tbl, pk, float64(tbl)/float64(pk))
}

// A guest whose linear memory grows past the point where Lua's collector starts
// costing whole frames should SAY SO, because nothing else downstream can see it.
//
// The live memory is a Lua array table with one slot per 32-bit word, and
// `traversestrongtable` walks every slot of it in a single `propagatemark` —
// one gray object, one indivisible unit of work, no pacing parameter can split
// it. Measured in Factorio 2.0.77 the worst tick is ~0.2 ms per MiB of LINEAR
// MEMORY, and that is memory declared-or-grown, not memory used: `mem_grow`
// writes a zero into every new word, and TinyGo's wasm `growHeap` DOUBLES the
// memory each time it runs out. A guest that needs 65 MiB gets 128 and pays
// ~26 ms of worst tick for a heap that is half untouched.
//
// The first downstream mod spent two rounds attributing that pause to
// `--persist` (it is not a persistence cost — the live table is the same object
// in every mode) with no way to see how big its heap had become. The size
// comparison this hangs off already happens on every guest entry, so the notice
// is free.
func TestAGuestThatGrowsIntoAGiantHeapSaysSo(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// 1 page to start, +255 on the first tick: 16 MiB, i.e. 4,194,304 live Lua
	// slots, which is where the pause stops being invisible (~3 ms).
	const growWAT = `(module
	(memory 1)
	(func (export "fk_on_tick") (param $tick i32)
		(drop (memory.grow (i32.const 255)))))`

	for _, mode := range []luagen.PersistMode{luagen.PersistTable, luagen.PersistPacked} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := packAt(t, growWAT, mode)
			out, err := h.RunString(saveLoadStub(dir, 1, 0) + `
for _, s in ipairs(logged) do print(s) end
`)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !strings.Contains(out, "16 MiB") || !strings.Contains(out, "linear memory") {
				t.Errorf("a guest that grew to 16 MiB logged:\n%s\nwant a line naming its "+
					"linear memory size — it is the only number that predicts the GC pause, "+
					"and the guest cannot see it", out)
			}
		})
	}
}

// THE 4 MiB WALL NOTICE IS GONE, AND THIS IS WHAT REPLACED IT.
//
// It used to fire four doublings below the budget notice, because 4 MiB of
// linear memory was 2^20 words in ONE Lua table and past that a table in
// FACTORIO stops behaving like an array for all of its keys: 200,000 stores
// into keys 1..200,000 cost 24 ms at 1,000,000 words and 482 ms at 1,100,000,
// the grow that crossed was a 2.7-second tick, and every LOAD paid ~2.9 s
// rebuilding the table. Nothing host-side could see it -- bin/lua52f is stock
// 5.2.1 and prices the same crossing at 3.0 ms against 1.3 -- so the notice was
// the whole channel.
//
// Linear memory is a vector of 2^19-word shards now, so no table the guest runs
// on can reach 2^20 keys at any size, and every sentence that notice printed is
// false. What this test asserts is therefore in two halves: the notice does NOT
// fire, and the reason it must not -- the memory past 4 MiB really is in shards
// and no shard is over the line. The second half is the load-bearing one. A
// guest whose notice was merely deleted would pass the first.
func TestAGuestPastFourMiBIsShardedAndSaysNothingAboutAWall(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// 60 pages declared, +8 on the first tick: 4.25 MiB, i.e. across what used
	// to be the wall and nowhere near the 16 MiB the budget notice waits for.
	const wallWAT = `(module
	(memory 60)
	(func (export "fk_on_tick") (param $tick i32)
		(drop (memory.grow (i32.const 8)))))`

	for _, mode := range []luagen.PersistMode{luagen.PersistTable, luagen.PersistPacked} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := packAt(t, wallWAT, mode)
			out, err := h.RunString(saveLoadStub(dir, 1, 0) + `
for _, s in ipairs(logged) do print(s) end
-- In table mode storage.fk_mem IS the live shard vector, so reading the shape
-- out of storage asserts the aliasing invariant one level down at the same
-- time: a grow APPENDS a shard to a table storage already holds. In packed
-- mode there is no vector in storage and only the notices are checked.
local mem = storage.fk_mem
if mem then
  local n, worst = 0, 0
  for i = 1, 1000 do
    local sh = mem[i]
    if not sh then break end
    n = n + 1
    if #sh > worst then worst = #sh end
  end
  print("shards " .. n)
  print("worst " .. worst)
end
`)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if strings.Contains(out, "wall") {
				t.Errorf("a 4.25 MiB guest still logged something about a wall:\n%s\n"+
					"there is no wall to warn about -- see agents/sharding.md", out)
			}
			if strings.Contains(out, "16 MiB") {
				t.Errorf("the BUDGET notice fired for a 4.25 MiB guest:\n%s", out)
			}
			if mode != luagen.PersistTable {
				return
			}
			// 4.25 MiB is 1,114,112 words: three shards, the last one partial.
			if !strings.Contains(out, "shards 3") {
				t.Errorf("4.25 MiB of guest memory is not in three shards:\n%s", out)
			}
			// THE PROPERTY THE WHOLE MILESTONE RESTS ON. Anything over 2^20
			// keys stops being an array in Factorio; 2^19 is the size chosen so
			// that can never happen, and this is where it is checked rather
			// than assumed.
			if !strings.Contains(out, "worst 524288") {
				t.Errorf("some shard is not 2^19 words:\n%s\nA shard over 2^20 keys is the "+
					"exact failure sharding exists to prevent", out)
			}
		})
	}
}

// The wall notice must not fire below the wall, for the same reason the budget
// notice must not fire below 16 MiB: a line every mod prints is a line every mod
// author learns to skip. A guest at the `auto` threshold's 1 MiB is a quarter of
// the way there and pays none of it.
func TestAGuestUnderTheFourMiBWallSaysNothingAboutIt(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	const underWAT = `(module
	(memory 16)
	(func (export "fk_on_tick") (param $tick i32)
		(drop (memory.grow (i32.const 8)))))`

	dir := packAt(t, underWAT, luagen.PersistTable)
	out, err := h.RunString(saveLoadStub(dir, 1, 0) + `
for _, s in ipairs(logged) do print(s) end
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "wall") {
		t.Errorf("a 1.5 MiB guest logged the wall notice:\n%s", out)
	}
}

// The notice is for heaps that actually cost something. A guest sitting on the
// 1 MiB the `auto` threshold is already about must not log at all, or the line
// becomes noise every mod author learns to skip.
func TestASmallHeapSaysNothing(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packAt(t, counterWAT, luagen.PersistTable)
	out, err := h.RunString(saveLoadStub(dir, 2, 0) + `
for _, s in ipairs(logged) do print(s) end
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "linear memory") {
		t.Errorf("a 64 KiB guest logged a heap notice:\n%s", out)
	}
}
