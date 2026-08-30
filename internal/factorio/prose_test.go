package factorio

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE DESCRIPTION'S PROSE REACHES BOTH BINDINGS, and it is asserted as a
// COVERAGE FRACTION rather than for one member.
//
// The generator emitted none of it for as long as it has existed, so a GUI
// author looking for what `add` accepts had the Factorio wiki and nothing in
// their own language. What makes this worth a gate rather than a one-off is the
// failure mode: prose that stopped being attached would leave every binding
// compiling, every test green and every count unmoved -- attachDocs keys on
// (class, name, is-it-a-method), and a kind added to Member without a case in
// that switch silently gets nothing.
func TestTheDescriptionsProseReachesBothBindings(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
			if err != nil {
				t.Fatal(err)
			}
			r := GenerateMembers(a)
			withDoc := 0
			for _, m := range r.Members {
				if m.Doc != "" {
					withDoc++
				}
			}
			// 3,566 of 4,262 at the GA pin. The floor is well under that and is
			// there to catch a switch that stopped matching, not to pin a
			// number the description owns: a pin is free to document fewer
			// members and the count moves in the census, not here.
			if min := len(r.Members) / 2; withDoc < min {
				t.Fatalf("%d of %d members carry the description's prose, want at "+
					"least %d -- attachDocs matched almost nothing, which is what a "+
					"member kind with no case in its switch looks like",
					withDoc, len(r.Members), min)
			}

			ev := GenerateEvents(a)
			g, err := GenerateGoWith(a, r, ev, "fkapi")
			if err != nil {
				t.Fatal(err)
			}
			rb, err := GenerateRust(a, r, ev)
			if err != nil {
				t.Fatal(err)
			}
			// BY NAME, because a fraction cannot say that what was attached is
			// the member's own sentence rather than a neighbour's. `add`'s
			// description is stable across every published description.
			const add = "Add a new child element to this GuiElement."
			if !strings.Contains(g.Source, "// Add: "+add) {
				t.Errorf("%s: the Go bindings carry no doc comment on LuaGuiElement::add", v)
			}
			if !strings.Contains(rb.Source, "    /// "+add) {
				t.Errorf("%s: the Rust bindings carry no doc comment on LuaGuiElement::add", v)
			}
			// AND THE BACKTICKS ARE GONE FROM GO AND KEPT IN RUST, which is one
			// sentence rendered two ways under a hard constraint on one side:
			// the generated Go package is carried through a raw string
			// downstream (TestNoBacktickReachesTheGeneratedSources), and Rust's
			// /// is markdown. LuaControl::driving's prose has one at every pin.
			const driving = "if the player is in a vehicle."
			if !strings.Contains(g.Source, "// Driving: 'true' "+driving) {
				t.Errorf("%s: the Go doc comment for LuaControl::driving did not "+
					"replace the description's backticks", v)
			}
			if !strings.Contains(rb.Source, "/// `true` "+driving) {
				t.Errorf("%s: the Rust doc comment for LuaControl::driving lost "+
					"the description's markdown", v)
			}
		})
	}
}

// A SENTENCE ENDS AT `. ` AND NOT AT EVERY `.`, which is the whole of the
// first-sentence policy and the one place it can go quietly wrong.
//
// The descriptions are full of `defines.events`, `1.0` and version numbers, so
// cutting at the first period would truncate a large fraction of them
// mid-clause -- and a truncated doc comment reads as a complete one.
func TestFirstSentenceCutsAtASentenceAndNotAtEveryPeriod(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Add a new child element. It must be unique.", "Add a new child element."},
		{"Reads defines.events.on_tick and nothing else.",
			"Reads defines.events.on_tick and nothing else."},
		{"Defaults to 1.0 when absent.", "Defaults to 1.0 when absent."},
		{"One sentence with no terminator", "One sentence with no terminator"},
		{"Wrapped\nover\nlines. And a second.", "Wrapped over lines."},
		{"", ""},
		// The API's own link markup, which is not markdown any renderer here
		// understands. oneLine strips the scheme and leaves the label.
		{"See [on_tick](runtime:on_tick) for more. Ignored.",
			"See [on_tick] for more."},
	} {
		if got := FirstSentence(c.in); got != c.want {
			t.Errorf("FirstSentence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// THE DOC PROSE MUST NOT REACH THE PACKAGED MEMBER TABLE.
//
// That table ships inside every mod, and 148 KB of prose no host call reads
// would be paid by every player on every load -- which is the same argument
// `fklua mod`'s whole pruning pass rests on. Nothing looks at Member.Doc on the
// way to the table today, and this is what says so after somebody adds a field
// to the row.
func TestTheDocProseDoesNotReachThePackagedTable(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSourceWith(a, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "Add a new child element") {
		t.Error("the member table carries the description's prose")
	}
	// ...and the ABI signature must not either: it digests the WIRE, and prose
	// is not part of it. Proved by moving one and watching the digest stand
	// still, which is the opposite direction from the typed-block test.
	before := APISignature(a)
	for i := range a.Classes {
		for j := range a.Classes[i].Methods {
			a.Classes[i].Methods[j].Description = "moved"
		}
	}
	if after := APISignature(a); after != before {
		t.Errorf("rewriting every method's prose moved the ABI signature "+
			"%s -> %s: the digest is about the wire", before, after)
	}
}

// `fklua docs` RENDERS PARAMETER LISTS AND VARIANT-GROUP FIELDS.
//
// A variant group's field names are the ones that appear NOWHERE in the guest's
// language: they have no struct field, they go in the typed form's `extra`, and
// there are 341 of them for LuaGuiElement::add alone. The reference is the only
// place a guest author can read them without leaving the project.
func TestDocsRenderParametersAndVariantGroups(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	g, err := GenerateGoWith(a, r, ev, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	md := Docs(a, r, ev, DocOptions{Lang: "go", Names: g.Names})
	for _, want := range []string{
		// A plain parameter table, with the DESCRIPTION's own type spelling.
		"| `type` | `GuiElementType` | required |",
		"| `style` | `string` | optional |",
		// The shared/variant split, named the way the bindings name it.
		"Shared parameters:",
		"Variant groups (21), selected by the table's discriminant.",
		// A field that exists in no generated struct in either language.
		"| `mouse_button_filter` | `MouseButtonFlags` | optional |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the rendered reference is missing %q", want)
		}
	}
	// AND A MEMBER THAT IS NOT A METHOD RENDERS NO PARAMETER TABLE, which is
	// what stops an attribute growing an empty one.
	i := strings.Index(md, "### `Caption`")
	if i < 0 {
		t.Fatal("LuaGuiElement::caption is not in the reference at all")
	}
	if j := strings.Index(md[i:], "### "); j > 0 {
		if strings.Contains(md[i:i+j], "Parameters:") {
			t.Error("an attribute rendered a parameter table")
		}
	}
}
