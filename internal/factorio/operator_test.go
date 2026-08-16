package factorio

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	luart "github.com/Techrocket9/fklua/runtime"
)

// Class operators: `obj[k]`, `#obj` and `obj(...)`.
//
// Eleven of them across seven classes at the 2.0.77 GA pin, nine across six at
// 2.1.x (which retired LuaFluidBox's pair in the fluid rework), and until
// 2026-08-03 the generators did not read Class.Operators at all -- so they were
// not bound, not DEFERRED, and not counted. That last word is what these tests
// are really about: a member the generator tries and cannot express shows up in
// the deferral report, and a member it never tries shows up nowhere. Reported
// by fklua-ports' resource-marker (RM1), and independently as qol-research's Q2
// and fluid-memory-storage's F-IDX, which turned out to be the same gap.

// operatorsByVersion is what each committed description declares, written out
// so a pin that adds or removes one fails HERE with a readable message rather
// than silently changing a count somewhere else.
//
// PER VERSION RATHER THAN ONE TABLE, because the operator set is one of the few
// things that genuinely MOVED between the two descriptions this repo ships, and
// both of them are pinnable. A single table would have to be the union -- which
// cannot say that a class is absent, and so could not have caught 2.1 taking
// LuaFluidBox's pair away, which is the exact event this table was written for.
// Moving the default pin to a description with no entry here fails with a
// message saying to add one, which is the right amount of ceremony for a change
// that also regenerates every binding in the repo.
var operatorsByVersion = map[string]map[string][]string{
	"2.0.77": {
		"LuaChunkIterator":   {"call"},
		"LuaCustomTable":     {"index", "length"},
		"LuaFluidBox":        {"index", "length"},
		"LuaGuiElement":      {"index"},
		"LuaInventory":       {"index", "length"},
		"LuaRandomGenerator": {"call"},
		"LuaTransportLine":   {"index", "length"},
	},
	"2.1.12": {
		"LuaChunkIterator":   {"call"},
		"LuaCustomTable":     {"index", "length"},
		"LuaGuiElement":      {"index"},
		"LuaInventory":       {"index", "length"},
		"LuaRandomGenerator": {"call"},
		"LuaTransportLine":   {"index", "length"},
	},
	"2.1.14": {
		"LuaChunkIterator":   {"call"},
		"LuaCustomTable":     {"index", "length"},
		"LuaGuiElement":      {"index"},
		"LuaInventory":       {"index", "length"},
		"LuaRandomGenerator": {"call"},
		"LuaTransportLine":   {"index", "length"},
	},
}

// operatorClasses is the table for the pin these tests actually load.
func operatorClasses(t *testing.T) map[string][]string {
	t.Helper()
	m, ok := operatorsByVersion[DefaultAPIVersion]
	if !ok {
		t.Fatalf("no operator table for the %s pin -- add one to "+
			"operatorsByVersion; the set moved between 2.0.77 and 2.1 and a "+
			"missing entry must not read as \"no operators\"", DefaultAPIVersion)
	}
	return m
}

// operatorCount is DERIVED from the table above rather than written beside it.
// Two numbers that have to agree about one fact is the shape this repo has been
// bitten by three times now; a pin that removes an operator should have exactly
// one place to be acknowledged.
func operatorCount(t *testing.T) int {
	n := 0
	for _, ops := range operatorClasses(t) {
		n += len(ops)
	}
	return n
}

func TestTheDescriptionStillDeclaresTheOperatorsThisTableNames(t *testing.T) {
	a := loadTestAPI(t)
	got := map[string][]string{}
	n := 0
	for _, c := range a.Classes {
		for _, o := range c.Operators {
			got[c.Name] = append(got[c.Name], o.Name)
			n++
		}
	}
	if want := operatorCount(t); n != want {
		t.Errorf("the pin declares %d class operators, not %d; every count and "+
			"rule below was written against this table", n, want)
	}
	for cls, want := range operatorClasses(t) {
		sort.Strings(got[cls])
		if strings.Join(got[cls], ",") != strings.Join(want, ",") {
			t.Errorf("%s declares operators %v, want %v", cls, got[cls], want)
		}
	}
	for cls := range got {
		if _, ok := operatorClasses(t)[cls]; !ok {
			t.Errorf("%s declares operators %v and is not in this test's table; "+
				"decide what its key type is before the generator guesses",
				cls, got[cls])
		}
	}
}

// EVERY OPERATOR REACHES THE MEMBER TABLE. The gap this closes was silence, so
// the test is a count and not a spot check.
func TestEveryOperatorReachesTheMemberTable(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	seen := map[string]bool{}
	for _, m := range r.Members {
		if m.IsOperator() {
			seen[m.Class+"::"+m.Name] = true
		}
	}
	for cls, ops := range operatorClasses(t) {
		for _, op := range ops {
			if !seen[cls+"::"+op] {
				t.Errorf("%s::%s is declared in the description and is not in the "+
					"member table: not bound, and not counted either", cls, op)
			}
		}
	}
	if want := operatorCount(t); len(seen) != want {
		t.Errorf("%d operator members, want %d", len(seen), want)
	}
}

// ...AND EVERY ONE OF THEM IS EMITTED BY BOTH BACKENDS.
//
// A name collision is the one thing that can quietly take an operator back out:
// `seen` in both generators is first-come and LuaInventory declares an ordinary
// attribute called `index`, which is exactly the class F-IDX was about. The
// operators are renamed to Get/Length/Call for that reason; this fails if the
// rename ever stops being enough.
func TestOperatorsBindOnEveryClassThatHasOne(t *testing.T) {
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
	for _, m := range r.Members {
		if !m.IsOperator() {
			continue
		}
		key := fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)
		if _, ok := g.Names[key]; !ok {
			t.Errorf("Go dropped the %s operator on %s -- most likely a name "+
				"collision with a member the class declares", m.Name, m.Class)
		}
		if _, ok := rb.Names[key]; !ok {
			t.Errorf("Rust dropped the %s operator on %s", m.Name, m.Class)
		}
	}
	if g.Names["LuaInventory::index/"+fmt.Sprint(MemberIndex)] != "Get" {
		t.Errorf("LuaInventory's index operator is %q, want Get",
			g.Names["LuaInventory::index/"+fmt.Sprint(MemberIndex)])
	}
}

// THE INDEX KEY'S TYPE, which the description does not carry and the generator
// therefore derives. The rule is in buildOperator; this pins the answer for all
// five so a pin that adds a sixth fails rather than being classified silently.
func TestOperatorKeyKinds(t *testing.T) {
	want := map[string]Kind{
		// A class that also answers `#` is a Lua SEQUENCE, so it indexes by
		// position.
		"LuaFluidBox":      KindU32,
		"LuaInventory":     KindU32,
		"LuaTransportLine": KindU32,
		// ...unless what it yields is itself tier 2, which is the description
		// saying the class is heterogeneous. LuaCustomTable yields `Any` and is
		// really keyed by `uint32 | string` at half its use sites.
		"LuaCustomTable": KindDyn,
		// And LuaGuiElement declares no `length`; its index is by child NAME.
		"LuaGuiElement": KindDyn,
	}
	a := loadTestAPI(t)
	for _, m := range GenerateMembers(a).Members {
		if m.Kind != MemberIndex {
			continue
		}
		w, ok := want[m.Class]
		if !ok {
			t.Errorf("%s has an index operator and no entry here", m.Class)
			continue
		}
		if len(m.Args) != 1 || m.Args[0].Kind != w {
			t.Errorf("%s index key is %v, want %v", m.Class, m.Args[0].Kind, w)
		}
	}
}

// `Any` MUST NOT BE A HANDLE, which is what canonicalUnion made it until the
// operators needed it: `string | boolean | number | table | LuaObject` matches
// shape B on a count -- one class, three scalars -- and is not shape B, because
// the `table` arm makes it a genuine any-value union. Getting this wrong types
// LuaCustomTable's index, remote.call and LuaLazyLoadedValue::get as returning
// an object and silently mistypes every string and number they really return.
func TestAnyIsTierTwoAndForceIDIsStillAHandle(t *testing.T) {
	a := loadTestAPI(t)
	m := newTypeMapper(a)
	for _, tc := range []struct {
		name string
		want Kind
	}{
		{"Any", KindDyn},
		{"AnyBasic", KindDyn},
		// The shape canonicalUnion was written for, which must not move.
		{"ForceID", KindHandle},
		{"SurfaceIdentification", KindHandle},
		{"MapPosition", KindStruct},
	} {
		f, err := m.mapType(Type{Name: tc.name}, 0)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if f.Kind != tc.want {
			t.Errorf("%s maps to %v, want %v", tc.name, f.Kind, tc.want)
		}
	}
}

// THE THREE NEW KINDS ARE THE ONES fk_abi.lua ANSWERS TO. The numbers are an
// ABI surface shared with hand-written Lua, and a mismatch is a member that
// dispatches as something else entirely.
func TestOperatorKindsMatchTheABI(t *testing.T) {
	src := luart.ABI()
	for _, want := range []struct {
		name string
		n    int
	}{
		{"M.IDX", MemberIndex}, {"M.LEN", MemberLen}, {"M.SELF", MemberSelf},
	} {
		if !strings.Contains(src, fmt.Sprintf("%s  = %d", want.name, want.n)) &&
			!strings.Contains(src, fmt.Sprintf("%s = %d", want.name, want.n)) {
			t.Errorf("fk_abi.lua does not set %s to %d", want.name, want.n)
		}
	}
}

// THE OPERATORS DISPATCHING FOR REAL, through fk_abi.lua under lua52f, against
// Lua objects with the metamethods Factorio's own carry.
//
// The generated-code tests above prove the members exist and are typed; this is
// the half that says the ABI does the right Lua thing with them, which is the
// half RM1 predicted wrongly (it read `obj[key]` as something the GET kind could
// already carry -- it cannot; GET resolves `obj[m.name]`).
func TestClassOperatorsDispatch(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	idx := r.MemberIndex()
	id := func(class, name string, kind int) int {
		v, ok := idx[fmt.Sprintf("%s::%s/%d", class, name, kind)]
		if !ok {
			t.Fatalf("no member id for %s::%s kind %d", class, name, kind)
		}
		return v
	}
	invIdx := id("LuaInventory", "index", MemberIndex)
	invLen := id("LuaInventory", "length", MemberLen)
	ctIdx := id("LuaCustomTable", "index", MemberIndex)
	rngCall := id("LuaRandomGenerator", "call", MemberSelf)

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)

-- An INVENTORY: a sequence, indexed by a 1-based integer, answering #.
-- setmetatable rather than a plain table, because that is what makes the two
-- expressions this test is about metamethod calls rather than table lookups --
-- exactly like the engine's own objects, and exactly what nothing before this
-- could reach.
local slots = { { n = "iron" }, { n = "copper" }, { n = "steel" } }
local inv = setmetatable({ valid = true }, {
  __index = function(_, k) return slots[k] end,
  __len   = function() return #slots end,
})

local function call(h, mid, argp, retp)
  return H.call(H.transient(h), mid, argp, retp)
end

-- inv[2] -- a u32 key in, a handle back.
local m = API.members[%d]
IO.st32(2048 + m.sig.args[1].at, 2)
local st = call(inv, %d, 2048, 4096)
local h = IO.ld32(4096 + m.sig.rets[1].at)
print("index st " .. st .. " name " .. tostring(H.get(h).n))

-- #inv -- no argument at all.
local lm = API.members[%d]
st = call(inv, %d, 0, 4096)
print("length st " .. st .. " n " .. IO.ld32(4096 + lm.sig.rets[1].at))

-- A CUSTOM TABLE, whose key is tier 2 because the class is heterogeneous:
-- force.technologies is keyed by string and game.players by number, and this is
-- the one binding that has to serve both. The values are plain numbers so that
-- what comes BACK is a tier-2 scalar: this stub's objects are Lua tables, and a
-- table crossing as tier 2 is encoded as a MAP, which needs the guest allocator
-- this harness deliberately does not have.
local techs = { automation = 7, [3] = 9 }
local ct = setmetatable({ valid = true }, { __index = function(_, k) return techs[k] end })
local cm = API.members[%d]
local function zero(at, n) for i = 0, n - 1 do IO.st8(at + i, 0) end end

-- A STRING key: tag 3, then (ptr, len) where write_dyn puts them.
for i = 1, #"automation" do IO.st8(1024 + i - 1, ("automation"):byte(i)) end
zero(2048, cm.argsize)
IO.st32(2048 + cm.sig.args[1].at, 3)
IO.st32(2048 + cm.sig.args[1].at + 8, 1024)
IO.st32(2048 + cm.sig.args[1].at + 12, 10)
st = call(ct, %d, 2048, 4096)
print("ct string st " .. st .. " tag " .. IO.ld32(4096 + cm.sig.rets[1].at) ..
      " v " .. IO.ldf64(4096 + cm.sig.rets[1].at + 8))

-- ...and a NUMBER key through the same member, which is the whole reason the
-- key is tier 2 rather than a string or a u32.
zero(2048, cm.argsize)
IO.st32(2048 + cm.sig.args[1].at, 2)
IO.stf64(2048 + cm.sig.args[1].at + 8, 3)
st = call(ct, %d, 2048, 4096)
print("ct number st " .. st .. " tag " .. IO.ld32(4096 + cm.sig.rets[1].at) ..
      " v " .. IO.ldf64(4096 + cm.sig.rets[1].at + 8))

-- THE CALL OPERATOR: the object itself is the callable. Two optional
-- arguments, so this also exercises M.call's trailing-argument trim on a kind
-- that never resolves a member name.
local rng = setmetatable({ valid = true }, {
  __call = function(_, lo, hi)
    if lo == nil then return 0.5 end
    return lo + hi
  end,
})
local rm = API.members[%d]
zero(2048, rm.argsize)
st = call(rng, %d, 2048, 4096)
print("call none st " .. st .. " v " .. IO.ldf64(4096 + rm.sig.rets[1].at))
IO.st8(2048 + rm.sig.args[1].has, 1)
IO.st32(2048 + rm.sig.args[1].at, 10)
IO.st8(2048 + rm.sig.args[2].has, 1)
IO.st32(2048 + rm.sig.args[2].at, 32)
st = call(rng, %d, 2048, 4096)
print("call both st " .. st .. " v " .. IO.ldf64(4096 + rm.sig.rets[1].at))

-- AN OPERATOR THAT RAISES must come back as a status, never as an unwind
-- through the wasm frame -- which is the whole reason each branch is pcall-ed.
local bad = setmetatable({ valid = true }, {
  __index = function() error("Index out of range") end,
  __len = function() error("no length") end,
})
print("raising index st " .. call(bad, %d, 2048, 4096))
print("raising length st " .. call(bad, %d, 0, 4096))
`, invIdx, invIdx, invLen, invLen, ctIdx, ctIdx, ctIdx, rngCall, rngCall, rngCall, invIdx, invLen))

	want := strings.Join([]string{
		"index st 0 name copper",
		"length st 0 n 3",
		// tag 2 is DYN_NUM: a heterogeneous class answers with a tier-2 value
		// on the way back as well as taking one on the way in.
		"ct string st 0 tag 2 v 7",
		"ct number st 0 tag 2 v 9",
		// No arguments present, so M.call trims to zero and the metamethod's
		// own default runs -- which is what a Lua author calling rng() gets.
		"call none st 0 v 0.5",
		"call both st 0 v 42",
		"raising index st 5",
		"raising length st 5",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
