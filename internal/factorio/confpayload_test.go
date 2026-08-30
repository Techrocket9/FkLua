package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// THE HOOK'S PAYLOAD, which it discarded for two milestones.
//
// script.on_configuration_changed hands its handler a ConfigurationChangedData:
// old_version, new_version, mod_changes, mod_startup_settings_changed,
// migration_applied and migrations. The FkLua hook dispatched with no arguments
// at all, so a guest could hear that SOMETHING moved and never what -- which
// neighbour appeared, disappeared or changed version, and from what. Four of the
// thirteen mods the temptations survey audited branch on mod_changes directly,
// and every consumer of the ecosystem's standard migration module does so
// transitively.
//
// Nothing in the API references the concept, so no generator had ever emitted
// it; the encode machinery was there all along.

// confFieldOffset is where a named field of the hook payload sits, out of the
// layout the packager will ship rather than out of a number written here.
func confFieldOffset(t *testing.T, ev EventReport, name string) int {
	t.Helper()
	blk, err := LayoutStruct(ev.ConfChanged)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range blk.Fields {
		if f.Name == name {
			return f.Offset
		}
	}
	t.Fatalf("%s has no field %q", ConfChangedConcept, name)
	return 0
}

// A GUEST THAT TAKES THE POINTER READS THE PAYLOAD.
//
// The guest logs the first mod_changes key and the two booleans, which is the
// whole chain in three lines: the host encoded a dictionary of structs into the
// per-level event buffer with H.write_struct, the pointer reached the export,
// and the bytes at the offsets the packaged layout names are what the engine
// passed.
func TestTheConfigurationChangedPayloadReachesTheGuest(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	ev := GenerateEvents(a)
	if ev.ConfChanged == nil {
		t.Fatalf("%s did not generate: %s", ConfChangedConcept, ev.ConfChangedSkip)
	}
	changes := confFieldOffset(t, ev, "mod_changes")
	startup := confFieldOffset(t, ev, "mod_startup_settings_changed")
	migrated := confFieldOffset(t, ev, "migration_applied")

	// A dictionary field is (ptr, count); an entry is a string KEY followed by
	// the ModChangeData value, so the key's own (ptr, len) is at the entry's
	// offset 0 and 4.
	wat := fmt.Sprintf(`(module
		(import "env" "fk_log" (func $log (param i32 i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 8192))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_configuration_changed") (param $p i32) (local $e i32)
			;; "changes=" plus the count, as a digit -- one byte, which is all a
			;; fixture needs and is what keeps this readable as wat.
			(i32.store8 (i32.const 0x108)
				(i32.add (i32.const 48)
					(i32.load (i32.add (local.get $p) (i32.const %[2]d)))))
			(call $log (i32.const 0x100) (i32.const 9))
			;; the two booleans, as '0'/'1' after their own labels
			(i32.store8 (i32.const 0x118)
				(i32.add (i32.const 48)
					(i32.load8_u (i32.add (local.get $p) (i32.const %[3]d)))))
			(call $log (i32.const 0x110) (i32.const 9))
			(i32.store8 (i32.const 0x128)
				(i32.add (i32.const 48)
					(i32.load8_u (i32.add (local.get $p) (i32.const %[4]d)))))
			(call $log (i32.const 0x120) (i32.const 9))
			;; the first entry's KEY, which is the mod's own name
			(local.set $e (i32.load (i32.add (local.get $p) (i32.const %[1]d))))
			(call $log (i32.load (local.get $e))
				(i32.load (i32.add (local.get $e) (i32.const 4)))))
		(data (i32.const 0x100) "changes=X")
		(data (i32.const 0x110) "startup=X")
		(data (i32.const 0x120) "migappl=X"))`,
		changes, changes+4, startup, migrated)

	dir := packConfPayload(t, wat, []string{"fk_on_configuration_changed",
		"fk_alloc", "fk_alloc_static", "fk_free"})

	out, err := h.RunString(fmt.Sprintf(confPayloadStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	got := strings.TrimSpace(out)
	for _, want := range []string{
		"log changes=1",
		"log startup=1",
		"log migappl=0",
		// mod_changes is keyed by MOD NAME, which is the whole point of the field.
		"log some-neighbour",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// ...AND A GUEST THAT TAKES NO PARAMETER IS UNCHANGED.
//
// This is the compatibility argument for putting the payload in the EXISTING
// hook rather than in a sibling export, and it is a property of the emitted Lua
// rather than of a promise: a wasm export of no parameters becomes a Lua
// function of no parameters, and Lua DISCARDS extra arguments. So a guest
// already in the field is called with a pointer it never looks at and behaves
// exactly as it did.
//
// Asserted through the real control.lua and the real dispatch path rather than
// argued, because the argument is about a language rule and the claim is about
// this runtime's plumbing.
func TestANoArgumentConfigurationChangedGuestStillWorks(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	wat := `(module
		(import "env" "fk_log" (func $log (param i32 i32)))
		(memory 1)
		(global $heap (mut i32) (i32.const 8192))
		(func $alloc (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32)
			(call $alloc (local.get $n)))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_configuration_changed")
			(call $log (i32.const 0x100) (i32.const 7)))
		(data (i32.const 0x100) "noargok"))`

	dir := packConfPayload(t, wat, []string{"fk_on_configuration_changed",
		"fk_alloc", "fk_alloc_static", "fk_free"})

	out, err := h.RunString(fmt.Sprintf(confPayloadStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "log noargok") {
		t.Errorf("a no-argument hook was not entered:\n%s", out)
	}
}

// THE LAYOUT IS PRUNED BY THE EXPORT, and that is the one pruning key here that
// is not a constant-id scan.
//
// There is no id to find: Factorio raises the hook and the guest never asks for
// it, so what says whether the layout can ever be used is whether the guest
// exports the hook at all. A guest that does not can never be handed one, and
// packaging the layout for it would be bytes in every save and every multiplayer
// join for a dispatch that cannot happen.
func TestTheHookPayloadLayoutIsPrunedByTheExport(t *testing.T) {
	a := loadTestAPI(t)
	ev := GenerateEvents(a)
	full, err := ev.luaEvents()
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := ev.WithoutConfChanged().luaEvents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "confchanged = {size=") {
		t.Error("the packaged event table has no confchanged layout")
	}
	if strings.Contains(pruned, "confchanged") {
		t.Error("WithoutConfChanged left the layout in the table")
	}

	// AND event_scratch DOES NOT MOVE WHEN IT IS PRUNED, which is what keeps a
	// mod that exports no hook packaging exactly what it packaged before. The
	// buffer is sized to the largest thing encoded into it, and the payload is
	// encoded into it -- so folding its size in unconditionally would move
	// event_scratch for every mod in existence.
	scratchOf := func(src string) string {
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "event_scratch") {
				return strings.TrimSpace(line)
			}
		}
		return ""
	}
	if scratchOf(full) != scratchOf(pruned) {
		t.Errorf("event_scratch moved with the hook payload: %q against %q "+
			"(it happens to fit at this pin, and a description where it did not "+
			"would legitimately move the FULL one only)",
			scratchOf(full), scratchOf(pruned))
	}
}

// packConfPayload builds a package whose API table carries the hook layout.
func packConfPayload(t *testing.T, wat string, exports []string) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)
	im := buildIR(t, wat)
	// The event table is pruned to nothing -- this guest subscribes to no event
	// -- so what is left in it is exactly the hook layout, which is the shape a
	// real guest that only wants the payload would package.
	apiSrc, err := full.Only(map[int]bool{}).LuaSourceWith(a, events.Only(map[int]bool{}))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-conf", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk, Exports: exports, APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The stub is the ENGINE's own call shape: on_configuration_changed's handler
// takes one table, and this is the table Factorio documents.
const confPayloadStub = `
package.path = %q
local logged = {}
function log(s) logged[#logged + 1] = tostring(s) end
defines = { events = { on_tick = 1 } }
game = {}
storage = {}
local handlers = {}
script = {
  mod_name = "fk-conf",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

require("control")
handlers.on_init()
handlers.on_config({
  mod_startup_settings_changed = true,
  migration_applied = false,
  mod_changes = { ["some-neighbour"] = { old_version = "1.2.3" } },
  migrations = {},
})
for i = 1, #logged do print("log " .. logged[i]) end
`

// BOTH BACKENDS EMIT THE STRUCT AND THE READER, which is the parity half.
//
// A generated shape added to one backend and not the other is this repo's own
// four-milestone defect (the Rust generator's missing event payload structs,
// found by three separate ports), and a census row cannot see it here: the hook
// payload is not a member and not an event, so the counts that keep the two
// backends in step are blind to it. `hook_payload_fields` is the row it does
// move, and it is host-side; this is the guest-side half.
func TestBothBackendsEmitTheHookPayload(t *testing.T) {
	_, _, g, rb := genBoth(t)

	for _, b := range []struct {
		lang, src string
		want      []string
	}{
		{"go", g.Source, []string{
			"type ConfigurationChangedData struct {",
			"ModChanges                []EntryStringModChangeData",
			"func ReadConfigurationChangedData(p uint32) ConfigurationChangedData {",
		}},
		{"rust", rb.Source, []string{
			"pub struct ConfigurationChangedData {",
			"pub fn read_configuration_changed_data(p: u32) -> ConfigurationChangedData {",
		}},
	} {
		for _, w := range b.want {
			if !strings.Contains(b.src, w) {
				t.Errorf("%s: the generated bindings have no %q", b.lang, w)
			}
		}
	}
}
