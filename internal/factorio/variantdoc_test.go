package factorio

import (
	"sort"
	"strings"
	"testing"
)

// A VARIANT-GROUP METHOD'S DOC COMMENT NAMES ITS GROUPS AND THEIR PARAMETERS,
// in both backends, in all three places the groups are reachable from.
//
// The tail of such a method crosses as tier 2, so its parameter names have no
// field, no type and no identifier anywhere in the generated crate. What the
// bindings said was that "the variant tail goes in extra" -- a parameter name
// and nothing that goes in it -- from which a reader cannot learn that
// `position` is a parameter of `create_segmented_unit` at all. Reported by
// WormholeBelts as item 11 of its gaps ledger, whose first reading of the typed
// path was that it was unusable.
//
// THE POPULATION AND THE EXPECTATIONS COME FROM THE DESCRIPTION, not from
// VariantGroupDocs, so this test cannot agree with the code by construction: the
// members are the ones the JSON declares `variant_parameter_groups` on, and the
// names asserted are the JSON's own.
func TestAVariantGroupMethodsDocNamesItsGroupsAndTheirParameters(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			byName := map[string]Member{}
			for _, m := range gen.Members.Members {
				if m.Kind == MemberCall {
					byName[MemberKey(m)] = m
				}
			}

			checked := 0
			for _, c := range gen.API.Classes {
				for _, meth := range c.Methods {
					if len(meth.VariantGroups) == 0 {
						continue
					}
					m, ok := byName[MemberKey(Member{Class: c.Name, Name: meth.Name, Kind: MemberCall})]
					if !ok {
						// A variant-group method the host did not bind at all
						// has no doc comment to carry anything. Nothing here
						// produces one at any committed pin; say so rather than
						// passing in silence.
						t.Logf("%s::%s is not bound, so it has no doc comment to check",
							c.Name, meth.Name)
						continue
					}
					checked++
					want := describedGroups(meth)

					goName := gen.Go.Names[MemberKey(m)]
					rsName := gen.Rust.Names[MemberKey(m)]
					if goName == "" || rsName == "" {
						t.Fatalf("%s::%s bound on the host and emitted by neither "+
							"backend (go %q, rust %q)", c.Name, meth.Name, goName, rsName)
					}
					goClass := exportName(c.Name)
					// THE TYPED FORM'S ARGUMENT STRUCT, named the way gogen and
					// rustgen name it: the class, the member, and the `args`
					// parameter, which is what the fallback in goMemberVariant
					// builds when the block has no concept of its own.
					argStruct := goClass + exportName(meth.Name) + "Args"

					sites := []struct {
						what, anchor, marker string
						src                  string
					}{
						{"the Go plain form", "func (o " + goClass + ") " + goName + "(args ", "//", gen.Go.Source},
						{"the Rust plain form", "pub fn " + rsName + "(&self, args: ", "///", gen.Rust.Source},
					}
					if len(m.TypedArgs) > 0 {
						sites = append(sites,
							struct{ what, anchor, marker, src string }{
								"the Go typed form",
								"func (o " + goClass + ") " + goName + "Typed(args ", "//", gen.Go.Source},
							struct{ what, anchor, marker, src string }{
								"the Rust typed form",
								"pub fn " + rsName + "_typed(&self, args: ", "///", gen.Rust.Source},
							struct{ what, anchor, marker, src string }{
								"the Go argument struct",
								"type " + argStruct + " struct {", "//", gen.Go.Source},
							struct{ what, anchor, marker, src string }{
								"the Rust argument struct",
								"pub struct " + argStruct + " {", "///", gen.Rust.Source},
						)
					}
					for _, s := range sites {
						blocks := docBlocksAbove(s.src, s.anchor, s.marker)
						if len(blocks) == 0 {
							t.Errorf("%s::%s: %s has no doc comment at all (anchor %q)",
								c.Name, meth.Name, s.what, s.anchor)
							continue
						}
						if !anyBlockLists(blocks, want) {
							t.Errorf("%s::%s: %s does not list the variant groups.\n"+
								"want %v\ngot  %v", c.Name, meth.Name, s.what,
								want, groupsOfBlocks(blocks))
						}
					}
				}
			}
			if checked == 0 {
				t.Fatal("no variant-group method was checked: a walk that matched " +
					"nothing passes forever, and every committed description has four")
			}
		})
	}
}

// A METHOD WITH NO VARIANT GROUPS GETS NO LISTING, which is the other half of
// the property and the one a renderer that fires unconditionally would break.
//
// The member is DERIVED: the first bound method, in sorted order, on a class
// that also declares a variant-group method and whose own parameter table
// declares no groups. Naming one would make this a test about that method.
func TestAMethodWithoutVariantGroupsGetsNoGroupListing(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			owners := map[string]bool{}
			for _, c := range gen.API.Classes {
				for _, meth := range c.Methods {
					if len(meth.VariantGroups) > 0 {
						owners[c.Name] = true
					}
				}
			}
			var keys []string
			plain := map[string]Member{}
			for _, m := range gen.Members.Members {
				if m.Kind != MemberCall || !owners[m.Class] || len(m.VariantGroups) > 0 {
					continue
				}
				if gen.Go.Names[MemberKey(m)] == "" || gen.Rust.Names[MemberKey(m)] == "" {
					continue
				}
				keys = append(keys, MemberKey(m))
				plain[MemberKey(m)] = m
			}
			sort.Strings(keys)
			if len(keys) == 0 {
				t.Fatal("no group-free method on a class that owns a variant-group " +
					"one: the derivation matched nothing and would pass forever")
			}
			m := plain[keys[0]]
			for _, s := range []struct{ what, anchor, marker, src string }{
				{"Go", "func (o " + exportName(m.Class) + ") " +
					gen.Go.Names[MemberKey(m)] + "(", "//", gen.Go.Source},
				{"Rust", "pub fn " + gen.Rust.Names[MemberKey(m)] + "(", "///", gen.Rust.Source},
			} {
				blocks := docBlocksAbove(s.src, s.anchor, s.marker)
				if len(blocks) == 0 {
					t.Fatalf("%s: %s has no doc comment at all (anchor %q), so this "+
						"test proves nothing", s.what, MemberKey(m), s.anchor)
				}
				for _, b := range blocks {
					if len(listingEntries(b)) > 0 {
						t.Errorf("%s: %s declares no variant groups and its doc "+
							"comment lists %v", s.what, MemberKey(m), listingEntries(b))
					}
					if strings.Contains(strings.Join(b, " "), "variant parameter group") {
						t.Errorf("%s: %s declares no variant groups and its doc "+
							"comment talks about them", s.what, MemberKey(m))
					}
				}
			}
		})
	}
}

// describedGroups reads the expectation straight out of the description: group
// name to its parameter names, in no particular order.
func describedGroups(meth Method) map[string][]string {
	out := map[string][]string{}
	for _, g := range meth.VariantGroups {
		names := make([]string, 0, len(g.Parameters))
		for _, p := range g.Parameters {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		out[g.Name] = names
	}
	return out
}

// docBlocksAbove returns the comment block immediately above EVERY line that
// contains anchor, with the comment marker and one following space stripped.
//
// Every match rather than the first: a member declared on a base class is
// forwarded onto its children, and a forwarder carries no doc comment of its
// own, so a caller must be free to ask whether ANY of them documents the thing.
func docBlocksAbove(src, anchor, marker string) [][]string {
	lines := strings.Split(src, "\n")
	var out [][]string
	for i, l := range lines {
		if !strings.Contains(l, anchor) {
			continue
		}
		var block []string
		for j := i - 1; j >= 0; j-- {
			t := strings.TrimLeft(lines[j], " \t")
			if strings.HasPrefix(t, "#[") {
				// A Rust attribute sits BETWEEN the doc comment and the item it
				// annotates, so walking back from `pub struct X {` meets
				// `#[derive(...)]` before the first `///`. It carries no
				// documentation; step over it rather than stopping.
				continue
			}
			if !strings.HasPrefix(t, marker) {
				break
			}
			t = strings.TrimPrefix(t, marker)
			block = append([]string{strings.TrimPrefix(t, " ")}, block...)
		}
		if len(block) > 0 {
			out = append(out, block)
		}
	}
	return out
}

// listingEntries pulls the group listing back out of a doc block: an entry per
// group, its continuation lines folded in.
//
// AN ENTRY IS AN INDENTED LINE, which is the shape both doc systems render as
// preformatted -- a tab once gofmt has been over the Go source, four spaces in
// the Rust one. A line indented FURTHER continues the entry above it.
func listingEntries(block []string) map[string][]string {
	out := map[string][]string{}
	var order []string
	cur := ""
	text := map[string]string{}
	for _, l := range block {
		body, ok := indented(l)
		if !ok {
			cur = ""
			continue
		}
		if strings.HasPrefix(body, "  ") {
			if cur != "" {
				text[cur] += " " + strings.TrimSpace(body)
			}
			continue
		}
		name, rest, _ := strings.Cut(body, ":")
		cur = name
		order = append(order, name)
		text[name] = strings.TrimSpace(rest)
	}
	for _, name := range order {
		body := text[name]
		if i := strings.Index(body, " -- "); i >= 0 {
			body = body[:i]
		}
		var params []string
		for _, p := range strings.Split(body, ", ") {
			p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), " (required)"))
			if p != "" {
				params = append(params, p)
			}
		}
		sort.Strings(params)
		out[name] = params
	}
	return out
}

// indented reports a preformatted doc line and returns it without the one level
// of indent that makes it one.
func indented(l string) (string, bool) {
	if strings.HasPrefix(l, "\t") {
		return strings.TrimPrefix(l, "\t"), true
	}
	if strings.HasPrefix(l, "    ") {
		return strings.TrimPrefix(l, "    "), true
	}
	return "", false
}

func anyBlockLists(blocks [][]string, want map[string][]string) bool {
	for _, b := range blocks {
		got := listingEntries(b)
		if len(got) != len(want) {
			continue
		}
		ok := true
		for name, params := range want {
			g, present := got[name]
			if !present || strings.Join(g, ",") != strings.Join(params, ",") {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func groupsOfBlocks(blocks [][]string) []map[string][]string {
	var out []map[string][]string
	for _, b := range blocks {
		if g := listingEntries(b); len(g) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// THE RUST LISTING IS FENCED AND THE GO ONE IS NOT, which is one rendering
// under two doc systems that read an indented block differently.
//
// godoc reads it as preformatted and stops there. rustdoc reads it as
// preformatted AND AS A DOCTEST: a CommonMark indented code block is collected,
// compiled as Rust and run, so the twelve listings the generator emits arrive at
// `cargo test --doc -p fkapi` as twelve programs saying things like
// `body-nodes: body_nodes (required)`, every one of which fails to compile. A
// fence with the `text` info string is the documented way to say "not Rust".
//
// THE ANCHORS ARE THE OTHER TEST'S, deliberately: this asks about the same
// sites, so a site that stops carrying a listing at all fails there rather than
// silently passing here.
func TestTheRustGroupListingIsFencedAsTextAndTheGoOneIsNot(t *testing.T) {
	for _, v := range committedVersions(t) {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			byName := map[string]Member{}
			for _, m := range gen.Members.Members {
				if m.Kind == MemberCall {
					byName[MemberKey(m)] = m
				}
			}
			checked := 0
			for _, c := range gen.API.Classes {
				for _, meth := range c.Methods {
					if len(meth.VariantGroups) == 0 {
						continue
					}
					m, ok := byName[MemberKey(Member{Class: c.Name, Name: meth.Name, Kind: MemberCall})]
					if !ok {
						continue
					}
					goName, rsName := gen.Go.Names[MemberKey(m)], gen.Rust.Names[MemberKey(m)]
					if goName == "" || rsName == "" {
						continue
					}
					goClass := exportName(c.Name)
					argStruct := goClass + exportName(meth.Name) + "Args"
					sites := []struct {
						what, anchor, marker, src string
						rust                      bool
					}{
						{"the Go plain form", "func (o " + goClass + ") " + goName + "(args ", "//", gen.Go.Source, false},
						{"the Rust plain form", "pub fn " + rsName + "(&self, args: ", "///", gen.Rust.Source, true},
					}
					if len(m.TypedArgs) > 0 {
						sites = append(sites,
							struct {
								what, anchor, marker, src string
								rust                      bool
							}{"the Go typed form",
								"func (o " + goClass + ") " + goName + "Typed(args ", "//", gen.Go.Source, false},
							struct {
								what, anchor, marker, src string
								rust                      bool
							}{"the Rust typed form",
								"pub fn " + rsName + "_typed(&self, args: ", "///", gen.Rust.Source, true},
							struct {
								what, anchor, marker, src string
								rust                      bool
							}{"the Go argument struct",
								"type " + argStruct + " struct {", "//", gen.Go.Source, false},
							struct {
								what, anchor, marker, src string
								rust                      bool
							}{"the Rust argument struct",
								"pub struct " + argStruct + " {", "///", gen.Rust.Source, true},
						)
					}
					for _, s := range sites {
						var listing []string
						for _, b := range docBlocksAbove(s.src, s.anchor, s.marker) {
							if len(listingEntries(b)) > 0 {
								listing = b
								break
							}
						}
						if listing == nil {
							// The listing itself is the other test's property.
							// Missing here means that one is already red.
							continue
						}
						checked++
						entries, unfenced, opener := fenceState(listing)
						if entries == 0 {
							t.Errorf("%s::%s: %s parsed a listing with no indented "+
								"entry line", c.Name, meth.Name, s.what)
							continue
						}
						if !s.rust {
							if opener != "" {
								t.Errorf("%s::%s: %s is fenced with %q; the Go "+
									"listing is an indented block and godoc has no "+
									"doctest to protect it from",
									c.Name, meth.Name, s.what, opener)
							}
							continue
						}
						if opener != "```text" {
							t.Errorf("%s::%s: %s opens its listing with %q, not "+
								"```text, so rustdoc will collect it as a doctest",
								c.Name, meth.Name, s.what, opener)
						}
						if unfenced != 0 {
							t.Errorf("%s::%s: %s leaves %d of %d listing lines "+
								"outside the fence", c.Name, meth.Name, s.what,
								unfenced, entries)
						}
					}
				}
			}
			if checked == 0 {
				t.Fatal("no variant-group listing was examined: a walk that " +
					"matched nothing passes forever")
			}
		})
	}
}

// fenceState walks a doc block and reports how many indented listing lines it
// has, how many of those sit OUTSIDE a fence, and the opening fence line when
// there is one.
func fenceState(block []string) (entries, unfenced int, opener string) {
	open := false
	for _, l := range block {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "```") {
			if !open && opener == "" {
				opener = t
			}
			open = !open
			continue
		}
		if _, ok := indented(l); !ok || strings.TrimSpace(l) == "" {
			continue
		}
		entries++
		if !open {
			unfenced++
		}
	}
	return entries, unfenced, opener
}

// VariantGroupLines ITSELF, on groups this repository's descriptions do not
// contain.
//
// The renderings above are read back out of generated sources, which can only
// exercise the shapes the committed descriptions happen to have -- and no group
// at any committed pin carries a DESCRIPTION, so the branch that appends one has
// never been executed by a test that could see it fail. The same is true of a
// group with no parameters at all. Both are hand-built here, and the expected
// text is written out rather than recomputed.
func TestVariantGroupLinesRendersNamesRequirednessAndDescriptions(t *testing.T) {
	gs := []GroupDoc{
		{Name: "body-nodes", Params: []ParamDoc{{Name: "body_nodes", Required: true}}},
		{
			Name:        "position-and-direction",
			Description: "Only when the unit has a head.",
			Params: []ParamDoc{
				{Name: "position", Required: true},
				{Name: "direction"},
				{Name: "extended"},
			},
		},
		{Name: "bare"},
	}
	want := []string{
		"    body-nodes: body_nodes (required)",
		"    position-and-direction: position (required), direction, extended --",
		"      Only when the unit has a head.",
		"    bare:",
	}
	got := VariantGroupLines(gs, 76)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	// THE ORDER IS THE CALLER'S. Sorting by name happens in VariantGroupDocs,
	// where the description's groups are reduced; this renders what it is given,
	// so a caller that has already ordered them is not reordered underneath.
	if got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
		t.Error("the entries were reordered")
	}
	if len(VariantGroupLines(nil, 76)) != 0 {
		t.Error("no groups rendered something")
	}
}

// A WIDTH TOO SMALL TO WRAP TO IS CLAMPED, and the clamp is a floor rather than
// a refusal because the callers pass a constant.
//
// Wrapping to single digits produces one word per line, which is unreadable and
// would not say why. The property is that every width below the floor renders
// exactly what the floor renders, and that the floor really does wrap something
// a generous width does not -- otherwise the clamp could be a no-op and this
// would still pass.
func TestVariantGroupLinesClampsAWidthTooSmallToWrapTo(t *testing.T) {
	gs := []GroupDoc{{Name: "item-stack", Params: []ParamDoc{
		{Name: "inventory_index", Required: true},
		{Name: "item_stack_index", Required: true},
		{Name: "source", Required: true},
	}}}
	floor := VariantGroupLines(gs, 24)
	if len(floor) < 2 {
		t.Fatalf("the floor width did not wrap this entry at all: %q", floor)
	}
	if wide := VariantGroupLines(gs, 200); len(wide) != 1 {
		t.Fatalf("a generous width wrapped anyway (%d lines), so the comparison "+
			"below says nothing: %q", len(wide), wide)
	}
	for _, w := range []int{-1, 0, 1, 23} {
		got := VariantGroupLines(gs, w)
		if strings.Join(got, "\n") != strings.Join(floor, "\n") {
			t.Errorf("width %d rendered\n%q\nand the floor renders\n%q", w, got, floor)
		}
	}
}
