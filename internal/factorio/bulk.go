package factorio

// THE BULK ATTRIBUTE READ: which members get one, asked once.
//
// A guest that polls -- read one attribute off four hundred entities -- pays a
// whole host call per entity, and a host call is the unit this ABI's cost model
// is denominated in. `fk.bulk_get` pays the crossing once; see fk_abi.lua's
// M.bulk_get for the contract and agents/abi.md for what it measures.
//
// THIS IS THE ONE PLACE THE POPULATION IS DECIDED, and both binding generators
// ask it. Two backends each deciding for themselves is the AD5 shape -- two
// things that agree until they do not -- and here it would be worse than
// usual, because the disagreement's symptom is a bulk binding the RUNTIME
// refuses with ERR_NO_MEMBER rather than a compile error.

// BulkEligible reports whether this member can be read in bulk, and returns the
// single return field and the stride between destination elements.
//
// The rule is entirely a property of the member's own SIGNATURE:
//
//   - it is a readable attribute (GET, or the LuaCustomTable handle twin GETH).
//     A method is not, and that is a scoping decision rather than an
//     impossibility: its arguments would have to become an array of argument
//     blocks, which is a second layout question with a second design behind it.
//     An attribute read has no arguments at all, which is what keeps this small.
//   - it returns EXACTLY ONE value. Nothing readable returns two, so this is a
//     guard rather than a filter, and it is what makes "the destination is an
//     array of the getter's own return block" a sentence with one meaning.
//   - that value is FIXED-WIDTH: a scalar or a handle. HostAllocatesFor is the
//     predicate, reused rather than restated -- it is already this package's
//     statement of "the host writes this without reaching for memory", and it
//     fails CLOSED, so a kind added later is ineligible until somebody says
//     otherwise. That is the direction this feature wants: a new kind silently
//     admitted to a flat array is a wrong answer, and one silently excluded is
//     a missing binding somebody notices.
//
// WHY STRINGS AND CONTAINERS ARE OUT, since both are readable and neither is
// fixed-width. A string element is a (ptr, len) into the 4 KiB scratch region,
// and a thousand of them exhaust it and fall through to fk_alloc per element --
// which is the allocation the region exists to remove, so a bulk string read
// would be slower per element than the per-call form it replaces. A container
// element would be a nested (ptr, count) into the arena, so the destination
// would stop being a flat array of one block, which is the property the whole
// design rests on. Both are refusals with a reason.
func BulkEligible(m Member) (Placed, int, bool) {
	if m.Kind != MemberGet && m.Kind != MemberGetHandle {
		return Placed{}, 0, false
	}
	if m.Unfillable != "" {
		return Placed{}, 0, false
	}
	if len(m.Args) != 0 || len(m.Rets) != 1 {
		return Placed{}, 0, false
	}
	_, rets, err := m.blocks()
	if err != nil || len(rets.Fields) != 1 {
		return Placed{}, 0, false
	}
	f := rets.Fields[0]
	if HostAllocatesFor(f.Kind) {
		return Placed{}, 0, false
	}
	return f, rets.Size, true
}

// BulkKinds is the eligible wire-kind set, for the gate that compares it against
// fk_abi.lua's BULKST table.
//
// Derived from HostAllocatesFor rather than listed, so it cannot drift from the
// predicate above -- the list is the thing being checked, and a second copy of
// it here would make the check compare two copies of one mistake.
func BulkKinds() []Kind {
	var out []Kind
	for k := KindI8; k <= KindDyn; k++ {
		if !HostAllocatesFor(k) {
			out = append(out, k)
		}
	}
	return out
}
