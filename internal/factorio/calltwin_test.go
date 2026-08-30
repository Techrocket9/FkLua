package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// THE METHOD HALF OF "NOTHING IN THE API RETURNS A LuaCustomTable".
//
// Kind 7 gave an ATTRIBUTE whose type is a LuaCustomTable a second,
// handle-returning member, so `force.TechnologiesRaw():Get(name)` costs two host
// calls where the materialising read cost 14,544 bytes of guest heap. Eleven
// METHODS return one and got nothing: the ten filtered prototype getters and
// LuaSettings::get_player_settings, which are what a prototype browser is made
// of and which each materialise their whole result per call.
//
// This is the population, asserted as an EQUALITY at every committed description
// rather than at the pin -- the same discipline the unfillable handlers took --
// so a description that grows a twelfth is a number that moves rather than a
// shape nobody re-read.
func TestEveryMethodReturningACustomTableGetsAHandleTwin(t *testing.T) {
	for _, v := range committedVersions(t) {
		a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		r := GenerateMembers(a)

		// What the DESCRIPTION says: a method whose single return is declared
		// LuaCustomTable. Derived here rather than listed, so the expectation and
		// the generator are not two copies of one list.
		want := map[string]bool{}
		for _, c := range a.Classes {
			for _, m := range c.Methods {
				if len(m.ReturnValues) == 1 &&
					isCustomTable(m.ReturnValues[0].Type) {
					want[c.Name+"::"+m.Name] = true
				}
			}
		}
		got := map[string]bool{}
		for _, m := range r.Members {
			if m.Kind == MemberCallHandle {
				got[m.Class+"::"+m.Name] = true
				if len(m.Rets) != 1 || m.Rets[0].Kind != KindHandle {
					t.Errorf("%s: %s::%s twins with %d returns, want one handle",
						v, m.Class, m.Name, len(m.Rets))
				}
				if len(m.TypedArgs) != 0 {
					t.Errorf("%s: %s::%s carries a typed argument list; the twin "+
						"differs only in what comes back", v, m.Class, m.Name)
				}
			}
		}
		if len(want) == 0 {
			t.Fatalf("%s: no method in the description returns a LuaCustomTable, "+
				"so this test audited nothing", v)
		}
		for k := range want {
			if !got[k] {
				t.Errorf("%s: %s returns a LuaCustomTable and has no handle twin, "+
					"so a point lookup on its result materialises the whole table",
					v, k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("%s: %s got a handle twin and does not return a "+
					"LuaCustomTable -- a plain dictionary is a Lua table with no "+
					"handle behind it", v, k)
			}
		}
	}
}

// THE TWINS ARE APPENDED, SO NO EXISTING MEMBER ID MOVES.
//
// Member ids are dense indices into the member slice and a guest bakes them in
// when its bindings are generated, so a member inserted beside the method it
// twins renumbers every member below it -- thousands of moved constants in the
// golden diff, and a stale downstream wasm calling a different function on every
// id it holds. The global functions were appended for exactly this reason and
// said so; this asserts it rather than restating it.
//
// The check is that every member of the report WITHOUT the twins keeps its id in
// the report WITH them, which is a statement about the ordering rule and not
// about any particular pin's numbers.
func TestTheHandleTwinsAppendAndRenumberNothing(t *testing.T) {
	for _, v := range committedVersions(t) {
		a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		r := GenerateMembers(a)

		var lastNonTwin int
		ids := map[string]int{}
		for _, m := range r.Members {
			if m.Kind == MemberCallHandle {
				continue
			}
			ids[fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)] = m.ID
			if m.ID > lastNonTwin {
				lastNonTwin = m.ID
			}
		}
		// Every twin sits AFTER every member that is not one. That is the whole
		// property: an id below the boundary is an id that did not move.
		for _, m := range r.Members {
			if m.Kind == MemberCallHandle && m.ID <= lastNonTwin {
				t.Errorf("%s: the handle twin %s::%s has id %d, below the last "+
					"ordinary member's %d -- every id after it moved", v, m.Class,
					m.Name, m.ID, lastNonTwin)
			}
		}
		// ...and the ids really are 1..n with no gap, so "after everything" and
		// "renumbered nothing" are the same sentence.
		if len(ids)+countKind(r, MemberCallHandle) != len(r.Members) {
			t.Errorf("%s: %d ordinary members plus %d twins is not %d", v, len(ids),
				countKind(r, MemberCallHandle), len(r.Members))
		}
	}
}

func countKind(r Report, kind int) int {
	n := 0
	for _, m := range r.Members {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// AND IT REACHES THE ENGINE, through the real fk_abi.lua against an
// engine-shaped LuaCustomTable.
//
// It is an end-to-end test for the reason the index setter's is: every part of
// this is a seam between two things that are separately correct. The generator
// emits a member whose declared return is a handle; `M.invoke` falls through to
// CALL's own line because everything that differs is the return kind;
// `encode_rets` writes a handle where the materialising member writes a
// (ptr, count); and the handle then has to RESOLVE, which is the whole point --
// a twin that returned a number nothing could index would pass every unit test
// in this package.
//
// The stub's custom table is a metatable rather than a plain table, exactly as
// the index setter's is: a LuaCustomTable is an object whose __index the engine
// implements, and a plain table here would pass whatever the ABI did.
func TestAMethodHandleTwinResolvesAndIndexes(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)

	rawID := full.MemberIndex()[fmt.Sprintf("LuaPrototypes::get_entity_filtered/%d",
		MemberCallHandle)]
	getID := full.MemberIndex()[fmt.Sprintf("LuaCustomTable::index/%d", MemberIndex)]
	if rawID == 0 || getID == 0 {
		t.Fatal("expected get_entity_filtered as a handle and LuaCustomTable's " +
			"index operator")
	}
	// Offsets derived, never assumed: a layout change should move this guest
	// rather than silently make it write somewhere else.
	rawArgs, rawRets, err := full.Members[rawID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(rawRets.Fields) != 1 || rawRets.Fields[0].Offset != 0 {
		t.Fatalf("the twin has %d return fields", len(rawRets.Fields))
	}
	filtAt := rawArgs.Fields[0].Offset
	getArgs, _, err := full.Members[getID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	keyAt := getArgs.Fields[0].Offset

	// Handle 4 is `prototypes`, fourth in the fixed 1..9 block. The guest asks
	// for a filtered table as a HANDLE and then indexes it by name -- two host
	// calls, against a materialising read that would copy every matching
	// prototype across first. Both statuses and the handle itself are written
	// into memory so the assertions read what the guest saw rather than what the
	// stub happened to record.
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(memory 1)
		(data (i32.const 1024) "iron-chest")
		(func (export "fk_on_tick") (param i32)
			;; an empty filter array, which is a (ptr, count) of (0, 0)
			(i32.store (i32.const %d) (i32.const 0))
			(i32.store (i32.const %d) (i32.const 0))
			(i32.store (i32.const 4096)
				(call $call (i32.const 4) (i32.const %d) (i32.const 2048)
					(i32.const 512)))
			(i32.store (i32.const 4104) (i32.load (i32.const 512)))
			(i32.store (i32.const %d) (i32.const 3))
			(i32.store (i32.const %d) (i32.const 1024))
			(i32.store (i32.const %d) (i32.const 10))
			(i32.store (i32.const 4100)
				(call $call (i32.load (i32.const 512)) (i32.const %d)
					(i32.const 3072) (i32.const 640)))))`,
		2048+filtAt, 2048+filtAt+4, rawID,
		3072+keyAt, 3072+keyAt+8, 3072+keyAt+12, getID)

	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	if !complete || len(used) != 2 || !used[rawID] || !used[getID] {
		t.Fatalf("the scan found %v (complete=%v); want exactly the twin and the "+
			"index operator -- a member kind the pruner cannot see is a mod that "+
			"ships the whole API", used, complete)
	}
	pruned := full.Only(used)
	apiSrc, err := pruned.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-calltwin", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk, Exports: []string{"fk_on_tick"}, APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-calltwin",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

-- An engine-shaped LuaCustomTable: an object whose __index the engine
-- implements, not a table with entries in it. The asked counter records that the
-- METHOD really ran, because a twin returning a stale handle would index a table
-- nobody built.
local asked, indexed = 0, {}
local entries = { ["iron-chest"] = { valid = true, object_name = "LuaEntityPrototype" } }
local tbl = setmetatable({ valid = true, object_name = "LuaCustomTable" }, {
  __index = function(_, k)
    indexed[#indexed + 1] = k
    return entries[k]
  end,
})
prototypes = {
  valid = true,
  object_name = "LuaPrototypes",
  get_entity_filtered = function(filters) asked = asked + 1 return tbl end,
}

require("control")
handlers[1]({ tick = 1 })
local m = storage.fk_mem[1]
print("asked " .. asked)
print("indexed " .. tostring(indexed[1]))
print("call st " .. tostring(m[1025]) .. " " .. tostring(m[1026]))
-- The twin returned a HANDLE, so the second call resolved it out of the handle
-- table rather than being handed a copy of the table's contents.
print("handle " .. tostring(m[1027] ~= nil and m[1027] ~= 0))
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "asked 1\nindexed iron-chest\ncall st 0 0\nhandle true"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the twin has to return a handle the index "+
			"operator can then resolve; a materialised table cannot be indexed by "+
			"a second host call)", got, want)
	}
	t.Logf("pruned member table: %d bytes for %d members of %d",
		len(apiSrc), len(pruned.Members), len(full.Members))
}
