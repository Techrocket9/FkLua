package factorio

import (
	"fmt"
	"strings"
	"testing"
)

// THE THREE GLOBAL FUNCTIONS -- `log`, `localised_print`, `table_size` -- which
// are on no class and were bound by nothing for as long as there have been
// binding generators.
//
// `global_functions_bound: 0` sat in every census.json with a comment saying it
// was a decision rather than an omission, and that is the only reason it could
// be re-taken: BetterBeltBalancer asked for `log`, which is THE ONLY WAY TO READ
// A LuaProfiler'S DURATION. LuaProfiler's complete member set is add, divide,
// reset, restart, stop, object_name, object_name_is and valid; nothing returns
// the number, and the engine renders it only when the profiler is an ELEMENT of
// a LocalisedString. `log{"", "took ", p}` is the whole idiom.
//
// This file has two halves. TestTheGlobalFunctionsAreBound is the generator's:
// the members exist, in both backends, at one id each. TestAGlobalFunctionDis-
// patches is the ABI's, and it is the one with teeth -- the kind's branch runs
// BEFORE the handle is resolved, which is a property no count can see.

// The three, as the description names them at every pin this repo owns.
var theGlobalFunctions = []string{"localised_print", "log", "table_size"}

// THE MEMBERS EXIST, IN BOTH BACKENDS, AT ONE ID EACH.
//
// Checked by NAME rather than by count, because a member counted twice and a
// member counted in neither cancel in a total -- the same reason
// TestBothBackendsBindTheSameMembers compares id sets.
func TestTheGlobalFunctionsAreBound(t *testing.T) {
	a, r, g, rb := genBoth(t)

	if len(a.GlobalFunctions) != len(theGlobalFunctions) {
		t.Fatalf("this description declares %d global functions and this test "+
			"knows %d: %v. A new one is news -- give it a row here rather than "+
			"loosening the check", len(a.GlobalFunctions),
			len(theGlobalFunctions), a.GlobalFunctions)
	}

	byName := map[string]Member{}
	for _, m := range r.Members {
		if m.Kind != MemberGlobalFunc {
			continue
		}
		if m.Class != "" {
			t.Errorf("global function %q carries class %q: a global function is "+
				"on NO class, and an empty Class is what both binding generators "+
				"branch on", m.Name, m.Class)
		}
		if m.HasValid {
			t.Errorf("global function %q claims a `valid` attribute: there is no "+
				"object to have one, and M.invoke never resolves a handle for "+
				"this kind", m.Name)
		}
		if _, dup := byName[m.Name]; dup {
			t.Errorf("global function %q is in the member table twice", m.Name)
		}
		byName[m.Name] = m
	}

	for _, name := range theGlobalFunctions {
		m, ok := byName[name]
		if !ok {
			t.Errorf("%s is not in the host member table. `fklua gen-bindings` "+
				"names what it skipped under the host member table's deferrals",
				name)
			continue
		}
		key := fmt.Sprintf("::%s/%d", name, MemberGlobalFunc)
		gn, gok := g.Names[key]
		rn, rok := rb.Names[key]
		if !gok || !rok {
			t.Errorf("%s bound in Go=%v Rust=%v: the two backends read one member "+
				"table and a member reaching one of them is a rendering gap",
				name, gok, rok)
			continue
		}
		t.Logf("%s -> id %d, Go %s, Rust %s", name, m.ID, gn, rn)
	}

	// THE ID ORDER, which is not decoration. Member ids are dense indices into
	// the report's slice, so a global function inserted anywhere but the END
	// renumbers every member below it -- an 8,000-line golden diff with the real
	// change somewhere inside it. They are appended, so these are the last three
	// ids in the table and nothing before them moved.
	for _, name := range theGlobalFunctions {
		if m, ok := byName[name]; ok && m.ID <= len(r.Members)-len(theGlobalFunctions) {
			t.Errorf("%s has id %d of %d members: the global functions are "+
				"appended AFTER every class so that adding one cannot renumber "+
				"anything", name, m.ID, len(r.Members))
		}
	}
}

// THE DISPATCH, through the real fk_abi.lua under lua52f, against globals shaped
// the way the engine's are.
//
// Five legs, each a property that could be wrong on its own:
//
//   - `log` with a nested LocalisedString carrying a DYN_OBJ, which is the
//     profiler idiom to the byte. The stub receives the ASSEMBLED Lua table and
//     the object arrives as the identity the handle names -- not a copy, not a
//     number;
//   - THE HANDLE OPERAND IS UNREAD. The same call made with a handle that
//     resolves to nothing answers identically, which is the only way to say out
//     loud that the branch runs before M.get. Every other kind answers
//     ERR_BAD_HANDLE here;
//   - `table_size` counts and returns, so the kind carries a RETURN as well as
//     arguments;
//   - a global this Factorio does not have is ERR_NO_MEMBER, the same
//     degradation a removed class member gets, rather than a Lua error;
//   - a global that RAISES is ERR_CALL_FAILED with the engine's own text in
//     last_error, never an unwind through the wasm frame the call came from.
func TestAGlobalFunctionDispatches(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	idx := r.MemberIndex()
	id := func(name string) int {
		v, ok := idx[fmt.Sprintf("::%s/%d", name, MemberGlobalFunc)]
		if !ok {
			t.Fatalf("no member id for the global function %s", name)
		}
		return v
	}
	logID, printID, sizeID := id("log"), id("localised_print"), id("table_size")

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)

-- The profiler. A LuaObject as far as this layer is concerned: it has a
-- a valid, an object_name, and NO accessor for its own duration -- which is the
-- whole reason log has to exist. tostring() stands in for the engine's own
-- rendering, which is what puts "Duration: 12.368959ms" in the log file.
local prof = { valid = true, object_name = "LuaProfiler" }
setmetatable(prof, { __tostring = function() return "Duration: 12.368959ms" end })

-- The globals, shaped the way _G's are: plain functions in the environment
-- table. genv is read lazily on every call, so binding here is the same act as
-- Factorio having them at load.
local seen = {}
local env = {
  log = function(v) seen[#seen + 1] = { fn = "log", v = v } end,
  localised_print = function(v) seen[#seen + 1] = { fn = "print", v = v } end,
  table_size = function(t) local n = 0 for _ in pairs(t) do n = n + 1 end return n end,
  boom = function() error("the engine said no") end,
}
H.bind_globals(env)

local function put(at, s) for i = 1, #s do IO.st8(at + i - 1, s:byte(i)) end end
local function zero(at, n) for i = 0, n - 1 do IO.st8(at + i, 0) end end
-- One tier-2 slot: tag at 0, payload at 8.
local function dyn(at, tag, a, b)
  zero(at, 16)
  IO.st32(at, tag)
  if a ~= nil then IO.st32(at + 8, a) end
  if b ~= nil then IO.st32(at + 12, b) end
end

-- log{"", "took ", p} -- a DYN_ARR of three elements at 1536, 16 bytes apiece.
-- The empty first element is LocalisedString's "concatenate the rest" form and
-- the third is the profiler, which is what the engine renders.
put(1024, "")
put(1030, "took ")
dyn(1536, H.DYN_STR, 1024, 0)
dyn(1552, H.DYN_STR, 1030, 5)
dyn(1568, H.DYN_OBJ, H.transient(prof))

local lm = API.members[%d]
zero(2048, lm.argsize)
dyn(2048 + lm.sig.args[1].at, H.DYN_ARR, 1536, 3)
-- HANDLE 0, which every other kind answers ERR_BAD_HANDLE.
-- The globals are read out of the log defensively: a dispatch that never
-- reached one leaves it empty, and a test that indexes nil there dies with a
-- Lua traceback instead of a diff. A red proof exists to read the WRONG
-- ANSWER, so an unreached global has to print as one.
local NOTCALLED = { fn = "NONE", v = { "", "", false } }
local function last() return seen[#seen] or NOTCALLED end

local st = H.call(0, %d, 2048, 0)
local e = last()
print("log st " .. st .. " fn " .. e.fn .. " n " .. #e.v ..
      " head '" .. tostring(e.v[1]) .. tostring(e.v[2]) .. "'" ..
      " same " .. tostring(e.v[3] == prof) ..
      " rendered " .. tostring(e.v[3]))

-- ...AND THE SAME CALL WITH A DEAD HANDLE, which is the property no count can
-- see: the GFUNC branch runs before M.get, so the handle is never resolved.
-- 4242 is in the persistent range and nothing put anything there.
st = H.call(4242, %d, 2048, 0)
print("log-dead st " .. st .. " same " .. tostring(last().v[3] == prof))

-- localised_print resolves ITS OWN global rather than log's: the member name in
-- the table is what genv is indexed by.
st = H.call(0, %d, 2048, 0)
print("print st " .. st .. " fn " .. last().fn)

-- table_size, which is the leg with a RETURN.
put(1200, "a") put(1201, "b") put(1202, "c")
zero(1600, 48)
dyn(1600, H.DYN_STR, 1200, 1) dyn(1616, H.DYN_STR, 1201, 1)
dyn(1632, H.DYN_STR, 1202, 1)
zero(1700, 96)
for i = 0, 2 do
  dyn(1700 + i * 32, H.DYN_STR, 1200 + i, 1)
  dyn(1700 + i * 32 + 16, H.DYN_NUM, 0, 0)
  IO.stf64(1700 + i * 32 + 16 + 8, i + 1)
end
local tm = API.members[%d]
zero(2048, tm.argsize)
dyn(2048 + tm.sig.args[1].at, H.DYN_MAP, 1700, 3)
zero(4096, tm.retsize)
st = H.call(0, %d, 2048, 4096)
print("size st " .. st .. " n " .. IO.ld32(4096 + tm.sig.rets[1].at))

-- A GLOBAL THIS FACTORIO DOES NOT HAVE. Same degradation a removed class member
-- gets: reported once, ERR_NO_MEMBER every time after, never a Lua error.
H.bind_members({ [1] = { kind = H.GFUNC, name = "no_such_global",
                         argsize = 0, retsize = 0, sig = { args = {}, rets = {} } },
                 [2] = { kind = H.GFUNC, name = "boom",
                         argsize = 0, retsize = 0, sig = { args = {}, rets = {} } } })
print("absent st " .. H.call(0, 1, 0, 0))

-- ...AND ONE THAT RAISES. A Factorio global can, and an error crossing the wasm
-- frame this call came from takes the mod down rather than the call.
st = H.call(0, 2, 0, 0)
print("raise st " .. st .. " err " .. tostring(H.last_error():match("said no") ~= nil))
`, logID, logID, logID, printID, sizeID, sizeID))

	want := strings.Join([]string{
		// Three elements, the first two concatenated, and the third IS the
		// profiler object rather than a copy or a handle number -- which is
		// what makes the engine render its duration.
		"log st 0 fn log n 3 head 'took ' same true rendered Duration: 12.368959ms",
		// The handle is not read, so a dead one answers identically.
		"log-dead st 0 same true",
		"print st 0 fn print",
		"size st 0 n 3",
		"absent st 3",
		"raise st 5 err true",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
