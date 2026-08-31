package factorio

import (
	"fmt"
	"strings"
)

// THE GO HALF OF THE BULK ATTRIBUTE READ.
//
// `LuaEntityUnitNumberBulk(ents, dst)` reads one attribute off every handle in
// `ents` in ONE crossing, where the loop a guest writes today pays a whole host
// call per element. Which members get one is BulkEligible's, asked once for both
// backends; what a crossing costs is fk_abi.lua's M.bulk_get and agents/abi.md.
//
// A PACKAGE-LEVEL FUNCTION AND NOT A METHOD, which is the one shape decision
// here. There is no receiver: the receivers are the array. A method with an
// unused `o` would read as a member of one object, and the `<Name>Into`
// precedent -- a second binding over one member id, entered in bound[] so
// inheritance forwards it -- cannot be followed, because inheritance forwards by
// FORWARDING and a forwarder cannot retype its own parameter: a []LuaEntity
// handed to a method declaring []LuaControl does not compile. So an inherited
// attribute gets its own bulk function on the inheriting class, rendered by this
// same code with the child's type name, which is what makes
// LuaEntitySurfaceIndexBulk exist at all.

// goBulkElem describes one destination element: the Go type a guest indexes and
// the size that type must occupy for the array to line up with the wire.
type goBulkElem struct {
	typ  string
	size int
}

// goBulkElemFor resolves the destination element type for a bulk-eligible
// member, registering an optional-element struct if this shape needs one.
//
// MANDATORY IS THE SCALAR ITSELF -- `[]uint32`, `[]float64`, `[]Object` -- so the
// common case has no generated type at all and the destination is the slice a
// guest was going to declare anyway.
//
// OPTIONAL IS A GENERATED STRUCT, because the wire element is the getter's own
// return block and that block carries a presence byte: the layout is `has` at 0,
// padding, then the value. There are at most eleven such shapes in the whole
// API, they are named after the Go type rather than after any member, and they
// are registered on demand -- so a description that stops having optional f32
// attributes stops emitting BulkOptFloat32 rather than carrying it forever.
func goBulkElemFor(g *goStructs, f Placed, size int) (goBulkElem, bool) {
	base, ok := goScalar(f.Kind)
	if !ok {
		return goBulkElem{}, false
	}
	if f.HasOffset < 0 {
		return goBulkElem{typ: base, size: size}, true
	}
	name := "BulkOpt" + exportName(base)
	g.bulkOpt(name, base, f, size)
	return goBulkElem{typ: name, size: size}, true
}

// bulkOpt registers one optional-element struct, idempotently.
//
// THE PADDING IS COMPUTED FROM THE LAYOUT rather than assumed, and the size is
// ASSERTED rather than trusted: a Go struct's own alignment rules decide where
// its fields land, and a value whose Go alignment exceeds the wire's would push
// the field past the offset the host writes at. That happens for exactly one
// kind -- u64, which this ABI aligns to 4 and Go may align to 8 -- so that one
// is carried as its two halves with a reader over them rather than as a uint64
// the compiler is free to move. Every other kind's Go alignment is its width,
// which is the wire's.
func (g *goStructs) bulkOpt(name, base string, f Placed, size int) {
	if g.bulkOpts[name] != "" {
		return
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("// %s is one element of a bulk read of an OPTIONAL attribute: the\n", name)
	w("// getter's own return block, which carries a presence byte because the\n")
	w("// description says the value may not be there. Has false means the API\n")
	w("// said nothing, or that this element could not be read at all -- the\n")
	w("// count the bulk call returns is what tells the two apart in aggregate.\n")
	w("type %s struct {\n", name)
	w("\tHas bool\n")
	if pad := f.Offset - 1; pad > 0 {
		w("\t_   [%d]byte\n", pad)
	}
	if f.Kind == KindU64 {
		// TWO HALVES AND A READER. See bulkOpt's header: this ABI aligns a u64
		// to 4 and Go is free to align it to 8, which would move the field off
		// the offset the host writes at -- silently, and only on some targets.
		w("\tLo  uint32\n")
		w("\tHi  uint32\n")
		w("}\n\n")
		w("// V is the value, reassembled from the two halves the wire carries.\n")
		w("func (b %s) V() uint64 { return uint64(b.Hi)<<32 | uint64(b.Lo) }\n\n", name)
	} else {
		w("\tV   %s\n", base)
		w("}\n\n")
	}
	w("var _ [%d]byte = [unsafe.Sizeof(%s{})]byte{}\n\n", size, name)
	g.bulkOpts[name] = b.String()
	g.bulkOptNames = append(g.bulkOptNames, name)
}

// goMemberBulk renders the bulk variant, or reports that this member has none.
//
// `typeName` is the class whose SLICE the objs parameter takes, which is the
// declaring class for a member of its own and the CHILD for an inherited one --
// the same member id either way, because a handle decides the object and
// dispatch never cared which Go type the guest was holding.
func goMemberBulk(g *goStructs, typeName string, m Member) (src, name string, ok bool) {
	f, size, elig := BulkEligible(m)
	if !elig || typeName == "" {
		return "", "", false
	}
	el, ok := goBulkElemFor(g, f, size)
	if !ok {
		return "", "", false
	}
	member := exportName(m.Name)
	if m.Kind == MemberGetHandle {
		member += "Raw"
	}
	if r, has := memberRename[MemberKey(m)]; has && member == r.WasGo {
		member = r.Go
	}
	name = typeName + member + "Bulk"

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	// THREE LINES OF DOC AND NOT THIRTEEN. There are 1,988 of these functions in
	// this file and the contract is identical for every one of them, so it is
	// stated once, at hostBulkGet, and pointed at from here. Repeating the
	// member's own prose would repeat what the ordinary getter above already
	// says, at two thousand times the cost.
	w("// %s reads %s off every handle in objs in ONE host call,\n", name, member)
	w("// writing element i to dst[i] and returning how many it READ. dst must be\n")
	w("// at least len(objs) long; see hostBulkGet for when an element is skipped.\n")
	w("func %s(objs []%s, dst []%s) (int, error) {\n", name, typeName, el.typ)
	w("\tif len(objs) == 0 {\n\t\treturn 0, nil\n\t}\n")
	w("\tif len(dst) < len(objs) {\n\t\treturn 0, StatusBadArgs\n\t}\n")
	w("\tif st := hostBulkGet(%d, ptr((*byte)(unsafe.Pointer(&objs[0]))), "+
		"uint32(len(objs)), ptr((*byte)(unsafe.Pointer(&dst[0]))), "+
		"ptr((*byte)(unsafe.Pointer(&bulkRead)))); st != 0 {\n", m.ID)
	w("\t\treturn 0, Status(st)\n\t}\n")
	w("\treturn int(bulkRead), nil\n")
	w("}\n\n")
	return b.String(), name, true
}
