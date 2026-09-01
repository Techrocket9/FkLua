package factorio

// A UNION THAT COLLAPSES TO A HANDLE, SAID WHERE THE AUTHOR READS IT.
//
// canonicalUnion's shape B -- one class plus scalar identifiers -- resolves to
// the class, so `LuaForceCreateSpacePlatformArgs.planet` is an `Object` where
// the description says `SpaceLocationID`, which is
// `LuaSpaceLocationPrototype | string`. The string arm is unreachable from a
// guest and ANY object handle type-checks: a `LuaPlanet` compiles and the engine
// refuses it at runtime. Reported by WormholeBelts as item 4 of its gaps ledger.
//
// THE COLLAPSE ITSELF IS NOT CHANGED, and the reasoning is recorded in
// canonicalUnion's own header and in mapWriteType's ("a parameter has the engine
// accepting the handle and the guest able to get one, so nothing there is
// unreachable"). Two roads were open and both are refused:
//
//   - MAKE THE FIELD TIER 2, the way mapWriteType does for an attribute's write
//     half. That is a WIRE change at every takes-table member carrying a
//     ForceID-shaped field, not an ergonomic one: the block's layout moves, so
//     every such member's fk_api_sig_ line moves and every guest in the field
//     has to be rebuilt -- to buy an arm the tier-2 form of the same member
//     already accepts.
//   - TYPE THE HANDLE PER CLASS, so the field is a LuaSpaceLocationPrototype
//     rather than an Object. That is a rendering change across the WHOLE API:
//     every one of the 261-odd class-typed fields and every handle return would
//     have to grow a type, both backends would need a conversion at every
//     boundary where a handle is minted, and the payoff is a compile error on
//     the small subset of positions where a union named exactly one class.
//
// So what closes the gap is that THE COLLAPSE IS SAID. A FieldSpec that came
// from shape B remembers the union it collapsed from -- the concept's name where
// it had one, and the arms in the description's own spelling -- and both
// backends render that into the doc comment of the struct field and of the
// method parameter. Nothing about the wire moves.
//
// ONE HELPER, FOUR CALL SITES, for the same reason variantdoc.go has one: this
// sentence is rendered on a Go field, a Go parameter, a Rust field and a Rust
// parameter, and four spellings of it is four chances to say different things
// about one collapse.

// CollapsedUnion records a union canonicalUnion reduced to its single class arm.
//
// It is DOCUMENTATION and nothing else: no layout, no member id and no byte of
// the wire depends on it, exactly as FieldSpec.LazyPayload does not.
type CollapsedUnion struct {
	// Concept is the description's own name for the union -- `SpaceLocationID`,
	// `ForceID` -- and is empty when the position declared the union inline.
	Concept string
	// Arms is the whole union in the description's own spelling, which is what
	// tells a reader which arms were dropped.
	Arms string
	// Class is the one class arm the position collapsed to.
	//
	// It is the CLASS and never the concept, which the nested case makes worth
	// saying: `ban_player` takes `PlayerIdentification | string`, whose first
	// arm is itself shape B, and the arm's own spelling there is a concept name
	// no guest can hold a handle to. canonicalUnion keeps the inner collapse's
	// answer for exactly that reason.
	Class string
	// UnderPrototypes reports the class as reachable from the `prototypes`
	// global, read off LuaPrototypes' attribute list rather than off the class
	// NAME -- seven of the 45 classes whose name ends in `Prototype` are not
	// there. See typeMapper.underPrototypes.
	UnderPrototypes bool
	// TierTwoTwin reports that the member this position belongs to also has a
	// PLAIN form taking the whole argument table as tier 2, where every arm of
	// the union still goes. True of a variant-group method's typed argument
	// struct and of nothing else: those are the members generated in two forms.
	TierTwoTwin bool
}

// String names the union the way every renderer here names it.
func (c CollapsedUnion) String() string {
	if c.Concept == "" {
		return c.Arms
	}
	return c.Concept + " (" + c.Arms + ")"
}

// CollapsedUnionLines renders the note for one collapsed position, wrapped at
// `width` and with no comment marker, so each backend prefixes its own.
//
// `name` is the identifier the reader sees: the Go or Rust field name, or the
// parameter name in the emitted signature.
//
// WHAT THE NOTE SAYS IS WHICH CLASS AND WHICH ARMS, and it deliberately says
// nothing about DIRECTION. The first draft ended "the union's scalar arms name
// one on the way in and a guest cannot send them", which is shape B's own
// argument and is false at two of the positions this renders on. A struct field
// is rendered wherever it appears, and `ItemIDAndQualityIDPair` and
// `RecipeIDAndQualityIDPair` appear only in RETURNS -- nothing is being sent
// there at all. And on the five fields of a variant-group method's typed
// argument struct the guest can send the string arm perfectly well, through the
// plain form of the same member, which is what TierTwoTwin adds a clause for.
// The load-bearing half survives both: this position carries only the handle.
//
// TWO CLAUSES ARE CONDITIONAL AND THE REST IS GENERIC OVER THE CLASS. Where the
// class is reachable from the `prototypes` global, saying so is the difference
// between a note and an answer; where the member has a tier-2 twin, the arm the
// reader wanted is two characters away and it would be perverse not to say so.
// Everywhere else the honest thing is to name the arms and stop, because how a
// guest obtains a `LuaForce` and how it obtains a `LuaSurface` are different
// questions this layer has no business guessing at.
func CollapsedUnionLines(name string, c CollapsedUnion, width int) []string {
	text := name + " is declared " + c.String() + ". Only the handle arm has a " +
		"fixed layout, so this position carries only the " + c.Class + " handle."
	if c.UnderPrototypes {
		text += " Find one under the prototypes global."
	}
	if c.TierTwoTwin {
		text += " The plain form of this member takes the whole table as tier 2, " +
			"where any arm goes."
	}
	return wrapComment(text, width)
}

// CollapsedArgPositions counts the positions at which a GUEST SENDS a value
// whose declared union collapsed to a handle.
//
// WHAT IS COUNTED IS THE SENDING HALF, and that is the whole of the definition.
// A RETURN is not counted, because a read is not a collapse a guest pays for:
// the engine's answer is determined, it returns the object, and the scalar arms
// are ways of naming one on the way IN -- canonicalUnion's shape B is right
// there and costs nothing. What costs is a position where the guest chooses the
// arm and has only one, so this walks Args and TypedArgs and not Rets.
//
// DEDUPLICATED THE WAY THE GENERATORS EMIT. A positional argument is one
// position per member, so its key carries the member. A field of a NAMED struct
// is emitted once however many members take that struct, so its key carries the
// concept name instead -- otherwise a concept used by fifty members would count
// fifty times and the row would move when a member was added rather than when a
// collapse was.
func CollapsedArgPositions(r Report) int {
	seen := map[string]bool{}
	n := 0
	var walk func(path string, f FieldSpec)
	walk = func(path string, f FieldSpec) {
		if f.Collapsed != nil {
			if !seen[path] {
				seen[path] = true
				n++
			}
			return
		}
		switch f.Kind {
		case KindStruct:
			base := path
			if f.TypeName != "" {
				base = f.TypeName
			}
			for _, sub := range f.Struct {
				walk(base+"."+sub.Name, sub)
			}
		case KindArray:
			if f.Elem != nil {
				walk(path+"[]", *f.Elem)
			}
		case KindDict:
			if f.Key != nil {
				walk(path+"{key}", *f.Key)
			}
			if f.Elem != nil {
				walk(path+"{value}", *f.Elem)
			}
		}
	}
	for _, m := range r.Members {
		for _, f := range m.Args {
			walk(MemberKey(m)+"."+f.Name, f)
		}
		for _, f := range m.TypedArgs {
			walk(MemberKey(m)+"."+f.Name, f)
		}
	}
	return n
}
