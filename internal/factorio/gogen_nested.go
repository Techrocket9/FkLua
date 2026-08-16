package factorio

import "fmt"

// NESTED CONTAINERS: an array whose elements are arrays, a dictionary whose
// values are arrays or dictionaries.
//
// THE HOST NEVER HAD THIS GAP. LayoutStruct lays a container's element out as a
// one-field block and recurses, `placedList` renders a nested `stride/key/elem`
// descriptor at any depth, and fk_abi.lua's `read_value` routes K_ARRAY and
// K_DICT to one `read_array` walk that calls `read_value` again on each element.
// So every one of these members has been in the member table, correctly laid
// out and correctly marshalled, since the ABI existed. What was missing was a
// GUEST TYPE and a guest-side codec, in both backends, for a shape one level
// deeper than the emitters recursed.
//
// AD4 IS THE PRECEDENT AND THE WARNING. There, a top-level dictionary rendered
// fine while a dictionary nested in a STRUCT refused -- one nesting level -- and
// unblocking it took 17 deferrals with it because a concept that cannot be
// expressed fails every member that mentions it. This is the same fact one level
// over: 16 of the 18 remaining member deferrals were "a dictionary of a
// dictionary" (8), "a dictionary of an array" (7) and "an array of an array"
// (1), and the container machinery each of them needs already existed at depth
// one.
//
// WHAT RECURSES AND WHAT DOES NOT. The VALUE of a dictionary and the ELEMENT of
// an array recurse, to any depth -- `LuaPlayer::get_alerts` is three levels
// (dictionary of dictionary of array of struct) and comes out whole. A dictionary
// KEY does not: it must still be a scalar, a handle or a tier-2 Value, because a
// key is a Lua table key and the description has no member keying one by a table.
// That refusal keeps its own census reason ("a dictionary keyed by ...") so that
// a description which grows one is a NEW number rather than a silent absence.
//
// DETERMINISM AT EVERY LEVEL, which is the constraint that decides the shape.
// Q3's rule -- a dictionary is an ORDERED PAIR SLICE in Go, never a map, because
// Go randomizes map iteration and a lockstep game turns a per-client walk order
// into a desync -- is a property of each level independently, so it is applied
// at each level independently. A nested dictionary is `[]Entry<K><V>` exactly as
// a top-level one is; the wire order a guest sends is the order it chose, all
// the way down, and the order it RECEIVES is the host's `pairs()` order at every
// level, which is unpromised at every level for the same reason. Rust reaches
// the same place from the other side: a `BTreeMap` iterates in key order, so a
// nested one is deterministic by construction, and a key that is not `Ord` falls
// back to the pair `Vec` -- the same choice rustDictType already makes, asked
// once per level.
//
// A CODEC FUNCTION RATHER THAN A DEEPER INLINE LOOP. The existing depth-one
// loops are inline at four sites per backend (a member's argument encode, its
// return decode, a struct field's encode and its decode) and inlining a second
// level would multiply that by the depth. Instead each distinct nested container
// gets one generated `decCtn<T>` / `encCtn<T>` / `valCtn<T>` triple, and the
// four existing sites gain one branch each: where they would call `goLoad` on a
// scalar or `decode<T>` on a struct, they call the container's decoder. The
// depth-one code is therefore emitted BYTE FOR BYTE as it was -- the golden diff
// is new members and new helpers, and nothing that already worked moved.

// goContainer is one nested container's whole description: the Go type it
// became, and the layout its codec has to walk.
//
// It is keyed by the Go TYPE, which is sound because the type determines the
// layout: kind-to-Go-type is injective over the scalars (KindI32 is int32 and
// KindU32 is uint32), a handle is only ever Object, tier 2 is only ever Value,
// and a named struct's own name fixes its block. sig() states that dependence
// as a check rather than leaving it as an argument -- two layouts arriving under
// one name would emit one codec and use it for both, which is the silent-wrong-
// bytes failure this package refuses everywhere else.
type goContainer struct {
	name   string
	goType string
	kind   Kind
	stride int

	// The array element, or the dictionary value: the two are the same slot in
	// the pair walk and differ only in what sits beside them.
	elemType string
	elemKind Kind
	elemOff  int
	// elemCtn is the codec of a value that is ITSELF a container, empty
	// otherwise. That is where the recursion lives.
	elemCtn string

	entryType string
	keyType   string
	keyKind   Kind
	keyOff    int
}

func (c goContainer) sig() string {
	return fmt.Sprintf("%d/%d/%d@%d/%d@%d/%s", c.kind, c.stride,
		c.elemKind, c.elemOff, c.keyKind, c.keyOff, c.elemCtn)
}

// goElemType names one placed element or dictionary value, registering whatever
// it reaches: a named struct, or a nested container's codec.
//
// `role` is "an array" or "a dictionary" and exists only so a refusal reads the
// way it always has -- the census counts reasons, and rewording one is a row
// that vanishes and a row that appears.
func (g *goStructs) goElemType(e Placed, spec *FieldSpec, fallback, role string) (goType, codec, why string, ok bool) {
	switch e.Kind {
	case KindStruct:
		// The element's own spec, for its NAME. Without it the struct would be
		// anonymous and a mod author could not declare a variable of the type
		// they were being handed.
		if spec == nil {
			return "", "", role + " of structs with no concept to name it", false
		}
		t, why, ok := g.add(*spec, fallback)
		return t, "", why, ok
	case KindArray, KindDict:
		return g.container(e, spec, fallback)
	}
	t, ok := goScalar(e.Kind)
	if !ok {
		return "", "", role + " of " + goScalarReason(e.Kind)[len("returns or takes "):], false
	}
	return t, "", "", true
}

// container registers the codec for one nested container and returns its Go
// type and the codec's name.
//
// No cycle guard, and none is possible: a container's layout is computed by
// LayoutStruct, which would not have terminated on a self-referential one. The
// recursion here follows a layout that already exists.
func (g *goStructs) container(p Placed, spec *FieldSpec, fallback string) (goType, codec, why string, ok bool) {
	if p.Elem == nil || p.Stride <= 0 {
		return "", "", "a nested container with no element layout", false
	}
	var sub *FieldSpec
	if spec != nil {
		sub = spec.Elem
	}
	c := goContainer{kind: p.Kind, stride: p.Stride,
		elemKind: p.Elem.Kind, elemOff: p.Elem.Offset}

	switch p.Kind {
	case KindArray:
		et, ec, why, ok := g.goElemType(*p.Elem, sub, fallback+"Elem", "an array")
		if !ok {
			return "", "", why, false
		}
		c.elemType, c.elemCtn = et, ec
		c.goType = "[]" + et
		c.name = "Slice" + goTypeIdent(et)

	case KindDict:
		if p.Key == nil {
			return "", "", "a dictionary with no key layout", false
		}
		// A KEY STAYS A SCALAR. See the header: a Lua table key is not a table,
		// and this refusal keeps its own reason so that a description which
		// grows one shows up as a number.
		kt, okk := goScalar(p.Key.Kind)
		if !okk {
			return "", "", "a dictionary keyed by " +
				goScalarReason(p.Key.Kind)[len("returns or takes "):], false
		}
		vt, vc, why, ok := g.goElemType(*p.Elem, sub, fallback+"Value", "a dictionary")
		if !ok {
			return "", "", why, false
		}
		c.keyType, c.keyKind, c.keyOff = kt, p.Key.Kind, p.Key.Offset
		c.elemType, c.elemCtn = vt, vc
		// EVERY LEVEL IS THE ORDERED PAIR SLICE, for the reason the top level
		// is: a Go map's walk order is randomized per process and this one is
		// nested inside a value that crosses the wire.
		c.entryType = g.entryFor(kt, vt)
		c.goType = "[]" + c.entryType
		c.name = "Slice" + c.entryType

	default:
		return "", "", "a nested " + p.Kind.String(), false
	}

	if old, seen := g.ctn[c.name]; seen {
		if old.sig() != c.sig() {
			return "", "", "a nested container whose Go type names two layouts", false
		}
		return c.goType, c.name, "", true
	}
	g.ctn[c.name] = c
	g.ctnOrder = append(g.ctnOrder, c.name)
	return c.goType, c.name, "", true
}

// goLoadElem renders the read of one element out of a pair slice, dispatching
// on what the element IS: a scalar, a named struct, or a nested container.
//
// It is the one place the four inline walks have to agree, which is why it is a
// function rather than a third copy of `if kind == KindStruct` at each of them.
func goLoadElem(buf string, off int, k Kind, typ, ctn string) string {
	switch {
	case ctn != "":
		return fmt.Sprintf("decCtn%s(&%s[%d])", ctn, buf, off)
	case k == KindStruct:
		return fmt.Sprintf("decode%s(&%s[%d])", typ, buf, off)
	}
	return goLoad(buf, off, k)
}

// goStoreElem is goLoadElem's mirror.
func goStoreElem(buf string, off int, k Kind, ctn, v string) string {
	if ctn != "" {
		return fmt.Sprintf("encCtn%s(&%s[%d], %s)", ctn, buf, off, v)
	}
	return goStore(buf, off, k, v)
}

// goValueElem renders one element as a tier-2 Value, recursing into a nested
// container through its generated helper.
func (g *goStructs) goValueElem(k Kind, ctn, acc string) string {
	if ctn != "" {
		return fmt.Sprintf("valCtn%s(%s)", ctn, acc)
	}
	return g.valueOfElem(k, acc)
}

// emitContainers writes the codec triple for every nested container reached.
//
// One decoder, one encoder and one tier-2 renderer apiece. All three are
// package-level functions rather than methods, because the type they operate on
// is a slice and Go cannot hang a method on one.
func (g *goStructs) emitContainers(w func(string, ...any)) {
	if len(g.ctnOrder) == 0 {
		return
	}
	// NO BACKTICKS IN THIS COMMENT. The generated package is carried through a
	// raw string in the test corpus and one backtick closes it; see
	// TestNoBacktickReachesTheGeneratedSources, which caught this text.
	w("\n// Nested-container codecs. A container's element or a dictionary's\n")
	w("// VALUE can itself be a container -- dictionary[string -> dictionary[\n")
	w("// string -> boolean]] is UtilityConstants's own\n")
	w("// default_trigger_target_mask_by_type -- and the wire for that is exactly\n")
	w("// the wire for the outer one, over pairs whose value slot holds another\n")
	w("// (ptr, count). One codec per distinct shape, called from wherever that\n")
	w("// shape appears, so the depth-one walks stayed as they were.\n")
	w("//\n")
	w("// A NESTED DICTIONARY IS AN ORDERED PAIR SLICE TOO. The rule is per\n")
	w("// level: Go randomizes map iteration, this crosses the wire, and a\n")
	w("// lockstep game turns a per-client order into a desync. What you send is\n")
	w("// the order the game sees, at every depth; what you RECEIVE is the host's\n")
	w("// pairs() order, unpromised at every depth.\n")
	for _, n := range g.ctnOrder {
		c := g.ctn[n]

		w("\nfunc decCtn%s(p *byte) %s {\n", n, c.goType)
		w("\th := unsafe.Slice(p, 8)\n")
		w("\tbase := uintptr(*(*uint32)(unsafe.Pointer(&h[0])))\n")
		w("\tn := int(*(*uint32)(unsafe.Pointer(&h[4])))\n")
		w("\tv := make(%s, n)\n", c.goType)
		w("\tfor i := 0; i < n; i++ {\n")
		w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(base+uintptr(i)*%d)), %d)\n",
			c.stride, c.stride)
		val := goLoadElem("d", c.elemOff, c.elemKind, c.elemType, c.elemCtn)
		if c.kind == KindDict {
			w("\t\tv[i] = %s{Key: %s, Val: %s}\n", c.entryType,
				goLoad("d", c.keyOff, c.keyKind), val)
		} else {
			w("\t\tv[i] = %s\n", val)
		}
		w("\t}\n\treturn v\n}\n")

		// The elements go where the host can reach them, which is the guest's
		// own allocator. The bracket is the CALLING member's -- everything that
		// reaches this is inside one.
		w("\nfunc encCtn%s(p *byte, v %s) {\n", n, c.goType)
		w("\th := unsafe.Slice(p, 8)\n")
		w("\tq := fkAlloc(uint32(len(v)) * %d)\n", c.stride)
		w("\tfor i := range v {\n")
		w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(q)+uintptr(i)*%d)), %d)\n",
			c.stride, c.stride)
		if c.kind == KindDict {
			w("\t\t%s\n", goStore("d", c.keyOff, c.keyKind, "v[i].Key"))
			w("\t\t%s\n", goStoreElem("d", c.elemOff, c.elemKind, c.elemCtn, "v[i].Val"))
		} else {
			w("\t\t%s\n", goStoreElem("d", c.elemOff, c.elemKind, c.elemCtn, "v[i]"))
		}
		w("\t}\n")
		w("\t*(*uint32)(unsafe.Pointer(&h[0])) = q\n")
		w("\t*(*uint32)(unsafe.Pointer(&h[4])) = uint32(len(v))\n}\n")

		w("\nfunc valCtn%s(v %s) Value {\n", n, c.goType)
		if c.kind == KindDict {
			w("\tm := make([]KeyValue, len(v))\n")
			w("\tfor i := range v {\n")
			w("\t\tm[i] = KeyValue{Key: %s, Val: %s}\n",
				g.valueOfElem(c.keyKind, "v[i].Key"),
				g.goValueElem(c.elemKind, c.elemCtn, "v[i].Val"))
			w("\t}\n\treturn OfMap(m...)\n}\n")
		} else {
			w("\ta := make([]Value, len(v))\n")
			w("\tfor i := range v {\n")
			w("\t\ta[i] = %s\n", g.goValueElem(c.elemKind, c.elemCtn, "v[i]"))
			w("\t}\n\treturn OfArray(a...)\n}\n")
		}
	}
}
