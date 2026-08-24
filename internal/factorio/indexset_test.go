package factorio

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	luart "github.com/Techrocket9/fklua/runtime"
)

// The index operator's WRITE half: `obj[k] = v`.
//
// `settings.global["name"] = {value = true}` is the only way a mod changes its
// own runtime-global setting, and until 2026-08-24 it had no expression in the
// bindings at all. Two layers were in the way and the second is why this needed
// an ABI change rather than a generator one: the description declares an
// operator's `read_type` and never a `write_type`, so a generator that mirrors
// it correctly emits no setter; and MemberSet takes its member NAME out of the
// generation-time member table, so it has nowhere to put a key. Filed by
// BetterBeltBalancer.
//
// These tests are about the two decisions that follow: which receivers are
// allowed a setter (indexWriteHalf, an allowlist over the description's PROSE)
// and what the ABI does with one (M.IDXSET).

// EVERY INDEX OPERATOR AT THE PIN HAS A VERDICT, and a `false` is a verdict.
//
// This is TestOperatorKeyKinds' shape and it is here for the same reason: the
// key type is derived because the description does not carry one, and so is the
// write half, so both want an enumeration that fails when a pin adds a class
// rather than a rule that silently classifies it. What that rule would be here
// is the whole question -- "no entry" cannot mean "not writable", because that
// is indistinguishable from "nobody looked", which is exactly how eleven class
// operators stayed invisible for five milestones.
func TestEveryIndexOperatorHasAWriteVerdict(t *testing.T) {
	a := loadTestAPI(t)
	seen := map[string]bool{}
	for _, c := range a.Classes {
		for _, o := range c.Operators {
			if o.Name != "index" {
				continue
			}
			seen[c.Name] = true
			if _, ok := indexWriteHalf[c.Name]; !ok {
				t.Errorf("%s declares an index operator and indexWriteHalf has no "+
					"row for it. Decide whether `%s[k] = v` is legal -- the "+
					"description says so in PROSE if at all, on the operator or on "+
					"the members that yield the class -- and write the row down "+
					"with the sentence you read.", c.Name, lowerFirstWord(c.Name))
			}
		}
	}
	for cls := range indexWriteHalf {
		if !seen[cls] {
			t.Errorf("indexWriteHalf has a row for %s and the %s pin declares no "+
				"index operator on it: a stale row is a claim about a class that "+
				"is not there", cls, DefaultAPIVersion)
		}
	}
}

// ...AND THE VERDICTS PRODUCE EXACTLY THE MEMBERS THEY SAY, in the table and in
// both backends.
//
// The negative half is the one with teeth. A setter on LuaInventory would be a
// binding that exists and always fails -- "a skipped member is skipped, never
// faked", pointed at a member that would be faked -- and nothing else in the
// suite would notice it, because every call of it comes back as a status like
// any other refusal.
func TestTheWritableIndexOperatorsGetASetter(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	g, err := GenerateGoWith(a, r, ev, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, ev)
	if err != nil {
		t.Fatal(err)
	}

	// What the description declares an index operator on, and what it yields,
	// so the setter's own signature can be checked against the reader's.
	readers := map[string]Member{}
	for _, m := range r.Members {
		if m.Kind == MemberIndex {
			readers[m.Class] = m
		}
	}
	setters := map[string]Member{}
	for _, m := range r.Members {
		if m.Kind == MemberIndexSet {
			setters[m.Class] = m
		}
	}

	for cls, want := range indexWriteHalf {
		if _, declared := readers[cls]; !declared {
			continue // not at this pin; TestEveryIndexOperatorHasAWriteVerdict says so
		}
		sm, got := setters[cls]
		if got != want {
			t.Errorf("%s: indexWriteHalf says %v and the member table says %v",
				cls, want, got)
			continue
		}
		if !want {
			continue
		}

		// THE KEY IS THE READER'S KEY. Two questions about one identity have to
		// ask the same code -- indexKey -- or a class indexed by position for a
		// get and by a tier-2 value for a set ships two answers to one question.
		rm := readers[cls]
		if len(sm.Args) != 2 {
			t.Errorf("%s setter takes %d arguments, want key and value",
				cls, len(sm.Args))
			continue
		}
		if sm.Args[0].Kind != rm.Args[0].Kind {
			t.Errorf("%s: the setter's key is %v and the reader's is %v",
				cls, sm.Args[0].Kind, rm.Args[0].Kind)
		}
		// AND THE VALUE IS THE READER'S RETURN, optionality included: the
		// description gives an operator exactly one type, so a write half with a
		// different one would be this generator inventing a signature.
		if sm.Args[1].Kind != rm.Rets[0].Kind {
			t.Errorf("%s: the setter's value is %v and the reader yields %v",
				cls, sm.Args[1].Kind, rm.Rets[0].Kind)
		}
		if sm.Args[1].Optional != rm.Rets[0].Optional {
			t.Errorf("%s: the setter's value is optional=%v and the reader's is %v "+
				"-- LuaFluidBox's whole nil-clears-the-box gesture rides on this",
				cls, sm.Args[1].Optional, rm.Rets[0].Optional)
		}
		// A setter returns nothing: an assignment is not an expression in Lua.
		if len(sm.Rets) != 0 {
			t.Errorf("%s setter declares %d return values", cls, len(sm.Rets))
		}

		// AND BOTH BACKENDS EMIT IT. A name collision is the one thing that can
		// take a member back out quietly, and it is what forced Get/Length/Call
		// on the read operators.
		key := fmt.Sprintf("%s::index/%d", cls, MemberIndexSet)
		if n := g.Names[key]; n != "Set" {
			t.Errorf("Go named %s's index setter %q, want Set", cls, n)
		}
		if n := rb.Names[key]; n != "set" {
			t.Errorf("Rust named %s's index setter %q, want set", cls, n)
		}
	}

	// The negative, stated as a count so a class added to the allowlist by
	// accident is visible here and not only in the census diff.
	var writable []string
	for cls, w := range indexWriteHalf {
		if w && readers[cls].Class != "" {
			writable = append(writable, cls)
		}
	}
	sort.Strings(writable)
	if len(setters) != len(writable) {
		t.Errorf("%d index setters in the table for %v", len(setters), writable)
	}
}

// THE KIND NUMBER IS AN ABI SURFACE shared with hand-written Lua, exactly as
// the three read operators' are: a member that dispatches as something else is
// a member that does something else.
func TestTheIndexSetKindMatchesTheABI(t *testing.T) {
	src := luart.ABI()
	if !strings.Contains(src, fmt.Sprintf("M.IDXSET = %d", MemberIndexSet)) {
		t.Errorf("fk_abi.lua does not set M.IDXSET to %d", MemberIndexSet)
	}
}

// THE ASSIGNMENT DISPATCHING FOR REAL, through fk_abi.lua under lua52f against
// Lua objects carrying the metamethods Factorio's own carry.
//
// The generator tests above prove the member exists and is typed; this is the
// half that says the ABI does the right Lua thing with it. Four legs, and each
// is a property that could be wrong on its own:
//
//   - a tier-2 key and a tier-2 TABLE value, which is `settings.global[name] =
//     {value = true}` to the byte and the whole reason this exists;
//   - the read operator seeing what the write left, so the two really are
//     halves of one thing rather than a write into somewhere else;
//   - a raising __newindex coming back as a STATUS with the engine's own text
//     in last_error, never as an unwind through the wasm frame -- which is what
//     `settings.startup` and every read-only custom table in the game do;
//   - an ABSENT value reaching the metamethod as nil, which is LuaFluidBox's
//     documented clear and needs no code of its own: M.call trims to the last
//     argument present and `local k, v = ...` does the rest.
func TestAnIndexAssignDispatches(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	idx := r.MemberIndex()
	id := func(class string, kind int) int {
		v, ok := idx[fmt.Sprintf("%s::index/%d", class, kind)]
		if !ok {
			t.Fatalf("no member id for %s::index kind %d", class, kind)
		}
		return v
	}
	ctSet := id("LuaCustomTable", MemberIndexSet)
	ctGet := id("LuaCustomTable", MemberIndex)
	fbSet := id("LuaFluidBox", MemberIndexSet)

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)

local function call(h, mid, argp, retp)
  return H.call(H.transient(h), mid, argp, retp)
end
local function zero(at, n) for i = 0, n - 1 do IO.st8(at + i, 0) end end
local function put(at, s) for i = 1, #s do IO.st8(at + i - 1, s:byte(i)) end end

-- A WRITABLE custom table. __newindex is what an assignment reaches on a
-- LuaObject, which is exactly why IDX could not carry this: the read goes
-- through __index and the write through a different metamethod entirely.
local store = {}
local ct = setmetatable({ valid = true }, {
  __index    = function(_, k) return store[k] end,
  __newindex = function(_, k, v) store[k] = v end,
})

-- settings.global["bbb-multi-edge-parts"] = {value = true}
local sm = API.members[%d]
put(1024, "bbb-multi-edge-parts")
put(1100, "value")
-- The map's one pair, at 1536: a DYN_STR key and a DYN_BOOL value, one 32-byte
-- pair of 16-byte values.
zero(1536, 32)
IO.st32(1536, 3) IO.st32(1536 + 8, 1100) IO.st32(1536 + 12, 5)
IO.st32(1536 + 16, 1) IO.st8(1536 + 16 + 8, 1)
zero(2048, sm.argsize)
local ka = 2048 + sm.sig.args[1].at
IO.st32(ka, 3) IO.st32(ka + 8, 1024) IO.st32(ka + 12, 20)
local va = 2048 + sm.sig.args[2].at
IO.st32(va, 6) IO.st32(va + 8, 1536) IO.st32(va + 12, 1)
local st = call(ct, %d, 2048, 0)
local held = store["bbb-multi-edge-parts"]
print("set st " .. st .. " value " .. tostring(held ~= nil and held.value))

-- ...and the READ operator sees what the write left, so the two are halves of
-- one thing rather than a write into somewhere else.
--
-- A SCALAR value for this leg, deliberately. A table coming BACK crosses as a
-- tier-2 map, which the host writes into the GUEST's heap through an allocator
-- this harness does not bind -- ERR_NO_SPACE, 6, and a property of the harness
-- rather than of the ABI. The leg above already read the table back out of the
-- stub in Lua; what is unproven until here is that IDX and IDXSET name the same
-- slot at all.
put(1200, "n")
zero(2048, sm.argsize)
ka = 2048 + sm.sig.args[1].at
IO.st32(ka, 3) IO.st32(ka + 8, 1200) IO.st32(ka + 12, 1)
va = 2048 + sm.sig.args[2].at
IO.st32(va, 2) IO.stf64(va + 8, 42)
print("set2 st " .. call(ct, %d, 2048, 0))

local gm = API.members[%d]
zero(2048, gm.argsize)
IO.st32(2048 + gm.sig.args[1].at, 3)
IO.st32(2048 + gm.sig.args[1].at + 8, 1200)
IO.st32(2048 + gm.sig.args[1].at + 12, 1)
st = call(ct, %d, 2048, 4096)
print("get st " .. st .. " tag " .. IO.ld32(4096 + gm.sig.rets[1].at) ..
      " v " .. IO.ldf64(4096 + gm.sig.rets[1].at + 8))

-- A READ-ONLY custom table -- settings.startup, game.players, every prototype
-- table -- raises from __newindex. It must come back as a status carrying the
-- engine's own words, never as an unwind through the frame the call came from.
local ro = setmetatable({ valid = true }, {
  __newindex = function() error("LuaCustomTable is read only") end,
})
st = call(ro, %d, 2048, 0)
print("readonly st " .. st .. " err " ..
      tostring(H.last_error():match("read only") ~= nil))

-- AN ABSENT VALUE IS A REAL nil: LuaFluidBox's index is declared optional and
-- its own prose says writing nil removes all fluid. Nothing special-cases it --
-- M.call trims to the last argument PRESENT and the branch binds two names.
local cleared = false
local fb = setmetatable({ valid = true }, {
  __newindex = function(_, k, v) cleared = (k == 3 and v == nil) end,
})
local fm = API.members[%d]
zero(2048, fm.argsize)
IO.st32(2048 + fm.sig.args[1].at, 3)
st = call(fb, %d, 2048, 0)
print("clear st " .. st .. " nil " .. tostring(cleared))
`, ctSet, ctSet, ctSet, ctGet, ctGet, ctSet, fbSet, fbSet))

	want := strings.Join([]string{
		"set st 0 value true",
		"set2 st 0",
		// tag 2 is DYN_NUM.
		"get st 0 tag 2 v 42",
		// The text is asserted beside the status, because a status with an
		// empty last_error is what a branch that forgot to record one produces.
		"readonly st 5 err true",
		"clear st 0 nil true",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// THE SETTER PRUNES LIKE EVERY OTHER MEMBER, and this is the whole path a
// downstream mod takes: settings.GlobalRaw() for the handle, then the write.
//
// Pruning is the property this project cares most about -- `fklua mod` ships
// the members whose ids it can prove constant at a call site and nothing else --
// and a new KIND cannot break it by construction, because the scan reads i32
// constants reaching `fk.call` and knows nothing about kinds. That argument is
// worth an assertion rather than a paragraph: two members of 4,259, in a table
// small enough to state, and the same guest driven through the real control.lua
// against an engine-shaped `settings`.
func TestAnIndexSetterPrunesAndReachesTheEngine(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)

	// Handle 9 is `settings`, ninth in the fixed 1..9 block.
	rawID := full.MemberIndex()[fmt.Sprintf("LuaSettings::global/%d", MemberGetHandle)]
	setID := full.MemberIndex()[fmt.Sprintf("LuaCustomTable::index/%d", MemberIndexSet)]
	if rawID == 0 || setID == 0 {
		t.Fatal("expected LuaSettings::global as a handle and LuaCustomTable's index setter")
	}
	// The offsets are derived rather than assumed: a layout change should move
	// the guest this test writes, not silently make it write somewhere else.
	_, rawRets, err := full.Members[rawID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rawRets.Fields); n != 1 || rawRets.Fields[0].Offset != 0 {
		t.Fatalf("LuaSettings::global as a handle has %d return fields", n)
	}
	setArgs, _, err := full.Members[setID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	keyAt, valAt := setArgs.Fields[0].Offset, setArgs.Fields[1].Offset

	// A guest that does what the downstream mod does: take the handle, then
	// settings.global["bbb-multi-edge-parts"] = {value = true}.
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(memory 1)
		(data (i32.const 1024) "bbb-multi-edge-parts")
		(data (i32.const 1100) "value")
		(func (export "fk_on_tick") (param i32)
			(drop (call $call (i32.const 9) (i32.const %d) (i32.const 0) (i32.const 512)))
			;; the value table's single pair: "value" -> true
			(i32.store (i32.const 1536) (i32.const 3))
			(i32.store (i32.const 1544) (i32.const 1100))
			(i32.store (i32.const 1548) (i32.const 5))
			(i32.store (i32.const 1552) (i32.const 1))
			(i32.store8 (i32.const 1560) (i32.const 1))
			;; the argument block: a string key and a map value
			(i32.store (i32.const %d) (i32.const 3))
			(i32.store (i32.const %d) (i32.const 1024))
			(i32.store (i32.const %d) (i32.const 20))
			(i32.store (i32.const %d) (i32.const 6))
			(i32.store (i32.const %d) (i32.const 1536))
			(i32.store (i32.const %d) (i32.const 1))
			(drop (call $call (i32.load (i32.const 512)) (i32.const %d)
				(i32.const 2048) (i32.const 0)))))`,
		rawID,
		2048+keyAt, 2048+keyAt+8, 2048+keyAt+12,
		2048+valAt, 2048+valAt+8, 2048+valAt+12,
		setID)

	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	if !complete || len(used) != 2 || !used[rawID] || !used[setID] {
		t.Fatalf("the scan found %v (complete=%v); want exactly the handle read "+
			"and the index setter -- a member kind the pruner cannot see is a mod "+
			"that ships the whole API", used, complete)
	}
	pruned := full.Only(used)
	apiSrc, err := pruned.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(apiSrc); n > 2000 {
		t.Errorf("the pruned table is %d bytes for two members", n)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2, Persist: luagen.PersistNone})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-idxset", Version: "0.1.0", Title: "t", Author: "x",
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
  mod_name = "fk-idxset",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

-- An engine-shaped settings.global: a LuaCustomTable is not a table with
-- entries in it, it is an object whose __newindex the engine implements. A
-- plain table here would pass whatever the ABI did.
local written = {}
local globals = setmetatable({ valid = true, object_name = "LuaCustomTable" }, {
  __index    = function(_, k) return written[k] end,
  __newindex = function(_, k, v) written[k] = v end,
})
settings = { valid = true, global = globals }

require("control")
handlers[1]({ tick = 1 })
local v = written["bbb-multi-edge-parts"]
print("wrote " .. tostring(v ~= nil and v.value))
print("keys " .. tostring(next(written)))
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "wrote true\nkeys bbb-multi-edge-parts"
	if strings.TrimSpace(out) != want {
		t.Errorf("got %q, want %q", strings.TrimSpace(out), want)
	}
	t.Logf("pruned member table: %d bytes for %d members of %d",
		len(apiSrc), len(pruned.Members), len(full.Members))
}
