package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// A CUSTOM INPUT is Factorio's keybind, and it is the one event genre a guest
// could not receive at all.
//
// `LuaEventType` is a union of four arms and fk.subscribe reached exactly one of
// them, the described `defines.events` set, through a dense index. A custom
// input is addressed by the PROTOTYPE'S OWN NAME -- script.on_event("my-input",
// f) -- and has no defines.events entry: measured on 2.0.77, the table holds 233
// keys and CustomInputEvent is not one of them.
//
// The trap was that the description carries CustomInputEvent as an ordinary
// event, so the generator emitted a complete binding -- the id constant, the
// payload struct, the reader, three field masks -- and a guest that found the
// right constant compiled, passed the pruning scan, and at load was told that
// this Factorio has no such event. A falsehood, about the author's one mistake.
//
// Nine of the thirteen mods the temptations survey audited subscribe to a custom
// input by name; one of them has no other entry point at all.

// customInputWat builds a guest whose fk_on_init subscribes the way `body`
// says, and whose fk_on_event logs the payload's `input_name` field.
//
// LOGGING input_name RATHER THAN A MARKER, and that is the point of the fixture:
// it proves the whole chain in one line -- the NAME reached script.on_event as a
// registration key, the dispatch came back into the guest, and the payload was
// encoded with the CustomInputEvent descriptor the ID selected. A marker would
// have proved only the middle one.
func customInputGuest(t *testing.T, name, body string) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var ciID, inputNameOff int
	for _, e := range events.Events {
		if e.Name != "CustomInputEvent" {
			continue
		}
		ciID = e.ID
		blk, err := LayoutStruct(e.Fields)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range blk.Fields {
			if f.Name == "input_name" {
				inputNameOff = f.Offset
			}
		}
	}
	if ciID == 0 {
		t.Fatal("the description has no CustomInputEvent")
	}

	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32 i32 i32) (result i32)))
		(import "env" "fk_log" (func $log (param i32 i32)))
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
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)
			(call $log
				(i32.load (i32.add (local.get $ptr) (i32.const %d)))
				(i32.load (i32.add (local.get $ptr) (i32.const %d)))))
		(data (i32.const 0x200) "fklua-test-inputfklua-no-such-inputok bad "))`,
		fmt.Sprintf(body, ciID), inputNameOff, inputNameOff+4)

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

// The stub is the ENGINE'S contract for a name-addressed event, and the refusal
// is the half that had to be measured rather than assumed.
//
// Probed on Factorio 2.0.77 with a bare Lua mod: script.on_event with a KNOWN
// custom-input prototype name is accepted; with an unknown one it RAISES
// `Unknown event name: <name>`; defines.events.CustomInputEvent is nil while the
// table holds 233 other keys; and script.raise_event refuses a custom input
// outright -- "ciprobe-real-input (ID 218) (218) can't be raised through
// script." -- which is why the in-game gate cannot press a key and this fixture
// fires the handler itself.
const customInputStub = `
package.path = %q
local logged = {}
function log(s) logged[#logged + 1] = tostring(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
local known = { ["fklua-test-input"] = true }
script = {
  mod_name = "t",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f, flt)
    if type(ev) == "string" and not known[ev] then
      error("Unknown event name: " .. ev, 0)
    end
    handlers[ev] = f
  end,
  set_event_filter = function() end,
}

require("control")
handlers.on_init()

print("registered " .. tostring(handlers["fklua-test-input"] ~= nil))
print("stray " .. tostring(handlers["fklua-no-such-input"] ~= nil))
if handlers["fklua-test-input"] then
  handlers["fklua-test-input"]({
    name = 1, tick = 7, player_index = 1, input_name = "fklua-test-input",
    cursor_position = { x = 0, y = 0 },
    cursor_display_location = { x = 0, y = 0 },
    in_gui = false,
  })
end
for i = 1, #logged do print("log " .. logged[i]) end
`

// A NAMED SUBSCRIPTION REGISTERS, DISPATCHES, AND CARRIES ITS PAYLOAD.
//
// The id supplies the LAYOUT and the name supplies the KEY, and neither is the
// other's business -- which is what makes this a widening of fk.subscribe rather
// than a third fk.register kind. A register descriptor is a tier-2 blob, and the
// scan that prunes the packaged event table reads an i32 constant at operand 0,
// so the register shape would have pruned the payload descriptor out of the very
// mod that needs it.
func TestASubscriptionByNameRegistersAndDispatches(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := customInputGuest(t, "fk-cin", `
		(drop (call $sub (i32.const %d) (i32.const 0) (i32.const 0)
			(i32.const 0x200) (i32.const 16)))`)

	out, err := h.RunString(fmt.Sprintf(customInputStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(out)
	for _, want := range []string{
		"registered true",
		// The payload's own input_name, read out of the encoded struct at the
		// offset the CustomInputEvent descriptor put it at.
		"log fklua-test-input",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A NAME THIS GAME DOES NOT HAVE IS A STATUS AND A LOG LINE, never an unwind.
//
// The engine raises `Unknown event name: <name>`, and taking a whole mod down at
// load for a typo in a keybind is worse than running without the keybind and
// saying so -- the same call the filter path already makes one branch over. What
// makes it more than a pcall is the ROLLBACK: `registered` and `filters` are
// already set when script.on_event is called, so leaving them behind would make
// a later subscription to the same name append to a list Factorio never
// registered a dispatcher for, and return SUCCESS while doing it.
func TestANameThisGameDoesNotHaveIsRefusedAndRolledBack(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// Subscribe to the bad name TWICE. The second call is the rollback probe: if
	// the first left its dispatcher list behind, the second takes the "already
	// registered" arm, appends and reports success, and the log carries one
	// refusal instead of two.
	dir := customInputGuest(t, "fk-cin-bad", `
		(drop (call $sub (i32.const %[1]d) (i32.const 0) (i32.const 0)
			(i32.const 0x210) (i32.const 19)))
		(drop (call $sub (i32.const %[1]d) (i32.const 0) (i32.const 0)
			(i32.const 0x210) (i32.const 19)))`)

	out, err := h.RunString(fmt.Sprintf(customInputStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(out)
	if !strings.Contains(got, "stray false") {
		t.Errorf("a refused name left a registration behind:\n%s", got)
	}
	if n := strings.Count(got, "Unknown event name: fklua-no-such-input"); n != 2 {
		t.Errorf("the engine's refusal was logged %d times, want 2 (once per "+
			"attempt -- one means the first attempt left its dispatcher behind "+
			"and the second silently succeeded):\n%s", n, got)
	}
	if !strings.Contains(got, "script.on_event refused the event name") {
		t.Errorf("the refusal is not diagnosed by fklua at all:\n%s", got)
	}
}

// ...AND THE UNNAMED FORM SAYS SOMETHING TRUE.
//
// This is the sentence the survey called out. A guest that subscribes to
// CustomInputEvent by its id alone is doing the one thing that cannot work, and
// the old message said "this Factorio has no event CustomInputEvent" -- which
// misdiagnoses the author's one mistake, since the event exists and is simply
// not addressed that way. The message names both causes and the remedy now.
func TestTheUnnamedCustomInputSubscriptionIsDiagnosedTruthfully(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := customInputGuest(t, "fk-cin-unnamed", `
		(drop (call $sub (i32.const %d) (i32.const 0) (i32.const 0)
			(i32.const 0) (i32.const 0)))`)

	out, err := h.RunString(fmt.Sprintf(customInputStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(out)
	if strings.Contains(got, "this Factorio has no event CustomInputEvent") {
		t.Errorf("the old, false sentence is still logged:\n%s", got)
	}
	for _, want := range []string{
		"could not resolve defines.events.CustomInputEvent",
		"addressed by NAME",
		"SubscribeNamed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// AN OLD GUEST'S THREE-ARGUMENT CALL IS UNCHANGED, which is the whole
// compatibility argument for widening the import rather than adding one.
//
// A wasm import declared with fewer parameters is called with fewer arguments,
// and Lua hands the rest a nil -- which `subscribe` reads exactly as 0, the same
// reading the mask parameter got when it was added. So a mod already in the
// field keeps registering by defines.events and never enters the named path.
func TestAThreeArgumentSubscribeStillRegistersByDefine(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := filterGuest(t, "fk-old-sub",
		"(drop (call $sub (i32.const %d) (i32.const 0)))")

	out, err := h.RunString(fmt.Sprintf(filterStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out); got != "filters none" {
		t.Errorf("got %q, want %q", got, "filters none")
	}
}
