package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// AN EVENT'S STRING FIELDS ARE LIVE FOR THE WHOLE HANDLER, and until the arena
// bracket landed they were live until the handler's first host call.
//
// The payload is encoded into the string scratch region, and `dispatch` resets
// that region when it finds depth == 0. The encode used to run in the closure
// `subscribe` installs -- BEFORE dispatch was entered -- so the order was:
// write the event's strings at the bottom of the region, reset the region to
// zero, enter the handler, and let the handler's own first host call write its
// returned string over them. run_callback's header has said for a milestone
// that the reset is "correct for an event, whose payload it encodes AFTER
// raising the depth"; that described the shape this test pins and described
// nothing that existed.
//
// It went unseen because every generated decoder copies eagerly:
// ReadOnConsoleChat turns the field into a Go string on the first line of the
// handler, so the clobber landed on bytes nobody read again. A guest reading
// LAZILY from the pointer it was handed -- which is what fk_abi.lua's own
// re-entrancy comment says a handler does -- got somebody else's data, with no
// error anywhere. So this is asserted at the BYTE, through a guest that reads
// the pointer rather than the value.
//
// The same reordering is what makes the arena bracket possible at all: a mark
// taken in `dispatch` is taken after the encode, which is the one allocation it
// exists to reclaim. See TestAHostInitiatedDispatchKeepsNoHeap for that half.
func TestAnEventsStringFieldSurvivesTheHandlersOwnHostCall(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	// A no-argument method whose single return is a STRING, so the handler's
	// host call is the smallest thing that writes into the region.
	callID := full.MemberIndex()["LuaGameScript::get_map_exchange_string/0"]
	if callID == 0 {
		t.Skip("this API has no LuaGameScript::get_map_exchange_string")
	}
	var chatID int
	var msgAt int = -1
	for _, e := range events.Events {
		if e.Name != "on_console_chat" {
			continue
		}
		chatID = e.ID
		// The offset the HOST will write to, taken from the same placement the
		// generated table is built from rather than counted by hand.
		blk, err := LayoutStruct(e.Fields)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range blk.Fields {
			if f.Name == "message" {
				msgAt = f.Offset
			}
		}
	}
	if chatID == 0 || msgAt < 0 {
		t.Skip("this API has no on_console_chat with a message field")
	}

	// The guest records where the message landed and its first byte, makes one
	// host call that returns a string, and records the same byte again. Handle 2
	// is `game`; see M.GLOBAL_NAMES.
	//
	// A FIXED scratch region rather than one carved out of the bump allocator,
	// because the assertion is about an ADDRESS: the message has to be provably
	// at the bottom of the region for the clobber to be the first thing the host
	// call overwrites.
	const (
		scratchBase = 16384
		scratchSize = 4096
		retp        = 3072
	)
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(import "fk" "subscribe" (func $sub (param i32) (result i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 8192))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_scratch_base") (result i32) (i32.const %d))
		(func (export "fk_scratch_size") (result i32) (i32.const %d))
		(func (export "fk_on_init") (drop (call $sub (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32) (local $mp i32)
			(local.set $mp (i32.load (i32.add (local.get $ptr) (i32.const %d))))
			(i32.store (i32.const 2048) (local.get $mp))
			(i32.store (i32.const 2052) (i32.load8_u (local.get $mp)))
			(drop (call $call (i32.const 2) (i32.const %d)
				(i32.const 0) (i32.const %d)))
			(i32.store (i32.const 2056) (i32.load8_u (local.get $mp)))
			(i32.store (i32.const 2060) (i32.load (i32.const %d)))))`,
		scratchBase, scratchSize, chatID, msgAt, callID, retp, retp+4)

	im := buildIR(t, wat)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	// Table mode, so storage.fk_mem IS the guest's word table and the test can
	// read what the handler recorded without the guest needing a way to say it.
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-eventscratch", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_on_event", "fk_alloc", "fk_alloc_static",
			"fk_free", "fk_scratch_base", "fk_scratch_size"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_console_chat = %d } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-eventscratch",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
game = { get_map_exchange_string = function() return "inner" end }

require("control")
handlers.on_init()
handlers[%d]({ name = %d, tick = 7, player_index = 1, message = "outer-message" })
-- 2048, 2052, 2056 and 2060 as word indices into the aliased memory.
print("at " .. tostring(storage.fk_mem[1][513]))
print("before " .. tostring(storage.fk_mem[1][514]))
print("after " .. tostring(storage.fk_mem[1][515]))
print("returned " .. tostring(storage.fk_mem[1][516]))
`, filepath.Join(dir, "?.lua"), chatID, chatID, chatID))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// `at` proves the message really is at the bottom of the region, so the
	// host call's own returned string is written over it under the old ordering
	// and after it under the new one -- without that line, `after` could pass
	// because the two never overlapped.
	//
	// `returned` is 5, the length of "inner": the host call has to have written
	// a string for this to be a test of anything.
	//
	// 111 is 'o', the first byte of "outer-message". Under the old ordering it
	// reads 105, which is 'i'.
	want := fmt.Sprintf("at %d\nbefore 111\nafter 111\nreturned 5", scratchBase)
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the handler's own host call wrote its "+
			"returned string over the event field the handler was still holding "+
			"a pointer to)", got, want)
	}
}
