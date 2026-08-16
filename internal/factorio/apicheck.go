package factorio

import (
	"fmt"
	"sort"
	"strings"
)

// `api check`: does MY mod survive the upgrade?
//
// The plan calls this the feature that matters most to a mod author, and the
// reason is arithmetic. 2.0.77 -> 2.1.12 has 200 breaking changes and a typical
// mod touches a few dozen members, so the honest answer for most mods is "none
// of them are yours" -- but only a cross-reference can say that, and without one
// an author reads 200 lines looking for the four that matter.
//
// The manifest is not new work. `UsedMembers` already recovers exactly which
// members a compiled guest references, because the table pruner needs the same
// answer to ship 1 KB instead of the whole ~1 MB table. This joins it to the
// diff.

// GuestSurface is everything about the API one compiled guest touches.
type GuestSurface struct {
	// Members are "Class::name" for every member the guest calls.
	Members []string
	// Events are the event names it subscribes to.
	Events []string
	// Concepts are the named types reachable from those members' signatures --
	// the argument and return shapes it therefore depends on.
	Concepts []string
	// Complete is false when a member or event id was not a compile-time
	// constant, so the scan could not see everything.
	//
	// When this is false the check CANNOT be trusted to be exhaustive, and
	// says so rather than reporting a clean bill.
	Complete bool
}

// SurfaceOf builds the manifest for a guest, given the report its ids index.
func SurfaceOf(r Report, usedMembers map[int]bool, membersComplete bool,
	usedEvents map[int]bool, eventsComplete bool, evs EventReport) GuestSurface {

	s := GuestSurface{Complete: membersComplete && eventsComplete}
	concepts := map[string]bool{}
	for _, m := range r.Members {
		if !usedMembers[m.ID] {
			continue
		}
		s.Members = append(s.Members, m.Class+"::"+m.Name)
		// The signature's named types matter too: a guest calling a member
		// whose argument is a MapGenSettings breaks when MapGenSettings gains a
		// field, even though the member itself did not change.
		for _, f := range m.Args {
			collectTypeNames(f, concepts)
		}
		for _, f := range m.Rets {
			collectTypeNames(f, concepts)
		}
	}
	for _, e := range evs.Events {
		if usedEvents[e.ID] {
			s.Events = append(s.Events, e.Name)
		}
	}
	for c := range concepts {
		s.Concepts = append(s.Concepts, c)
	}
	sort.Strings(s.Members)
	sort.Strings(s.Events)
	sort.Strings(s.Concepts)
	return s
}

// collectTypeNames walks a field spec gathering every named concept in it.
//
// Recursive because a struct field can be a struct, an array of one, or a
// dictionary of one, and a change three levels down still moves the offsets a
// guest compiled against.
func collectTypeNames(f FieldSpec, into map[string]bool) {
	if f.TypeName != "" {
		into[f.TypeName] = true
	}
	for _, sub := range f.Struct {
		collectTypeNames(sub, into)
	}
	if f.Elem != nil {
		collectTypeNames(*f.Elem, into)
	}
	if f.Key != nil {
		collectTypeNames(*f.Key, into)
	}
}

// CheckResult is what an upgrade would do to one guest.
type CheckResult struct {
	From, To string
	// Hits are the breaking changes that touch this guest.
	Hits []Change
	// Ignored counts breaking changes that do not.
	Ignored int
	// Surface is the manifest that was checked, for the report's header.
	Surface GuestSurface
}

// CheckGuest cross-references a guest's surface against a diff.
func CheckGuest(s GuestSurface, d APIDiff) CheckResult {
	res := CheckResult{From: d.From, To: d.To, Surface: s}
	mine := map[string]bool{}
	for _, m := range s.Members {
		mine[m] = true
	}
	for _, e := range s.Events {
		mine[e] = true
	}
	for _, c := range s.Concepts {
		mine[c] = true
	}
	for _, c := range d.Breaking() {
		// api_version is everyone's problem: a schema change invalidates every
		// generator rather than one member.
		if c.What == "api_version" || mine[c.What] {
			res.Hits = append(res.Hits, c)
			continue
		}
		// A member's identity is "Class::name"; a class-level change reports
		// just the class, and takes every member on it with it.
		if cls, _, ok := strings.Cut(c.What, "::"); ok && mine[cls] {
			res.Hits = append(res.Hits, c)
			continue
		}
		hit := false
		for _, m := range s.Members {
			if cls, _, _ := strings.Cut(m, "::"); cls == c.What {
				res.Hits = append(res.Hits, c)
				hit = true
				break
			}
		}
		if !hit {
			res.Ignored++
		}
	}
	return res
}

// Report renders the answer a mod author reads.
func (r CheckResult) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# api check: %s -> %s\n\n", r.From, r.To)
	fmt.Fprintf(&b, "This guest touches %d member(s), %d event(s) and %d named type(s).\n\n",
		len(r.Surface.Members), len(r.Surface.Events), len(r.Surface.Concepts))

	if !r.Surface.Complete {
		b.WriteString("**This check is NOT exhaustive.** A member or event id was not a\n")
		b.WriteString("compile-time constant, so the scan could not see everything the guest\n")
		b.WriteString("reaches. Treat a clean result as unproven rather than as a pass.\n\n")
	}

	if len(r.Hits) == 0 {
		fmt.Fprintf(&b, "**Nothing this guest uses is affected.** %d breaking change(s) in\n", r.Ignored)
		b.WriteString("the release touch members it never calls.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "**%d breaking change(s) affect this guest**, out of %d in the release.\n\n",
		len(r.Hits), len(r.Hits)+r.Ignored)
	for _, c := range r.Hits {
		fmt.Fprintf(&b, "- `%s` — %s\n", c.What, c.Detail)
	}
	return b.String()
}
