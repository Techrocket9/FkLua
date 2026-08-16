package factorio

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// Coverage numbers live in the census, not here. What this keeps is the
// SHAPE of the report: that a skip is attributed rather than anonymous.
func TestEverySkipHasAReason(t *testing.T) {
	r := GenerateMembers(loadTestAPI(t))
	total := 0
	for _, n := range r.Reasons {
		total += n
	}
	if total != len(r.Skipped) {
		t.Errorf("%d skips but %d attributed; a skip with no reason is one nobody "+
			"can act on", len(r.Skipped), total)
	}
	for _, s := range r.Skipped {
		if s.Reason == "" || s.Name == "" {
			t.Errorf("skip with no reason or name: %+v", s)
		}
	}
	if r.Reasons["other"] > 0 {
		t.Errorf("%d skips fell into \"other\"; classify() should name them",
			r.Reasons["other"])
	}
}

// Member ids are dense and 1-based, matching the Lua table they index.
func TestMemberIDsAreDenseAndOneBased(t *testing.T) {
	r := GenerateMembers(loadTestAPI(t))
	for i, m := range r.Members {
		if m.ID != i+1 {
			t.Fatalf("member %d has id %d", i, m.ID)
		}
	}
}

// Generation is deterministic. The member table ships with the mod and the
// guest was compiled against these ids, so two runs producing different
// numbering would mean a rebuild silently invalidates a guest.
func TestGenerationIsDeterministic(t *testing.T) {
	a := loadTestAPI(t)
	x, y := GenerateMembers(a), GenerateMembers(a)
	if len(x.Members) != len(y.Members) {
		t.Fatalf("%d vs %d members", len(x.Members), len(y.Members))
	}
	for i := range x.Members {
		if x.Members[i].Class != y.Members[i].Class ||
			x.Members[i].Name != y.Members[i].Name ||
			x.Members[i].Kind != y.Members[i].Kind {
			t.Fatalf("member %d differs: %v vs %v", i, x.Members[i], y.Members[i])
		}
	}
}

func findMember(r Report, class, name string, kind int) (Member, bool) {
	for _, m := range r.Members {
		if m.Class == class && m.Name == name && m.Kind == kind {
			return m, true
		}
	}
	return Member{}, false
}

// The two union families the generator canonicalises, checked on the concepts
// that actually drive the numbers rather than on invented examples.
func TestCanonicalUnions(t *testing.T) {
	r := GenerateMembers(loadTestAPI(t))

	// A. table + tuple shorthand -> the table. LuaControl::position is a
	// MapPosition: table{x,y} | tuple[double,double].
	//
	// LuaControl, not LuaEntity, and the difference matters: CLASSES INHERIT,
	// and an inherited member appears in neither the child's method list nor
	// its attribute list. LuaEntity gets `position` from LuaControl and has no
	// entry of its own. Dispatch does not care -- it is name-based and the
	// handle decides the object -- so one entry per DECLARING class is enough,
	// and the guest bindings for a subclass reference the parent's ids.
	pos, ok := findMember(r, "LuaControl", "position", MemberGet)
	if !ok {
		t.Fatal("LuaControl::position was skipped; MapPosition should canonicalise")
	}
	f := pos.Rets[0]
	if f.Kind != KindStruct {
		t.Fatalf("position is %v, want a struct", f.Kind)
	}
	var names []string
	for _, x := range f.Struct {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "x" || names[1] != "y" {
		t.Errorf("position fields = %v, want x and y", names)
	}

	// B. class + scalar identifiers -> the handle. LuaEntity::damage takes a
	// ForceID: string | uint8 | LuaForce.
	dmg, ok := findMember(r, "LuaEntity", "damage", MemberCall)
	if !ok {
		t.Fatal("LuaEntity::damage was skipped; ForceID should canonicalise")
	}
	var sawHandle bool
	for _, a := range dmg.Args {
		if a.Name == "force" {
			sawHandle = a.Kind == KindHandle
		}
	}
	if !sawHandle {
		t.Errorf("damage's force parameter should be a handle; args = %v", dmg.Args)
	}
}

// A recursive type is carried by tier 2 rather than refused. LocalisedString is
// defined in terms of itself, so no FIXED layout holds it -- but a tagged value
// does, because the tag says what each level actually contains.
func TestARecursiveTypeBecomesADynamicValue(t *testing.T) {
	r := GenerateMembers(loadTestAPI(t))
	m, ok := findMember(r, "LuaGuiElement", "caption", MemberGet)
	if !ok {
		t.Fatal("LuaGuiElement::caption is a LocalisedString and tier 2 should carry it")
	}
	if m.Rets[0].Kind != KindDyn {
		t.Errorf("caption is %v, want a dynamic value", m.Rets[0].Kind)
	}
	// And nothing is attributed to tier 2 any more, because nothing is refused
	// for being a union.
	if n := r.Reasons["union or recursive type (tier 2)"]; n != 0 {
		t.Errorf("%d members still refused as unions", n)
	}
}

// A member with ONE unexpressible field is skipped ENTIRELY. A struct silently
// missing a field is a wrong value the guest cannot detect; a struct that does
// not exist is a missing binding it can see.
func TestAnUnexpressibleFieldSkipsTheWholeMember(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Concepts: []Concept{
			{Name: "Weird", Type: Type{Complex: "function"}},
		},
		Classes: []Class{{
			Name: "LuaThing",
			Methods: []Method{{
				Name:       "配",
				Parameters: []Parameter{{Name: "ok", Type: Type{Name: "uint32"}}},
			}},
			Attributes: []Attribute{{
				Name: "shape",
				ReadType: &Type{Complex: "table", Parameters: []Parameter{
					{Name: "good", Type: Type{Name: "double"}},
					{Name: "bad", Type: Type{Name: "Weird"}},
				}},
			}},
		}},
	}
	r := GenerateMembers(a)
	if _, ok := findMember(r, "LuaThing", "shape", MemberGet); ok {
		t.Error("a struct with an unexpressible field must not be emitted with " +
			"the field quietly dropped")
	}
	// The rest of the class still generates: one bad member does not poison it.
	if _, ok := findMember(r, "LuaThing", "配", MemberCall); !ok {
		t.Error("the sibling method should still be generated")
	}
}

// A read/write attribute produces TWO entries. That is why the entry count
// exceeds the 3774 members the API documents.
func TestAReadWriteAttributeProducesBothKinds(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Classes: []Class{{
			Name: "LuaThing",
			Attributes: []Attribute{
				{Name: "rw", ReadType: &Type{Name: "double"}, WriteType: &Type{Name: "double"}},
				{Name: "ro", ReadType: &Type{Name: "double"}},
			},
		}},
	}
	r := GenerateMembers(a)
	if len(r.Members) != 3 {
		t.Fatalf("got %d members, want 3 (rw get, rw set, ro get)", len(r.Members))
	}
	if _, ok := findMember(r, "LuaThing", "rw", MemberSet); !ok {
		t.Error("the writable attribute needs a SET entry")
	}
	if _, ok := findMember(r, "LuaThing", "ro", MemberSet); ok {
		t.Error("a read-only attribute must not get a SET entry")
	}
}

// Every generated signature has to survive layout, or the member table would
// carry entries the codec cannot place. This is the join between the two halves.
func TestEveryGeneratedSignatureLaysOut(t *testing.T) {
	r := GenerateMembers(loadTestAPI(t))
	for _, m := range r.Members {
		if _, err := LayoutStruct(named(m.Args, "a")); err != nil {
			t.Fatalf("%s::%s args do not lay out: %v", m.Class, m.Name, err)
		}
		if _, err := LayoutStruct(named(m.Rets, "r")); err != nil {
			t.Fatalf("%s::%s rets do not lay out: %v", m.Class, m.Name, err)
		}
	}
}

// named fills in any blank field name so a positional list can go through the
// named-field layout.
func named(fs []FieldSpec, prefix string) []FieldSpec {
	out := append([]FieldSpec(nil), fs...)
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = prefix + string(rune('0'+i%10))
		}
	}
	return out
}

// The whole chain, end to end: runtime-api.json -> generator -> Lua source ->
// lua52f -> bind_members -> a dispatched call against a stub object.
//
// Everything before this was tested against hand-written descriptors. This is
// the first point at which the descriptors come from the real API.
func TestGeneratedTableLoadsAndDispatches(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}

	idx := r.MemberIndex()
	// A boolean attribute read, and a method taking a u32 and returning a bool.
	//
	// SKIPPED rather than failed if a future API drops either: this test is
	// about the chain, not about these two members, and a member disappearing
	// is the census's news to report.
	getID, ok := idx["LuaAISettings::allow_destroy_when_commands_fail/1"]
	if !ok {
		t.Skip("this API has no LuaAISettings::allow_destroy_when_commands_fail")
	}
	callID, ok := idx["LuaConstantCombinatorControlBehavior::remove_section/0"]
	if !ok {
		t.Skip("this API has no LuaConstantCombinatorControlBehavior::remove_section")
	}

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
print("version " .. API.application_version .. " api " .. API.api_version)
local n = 0
for _ in pairs(API.members) do n = n + 1 end
print("members " .. n)

H.bind_members(API.members)

-- A boolean attribute, read off a stub.
local settings = { valid = true, allow_destroy_when_commands_fail = true }
local g = API.members[%d]
print("get retsize " .. g.retsize)
print("get status " .. H.call(H.transient(settings), %d, 0, 4096))
print("get value " .. tostring(IO.ld8(4096) ~= 0))

-- A method: one u32 in, one boolean out.
local seen
local behaviour = { valid = true,
  remove_section = function(i) seen = i return i == 3 end }
local c = API.members[%d]
print("call argsize " .. c.argsize .. " retsize " .. c.retsize)
IO.st32(2048 + c.sig.args[1].at, 3)
print("call status " .. H.call(H.transient(behaviour), %d, 2048, 4096))
print("guest passed " .. tostring(seen) .. ", got " .. tostring(IO.ld8(4096) ~= 0))
`, getID, getID, callID, callID))

	want := fmt.Sprintf("version %s api %d\nmembers %d\n",
		a.ApplicationVersion, a.APIVersion, len(r.Members)) +
		"get retsize 1\n" +
		"get status 0\n" +
		"get value true\n" +
		"call argsize 4 retsize 1\n" +
		"call status 0\n" +
		"guest passed 3, got true"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The generated source has to be parseable Lua before anything else is true of
// it. About a megabyte of it, which against the day-0 probe's measured 4 MB in
// 106 ms is a few tens of ms of load -- worth knowing, not worth designing
// around, and not worth a literal that moves with the pin.
func TestGeneratedSourceIsWellFormedLua(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(src) > 2<<20 {
		t.Errorf("generated table is %d bytes; the chunk budget deserves a look", len(src))
	}
	got := runMarshalWithFile(t, "fk_api_gen.lua", src, `
local API = require("fk_api_gen")
-- Every entry has to carry what a guest and the codec both need.
local bad = 0
for id, m in pairs(API.members) do
  if type(m.kind) ~= "number" or type(m.name) ~= "string"
     or type(m.argsize) ~= "number" or type(m.retsize) ~= "number"
     or type(m.sig) ~= "table" or type(m.sig.args) ~= "table"
     or type(m.sig.rets) ~= "table" then
    bad = bad + 1
  end
end
print("malformed " .. bad)
`)
	if got != "malformed 0" {
		t.Errorf("got %q", got)
	}
	t.Logf("fk_api_gen.lua: %d bytes, %d members", len(src), len(r.Members))
}

// The wiring, end to end: a guest that imports fk.call, packaged with a pruned
// member table, driven through its own control.lua the way Factorio drives it.
//
// Everything before this tested a piece. This is the first test where a guest
// reaches a game object by calling the import, through the real control.lua,
// with the real generated table.
func TestAPackagedGuestReachesTheAPI(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)

	// LuaGameScript::speed is a float, readable and writable. Handle 2 is
	// `game` -- second in the fixed 1..9 block, alphabetically.
	getID := full.MemberIndex()["LuaGameScript::speed/1"]
	setID := full.MemberIndex()["LuaGameScript::speed/2"]
	if getID == 0 || setID == 0 {
		t.Fatal("expected GET and SET entries for LuaGameScript::speed")
	}

	// A guest that reads game.speed, doubles it, and writes it back.
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(memory 1)
		(func (export "fk_on_tick") (param i32)
			(drop (call $call (i32.const 2) (i32.const %d) (i32.const 0) (i32.const 1024)))
			(f32.store (i32.const 2048)
				(f32.mul (f32.load (i32.const 1024)) (f32.const 2)))
			(drop (call $call (i32.const 2) (i32.const %d) (i32.const 2048) (i32.const 0)))))`,
		getID, setID)

	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	if !complete || len(used) != 2 {
		t.Fatalf("scan found %v (complete=%v); want the two constant ids", used, complete)
	}
	pruned := full.Only(used)
	apiSrc, err := pruned.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2, Persist: luagen.PersistNone})
	if err != nil {
		t.Fatal(err)
	}

	pkg := &Package{
		Info: Info{Name: "fk-api", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk, Exports: []string{"fk_on_tick"}, APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Drive it the way Factorio does, with a stub `game` in _G.
	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-api",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
game = { speed = 1.5 }

require("control")
handlers[1]({ tick = 1 })
print("speed " .. game.speed)
handlers[1]({ tick = 2 })
print("speed " .. game.speed)
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// 1.5 doubled twice, through a float round trip each time.
	if want := "speed 3\nspeed 6"; strings.TrimSpace(out) != want {
		t.Errorf("got %q, want %q", strings.TrimSpace(out), want)
	}

	// And the mod carries two members, not the whole table.
	if n := len(apiSrc); n > 2000 {
		t.Errorf("the pruned table is %d bytes for two members", n)
	}
	t.Logf("pruned member table: %d bytes for %d members", len(apiSrc), len(pruned.Members))
}

// THE HOST-SIDE STRING PREDICATE, exercised rather than reasoned about.
//
// MemberGetEq reads a string attribute and compares it against bytes still
// sitting in guest memory. Two things about it can only be checked by running
// it, and the second is the one a length fast path can silently break:
//
//   - a MATCH has to decode the guest's string and compare it;
//   - a string of the SAME LENGTH that differs has to come back false, which is
//     exactly the case a length check cannot answer and would skip past if the
//     code fell through wrongly.
//
// The rest of the table is the boring half and is here because each row was a
// way to get it wrong: a different length, an empty want, and an attribute that
// is not a string at all.
func TestAStringPredicateComparesHostSide(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	eqID, ok := r.MemberIndex()[fmt.Sprintf("LuaEntity::name/%d", MemberGetEq)]
	if !ok {
		t.Skip("this API has no LuaEntity::name, or it stopped being a plain string")
	}

	// Every case writes its `want` at 1024 and reads the answer at 4096. The
	// argument's offset comes from the signature rather than from a 0 here, for
	// the same reason call_eq reads it there.
	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)
local m = API.members[%d]
print("kind " .. m.kind .. " argsize " .. m.argsize .. " retsize " .. m.retsize)

local function ask(name, want)
  for i = 1, #want do IO.st8(1024 + i - 1, want:byte(i)) end
  IO.st32(2048 + m.sig.args[1].at, 1024)
  IO.st32(2048 + m.sig.args[1].at + 4, #want)
  IO.st8(4096, 255)
  local st = H.call(H.transient({ valid = true, name = name }), %d, 2048, 4096)
  if st ~= 0 then return "status " .. st end
  return tostring(IO.ld8(4096) ~= 0)
end

print("equal            " .. ask("transport-belt", "transport-belt"))
print("same length      " .. ask("transport-belt", "transport-bolt"))
print("longer want      " .. ask("transport-belt", "transport-belts"))
print("shorter want     " .. ask("transport-belt", "transport-bel"))
print("empty want       " .. ask("transport-belt", ""))
print("empty both       " .. ask("", ""))
print("not a string     " .. ask(17, "17"))
`, eqID, eqID))

	want := "kind 3 argsize 8 retsize 1\n" +
		"equal            true\n" +
		"same length      false\n" +
		"longer want      false\n" +
		"shorter want     false\n" +
		"empty want       false\n" +
		"empty both       true\n" +
		// A number where the API promised a string means the running Factorio
		// disagrees with the description this mod was built against. "No" is
		// the honest answer to "is this string equal to that one"; coercing
		// would make `17 == "17"` true and hide the disagreement.
		"not a string     false"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
