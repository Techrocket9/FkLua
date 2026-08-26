package factorio

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The semantic API diff.
//
// `census.json` answers "how much moved" and this answers "what moved, and does
// it break me". They are different questions and neither substitutes for the
// other: a version bump that removes one member and adds one leaves every
// census count identical.
//
// Every change lands in exactly one class, and the boundary is drawn from the
// GUEST's point of view rather than the API's:
//
//   - BREAKING: a guest that compiled before may now fail. A member or class
//     removed, a parameter added or removed, a type changed, an event field
//     gone. Note that ADDING a parameter is breaking here even though it is
//     source-compatible in Lua -- FkLua's wire layout is positional, so a new
//     parameter moves every offset after it.
//   - ADDITIVE: new surface. Nothing that existed changed.
//   - COSMETIC: descriptions, examples, ordering. Regenerating produces a
//     different file and no different behaviour.
//
// The classification is what makes an upgrade reviewable: a human reads the
// breaking list and nothing else.

// ChangeKind is how a single change is classified.
type ChangeKind int

const (
	// Cosmetic is documentation-only: a description or an example moved.
	Cosmetic ChangeKind = iota
	// Additive is new surface that cannot break an existing guest.
	Additive
	// Breaking may stop an existing guest compiling or change what it does.
	Breaking
)

func (k ChangeKind) String() string {
	switch k {
	case Breaking:
		return "BREAKING"
	case Additive:
		return "additive"
	}
	return "cosmetic"
}

// Change is one difference between two API versions.
type Change struct {
	Kind ChangeKind `json:"kind"`
	// What identifies the thing: "LuaEntity::destroy", "on_tick",
	// "MapPosition". Stable enough to grep for in a mod's source.
	What string `json:"what"`
	// Detail says what happened, in a sentence a mod author can act on.
	Detail string `json:"detail"`
}

// APIDiff is every change between two versions, classified.
type APIDiff struct {
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Changes []Change `json:"changes"`
}

// Counts returns how many changes fall in each class.
func (d APIDiff) Counts() (breaking, additive, cosmetic int) {
	for _, c := range d.Changes {
		switch c.Kind {
		case Breaking:
			breaking++
		case Additive:
			additive++
		default:
			cosmetic++
		}
	}
	return
}

// Breaking returns only the changes that can break a guest.
func (d APIDiff) Breaking() []Change {
	var out []Change
	for _, c := range d.Changes {
		if c.Kind == Breaking {
			out = append(out, c)
		}
	}
	return out
}

// DiffAPI compares two API descriptions.
func DiffAPI(from, to *API) APIDiff {
	d := APIDiff{From: from.ApplicationVersion, To: to.ApplicationVersion}
	add := func(k ChangeKind, what, detail string) {
		d.Changes = append(d.Changes, Change{Kind: k, What: what, Detail: detail})
	}

	if from.APIVersion != to.APIVersion {
		// The SCHEMA changed, not the API. Every generator reads it, so this is
		// the one change that can invalidate the whole pipeline rather than one
		// member -- and it has held at 6 across every version seen so far.
		add(Breaking, "api_version",
			fmt.Sprintf("the JSON schema went %d -> %d; the generators read it directly",
				from.APIVersion, to.APIVersion))
	}

	diffClasses(from, to, add)
	diffEvents(from, to, add)
	diffConcepts(from, to, add)
	diffDefines(from, to, add)

	// Deterministic order: breaking first (that is the list a human reads), then
	// by name. Map iteration must never reach the output.
	sort.SliceStable(d.Changes, func(i, j int) bool {
		if d.Changes[i].Kind != d.Changes[j].Kind {
			return d.Changes[i].Kind > d.Changes[j].Kind
		}
		if d.Changes[i].What != d.Changes[j].What {
			return d.Changes[i].What < d.Changes[j].What
		}
		return d.Changes[i].Detail < d.Changes[j].Detail
	})
	return d
}

func diffClasses(from, to *API, add func(ChangeKind, string, string)) {
	oldC := map[string]Class{}
	for _, c := range from.Classes {
		oldC[c.Name] = c
	}
	newC := map[string]Class{}
	for _, c := range to.Classes {
		newC[c.Name] = c
	}

	for _, c := range from.Classes {
		n, ok := newC[c.Name]
		if !ok {
			add(Breaking, c.Name, "class removed")
			continue
		}
		if c.Parent != n.Parent {
			// Inheritance is real: a member reached through a parent is
			// callable on the child and appears in neither list, so a changed
			// parent silently moves the whole inherited surface.
			add(Breaking, c.Name,
				fmt.Sprintf("parent changed %q -> %q, which moves every inherited member",
					c.Parent, n.Parent))
		}
		diffMembers(c, n, add)
	}
	for _, c := range to.Classes {
		if _, ok := oldC[c.Name]; !ok {
			add(Additive, c.Name, fmt.Sprintf("new class (%d methods, %d attributes)",
				len(c.Methods), len(c.Attributes)))
		}
	}
}

func diffMembers(oldCl, newCl Class, add func(ChangeKind, string, string)) {
	oldM := map[string]Method{}
	for _, m := range oldCl.Methods {
		oldM[m.Name] = m
	}
	newM := map[string]Method{}
	for _, m := range newCl.Methods {
		newM[m.Name] = m
	}
	for _, m := range oldCl.Methods {
		id := oldCl.Name + "::" + m.Name
		n, ok := newM[m.Name]
		if !ok {
			add(Breaking, id, "method removed")
			continue
		}
		diffSignature(id, m, n, add)
		if m.Description != n.Description {
			add(Cosmetic, id, "description changed")
		}
	}
	for _, m := range newCl.Methods {
		if _, ok := oldM[m.Name]; !ok {
			add(Additive, newCl.Name+"::"+m.Name, "new method")
		}
	}

	oldA := map[string]Attribute{}
	for _, a := range oldCl.Attributes {
		oldA[a.Name] = a
	}
	newA := map[string]Attribute{}
	for _, a := range newCl.Attributes {
		newA[a.Name] = a
	}
	for _, a := range oldCl.Attributes {
		id := oldCl.Name + "::" + a.Name
		n, ok := newA[a.Name]
		if !ok {
			add(Breaking, id, "attribute removed")
			continue
		}
		// Readability and writability ARE the presence of read_type and
		// write_type, so one comparison covers both the access and the shape.
		if s, t := optTypeSig(a.ReadType), optTypeSig(n.ReadType); s != t {
			switch {
			case t == "":
				add(Breaking, id, "no longer readable")
			case s == "":
				add(Additive, id, "now readable, as "+t)
			default:
				add(Breaking, id, fmt.Sprintf("read type changed %s -> %s", s, t))
			}
		}
		if s, t := optTypeSig(a.WriteType), optTypeSig(n.WriteType); s != t {
			switch {
			case t == "":
				add(Breaking, id, "no longer writable")
			case s == "":
				add(Additive, id, "now writable, as "+t)
			default:
				add(Breaking, id, fmt.Sprintf("write type changed %s -> %s", s, t))
			}
		}
		if a.Description != n.Description {
			add(Cosmetic, id, "description changed")
		}
	}
	for _, a := range newCl.Attributes {
		if _, ok := oldA[a.Name]; !ok {
			add(Additive, newCl.Name+"::"+a.Name, "new attribute")
		}
	}
}

// diffSignature compares two methods' parameters and returns.
//
// A parameter added or removed is BREAKING even though Lua would tolerate it:
// FkLua lays arguments out positionally in a fixed block, so a new parameter
// moves every offset after it and a guest built against the old layout writes
// into the wrong slots.
func diffSignature(id string, o, n Method, add func(ChangeKind, string, string)) {
	if o.TakesTable() != n.TakesTable() {
		add(Breaking, id, "calling convention changed between positional and table")
		return
	}
	oldP := map[string]Parameter{}
	for _, p := range o.Parameters {
		oldP[p.Name] = p
	}
	newP := map[string]Parameter{}
	for _, p := range n.Parameters {
		newP[p.Name] = p
	}
	for _, p := range o.Parameters {
		q, ok := newP[p.Name]
		if !ok {
			add(Breaking, id, fmt.Sprintf("parameter %q removed", p.Name))
			continue
		}
		if s, t := typeSig(p.Type), typeSig(q.Type); s != t {
			add(Breaking, id, fmt.Sprintf("parameter %q type changed %s -> %s", p.Name, s, t))
		}
		// Optional -> required breaks every caller that omitted it. The reverse
		// is additive.
		if p.Optional && !q.Optional {
			add(Breaking, id, fmt.Sprintf("parameter %q is no longer optional", p.Name))
		}
	}
	for _, p := range n.Parameters {
		if _, ok := oldP[p.Name]; !ok {
			add(Breaking, id, fmt.Sprintf(
				"parameter %q added, which moves the wire offsets after it", p.Name))
		}
	}
	if len(o.ReturnValues) != len(n.ReturnValues) {
		add(Breaking, id, fmt.Sprintf("returns %d value(s), was %d",
			len(n.ReturnValues), len(o.ReturnValues)))
		return
	}
	for i := range o.ReturnValues {
		if s, t := typeSig(o.ReturnValues[i].Type), typeSig(n.ReturnValues[i].Type); s != t {
			add(Breaking, id, fmt.Sprintf("return %d type changed %s -> %s", i, s, t))
		}
	}
}

func diffEvents(from, to *API, add func(ChangeKind, string, string)) {
	oldE := map[string]Event{}
	for _, e := range from.Events {
		oldE[e.Name] = e
	}
	newE := map[string]Event{}
	for _, e := range to.Events {
		newE[e.Name] = e
	}
	for _, e := range from.Events {
		n, ok := newE[e.Name]
		if !ok {
			add(Breaking, e.Name, "event removed")
			continue
		}
		oldF := map[string]Parameter{}
		for _, f := range e.Data {
			oldF[f.Name] = f
		}
		for _, f := range e.Data {
			q, ok := fieldByName(n.Data, f.Name)
			if !ok {
				add(Breaking, e.Name, fmt.Sprintf("field %q removed", f.Name))
				continue
			}
			if s, t := typeSig(f.Type), typeSig(q.Type); s != t {
				add(Breaking, e.Name, fmt.Sprintf("field %q type changed %s -> %s", f.Name, s, t))
			}
		}
		for _, f := range n.Data {
			if _, ok := oldF[f.Name]; !ok {
				// The event's scratch block is laid out by `order`, so a new
				// field moves the ones after it.
				add(Breaking, e.Name, fmt.Sprintf(
					"field %q added, which moves the fields after it", f.Name))
			}
		}
	}
	for _, e := range to.Events {
		if _, ok := oldE[e.Name]; !ok {
			add(Additive, e.Name, "new event")
		}
	}
}

func fieldByName(fs []Parameter, name string) (Parameter, bool) {
	for _, f := range fs {
		if f.Name == name {
			return f, true
		}
	}
	return Parameter{}, false
}

func diffConcepts(from, to *API, add func(ChangeKind, string, string)) {
	oldC := map[string]Concept{}
	for _, c := range from.Concepts {
		oldC[c.Name] = c
	}
	newC := map[string]Concept{}
	for _, c := range to.Concepts {
		newC[c.Name] = c
	}
	for _, c := range from.Concepts {
		n, ok := newC[c.Name]
		if !ok {
			add(Breaking, c.Name, "concept removed")
			continue
		}
		diffShape(c.Name, c.Type, n.Type, add)
	}
	for _, c := range to.Concepts {
		if _, ok := oldC[c.Name]; !ok {
			add(Additive, c.Name, "new concept")
		}
	}
}

// diffShape reports what changed between two types, FIELD BY FIELD when both
// are tables.
//
// The whole-signature form is unreadable at this size -- a BlueprintControlBehavior
// renders as 900 characters of which four matter -- and the point of the
// breaking list is that a human reads it. Falls back to the signature when the
// shapes are not comparable field-wise, because "it changed and here is both"
// still beats silence.
func diffShape(name string, o, n Type, add func(ChangeKind, string, string)) {
	if typeSig(o) == typeSig(n) {
		return
	}
	if o.Complex != "table" || n.Complex != "table" {
		if o.Complex == "union" && n.Complex == "union" {
			diffUnion(name, o, n, add)
			return
		}
		add(Breaking, name, fmt.Sprintf("shape changed %s -> %s", typeSig(o), typeSig(n)))
		return
	}
	oldF := map[string]Parameter{}
	for _, p := range o.Parameters {
		oldF[p.Name] = p
	}
	newF := map[string]Parameter{}
	for _, p := range n.Parameters {
		newF[p.Name] = p
	}
	for _, p := range o.Parameters {
		q, ok := newF[p.Name]
		if !ok {
			add(Breaking, name, fmt.Sprintf("field %q removed", p.Name))
			continue
		}
		if a, b := typeSig(p.Type), typeSig(q.Type); a != b {
			add(Breaking, name, fmt.Sprintf("field %q type changed %s -> %s", p.Name, a, b))
		}
	}
	for _, p := range n.Parameters {
		if _, ok := oldF[p.Name]; !ok {
			// A table concept becomes a fixed-layout struct on the wire, so a
			// new field moves every offset after it.
			add(Breaking, name, fmt.Sprintf(
				"field %q added, which moves the fields after it", p.Name))
		}
	}
}

// diffUnion reports which alternatives a union gained or lost.
//
// Gaining one is ADDITIVE for a reader and breaking for a writer, and tier 2
// carries unions dynamically either way -- so a gained alternative is additive
// here and a lost one is not.
func diffUnion(name string, o, n Type, add func(ChangeKind, string, string)) {
	oldO := map[string]bool{}
	for _, t := range o.Options {
		oldO[typeSig(t)] = true
	}
	newO := map[string]bool{}
	for _, t := range n.Options {
		newO[typeSig(t)] = true
	}
	var gone, gained []string
	for k := range oldO {
		if !newO[k] {
			gone = append(gone, k)
		}
	}
	for k := range newO {
		if !oldO[k] {
			gained = append(gained, k)
		}
	}
	sort.Strings(gone)
	sort.Strings(gained)
	if len(gone) > 0 {
		add(Breaking, name, "union lost "+strings.Join(gone, ", "))
	}
	if len(gained) > 0 {
		add(Additive, name, "union gained "+strings.Join(gained, ", "))
	}
}

// diffDefines reports what moved in `defines`, at BOTH levels.
//
// The group level is the coarse one -- `defines.inventory` gone entirely -- and
// the value level is the one a guest actually stands on: `fk.define` ids are
// dense indices over the flattened value paths (definePaths, gen.go), so
// `defines.inventory.furnace_result` disappearing while `defines.inventory`
// survives is invisible to a group-name comparison and is exactly the shape
// 2.0.77 -> 2.1.12 has twenty of.
//
// Both are reported rather than the finer one alone. A removed group takes its
// values with it and therefore produces a finding per value as well, which is
// not noise: the group line is what a human reads, and the value lines are what
// `api check` cross-references a guest's surface against.
func diffDefines(from, to *API, add func(ChangeKind, string, string)) {
	oldD := map[string]bool{}
	for _, x := range from.Defines {
		oldD[x.Name] = true
	}
	newD := map[string]bool{}
	for _, x := range to.Defines {
		newD[x.Name] = true
	}
	// Iterated over the SLICES rather than the maps, so the order changes reach
	// `add` in is the description's own. DiffAPI sorts afterwards either way;
	// this is so that a caller reading the raw sequence never sees map order.
	for _, x := range from.Defines {
		if !newD[x.Name] {
			add(Breaking, "defines."+x.Name, "define removed")
		}
	}
	for _, x := range to.Defines {
		if !oldD[x.Name] {
			add(Additive, "defines."+x.Name, "new define")
		}
	}

	oldPaths, newPaths := definePaths(from), definePaths(to)
	oldV := map[string]bool{}
	for _, p := range oldPaths {
		oldV[p] = true
	}
	newV := map[string]bool{}
	for _, p := range newPaths {
		newV[p] = true
	}
	for _, p := range oldPaths {
		if !newV[p] {
			add(Breaking, "defines."+p, "define value removed")
		}
	}
	for _, p := range newPaths {
		if !oldV[p] {
			add(Additive, "defines."+p, "new define value")
		}
	}
}

// typeSig renders a type as a comparable string.
//
// Compared as text rather than structurally because the question is "did this
// change", not "how". A false positive costs a reviewer one line; a false
// negative ships a broken mod.
func typeSig(t Type) string {
	if t.Complex == "" {
		return t.Name
	}
	var b strings.Builder
	b.WriteString(t.Complex)
	switch t.Complex {
	case "array":
		b.WriteString("<" + optTypeSig(t.Value) + ">")
	case "dictionary", "LuaCustomTable":
		b.WriteString("<" + optTypeSig(t.Key) + "," + optTypeSig(t.Value) + ">")
	case "union":
		var parts []string
		for _, o := range t.Options {
			parts = append(parts, typeSig(o))
		}
		sort.Strings(parts) // order within a union is not meaningful
		b.WriteString("<" + strings.Join(parts, "|") + ">")
	case "literal":
		fmt.Fprintf(&b, "<%v>", t.Literal)
	case "table", "LuaStruct":
		var parts []string
		for _, p := range t.Parameters {
			parts = append(parts, p.Name+":"+typeSig(p.Type))
		}
		sort.Strings(parts)
		b.WriteString("{" + strings.Join(parts, ",") + "}")
	}
	return b.String()
}

// optTypeSig is typeSig for an optional type, where absent is meaningful --
// an attribute with no write_type is not writable.
func optTypeSig(t *Type) string {
	if t == nil {
		return ""
	}
	return typeSig(*t)
}

// Markdown renders the diff as a report a human reads top-down.
func (d APIDiff) Markdown() string {
	br, ad, co := d.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "# API diff: %s -> %s\n\n", d.From, d.To)
	fmt.Fprintf(&b, "**%d breaking**, %d additive, %d cosmetic.\n\n", br, ad, co)
	if br == 0 {
		b.WriteString("No breaking changes. A guest built against the old version\n")
		b.WriteString("keeps working; regenerate the bindings to reach the new surface.\n\n")
	}
	section := func(title string, want ChangeKind) {
		var rows []Change
		for _, c := range d.Changes {
			if c.Kind == want {
				rows = append(rows, c)
			}
		}
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", title, len(rows))
		for _, c := range rows {
			fmt.Fprintf(&b, "- `%s` — %s\n", c.What, c.Detail)
		}
		b.WriteString("\n")
	}
	section("Breaking", Breaking)
	section("Additive", Additive)
	section("Cosmetic", Cosmetic)
	return b.String()
}

// JSON renders the diff for a tool to consume.
func (d APIDiff) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
