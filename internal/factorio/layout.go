package factorio

import "fmt"

// Struct layout for the host-call ABI.
//
// fk.call hands the host two pointers into guest memory: one block of arguments
// and one for results. Both are laid out exactly as a C struct would be -- each
// field aligned to its own alignment, in declaration order, the whole padded to
// the widest -- so the generator can emit a matching `#[repr(C)]` struct or Go
// struct on the guest side and the two ends agree by construction.
//
// The offsets are computed HERE, once, and travel in the member table. Nothing
// recalculates them per call.
//
// THE KIND NUMBERS ARE SHARED WITH runtime/lua/fk_abi.lua and are a wire
// contract between two languages that cannot check each other at build time.
// TestKindNumbersMatchTheLuaABI is what checks it instead.

// HostAllocatesFor reports whether the host may call fk_alloc while encoding a
// RETURN of this kind, so the guest binding has to bracket the call with the
// allocation mark.
//
// It is deliberately a WHITELIST of the kinds that cannot allocate, and the
// reason is a bug rather than a preference. The predicate used to enumerate the
// kinds that DO -- array, dict, dyn -- and that list silently drifted from the
// question it was answering. KindString was missing from it, even though
// write_field's K_STRING branch calls alloc_ exactly as an array does; the only
// difference is that a string has a fixed layout and so goes through
// write_field rather than write_value. The result was that 290 of 291
// string-returning Go members never released the buffer the host took for them,
// and a mod reading entity.name in on_tick grew the pin list by sixty entries a
// second, forever, on every client.
//
// KindStruct is on the allocating side for the same reason: its fields are
// encoded by write_field, so a struct with a string in it allocates too, and
// nothing here can see that without resolving the concept.
//
// A blacklist fails open, which is how that happened. This fails closed: a kind
// added later allocates until someone says otherwise.
func HostAllocatesFor(k Kind) bool {
	switch k {
	case KindI8, KindU8, KindI16, KindU16, KindI32, KindU32,
		KindF32, KindF64, KindBool, KindHandle, KindU64:
		return false
	}
	return true
}

// Kind is the wire type of one field.
type Kind int

const (
	KindI8 Kind = iota + 1
	KindU8
	KindI16
	KindU16
	KindI32
	KindU32
	KindF32
	KindF64
	KindBool
	// KindString is a (pointer, length) pair of i32s, not the bytes.
	KindString
	// KindHandle is an i32 index into the handle table.
	KindHandle
	// KindU64 is a (lo, hi) pair of i32s. A double holds integers exactly only
	// to 2^53, so a u64 above that cannot round-trip -- the same ceiling
	// string.pack has, and every u64 the API actually returns is far below it.
	KindU64
	// KindStruct is a nested block, laid out inline exactly as a C struct
	// member is. Its size and alignment come from its own fields.
	KindStruct
	// KindArray is (ptr, count) -- a slice header. The elements live in
	// separately allocated memory because their number is not known until the
	// value exists, which is the same reason a string is (ptr, len).
	KindArray
	// KindDict is (ptr, count) over an array of key/value PAIRS. The layout is
	// an array's; only the table built from it differs, so the two share
	// everything except that last step.
	KindDict
	// KindDyn is TIER 2: a self-describing tagged value.
	//
	// One codec instead of 93 generated union types. A tag says what is
	// actually there, so it carries a structural union, a recursive type like
	// LocalisedString, and anything else a fixed layout cannot hold -- and it
	// tolerates version skew, because the tag describes the value rather than
	// what the schema said the value would be.
	//
	// 16 bytes: tag at 0, payload at 8. The payload is an f64, a (ptr, len)
	// string, a handle, or a (ptr, count) over more dynamic values.
	KindDyn
)

var kindName = map[Kind]string{
	KindI8: "i8", KindU8: "u8", KindI16: "i16", KindU16: "u16",
	KindI32: "i32", KindU32: "u32", KindF32: "f32", KindF64: "f64",
	KindBool: "bool", KindString: "string", KindHandle: "handle", KindU64: "u64",
	KindStruct: "struct", KindArray: "array", KindDict: "dict",
	KindDyn: "dyn",
}

func (k Kind) String() string {
	if s, ok := kindName[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Size is how many bytes the field occupies.
func (k Kind) Size() int {
	switch k {
	case KindI8, KindU8, KindBool:
		return 1
	case KindI16, KindU16:
		return 2
	case KindI32, KindU32, KindF32, KindHandle:
		return 4
	case KindF64, KindString, KindU64, KindArray, KindDict:
		return 8
	case KindDyn:
		return 16
	}
	return 0
}

// Align is the boundary the field starts on.
//
// A string and a u64 are each two i32s rather than one 8-byte scalar, so they
// align to 4 and not to 8. Getting that wrong shifts every following field.
func (k Kind) Align() int {
	switch k {
	case KindString, KindU64, KindArray, KindDict:
		return 4
	case KindDyn:
		return 8 // its payload slot holds an f64
	}
	return k.Size()
}

// Field is one placed member of a block.
type Field struct {
	Kind   Kind
	Offset int
}

// Block is a laid-out argument or result struct.
type Block struct {
	Fields []Field
	// Size is the whole block including trailing padding, which is what a guest
	// allocating one has to reserve.
	Size int
	// Align is the widest member's alignment, and therefore the block's own.
	Align int
}

// Layout places fields in declaration order under C rules.
func Layout(kinds []Kind) (Block, error) {
	b := Block{Align: 1}
	off := 0
	for i, k := range kinds {
		if k.Size() == 0 {
			return Block{}, fmt.Errorf("field %d: %v has no wire representation", i, k)
		}
		a := k.Align()
		if a > b.Align {
			b.Align = a
		}
		if r := off % a; r != 0 {
			off += a - r
		}
		b.Fields = append(b.Fields, Field{Kind: k, Offset: off})
		off += k.Size()
	}
	// Trailing padding, so an array of these blocks would also be aligned. A
	// guest struct has it whether or not we account for it, and a size that
	// disagrees with the guest's sizeof is a bug that only shows up on the
	// second element.
	if r := off % b.Align; r != 0 {
		off += b.Align - r
	}
	b.Size = off
	return b, nil
}

// LuaTable renders the block as the Lua the member table holds:
// `{ {kind=5,at=0}, {kind=8,at=8} }`.
func (b Block) LuaTable() string {
	s := "{"
	for i, f := range b.Fields {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("{kind=%d,at=%d}", int(f.Kind), f.Offset)
	}
	return s + "}"
}

// ---------------------------------------------------------------------------
// Structs: tier 1
//
// The events and the table-shaped concepts (`events` and
// `table_shaped_concepts` in census.json) are between them about 90% of the
// traffic. Both are named-field tables on the Lua side, so a block decodes
// into a table keyed by name rather than into a positional argument list.
//
// OPTIONALITY IS NOT AN EDGE CASE HERE. A large fraction of the fields across
// the table-shaped concepts are optional -- it was 619 of 1203 at the 2.0.77
// pin and the ratio moves with the description, which is the reason no number
// is asserted here -- so presence has to be represented rather than inferred.
// An optional field expands into a presence bool followed by its value, which
// is exactly what a Rust Option<T> or a Go
// *T lowers to under repr(C). Matching what the guest language would emit
// anyway is worth more than the byte a bitmask would save: the generator has to
// produce that struct too, and one obviously-correct shape beats two clever
// ones.
// ---------------------------------------------------------------------------

// FieldSpec describes one field before placement.
type FieldSpec struct {
	Name     string
	Kind     Kind
	Optional bool
	// TypeName is the API concept this came from, when it had a name.
	//
	// The host does not need it -- a layout is a layout -- but a GUEST does:
	// without it every struct field would be an anonymous Go struct repeated at
	// each use, and a mod author could not name the type they were passing.
	TypeName string
	// LazyPayload is the rendered parameter of a LuaLazyLoadedValue<T>, and it
	// is the one member of this struct that is DOCUMENTATION rather than
	// layout.
	//
	// The field crosses as a plain handle, because the whole point of that type
	// is that T is not constructed unless the guest calls Get() -- expanding T
	// here would defeat it. But `Mapping Object` tells a reader nothing about
	// what Get() yields, and the type occurs exactly once in the description:
	// too rarely to justify per-site generic typing, too usefully to leave
	// unsaid. So the generators render this into the field's doc comment, and
	// nothing downstream of LayoutStruct ever sees it -- Placed does not carry
	// it, and no offset depends on it.
	LazyPayload string
	// Variants is the VARIANT PARAMETER GROUP listing of the member whose
	// SHARED parameters this struct carries, and it is the second member of
	// this struct that is DOCUMENTATION rather than layout.
	//
	// A variant group's parameters are not fields of the typed form's argument
	// block: they are keys of the tier-2 tail beside it. So the struct is a
	// correct and INCOMPLETE picture of what the method takes, and its doc
	// comment is written from the struct's own name with no other route back to
	// the method -- which is why the listing travels here rather than being
	// looked up at emit time. Set only on the argument struct of a
	// variant-group method's typed form; nil everywhere else, and nothing
	// downstream of LayoutStruct sees it. See variantdoc.go.
	Variants *VariantDoc
	// Collapsed is the union this field's declared type was, when
	// canonicalUnion reduced it to its single class arm -- shape B, "one class
	// plus scalar identifiers". Nil everywhere else.
	//
	// The THIRD member of this struct that is documentation rather than layout,
	// and it is here for the reason the other two are: the layout says `handle`
	// and cannot say WHICH handle or what the description offered instead, so a
	// field typed `Object` where the API says `SpaceLocationID` reads as an
	// arbitrary object -- and any object handle type-checks. See
	// collapseddoc.go, and WormholeBelts' item 4.
	Collapsed *CollapsedUnion
	// Struct is the nested layout when Kind is KindStruct.
	Struct []FieldSpec
	// Elem is the element when Kind is KindArray, or the VALUE when KindDict.
	Elem *FieldSpec
	// Key is the key type when Kind is KindDict.
	Key *FieldSpec
	// Positional marks a KindStruct whose concept declares BOTH a keyed table
	// and an array shorthand -- canonicalUnion's shape A. It is set there and
	// nowhere else.
	//
	// IT IS NOT A LAYOUT PROPERTY, which is why it is a flag rather than a
	// second shape: the block is placed identically either way. What it records
	// is that the HOST may be handed either form of the same value, because the
	// description says the engine may send either -- and `Vector`'s own text
	// says which one it does send ("The game will always provide the array
	// format"). Without it write_struct looked up t[f.name] on `{2.19, 0}`,
	// found nil in both slots, and wrote a pair of zeros with the presence byte
	// set: a plausible number, status OK, nothing logged. Filed by WormholeBelts
	// (item 8) after LuaEntityPrototype::inserter_drop_position read 0 tiles off
	// the six inserter prototypes a base plus Space Age game defines, against
	// 2.19 tiles for the same reach measured off live entities -- which reads
	// LuaEntity::drop_position, a MapPosition rather than a Vector, and arrives
	// keyed. Different concepts, which is what makes the pair evidence for the
	// engine choosing PER MEMBER -- and examples/api measures that pair here
	// rather than quoting it, reading a Vector off a prototype and a
	// MapPosition off a chest in one real run, where the first needs the flag
	// and the second never has.
	//
	// THE FIELD ORDER IS THE TUPLE ORDER BY CONSTRUCTION: LayoutStruct places in
	// the description's `order`, which is exactly what the array shorthand is an
	// ordering of. BoundingBox is the case that proves it rather than assuming
	// it -- its parameters arrive in the JSON as left_top, orientation,
	// right_bottom and its `order` is left_top, right_bottom, orientation, which
	// is the shorthand {{-2,-3},{5,8}} with the optional third member left off
	// the end.
	Positional bool
}

// Placed is one field with its offset resolved.
type Placed struct {
	Name   string
	Kind   Kind
	Offset int
	// HasOffset is where the presence bool lives, or -1 when the field is
	// mandatory. A decoder that ignores it reads uninitialised memory as a
	// value, which is a plausible-looking wrong answer rather than a crash.
	HasOffset int
	// Fields, Size and Align are set only for KindStruct.
	Fields      []Placed
	Size, Align int
	// Elem, Key and Stride are set for KindArray and KindDict. Stride is the
	// distance between consecutive elements, which for a struct element is its
	// already-padded size -- that padding is why an array of structs cannot
	// just use the sum of the field widths.
	Elem   *Placed
	Key    *Placed
	Stride int
	// Positional carries FieldSpec.Positional through placement, so a nested
	// shape-A struct keeps its own flag and `{{1,2},{3,4}}` reads right at every
	// level. See FieldSpec.Positional.
	Positional bool
}

// StructBlock is a laid-out named-field block.
type StructBlock struct {
	Fields      []Placed
	Size, Align int
}

// IsDynValueStruct reports the shape ModSetting has: a whole struct whose only
// content is ONE mandatory tier-2 value.
//
// It is what earns a generated struct the typed Bool/Num/Str/Obj readers, and
// it is a rule over the SHAPE rather than a list of names for the reason this
// repo keeps meeting: a name list is a decision nobody re-reads, and the same
// shape arriving at a later pin under a different name would silently get
// nothing. Both generators ask this one function, so they cannot disagree, and
// the census counts what it matched.
//
// MANDATORY is part of the shape. An optional single field would make the
// readers answer for a value that may not be there at all, and there the
// presence byte -- the guest's own pointer, or Option -- is the honest way to
// ask.
func IsDynValueStruct(b StructBlock) bool {
	return len(b.Fields) == 1 && b.Fields[0].Kind == KindDyn && b.Fields[0].HasOffset < 0
}

// LayoutStruct places named fields, expanding each optional one into a presence
// bool ahead of its value.
func LayoutStruct(specs []FieldSpec) (StructBlock, error) {
	b := StructBlock{Align: 1}
	off := 0
	place := func(align, size int) int {
		if align > b.Align {
			b.Align = align
		}
		if r := off % align; r != 0 {
			off += align - r
		}
		at := off
		off += size
		return at
	}

	for i, f := range specs {
		if f.Name == "" {
			return StructBlock{}, fmt.Errorf("field %d has no name", i)
		}
		p := Placed{Name: f.Name, Kind: f.Kind, HasOffset: -1}
		if f.Optional {
			p.HasOffset = place(1, 1)
		}
		switch f.Kind {
		case KindStruct:
			nested, err := LayoutStruct(f.Struct)
			if err != nil {
				return StructBlock{}, fmt.Errorf("%s: %w", f.Name, err)
			}
			p.Fields, p.Size, p.Align = nested.Fields, nested.Size, nested.Align
			p.Positional = f.Positional
			p.Offset = place(nested.Align, nested.Size)

		case KindArray, KindDict:
			if f.Elem == nil {
				return StructBlock{}, fmt.Errorf("%s: %v with no element type", f.Name, f.Kind)
			}
			// The element is laid out as a one-field block, which gives it the
			// same padding rule a real array element gets.
			var elemSpecs []FieldSpec
			if f.Kind == KindDict {
				if f.Key == nil {
					return StructBlock{}, fmt.Errorf("%s: dict with no key type", f.Name)
				}
				k := *f.Key
				k.Name = "key"
				v := *f.Elem
				v.Name = "value"
				elemSpecs = []FieldSpec{k, v}
			} else {
				e := *f.Elem
				e.Name = "elem"
				elemSpecs = []FieldSpec{e}
			}
			pair, err := LayoutStruct(elemSpecs)
			if err != nil {
				return StructBlock{}, fmt.Errorf("%s: %w", f.Name, err)
			}
			p.Stride = pair.Size
			if f.Kind == KindDict {
				k, v := pair.Fields[0], pair.Fields[1]
				p.Key, p.Elem = &k, &v
			} else {
				e := pair.Fields[0]
				p.Elem = &e
			}
			p.Offset = place(f.Kind.Align(), f.Kind.Size())

		default:
			if f.Kind.Size() == 0 {
				return StructBlock{}, fmt.Errorf("%s: %v has no wire representation", f.Name, f.Kind)
			}
			p.Offset = place(f.Kind.Align(), f.Kind.Size())
		}
		b.Fields = append(b.Fields, p)
	}
	if r := off % b.Align; r != 0 {
		off += b.Align - r
	}
	b.Size = off
	return b, nil
}

// LuaTable renders the block as the descriptor the member table holds.
func (b StructBlock) LuaTable() string { return placedList(b.Fields) }

func placedList(fs []Placed) string {
	s := "{"
	for i, f := range fs {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("{name=%q,kind=%d,at=%d", f.Name, int(f.Kind), f.Offset)
		if f.HasOffset >= 0 {
			s += fmt.Sprintf(",has=%d", f.HasOffset)
		}
		switch f.Kind {
		case KindStruct:
			// pos= is emitted only when true, so every struct descriptor the
			// description does not call positional is byte for byte what it was.
			if f.Positional {
				s += ",pos=true"
			}
			s += ",fields=" + placedList(f.Fields)
		case KindArray:
			s += fmt.Sprintf(",stride=%d,elem=%s", f.Stride, placedOne(*f.Elem))
		case KindDict:
			s += fmt.Sprintf(",stride=%d,key=%s,elem=%s",
				f.Stride, placedOne(*f.Key), placedOne(*f.Elem))
		}
		s += "}"
	}
	return s + "}"
}

func placedOne(f Placed) string {
	return placedList([]Placed{f})[1 : len(placedList([]Placed{f}))-1]
}
