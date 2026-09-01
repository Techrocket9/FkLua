package factorio

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// A `table | tuple` CONCEPT ARRIVES IN WHICHEVER FORM THE ENGINE SENDS, AND
// BOTH READ THE SAME.
//
// Ten concepts are declared as a keyed table plus an array shorthand --
// canonicalUnion's shape A -- and the engine picks per member. `Vector`'s own
// description says which one it picks: "The game will always provide the array
// format". write_struct looked the fields up by NAME only, so `{2.19, 0}` gave
// nil twice and wrote a pair of ZEROS with the presence byte set: a plausible
// number, status OK, nothing logged. Measured on 2.0.77 as
// LuaEntityPrototype::inserter_drop_position reading 0 tiles off the six
// inserter prototypes a base plus Space Age game defines, against 2.19 tiles
// for the same reach measured off live entities -- through
// LuaEntity::drop_position, which is a MapPosition rather than a Vector and
// arrives keyed. Filed by WormholeBelts (item 8).
//
// THE PER-MEMBER CHOICE IS OBSERVED HERE RATHER THAN QUOTED, and it has to be
// observed in a real game: no stub can prove which form the engine picks,
// because a stub returns whatever it was written to return. guest/*/examples/api
// reads BOTH in one run -- inserter_drop_position (a Vector) off the inserter
// prototype and LuaEntity::position (a MapPosition) off the chest it builds at
// (8, 8) -- and logs `shorthand struct: inserter_drop_position = 0.00,1.20`
// beside `keyed struct: entity.position = 8.50,8.50`, in both language arms.
// Delete write_struct's positional fallback and the first reads 0.00,0.00 while
// the second does not move. Both are asserted by run-guest.sh's MUST_RE.
//
// SEVEN LEGS, AND THE THIRD IS WHY THE OTHERS MEAN ANYTHING, one printed line
// each. The array form and the keyed form must agree; a descriptor WITHOUT pos=
// given the array form must still read zeros, because otherwise this would be a
// general fallback rather than a flag the description earns; a table carrying
// BOTH spellings reads by name, which is what keeps every keyed value the engine
// already sends reading exactly as it did; and BoundingBox's nested shape in the
// array, keyed and mixed forms is the case a per-call `pos` argument would get
// wrong, since BoundingBox is positional and so is each of the two MapPositions
// inside it.
func TestAPositionalStructAcceptsEitherForm(t *testing.T) {
	// MapPosition's shape, by hand rather than out of a description: what is
	// under test is the FLAG's mechanism, and the description's own concepts are
	// covered by the generator test below.
	xy := []FieldSpec{{Name: "x", Kind: KindF64}, {Name: "y", Kind: KindF64}}

	posBlk, err := LayoutStruct([]FieldSpec{
		{Name: "v", Kind: KindStruct, Positional: true, Struct: xy},
	})
	if err != nil {
		t.Fatal(err)
	}
	// THE NEGATIVE CONTROL: the same fields at the same offsets with the flag
	// off. Anything but zeros here would mean write_struct had grown a fallback
	// nothing declared.
	plainBlk, err := LayoutStruct([]FieldSpec{
		{Name: "v", Kind: KindStruct, Struct: xy},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posBlk.LuaTable(), "pos=true") {
		t.Errorf("the flagged descriptor does not render pos=true: %s", posBlk.LuaTable())
	}
	if strings.Contains(plainBlk.LuaTable(), "pos=") {
		t.Errorf("the unflagged descriptor renders pos=: %s", plainBlk.LuaTable())
	}
	// The two differ in that string and in nothing else, which is what says the
	// flag costs no bytes anywhere it is off.
	if strings.Replace(posBlk.LuaTable(), ",pos=true", "", 1) != plainBlk.LuaTable() {
		t.Errorf("pos=true is not the only difference:\n  %s\n  %s",
			posBlk.LuaTable(), plainBlk.LuaTable())
	}

	// BoundingBox's shape: two positional MapPositions and a trailing OPTIONAL
	// the two-element shorthand does not carry. The order is the description's
	// `order` -- left_top, right_bottom, orientation -- which is what makes the
	// placed index the tuple index.
	boxBlk, err := LayoutStruct([]FieldSpec{{
		Name: "b", Kind: KindStruct, Positional: true, Struct: []FieldSpec{
			{Name: "left_top", Kind: KindStruct, Positional: true, Struct: xy},
			{Name: "right_bottom", Kind: KindStruct, Positional: true, Struct: xy},
			{Name: "orientation", Kind: KindF64, Optional: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(boxBlk.LuaTable(), "pos=true"); n != 3 {
		t.Errorf("the nested descriptor carries %d pos=true, want 3 "+
			"(the box and both of its corners): %s", n, boxBlk.LuaTable())
	}

	out := runMarshal(t, fmt.Sprintf(`
local pos, plain, box = %s, %s, %s
local function show(v)
  if v == nil then return "absent" end
  return string.format("%%.2f", v)
end
local function vec(fields, base, t)
  local st = H.write_struct(fields, base, t)
  local v = H.read_struct(fields, base).v
  return st .. " " .. show(v.x) .. "," .. show(v.y)
end
-- Distinct bases, so a leg that wrote nothing at all cannot read the previous
-- leg's answer and pass.
print("array   " .. vec(pos,   1024, {v = {1.5, -2}}))
print("keyed   " .. vec(pos,   1280, {v = {x = 1.5, y = -2}}))
print("control " .. vec(plain, 1536, {v = {1.5, -2}}))
-- A table carrying BOTH spellings: the name wins, which is what keeps every
-- keyed value the engine already sends reading exactly as it did.
print("both    " .. vec(pos,   1792, {v = {1.5, -2, x = 9.5, y = 9.5}}))

local function bbox(base, t)
  local st = H.write_struct(box, base, t)
  local b = H.read_struct(box, base).b
  return st .. " " .. show(b.left_top.x) .. "," .. show(b.left_top.y) ..
    " " .. show(b.right_bottom.x) .. "," .. show(b.right_bottom.y) ..
    " " .. show(b.orientation)
end
print("box arr " .. bbox(2048, {b = {{-2, -3}, {5, 8}}}))
print("box key " .. bbox(2304, {b = {left_top = {x = -2, y = -3},
  right_bottom = {x = 5, y = 8}, orientation = 0.25}}))
-- MIXED, which no single reading of "the engine sends one form" covers: the
-- outer table is keyed and the corners are shorthand, which is what the
-- BoundingBox example in the description itself looks like halfway written.
print("box mix " .. bbox(2560, {b = {left_top = {-2, -3}, right_bottom = {5, 8}}}))
`, posBlk.LuaTable(), plainBlk.LuaTable(), boxBlk.LuaTable()))

	want := strings.Join([]string{
		"array   0 1.50,-2.00",
		"keyed   0 1.50,-2.00",
		// The defect, preserved as the control: no flag, no fallback, zeros.
		"control 0 0.00,0.00",
		"both    0 9.50,9.50",
		"box arr 0 -2.00,-3.00 5.00,8.00 absent",
		"box key 0 -2.00,-3.00 5.00,8.00 0.25",
		"box mix 0 -2.00,-3.00 5.00,8.00 absent",
	}, "\n")
	if out != want {
		t.Errorf("through fk_abi:\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// THE TEN CONCEPTS THE DESCRIPTION DECLARES BOTH WAYS ALL CARRY THE FLAG, AND A
// TABLE-ONLY CONCEPT DOES NOT.
//
// The population is derived from the description rather than listed here, for
// the reason TestOnlyTwoWritableAttributesAreAUnionWithAClass derives its own:
// a list is a decision nobody re-reads, and a concept that GAINED the shape at a
// later pin would silently get nothing. The names are then compared against the
// list, which is the half that notices a pin adding an eleventh.
//
// At every committed description, because the mapper is one code path serving
// all of them.
var tableOrTupleConcepts = []string{
	"BoundingBox", "ChunkPosition", "Color", "ColorModifier",
	"EquipmentPosition", "GuiLocation", "MapPosition", "TilePosition",
	"Vector", "Vector3D",
}

func TestTheTableOrTupleConceptsRenderPositional(t *testing.T) {
	// Shorthand ELEMENTS compared against the table members they index. A run
	// that compared none would pass while asserting nothing, which is what the
	// zero check at the end is for. The two shorthand kinds are counted APART
	// on purpose: only the tuple one can carry a zero tooth (see the end of the
	// test), and one counter fed by both could be held above zero by arrays
	// while the tuple walk compared nothing.
	elements, arrayElements := 0, 0
	for _, v := range committedVersions(t) {
		a := loadShapeAPI(t, v)
		m := newTypeMapper(a)

		var derived []string
		for i := range a.Concepts {
			if declaresATableAndAShorthand(&a.Concepts[i]) {
				derived = append(derived, a.Concepts[i].Name)
			}
		}
		sort.Strings(derived)
		if len(derived) == 0 {
			t.Fatalf("%s: no table-or-tuple concept found, so this test asserts "+
				"nothing -- the derivation broke, not the description", v)
		}
		if strings.Join(derived, ",") != strings.Join(tableOrTupleConcepts, ",") {
			t.Errorf("%s: table-or-tuple concepts are %v, want %v. A new one wants "+
				"canonicalUnion's shape A read before this list is edited",
				v, derived, tableOrTupleConcepts)
		}

		for _, name := range derived {
			f, err := m.mapNamed(name, 0)
			if err != nil {
				t.Errorf("%s: %s does not map: %v", v, name, err)
				continue
			}
			if f.Kind != KindStruct {
				t.Errorf("%s: %s maps to kind %d, want a struct (%d)",
					v, name, f.Kind, KindStruct)
				continue
			}
			if !f.Positional {
				t.Errorf("%s: %s is a table plus an array shorthand and is NOT "+
					"marked positional, so the engine's array form would write "+
					"zeros through it", v, name)
				continue
			}
			// EVERY DECLARED MEMBER REACHED THE STRUCT, which is what makes the
			// placed index the tuple index. A field the mapper OMITS -- the one
			// omission this generator performs is a `nil`-typed member, see
			// typeMapper.omit -- would shift every member after it against the
			// array form while leaving the keyed form correct, so the two would
			// disagree with nothing saying which was right. No committed
			// description does that to any of these ten, and this is what says
			// so rather than assuming it.
			if n := declaredTableParams(a, name); n != len(f.Struct) {
				t.Errorf("%s: %s declares %d table members and placed %d -- a "+
					"positional read indexes by placed position, so an omitted "+
					"member misaligns every one after it", v, name, n, len(f.Struct))
			}
			// ...AND THE TUPLE IS THE OTHER HALF OF THE SAME PREMISE. The line
			// above compares the TABLE option with what was placed, which catches
			// a member the mapper dropped. It says nothing about the shorthand,
			// and the shorthand is the thing being indexed: a concept declared
			// `table{x, y} | tuple[f, f, f]` would place two fields, read the
			// first two elements and DROP the third with every check green. So
			// each shorthand is asserted against the same `order` the placement
			// follows -- no longer than the table, and element i the type of the
			// member the mapper put at index i.
			tbl, tuples, arrays := declaredShapeA(a, name)
			if len(tuples) == 0 {
				t.Errorf("%s: %s has %d array-typed shorthands and no tuple, so "+
					"the element-by-element check asserts nothing -- an array "+
					"shorthand declares no length, and this tooth wants "+
					"re-reading before one ships", v, name, len(arrays))
			}
			// AN ARRAY SHORTHAND BESIDE A TUPLE IS STILL CHECKABLE, and until
			// this loop it was not checked at all: the arm above only fires when
			// there is NO tuple, so `table{x, y} | tuple[f, f] | array[T]` had
			// every element of the tuple compared and nothing said about T. An
			// array declares no length, so any index may land in any member --
			// which makes its one element type a claim about EVERY member, and
			// an element type contradicting even one of them is a shorthand
			// write_struct would read into a field of another type.
			for _, et := range arrays {
				for i, p := range tbl {
					if got, want := et.String(), p.Type.String(); got != want {
						t.Errorf("%s: %s has an array shorthand of %s and its "+
							"member at order %d (%s) is %s -- an array index may "+
							"land in any member, so its element type has to "+
							"serve all of them", v, name, got, i, p.Name, want)
					}
					arrayElements++
				}
			}
			for _, tup := range tuples {
				if len(tup) > len(tbl) {
					t.Errorf("%s: %s has a %d-element shorthand over %d table "+
						"members -- the elements past the end have no field to "+
						"land in and would be dropped silently",
						v, name, len(tup), len(tbl))
				}
				for i, et := range tup {
					if i >= len(tbl) || i >= len(f.Struct) {
						break // the length errors above have already said so
					}
					if got, want := et.String(), tbl[i].Type.String(); got != want {
						t.Errorf("%s: %s shorthand element %d is %s and the "+
							"member at order %d (%s) is %s -- the positional "+
							"index rests on those being the same member",
							v, name, i, got, i, tbl[i].Name, want)
					}
					// The bridge from the description's `order` to the PLACED
					// index, which is what write_struct's t[i] actually uses.
					if f.Struct[i].Name != tbl[i].Name {
						t.Errorf("%s: %s placed %q at index %d where `order` "+
							"says %q -- the placement and the shorthand no "+
							"longer agree on what element %d is",
							v, name, f.Struct[i].Name, i, tbl[i].Name, i)
					}
					elements++
				}
			}
			// ...and it survives PLACEMENT and reaches the descriptor, which is
			// the only form the host ever sees.
			blk, err := LayoutStruct([]FieldSpec{{Name: "v", Kind: KindStruct,
				Positional: f.Positional, Struct: f.Struct}})
			if err != nil {
				t.Errorf("%s: %s does not lay out: %v", v, name, err)
				continue
			}
			if !strings.Contains(blk.LuaTable(), "pos=true") {
				t.Errorf("%s: %s is flagged and its descriptor does not say so: %s",
					v, name, blk.LuaTable())
			}
		}

		// A TABLE-ONLY CONCEPT MUST NOT BE FLAGGED. `ItemStackDefinition` is a
		// plain table concept at every committed pin: no union, no shorthand,
		// and a positional read of it would take `name` from t[1].
		plain, err := m.mapNamed("ItemStackDefinition", 0)
		if err != nil {
			t.Fatalf("%s: ItemStackDefinition does not map: %v", v, err)
		}
		if plain.Kind != KindStruct {
			t.Fatalf("%s: ItemStackDefinition is kind %d, not a struct", v, plain.Kind)
		}
		if plain.Positional {
			t.Errorf("%s: ItemStackDefinition is a table concept with no array "+
				"shorthand and is marked positional", v)
		}
	}
	// Ten concepts at five descriptions, two to four elements each. A zero here
	// means the TUPLE walk stopped finding anything to compare.
	//
	// The array walk gets no zero tooth of its own and cannot have one: no
	// committed description carries an array shorthand inside any of these ten,
	// so arrayElements is 0 at every pin today and a check demanding otherwise
	// would fail the run for the shape being absent rather than for the walk
	// being broken. What stands in for it is the injection -- an `array` option
	// appended beside Vector's tuple has to redden this test -- and the arm
	// above it, which still fails loudly when a concept's shorthands are ALL
	// arrays. Counting the two kinds into one number is what those two teeth
	// could not survive: arrays alone would hold the total above zero.
	if elements == 0 {
		t.Errorf("no TUPLE element was compared against a table member, so the "+
			"shorthand half of the arity tooth asserted nothing (%d array "+
			"element-type comparisons were made)", arrayElements)
	}
}

// declaredShapeA reads canonicalUnion's shape A off the DESCRIPTION and returns
// the table option's parameters SORTED BY `order` -- which is the order
// typeMapper.mapFields places in, so index i here is the placed index -- every
// tuple option's element types, and every ARRAY option's element type. A
// concept that is not shape A returns three zero values.
func declaredShapeA(a *API, name string) (tbl []Parameter, tuples [][]Type, arrays []Type) {
	for i := range a.Concepts {
		if a.Concepts[i].Name != name {
			continue
		}
		t := a.Concepts[i].Type
		for t.Complex == "type" && t.Value != nil {
			t = *t.Value
		}
		for _, o := range t.Options {
			for o.Complex == "type" && o.Value != nil {
				o = *o.Value
			}
			switch o.Complex {
			case "table":
				tbl = append([]Parameter(nil), o.Parameters...)
				sort.SliceStable(tbl, func(i, j int) bool { return tbl[i].Order < tbl[j].Order })
			case "tuple":
				tuples = append(tuples, o.Values)
			case "array":
				// An array shorthand declares an element TYPE and no length, so
				// there is no element-by-element comparison to make -- but the
				// one type it does declare has to serve EVERY member, since any
				// index may land in any of them. None of the ten has an array
				// shorthand at any committed pin; the caller checks the element
				// type against every member rather than skipping it.
				if o.Value == nil {
					arrays = append(arrays, Type{Name: "(no element type)"})
					continue
				}
				arrays = append(arrays, *o.Value)
			}
		}
		return tbl, tuples, arrays
	}
	return nil, nil, nil
}

// declaredTableParams counts the members the concept's TABLE option declares,
// read off the description rather than off anything the mapper produced.
func declaredTableParams(a *API, name string) int {
	for i := range a.Concepts {
		if a.Concepts[i].Name != name {
			continue
		}
		t := a.Concepts[i].Type
		for t.Complex == "type" && t.Value != nil {
			t = *t.Value
		}
		for _, o := range t.Options {
			for o.Complex == "type" && o.Value != nil {
				o = *o.Value
			}
			if o.Complex == "table" {
				return len(o.Parameters)
			}
		}
	}
	return -1
}

// declaresATableAndAShorthand is canonicalUnion's shape A read off the
// DESCRIPTION: a union of exactly one table option and at least one tuple or
// array option, and nothing else.
//
// Deliberately independent of canonicalUnion, so the test cannot agree with the
// code by construction.
func declaresATableAndAShorthand(c *Concept) bool {
	t := c.Type
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if t.Complex != "union" {
		return false
	}
	tables, shorthands := 0, 0
	for _, o := range t.Options {
		for o.Complex == "type" && o.Value != nil {
			o = *o.Value
		}
		switch o.Complex {
		case "table":
			tables++
		case "tuple", "array":
			shorthands++
		default:
			return false
		}
	}
	return tables == 1 && shorthands > 0
}

// THE COUNT IS NON-ZERO AT EVERY COMMITTED DESCRIPTION, which is the census
// row's anti-vacuity tooth.
//
// `positional_struct_positions` exists because the population it counts was
// silently wrong for as long as the ABI has existed and nothing anywhere wrote
// the number down. A row that read 0 would look exactly like a description that
// stopped carrying the shape, so the number is asserted to be there rather than
// merely committed.
func TestThePositionalCensusRowIsNonZeroEverywhere(t *testing.T) {
	for _, v := range committedVersions(t) {
		a := loadShapeAPI(t, v)
		c, err := TakeCensus(a)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if c.PositionalStructs <= 0 {
			t.Errorf("%s: positional_struct_positions is %d -- ten concepts have "+
				"the shape at every committed pin, so a zero is the rule having "+
				"stopped firing", v, c.PositionalStructs)
		}
		// ...and it really is counting POSITIONS rather than concepts: there are
		// ten of those and hundreds of references to them.
		if c.PositionalStructs < len(tableOrTupleConcepts) {
			t.Errorf("%s: positional_struct_positions is %d, fewer than the %d "+
				"concepts that have the shape -- the walk is missing blocks",
				v, c.PositionalStructs, len(tableOrTupleConcepts))
		}
	}
}
