package factorio

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A UNION THAT COLLAPSED TO A HANDLE SAYS SO WHERE THE AUTHOR READS IT, in both
// backends, on a struct field and on a method parameter.
//
// canonicalUnion's shape B resolves "one class plus scalar identifiers" to the
// class, so `LuaForceCreateSpacePlatformArgs.planet` is an `Object` where the
// description says `SpaceLocationID`. The string arm is then unreachable and any
// object handle type-checks -- a `LuaPlanet` compiles and the engine refuses it
// at runtime. Reported by WormholeBelts as item 4 of its gaps ledger. The
// collapse is FkLua's recorded design and does not move; what closes the gap is
// that it is stated.
//
// THE POSITIONS AND THE EXPECTED TEXT BOTH COME FROM THE DESCRIPTION. The first
// cut of this test derived the population from the JSON and then read the
// expected concept, arms and class back out of `FieldSpec.Collapsed` -- so every
// value it asserted was a value the code under test had produced, and a note
// naming a CONCEPT where a class belongs stayed green. collapseDeriver
// re-implements the shape-B rule over the description alone, and what is
// compared against the emitted text is what IT says the class and the arms are.
func TestACollapsedUnionSaysSoInBothBackends(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			d := newCollapseDeriver(gen.API)

			// A FIELD and a POSITIONAL PARAMETER, both selected by asking the
			// DESCRIPTION which positions declare a shape-B union.
			field, fieldOK := firstCollapsedField(gen, d)
			param, paramOK := firstCollapsedParam(gen, d)
			if !fieldOK {
				t.Fatal("no struct field is declared as a union of one class " +
					"and a scalar: the derivation matched nothing and would " +
					"pass forever")
			}
			if !paramOK {
				t.Fatal("no positional parameter is declared as a union of one " +
					"class and a scalar: nothing to check")
			}

			for _, c := range []struct {
				what, ident string
				union       describedCollapse
				anchor      string
				marker, src string
			}{
				{"the Go struct field", exportName(field.name), field.union,
					"type " + field.owner + " struct {", "//", gen.Go.Source},
				{"the Rust struct field", rustName(field.name), field.union,
					"pub struct " + field.owner + " {", "///", gen.Rust.Source},
				{"the Go parameter", param.name, param.union,
					"func (o " + exportName(param.class) + ") " + param.goName + "(", "//", gen.Go.Source},
				{"the Rust parameter", param.name, param.union,
					"pub fn " + param.rsName + "(&self", "///", gen.Rust.Source},
			} {
				// The concept's NAME and its ARMS, both, and both DERIVED from
				// the description. Either alone leaves the reader where they
				// were: the name without the arms does not say what was
				// dropped, and the arms without the name do not say what to
				// look up. The class is the third, and it is the one a note
				// that named the concept instead got wrong.
				for _, want := range []string{
					c.union.Concept,
					c.union.Arms,
					"only the " + c.union.Class + " handle",
				} {
					if !inSomeBlockOrBody(c.src, c.anchor, c.marker, c.ident, want) {
						t.Errorf("%s: %s (%s) does not name %q",
							c.what, c.ident, c.union.Concept, want)
					}
				}
			}

			// A PLAIN HANDLE-TYPED PARAMETER CARRIES NO SUCH LINE, which is the
			// other half: a class-typed parameter did not collapse from
			// anything, and a renderer that fired on every handle would say so
			// about all of them.
			p, ok := firstPlainHandleParam(gen)
			if !ok {
				t.Fatal("no parameter declared as a bare class: nothing to " +
					"prove the negative with")
			}
			for _, c := range []struct{ what, anchor, marker, src string }{
				{"Go", "func (o " + exportName(p.class) + ") " + p.goName + "(", "//", gen.Go.Source},
				{"Rust", "pub fn " + p.rsName + "(&self", "///", gen.Rust.Source},
			} {
				blocks := docBlocksAbove(c.src, c.anchor, c.marker)
				if len(blocks) == 0 {
					t.Fatalf("%s: %s::%s has no doc comment at all, so this "+
						"proves nothing", c.what, p.class, p.member)
				}
				for _, b := range blocks {
					if strings.Contains(strings.Join(b, " "), "is declared") {
						t.Errorf("%s: %s::%s takes a bare class and its doc "+
							"comment talks about a collapsed union", c.what, p.class, p.member)
					}
				}
			}
		})
	}
}

// EVERY NOTE IN THE GENERATED SOURCES SAYS WHAT THE DESCRIPTION SAYS, over all
// of them rather than over the two the test above picks.
//
// It is the whole-source form of the same property, and it is what catches a
// collapse the selection above happens not to land on. THREE THINGS ARE DERIVED
// and none of them is read back from the code under test: the class arm (a note
// may name only a CLASS, and only the class the description's own union
// resolves to), the `prototypes` hint (true of the classes LuaPrototypes hands
// out, which is not the set whose names end in `Prototype`) and the tier-2
// clause (true on the argument struct of a member generated in two forms, false
// everywhere else).
//
// WHAT MADE THIS NECESSARY: `LuaGameScript::ban_player` takes
// `PlayerIdentification | string`, an inline union whose first arm is ITSELF
// shape B. canonicalUnion overwrote the inner collapse's class with the arm's
// own spelling, so both backends said "it takes the PlayerIdentification
// handle" at every committed pin -- naming a concept where a class belongs,
// which is the exact confusion the note exists to prevent.
func TestEveryCollapseNoteNamesWhatTheDescriptionDeclares(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			d := newCollapseDeriver(gen.API)
			want := map[string]bool{}
			for _, c := range describedCollapsePositions(gen.API, d) {
				want[c.key()] = true
			}
			if len(want) == 0 {
				t.Fatal("the description derives no collapsed position at all: " +
					"a walk that matched nothing passes forever")
			}
			classes := map[string]bool{}
			for _, c := range gen.API.Classes {
				classes[c.Name] = true
			}
			for _, s := range []struct{ what, src, marker string }{
				{"Go", gen.Go.Source, "//"},
				{"Rust", gen.Rust.Source, "///"},
			} {
				notes := collapseNotes(s.src, s.marker)
				if len(notes) == 0 {
					t.Fatalf("%s: not one collapse note in the generated "+
						"source, so nothing here is being checked", s.what)
				}
				// THE OWNER IN THE KEY IS DOING WORK, and this is where that
				// is asserted rather than argued. Content alone -- ident,
				// declared type, class, both clauses -- is shared very widely:
				// strip the owner and the notes collapse to a few dozen
				// distinct TEXTS, at which point every set comparison below and
				// in the tier-2 test stops being about positions at all.
				content := map[string]bool{}
				for _, n := range notes {
					content[noteKey("", n.ident, n.declared, n.class, n.protos, n.tierTwo)] = true
				}
				seen := map[string]bool{}
				for _, n := range notes {
					if seen[n.key()] {
						continue
					}
					seen[n.key()] = true
					if !classes[n.class] && !noteClassException[n.class] {
						t.Errorf("%s: a note says %s carries the %q handle, and "+
							"%q is not a class in this description",
							s.what, n.ident, n.class, n.class)
						continue
					}
					if !want[n.key()] {
						t.Errorf("%s: the note on %s matches nothing the "+
							"description declares: %s", s.what, n.ident, n.key())
					}
				}
				if len(seen) <= len(content) {
					t.Errorf("%s: %d notes reduce to %d keys and to %d distinct "+
						"TEXTS: the owner is not distinguishing positions, so "+
						"a note placed wrongly is answered by an identical one "+
						"somewhere else", s.what, len(notes), len(seen), len(content))
				}
				t.Logf("%s: %d notes, %d keys, %d distinct texts",
					s.what, len(notes), len(seen), len(content))
			}
		})
	}
}

// noteClassException is the ONE thing a collapse note may name that the
// description does not list among its classes.
//
// `LuaObject` is the description's own "any object": `"complex_type":
// "builtin"` in the concepts, not one of the 148 classes, and yet a real handle
// -- it is what `Object` is in both backends. A note that names it is honest,
// so the guard admits it BY NAME rather than by loosening.
//
// It is stated here instead of being written inline because it used to be an
// inline `&& n.class != "LuaObject"`, and the single shape it let through was
// the single shape canonicalUnion's fallback got wrong: an arm that resolves to
// a bare handle without collapsing. That fallback now resolves the arm first
// (see typeMapper.armHandleClass), and this stays as the statement of what a
// note is allowed to say. TestACollapseNoteMayNameOnlyAClassOrLuaObject holds
// both halves.
var noteClassException = map[string]bool{"LuaObject": true}

// A NOTE NAMES A CLASS, OR IT NAMES `LuaObject`, AND THERE IS NO THIRD OPTION.
//
// The committed descriptions cannot show this: none of them declares a concept
// that is a plain alias for a class, so `o.String()` and the resolved class are
// the same string everywhere and a note naming a CONCEPT is unreachable. That
// is exactly why it is built here -- the same reason the prototypes test builds
// a description where the two rules disagree.
//
// Three arms, three answers. A plain alias for a class must name the CLASS; an
// alias for `LuaObject` must name `LuaObject`, the stated exception, and the
// whole-source guard must accept it; and an arm that is a class already must be
// unaffected.
func TestACollapseNoteMayNameOnlyAClassOrLuaObject(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Concepts: []Concept{
			// A concept that is a plain alias for a class. `o.String()` says
			// "PlanetRef"; no guest can hold a handle to that.
			{Name: "PlanetRef", Type: Type{Name: "LuaPlanet"}},
			// ...and one wrapped in the description's `type` box, which
			// mapNamed unwraps and this must too.
			{Name: "BoxedPlanetRef", Type: Type{Complex: "type", Value: &Type{Name: "PlanetRef"}}},
			// An alias for the builtin "any object".
			{Name: "AnyRef", Type: Type{Name: "LuaObject"}},
		},
		Classes: []Class{
			{Name: "LuaPlanet"},
			{Name: "LuaThing", Methods: []Method{{
				Name: "take",
				Parameters: []Parameter{
					{Name: "aliased", Type: Type{Complex: "union", Options: []Type{
						{Name: "PlanetRef"}, {Name: "string"}}}},
					{Name: "boxed", Order: 1, Type: Type{Complex: "union", Options: []Type{
						{Name: "BoxedPlanetRef"}, {Name: "string"}}}},
					{Name: "anyobj", Order: 2, Type: Type{Complex: "union", Options: []Type{
						{Name: "AnyRef"}, {Name: "string"}}}},
					{Name: "direct", Order: 3, Type: Type{Complex: "union", Options: []Type{
						{Name: "LuaPlanet"}, {Name: "string"}}}},
				},
			}}},
		},
	}
	m, ok := findMember(GenerateMembers(a), "LuaThing", "take", MemberCall)
	if !ok {
		t.Fatal("the synthetic method did not generate, so nothing is being asked")
	}
	if len(m.Args) != 4 {
		t.Fatalf("got %d arguments, want 4", len(m.Args))
	}
	classes := map[string]bool{}
	for _, c := range a.Classes {
		classes[c.Name] = true
	}
	for _, c := range []struct {
		what string
		arg  FieldSpec
		want string
	}{
		{"an arm that is a concept aliasing a class", m.Args[0], "LuaPlanet"},
		{"the same alias behind the description's `type` box", m.Args[1], "LuaPlanet"},
		{"an arm that is a concept aliasing LuaObject", m.Args[2], "LuaObject"},
		{"an arm that is a class already", m.Args[3], "LuaPlanet"},
	} {
		if c.arg.Collapsed == nil {
			t.Errorf("%s: the argument did not collapse at all", c.what)
			continue
		}
		if got := c.arg.Collapsed.Class; got != c.want {
			t.Errorf("%s: the note names the %q handle, want %q", c.what, got, c.want)
		}
		// AND THE GUARD ACCEPTS EXACTLY THAT. A class, or the one stated
		// exception, and nothing else -- which is what makes admitting
		// LuaObject a decision rather than a hole.
		if !classes[c.arg.Collapsed.Class] && !noteClassException[c.arg.Collapsed.Class] {
			t.Errorf("%s: the whole-source guard would reject %q",
				c.what, c.arg.Collapsed.Class)
		}
		line := strings.Join(CollapsedUnionLines(c.arg.Name, *c.arg.Collapsed, 200), " ")
		if !strings.Contains(line, "only the "+c.want+" handle") {
			t.Errorf("%s: %q", c.what, line)
		}
	}
	// THE EXCEPTION IS ONE NAME AND NOT A CATEGORY: `LuaObject` is admitted
	// because it is a handle, and a concept that merely SOUNDS like one is not.
	if noteClassException["PlanetRef"] || noteClassException["QualityID"] {
		t.Error("the exception admits a concept name, which is the confusion " +
			"the whole note exists to prevent")
	}
}

// THE CENSUS COUNTS THE COLLAPSED SENDING POSITIONS, and the row is non-zero at
// every description.
//
// It is asserted non-zero rather than pinned: a pin would move on any
// description that adds a member taking a ForceID, which is not what a reader
// should be made to acknowledge. What a zero would mean is that the recording
// stopped, which is exactly the silent failure the row exists to prevent.
func TestTheCensusCountsCollapsedUnionArguments(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			if gen.Census.CollapsedUnionArgs <= 0 {
				t.Fatalf("collapsed_union_arg_positions is %d: nothing is being "+
					"counted", gen.Census.CollapsedUnionArgs)
			}
			if got := CollapsedArgPositions(gen.Members); got != gen.Census.CollapsedUnionArgs {
				t.Errorf("the census says %d collapsed argument positions and the "+
					"walk finds %d", gen.Census.CollapsedUnionArgs, got)
			}
			// A RETURN IS NOT ONE, which is the definition the row rests on. The
			// same concept in a return position must not raise the count, so a
			// walk that had started reading Rets would have to disagree with
			// this.
			ret := 0
			for _, m := range gen.Members.Members {
				for _, f := range m.Rets {
					if f.Collapsed != nil {
						ret++
					}
				}
			}
			if ret == 0 {
				t.Fatal("no return collapsed to a handle at all, so this says " +
					"nothing about whether returns are counted")
			}
			if CollapsedArgPositions(Report{Members: []Member{{
				Class: "X", Name: "y", Rets: []FieldSpec{{
					Name: "r0", Kind: KindHandle,
					Collapsed: &CollapsedUnion{Concept: "ForceID", Arms: "string | LuaForce", Class: "LuaForce"},
				}},
			}}}) != 0 {
				t.Error("a collapsed RETURN raised the argument count")
			}
		})
	}
}

// CollapsedUnionLines ITSELF, on all four shapes it can render.
//
// The renderings above are read back out of generated sources, which exercise
// only what the committed descriptions happen to contain -- and the sentence has
// two conditional clauses, so four combinations. The text is written out here
// rather than recomputed, because that is the point: this is where the wording
// is pinned.
//
// THE DIRECTION CLAUSE IS DELIBERATELY ABSENT, and this is where that is
// stated. The first draft ended "the union's scalar arms name one on the way in
// and a guest cannot send them", which is false on a field that appears only in
// a RETURN, and false again where the member's plain form still takes the
// string.
func TestCollapsedUnionLinesNamesTheClassTheArmsAndNothingAboutDirection(t *testing.T) {
	for _, c := range []struct {
		what string
		name string
		u    CollapsedUnion
		want string
	}{
		{
			"a named concept whose class is not under prototypes",
			"Force",
			CollapsedUnion{Concept: "ForceID", Arms: "string | uint8 | LuaForce", Class: "LuaForce"},
			"Force is declared ForceID (string | uint8 | LuaForce). Only the " +
				"handle arm has a fixed layout, so this position carries only " +
				"the LuaForce handle.",
		},
		{
			"a named concept whose class IS under prototypes",
			"planet",
			CollapsedUnion{Concept: "SpaceLocationID", Arms: "LuaSpaceLocationPrototype | string",
				Class: "LuaSpaceLocationPrototype", UnderPrototypes: true},
			"planet is declared SpaceLocationID (LuaSpaceLocationPrototype | " +
				"string). Only the handle arm has a fixed layout, so this " +
				"position carries only the LuaSpaceLocationPrototype handle. " +
				"Find one under the prototypes global.",
		},
		{
			"an INLINE union, which has no concept name to give",
			"player",
			CollapsedUnion{Arms: "PlayerIdentification | string", Class: "LuaPlayer"},
			"player is declared PlayerIdentification | string. Only the handle " +
				"arm has a fixed layout, so this position carries only the " +
				"LuaPlayer handle.",
		},
		{
			"a field of a member that also has a plain tier-2 form",
			"quality",
			CollapsedUnion{Concept: "QualityID", Arms: "LuaQualityPrototype | string",
				Class: "LuaQualityPrototype", UnderPrototypes: true, TierTwoTwin: true},
			"quality is declared QualityID (LuaQualityPrototype | string). Only " +
				"the handle arm has a fixed layout, so this position carries " +
				"only the LuaQualityPrototype handle. Find one under the " +
				"prototypes global. The plain form of this member takes the " +
				"whole table as tier 2, where any arm goes.",
		},
	} {
		got := strings.Join(strings.Fields(strings.Join(
			CollapsedUnionLines(c.name, c.u, 72), " ")), " ")
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.what, got, c.want)
		}
		if strings.Contains(got, "cannot send") {
			t.Errorf("%s: the note claims a direction, which is false on a "+
				"return-only field and on a member with a tier-2 twin", c.what)
		}
	}
	// THE WIDTH IS A WRAP AND NOT A TRUNCATION: every word survives at every
	// width, including one too small to wrap to, which is the caller's bug
	// rather than a request.
	u := CollapsedUnion{Concept: "ForceID", Arms: "string | uint8 | LuaForce", Class: "LuaForce"}
	wide := strings.Join(strings.Fields(strings.Join(
		CollapsedUnionLines("Force", u, 400), " ")), " ")
	for _, w := range []int{-1, 0, 24, 40, 72} {
		got := strings.Join(strings.Fields(strings.Join(
			CollapsedUnionLines("Force", u, w), " ")), " ")
		if got != wide {
			t.Errorf("width %d lost or changed a word:\n got %q\nwant %q", w, got, wide)
		}
	}
	if len(CollapsedUnionLines("Force", u, 400)) != 1 {
		t.Error("a generous width wrapped anyway, so the comparison above " +
			"compared wrapped text with wrapped text")
	}
	if len(CollapsedUnionLines("Force", u, 40)) < 2 {
		t.Error("a narrow width did not wrap at all, so the comparison above " +
			"said nothing about wrapping")
	}
}

// --- the description-side derivation -------------------------------------
//
// EVERYTHING BELOW RE-IMPLEMENTS SHAPE B OVER THE JSON, and it is deliberately
// not a call into canonicalUnion. The rule is small enough to state twice: a
// union naming exactly one class-valued arm and at least one scalar one, with
// nothing else in it, resolves to that class. Stating it twice is what lets the
// tests above disagree with the generator.

// describedCollapse is what the DESCRIPTION says about one position whose
// declared type is such a union.
type describedCollapse struct {
	// owner is the STRUCT or the MEMBER the position sits in, spelled the way
	// the backends spell it. It is half the key, and the half whose absence
	// made the tier-2 assertion answerable by a neighbour: ident, declared type
	// and class together are shared by 129 of the 253 derived positions at the
	// GA pin, so a note given the clause wrongly on a returns-only struct was
	// answered by a genuine one on a variant-group method's argument struct.
	// See noteKey.
	owner string
	// ident is the description's own name for the position. The backends spell
	// it three ways between them; see normIdent.
	ident string
	// Concept is the position's own declared name, empty for an inline union.
	// The OUTERMOST name wins along an alias chain: it is the name the author
	// typed and the one they will look up.
	Concept string
	// Arms is the union in the description's own spelling.
	Arms string
	// Class is the one class arm, and it is a CLASS even where the arm that
	// produced it was a concept that itself collapses to one.
	Class string
	// UnderPrototypes is the class being one LuaPrototypes hands out.
	UnderPrototypes bool
	// TierTwoTwin is the position belonging to a member generated in two forms.
	TierTwoTwin bool
}

func (d describedCollapse) declared() string {
	if d.Concept == "" {
		return d.Arms
	}
	return d.Concept + " (" + d.Arms + ")"
}

// key is the note's whole content AND ITS POSITION in the one spelling both
// sides compare on.
func (d describedCollapse) key() string {
	return noteKey(d.owner, d.ident, d.declared(), d.Class, d.UnderPrototypes, d.TierTwoTwin)
}

type collapseDeriver struct {
	classes  map[string]bool
	concepts map[string]Concept
	// protos is every class reachable from the `prototypes` global, read off
	// LuaPrototypes' own attribute list. Seven of the 45 classes whose name ends
	// in `Prototype` are not among them, which is why the hint is derived rather
	// than spelled.
	protos map[string]bool
}

func newCollapseDeriver(a *API) *collapseDeriver {
	d := &collapseDeriver{
		classes:  map[string]bool{},
		concepts: map[string]Concept{},
		protos:   map[string]bool{},
	}
	for _, c := range a.Classes {
		d.classes[c.Name] = true
	}
	for _, c := range a.Concepts {
		d.concepts[c.Name] = c
	}
	for _, c := range a.Classes {
		if c.Name != "LuaPrototypes" {
			continue
		}
		for _, at := range c.Attributes {
			t := at.ReadType
			if t == nil {
				continue
			}
			for t.Complex == "type" && t.Value != nil {
				t = t.Value
			}
			v := t
			if t.Complex == "LuaCustomTable" || t.Complex == "dictionary" {
				v = t.Value
			}
			if v != nil && v.IsNamed() && d.classes[v.Name] {
				d.protos[v.Name] = true
			}
		}
	}
	return d
}

const (
	armOther = iota
	armClass
	armScalar
)

// scalarBuiltins is the description's primitive vocabulary. A name that is
// neither a class, a concept nor one of these is not a type this API declares,
// and a union containing one does not map at all.
var scalarBuiltins = map[string]bool{
	"boolean": true, "string": true, "double": true, "float": true,
	"number": true, "int8": true, "uint8": true, "int16": true,
	"uint16": true, "int32": true, "uint32": true, "uint64": true,
}

// arm classifies one option of a union.
func (d *collapseDeriver) arm(t Type, depth int) (int, string) {
	if depth > 12 {
		return armOther, ""
	}
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if !t.IsNamed() {
		if t.Complex == "literal" {
			return armScalar, "" // a scalar with its value pinned
		}
		return armOther, "" // any other structural type is a shape
	}
	switch {
	case t.Name == "table":
		return armOther, "" // "any table" has no layout: tier 2
	case t.Name == "LuaObject":
		return armClass, "LuaObject" // "any object" is still a handle
	case d.classes[t.Name]:
		return armClass, t.Name
	case strings.HasPrefix(t.Name, "defines."):
		return armScalar, "" // a named integer constant
	case scalarBuiltins[t.Name]:
		return armScalar, ""
	}
	c, ok := d.concepts[t.Name]
	if !ok {
		return armOther, ""
	}
	if inner, ok := d.collapse(c.Type, depth+1); ok {
		// THE ARM IS ITSELF SHAPE B, so it resolves to a class -- and to the
		// INNER class, not to the concept's own name, which is what ban_player
		// was being told it took a handle to.
		return armClass, inner.Class
	}
	return d.arm(c.Type, depth+1)
}

// collapse reports the shape-B collapse of a declared type, where it has one.
func (d *collapseDeriver) collapse(t Type, depth int) (describedCollapse, bool) {
	if depth > 12 {
		return describedCollapse{}, false
	}
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if t.IsNamed() {
		c, ok := d.concepts[t.Name]
		if !ok {
			return describedCollapse{}, false
		}
		inner, ok := d.collapse(c.Type, depth+1)
		if !ok {
			return describedCollapse{}, false
		}
		// The outermost name wins: the position is named by what the position
		// says, not by what the concept resolves through.
		inner.Concept = t.Name
		return inner, true
	}
	if t.Complex != "union" {
		return describedCollapse{}, false
	}
	nClass, nScalar, class := 0, 0, ""
	for _, o := range t.Options {
		k, c := d.arm(o, depth+1)
		switch k {
		case armClass:
			nClass++
			class = c
		case armScalar:
			nScalar++
		default:
			return describedCollapse{}, false
		}
	}
	if nClass != 1 || nScalar == 0 {
		return describedCollapse{}, false
	}
	return describedCollapse{Arms: t.String(), Class: class,
		UnderPrototypes: d.protos[class]}, true
}

// describedCollapsePositions is every position in the description whose declared
// type collapses, carrying the identifier that position has AND THE OWNER IT
// SITS IN.
//
// It is WIDER than the sending half the census counts, on purpose: a struct
// field's note is rendered wherever the struct is, including on one that only
// ever comes back from the engine.
//
// THE OWNER IS SPELLED THE WAY THE BACKENDS SPELL IT, because it is compared
// against a name read out of the generated source. Three rules cover every
// position that carries a note at any committed pin, and they are the
// generators' own:
//
//   - a table-shaped CONCEPT is a struct of that name;
//   - a `takes_table` method's parameters, and a variant-group method's, are
//     the fields of `<Class><Method>Args` -- goStructs.add's fallback, and the
//     name variantdoc_test.go already reconstructs the same way;
//   - any other method's parameters belong to `<Class>::<method>`, and an
//     attribute's write half to `<Class>::set_<attribute>`, which is what
//     goMemberName builds for MemberSet.
//
// A nested ANONYMOUS table inherits its parent's owner, which is not the name
// the backends would synthesise for it. Nothing at any committed pin carries a
// note there; if one ever does, the whole-source check below fails on it rather
// than admitting it, which is the right direction to be wrong in.
func describedCollapsePositions(a *API, d *collapseDeriver) []describedCollapse {
	var out []describedCollapse
	add := func(owner, name string, t Type, tierTwo bool) {
		c, ok := d.collapse(t, 0)
		if !ok {
			return
		}
		c.owner = owner
		c.ident = name
		c.TierTwoTwin = tierTwo
		out = append(out, c)
	}
	var table func(owner string, t Type, depth int)
	table = func(owner string, t Type, depth int) {
		if depth > 8 {
			return
		}
		for t.Complex == "type" && t.Value != nil {
			t = *t.Value
		}
		switch t.Complex {
		case "table", "LuaStruct":
			for _, p := range t.Parameters {
				add(owner, p.Name, p.Type, false)
				table(owner, p.Type, depth+1)
			}
			for _, at := range t.Attributes {
				if at.ReadType != nil {
					add(owner, at.Name, *at.ReadType, false)
				}
			}
			for _, g := range t.VariantGroups {
				for _, p := range g.Parameters {
					table(owner, p.Type, depth+1)
				}
			}
		case "array", "LuaLazyLoadedValue", "dictionary", "LuaCustomTable":
			if t.Key != nil {
				table(owner, *t.Key, depth+1)
			}
			if t.Value != nil {
				table(owner, *t.Value, depth+1)
			}
		case "union":
			for _, o := range t.Options {
				table(owner, o, depth+1)
			}
		case "tuple":
			for _, o := range t.Values {
				table(owner, o, depth+1)
			}
		}
	}
	meth := func(class, goName string, m Method) {
		// A METHOD WITH VARIANT GROUPS IS GENERATED IN TWO FORMS, so its own
		// parameters are the fields of a struct whose member also has a plain
		// tier-2 twin. No other method's parameters have that escape.
		tierTwo := len(m.VariantGroups) > 0
		owner := class + "::" + goName
		if tierTwo || m.TakesTable() {
			// One struct argument rather than positional ones, so the
			// parameters are FIELDS and the owner is the struct.
			owner = exportName(class) + exportName(goName) + "Args"
		}
		for _, p := range m.Parameters {
			add(owner, p.Name, p.Type, tierTwo)
			table(owner, p.Type, 0)
		}
		for _, g := range m.VariantGroups {
			for _, p := range g.Parameters {
				table(owner, p.Type, 0)
			}
		}
		for _, r := range m.ReturnValues {
			table(owner, r.Type, 0)
		}
	}
	for _, c := range a.Classes {
		for _, m := range c.Methods {
			meth(c.Name, m.Name, m)
		}
		for _, at := range c.Attributes {
			if at.ReadType != nil {
				table(c.Name+"::"+at.Name, *at.ReadType, 0)
			}
			if at.WriteType != nil {
				// A SETTER'S PARAMETER IS CALLED `value` in both backends,
				// whatever the attribute is called, and the member is `Set`
				// plus the attribute's name.
				add(c.Name+"::set_"+at.Name, "value", *at.WriteType, false)
				table(c.Name+"::set_"+at.Name, *at.WriteType, 0)
			}
		}
		for _, op := range c.Operators {
			if op.ReadType != nil {
				// An index operator's write half is a member taking `value`
				// too, and it is named BARE `Set` -- see goMemberName's
				// MemberIndexSet case.
				add(c.Name+"::set", "value", *op.ReadType, false)
				table(c.Name+"::get", *op.ReadType, 0)
			}
			meth(c.Name, operatorMemberName(op.Name),
				Method{Name: op.Name, Parameters: op.Parameters, ReturnValues: op.ReturnValues})
		}
	}
	for _, m := range a.GlobalFunctions {
		// A global function has no class, and the backends emit it as a free
		// one. `::` with nothing before it is that.
		meth("", m.Name, m)
	}
	for _, c := range a.Concepts {
		table(c.Name, c.Type, 0)
	}
	for _, e := range a.Events {
		for _, p := range e.Data {
			add(e.Name, p.Name, p.Type, false)
			table(e.Name, p.Type, 0)
		}
	}
	return out
}

// operatorMemberName is the member name the backends give an operator, which is
// not the description's own: see goMemberName's MemberIndex, MemberLen and
// MemberSelf cases.
func operatorMemberName(op string) string {
	switch op {
	case "index":
		return "get"
	case "length":
		return "length"
	case "call":
		return "call"
	}
	return op
}

// --- reading the notes back out of a generated source ---------------------

var collapseNoteRE = regexp.MustCompile(
	`(\S+) is declared (.+?)\. Only the handle arm has a fixed layout, so this ` +
		`position carries only the (\S+) handle\.` +
		`( Find one under the prototypes global\.)?` +
		`( The plain form of this member takes the whole table as tier 2, where any arm goes\.)?`)

type emittedNote struct {
	owner                  string
	ident, declared, class string
	protos, tierTwo        bool
}

func (n emittedNote) key() string {
	return noteKey(n.owner, n.ident, n.declared, n.class, n.protos, n.tierTwo)
}

// noteKey is the one spelling of a note's content AND ITS POSITION that both
// sides compare on.
//
// THE OWNER IS IN THE KEY BECAUSE THE CONTENT ALONE IS NOT A POSITION. A note's
// identifier, declared type and class are shared very widely -- 129 of the 253
// derived positions at the GA pin share all three with one of the three genuine
// tier-2 positions -- so a key without an owner turns a set of POSITIONS into a
// set of TEXTS, and the tier-2 assertion's false-positive half becomes
// unenforceable: a clause added wrongly to `ItemIDAndQualityIDPair.quality`, a
// struct that appears only in returns, is answered by the identical text on
// `LuaSurfaceCreateEntityArgs.quality` and the whole file stays green.
//
// The identifier is NORMALISED because three spellings of it reach the sources:
// the description's `by_player`, Go's `ByPlayer`, and Rust's `r#type` for a name
// that is a keyword there. Case, underscores and the raw-identifier prefix are
// exactly the differences between them, and nothing else is. The owner is
// normalised the same way, per part, because Go spells a member `Teleport` and
// Rust spells it `teleport`.
func noteKey(owner, ident, declared, class string, protos, tierTwo bool) string {
	k := normOwner(owner) + "." + normIdent(ident) + "|" + declared + "|" + class
	if protos {
		k += "|prototypes"
	}
	if tierTwo {
		k += "|tier2"
	}
	return k
}

func normIdent(s string) string {
	s = strings.TrimPrefix(s, "r#")
	s = strings.TrimSuffix(s, "_")
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// normOwner normalises an owner, keeping the two namespaces apart: a STRUCT
// name has no separator, a MEMBER is `class::member` with either half possibly
// empty. `LuaSurfaceCreateEntityArgs` and `LuaSurface::create_entity` are
// different owners and must not fold together.
func normOwner(s string) string {
	if i := strings.Index(s, "::"); i >= 0 {
		return normIdent(s[:i]) + "::" + normMember(s[i+2:])
	}
	return "struct:" + normIdent(s)
}

// memberVariantSuffixes are the EXTRA EMISSIONS of one member the backends
// give a second name to, and whose doc comment therefore carries the same
// collapse note: `Into` for a container return written into a caller's slice,
// `Raw` for the handle variant of an attribute read or a call. They fold onto
// the member they are a form of, because they are one position in the
// description and the note on each is the same true sentence.
//
// It is exactly the two that are REACHED. `Is` (MemberGetEq) takes a string,
// `Typed` (a variant-group method's typed form) takes one struct argument and
// renders its notes on that struct's fields, so neither can carry a note at
// all. A third form appearing would fail the whole-source check by name rather
// than being admitted, which is the direction to be wrong in.
//
// The stripping is applied to BOTH sides, so a description member whose own
// name ends in one of them still matches itself.
var memberVariantSuffixes = []string{"into", "raw"}

func normMember(s string) string {
	n := normIdent(s)
	for _, suf := range memberVariantSuffixes {
		if len(n) > len(suf) && strings.HasSuffix(n, suf) {
			return n[:len(n)-len(suf)]
		}
	}
	return n
}

// The declarations a note can sit against. Go source contains none of the Rust
// forms and Rust none of the Go ones, so one scanner reads both without being
// told which it is looking at.
var (
	goStructOpenRE = regexp.MustCompile(`^type ([A-Za-z0-9_]+) struct \{$`)
	rsStructOpenRE = regexp.MustCompile(`^pub struct ([A-Za-z0-9_]+) \{$`)
	rsImplOpenRE   = regexp.MustCompile(`^impl ([A-Za-z0-9_]+) \{$`)
	goMethodRE     = regexp.MustCompile(`^func \(o ([A-Za-z0-9_]+)\) ([A-Za-z0-9_]+)\(`)
	goFuncRE       = regexp.MustCompile(`^func ([A-Za-z0-9_]+)\(`)
	rsFnRE         = regexp.MustCompile(`^pub (?:unsafe )?fn ([A-Za-z0-9_]+)[(<]`)
)

// collapseNotes reads every collapse note out of a generated source, with each
// comment run flattened first so a wrapped sentence still matches, AND WITH THE
// DECLARATION IT SITS ON.
//
// A note reaches the source two ways and the owner is found differently for
// each. A METHOD's note is a doc block, so the owner is on the line that FOLLOWS
// the run -- `func (o LuaControl) Teleport(` in Go, `pub fn teleport(` inside
// `impl LuaControl {` in Rust. A STRUCT FIELD's note sits INSIDE the type
// declaration, so the owner is the enclosing `type X struct {` / `pub struct X
// {`, which came before. The anchor line wins where both are available, because
// only a field's own declaration can follow a field's note.
func collapseNotes(src, marker string) []emittedNote {
	var out []emittedNote
	var run []string
	curStruct, curImpl := "", ""
	flush := func(anchor string) {
		if len(run) == 0 {
			return
		}
		joined := strings.Join(strings.Fields(strings.Join(run, " ")), " ")
		run = nil
		ms := collapseNoteRE.FindAllStringSubmatch(joined, -1)
		if len(ms) == 0 {
			return
		}
		owner := noteOwner(anchor, curStruct, curImpl)
		for _, m := range ms {
			out = append(out, emittedNote{
				owner: owner,
				ident: m[1], declared: m[2], class: m[3],
				protos: m[4] != "", tierTwo: m[5] != "",
			})
		}
	}
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(t, marker) {
			run = append(run, strings.TrimPrefix(strings.TrimPrefix(t, marker), " "))
			continue
		}
		flush(t)
		switch {
		case goStructOpenRE.MatchString(t):
			curStruct = goStructOpenRE.FindStringSubmatch(t)[1]
		case rsStructOpenRE.MatchString(t):
			curStruct = rsStructOpenRE.FindStringSubmatch(t)[1]
		case rsImplOpenRE.MatchString(t):
			curImpl = rsImplOpenRE.FindStringSubmatch(t)[1]
		case strings.HasPrefix(l, "}"):
			// A brace in column ZERO, which is where both backends close a
			// type declaration and where Rust closes an impl block. Every
			// brace INSIDE one is indented.
			curStruct, curImpl = "", ""
		}
	}
	flush("")
	return out
}

// noteOwner names the declaration a comment run belongs to.
func noteOwner(anchor, curStruct, curImpl string) string {
	if m := goMethodRE.FindStringSubmatch(anchor); m != nil {
		return m[1] + "::" + m[2]
	}
	if m := rsFnRE.FindStringSubmatch(anchor); m != nil {
		return curImpl + "::" + m[1]
	}
	if m := goFuncRE.FindStringSubmatch(anchor); m != nil {
		return "::" + m[1]
	}
	return curStruct
}

type collapsedField struct {
	owner string
	name  string
	union describedCollapse
}

type collapsedParam struct {
	class, member  string
	name           string
	goName, rsName string
	union          describedCollapse
}

// firstCollapsedField finds the first struct field, by sorted owner and name,
// whose DECLARED type the description says collapses -- and it looks the owner
// up in the generated report rather than trusting the fallback naming rule.
func firstCollapsedField(gen *stdGeneration, d *collapseDeriver) (collapsedField, bool) {
	var keys []string
	found := map[string]collapsedField{}
	for _, m := range gen.Members.Members {
		for _, f := range m.Args {
			// A NAMED struct is a concept, and its fields are the CONCEPT's
			// parameters -- which is where the declared type is read from, so
			// the expectation comes from the JSON and not from the FieldSpec.
			if f.Kind != KindStruct || f.TypeName == "" {
				continue
			}
			declared := describedTableFields(d, f.TypeName)
			for _, sub := range f.Struct {
				u, ok := d.collapse(declared[sub.Name], 0)
				if !ok {
					continue
				}
				k := exportName(f.TypeName) + "." + sub.Name
				if _, seen := found[k]; seen {
					continue
				}
				keys = append(keys, k)
				found[k] = collapsedField{exportName(f.TypeName), sub.Name, u}
			}
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return collapsedField{}, false
	}
	return found[keys[0]], true
}

// firstCollapsedParam finds the first bound method, by sorted key, with a
// POSITIONAL parameter the description says collapses.
func firstCollapsedParam(gen *stdGeneration, d *collapseDeriver) (collapsedParam, bool) {
	meths := describedMethods(gen.API)
	var keys []string
	found := map[string]collapsedParam{}
	for _, m := range gen.Members.Members {
		if m.Kind != MemberCall {
			continue
		}
		meth, ok := meths[m.Class+"::"+m.Name]
		if !ok || meth.TakesTable() || len(meth.VariantGroups) > 0 {
			continue
		}
		g, r := gen.Go.Names[MemberKey(m)], gen.Rust.Names[MemberKey(m)]
		if g == "" || r == "" {
			continue
		}
		for _, f := range m.Args {
			u, ok := d.collapse(declaredParam(meth, f.Name), 0)
			if !ok {
				continue
			}
			k := MemberKey(m) + "." + f.Name
			keys = append(keys, k)
			found[k] = collapsedParam{m.Class, m.Name, f.Name, g, r, u}
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return collapsedParam{}, false
	}
	return found[keys[0]], true
}

// describedTableFields is the declared type of every field of a table-shaped
// CONCEPT, read out of the description. An unknown or non-table name has none,
// and a lookup into the empty map yields a type that collapses to nothing.
func describedTableFields(d *collapseDeriver, name string) map[string]Type {
	out := map[string]Type{}
	c, ok := d.concepts[name]
	if !ok {
		return out
	}
	t := c.Type
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if t.Complex != "table" && t.Complex != "LuaStruct" {
		return out
	}
	for _, p := range t.Parameters {
		out[p.Name] = p.Type
	}
	for _, at := range t.Attributes {
		if at.ReadType != nil {
			out[at.Name] = *at.ReadType
		}
	}
	return out
}

func describedMethods(a *API) map[string]Method {
	out := map[string]Method{}
	for _, c := range a.Classes {
		for _, m := range c.Methods {
			out[c.Name+"::"+m.Name] = m
		}
	}
	return out
}

// declaredParam is the description's own type for one named parameter, and a
// type naming nothing when the method has no parameter of that name.
func declaredParam(m Method, name string) Type {
	for _, p := range m.Parameters {
		if p.Name == name {
			return p.Type
		}
	}
	return Type{Name: "nil"}
}

// firstPlainHandleParam finds a method whose first argument is a bare
// class-typed handle -- one that collapsed from nothing.
func firstPlainHandleParam(gen *stdGeneration) (collapsedParam, bool) {
	var keys []string
	found := map[string]collapsedParam{}
	for _, m := range gen.Members.Members {
		if m.Kind != MemberCall || len(m.Args) == 0 {
			continue
		}
		g, r := gen.Go.Names[MemberKey(m)], gen.Rust.Names[MemberKey(m)]
		if g == "" || r == "" {
			continue
		}
		ok := false
		for _, f := range m.Args {
			if f.Kind == KindHandle && f.Collapsed == nil {
				ok = true
			}
			if f.Collapsed != nil {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		keys = append(keys, MemberKey(m))
		found[MemberKey(m)] = collapsedParam{class: m.Class, member: m.Name, goName: g, rsName: r}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return collapsedParam{}, false
	}
	return found[keys[0]], true
}

// inSomeBlockOrBody reports whether `want` appears in a comment run that also
// names `ident`, anywhere the anchor occurs.
//
// A STRUCT FIELD'S NOTE IS NOT A DOC BLOCK ABOVE THE ANCHOR -- it sits INSIDE
// the type declaration -- so this reads forward from the anchor as well as
// backward from it, and requires the identifier and the wanted text to land in
// one comment run so a neighbour's note cannot answer for this field.
func inSomeBlockOrBody(src, anchor, marker, ident, want string) bool {
	for _, b := range docBlocksAbove(src, anchor, marker) {
		if runNames(b, ident, want) {
			return true
		}
	}
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if !strings.Contains(l, anchor) {
			continue
		}
		var run []string
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimLeft(lines[j], " \t")
			if strings.HasPrefix(t, marker) {
				run = append(run, strings.TrimPrefix(strings.TrimPrefix(t, marker), " "))
				continue
			}
			if len(run) > 0 && runNames(run, ident, want) {
				return true
			}
			run = nil
			if t == "}" {
				break
			}
		}
	}
	return false
}

// runNames reports a comment run that names both the identifier and the text,
// with the run flattened first so a wrapped sentence still matches.
func runNames(run []string, ident, want string) bool {
	joined := strings.Join(strings.Fields(strings.Join(run, " ")), " ")
	return strings.Contains(joined, ident+" is declared") &&
		strings.Contains(joined, want)
}

// "FIND ONE UNDER THE PROTOTYPES GLOBAL" IS DERIVED FROM LuaPrototypes AND NOT
// FROM THE CLASS NAME, which are two different sets.
//
// The first cut asked `strings.HasSuffix(class, "Prototype")`. That is true of
// 45 classes at every committed pin and RIGHT ABOUT 38 of them:
// LuaBurnerPrototype, LuaFluidBoxPrototype, LuaHeatBufferPrototype and the four
// energy-source prototypes are reached through an entity prototype and are not
// under `prototypes` at all, so the sentence would have sent a reader to a
// table with nothing of that name in it. It is wrong in the other direction
// too -- what `prototypes` hands out is 42 classes at 2.0.77, 2.1.12 and 2.1.14
// and 43 from 2.1.16, and the ones the name rule would miss are LuaModData, the
// two noise classes and the item-group classes: LuaGroup up to 2.1.14, split
// into LuaItemGroup and LuaItemSubGroup from 2.1.16.
//
// EVERY ONE OF THOSE COUNTS IS RE-DERIVED BELOW AND LOGGED rather than pinned,
// because a number in a comment is the thing this whole derivation exists to
// stop being trusted: the first cut of this paragraph said "42" of the name
// rule while listing seven exceptions to 45.
//
// NO COMMITTED DESCRIPTION HAS A COLLAPSE WHERE THE TWO RULES DISAGREE, so the
// generated sources cannot tell them apart and a test read off them would pass
// under either. The first half below therefore builds a description where they
// disagree in BOTH directions; the second holds the real derivation to
// LuaPrototypes' own attribute list at every pin.
func TestThePrototypesHintIsDerivedFromLuaPrototypesAndNotFromTheName(t *testing.T) {
	a := &API{
		APIVersion: 6,
		Concepts: []Concept{
			// A class whose NAME says prototype and which `prototypes` does not
			// hand out.
			{Name: "FooID", Type: Type{Complex: "union", Options: []Type{
				{Name: "LuaFooPrototype"}, {Name: "string"}}}},
			// ...and a class `prototypes` DOES hand out, whose name says
			// nothing.
			{Name: "BarID", Type: Type{Complex: "union", Options: []Type{
				{Name: "LuaBar"}, {Name: "string"}}}},
		},
		Classes: []Class{
			{Name: "LuaFooPrototype"},
			{Name: "LuaBar"},
			{Name: "LuaPrototypes", Attributes: []Attribute{{
				Name: "bar",
				ReadType: &Type{Complex: "LuaCustomTable",
					Key:   &Type{Name: "string"},
					Value: &Type{Name: "LuaBar"}},
			}}},
			{Name: "LuaThing", Methods: []Method{{
				Name: "take",
				Parameters: []Parameter{
					{Name: "foo", Type: Type{Name: "FooID"}},
					{Name: "bar", Order: 1, Type: Type{Name: "BarID"}},
				},
			}}},
		},
	}
	m, ok := findMember(GenerateMembers(a), "LuaThing", "take", MemberCall)
	if !ok {
		t.Fatal("the synthetic method did not generate, so nothing is being asked")
	}
	if len(m.Args) != 2 {
		t.Fatalf("got %d arguments, want 2", len(m.Args))
	}
	for _, c := range []struct {
		what string
		arg  FieldSpec
		want bool
	}{
		{"a class named *Prototype that prototypes does NOT hand out", m.Args[0], false},
		{"a class prototypes DOES hand out, named nothing of the sort", m.Args[1], true},
	} {
		if c.arg.Collapsed == nil {
			t.Errorf("%s: the argument did not collapse at all", c.what)
			continue
		}
		if c.arg.Collapsed.UnderPrototypes != c.want {
			t.Errorf("%s: UnderPrototypes is %v, want %v (class %s)",
				c.what, c.arg.Collapsed.UnderPrototypes, c.want, c.arg.Collapsed.Class)
		}
		line := strings.Join(CollapsedUnionLines(c.arg.Name, *c.arg.Collapsed, 200), " ")
		if got := strings.Contains(line, "prototypes global"); got != c.want {
			t.Errorf("%s: the note %s the prototypes global: %q", c.what,
				map[bool]string{true: "names", false: "does not name"}[got], line)
		}
	}

	// AND AT EVERY COMMITTED PIN the generator's own set is LuaPrototypes' own
	// attribute list, re-derived here, with the divergence from the name rule
	// counted so this cannot become vacuous if a description ever makes the two
	// agree.
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			d := newCollapseDeriver(gen.API)
			got := newTypeMapper(gen.API).underPrototypes
			if len(got) != len(d.protos) {
				t.Fatalf("the generator finds %d classes under prototypes and "+
					"LuaPrototypes' attribute list has %d", len(got), len(d.protos))
			}
			for n := range d.protos {
				if !got[n] {
					t.Errorf("%s is handed out by prototypes and the generator "+
						"does not think so", n)
				}
			}
			// THE FOUR NUMBERS, DERIVED. How many classes the name rule
			// claims, how many `prototypes` really hands out, how often the
			// rule is right, and the two divergence sets by name -- all read
			// off this description rather than written into a sentence, which
			// is what the comments above now defer to.
			var named, missing, extra []string
			for _, c := range gen.API.Classes {
				if strings.HasSuffix(c.Name, "Prototype") {
					named = append(named, c.Name)
					if !d.protos[c.Name] {
						missing = append(missing, c.Name)
					}
					continue
				}
				if d.protos[c.Name] {
					extra = append(extra, c.Name)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			t.Logf("the name rule claims %d classes, prototypes hands out %d, "+
				"and the rule is right about %d; named but absent (%d): %s; "+
				"present but unnamed (%d): %s",
				len(named), len(d.protos), len(named)-len(missing),
				len(missing), strings.Join(missing, ", "),
				len(extra), strings.Join(extra, ", "))
			if len(missing) == 0 || len(extra) == 0 {
				t.Errorf("the name rule and the derivation agree everywhere in "+
					"this description (%d named but absent, %d present but "+
					"unnamed), so nothing here distinguishes them",
					len(missing), len(extra))
			}
			// The classes the notes actually carry the hint on, which is the
			// subset a reader meets.
			hinted := map[string]bool{}
			for _, n := range collapseNotes(gen.Rust.Source, "///") {
				if n.protos {
					hinted[n.class] = true
				}
			}
			if len(hinted) == 0 {
				t.Fatal("no note carries the hint at all")
			}
			for n := range hinted {
				if !d.protos[n] {
					t.Errorf("a note sends a reader to prototypes for %s, which "+
						"LuaPrototypes does not hand out", n)
				}
			}
			sorted := make([]string, 0, len(hinted))
			for n := range hinted {
				sorted = append(sorted, n)
			}
			sort.Strings(sorted)
			t.Logf("the hint fires on %d classes: %s", len(sorted), strings.Join(sorted, ", "))
		})
	}
}

// THE TIER-2 CLAUSE LANDS ON THE MEMBERS GENERATED IN TWO FORMS AND NOWHERE
// ELSE, which is the other clause that is not generic over the class.
//
// A variant-group method's typed argument struct is the one place a collapsed
// field has an escape: the same member's PLAIN form takes the whole table as
// tier 2, where the string arm the collapse dropped still goes. Saying "a guest
// cannot send them" there was simply false, and saying nothing would leave the
// reader looking for a handle they do not need.
//
// Both halves are asserted. Every position the description says has such a twin
// must carry the clause, and no note without such a twin may carry it.
func TestTheTierTwoClauseLandsOnAMemberGeneratedInTwoForms(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			d := newCollapseDeriver(gen.API)
			want := map[string]bool{}
			for _, c := range describedCollapsePositions(gen.API, d) {
				if c.TierTwoTwin {
					want[c.key()] = true
				}
			}
			if len(want) == 0 {
				t.Fatal("the description declares no collapsed parameter on a " +
					"variant-group method: a walk that matched nothing passes " +
					"forever")
			}
			for _, s := range []struct{ what, src, marker string }{
				{"Go", gen.Go.Source, "//"},
				{"Rust", gen.Rust.Source, "///"},
			} {
				got := map[string]bool{}
				for _, n := range collapseNotes(s.src, s.marker) {
					if n.tierTwo {
						got[n.key()] = true
					}
				}
				for k := range want {
					if !got[k] {
						t.Errorf("%s: no note says the plain form still takes "+
							"any arm for %s", s.what, k)
					}
				}
				for k := range got {
					if !want[k] {
						t.Errorf("%s: a note claims a plain tier-2 form for %s, "+
							"and the description gives that member only one form",
							s.what, k)
					}
				}
			}
		})
	}
}
