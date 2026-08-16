package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	luart "github.com/Techrocket9/fklua/runtime"
)

// The collector's PACING PROTOCOL, driven through the real control.lua against a
// stand-in `script` and `storage`.
//
// The collector itself is guest Go and is gated in internal/guest; what is
// tested here is the half that lives in runtime/lua/fk_mod.lua and ships
// verbatim into every mod -- when the one-shot on_tick is registered, when it is
// torn down, when the write barrier is armed and disarmed, what the step is
// handed, and what happens to a collection that a save lands in the middle of.
//
// A wat stand-in rather than a TinyGo build, for the reason the audit's own
// lesson gives: "persistence bugs live at mode and lifecycle seams, and a test
// that hands live values between instances cannot see them. Replay the
// control.lua protocol through a stand-in `storage` instead, per mode." A
// stand-in collector also lets the phase sequence be CHOSEN, so the mark/sweep
// boundary lands where the assertion needs it rather than where a real heap
// happens to put it.

// gcPaceWAT is a collector-shaped guest whose phase sequence is fixed:
//
//	step 1, 2, 3 -> phase 1 (marking)
//	step 4, 5    -> phase 2 (sweeping)
//	step 6       -> phase 0 (done)
//
// It records what every step was handed, which is what makes "the first step
// after a load is told the page record was lost" an assertion rather than a
// hope.
//
// Word indices in table mode are byte/4 + 1 WITHIN A SHARD, and fk_mem is the
// shard vector, so byte 4096 is storage.fk_mem[1][1025]. Everything this file
// reads is in the first shard by construction.
const gcPaceWAT = `(module
	(import "fk" "gc" (func $gc (result i32)))
	(memory 1)
	(func (export "fk_gc_dirty_base") (result i32) (i32.const 8192))
	(func (export "fk_gc_dirty_cap") (result i32) (i32.const 64))
	;; 4096 steps taken, 4100 last ndirty, 4104 times DirtyAll was seen,
	;; 4108 times a nonzero real count was seen.
	(func (export "fk_gc_step") (param $n i32) (result i32) (local $s i32)
		(local.set $s (i32.add (i32.load (i32.const 4096)) (i32.const 1)))
		(i32.store (i32.const 4096) (local.get $s))
		(i32.store (i32.const 4100) (local.get $n))
		(if (i32.eq (local.get $n) (i32.const -1))
			(then (i32.store (i32.const 4104)
				(i32.add (i32.load (i32.const 4104)) (i32.const 1))))
			(else (if (i32.gt_u (local.get $n) (i32.const 0))
				(then (i32.store (i32.const 4108)
					(i32.add (i32.load (i32.const 4108)) (i32.const 1)))))))
		(if (i32.le_u (local.get $s) (i32.const 3)) (then (return (i32.const 1))))
		(if (i32.le_u (local.get $s) (i32.const 5)) (then (return (i32.const 2))))
		(i32.const 0))
	;; A guest asking for a collection, the way fkgc.CollectIfNeeded does.
	;;
	;; There is deliberately NO fk_on_tick here. A guest that exports one gets a
	;; permanent on_tick registration, which would make "is the one-shot still
	;; registered?" unanswerable -- the dispatcher would be there either way.
	;; The stores above are what dirties a page for the drain to find.
	(func (export "fk_on_init") (drop (call $gc))))`

func gcPacePackage(t *testing.T, name string, persist luagen.PersistMode) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)
	im := buildIR(t, gcPaceWAT)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	// --gc=collected is what emits the persist.gc surface control.lua drives.
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: persist, GC: luagen.GCCollected})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: name, Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_gc_step",
			"fk_gc_dirty_base", "fk_gc_dirty_cap"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "?.lua")
}

// gcPacePrelude is the stand-in Factorio: a `script` that records what was
// registered, a `storage` that survives a reload, and a word reader that means
// the same thing in BOTH persistence modes.
//
// That last one is the mode seam the audit warns about. In table mode
// storage.fk_mem IS the live word table. In packed mode it is not in `storage`
// at all -- what is there is an array of string.pack pages, written by the flush
// at the end of each dispatch -- so a test that read storage.fk_mem there would
// read a nil and pass for the wrong reason.
var gcPacePrelude = "\n" + expandClearLoaded(`
function log(s) end
defines = { events = { on_tick = 1, on_player_created = 2 } }
storage = {}
handlers = {}
script = {
  mod_name = "MOD",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
-- The SAVE/LOAD boundary: a fresh chunk, a fresh dispatcher table, and the same
-- storage. Nothing but storage crosses, which is what Factorio actually does.
function reload()
  handlers = {}
  --@CLEAR_LOADED@
  require("control")
end
function RD(byte)
  if storage.fk_mem then
    local w = byte / 4
    local o = w % 524288
    return storage.fk_mem[(w - o) / 524288 + 1][o + 1]
  end
  local p = (byte - byte % 4096) / 4096
  local s = storage.fk_pages and storage.fk_pages[p + 1]
  if not s then return nil end
  return (string.unpack("<I4", s, (byte % 4096) + 1))
end
function REG() return handlers[1] ~= nil end
`)

func gcPaceReaders(name string) string {
	return strings.Replace(gcPacePrelude, "MOD", name, 1)
}

// A COLLECTION IS ONE REGISTRATION, HELD ONLY WHILE IT RUNS.
//
// This is the stage-A property the whole design was given a GO on: an idle guest
// registers nothing and pays nothing, so the collector costs a guest with a
// small heap exactly what fk.defer costs one that never defers. The assertion is
// the same one TestManyEventsInOneTickFlushOnce makes for the deferred flush --
// that nothing is left registered on on_tick when the work is done.
//
// It also pins the phase-to-barrier mapping, which is where half the design's
// value is: MEMDIRTY is armed for MARKING ONLY. A sweep needs no barrier,
// because the mark bitmap is fixed once marking terminates, so arming through
// one would be paying agents/gc.md's measured 7-13% store cost for nothing.
func TestACollectionIsOneRegistrationHeldOnlyWhileItRuns(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	path := gcPacePackage(t, "fk-gc-pace", luagen.PersistTable)

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
%s
require("control")
print("before " .. tostring(REG()) .. " " .. tostring(storage.fk_gc))
handlers.on_init()
print("armed " .. tostring(REG()) .. " " .. tostring(storage.fk_gc))
for tick = 1, 8 do
  if handlers[1] then handlers[1]({ tick = tick }) end
  print(string.format("tick %%d steps=%%d registered=%%s flag=%%s",
    tick, RD(4096), tostring(REG()), tostring(storage.fk_gc)))
end
`, path, gcPaceReaders("fk-gc-pace")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := strings.Join([]string{
		// Nothing before the guest asks. This line IS the zero-idle-cost claim.
		"before false nil",
		"armed true true",
		// One step per tick, re-registering itself while the collection runs.
		"tick 1 steps=1 registered=true flag=true",
		"tick 2 steps=2 registered=true flag=true",
		"tick 3 steps=3 registered=true flag=true",
		"tick 4 steps=4 registered=true flag=true",
		"tick 5 steps=5 registered=true flag=true",
		// Step 6 reports idle: teardown, and the storage flag goes with it.
		"tick 6 steps=6 registered=false flag=nil",
		// ...and then nothing happens at all, forever.
		"tick 7 steps=6 registered=false flag=nil",
		"tick 8 steps=6 registered=false flag=nil",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a collection registers ONE one-shot "+
			"on_tick, re-arms it while it is still collecting, and tears it down "+
			"with the storage flag when the step reports idle)", got, want)
	}
}

// A SAVE CAN LAND BETWEEN TWO STEPS OF ONE COLLECTION, and both halves of what
// that costs are here.
//
// The collector's own state -- phase, mark bitmap, gray stack, sweep cursor,
// free runs -- is in linear memory and comes back with it, which is the property
// agents/gc.md called this design's cheapest and which stage B proved for the
// state BETWEEN collections. What does not come back is two things, and they
// fail in opposite directions:
//
//   - The on_tick registration. Factorio does not save one. Without the
//     `storage.fk_gc` flag and the re-arm below, a guest saved mid-collection
//     comes back with a collection in progress and nothing to step it: the write
//     barrier stays off forever and the heap never gets swept.
//   - The DIRTY PAGE SET. It is a Lua table inside the generated chunk and no
//     `storage` entry mirrors it, so every write between the last step and the
//     save is unrecorded -- which is a live object swept, not stale memory. The
//     first step after a load is therefore handed 4294967295, fkgc.DirtyAll,
//     and re-scans everything it had marked.
//
// Taken MID-MARK here and mid-sweep in the next test, because the two resume
// through different code: a mark resumes with the barrier armed and a re-scan
// owed, a sweep resumes with the barrier off and a cursor.
func TestACollectionSurvivesASaveTakenMidMark(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, mode := range []luagen.PersistMode{luagen.PersistTable, luagen.PersistPacked} {
		t.Run(mode.String(), func(t *testing.T) {
			path := gcPacePackage(t, "fk-gc-save", mode)
			// Mid-mark: two steps taken (the stand-in marks for three), then the
			// save. Reading the step counter back has to go through the same
			// door a guest would -- in packed mode the live word table is not
			// in `storage` at all, so the export is the only honest reader.
			out, err := h.RunString(fmt.Sprintf(`
package.path = %q
%s
require("control")
handlers.on_init()
handlers[1]({ tick = 1 })
handlers[1]({ tick = 2 })
print("presave steps " .. tostring(RD(4096)))
-- THE SAVE. Registrations do not survive it; storage does.
reload()
handlers.on_load()
print("rearmed " .. tostring(REG()))
for tick = 3, 8 do
  if handlers[1] then handlers[1]({ tick = tick }) end
end
print("steps " .. tostring(RD(4096)))
print("dirtyall " .. tostring(RD(4104)))
print("registered " .. tostring(REG()))
print("flag " .. tostring(storage.fk_gc))
`, path, gcPaceReaders("fk-gc-save")))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			got := strings.TrimSpace(out)
			// Six steps in total across the save: the collection RESUMED rather
			// than restarted, which is the whole claim.
			want := strings.Join([]string{
				"presave steps 2",
				"rearmed true",
				"steps 6",
				"dirtyall 1",
				"registered false",
				"flag nil",
			}, "\n")
			if got != want {
				t.Errorf("got:\n%s\nwant:\n%s\n(a collection interrupted by a save "+
					"resumes from where it was, and the first step after the load is "+
					"told the dirty page record was lost)", got, want)
			}
		})
	}
}

// The same, MID-SWEEP: the save lands after marking has terminated.
//
// A sweep resumes through different code and with a different obligation. There
// is no barrier to re-arm -- the mark bitmap is fixed, so a store cannot change
// a decision the sweep makes -- and the state that has to come back is the sweep
// cursor and the per-class free runs, all of which are linear memory. What is
// asserted here is that the collection still FINISHES: a resumed sweep that
// never reported idle would leave the one-shot registered forever, which is the
// permanent per-tick cost the whole one-shot design exists to avoid.
func TestACollectionSurvivesASaveTakenMidSweep(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, mode := range []luagen.PersistMode{luagen.PersistTable, luagen.PersistPacked} {
		t.Run(mode.String(), func(t *testing.T) {
			path := gcPacePackage(t, "fk-gc-save2", mode)
			out, err := h.RunString(fmt.Sprintf(`
package.path = %q
%s
require("control")
handlers.on_init()
-- Four steps: three marking, one sweeping. The save lands mid-sweep.
for tick = 1, 4 do handlers[1]({ tick = tick }) end
reload()
handlers.on_load()
print("rearmed " .. tostring(REG()))
for tick = 5, 8 do
  if handlers[1] then handlers[1]({ tick = tick }) end
end
print("steps " .. tostring(RD(4096)))
print("registered " .. tostring(REG()))
print("flag " .. tostring(storage.fk_gc))
`, path, gcPaceReaders("fk-gc-save2")))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			want := strings.Join([]string{
				"rearmed true",
				"steps 6",
				"registered false",
				"flag nil",
			}, "\n")
			if got := strings.TrimSpace(out); got != want {
				t.Errorf("got:\n%s\nwant:\n%s\n(a sweep interrupted by a save has to "+
					"resume from its cursor and still reach idle, or the one-shot "+
					"on_tick is registered forever)", got, want)
			}
		})
	}
}

// THE SAFE-POINT PRECONDITION, written down and enforced.
//
// Everything the incremental marker claims rests on one fact about this target,
// and stage B found the fact before stage C needed it:
//
//	A COLLECTION STEP RUNS ONLY AT AN OUTERMOST DISPATCH BOUNDARY. There, and
//	only there, the wasm operand stack and the shadow stack are both empty --
//	verified two independent ways in agents/gc.md section 1 -- so every live
//	reference the guest holds is in the heap or in [__global_base,
//	__heap_base), and there is no third place.
//
// That is what makes re-scanning the roots plus the dirtied pages SUFFICIENT at
// mark termination. At a nested dispatch it is false: the outer handler's live
// values are on the shadow stack, the conservative scan does see those -- but
// the deleted-reference argument does not survive, because a reference the
// mutator moved out of a black object and is holding in a frame the marker
// already walked past would be lost.
//
// Two halves, because half of it cannot be triggered today and saying so is
// better than pretending.
//
// The POSITIVE half is dynamic: the real control.lua drives real steps, and the
// depth guard inside gc_step does not fire. on_tick is raised by the engine's
// own loop and never from inside another event, so a step reached from the
// one-shot is outermost by construction -- and a change that made it reachable
// from anywhere else would trip the guard on the first collection rather than
// producing a swept live object months later.
//
// The NEGATIVE half is a text property. There is no way to raise on_tick from
// inside a dispatch through this ABI, so the guard cannot be provoked without
// building the very re-entrancy it exists to forbid. What can be asserted is
// that the guard is THERE, in the file that ships verbatim into every mod, and
// that it names the depth counter rather than something that happens to be zero.
func TestACollectionStepRunsOnlyAtAnOutermostDispatch(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	path := gcPacePackage(t, "fk-gc-safepoint", luagen.PersistTable)
	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
%s
require("control")
handlers.on_init()
for tick = 1, 6 do
  if handlers[1] then handlers[1]({ tick = tick }) end
end
-- steps: the guard did not fire, and every step ran.
-- realdirty: the step was handed a REAL page count at least once, which is the
-- barrier's data actually crossing the boundary rather than a zero every time.
print("steps " .. tostring(RD(4096)))
print("realdirty " .. tostring(RD(4108) ~= nil and RD(4108) > 0))
`, path, gcPaceReaders("fk-gc-safepoint")))
	if err != nil {
		t.Fatalf("the depth guard fired, or a step raised: %v", err)
	}
	want := "steps 6\nrealdirty true"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(six steps at an outermost dispatch, and "+
			"at least one of them handed a real dirty-page count)", got, want)
	}

	// The negative half.
	src := luart.ModGlue()
	i := strings.Index(src, "gc_step = function()")
	if i < 0 {
		t.Fatal("runtime/lua/fk_mod.lua has no gc_step; the pacing handler was " +
			"renamed and this test no longer looks at what it says it does")
	}
	j := strings.Index(src[i:], "\nend\n")
	if j < 0 {
		t.Fatal("could not find the end of gc_step")
	}
	body := src[i : i+j]
	if !strings.Contains(body, "dispatch_depth()") {
		t.Errorf("gc_step does not test dispatch_depth(). The whole marking "+
			"argument is that a step runs where the shadow stack is empty; "+
			"without this check a future re-entrancy would show up as a live "+
			"object swept, months later, in a lockstep game:\n%s", body)
	}
	if !strings.Contains(body, "error(") {
		t.Errorf("gc_step tests the depth but does not RAISE on it. A logged "+
			"warning for a broken soundness precondition is a warning nobody "+
			"reads:\n%s", body)
	}
}
