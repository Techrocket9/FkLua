package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// THE PER-LEVEL EVENT BUFFER IS ALLOCATED ONCE PER HEAP, NOT ONCE PER LOAD.
//
// fk_mod.lua caches the event scratch buffer it dispatches through in a Lua
// LOCAL, one entry per nesting level, allocated lazily through fk_alloc_static
// at the first dispatch that needs one. A Lua local is rebuilt empty by every
// load, because a load re-executes control.lua. The guest HEAP is not: it comes
// back from the save with that buffer already in it, at the address the previous
// session put it.
//
// So the first dispatch after a load allocated a SECOND buffer beside one that
// was already there. Three consequences, in rising order of how much they
// matter: event_scratch bytes of guest heap leaked per level per load, into the
// save; one more entry pinned in the guest's `kept` list, which the collector
// then keeps forever; and every allocation the loaded instance makes afterwards
// landing that much further up than on an instance that never reloaded. The
// last one is the reason this is a runtime defect and not an untidiness --
// under the default --persist=table the guest heap IS storage.fk_mem, which
// Factorio CRCs, and script.on_load runs on a JOINING CLIENT and on no other
// peer. See CLAUDE.md's "no peer-local signal may mutate guest state", and P12
// in agents/ipc.md.
//
// The fix is a `storage` mirror under the same_build() gate state_load already
// applies to the heap, which is what storage.fk_deferred, storage.fk_gc and
// storage.fk_handles are all doing there. This asserts it through the real
// control.lua, over six sessions of one guest:
//
//	A  a new map. One dispatch, one buffer.
//	B  a LOAD of A's save, same build. The heap is adopted and the buffer is
//	   REUSED -- same address, nothing newly pinned.
//	C  a load whose saved build stamp does not match: the heap is discarded, so
//	   the cache must be discarded with it. It allocates, in a fresh heap.
//	D  a load of a save that carries NO mirror, which is what every save
//	   written before the fix looks like. It pays for exactly one twin and then
//	   publishes a mirror from inside the dispatch that made it...
//	E  ...so the next load reuses. The last load pays, rather than every load.
//	F  a mirror recorded at a size this build does not ask for. Refused: an
//	   address does not carry its own length, and reusing one that is too small
//	   is a silent overwrite rather than a wasted allocation. fk_migrate_adopt
//	   is what reaches it -- it hands over another build's heap on purpose.
//
// It is asserted at the ADDRESS and at the ALLOCATION COUNT, both read out of
// the guest's own linear memory, because those are the two things that moved.
// A test that only compared what the handler decoded would see nothing at all:
// every leg here reads the right event.
func TestTheEventBufferIsAllocatedOncePerHeapAndNotOncePerLoad(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var chatID int
	for _, e := range events.Events {
		if e.Name == "on_console_chat" {
			chatID = e.ID
		}
	}
	if chatID == 0 {
		t.Skip("this API has no on_console_chat")
	}

	// The guest is an instrument and nothing else. Three words it never reads
	// back, well below anything the runtime touches: the bump allocator starts
	// at 32768 and the string scratch region is 16384..20480.
	//
	//	1024  how many times fk_alloc_static has been called. The `kept` list
	//	      in a real guest, counted rather than held.
	//	1028  the buffer address the host most recently dispatched through.
	//	1032  how many events have been dispatched.
	//
	// The dispatch count is what separates "the buffer was reused" from "this
	// is a fresh heap that happens to allocate at the same place": in a leg
	// that adopted, it carries A's value forward; in a leg that did not, it
	// starts again at one. Without it, C and A are indistinguishable.
	//
	// The bump pointer is a wasm GLOBAL rather than a memory word on purpose --
	// that is where a real guest keeps it, and it means this exercises
	// P.setglobals restoring it across the load exactly as the heap is restored.
	//
	// The subscription is made from _initialize and NOT from fk_on_init, which
	// is the difference between a guest and a fixture: fk_on_init runs on a new
	// map only, and control.lua is re-executed on every load, so _initialize is
	// where a real guest subscribes -- see fk_mod.lua's own note about it. A
	// guest that subscribed from fk_on_init would receive no events at all in
	// any of the four load legs below, which is how this was found.
	const (
		keptAt  = 1024
		bufAt   = 1028
		dispAt  = 1032
		heapAt  = 32768
		scratch = 16384
	)
	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const %d))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(i32.store (i32.const %d)
				(i32.add (i32.load (i32.const %d)) (i32.const 1)))
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_scratch_base") (result i32) (i32.const %d))
		(func (export "fk_scratch_size") (result i32) (i32.const 4096))
		(func (export "_initialize") (drop (call $sub (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(i32.store (i32.const %d) (local.get $ptr))
			(i32.store (i32.const %d)
				(i32.add (i32.load (i32.const %d)) (i32.const 1)))))`,
		heapAt, keptAt, keptAt, scratch, chatID, bufAt, dispAt, dispAt)

	im := buildIR(t, wat)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	// Table mode, so storage.fk_mem IS the guest's word table and the three
	// counters can be read straight out of the save -- and because table mode is
	// the mode in which this divergence is a CRC failure rather than a private
	// discrepancy. A BuildID has to exist at all: state_load refuses to adopt a
	// heap with no stamp, and a leg that adopted nothing would "pass" by
	// starting from the same fresh memory every time.
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable, BuildID: "buf-pin"})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-bufpin", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"_initialize", "fk_on_event", "fk_alloc",
			"fk_alloc_static", "fk_free", "fk_scratch_base", "fk_scratch_size"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Word indices into the aliased shard: byte/4 + 1.
	out, err := h.RunString(fmt.Sprintf(bufPinScript,
		filepath.Join(dir, "?.lua"), chatID, chatID, chatID,
		keptAt/4+1, dispAt/4+1, bufAt/4+1))
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
	for _, name := range []string{"A", "B", "C", "D", "E", "F"} {
		if legs[name] == nil {
			t.Fatalf("the script never reached leg %s:\n%s", name, out)
		}
		t.Logf("%s %v", name, legs[name])
	}
	get := func(leg, k string) string { return legs[leg][k] }

	// The harness has to be doing the thing before any of this means anything:
	// one buffer in A, and a mirror in `storage` carrying its address.
	if get("A", "kept") != "1" || get("A", "disp") != "1" {
		t.Fatalf("leg A did not allocate exactly one buffer for one dispatch: %v",
			legs["A"])
	}
	for _, leg := range []string{"A", "B", "C", "D", "E", "F"} {
		if b, m := get(leg, "buf"), get(leg, "mirror"); b == "" || b != m {
			t.Errorf("leg %s dispatched through %s while the storage mirror said "+
				"%s -- the mirror is what a load reads, so the two disagreeing "+
				"means the next load gets the wrong address", leg, b, m)
		}
	}

	// B IS THE FIX. Same build, so the heap is adopted -- which `disp` proves,
	// because a fresh heap would have restarted the count -- and the buffer that
	// came back with it is the one used, with nothing newly allocated.
	if get("B", "disp") != "2" {
		t.Fatalf("leg B did not adopt A's heap (disp=%s, want 2), so it cannot "+
			"say anything about reuse: %v", get("B", "disp"), legs["B"])
	}
	if get("B", "buf") != get("A", "buf") {
		t.Errorf("A LOAD ALLOCATED A SECOND EVENT BUFFER: leg A dispatched "+
			"through %s and leg B, over A's own heap, through %s. Every "+
			"allocation the loaded instance makes afterwards lands that far "+
			"further up than on an instance that never reloaded -- which on a "+
			"multiplayer join is the server and the joining client disagreeing "+
			"about storage.fk_mem forever.", get("A", "buf"), get("B", "buf"))
	}
	if get("B", "kept") != get("A", "kept") {
		t.Errorf("a load pinned a new static allocation: kept went %s -> %s. The "+
			"buffer was already in the heap it just adopted.",
			get("A", "kept"), get("B", "kept"))
	}

	// C IS THE NEGATIVE, and it has to fail in the OTHER direction. The saved
	// build stamp does not match, so state_load declines the heap; a cache
	// carried over from it would be pointers into a heap this guest is not
	// running on. kept=1 in a fresh heap is an allocation that DID happen --
	// reuse of the stale mirror would read 0.
	if get("C", "disp") != "1" {
		t.Fatalf("leg C adopted a heap it should have discarded (disp=%s, want "+
			"1): %v", get("C", "disp"), legs["C"])
	}
	if get("C", "kept") != "1" {
		t.Errorf("A REBUILT GUEST REUSED THE PREVIOUS BUILD'S BUFFER ADDRESS: "+
			"kept=%s in a fresh heap, where 1 is one allocation and 0 is none. "+
			"A pointer means nothing outside the heap laid out by the build that "+
			"made it, which is why the mirror sits under the same_build() gate.",
			get("C", "kept"))
	}

	// D AND E ARE THE SAVE THAT PREDATES THE MIRROR, which is every save written
	// by an older runtime -- and there are such saves, because the build stamp
	// is over the guest wasm and the API pin and nothing about FkLua itself, so
	// upgrading FkLua and repackaging leaves same_build() true over one. (Not
	// across the commit that folded the pin IN: that changed the construction,
	// so every stamp moved once. Every upgrade after it is the ordinary case
	// again, which is the one these two legs are about.) D pays for one twin. E
	// is the point: it does not, because D published a mirror from inside the
	// dispatch that allocated.
	if get("D", "kept") != "2" {
		t.Errorf("a save with no mirror cost %s allocations rather than the one "+
			"twin it has to (kept was %s): %v",
			get("D", "kept"), get("A", "kept"), legs["D"])
	}
	if get("E", "buf") != get("D", "buf") || get("E", "kept") != get("D", "kept") {
		t.Errorf("THE MIRROR DID NOT HEAL: a save written before it existed cost "+
			"a twin on load D (buf %s) and cost another on load E (buf %s, kept "+
			"%s -> %s). publish_buffers() is called at the allocation precisely "+
			"so that the LAST such load pays rather than every one.",
			get("D", "buf"), get("E", "buf"), get("D", "kept"), get("E", "kept"))
	}

	// F IS THE SIZE GUARD. An address says where a buffer starts and nothing
	// about how much room is behind it, and the size is not a constant of the
	// guest -- API.event_scratch comes out of the PACKAGED event table, so two
	// packages of one wasm against two API pins can disagree about it. The
	// stamp folds the API pin in since 2026-08-07, so that pair no longer
	// reaches here through same_build(); fk_migrate_adopt does, because handing
	// over another build's heap is the whole of what it is for. Reusing a buffer
	// allocated smaller than what write_struct is about to put in it overwrites
	// whatever the guest allocated next, silently. So a mirror recorded at
	// another size is refused and the allocation happens again: one buffer,
	// against a class of corruption with no error message.
	if get("F", "buf") == get("E", "buf") || get("F", "kept") == get("E", "kept") {
		t.Errorf("A BUFFER RECORDED AT ANOTHER SIZE WAS REUSED ANYWAY: leg E "+
			"dispatched through %s with kept=%s and leg F, whose mirror claims a "+
			"size this build does not ask for, through %s with kept=%s. It has "+
			"to allocate instead -- the recorded address cannot be known to have "+
			"room for what write_struct is about to write.",
			get("E", "buf"), get("E", "kept"), get("F", "buf"), get("F", "kept"))
	}
	if get("F", "evn") != get("E", "evn") {
		t.Errorf("leg F refused the stale mirror but did not republish a correct "+
			"one: evn is %s where the build asks for %s, so the next load would "+
			"refuse again and allocate again, forever",
			get("F", "evn"), get("E", "evn"))
	}
}

// Six sessions of one mod in one interpreter. `boot` is what the engine does on
// every load: control.lua is re-executed, which is the whole reason a Lua local
// cannot be trusted to survive one. Clearing every package.loaded entry a mod
// ships is what makes the next require genuinely fresh rather than a second
// reference to the first -- see clearloaded_test.go for the list and why it is
// written down once.
//
// The rebuilt-guest leg tampers with the SAVED stamp rather than packaging the
// guest a second time, and that is the same comparison read from the other side:
// same_build() is `storage.fk_build == P.build`, and a rebuild is exactly the
// case where those two strings differ.
var bufPinScript = expandClearLoaded(`package.path = %q
function log(s) end
defines = { events = { on_console_chat = %d } }
game = {}

local handlers = {}
script = {
  mod_name = "fk-bufpin",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local o = {}
  for k, val in pairs(v) do o[k] = deepcopy(val) end
  return o
end

local function boot(st)
  handlers = {}
  storage = st
  --@CLEAR_LOADED@
  require("control")
end

local function chat()
  handlers[%d]({ name = %d, tick = 7, player_index = 1, message = "hi" })
end

local function report(tag)
  local m, b = storage.fk_mem[1], storage.fk_bufs
  print(tag .. " kept=" .. tostring(m[%d]) .. " disp=" .. tostring(m[%d]) ..
        " buf=" .. tostring(m[%d]) ..
        " mirror=" .. tostring(b and b.ev and b.ev[1]) ..
        " evn=" .. tostring(b and b.evn))
end

-- A: a new map.
boot({})
handlers.on_init()
chat()
report("A")
local save = deepcopy(storage)

-- B: a load of it, same build.
boot(deepcopy(save))
handlers.on_load()
chat()
report("B")

-- C: a load whose save was written by another build. on_configuration_changed
-- is what Factorio raises for that, after on_load and before the first tick.
local c = deepcopy(save)
c.fk_build = "some-other-build"
boot(c)
handlers.on_load()
handlers.on_config()
chat()
report("C")

-- D: a load of a save written before the mirror existed. Same build, so the
-- heap is adopted; no mirror, so the cache starts empty and pays for one twin.
local d = deepcopy(save)
d.fk_bufs = nil
boot(d)
handlers.on_load()
chat()
report("D")

-- E: and a load of what D saved, which now carries one.
boot(deepcopy(d))
handlers.on_load()
chat()
report("E")

-- F: a mirror recorded at a size this build does not ask for, which is what
-- fk_migrate_adopt produces -- it hands over another build's heap on purpose,
-- and that build's packaged event table may have asked for a different
-- event_scratch. The address has to be refused: it says where the buffer starts
-- and nothing about how much room is behind it.
local f = deepcopy(storage)
f.fk_bufs.evn = f.fk_bufs.evn + 8
boot(f)
handlers.on_load()
chat()
report("F")
`)
