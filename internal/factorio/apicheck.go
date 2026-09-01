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
//
// ALL THREE PRUNING SCANS FEED IT, and the third is here because it was not:
// `UsedDefines` existed for the pruner and this file never read it, so a guest
// whose `defines.*` value a release renamed or removed got a CLEAN verdict --
// the one answer the feature exists to make impossible.

// GuestSurface is everything about the API one compiled guest touches.
type GuestSurface struct {
	// Members are "Class::name" for every member the guest calls.
	Members []string
	// Events are the event names it subscribes to.
	Events []string
	// Defines are the dotted paths of the `defines.*` VALUES it reads, carrying
	// the "defines." prefix so they are the same strings the diff reports.
	//
	// Values rather than groups, because a value is what a guest asks for: a
	// `fk.define` id is a dense index over the flattened value paths, so a
	// group surviving says nothing about whether the constant a guest baked an
	// id for is still there.
	Defines []string
	// Concepts are the named types reachable from those members' signatures AND
	// from those events' payloads -- the argument, return and payload shapes it
	// therefore depends on.
	Concepts []string
	// Complete is false when a member, event or define id was not a
	// compile-time constant, so the scan could not see everything.
	//
	// When this is false the check CANNOT be trusted to be exhaustive, and
	// says so rather than reporting a clean bill.
	Complete bool
}

// SurfaceOf builds the manifest for a guest, given the reports its ids index.
//
// EVERY REPORT HERE MUST COME FROM THE `from` DESCRIPTION, which is the pin the
// guest was compiled against. Member, event and define ids are all dense
// indices assigned per version, so resolving them against anything else names
// different things -- silently, since every id still resolves to something.
func SurfaceOf(r Report, usedMembers map[int]bool, membersComplete bool,
	usedEvents map[int]bool, eventsComplete bool, evs EventReport,
	usedDefines map[int]bool, definesComplete bool, defs DefineReport) GuestSurface {

	s := GuestSurface{
		Complete: membersComplete && eventsComplete && definesComplete,
	}
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
		if !usedEvents[e.ID] {
			continue
		}
		s.Events = append(s.Events, e.Name)
		// A PAYLOAD'S NAMED TYPES COUNT FOR THE SAME REASON A SIGNATURE'S DO,
		// and this loop collected the name alone while the member loop above
		// collected both. An event's scratch block is laid out from its fields,
		// so a concept one of them names gaining a field moves every offset
		// after it in a reader the guest was compiled against -- the event
		// itself having changed in no way the diff reports against its name.
		//
		// It is not redundant with the member loop: at the 2.0.77 pin FIVE
		// concepts are reachable ONLY through an event payload
		// (`OldTileAndPosition` across the six tile events,
		// `PathfinderWaypoint`, `PostSegmentDiedData`, `SelectedPrototypeData`
		// and `LuaLazyLoadedValue`), so no member a guest could call ever puts
		// one of them on the surface.
		for _, f := range e.Fields {
			collectTypeNames(f, concepts)
		}
	}
	for _, d := range defs.Defines {
		if usedDefines[d.ID] {
			s.Defines = append(s.Defines, "defines."+d.Path)
		}
	}
	for c := range concepts {
		s.Concepts = append(s.Concepts, c)
	}
	sort.Strings(s.Members)
	sort.Strings(s.Events)
	sort.Strings(s.Defines)
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
// heard of; it reaches the guest because a member it DOES call takes one, or
// because an event it subscribes to carries one in its payload.
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
	// MatchDefine is a `defines.*` value the guest reads, by its dotted path.
	MatchDefine = "define"
	// MatchConcept is a named type reachable from a signature the guest uses or
	// from the payload of an event it subscribes to.
	MatchConcept = "concept"
	// MatchClass is a class-level change, which takes every member on that
	// class with it.
	MatchClass = "class"
)

// WHERE THE `from` VERSION CAME FROM, and it is a data interface for the same
// reason Match is: `api check --json` prints it verbatim, so a new value is a
// new constant here and nowhere else.
//
// It is carried because `--from` stopped being one thing. The flag defaults to
// whatever the resolver found -- an explicit flag, the guest's OWN pin stamp,
// the project manifest, this binary's default -- and a caller that passed no
// flag would otherwise have no way to learn which of the four it got. That is
// not a cosmetic difference: member, event and define ids are dense per-version
// indices, so the answer to "what does this guest touch" is a DIFFERENT answer
// under each one, and every id still resolves to something under all of them.
const (
	// FromSourceFlag is an explicit --from on the command line.
	FromSourceFlag = "flag"
	// FromSourceStamp is the guest's own pin stamp: the FACT about which
	// description its ids were assigned over, and the strongest of the four.
	FromSourceStamp = "stamp"
	// FromSourceManifest is `[fklua] api` in an fklua.toml beside the caller:
	// the project's stated INTENT, for a guest that carries no stamp.
	FromSourceManifest = "manifest"
	// FromSourceDefault is this binary's DefaultAPIVersion, the last resort --
	// and it has moved under this repo twice.
	FromSourceDefault = "default"
)

// FromSources is that closed set as a value, in resolution order.
//
// So that "what may `from_source` say" is answerable without restating four
// constants a fifth time, and so that the two things which have to cover all of
// them -- FromSourcePhrase and VerdictDoc's refusal -- are checkable against one
// list rather than against each other.
var FromSources = []string{
	FromSourceFlag, FromSourceStamp, FromSourceManifest, FromSourceDefault,
}

// ValidFromSource reports whether src is one of them.
//
// THE EMPTY STRING IS NOT ONE. "Nobody recorded which rule answered" is not a
// rule, and it is the value a CheckResult built by hand carries by default --
// which is why VerdictDoc refuses it rather than shipping `"from_source": ""`
// into a document whose whole contract is that the field names one of four.
func ValidFromSource(src string) bool {
	for _, s := range FromSources {
		if s == src {
			return true
		}
	}
	return false
}

// FromSourcePhrase is the same fact in the human report's words.
//
// ONE FUNCTION rather than a phrase spelled beside each constant, because the
// report and the document must not be able to disagree about which of the four
// answered -- this repo's most-repeated failure shape.
func FromSourcePhrase(src string) string {
	switch src {
	case FromSourceFlag:
		return "the --from flag"
	case FromSourceStamp:
		return "the guest's own pin stamp"
	case FromSourceManifest:
		return "fklua.toml, [fklua] api"
	case FromSourceDefault:
		return "fklua's default pin"
	}
	// A FIFTH VALUE MUST NOT PRINT AS A RAW TOKEN. Returning `src` unchanged
	// renders `(from: env)` in the report, which reads exactly like a phrase
	// somebody wrote -- so a constant added to the set above and forgotten here
	// would ship silently into the one output a mod author actually reads. This
	// says what it is instead, and VerdictDoc refuses the same value outright.
	return fmt.Sprintf("an unrecognised resolution rule %q", src)
}

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
	// FromSource is which of the four resolution rules chose From: one of the
	// FromSource* constants. Set by whoever resolved it, since the rules live
	// where the command line, the module and the manifest are.
	FromSource string
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
	// Defines cannot collide with anything above: every one of these carries the
	// "defines." prefix and no class, member or event name does.
	for _, d := range s.Defines {
		mine[d] = MatchDefine
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
	// From and To are the RESOLVED versions, always. A harness that passed no
	// --from has no other way to learn which description its answer is about.
	From string `json:"from"`
	To   string `json:"to"`
	// FromSource is WHICH RULE chose From: one of the FromSource* constants.
	// `from` alone cannot answer it, because the four rules routinely resolve
	// to the same string -- and a caller that omitted --from gets a different
	// question answered depending on which one fired.
	FromSource string `json:"from_source"`
	// Guest is the module path as it was given on the command line.
	Guest string `json:"guest"`
	// Verdict is one of the Verdict* constants.
	Verdict string `json:"verdict"`
	// Complete is false when a member, event or define id was not a
	// compile-time constant, so the scan could not see everything the guest
	// reaches.
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
	Defines  int `json:"defines"`
	Concepts int `json:"concepts"`
}

// VerdictDoc builds the machine-readable document. `Verdict` above is the one
// word inside it; this is the whole of what a caller is handed.
//
// IT REFUSES A FromSource OUTSIDE THE CLOSED SET RATHER THAN DEFAULTING ONE, and
// the empty string is outside it. `from_source` is a value a script switches on,
// so `"from_source": ""` is a document that broke its own contract while looking
// exactly like one that kept it -- and it is what a CheckResult assembled by a
// library caller carries until somebody sets the field, which is a whole class
// of caller the command-line path cannot speak for. Defaulting it would be
// worse than the empty string: it would NAME a rule that did not fire, which is
// this field's one job to prevent.
func (r CheckResult) VerdictDoc() (CheckVerdict, error) {
	if !ValidFromSource(r.FromSource) {
		return CheckVerdict{}, fmt.Errorf(
			"CheckResult.FromSource is %q, which is not one of %s: `from_source` "+
				"names WHICH RULE resolved `from`, and there is no honest default "+
				"for a rule that did not fire",
			r.FromSource, strings.Join(FromSources, ", "))
	}
	findings := r.Hits
	if findings == nil {
		findings = []CheckFinding{}
	}
	return CheckVerdict{
		From: r.From, To: r.To, FromSource: r.FromSource, Guest: r.Guest,
		Verdict:  r.Verdict(),
		Complete: r.Surface.Complete,
		ExitCode: r.ExitCode(),
		Surface: CheckVerdictSurface{
			Members:  len(r.Surface.Members),
			Events:   len(r.Surface.Events),
			Defines:  len(r.Surface.Defines),
			Concepts: len(r.Surface.Concepts),
		},
		BreakingTotal: len(r.Hits) + r.Ignored,
		Ignored:       r.Ignored,
		Findings:      findings,
	}, nil
}

// JSON is the verdict document, indented, with a trailing newline so it lands
// in a terminal and a file the same way.
func (r CheckResult) JSON() ([]byte, error) {
	doc, err := r.VerdictDoc()
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Report renders the answer a mod author reads.
//
// AN UNRESOLVED FromSource PRINTS LOUDLY HERE rather than printing the header
// this command had before the field existed. VerdictDoc refuses that value
// outright, and the two halves of one closed set may not disagree about it: a
// report that quietly dropped the phrase would read as an OLDER fklua's output,
// which is the one reading a mod author cannot check. The report is presentation
// and stays a string -- it has no error to return -- so what it does with a
// value outside the set is say so, in FromSourcePhrase's own words.
func (r CheckResult) Report() string {
	var b strings.Builder
	// THE HEADER NAMES WHERE `from` CAME FROM, for the reason the document
	// carries `from_source`: the four rules resolve to the same string often
	// enough that the version alone does not say which question was asked.
	// Appended rather than spliced between the versions so the "A -> B" shape a
	// reader scans for is where it has always been.
	fmt.Fprintf(&b, "# api check: %s -> %s (from: %s)\n\n", r.From, r.To,
		FromSourcePhrase(r.FromSource))
	fmt.Fprintf(&b, "This guest touches %d member(s), %d event(s), %d define(s) and %d named type(s).\n\n",
		len(r.Surface.Members), len(r.Surface.Events), len(r.Surface.Defines),
		len(r.Surface.Concepts))

	if !r.Surface.Complete {
		b.WriteString("**This check is NOT exhaustive.** A member, event or define id was not\n")
		b.WriteString("a compile-time constant, so the scan could not see everything the guest\n")
		b.WriteString("reaches. Treat a clean result as unproven rather than as a pass.\n\n")
	}

	if len(r.Hits) == 0 {
		fmt.Fprintf(&b, "**Nothing this guest uses is affected.** %d breaking change(s) in\n", r.Ignored)
		b.WriteString("the release touch nothing on the surface above.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "**%d breaking change(s) affect this guest**, out of %d in the release.\n\n",
		len(r.Hits), len(r.Hits)+r.Ignored)
	for _, c := range r.Hits {
		fmt.Fprintf(&b, "- `%s` — %s\n", c.What, c.Detail)
	}
	return b.String()
}
