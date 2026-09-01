package factorio

import (
	"sort"
	"strings"
)

// The VARIANT PARAMETER GROUPS of a method, reduced once and rendered by
// everything that shows them.
//
// A variant-group method's parameter table is a discriminated union: the shared
// parameters are ordinary described parameters, and the tail is a group of
// parameters selected by a discriminant. The tail crosses as tier 2 -- as the
// whole of `args` in the plain form, and as `extra` beside the block in the
// typed one -- so its parameter names have no field, no type and no identifier
// anywhere in the guest's language.
//
// AND NOTHING SAID WHERE THEY WENT. `LuaSurface::create_segmented_unit`
// declares four ordinary parameters and two variant groups, so the generated
// LuaSurfaceCreateSegmentedUnitArgs carries the four and the typed method's doc
// comment said only that the variant tail goes in `extra`. From that a reader
// cannot learn that `position` is a parameter of the method at all -- reported
// by WormholeBelts (item 11 of its gaps ledger), whose first reading of the
// typed path was that it was unusable.
//
// ONE HELPER, FOUR RENDERINGS. The listing below is produced here and rendered
// into the typed variant's doc comment, the generated Args struct's doc comment
// and the plain tier-2 form's doc comment in BOTH backends, plus `fklua docs`'
// own group table. A fact rendered by two generators and the docs renderer
// comes from one helper: three spellings of one listing is how they drift, and
// this package has met that shape often enough to have a rule about it.
//
// IT IS DOCUMENTATION AND NOTHING ELSE. No member id, no layout and no byte of
// the wire depends on any of it -- the same standing FieldSpec.LazyPayload has,
// and for the same reason: what a reader needs and what the host needs are
// different questions.

// ParamDoc is one parameter of a method or of a variant group, in the
// DESCRIPTION's own spelling of its type.
//
// The type is the description's rather than the generated Go or Rust one on
// purpose: a reader who wants the binding's type has the binding, and what they
// cannot get anywhere else is what Factorio calls it.
type ParamDoc struct {
	Name string
	Type string
	// Required is the description's `optional` inverted, because a listing
	// reads better marking the few that must be there than the many that need
	// not be.
	Required    bool
	Description string
}

// GroupDoc is one variant parameter group reduced to what a reader needs.
type GroupDoc struct {
	Name string
	// Description is the group's own prose, first sentence, flattened. Empty
	// for every group at every description committed here, which is precisely
	// why it is carried rather than assumed away.
	Description string
	Params      []ParamDoc
}

// VariantDoc is a group listing carried to a place that renders it, with the
// member it belongs to.
//
// It travels on the FieldSpec of the typed form's argument struct, because that
// struct's doc comment is written from the struct's own name and has no other
// route back to the method whose shared parameters it holds.
type VariantDoc struct {
	// Owner is "Class::method", the description's own spelling.
	Owner  string
	Groups []GroupDoc
}

// ParamDocs reduces a parameter list, in the description's own `order`.
func ParamDocs(ps []Parameter) []ParamDoc {
	in := append([]Parameter(nil), ps...)
	sort.SliceStable(in, func(i, j int) bool { return in[i].Order < in[j].Order })
	out := make([]ParamDoc, 0, len(in))
	for _, p := range in {
		out = append(out, ParamDoc{
			Name:        p.Name,
			Type:        p.Type.String(),
			Required:    !p.Optional,
			Description: oneLine(p.Description),
		})
	}
	return out
}

// VariantGroupDocs reduces a method's variant parameter groups.
//
// GROUPS BY NAME, PARAMETERS BY `order`. A group's `order` is a rendering hint
// for the wiki and its name is what a guest author types, so sorting the groups
// by name puts the listing in the order somebody looking one up would scan; a
// parameter's order is the description's own statement about its own table and
// is kept. Both are total and deterministic, which is what a generated source
// file needs from them.
func VariantGroupDocs(gs []VariantGroup) []GroupDoc {
	in := append([]VariantGroup(nil), gs...)
	sort.SliceStable(in, func(i, j int) bool { return in[i].Name < in[j].Name })
	out := make([]GroupDoc, 0, len(in))
	for _, g := range in {
		out = append(out, GroupDoc{
			Name:        g.Name,
			Description: oneLine(g.Description),
			Params:      ParamDocs(g.Parameters),
		})
	}
	return out
}

// VariantGroupLines renders the listing as doc-comment BODY lines: one entry
// per group, wrapped at `width`, with continuations indented two more than the
// entry they continue.
//
// It returns the lines WITHOUT a comment marker, so each backend prefixes its
// own (`// `, `/// `) and neither has to know how the other spells a comment.
//
// FOUR SPACES OF INDENT, WHICH IS A RENDERING DECISION AND NOT WHITESPACE. Both
// doc systems read an indented block as PREFORMATTED: gofmt rewrites it to a
// tab and godoc shows a code block, and four spaces after Rust's `/// ` is a
// CommonMark indented code block. Two spaces would be a lazy continuation of
// the sentence above in rustdoc, so the whole listing would render as one run-on
// paragraph with its line breaks gone -- which is the one thing a listing may
// not do. A blank comment line goes between the introducing sentence and the
// first entry, because an indented block cannot interrupt a paragraph -- and
// only the RUST backend emits it. The Go one does not: it writes the intro and
// the entries with nothing between them, and `go/format` supplies the separator
// (and rewrites the four spaces to a tab) when gogen runs the whole file
// through it. So the committed Go source carries the shape and the generator
// never wrote it, which is why the test's indented() helper takes a tab or four
// spaces rather than pinning one.
//
// AND IN A RUST DOC COMMENT AN INDENTED BLOCK IS ALSO A DOCTEST, which the Go
// side has no equivalent of: rustdoc collects it, compiles it as Rust and runs
// it. The indent stays and the RUST backend fences it -- see rustListing, which
// is the one caller that wraps this and the reason it is a separate function
// rather than a flag here.
//
// NO BACKTICKS ARE ADDED HERE, which is the one place the two backends really
// do differ: Go's generated package is carried through a raw string downstream
// and TestNoBacktickReachesTheGeneratedSources is the standing gate, so the Go
// site replaces any backtick the DESCRIPTION brought with a single quote.
// Adding decoration here would put that gate's problem in the shared code and
// then hide it behind a per-backend fix.
func VariantGroupLines(gs []GroupDoc, width int) []string {
	if width < 24 {
		// A width this small is a caller bug rather than a request; wrapping to
		// it would produce one word per line, which is unreadable and would not
		// say so.
		width = 24
	}
	var out []string
	for _, g := range gs {
		names := make([]string, 0, len(g.Params))
		for _, p := range g.Params {
			if p.Required {
				names = append(names, p.Name+" (required)")
				continue
			}
			names = append(names, p.Name)
		}
		text := g.Name + ":"
		if len(names) > 0 {
			text += " " + strings.Join(names, ", ")
		}
		if g.Description != "" {
			text += " -- " + g.Description
		}
		for i, l := range wrapComment(text, width-6) {
			if i == 0 {
				out = append(out, "    "+l)
				continue
			}
			out = append(out, "      "+l)
		}
	}
	return out
}

// rustListing is VariantGroupLines FENCED, which is what a RUST doc comment
// needs and a Go one does not.
//
// AN INDENTED BLOCK IN A `///` COMMENT IS A DOCTEST. rustdoc collects a
// CommonMark indented code block exactly as it collects a bare ``` fence,
// compiles it as Rust and runs it -- so the listing that renders correctly in
// godoc arrives at `cargo test --doc` as twelve programs that say things like
// `body-nodes: body_nodes (required)`, and every one of them fails to compile.
// A fence with the `text` info string is the documented way to say "this is not
// Rust": rustdoc does not test it, and the rendered block loses the Run button
// it should never have had.
//
// THE INDENT STAYS INSIDE THE FENCE. It is what groups a wrapped continuation
// line under the entry it continues, and a fenced block preserves it verbatim;
// dropping it here would make the two backends' listings differ in shape for a
// reason that has nothing to do with either.
//
// The fence is the only backtick this file emits and it is Rust-only, which is
// the same split goDocText is: the GO package is carried through a raw string
// downstream and TestNoBacktickReachesTheGeneratedSources is the standing gate.
func rustListing(gs []GroupDoc, width int) []string {
	body := VariantGroupLines(gs, width)
	if len(body) == 0 {
		return nil
	}
	out := make([]string, 0, len(body)+2)
	out = append(out, "```text")
	out = append(out, body...)
	return append(out, "```")
}

// goDocText is the description's prose made safe for the GENERATED GO package.
//
// The package is carried through a raw string downstream, so a backtick
// arriving from data would close it -- TestNoBacktickReachesTheGeneratedSources
// is the standing gate. A single quote rather than nothing, so `true` reads as
// 'true' and the author's own emphasis survives. Rust's /// is markdown and
// keeps them, which is one sentence rendered two ways under a hard constraint
// on one side rather than a drift.
func goDocText(s string) string { return strings.ReplaceAll(s, "`", "'") }
