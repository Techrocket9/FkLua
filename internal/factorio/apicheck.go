package factorio

import (
	"encoding/json"
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

// Match names WHICH PART OF THE GUEST'S SURFACE a finding touches, and it is
// carried because "why does this concern me" is the question a reader has after
// "what moved". A `MapPosition` gaining a field is a member the guest never
// heard of; it reaches the guest because a member it DOES call takes one.
//
// The values are a closed set and they are a data interface -- `api check
// --json` prints them verbatim -- so a new one is a new value here and nowhere
// else.
const (
	// MatchSchema is api_version itself: a schema bump invalidates every
	// generator rather than one member, so it reaches every guest.
	MatchSchema = "schema"
	// MatchMember is a member the guest calls, by "Class::name".
	MatchMember = "member"
	// MatchEvent is an event the guest subscribes to.
	MatchEvent = "event"
	// MatchConcept is a named type reachable from a signature the guest uses.
	MatchConcept = "concept"
	// MatchClass is a class-level change, which takes every member on that
	// class with it.
	MatchClass = "class"
)

// The three verdicts, which are what a calling harness branches on. They are a
// partition: exactly one holds for any result.
const (
	// VerdictClean is a complete scan that found nothing. The only verdict that
	// says an upgrade is safe for this guest.
	VerdictClean = "clean"
	// VerdictImpacted is at least one breaking change touching this guest.
	VerdictImpacted = "impacted"
	// VerdictUnproven is a scan that could not see everything the guest reaches
	// and found nothing in what it could see. Not a pass: a member id that was
	// not a compile-time constant is a member this cannot cross-reference.
	VerdictUnproven = "unproven"
)

// CheckFinding is one breaking change that touches the guest.
//
// It is not `Change` with a field added, because these two render into
// different documents for different readers: `Change` is what `api diff --json`
// writes, where `kind` is the three-way classification as a number, and here
// every finding is breaking by construction and `kind` is a word.
type CheckFinding struct {
	// What identifies the thing that moved: "LuaEntity::destroy", "on_tick",
	// "MapPosition".
	What string `json:"what"`
	// Kind is the change's classification, always "breaking" here: nothing
	// additive or cosmetic is cross-referenced.
	Kind string `json:"kind"`
	// Match is why it reaches this guest: one of the Match* values above.
	Match string `json:"match"`
	// Detail says what happened, in a sentence a mod author can act on.
	Detail string `json:"detail"`
}

// CheckResult is what an upgrade would do to one guest.
type CheckResult struct {
	From, To string
	// Guest is the module that was checked, as the caller named it. Carried for
	// the JSON verdict, where a harness checking many guests in a loop has
	// nothing else to tell the documents apart by.
	Guest string
	// Hits are the breaking changes that touch this guest.
	Hits []CheckFinding
	// Ignored counts breaking changes that do not.
	Ignored int
	// Surface is the manifest that was checked, for the report's header.
	Surface GuestSurface
}

// CheckGuest cross-references a guest's surface against a diff.
func CheckGuest(s GuestSurface, d APIDiff) CheckResult {
	res := CheckResult{From: d.From, To: d.To, Surface: s}
	mine := map[string]string{}
	for _, m := range s.Members {
		mine[m] = MatchMember
	}
	for _, e := range s.Events {
		mine[e] = MatchEvent
	}
	// Concepts last and non-destructively: a class name can be both a concept
	// the guest names in a signature and the class a member it calls lives on,
	// and the member reading is the more specific one.
	for _, c := range s.Concepts {
		if mine[c] == "" {
			mine[c] = MatchConcept
		}
	}
	hit := func(c Change, match string) {
		res.Hits = append(res.Hits, CheckFinding{
			What: c.What, Kind: strings.ToLower(c.Kind.String()),
			Match: match, Detail: c.Detail,
		})
	}
	for _, c := range d.Breaking() {
		// api_version is everyone's problem: a schema change invalidates every
		// generator rather than one member.
		if c.What == "api_version" {
			hit(c, MatchSchema)
			continue
		}
		if m := mine[c.What]; m != "" {
			hit(c, m)
			continue
		}
		// A member's identity is "Class::name"; a class-level change reports
		// just the class, and takes every member on it with it.
		if cls, _, ok := strings.Cut(c.What, "::"); ok && mine[cls] != "" {
			hit(c, MatchClass)
			continue
		}
		found := false
		for _, m := range s.Members {
			if cls, _, _ := strings.Cut(m, "::"); cls == c.What {
				hit(c, MatchClass)
				found = true
				break
			}
		}
		if !found {
			res.Ignored++
		}
	}
	return res
}

// Verdict is the one word a caller branches on. See the Verdict* constants.
//
// Findings win over an incomplete scan: both exit non-zero, and "something you
// call breaks" is the more actionable of the two to lead with. `complete` in
// the JSON document is what says the impacted list may not be the whole of it.
func (r CheckResult) Verdict() string {
	switch {
	case len(r.Hits) > 0:
		return VerdictImpacted
	case !r.Surface.Complete:
		return VerdictUnproven
	}
	return VerdictClean
}

// ExitCode is the process status the verdict implies, and it is DERIVED from
// the verdict rather than decided beside it: two functions answering one
// question is how a table and the code behind it come apart.
//
// 2 is not here because it is not a verdict: it means the check could not be
// run at all, which is the caller's to report.
func (r CheckResult) ExitCode() int {
	if r.Verdict() == VerdictClean {
		return 0
	}
	return 1
}

// CheckVerdict is the machine-readable form of a CheckResult: what `api check
// --json` writes, and a data interface rather than a rendering.
//
// The human report above is presentation and is free to grow a sentence; this
// is not. Field names are lowercase_snake, every one is always present, and
// `findings` is an array even when it is empty -- a caller that has to
// distinguish null from [] is a caller parsing twice.
type CheckVerdict struct {
	// From and To are the RESOLVED versions, always. --from defaults to the
	// binary's own pin, so a harness that did not pass one has no other way to
	// learn which description its answer is about.
	From string `json:"from"`
	To   string `json:"to"`
	// Guest is the module path as it was given on the command line.
	Guest string `json:"guest"`
	// Verdict is one of the Verdict* constants.
	Verdict string `json:"verdict"`
	// Complete is false when a member or event id was not a compile-time
	// constant, so the scan could not see everything the guest reaches.
	Complete bool `json:"complete"`
	// ExitCode is what the process exited with, restated so a caller that
	// captured only stdout still has it.
	ExitCode int `json:"exit_code"`
	// Surface counts what was cross-referenced.
	Surface CheckVerdictSurface `json:"surface"`
	// BreakingTotal is every breaking change between the two versions;
	// Ignored is how many of them touch nothing this guest uses.
	BreakingTotal int `json:"breaking_total"`
	Ignored       int `json:"ignored"`
	// Findings are the breaking changes that DO touch this guest.
	Findings []CheckFinding `json:"findings"`
}

// CheckVerdictSurface is how much of the API the guest was found to touch.
type CheckVerdictSurface struct {
	Members  int `json:"members"`
	Events   int `json:"events"`
	Concepts int `json:"concepts"`
}

// VerdictDoc builds the machine-readable document. `Verdict` above is the one
// word inside it; this is the whole of what a caller is handed.
func (r CheckResult) VerdictDoc() CheckVerdict {
	findings := r.Hits
	if findings == nil {
		findings = []CheckFinding{}
	}
	return CheckVerdict{
		From: r.From, To: r.To, Guest: r.Guest,
		Verdict:  r.Verdict(),
		Complete: r.Surface.Complete,
		ExitCode: r.ExitCode(),
		Surface: CheckVerdictSurface{
			Members:  len(r.Surface.Members),
			Events:   len(r.Surface.Events),
			Concepts: len(r.Surface.Concepts),
		},
		BreakingTotal: len(r.Hits) + r.Ignored,
		Ignored:       r.Ignored,
		Findings:      findings,
	}
}

// JSON is the verdict document, indented, with a trailing newline so it lands
// in a terminal and a file the same way.
func (r CheckResult) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r.VerdictDoc(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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
