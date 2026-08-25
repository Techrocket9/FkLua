package factorio

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// A `nil`-TYPED FIELD IS OMITTED, AND ITS STRUCT STILL BINDS.
//
// 2.1 gave UtilityConstants a field `frozen_color_lookup` whose type is the
// concept `ColorLookupTable`, which the description declares as `nil` and
// describes as "Does not return the value at runtime.". The mapper refused
// `nil` -- correctly, it has no representation -- and mapFields turns a refused
// field into a refused STRUCT, so the concept went, and with it
// LuaPrototypes::utility_constants: the first attribute-shaped host skip this
// generator had ever produced.
//
// That is AD4 exactly. There, a nested dictionary one field could not express
// took CollisionMask, MapGenSettings and 17 members with it, because a
// FIELD-level problem was answered at CONCEPT level. The lesson is not "always
// drop the field" -- mapFields' rule is right for a field that carries a value
// this layer cannot express, where dropping it would hand the guest a struct
// that is silently wrong. It is that the question belongs at the level the fact
// is at, and a nil type is a fact about one field: there is no value, so
// nothing is lost by leaving it out.
//
// THE FIXTURE IS SYNTHETIC ON PURPOSE. UtilityConstants itself is deferred by
// both binding generators for an unrelated and separately counted reason (a
// dictionary of a dictionary, in `default_trigger_target_mask_by_type`), so it
// cannot show that the struct GENERATES. This carries the same shape -- a
// scalar, the nil field, a scalar after it -- so the offsets prove the omission
// costs no bytes and shifts nothing.
func TestANilTypedFieldIsOmittedAndItsStructStillBinds(t *testing.T) {
	a := nilFieldAPI()
	r := GenerateMembers(a)

	if len(r.Skipped) != 0 {
		t.Fatalf("nothing should be skipped, got %+v", r.Skipped)
	}
	mem, ok := findMember(r, "LuaProtos", "consts", MemberGet)
	if !ok {
		t.Fatal("the attribute returning the nil-carrying concept did not bind; " +
			"one always-absent field must not take its struct down")
	}

	// The struct is the survivors, in order, and nothing else.
	got := mem.Rets[0].Struct
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "before,after" {
		t.Errorf("struct fields are %v, want [before after]", names)
	}

	// THE OMISSION IS RECORDED, with enough to find it in the description. The
	// declared type is the ALIAS -- the field says `ColorLookupTable` and only
	// the concept says `nil` -- and recording the resolved name would send a
	// reader looking for a nil-typed field, which no published version has.
	if len(r.Omitted) != 1 {
		t.Fatalf("got %d omissions, want 1: %+v", len(r.Omitted), r.Omitted)
	}
	want := OmittedField{Owner: "FkConsts", Field: "frozen_color_lookup",
		Type: "ColorLookupTable", Reason: "nil"}
	if r.Omitted[0] != want {
		t.Errorf("omission recorded as %+v, want %+v", r.Omitted[0], want)
	}
	if r.OmittedBy["nil"] != 1 {
		t.Errorf("omissions by reason are %v, want one under \"nil\"", r.OmittedBy)
	}

	// THE LAYOUT IS THE ONE THE FIELD NEVER EXISTED IN. Not merely "the field is
	// gone" -- a reserved hole would also satisfy that, and would cost every
	// guest eight bytes forever to carry a value the description says never
	// arrives.
	blk, err := LayoutStruct(got)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := LayoutStruct([]FieldSpec{
		{Name: "before", Kind: KindF64},
		{Name: "after", Kind: KindU32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if blk.Size != ctrl.Size {
		t.Errorf("struct is %d bytes, want %d -- the omitted field is costing space",
			blk.Size, ctrl.Size)
	}
	lua := map[string]int{}
	for i, f := range blk.Fields {
		if f.Offset != ctrl.Fields[i].Offset {
			t.Errorf("%s is at %d, want %d", f.Name, f.Offset, ctrl.Fields[i].Offset)
		}
		lua[f.Name] = f.Offset
	}

	// BOTH BACKENDS, FROM ONE ANALYSIS. AD5 is the reason this is not two
	// tests: the identical defect was fixed in the Go generator with a test and
	// left standing in the Rust one for two milestones, because the test was
	// written against one backend and a mod author found the other by grepping
	// the committed bindings.
	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	goOff := decodeOffsets(t, g.Source, "func decodeFkConsts(", `v\.(\w+) = .*&d\[(\d+)\]`)
	rsOff := decodeOffsets(t, rb.Source, "pub fn decode_at(", `v\.(\w+) = .*\bd\[(\d+)\.\.`)

	// The field is absent from both, by name in whatever case each renders it.
	for lang, off := range map[string]map[string]int{"go": goOff, "rust": rsOff} {
		for name := range off {
			if strings.EqualFold(strings.ReplaceAll(name, "_", ""), "frozencolorlookup") {
				t.Errorf("%s: the nil-typed field %q reached the generated decoder", lang, name)
			}
		}
		if len(off) != 2 {
			t.Errorf("%s decoder touches %d fields (%v), want 2", lang, len(off), off)
		}
	}

	// AND THE THREE AGREE ON THE WIRE. The Lua layout table is what fk_abi
	// encodes against and the two decoders' offsets are compiled into the
	// guests; a disagreement between them is past every type checker in the
	// system and is exactly what a shifted field would produce.
	// Each backend spells the field its own way -- Go exports `Before`, Rust
	// keeps `before` -- so both are folded to one key before comparing.
	for _, c := range []struct {
		lang string
		off  map[string]int
	}{
		{"go", goOff},
		{"rust", rsOff},
	} {
		for name, at := range c.off {
			luaAt, ok := lua[strings.ToLower(strings.ReplaceAll(name, "_", ""))]
			if !ok {
				t.Errorf("%s decodes a field %q the Lua layout does not have", c.lang, name)
				continue
			}
			if luaAt != at {
				t.Errorf("%s reads %s at %d, fk_abi writes it at %d", c.lang, name, at, luaAt)
			}
		}
	}

	// Finally through the REAL fk_abi.lua, because the offsets above are only
	// numbers until something encodes and decodes at them.
	out := runMarshal(t, fmt.Sprintf(`
local fields = %s
local base = 2048
local function fieldAt(n) for _, f in ipairs(fields) do if f.name == n then return f end end end
IO.stf64(base + fieldAt("before").at, 6.25)
IO.st32(base + fieldAt("after").at, 4242)
local v = H.read_struct(fields, base)
local n = 0
for _ in pairs(v) do n = n + 1 end
print(v.before .. " " .. v.after .. " keys=" .. n)
print("frozen=" .. tostring(v.frozen_color_lookup))
`, blk.LuaTable()))

	// keys=2: the omitted field is not present-and-nil, it is not there at all.
	if want := "6.25 4242 keys=2\nfrozen=nil"; out != want {
		t.Errorf("through fk_abi:\ngot:  %q\nwant: %q", out, want)
	}
}

// THE ALIAS HOP IS WHY THIS WAS INVISIBLE, so it is pinned on its own.
//
// No published version has a field whose declared type is the string "nil": the
// only `nil` in a 2.1.x description is one concept DEFINITION, three hundred
// entries away from the single field that carries it. A check that looked at
// the field's spelling would find nothing and pass forever.
func TestTheNilTypeIsFollowedThroughItsConceptAlias(t *testing.T) {
	m := newTypeMapper(&API{Concepts: []Concept{
		{Name: "ColorLookupTable", Type: Type{Name: "nil"}},
		{Name: "Aliased", Type: Type{Name: "ColorLookupTable"}},
		// The description-only wrapper, which exists purely to hang a comment
		// on a type and must not hide one either.
		{Name: "Described", Type: Type{Complex: "type", Description: "why",
			Value: &Type{Name: "Aliased"}}},
		{Name: "Real", Type: Type{Name: "double"}},
		{Name: "Container", Type: Type{Complex: "array", Value: &Type{Name: "ColorLookupTable"}}},
	}})

	for _, c := range []struct {
		what string
		t    Type
		want bool
	}{
		{"the literal type", Type{Name: "nil"}, true},
		{"a concept declared nil", Type{Name: "ColorLookupTable"}, true},
		{"an alias of that concept", Type{Name: "Aliased"}, true},
		{"a described alias", Type{Name: "Described"}, true},
		{"an ordinary concept", Type{Name: "Real"}, false},
		{"an ordinary builtin", Type{Name: "uint32"}, false},
		{"an unknown name", Type{Name: "NoSuchThing"}, false},
		// A CONTAINER OF NIL IS NOT AN ABSENT VALUE and must not be omitted on
		// this rule. An array still has a count the guest reads, and a nil
		// union arm is Lua spelling optionality, which this ABI carries in a
		// presence byte. Neither shape occurs in any published version; if one
		// appears it arrives as a skip, loudly, rather than as a field that
		// quietly stopped being generated.
		{"an array of nil", Type{Complex: "array", Value: &Type{Name: "nil"}}, false},
		{"a concept that is an array of nil", Type{Name: "Container"}, false},
		{"a union with a nil arm", Type{Complex: "union", Options: []Type{
			{Name: "nil"}, {Name: "double"}}}, false},
	} {
		if got := m.nilTyped(c.t); got != c.want {
			t.Errorf("%s: nilTyped = %v, want %v", c.what, got, c.want)
		}
	}
}

// NIL SOMEWHERE THAT IS NOT A STRUCT FIELD IS STILL A SKIP.
//
// The omission is a decision about one position, taken because a field is the
// only place the description puts a nil today and because a struct can simply
// not have a field. A parameter and a return are different questions -- an
// arity is not optional the way a key is -- and neither occurs. Leaving them as
// skips means that if 2.2 grows one it lands in host_skips_by_reason as a NEW
// reason and shows up in the version diff, which is how a shape nobody has
// reasoned about should arrive.
func TestNilInAPositionThatIsNotAStructFieldIsStillASkip(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Concepts:   []Concept{{Name: "ColorLookupTable", Type: Type{Name: "nil"}}},
		Classes: []Class{{
			Name: "LuaThing",
			Methods: []Method{
				{Name: "takes", Parameters: []Parameter{
					{Name: "p", Type: Type{Name: "ColorLookupTable"}}}},
				{Name: "returns", ReturnValues: []ReturnValue{
					{Type: Type{Name: "ColorLookupTable"}}}},
				{Name: "fine", Parameters: []Parameter{
					{Name: "p", Type: Type{Name: "uint32"}}}},
			},
			// An ATTRIBUTE typed nil outright is also a skip and not an
			// omission: there is no struct to omit it from, and a GET member
			// returning nothing is not a member.
			Attributes: []Attribute{
				{Name: "attr", ReadType: &Type{Name: "ColorLookupTable"}},
			},
		}},
	}
	r := GenerateMembers(a)

	if len(r.Omitted) != 0 {
		t.Errorf("nothing here is a struct field; got omissions %+v", r.Omitted)
	}
	skipped := map[string]bool{}
	for _, s := range r.Skipped {
		skipped[s.Name] = true
		if !strings.Contains(s.Reason, "nil") {
			t.Errorf("%s skipped for %q, want the nil reason preserved", s.Name, s.Reason)
		}
	}
	for _, want := range []string{"takes", "returns", "attr"} {
		if !skipped[want] {
			t.Errorf("%s should still be skipped", want)
		}
	}
	if r.Reasons["nil"] != 3 {
		t.Errorf("skips by reason %v, want 3 under \"nil\"", r.Reasons)
	}
	if _, ok := findMember(r, "LuaThing", "fine", MemberCall); !ok {
		t.Error("the sibling method should still bind")
	}
}

// A STRUCT OF NOTHING BUT NIL FIELDS IS STILL REFUSED, and is not emitted as an
// empty type.
//
// This is AD5's shape, which this repo has already shipped twice: `pub struct
// CollisionMask {}` with a zero-byte codec that compiles, runs, and returns a
// default while sixteen bytes of wire sit unread. Omitting fields makes an
// empty struct newly reachable -- omit every field and the survivors are none
// -- so the existing "no expressible fields" refusal has to be what catches it.
func TestAStructOfNothingButNilFieldsIsStillRefused(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Concepts: []Concept{
			{Name: "ColorLookupTable", Type: Type{Name: "nil"}},
			{Name: "AllAbsent", Type: Type{Complex: "table", Parameters: []Parameter{
				{Name: "a", Order: 0, Type: Type{Name: "ColorLookupTable"}},
				{Name: "b", Order: 1, Type: Type{Name: "ColorLookupTable"}},
			}}},
		},
		Classes: []Class{{Name: "LuaProtos", Attributes: []Attribute{
			{Name: "empty", ReadType: &Type{Name: "AllAbsent"}},
		}}},
	}
	r := GenerateMembers(a)

	if _, ok := findMember(r, "LuaProtos", "empty", MemberGet); ok {
		t.Error("a struct whose every field was omitted must not bind: there is " +
			"nothing for the member to carry")
	}
	if r.Reasons["empty table"] != 1 {
		t.Errorf("skips by reason %v, want one under \"empty table\"", r.Reasons)
	}
	// The omissions are still counted. They are facts about the description's
	// fields and stay true whether or not the struct around them survived --
	// and the skip is counted separately, in its own row, so neither number is
	// standing in for the other.
	if len(r.Omitted) != 2 {
		t.Errorf("got %d omissions, want 2: %+v", len(r.Omitted), r.Omitted)
	}

	// And neither backend emits the name.
	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g.Source, "type AllAbsent struct") {
		t.Error("Go emitted a struct for a concept with no expressible fields")
	}
	if strings.Contains(rb.Source, "pub struct AllAbsent") {
		t.Error("Rust emitted a struct for a concept with no expressible fields")
	}
}

// THE OMISSION IS COUNTED IN THE COMMITTED CENSUS, which is the whole reason it
// is allowed to be silent everywhere else.
//
// Dropping the field is invisible by construction: the member binds, the struct
// generates, both backends agree and every gate is green. The only thing
// standing between that and a description that grows a hundred of these
// unnoticed is a row in the one file a version bump regenerates and a reviewer
// diffs. This repo's own standing rule -- a 0 nobody writes down is how eleven
// class operators stayed invisible for five milestones -- is what it is here
// to satisfy.
//
// AGAINST shapeAPIVersion RATHER THAN THE PIN. `nil` occurs exactly once in
// every published description that has it -- as the concept ColorLookupTable,
// in every 2.1.x one -- and NEVER in 2.0.77, so at the GA pin the honest
// answer is 0 and this test would be asserting that a mechanism it cannot
// reach still works. It deliberately does not compare against a committed
// census.json: it takes its own and reads the row out of it. See
// shapeAPIVersion.
func TestTheOmittedFieldIsCountedInTheCensus(t *testing.T) {
	c, err := TakeCensus(loadShapeAPI(t, shapeAPIVersion))
	if err != nil {
		t.Fatal(err)
	}
	if c.FieldsOmitted != 1 || c.FieldsOmittedBy["nil"] != 1 {
		t.Errorf("census says %d fields omitted, by reason %v; want 1 under \"nil\" "+
			"(UtilityConstants::frozen_color_lookup)", c.FieldsOmitted, c.FieldsOmittedBy)
	}
	// The row moving has to be visible as a DIFF, not just present in the file.
	old := c
	old.FieldsOmitted, old.FieldsOmittedBy = 0, map[string]int{}
	var saw bool
	for _, line := range old.Diff(c) {
		if strings.Contains(line, "fields omitted") || strings.Contains(line, "field omission") {
			saw = true
		}
	}
	if !saw {
		t.Error("a change in the omission count does not appear in the census diff, " +
			"so a description that grows more of them would land unremarked")
	}
}

// AND THE DEFECT ITSELF, against the description that HAS it: the nil concept
// costs no member any more.
//
// shapeAPIVersion rather than the pin, and this is the test that makes the
// reason unmistakable -- 2.0.77 contains no `nil` type anywhere, so every
// assertion below would hold there by vacuity. A gate that cannot fail is worse
// than one nobody added. See shapeAPIVersion.
//
// LuaPrototypes::utility_constants is the attribute that went, and it went
// because one field of the concept it returns is typed `nil`. It is back in the
// host member table and the "nil" skip reason is gone. What it is NOT is bound
// in the two guest backends -- UtilityConstants also carries
// `default_trigger_target_mask_by_type`, a dictionary of a dictionary, which is
// a real and separately counted limitation of both generators. That is the
// honest state and the test says so rather than asserting the stronger claim:
// the member moved from a skip nobody could reach to a deferral with a name
// that already has seven other instances, and it comes along free on the day
// that shape is built.
func TestTheNilConceptNoLongerCostsAnAttribute(t *testing.T) {
	a := loadShapeAPI(t, shapeAPIVersion)
	r := GenerateMembers(a)

	if _, ok := findMember(r, "LuaPrototypes", "utility_constants", MemberGet); !ok {
		t.Error("LuaPrototypes::utility_constants is still missing from the member table")
	}
	if n := r.Reasons["nil"]; n != 0 {
		t.Errorf("%d members still skipped for nil; the reason should be gone", n)
	}
	for _, s := range r.Skipped {
		if strings.Contains(s.Reason, "nil") {
			t.Errorf("%s::%s is still skipped for %q", s.Class, s.Name, s.Reason)
		}
	}
	// Every remaining skip is a METHOD, which is what it was before 2.1 added
	// this one and what the reconciliation test's identity depends on.
	byName := map[string]bool{}
	for _, c := range a.Classes {
		for _, m := range c.Methods {
			byName[c.Name+"::"+m.Name] = true
		}
	}
	for _, s := range r.Skipped {
		if !byName[s.Class+"::"+s.Name] {
			t.Errorf("%s::%s is a skip that is not a method (%s)", s.Class, s.Name, s.Reason)
		}
	}
	// The omission is where the cost went, and it is one field rather than a
	// concept, an attribute and a member.
	if len(r.Omitted) != 1 || r.Omitted[0].Owner != "UtilityConstants" ||
		r.Omitted[0].Field != "frozen_color_lookup" {
		t.Errorf("omissions are %+v, want just UtilityConstants::frozen_color_lookup",
			r.Omitted)
	}
}

// nilFieldAPI is the 2.1 shape in miniature: a concept declared `nil`, and a
// struct that carries it between two ordinary fields so the offsets say whether
// the omission left a hole.
func nilFieldAPI() *API {
	return &API{
		APIVersion: 6,
		Concepts: []Concept{
			{Name: "ColorLookupTable", Type: Type{Name: "nil"},
				Description: "Does not return the value at runtime."},
			{Name: "FkConsts", Type: Type{Complex: "table", Parameters: []Parameter{
				{Name: "before", Order: 0, Type: Type{Name: "double"}},
				{Name: "frozen_color_lookup", Order: 1, Type: Type{Name: "ColorLookupTable"}},
				{Name: "after", Order: 2, Type: Type{Name: "uint32"}},
			}}},
		},
		Classes: []Class{{Name: "LuaProtos", Attributes: []Attribute{
			{Name: "consts", ReadType: &Type{Name: "FkConsts"}},
		}}},
	}
}

// decodeOffsets pulls field -> byte offset out of a generated decoder body.
//
// Reading the emitted SOURCE rather than the FieldSpec it came from is the
// point: the specs are shared, so a test over them would pass with either
// backend emitting anything at all.
func decodeOffsets(t *testing.T, src, marker, pattern string) map[string]int {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("no %q in the generated source", marker)
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	out := map[string]int{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(body, -1) {
		var at int
		fmt.Sscanf(m[2], "%d", &at)
		out[m[1]] = at
	}
	if len(out) == 0 {
		t.Fatalf("no field offsets matched %q in:\n%s", pattern, body)
	}
	return out
}
