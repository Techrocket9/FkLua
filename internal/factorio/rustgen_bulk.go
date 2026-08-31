package factorio

import (
	"fmt"
	"strings"
)

// THE RUST HALF OF THE BULK ATTRIBUTE READ, and gogen_bulk.go's mirror.
//
// Deliberately the same shape as the Go one, down to which questions are asked
// in which order, because the wire is the same and two backends that answer it
// differently is the AD5 defect this repo keeps meeting. What differs is what
// the language can say: a slice arrives as &[T] and &mut [T] rather than as two
// slices whose lengths a caller has to keep straight, and the count comes back
// in a Result<usize, Status> rather than beside an error.

// rustBulkElemFor resolves the destination element type, registering an
// optional-element struct if this shape needs one. gogen_bulk.go's twin, and
// BulkEligible is the shared half neither backend re-decides.
func rustBulkElemFor(g *rustStructs, f Placed, size int) (string, bool) {
	base, ok := rustScalar(f.Kind)
	if !ok {
		return "", false
	}
	if f.HasOffset < 0 {
		return base, true
	}
	name := "BulkOpt" + exportName(base)
	g.bulkOpt(name, base, f, size)
	return name, true
}

// bulkOpt registers one optional-element struct, idempotently.
//
// #[repr(C)] IS THE WHOLE OF IT. A repr(Rust) struct's field order and padding
// are the compiler's to choose, and this one has to match a block the host
// writes: presence byte at 0, padding, then the value. The explicit padding
// field is what makes the offsets the layout's rather than the compiler's, and
// the size assertion is what says so at build time rather than in a comment.
//
// u64 is carried as its two halves for the same reason gogen_bulk.go carries it
// that way: this ABI aligns a u64 to 4 and Rust aligns it to 8, so a u64 field
// would sit two bytes past where the host wrote it.
func (g *rustStructs) bulkOpt(name, base string, f Placed, size int) {
	if g.bulkOpts[name] != "" {
		return
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("/// One element of a bulk read of an OPTIONAL attribute: the getter's own\n")
	w("/// return block, which carries a presence byte because the description\n")
	w("/// says the value may not be there. `has` false means the API said\n")
	w("/// nothing, or that this element could not be read at all -- the count\n")
	w("/// the bulk call returns is what tells the two apart in aggregate.\n")
	w("#[derive(Copy, Clone, Debug, Default)]\n")
	w("#[repr(C)]\npub struct %s {\n", name)
	w("    pub has: bool,\n")
	if pad := f.Offset - 1; pad > 0 {
		w("    _pad: [u8; %d],\n", pad)
	}
	if f.Kind == KindU64 {
		w("    pub lo: u32,\n    pub hi: u32,\n}\n\n")
		w("impl %s {\n", name)
		w("    /// The value, reassembled from the two halves the wire carries.\n")
		w("    pub fn v(&self) -> u64 { ((self.hi as u64) << 32) | self.lo as u64 }\n")
		w("}\n\n")
	} else {
		w("    pub v: %s,\n}\n\n", base)
	}
	w("const _: () = assert!(core::mem::size_of::<%s>() == %d);\n\n", name, size)
	g.bulkOpts[name] = b.String()
	g.bulkOptNames = append(g.bulkOptNames, name)
}

// rustSnake turns a generated TYPE name into a snake_case identifier prefix.
//
// rustName lowercases and does not break at a case boundary, which is right for
// a member name -- the description already spells those in snake_case -- and
// wrong for a class, where LuaEntity would come out `luaentity`. A free function
// named after a type has to spell the type the way Rust would.
func rustSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// rustMemberBulk renders the bulk variant, or reports that this member has none.
//
// A FREE FUNCTION AND NOT AN INHERENT METHOD, for gogen_bulk.go's reason: there
// is no receiver, because the receivers are the slice. It is emitted OUTSIDE the
// class's impl block, which is why the caller has to close that block first.
//
// AND IT TAKES A &[Object], which is what every array-of-handles return in the
// API is -- rust_scalar renders KindHandle as Object and there is no per-class
// element type anywhere, so a &[LuaEntity] would not typecheck against the one
// thing a guest ever holds.
func rustMemberBulk(g *rustStructs, typeName string, m Member) (src, name string, ok bool) {
	f, size, elig := BulkEligible(m)
	if !elig || typeName == "" {
		return "", "", false
	}
	el, ok := rustBulkElemFor(g, f, size)
	if !ok {
		return "", "", false
	}
	member := rustName(m.Name)
	if m.Kind == MemberGetHandle {
		member += "_raw"
	}
	if r, has := memberRename[MemberKey(m)]; has && member == r.WasRust {
		member = r.Rust
	}
	// STRIP THE RAW-IDENTIFIER ESCAPE, because this name is a COMPOUND. rustName
	// answers `r#type` for a member called `type`, which is right when the whole
	// identifier is that word and is a syntax error in the middle of one -- and
	// the compound cannot be a keyword itself, since it ends in `_bulk`.
	member = strings.TrimPrefix(member, "r#")
	name = rustSnake(typeName) + "_" + member + "_bulk"

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("/// Reads %s::%s off every handle in objs in ONE host call, writing\n",
		typeName, member)
	w("/// element i to dst[i] and returning how many it READ. dst must be at\n")
	w("/// least objs.len() long, and objs is a &[Object] because that is what a\n")
	w("/// search returns; see fk_bulk_get for when an element is skipped.\n")
	w("pub fn %s(objs: &[Object], dst: &mut [%s]) -> Result<usize, Status> {\n",
		name, el)
	w("    if objs.is_empty() {\n        return Ok(0);\n    }\n")
	w("    if dst.len() < objs.len() {\n        return Err(Status(Status::BAD_ARGS));\n    }\n")
	w("    let st = unsafe {\n")
	w("        fk_bulk_get(%d, objs.as_ptr() as u32, objs.len() as u32,\n", m.ID)
	w("            dst.as_mut_ptr() as u32, core::ptr::addr_of_mut!(BULK_READ) as u32)\n")
	w("    };\n")
	w("    if st != 0 {\n        return Err(Status(st));\n    }\n")
	w("    Ok(unsafe { BULK_READ } as usize)\n")
	w("}\n\n")
	return b.String(), name, true
}
