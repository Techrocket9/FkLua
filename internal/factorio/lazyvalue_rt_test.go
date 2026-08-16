package factorio

import (
	"fmt"
	"strings"
	"testing"
)

// LuaLazyLoadedValue end to end, against the real layout and the real member
// table, through lua52f.
//
// The generator tests next door prove the SHAPE -- a handle field, a bound
// `get`, an event that is no longer skipped. They cannot prove the property the
// whole design rests on, which is a claim about what the host DOES NOT DO:
// encoding `on_player_setup_blueprint` must not construct the dictionary. That
// is only observable by running the marshaller against an object that can tell
// you whether it was asked, which is what this file is.
//
// The stub is a table with a `get` that counts its calls, wearing
// `object_name = "LuaLazyLoadedValue"` so write_dyn and the handle table treat
// it as a LuaObject the way the engine's userdata is treated. Everything else
// -- the field offsets, the member id, the tag numbers -- comes from the
// generator and the pinned description rather than from this test.
func TestLazyLoadedValueCrossesWithoutBuildingItsPayload(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	getID, ok := r.MemberIndex()[fmt.Sprintf("LuaLazyLoadedValue::get/%d", MemberCall)]
	if !ok {
		t.Fatal("no member id for LuaLazyLoadedValue::get")
	}

	// The REAL event layout, from the generator, so the offset this test reads
	// the handle at is the offset a generated guest decoder reads it at.
	evs := GenerateEvents(a)
	var fields []FieldSpec
	for _, e := range evs.Events {
		if e.Name == "on_player_setup_blueprint" {
			fields = e.Fields
		}
	}
	if fields == nil {
		t.Fatal("on_player_setup_blueprint is not bound; the generator tests say why")
	}
	blk, err := LayoutStruct(fields)
	if err != nil {
		t.Fatal(err)
	}
	mapIdx := 0
	for i, p := range blk.Fields {
		if p.Name == "mapping" {
			mapIdx = i + 1 // Lua is 1-based
		}
	}
	if mapIdx == 0 {
		t.Fatal("no mapping field in the laid-out event")
	}

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)
local fields = %s

-- A bump allocator, which write_dyn needs for the value it will eventually
-- build. Counting the calls is the second instrument: the point is not the
-- absolute number but WHEN it moves -- the encode pays only for its two string
-- fields, and every byte the payload costs is charged at get() instead.
local next_, allocs = 16384, 0
H.bind_alloc(function(n) allocs = allocs + 1 local p = next_ next_ = next_ + n + 8 return p end,
             function() end)

-- THE STUB. "built" is the whole instrument: the engine's contract is that the
-- dictionary is constructed inside get() and nowhere else, so this counter
-- standing at 0 after the encode IS the laziness claim.
local built = 0
local e7  = { valid = true, object_name = "LuaEntity", unit_number = 7 }
local e11 = { valid = true, object_name = "LuaEntity", unit_number = 11 }
-- Keys 1 and 2: a blueprint's entity indices are dense from 1, which is what
-- the engine really hands over. See the DENSE/SPARSE note below -- it decides
-- which tier-2 tag a guest gets, and it is not the one the declared type
-- suggests.
local payload = { [1] = e7, [2] = e11 }
local lazy = {
  valid = true,
  object_name = "LuaLazyLoadedValue",
  get = function()
    built = built + 1
    return payload
  end,
}

-- Encode the event exactly as a dispatch does. Every other field is filled too,
-- so the mapping handle is written by the same pass that writes the rest and
-- not by a one-field special case.
local st = H.write_struct(fields, 1024, {
  player_index = 3,
  surface      = { valid = true, object_name = "LuaSurface" },
  area         = { left_top = { x = 0, y = 0 }, right_bottom = { x = 4, y = 4 } },
  item         = "blueprint",
  quality      = "normal",
  alt          = false,
  mapping      = lazy,
  name         = 68,
  tick         = 4242,
})
print("encode st " .. st .. " built " .. built .. " allocs " .. allocs)
local after_encode = allocs

-- The handle landed where the generated decoder will look for it, and it
-- resolves to the very object -- not a copy, not a materialised dictionary.
local h = IO.ld32(1024 + fields[%d].at)
print("handle nonzero " .. tostring(h ~= 0) .. " resolves " ..
      tostring(H.get(h) == lazy) .. " built " .. built)

-- NOW ask for it, through the same generated member a guest calls.
local m = API.members[%d]
local st2 = H.call(h, %d, 2048, 4096)
local at = 4096 + m.sig.rets[1].at
print("get st " .. st2 .. " built " .. built ..
      " charged " .. (allocs - after_encode) .. " tag " .. IO.ld32(at))

-- ...and decode the tier-2 value the guest receives. The values are OBJECT
-- handles, each resolving to the entity the stub put there -- a dictionary
-- crossing as a scalar here would mean Get() is mistyped, which is the defect
-- the Any work removed.
--
-- DENSE OR SPARSE DECIDES THE TAG, and the declared type does not. The API
-- calls this dictionary<uint32, LuaEntity>, but Lua has one table type: a
-- dictionary whose keys happen to be 1..n is indistinguishable from an array,
-- so write_dyn tags it DYN_ARR (5). A blueprint's entity indices ARE dense from
-- 1, so that is the tag a guest will see in practice -- while a blueprint with
-- a hole in its indices yields DYN_MAP (6) through the same member on the same
-- build. A guest must handle both; this is the tag it will usually get.
local v = H.read_value({ kind = H.K_DYN, at = 0 }, at)
local n = 0
for _ in pairs(v) do n = n + 1 end
print("dense tag " .. IO.ld32(at) .. " pairs " .. n ..
      " one " .. tostring(v[1].unit_number) .. " two " .. tostring(v[2].unit_number))

-- The same member, the same build, a SPARSE mapping: tag 6, and the keys
-- survive as keys rather than being renumbered.
payload = { [1] = e7, [4] = e11 }
H.call(h, %d, 2048, 4096)
local sv = H.read_value({ kind = H.K_DYN, at = 0 }, at)
print("sparse tag " .. IO.ld32(at) ..
      " one " .. tostring(sv[1].unit_number) .. " four " .. tostring(sv[4].unit_number))

-- Calling asks the engine every time: the host caches nothing, so a guest that
-- wants the value twice pays twice and one that never asks pays never.
print("built after two gets " .. built)

-- LIFETIME. The instance is valid only during its own dispatch, and the
-- transient space is what already enforces that for every event payload handle.
H.clear_transient()
print("after dispatch " .. tostring(H.get(h)))
`, blk.LuaTable(), mapIdx, getID, getID, getID))

	want := strings.Join([]string{
		// THE HEADLINE. The whole event encoded and get() was never called, so
		// the dictionary of every entity in the blueprint was never built. The
		// two allocations are the two STRING fields (`item`, `quality`); the
		// mapping contributed none, which is what a handle costs.
		"encode st 0 built 0 allocs 2",
		"handle nonzero true resolves true built 0",
		// ...and the payload is charged here instead, at the one call that asked
		// for it: one allocation for the pair block write_dyn builds.
		"get st 0 built 1 charged 1 tag 5",
		"dense tag 5 pairs 2 one 7 two 11",
		"sparse tag 6 one 7 four 11",
		"built after two gets 2",
		"after dispatch nil",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
