package factorio

import (
	"fmt"
	"strings"
)

// The Rust half of the nested-container recursion. Read gogen_nested.go first:
// the analysis is one analysis and this is the second RENDERING of it, which is
// the whole claim of a second backend.
//
// TWO THINGS DIFFER AND BOTH FOLLOW FROM THE LANGUAGE. A nested dictionary is a
// `BTreeMap` where its own key is `Ord` and a `Vec` of pairs where it is not --
// the choice rustDictType already makes, asked once PER LEVEL rather than once
// per member, so an inner dictionary keyed by a string is a map inside an outer
// one keyed by a tier-2 union that is a pair vector. The determinism argument is
// per level in exactly the same way: a BTreeMap iterates in key order by
// construction, and where the key cannot be ordered the pair vector's order is
// the guest's own. Neither level can produce a per-client walk.
//
// And the codec is a FREE FUNCTION rather than an inherent method, for the
// reason Go's is a package-level func: the type is a `Vec` or a `BTreeMap` and
// neither is this crate's to hang an impl on.

// rustContainer mirrors goContainer. Keyed by the Rust type, which determines
// the layout for the same reason the Go type does; sig() checks that rather
// than assuming it.
type rustContainer struct {
	name   string
	rsType string
	kind   Kind
	stride int

	elemType string
	elemKind Kind
	elemOff  int
	elemCtn  string

	keyType string
	keyKind Kind
	keyOff  int
	// ord is whether a dictionary container became a BTreeMap. Asked at THIS
	// level about THIS level's key.
	ord bool
}

func (c rustContainer) sig() string {
	return fmt.Sprintf("%d/%d/%d@%d/%d@%d/%v/%s", c.kind, c.stride,
		c.elemKind, c.elemOff, c.keyKind, c.keyOff, c.ord, c.elemCtn)
}

// rustCtnIdent turns a container's Rust type into a snake_case identifier
// fragment: `Vec<Object>` is `vec_object`, `BTreeMap<LuaStr, bool>` is
// `map_luastr_bool`, and a pair vector says so -- `Vec<(Value, f64)>` is
// `pair_value_f64` -- so a map and a pair vector over the same two types cannot
// land on one name.
func rustCtnIdent(t string) string {
	s := strings.NewReplacer(
		"Vec<(", "pair_",
		"Vec<", "vec_",
		"BTreeMap<", "map_",
		", ", "_",
		",", "_",
		">", "",
		")", "",
		"(", "",
		" ", "",
	).Replace(t)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rustElemType names one placed element or dictionary value, registering the
// named struct or the nested container's codec it reaches.
func (g *rustStructs) rustElemType(e Placed, spec *FieldSpec, fallback, role string) (rsType, codec, why string, ok bool) {
	switch e.Kind {
	case KindStruct:
		if spec == nil {
			return "", "", role + " of structs with no concept to name it", false
		}
		t, why, ok := g.add(*spec, fallback)
		return t, "", why, ok
	case KindArray, KindDict:
		return g.container(e, spec, fallback)
	}
	t, ok := rustScalar(e.Kind)
	if !ok {
		return "", "", role + " of " + goScalarReason(e.Kind)[len("returns or takes "):], false
	}
	return t, "", "", true
}

// container registers the codec for one nested container.
func (g *rustStructs) container(p Placed, spec *FieldSpec, fallback string) (rsType, codec, why string, ok bool) {
	if p.Elem == nil || p.Stride <= 0 {
		return "", "", "a nested container with no element layout", false
	}
	var sub *FieldSpec
	if spec != nil {
		sub = spec.Elem
	}
	c := rustContainer{kind: p.Kind, stride: p.Stride,
		elemKind: p.Elem.Kind, elemOff: p.Elem.Offset}

	switch p.Kind {
	case KindArray:
		et, ec, why, ok := g.rustElemType(*p.Elem, sub, fallback+"Elem", "an array")
		if !ok {
			return "", "", why, false
		}
		c.elemType, c.elemCtn = et, ec
		c.rsType = "Vec<" + et + ">"

	case KindDict:
		if p.Key == nil {
			return "", "", "a dictionary with no key layout", false
		}
		kt, okk := rustScalar(p.Key.Kind)
		if !okk {
			return "", "", "a dictionary keyed by " +
				goScalarReason(p.Key.Kind)[len("returns or takes "):], false
		}
		vt, vc, why, ok := g.rustElemType(*p.Elem, sub, fallback+"Value", "a dictionary")
		if !ok {
			return "", "", why, false
		}
		c.keyType, c.keyKind, c.keyOff = kt, p.Key.Kind, p.Key.Offset
		c.elemType, c.elemCtn = vt, vc
		c.ord = rustDictOrd(p.Key.Kind)
		c.rsType = rustDictType(kt, vt, p.Key.Kind)

	default:
		return "", "", "a nested " + p.Kind.String(), false
	}

	c.name = rustCtnIdent(c.rsType)
	if old, seen := g.ctn[c.name]; seen {
		if old.sig() != c.sig() {
			return "", "", "a nested container whose Rust type names two layouts", false
		}
		return c.rsType, c.name, "", true
	}
	g.ctn[c.name] = c
	g.ctnOrder = append(g.ctnOrder, c.name)
	return c.rsType, c.name, "", true
}

// rustLoadElem reads one element out of a pair slice: a scalar, a named struct,
// or a nested container through its generated decoder.
func rustLoadElem(buf string, off int, k Kind, typ, ctn string) string {
	if ctn != "" {
		return fmt.Sprintf("dec_ctn_%s(&%s[%d..])", ctn, buf, off)
	}
	return rustLoad(buf, off, k, typ)
}

// rustStoreElem is rustLoadElem's mirror. `byRef` says the accessor is already
// a reference, which every element position is -- they all come out of an
// iterator.
func rustStoreElem(buf string, off int, k Kind, ctn, v string, byRef bool) string {
	if ctn != "" {
		amp := "&"
		if byRef {
			amp = ""
		}
		return fmt.Sprintf("enc_ctn_%s(&mut %s[%d..], %s%s);", ctn, buf, off, amp, v)
	}
	return rustStore(buf, off, k, v, byRef)
}

// rustValueElem renders one element as a tier-2 Value, recursing through the
// container's generated renderer.
func rustValueElem(k Kind, ctn, acc string, ref bool) string {
	if ctn != "" {
		amp := "&"
		if ref {
			amp = ""
		}
		return fmt.Sprintf("val_ctn_%s(%s%s)", ctn, amp, acc)
	}
	return rustValueOf(k, acc, ref)
}

// emitContainers writes the codec triple for every nested container reached.
func (g *rustStructs) emitContainers(w func(string, ...any)) {
	if len(g.ctnOrder) == 0 {
		return
	}
	w("\n/// Nested-container codecs. A container's element or a dictionary's\n")
	w("/// VALUE can itself be a container -- `UtilityConstants`'s\n")
	w("/// `default_trigger_target_mask_by_type` is\n")
	w("/// `dictionary[string -> dictionary[string -> boolean]]` -- and the wire\n")
	w("/// for that is the wire for the outer one, over pairs whose value slot\n")
	w("/// holds another (ptr, count). One codec per distinct shape, so the\n")
	w("/// depth-one walks stayed exactly as they were.\n")
	w("///\n")
	w("/// THE CONTAINER CHOICE IS PER LEVEL: a `BTreeMap` where that level's own\n")
	w("/// key is `Ord`, an ordered `Vec` of pairs where it is not. Either way the\n")
	w("/// wire order is deterministic, which is what a lockstep game needs of a\n")
	w("/// value that crosses it.\n")
	for _, n := range g.ctnOrder {
		c := g.ctn[n]

		w("\npub fn dec_ctn_%s(d: &[u8]) -> %s {\n", n, c.rsType)
		w("    let base = rd_u32(&d[..], 0) as usize;\n")
		w("    let n = rd_u32(&d[..], 4) as usize;\n")
		if c.kind == KindDict && c.ord {
			w("    let mut v = BTreeMap::new();\n")
		} else {
			w("    let mut v = Vec::with_capacity(n);\n")
		}
		w("    for i in 0..n {\n")
		w("        let s = unsafe { core::slice::from_raw_parts((base + i * %d) as *const u8, %d) };\n",
			c.stride, c.stride)
		val := rustLoadElem("s", c.elemOff, c.elemKind, c.elemType, c.elemCtn)
		switch {
		case c.kind == KindArray:
			w("        v.push(%s);\n", val)
		case c.ord:
			w("        v.insert(%s, %s);\n", rustLoad("s", c.keyOff, c.keyKind, c.keyType), val)
		default:
			w("        v.push((%s, %s));\n", rustLoad("s", c.keyOff, c.keyKind, c.keyType), val)
		}
		w("    }\n    v\n}\n")

		w("\npub fn enc_ctn_%s(d: &mut [u8], v: &%s) {\n", n, c.rsType)
		w("    let q = galloc((v.len() * %d) as u32);\n", c.stride)
		switch {
		case c.kind == KindArray:
			w("    for (i, e) in v.iter().enumerate() {\n")
		case c.ord:
			w("    for (i, (k, e)) in v.iter().enumerate() {\n")
		default:
			w("    for (i, (k, e)) in v.iter().map(|t| (&t.0, &t.1)).enumerate() {\n")
		}
		w("        let s = unsafe { core::slice::from_raw_parts_mut((q as usize + i * %d) as *mut u8, %d) };\n",
			c.stride, c.stride)
		if c.kind == KindDict {
			w("        %s\n", rustStore("s", c.keyOff, c.keyKind, "k", true))
		}
		w("        %s\n", rustStoreElem("s", c.elemOff, c.elemKind, c.elemCtn, "e", true))
		w("    }\n")
		w("    wr_u32(&mut d[..], 0, q);\n")
		w("    wr_u32(&mut d[..], 4, v.len() as u32);\n}\n")

		w("\npub fn val_ctn_%s(v: &%s) -> Value {\n", n, c.rsType)
		if c.kind == KindArray {
			w("    let mut a: Vec<Value> = Vec::with_capacity(v.len());\n")
			w("    for e in v.iter() {\n")
			w("        a.push(%s);\n", rustValueElem(c.elemKind, c.elemCtn, "e", true))
			w("    }\n    Value::Array(a)\n}\n")
			continue
		}
		w("    let mut m: Vec<(Value, Value)> = Vec::new();\n")
		if c.ord {
			w("    for (k, e) in v.iter() {\n")
		} else {
			w("    for (k, e) in v.iter().map(|t| (&t.0, &t.1)) {\n")
		}
		w("        m.push((%s, %s));\n", rustValueOf(c.keyKind, "k", true),
			rustValueElem(c.elemKind, c.elemCtn, "e", true))
		w("    }\n    Value::Map(m)\n}\n")
	}
}
