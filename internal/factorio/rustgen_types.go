package factorio

import (
	"fmt"
	"strings"
)

// Type resolution and the struct collector for the Rust backend.
//
// Deliberately the same SHAPE as gogen.go's: the questions are identical
// because the wire is, and answering them differently would mean the layout
// machinery had leaked a language into itself.

func rustFieldFor(g *rustStructs, p Placed, specs []FieldSpec, i int, fallback string) (string, string, bool) {
	if p.Kind == KindArray {
		t, _, _, why, ok := rustArrayElem(g, p, specs, i, fallback)
		if !ok {
			return "", why, false
		}
		return "Vec<" + t + ">", "", true
	}
	if p.Kind == KindDict {
		kt, vt, key, _, _, why, ok := rustDictKV(g, p, specs, i, fallback)
		if !ok {
			return "", why, false
		}
		return rustDictType(kt, vt, key.Kind), "", true
	}
	if p.Kind != KindStruct {
		t, ok := rustScalar(p.Kind)
		if !ok {
			return "", goScalarReason(p.Kind), false
		}
		return t, "", true
	}
	if i >= len(specs) {
		return "", "a struct field with no concept to name it", false
	}
	return g.add(specs[i], fallback)
}

// rustArrayElem resolves an array's element type, and the codec name when that
// element is ITSELF a container. See rustgen_nested.go: a nested array used to
// come back "an array of an array", which was true of this generator and never
// of the wire.
func rustArrayElem(g *rustStructs, p Placed, specs []FieldSpec, i int, fallback string) (string, Placed, string, string, bool) {
	var elem Placed
	if p.Elem == nil || p.Stride <= 0 {
		return "", elem, "", "an array with no element layout", false
	}
	elem = *p.Elem
	var sub *FieldSpec
	if i < len(specs) {
		sub = specs[i].Elem
	}
	t, codec, why, ok := g.rustElemType(elem, sub, fallback+"Elem", "an array")
	if !ok {
		return "", elem, "", why, false
	}
	return t, elem, codec, "", true
}

// rustDictKV resolves a dictionary's key and value types, and the codec name
// when the VALUE is itself a container. The key does not recurse; see
// rustgen_nested.go.
func rustDictKV(g *rustStructs, p Placed, specs []FieldSpec, i int, fallback string) (kt, vt string, key, val Placed, valCodec, why string, ok bool) {
	if p.Key == nil || p.Elem == nil || p.Stride <= 0 {
		return "", "", key, val, "", "a dictionary with no key/value layout", false
	}
	key, val = *p.Key, *p.Elem
	// A NON-Ord KEY IS NOT A REFUSAL ANY MORE, and nothing here has to know
	// which container it becomes. `Value` is not Ord -- it holds f64, which is
	// only PartialOrd -- so it cannot key a BTreeMap; the caller asks
	// rustDictType and gets the ordered pair vector instead. That mirrors the Go
	// side's "the caller picks the container", and it is what binds
	// game.surfaces, game.players and game.forces, whose key is
	// `uint32 | string` and therefore KindDyn.
	k, ok2 := rustScalar(key.Kind)
	if !ok2 {
		return "", "", key, val, "", "a dictionary keyed by " +
			goScalarReason(key.Kind)[len("returns or takes "):], false
	}
	var sub *FieldSpec
	if i < len(specs) {
		sub = specs[i].Elem
	}
	v, vc, why, ok3 := g.rustElemType(val, sub, fallback+"Value", "a dictionary")
	if !ok3 {
		return "", "", key, val, "", why, false
	}
	return k, v, key, val, vc, "", true
}

// rustDictOrd reports whether a dictionary key can key a BTreeMap.
//
// `Value` holds an f64 and a Vec, so it is neither Ord nor Hash; f32 and f64
// are only PartialOrd. Everything else the API keys a dictionary by -- string,
// the integer widths, a handle -- is Ord, and Object derives it in the runtime
// precisely so five members can return one.
func rustDictOrd(k Kind) bool {
	switch k {
	case KindDyn, KindF32, KindF64:
		return false
	}
	return true
}

// rustDictType names the container a dictionary becomes, and the choice is the
// key's alone.
//
// A BTreeMap WHERE THE KEY ALLOWS IT, which is the one place this backend is
// straightforwardly better than the Go one: a BTreeMap iterates in key order,
// so a dictionary that crosses in BOTH directions -- a struct field is an
// argument as often as a return -- has a deterministic wire order by
// construction. That is what forced Go to a pair slice and what
// `rustgen.go`'s header claims Rust can express; this is the claim being
// cashed.
//
// AND A Vec OF PAIRS WHERE IT DOES NOT, rather than a deferral. A tuple is the
// pair type -- no name to invent, none to deduplicate, and `for (k, v) in ...`
// where Go needs `e.Key`/`e.Val` -- and it is the same shape the hand-written
// runtime already chose for tier 2's own `Value::Map`. The order is then the
// host's, which is the only order there is when nothing can sort it.
func rustDictType(kt, vt string, key Kind) string {
	if rustDictOrd(key) {
		return "BTreeMap<" + kt + ", " + vt + ">"
	}
	return "Vec<(" + kt + ", " + vt + ")>"
}

// rustStore writes one field. `byRef` is true when the value is a reference
// from an iterator, which changes how a String is read.
func rustStore(buf string, off int, k Kind, v string, byRef bool) string {
	deref := ""
	if byRef {
		deref = "*"
	}
	switch k {
	case KindStruct:
		return fmt.Sprintf("%s.encode_at(&mut %s[%d..]);", v, buf, off)
	case KindString:
		// put_str takes BYTES, so one call site serves every shape a string
		// reaches this from: a `&str` argument, a `LuaStr` field, and either of
		// them behind a reference from an iterator -- `.as_bytes()` exists on
		// all four and a method call auto-derefs. That is why there is no amp
		// juggling here where the other kinds need it.
		return fmt.Sprintf("put_str(&mut %s[..], %d, %s.as_bytes());", buf, off, v)
	case KindHandle:
		// No deref: field access auto-derefs through a reference, and `*e.0`
		// would parse as `*(e.0)` -- dereferencing the u32 rather than the
		// handle.
		return fmt.Sprintf("wr_u32(&mut %s[..], %d, %s.0);", buf, off, v)
	case KindBool:
		return fmt.Sprintf("%s[%d] = if %s%s { 1 } else { 0 };", buf, off, deref, v)
	case KindDyn:
		// write_dyn borrows. An iterator already hands us a reference; an owned
		// parameter has to be borrowed here.
		amp := "&"
		if byRef {
			amp = ""
		}
		return fmt.Sprintf("write_dyn(&mut %s[%d..], %s%s);", buf, off, amp, v)
	case KindF64:
		return fmt.Sprintf("%s[%d..%d].copy_from_slice(&(%s%s).to_le_bytes());", buf, off, off+8, deref, v)
	case KindF32:
		return fmt.Sprintf("%s[%d..%d].copy_from_slice(&(%s%s).to_le_bytes());", buf, off, off+4, deref, v)
	case KindU64:
		return fmt.Sprintf("%s[%d..%d].copy_from_slice(&(%s%s).to_le_bytes());", buf, off, off+8, deref, v)
	}
	t, _ := rustScalar(k)
	sz := k.Size()
	return fmt.Sprintf("%s[%d..%d].copy_from_slice(&(%s%s as %s).to_le_bytes());",
		buf, off, off+sz, deref, v, t)
}

// rustLoad reads one field back.
func rustLoad(buf string, off int, k Kind, typ string) string {
	switch k {
	case KindStruct:
		return fmt.Sprintf("%s::decode_at(&%s[%d..])", typ, buf, off)
	case KindString:
		return fmt.Sprintf("get_str(&%s[..], %d)", buf, off)
	case KindHandle:
		return fmt.Sprintf("Object(rd_u32(&%s[..], %d))", buf, off)
	case KindBool:
		return fmt.Sprintf("%s[%d] != 0", buf, off)
	case KindDyn:
		return fmt.Sprintf("read_dyn(&%s[%d..])", buf, off)
	}
	t, _ := rustScalar(k)
	sz := k.Size()
	return fmt.Sprintf("%s::from_le_bytes(%s[%d..%d].try_into().unwrap())", t, buf, off, off+sz)
}

// ---------------------------------------------------------------------------
// Struct types
// ---------------------------------------------------------------------------

type rustStructs struct {
	byName map[string]StructBlock
	order  []string
	// dynValue counts the structs emitDynReaders matched -- gogen.go's twin.
	dynValue  int
	blocked   map[string]bool
	fieldType map[string]string
	elem      map[string]structArray
	// dict describes a DICTIONARY field, keyed "Parent.field", for the same
	// reason elem does: the stride and the two offsets are in the layout and
	// the value's TYPE NAME is in the spec, and neither is recoverable from the
	// other afterwards.
	dict map[string]structRustDict
	// ctn holds the NESTED containers, keyed by the Rust type. See
	// rustgen_nested.go.
	ctn      map[string]rustContainer
	ctnOrder []string
	// note maps "Parent.field" to a doc comment the field cannot state for
	// itself. Only LuaLazyLoadedValue uses it: such a field crosses as a bare
	// Object, so without a line saying what the handle IS and what get() yields,
	// a reader has a u32 and no way to find out. See FieldSpec::LazyPayload.
	note map[string]string
}

// structRustDict is one dictionary-typed struct field. A dictionary is an array
// of key/value PAIRS on the wire, so this is structArray plus the key: stride
// is the pair's padded size and both offsets are placed WITHIN the pair.
type structRustDict struct {
	rsType           string // the whole container, BTreeMap<..> or Vec<(..)>
	keyType, valType string
	keyKind, valKind Kind
	keyOff, valOff   int
	stride           int
	// ord is whether the container is a BTreeMap; see rustDictType.
	ord bool
	// valCtn is the codec of a VALUE that is itself a container.
	valCtn string
}

func newRustStructs() *rustStructs {
	return &rustStructs{
		byName:    map[string]StructBlock{},
		blocked:   map[string]bool{},
		fieldType: map[string]string{},
		elem:      map[string]structArray{},
		dict:      map[string]structRustDict{},
		ctn:       map[string]rustContainer{},
		note:      map[string]string{},
	}
}

// taken reports a name already claimed -- or already refused -- by a concept
// type, so an event payload cannot quietly reuse someone else's layout.
func (g *rustStructs) taken(name string) bool {
	_, done := g.byName[name]
	return done || g.blocked[name]
}

func (g *rustStructs) add(f FieldSpec, fallback string) (string, string, bool) {
	name := exportName(f.TypeName)
	if f.TypeName == "" {
		name = fallback
	}
	if g.blocked[name] {
		return "", "struct " + name + " is itself deferred", false
	}
	if _, done := g.byName[name]; done {
		return name, "", true
	}
	blk, err := LayoutStruct(f.Struct)
	if err != nil {
		g.blocked[name] = true
		return "", "struct " + name + " has no memory layout", false
	}
	g.byName[name] = blk
	g.order = append(g.order, name)

	fail := func(why string) (string, string, bool) {
		g.blocked[name] = true
		delete(g.byName, name)
		// AND OUT OF THE EMISSION ORDER. The name was reserved above so a type
		// reachable from itself does not spin; leaving it here made emit() read
		// a zero StructBlock out of byName and write `pub struct X {}` under the
		// concept's REAL name, with a codec that reads and writes ZERO BYTES and
		// returns a default value while the wire holds sixteen. `CollisionMask`
		// and `MapGenSettings` both shipped that way. It is the same defect the
		// Go generator carried and fixed -- see gogen.go's twin comment -- and
		// the reason it survived a second time in this backend is that it is
		// invisible until something references the empty type: every member that
		// would have was deferred for the same underlying reason.
		//
		// Reported from the field as fklua-ports' autodeconstruct AD5, which is
		// what makes this the second sighting rather than a review finding.
		for i, n := range g.order {
			if n == name {
				g.order = append(g.order[:i], g.order[i+1:]...)
				break
			}
		}
		return "", why, false
	}
	for i, sub := range f.Struct {
		// A lazily-loaded value's payload type, which the field's own Rust type
		// cannot state: it is an Object like any other handle. Recorded here
		// rather than recovered at emit time for the same reason fieldType is --
		// Placed does not carry it.
		if sub.LazyPayload != "" {
			g.note[name+"."+sub.Name] = sub.LazyPayload
		}
		if sub.Kind == KindStruct {
			child, why, ok := g.add(sub, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.fieldType[name+"."+sub.Name] = child
			continue
		}
		if sub.Kind == KindArray {
			if i >= len(blk.Fields) {
				return fail("a struct array field with no placed layout")
			}
			p := blk.Fields[i]
			et, elem, ctn, why, ok := rustArrayElem(g, p, f.Struct, i, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.elem[name+"."+sub.Name] = structArray{
				goType: et, kind: elem.Kind, off: elem.Offset, stride: p.Stride,
				ctn: ctn,
			}
			continue
		}
		if sub.Kind == KindDict {
			// A DICTIONARY NESTED IN A STRUCT, which was the last shape this
			// collector refused and the one that cost the most: a top-level
			// dictionary RETURN rendered fine, so only the nesting failed, and
			// it took `CollisionMask` and `MapGenSettings` down with it --
			// 17 of the 47 deferrals between them (fklua-ports' autodeconstruct
			// AD4). The Lua side never had the gap: read_value routes K_DICT to
			// the same read_array walk an array uses.
			if i >= len(blk.Fields) {
				return fail("a struct dictionary field with no placed layout")
			}
			p := blk.Fields[i]
			kt, vt, key, val, vc, why, ok := rustDictKV(g, p, f.Struct, i, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.dict[name+"."+sub.Name] = structRustDict{
				rsType:  rustDictType(kt, vt, key.Kind),
				keyType: kt, valType: vt,
				keyKind: key.Kind, valKind: val.Kind,
				keyOff: key.Offset, valOff: val.Offset, stride: p.Stride,
				ord: rustDictOrd(key.Kind), valCtn: vc,
			}
			continue
		}
		if _, ok := rustScalar(sub.Kind); !ok {
			return fail(goScalarReason(sub.Kind))
		}
	}
	return name, "", true
}

func (g *rustStructs) fieldTypeOf(owner string, p Placed) string {
	if p.Kind == KindDict {
		if e, ok := g.dict[owner+"."+p.Name]; ok {
			return e.rsType
		}
		return "Vec<(u8, u8)>"
	}
	if p.Kind == KindArray {
		if e, ok := g.elem[owner+"."+p.Name]; ok {
			return "Vec<" + e.goType + ">"
		}
		return "Vec<u8>"
	}
	if p.Kind == KindStruct {
		if n, ok := g.fieldType[owner+"."+p.Name]; ok {
			return n
		}
		return "()"
	}
	t, _ := rustScalar(p.Kind)
	return t
}

func (g *rustStructs) emit(w func(string, ...any)) {
	// The nested-container codecs first: they are free functions the struct
	// impls below call, and a reader meeting `dec_ctn_map_luastr_bool` inside
	// `UtilityConstants::decode_at` should have just read it.
	g.emitContainers(w)
	for _, name := range g.order {
		blk := g.byName[name]
		w("\n/// Mirrors the API type of the same name, laid out to match the wire.\n")
		w("#[derive(Clone, Debug, Default)]\npub struct %s {\n", name)
		for _, p := range blk.Fields {
			t := g.fieldTypeOf(name, p)
			// A container is not wrapped: a Vec and a BTreeMap are already
			// empty-able, and Option<Vec<T>> would make a caller unwrap twice to
			// say the same thing. The same rule the member signatures use.
			if p.HasOffset >= 0 && p.Kind != KindArray && p.Kind != KindDict {
				t = "Option<" + t + ">"
			}
			// A lazily-loaded value is an Object and reads like every other
			// handle, so say what it is and what it yields. The engine builds
			// the payload only when get() is called; not calling it is free.
			if note := g.note[name+"."+p.Name]; note != "" {
				w("    /// A `LuaLazyLoadedValue<%s>`.\n", note)
				w("    ///\n")
				w("    /// The payload is NOT crossed with the event: wrap this in\n")
				w("    /// [`LuaLazyLoadedValue`] and call `get()` to make the engine\n")
				w("    /// build it, which is the only point at which it costs\n")
				w("    /// anything. `get()` returns a tier-2 `Value`; for the type\n")
				w("    /// above that is a map whose values are `Object`s.\n")
				w("    ///\n")
				w("    /// It is valid ONLY during this dispatch. Retaining it gives a\n")
				w("    /// live handle over a dead LuaObject, which the next call\n")
				w("    /// reports as `Error::Invalid`.\n")
			}
			w("    pub %s: %s,\n", rustName(p.Name), t)
		}
		w("}\n\n")

		// AN ABSENT OPTIONAL CONTAINER IS ABSENT, and until the typed-args
		// round nothing in this repo encoded one. This branch set the presence
		// byte UNCONDITIONALLY, so an optional array or dictionary field a Rust
		// guest never touched crossed as PRESENT AND EMPTY -- `tags = {}` on
		// LuaGuiElement::add where the Go guest sent no `tags` key at all. The
		// two backends therefore called the engine differently from the same
		// spec, which is the AD5 shape and is what a mirror test is for.
		//
		// EMPTY MEANS ABSENT HERE, and it has to: Go's optional container keeps
		// its own nilable type, so nil is absent and an empty slice is
		// present-and-empty, while a BTreeMap and a Vec have no nil and cannot
		// say both. Given one of the two readings, absent is the one this ABI's
		// own rule names -- an absent optional is left alone -- and it is the
		// one a caller who wrote nothing meant. A Rust guest that really wants
		// an empty container sent has no expression for it; that is a stated
		// residual rather than a silent one, and closing it means
		// Option<BTreeMap<..>> in the generated struct.
		w("impl %s {\n", name)
		w("    pub fn encode_at(&self, d: &mut [u8]) {\n")
		w("        for b in d[..%d].iter_mut() { *b = 0; }\n", blk.Size)
		for _, p := range blk.Fields {
			fn := rustName(p.Name)
			if e, ok := g.elem[name+"."+p.Name]; ok && p.Kind == KindArray {
				if p.HasOffset >= 0 {
					w("        if !self.%s.is_empty() { d[%d] = 1; }\n", fn, p.HasOffset)
				}
				w("        let p = galloc((self.%s.len() * %d) as u32);\n", fn, e.stride)
				w("        for (i, e) in self.%s.iter().enumerate() {\n", fn)
				w("            let s = unsafe { core::slice::from_raw_parts_mut((p as usize + i * %d) as *mut u8, %d) };\n",
					e.stride, e.stride)
				w("            %s\n        }\n",
					rustStoreElem("s", e.off, e.kind, e.ctn, "e", true))
				w("        wr_u32(&mut d[..], %d, p);\n", p.Offset)
				w("        wr_u32(&mut d[..], %d, self.%s.len() as u32);\n", p.Offset+4, fn)
				continue
			}
			if e, ok := g.dict[name+"."+p.Name]; ok && p.Kind == KindDict {
				// THE SAME WALK AS THE ARRAY ABOVE, over PAIRS -- which is the
				// observation that lets fk_abi.lua share one decoder between the
				// two, and it holds one level further up here as well: a
				// BTreeMap's iter() yields (&K, &V) and a Vec<(K, V)>'s yields
				// &(K, V), and `for (i, (k, v)) in x.iter().enumerate()` binds
				// k: &K and v: &V either way. So the emitted body does not
				// depend on which container rustDictType chose.
				if p.HasOffset >= 0 {
					w("        if !self.%s.is_empty() { d[%d] = 1; }\n", fn, p.HasOffset)
				}
				w("        let p = galloc((self.%s.len() * %d) as u32);\n", fn, e.stride)
				w("        for (i, (k, v)) in self.%s.iter().enumerate() {\n", fn)
				w("            let s = unsafe { core::slice::from_raw_parts_mut((p as usize + i * %d) as *mut u8, %d) };\n",
					e.stride, e.stride)
				w("            %s\n", rustStore("s", e.keyOff, e.keyKind, "k", true))
				w("            %s\n        }\n",
					rustStoreElem("s", e.valOff, e.valKind, e.valCtn, "v", true))
				w("        wr_u32(&mut d[..], %d, p);\n", p.Offset)
				w("        wr_u32(&mut d[..], %d, self.%s.len() as u32);\n", p.Offset+4, fn)
				continue
			}
			if p.HasOffset >= 0 {
				w("        if let Some(v) = &self.%s {\n            d[%d] = 1;\n", fn, p.HasOffset)
				w("            %s\n        }\n", rustStore("d", p.Offset, p.Kind, "v", true))
				continue
			}
			w("        %s\n", rustStore("d", p.Offset, p.Kind, "self."+fn, false))
		}
		w("    }\n\n")

		// A table concept with no fields exists in the API, and its decoder
		// would leave `d` unused -- which rustc warns on. Same shape as the Go
		// generator's `_ = p`.
		arg := "d"
		if len(blk.Fields) == 0 {
			arg = "_d"
		}
		mut_ := "mut "
		if len(blk.Fields) == 0 {
			mut_ = "" // nothing to assign into, so `mut` would warn too
		}
		w("    pub fn decode_at(%s: &[u8]) -> Self {\n        let %sv = Self::default();\n", arg, mut_)
		for _, p := range blk.Fields {
			fn := rustName(p.Name)
			if e, ok := g.elem[name+"."+p.Name]; ok && p.Kind == KindArray {
				if p.HasOffset >= 0 {
					w("        if d[%d] != 0 {\n", p.HasOffset)
				}
				w("        {\n            let base = rd_u32(&d[..], %d) as usize;\n", p.Offset)
				w("            let n = rd_u32(&d[..], %d) as usize;\n", p.Offset+4)
				w("            for i in 0..n {\n")
				w("                let s = unsafe { core::slice::from_raw_parts((base + i * %d) as *const u8, %d) };\n",
					e.stride, e.stride)
				w("                v.%s.push(%s);\n            }\n        }\n",
					fn, rustLoadElem("s", e.off, e.kind, e.goType, e.ctn))
				if p.HasOffset >= 0 {
					w("        }\n")
				}
				continue
			}
			if e, ok := g.dict[name+"."+p.Name]; ok && p.Kind == KindDict {
				if p.HasOffset >= 0 {
					w("        if d[%d] != 0 {\n", p.HasOffset)
				}
				w("        {\n            let base = rd_u32(&d[..], %d) as usize;\n", p.Offset)
				w("            let n = rd_u32(&d[..], %d) as usize;\n", p.Offset+4)
				w("            for i in 0..n {\n")
				w("                let s = unsafe { core::slice::from_raw_parts((base + i * %d) as *const u8, %d) };\n",
					e.stride, e.stride)
				key := rustLoad("s", e.keyOff, e.keyKind, e.keyType)
				val := rustLoadElem("s", e.valOff, e.valKind, e.valType, e.valCtn)
				if e.ord {
					w("                v.%s.insert(%s, %s);\n            }\n        }\n", fn, key, val)
				} else {
					w("                v.%s.push((%s, %s));\n            }\n        }\n", fn, key, val)
				}
				if p.HasOffset >= 0 {
					w("        }\n")
				}
				continue
			}
			if p.HasOffset >= 0 {
				w("        if d[%d] != 0 {\n            v.%s = Some(%s);\n        }\n",
					p.HasOffset, fn, rustLoad("d", p.Offset, p.Kind, g.fieldTypeOf(name, p)))
				continue
			}
			w("        v.%s = %s;\n", fn, rustLoad("d", p.Offset, p.Kind, g.fieldTypeOf(name, p)))
		}
		w("        v\n    }\n")
		g.emitToValue(w, name, blk)
		g.emitDynReaders(w, name, blk)
		w("}\n")
	}
}

// emitDynReaders writes typed readers over a struct whose whole content is ONE
// tier-2 value. See gogen.go's emitDynReaders for the argument; the shape is
// ModSetting's and the predicate is IsDynValueStruct, asked by both generators
// so they cannot disagree about what matched.
//
// Named for the Value accessors they delegate to, which in Rust keep their
// as_ prefix because nothing there collides.
func (g *rustStructs) emitDynReaders(w func(string, ...any), name string, blk StructBlock) {
	if !IsDynValueStruct(blk) {
		return
	}
	g.dynValue++
	fn := rustName(blk.Fields[0].Name)
	w("\n    /// The one tier-2 value this carries, read as the type the tag\n")
	w("    /// names. None for every other tag, which is the contract\n")
	w("    /// [Value::as_bool] and its siblings have -- these delegate.\n")
	for _, m := range [][2]string{
		{"as_bool", "bool"}, {"as_num", "f64"}, {"as_str", "&LuaStr"}, {"as_obj", "Object"},
	} {
		w("    pub fn %s(&self) -> Option<%s> { self.%s.%s() }\n", m[0], m[1], fn, m[0])
	}
}

// emitToValue writes the tier-2 constructor for one generated struct. See
// gogen.go's emitToValue for the whole argument; the short form is that a
// UNION-typed struct field has no generated type and arrives as a raw `Value`,
// so a guest fills `LogisticFilter::value` by writing the Lua table out with
// three string keys that nothing checks -- reported by four of the seven ports
// (R4 on LogisticFilter.value and SignalID, FTS3 on WaitCondition.condition,
// resource-marker on MineableProperties.products, nixie-tubes on
// ScriptRenderTarget). The typed struct already exists; this is the way to spend
// it. An absent optional is OMITTED rather than sent as Nil, which is what an
// absent optional means everywhere else in this ABI.
func (g *rustStructs) emitToValue(w func(string, ...any), name string, blk StructBlock) {
	w("\n    /// Renders this as the tier-2 table the engine expects, so a\n")
	w("    /// union-typed field can be filled from the typed struct instead of\n")
	w("    /// from hand-written key strings. An absent optional is omitted.\n")
	w("    pub fn to_value(&self) -> Value {\n")
	if len(blk.Fields) == 0 {
		w("        Value::Map(Vec::new())\n    }\n")
		return
	}
	w("        let mut kv: Vec<(Value, Value)> = Vec::with_capacity(%d);\n", len(blk.Fields))
	for _, p := range blk.Fields {
		fn := rustName(p.Name)
		switch {
		case p.Kind == KindArray:
			e := g.elem[name+"."+p.Name]
			w("        if !self.%s.is_empty() {\n", fn)
			w("            let mut a: Vec<Value> = Vec::with_capacity(self.%s.len());\n", fn)
			w("            for e in self.%s.iter() {\n", fn)
			w("                a.push(%s);\n", rustValueElem(e.kind, e.ctn, "e", true))
			w("            }\n")
			w("            kv.push((Value::Str(LuaStr::from(%q)), Value::Array(a)));\n        }\n", p.Name)
		case p.Kind == KindDict:
			e := g.dict[name+"."+p.Name]
			w("        if !self.%s.is_empty() {\n", fn)
			w("            let mut m: Vec<(Value, Value)> = Vec::new();\n")
			if e.ord {
				w("            for (k, v) in self.%s.iter() {\n", fn)
			} else {
				w("            for (k, v) in self.%s.iter().map(|p| (&p.0, &p.1)) {\n", fn)
			}
			w("                m.push((%s, %s));\n",
				rustValueOf(e.keyKind, "k", true),
				rustValueElem(e.valKind, e.valCtn, "v", true))
			w("            }\n")
			w("            kv.push((Value::Str(LuaStr::from(%q)), Value::Map(m)));\n        }\n", p.Name)
		case p.HasOffset >= 0:
			w("        if let Some(x) = &self.%s {\n", fn)
			w("            kv.push((Value::Str(LuaStr::from(%q)), %s));\n        }\n",
				p.Name, rustValueOf(p.Kind, "x", true))
		default:
			w("        kv.push((Value::Str(LuaStr::from(%q)), %s));\n",
				p.Name, rustValueOf(p.Kind, "self."+fn, false))
		}
	}
	w("        Value::Map(kv)\n    }\n")
}

// rustValueOf renders one value as a tier-2 Value. `ref_` says the accessor is
// already a reference, which decides whether a copy needs a deref.
func rustValueOf(k Kind, acc string, ref bool) string {
	deref := ""
	if ref {
		deref = "*"
	}
	switch k {
	case KindStruct:
		return acc + ".to_value()"
	case KindString:
		return "Value::Str(" + acc + ".clone())"
	case KindBool:
		return "Value::Bool(" + deref + acc + ")"
	case KindHandle:
		return "Value::Obj(" + deref + acc + ")"
	case KindDyn:
		return acc + ".clone()"
	}
	return "Value::Number(" + deref + acc + " as f64)"
}

// name0 without the Set prefix, shared with the Go backend's naming.
var _ = strings.TrimSpace
