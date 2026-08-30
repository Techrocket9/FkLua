package factorio

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// An attribute's WRITE half when its type is a union with a class in it.
//
// `LuaGuiElement::style` is declared `LuaStyle | string` on the write side and
// the ENGINE ACCEPTS ONLY THE STRING. canonicalUnion's shape B -- one class plus
// scalar identifiers -- collapsed it to the handle arm, so the generated setter
// took an `Object` and no string could be expressed: a member that exists and
// cannot be used. Four of the thirteen mods the temptations survey audited
// restyle at runtime, one of them at 31 sites; what kept it hidden is that
// `style` can also be set at creation time inside `add`'s option table.
//
// The rule is over the SHAPE and not over a name -- see mapWriteType. These
// tests are the two halves of that: exactly which attributes the shape reaches,
// and that a string really crosses on the one it does.

// writableUnionsWithAClass is what the description contains, at every pin.
//
// AN EQUALITY OVER EVERY COMMITTED DESCRIPTION rather than a check at the pin.
// The claim the fix rests on is that the shape is RARE and that both instances
// of it are accounted for, so a description that grows a third writable
// union-with-a-class is a number that moves here rather than a member quietly
// classified by a rule nobody re-read. `LuaControl::opened` names eight classes,
// so nHandle > 1 disqualifies shape B and it was already tier 2; `style` names
// one, which is the whole difference between them.
var writableUnionsWithAClass = []string{
	"LuaControl::opened",
	"LuaGuiElement::style",
}

func TestOnlyTwoWritableAttributesAreAUnionWithAClass(t *testing.T) {
	for _, v := range committedVersions(t) {
		a := loadShapeAPI(t, v)
		m := newTypeMapper(a)
		var got []string
		for _, c := range a.Classes {
			for _, at := range c.Attributes {
				if at.WriteType == nil {
					continue
				}
				if unionNamesAClass(a, *at.WriteType) {
					got = append(got, c.Name+"::"+at.Name)
				}
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(writableUnionsWithAClass, ",") {
			t.Errorf("%s: writable union-with-a-class attributes are %v, want %v. "+
				"A new one wants mapWriteType read before this list is edited",
				v, got, writableUnionsWithAClass)
		}
		// ...and both of them cross as tier 2 on the write side, which is the
		// property the rule exists to produce.
		for _, name := range got {
			parts := strings.SplitN(name, "::", 2)
			for _, c := range a.Classes {
				if c.Name != parts[0] {
					continue
				}
				for _, at := range c.Attributes {
					if at.Name != parts[1] {
						continue
					}
					f, err := m.mapWriteType(*at.WriteType)
					if err != nil {
						t.Errorf("%s: %s write half: %v", v, name, err)
						continue
					}
					if f.Kind != KindDyn {
						t.Errorf("%s: %s writes as kind %d, want tier 2 (%d): a "+
							"union the guest chooses an arm of must carry every arm",
							v, name, f.Kind, KindDyn)
					}
				}
			}
		}
	}
}

// unionNamesAClass reports a type that is a union with at least one class arm.
// Deliberately independent of canonicalUnion: this is the POPULATION the rule
// could reach, and deriving it from the rule would make the test agree with the
// code by construction.
func unionNamesAClass(a *API, t Type) bool {
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if t.Complex != "union" {
		return false
	}
	classes := map[string]bool{}
	for _, c := range a.Classes {
		classes[c.Name] = true
	}
	for _, o := range t.Options {
		for o.Complex == "type" && o.Value != nil {
			o = *o.Value
		}
		if classes[o.Name] {
			return true
		}
	}
	return false
}

// The READ half is untouched, and the setter really takes a Value in both
// languages.
//
// The read is where shape B is right: the engine returns the object, so
// collapsing there costs a guest nothing and is what every existing guest calls.
// Changing both sides would have been the easy mistake.
func TestTheStyleSetterTakesAValueAndTheGetterStillAHandle(t *testing.T) {
	a, r, g, rb := genBoth(t)

	var get, set Member
	for _, m := range r.Members {
		if m.Class != "LuaGuiElement" || m.Name != "style" {
			continue
		}
		switch m.Kind {
		case MemberGet:
			get = m
		case MemberSet:
			set = m
		}
	}
	if get.Class == "" || set.Class == "" {
		t.Fatal("LuaGuiElement::style has no get/set pair in the member table")
	}
	if get.Rets[0].Kind != KindHandle {
		t.Errorf("the style GETTER returns kind %d, want a handle (%d)",
			get.Rets[0].Kind, KindHandle)
	}
	if set.Args[0].Kind != KindDyn {
		t.Errorf("the style SETTER takes kind %d, want tier 2 (%d)",
			set.Args[0].Kind, KindDyn)
	}

	for _, b := range []struct{ lang, want, reject string }{
		{"go", "func (o LuaGuiElement) SetStyle(value Value) error {",
			"func (o LuaGuiElement) SetStyle(value Object) error {"},
		{"rust", "pub fn set_style(&self, value: &Value) -> Result<(), Status> {",
			"pub fn set_style(&self, value: Object) -> Result<(), Status> {"},
	} {
		src := g.Source
		if b.lang == "rust" {
			src = rb.Source
		}
		if strings.Contains(src, b.reject) {
			t.Errorf("%s: the style setter still takes a handle, which no string "+
				"can reach", b.lang)
		}
		if !strings.Contains(src, b.want) {
			t.Errorf("%s: the generated bindings have no %q", b.lang, b.want)
		}
	}
	_ = a
}

// ...AND A STRING REALLY CROSSES, through the real fk_abi.lua under lua52f.
//
// The generator tests above say the member is typed; this is the half that says
// the engine's own gesture -- `element.style = "frame"` -- reaches the object as
// a Lua STRING rather than as a handle, a table or nothing. Two legs, because
// the point of tier 2 is that BOTH arms survive: a string, which is what the
// engine accepts, and a handle, which the union also declares and which a
// collapse-to-string would have broken in the other direction.
func TestAStyleNameCrossesOnTheWriteHalf(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := r.MemberIndex()["LuaGuiElement::style/"+fmt.Sprint(MemberSet)]
	if !ok {
		t.Fatal("no member id for LuaGuiElement::style set")
	}

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, fmt.Sprintf(`
local API = require("fk_api_gen")
H.bind_members(API.members)
local function put(at, s) for i = 1, #s do IO.st8(at + i - 1, s:byte(i)) end end
local function zero(at, n) for i = 0, n - 1 do IO.st8(at + i, 0) end end

local seen
local el = setmetatable({ valid = true }, {
  __newindex = function(_, k, v) if k == "style" then seen = v end end,
})
local sm = API.members[%d]

-- element.style = "frame_button"
put(1024, "frame_button")
zero(2048, sm.argsize)
local va = 2048 + sm.sig.args[1].at
IO.st32(va, 3) IO.st32(va + 8, 1024) IO.st32(va + 12, 12)
local st = H.call(H.transient(el), %d, 2048, 0)
print("string st " .. st .. " type " .. type(seen) .. " value " .. tostring(seen))

-- ...and the HANDLE arm the union also declares, which a collapse to string
-- would have broken the other way round.
local style = setmetatable({ valid = true, object_name = "LuaStyle" }, {})
local hs = H.transient(style)
zero(2048, sm.argsize)
IO.st32(va, 4) IO.st32(va + 8, hs)
st = H.call(H.transient(el), %d, 2048, 0)
print("handle st " .. st .. " same " .. tostring(seen == style))
`, id, id, id))

	want := []string{
		"string st 0 type string value frame_button",
		"handle st 0 same true",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}
