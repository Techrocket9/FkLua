package factorio

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// A NESTED DICTIONARY CROSSES, AND THE THREE ENDS AGREE ON EVERY BYTE.
//
// The fixture is `UtilityConstants::default_trigger_target_mask_by_type` in
// miniature -- `dictionary[string -> dictionary[string -> boolean]]` between two
// ordinary fields -- because that one field is what deferred
// `LuaPrototypes::utility_constants` in both backends after the nil-field fix
// moved it out of the host skip list. Seven more members had the same shape and
// eight more had `dictionary of an array`.
//
// ONE TEST OVER BOTH BACKENDS, which is AD5's rule: the identical defect was
// fixed in the Go generator with a test and left standing in the Rust one for
// two milestones because the test was written against one backend, and a mod
// author found it by grepping the committed bindings.
//
// WHAT IT PINS THAT NO COMPILER CHECKS is the STRIDE AND THE OFFSETS AT EACH
// LEVEL. Both are computed in Go, written into the Lua descriptor, and COMPILED
// INTO both guests as literals; nothing at build time relates the three. The
// inner pair is the interesting one -- a string key is (ptr, len) aligned to 4,
// not to 8, so a `bool` value sits at 8 and the pair pads to 12 rather than to
// 9 or 16. That is the (dyn, handle) stride-24 lesson met one level down: a
// decoder using the value's own width as the stride reads the next pair's key
// as a boolean and is wrong from the second entry onward.
func TestANestedDictionaryCrossesInsideAStruct(t *testing.T) {
	a := nestedDictAPI()
	r := GenerateMembers(a)
	if len(r.Skipped) != 0 {
		t.Fatalf("nothing should be skipped, got %+v", r.Skipped)
	}
	mem, ok := findMember(r, "LuaProtos", "consts", MemberGet)
	if !ok {
		t.Fatal("the attribute returning the nested-dictionary concept did not bind")
	}
	blk, err := LayoutStruct(mem.Rets[0].Struct)
	if err != nil {
		t.Fatal(err)
	}

	// THE LAYOUT, stated as literals rather than recomputed, because a test that
	// derives its expectation the way the code does asserts nothing.
	var outer Placed
	for _, f := range blk.Fields {
		if f.Name == "default_trigger_target_mask_by_type" {
			outer = f
		}
	}
	if outer.Kind != KindDict {
		t.Fatalf("the nested field is %v, not a dictionary", outer.Kind)
	}
	inner := outer.Elem
	if inner == nil || inner.Kind != KindDict {
		t.Fatalf("the dictionary's VALUE is not a dictionary: %+v", inner)
	}
	for _, c := range []struct {
		what string
		got  int
		want int
	}{
		// The outer pair: a (ptr, len) string key at 0, an (ptr, count) value at
		// 8, both 4-aligned, so 16 with no padding.
		{"outer stride", outer.Stride, 16},
		{"outer key offset", outer.Key.Offset, 0},
		{"outer value offset", inner.Offset, 8},
		// The inner pair: string key at 0, bool at 8, block aligned to 4 -- so
		// the pair is TWELVE bytes, which is the number a hand-written decoder
		// gets wrong.
		{"inner stride", inner.Stride, 12},
		{"inner key offset", inner.Key.Offset, 0},
		{"inner value offset", inner.Elem.Offset, 8},
	} {
		if c.got != c.want {
			t.Errorf("%s is %d, want %d", c.what, c.got, c.want)
		}
	}

	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}

	// THE INNER CODEC, in both languages, with the layout it walks read out of
	// the emitted source. `[]EntryStringBool` in Go and `BTreeMap<LuaStr, bool>`
	// in Rust: the same wire, each language's own container, both deterministic
	// in iteration order -- a slice because the guest chose it, a BTreeMap
	// because it is sorted.
	goInner := scrape(t, g.Source, "func decCtnSliceEntryStringBool(", map[string]string{
		"stride": `uintptr\(i\)\*(\d+)`,
		"key":    `Key: getStr\(&d\[(\d+)\]\)`,
		"value":  `Val: \*\(\*bool\)\(unsafe\.Pointer\(&d\[(\d+)\]\)\)`,
	})
	rsInner := scrape(t, rb.Source, "pub fn dec_ctn_map_luastr_bool(", map[string]string{
		"stride": `i \* (\d+)`,
		"key":    `get_str\(&s\[\.\.\], (\d+)\)`,
		"value":  `s\[(\d+)\] != 0`,
	})
	// AND THE OUTER WALK, which is where each backend calls the inner codec --
	// so this also proves the recursion is wired rather than merely generated.
	goOuter := scrape(t, g.Source, "func decodeFkConsts(", map[string]string{
		"stride": `uintptr\(i\)\*(\d+)`,
		"key":    `Key: getStr\(&e\[(\d+)\]\)`,
		"value":  `Val: decCtnSliceEntryStringBool\(&e\[(\d+)\]\)`,
	})
	rsOuter := scrape(t, rb.Source, "pub fn decode_at(", map[string]string{
		"stride": `i \* (\d+)`,
		"key":    `get_str\(&s\[\.\.\], (\d+)\)`,
		"value":  `dec_ctn_map_luastr_bool\(&s\[(\d+)\.\.\]\)`,
	})

	for _, c := range []struct {
		what string
		lua  map[string]int
		go_  map[string]int
		rs   map[string]int
	}{
		{"inner",
			map[string]int{"stride": inner.Stride, "key": inner.Key.Offset,
				"value": inner.Elem.Offset}, goInner, rsInner},
		{"outer",
			map[string]int{"stride": outer.Stride, "key": outer.Key.Offset,
				"value": inner.Offset}, goOuter, rsOuter},
	} {
		for k, want := range c.lua {
			if c.go_[k] != want {
				t.Errorf("%s %s: fk_abi %d, Go %d", c.what, k, want, c.go_[k])
			}
			if c.rs[k] != want {
				t.Errorf("%s %s: fk_abi %d, Rust %d", c.what, k, want, c.rs[k])
			}
		}
	}

	// AND THROUGH THE REAL fk_abi.lua, because the numbers above are numbers
	// until something encodes and decodes at them. Written with write_struct and
	// read back with read_struct, so both directions of the recursion run.
	out := runMarshal(t, fmt.Sprintf(`
local fields = %s
local base, next_ = 2048, 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)
local st = H.write_struct(fields, base, {
  before = 1.5,
  default_trigger_target_mask_by_type = {
    ["car"]   = { ground = true,  air = false },
    ["plane"] = { air = true },
  },
  after = 77,
})
local v = H.read_struct(fields, base)
local m = v.default_trigger_target_mask_by_type
local n = 0
for _ in pairs(m) do n = n + 1 end
print(st .. " " .. v.before .. " " .. v.after .. " groups=" .. n ..
  " car.ground=" .. tostring(m.car.ground) ..
  " car.air=" .. tostring(m.car.air) ..
  " plane.air=" .. tostring(m.plane.air) ..
  " plane.ground=" .. tostring(m.plane.ground))
`, blk.LuaTable()))

	want := "0 1.5 77 groups=2 car.ground=true car.air=false plane.air=true plane.ground=nil"
	if out != want {
		t.Errorf("through fk_abi:\ngot:  %q\nwant: %q", out, want)
	}
}

// A NESTED ARRAY AND A DICTIONARY OF ARRAYS, the other two shapes, pinned for
// their strides in both backends.
//
// `LuaEntity::fluidbox_neighbours` is the array of arrays and
// `LuaForce::logistic_networks` the dictionary of arrays; between them they were
// 8 of the 18 remaining deferrals. The recursion that binds a dictionary of
// dictionaries covers both without a branch of its own, which is worth asserting
// rather than assuming -- "the same recursion covers it" is a claim about
// goElemType being reached from three call sites, not a fact about the wire.
//
// AGAINST shapeAPIVersion RATHER THAN THE PIN: three of these four members are
// 2.1 shapes -- LuaEntity::fluidbox_neighbours and LuaPlayer::get_alerts do not
// exist in 2.0.77 at all -- and what is under test is whether the generators
// can express the shape, not what the default description happens to contain.
// See shapeAPIVersion.
func TestANestedArrayAndADictionaryOfArraysCross(t *testing.T) {
	a := loadShapeAPI(t, shapeAPIVersion)
	r := GenerateMembers(a)
	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ what, goSig, rsSig string }{
		{"an array of arrays",
			"func (o LuaEntity) FluidboxNeighbours() ([][]Object, error)",
			"pub fn fluidbox_neighbours(&self) -> Result<Vec<Vec<Object>>, Status>"},
		{"a dictionary of arrays",
			"func (o LuaForce) LogisticNetworks() ([]EntryStringSliceObject, error)",
			"pub fn logistic_networks(&self) -> Result<BTreeMap<LuaStr, Vec<Object>>, Status>"},
		{"a dictionary of dictionaries",
			"func (o LuaRemote) Interfaces() ([]EntryStringSliceEntryStringBool, error)",
			"pub fn interfaces(&self) -> Result<BTreeMap<LuaStr, BTreeMap<LuaStr, bool>>, Status>"},
		{"three levels: a dictionary of dictionaries of arrays of structs",
			"func (o LuaPlayer) GetAlerts(filter AlertFilter) ([]EntryUint32SliceEntryUint32SliceAlert, error)",
			"pub fn get_alerts(&self, filter: AlertFilter) -> Result<BTreeMap<u32, BTreeMap<u32, Vec<Alert>>>, Status>"},
	} {
		if !strings.Contains(g.Source, c.goSig) {
			t.Errorf("%s: the Go bindings do not carry\n\t%s", c.what, c.goSig)
		}
		if !strings.Contains(rb.Source, c.rsSig) {
			t.Errorf("%s: the Rust bindings do not carry\n\t%s", c.what, c.rsSig)
		}
	}
}

// UTILITY_CONSTANTS BINDS, and a leaf of the nested dictionary is reachable.
//
// This is the member the nil-field fix moved from "an attribute-shaped host
// skip nobody could reach" to "a deferral with a name", on the promise that it
// would arrive free the day the nested shape was built. It arrived; this is the
// day, and this asserts it in the form the promise was made in -- the member is
// bound in BOTH guest backends and its nested field is typed all the way down
// to the boolean, rather than merely present.
//
// AGAINST shapeAPIVersion RATHER THAN THE PIN, because the whole story is a 2.1
// one: the `nil` field that cost the attribute and the
// dictionary-of-dictionaries that then deferred it are both shapes 2.0.77 does
// not have. UtilityConstants binds at the GA pin too, and trivially -- it has
// neither field there, so asserting it against 2.0.77 would assert nothing.
// See shapeAPIVersion.
func TestUtilityConstantsBindsAndReachesItsNestedLeaf(t *testing.T) {
	a := loadShapeAPI(t, shapeAPIVersion)
	r := GenerateMembers(a)
	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	key := "LuaPrototypes::utility_constants/" + fmt.Sprint(MemberGet)
	if _, ok := g.Names[key]; !ok {
		t.Error("LuaPrototypes::utility_constants is still deferred in Go")
	}
	if _, ok := rb.Names[key]; !ok {
		t.Error("LuaPrototypes::utility_constants is still deferred in Rust")
	}
	for _, want := range []string{
		"func (o LuaPrototypes) UtilityConstants() (UtilityConstants, error)",
		// The leaf: `[]EntryStringBool` is `dictionary[string -> boolean]`, and
		// a slice of THOSE is the outer dictionary. A guest reads
		// `c.DefaultTriggerTargetMaskByType[i].Val[j].Val` and gets a bool.
		"DefaultTriggerTargetMaskByType                                []EntryStringSliceEntryStringBool",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the Go bindings do not carry\n\t%s", want)
		}
	}
	for _, want := range []string{
		"pub fn utility_constants(&self) -> Result<UtilityConstants, Status>",
		"pub default_trigger_target_mask_by_type: BTreeMap<LuaStr, BTreeMap<LuaStr, bool>>,",
	} {
		if !strings.Contains(rb.Source, want) {
			t.Errorf("the Rust bindings do not carry\n\t%s", want)
		}
	}
	// And the concept it returns is a real struct rather than AD5's empty one.
	if strings.Contains(rb.Source, "pub struct UtilityConstants {\n}") {
		t.Error("UtilityConstants is emitted with no fields")
	}
}

// A DICTIONARY KEYED BY A CONTAINER IS STILL REFUSED, and that refusal keeps a
// census reason of its own.
//
// The recursion is deliberately one-sided: a container's ELEMENT and a
// dictionary's VALUE recurse to any depth, a dictionary's KEY does not. A Lua
// table key is not a table, no member in any pinned version keys one that way,
// and a shape nobody has reasoned about should arrive as a NEW number in the
// census diff rather than as a binding somebody guessed at. Asserted so that a
// future widening is a deliberate act.
func TestADictionaryKeyedByAContainerIsStillRefused(t *testing.T) {
	for _, c := range []struct {
		lang string
		try  func(FieldSpec) (string, bool)
	}{
		{"go", func(f FieldSpec) (string, bool) {
			g := newGoStructs()
			_, why, ok := g.add(f, "Outer")
			return why, ok
		}},
		{"rust", func(f FieldSpec) (string, bool) {
			g := newRustStructs()
			_, why, ok := g.add(f, "Outer")
			return why, ok
		}},
	} {
		f := FieldSpec{Name: "Outer", Kind: KindStruct, TypeName: "Outer", Struct: []FieldSpec{
			{Name: "bad", Kind: KindDict,
				Key:  &FieldSpec{Kind: KindArray, Elem: &FieldSpec{Kind: KindU32}},
				Elem: &FieldSpec{Kind: KindU32}},
		}}
		why, ok := c.try(f)
		if ok {
			t.Errorf("%s: a dictionary keyed by an array was accepted", c.lang)
			continue
		}
		if !strings.Contains(why, "keyed by") {
			t.Errorf("%s: refused with %q, which does not name the KEY as the "+
				"reason -- the census bucket is what tells a reader this is a "+
				"decision rather than a nesting depth nobody reached", c.lang, why)
		}
	}
}

// nestedDictAPI is UtilityConstants's shape in miniature: two ordinary fields
// around a dictionary whose value is a dictionary.
func nestedDictAPI() *API {
	inner := Type{Complex: "dictionary",
		Key: &Type{Name: "string"}, Value: &Type{Name: "boolean"}}
	return &API{
		APIVersion: 6,
		Concepts: []Concept{
			{Name: "FkConsts", Type: Type{Complex: "table", Parameters: []Parameter{
				{Name: "before", Order: 0, Type: Type{Name: "double"}},
				{Name: "default_trigger_target_mask_by_type", Order: 1,
					Type: Type{Complex: "dictionary",
						Key: &Type{Name: "string"}, Value: &inner}},
				{Name: "after", Order: 2, Type: Type{Name: "uint32"}},
			}}},
		},
		Classes: []Class{{Name: "LuaProtos", Attributes: []Attribute{
			{Name: "consts", ReadType: &Type{Name: "FkConsts"}},
		}}},
	}
}

// scrape pulls named integers out of one generated function body.
//
// Reading the emitted SOURCE rather than the layout it came from is the point:
// the layout is shared between the two backends and the host, so a test over it
// would pass with either backend emitting anything at all.
func scrape(t *testing.T, src, marker string, pats map[string]string) map[string]int {
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
	for name, p := range pats {
		m := regexp.MustCompile(p).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: nothing matched %q in:\n%s", name, p, body)
		}
		var v int
		fmt.Sscanf(m[1], "%d", &v)
		out[name] = v
	}
	return out
}
