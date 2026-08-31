package factorio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The census: every number a Factorio version bump moves, in one committed file.
//
// These used to be Go literals scattered across three test files, which made an
// API upgrade a source edit -- exactly the manual step automatic regeneration is
// supposed to remove. Measured against 2.1.12: the pipeline handled 482 more
// members with no code change at all, and SEVEN TESTS failed, every one of them
// a moved count rather than a logic error.
//
// So the counts live in `api/<version>/census.json` beside the description they
// describe. A version bump is then:
//
//	fklua gen-bindings          # rewrites the bindings AND the census
//	git diff                    # one data diff saying exactly what moved
//
// ONE COMMAND, EVERY DESCRIPTION. That command writes bindings at one pin and a
// census for every version the checkout owns, which is not symmetry for its own
// sake: the generators are ONE code path serving N descriptions, so a row they
// gain moves every committed version's numbers on the same day. Writing only the
// invoked pin's file left the rest behind with nothing saying so, and a mod
// project pinning one of those versions then failed `gen-bindings --check` on a
// file it may not write. See cmd/fklua's censusPass for the whole of it -- and
// note that it is also why there is no `fklua census` subcommand: a second
// command writing this file is exactly the split the paragraph above is about.
//
// The pins are kept rather than loosened to floors, because a shrinking API is
// news too: 2.1.12 REMOVED two operators, which an equality check catches and a
// ">= 9" would not.

// CensusData is the shape of api/<version>/census.json.
//
// Field order is the JSON key order, so a diff reads top-down from raw counts
// to what the generators made of them.
type CensusData struct {
	ApplicationVersion string `json:"application_version"`
	APIVersion         int    `json:"api_version"`

	Classes    int `json:"classes"`
	Members    int `json:"members"`
	Methods    int `json:"methods"`
	Attributes int `json:"attributes"`
	Events     int `json:"events"`
	Concepts   int `json:"concepts"`
	Defines    int `json:"defines"`
	// DefineValues is every named value under those groups except
	// defines.events, which has its own resolved table. This is the number the
	// generated defines table is pruned FROM.
	DefineValues    int `json:"define_values"`
	GlobalObjects   int `json:"global_objects"`
	GlobalFunctions int `json:"global_functions"`

	PositionalMethods int `json:"positional_methods"`
	TakesTableMethods int `json:"takes_table_methods"`
	VariantMethods    int `json:"variant_group_methods"`
	SubclassMethods   int `json:"subclass_restricted_methods"`
	// SubclassAttributes is the half that was never counted. See the loop that
	// fills it, and "Subclass restrictions" in agents/abi.md for why the answer
	// is to DOCUMENT rather than to suppress: NONE of the subclass tokens is a
	// class name -- an overlap of exactly zero with the class graph, re-checked
	// at the 2.1.14 pin -- so there is no forwarder a generator could correctly
	// refuse.
	SubclassAttributes int `json:"subclass_restricted_attributes"`
	// OptionalReadAttributes is how many READABLE attributes the description
	// declares `optional: true` -- LuaEntity.temperature among them, present on
	// a reactor and absent on a chest.
	//
	// It is a row because that population is the exact blast radius of the
	// defect a downstream port found (fklua-ports-samples, Q4): the generator honoured
	// `optional` on a method's return and dropped it on an attribute, so every
	// one of these was typed as always-present and an absent one came back as
	// ERR_NO_MEMBER -- indistinguishable from a member this Factorio version
	// does not have, which is the one distinction that status exists to make.
	// The size of that population moved by nearly half between the 2.0.77 and
	// 2.1.14 pins, and it was written into three comments as a literal each
	// time, so it belongs where the other description-derived counts live.
	OptionalReadAttributes int `json:"optional_readable_attributes"`
	Operators              int `json:"operators"`
	AttributeOperators     int `json:"attribute_shaped_operators"`
	// OperatorsBound is how many of Operators reached the member table. It sat
	// at ZERO for five milestones with nothing saying so: no generator read
	// Class.Operators, so they were not bound, not deferred and not
	// counted, and fklua-ports' resource-marker (RM1) found LuaChunkIterator
	// binding three members none of which was the iterator. The row exists so
	// that a shape the API HAS and the generators do not can never again be
	// invisible in the one file whose job is to count what there is.
	OperatorsBound int `json:"operators_bound"`
	// GlobalFunctionsBound is the same row for the three global functions
	// (localised_print, log, table_size), and it is the clearest thing this file
	// has ever done: it read 0 for as long as there have been binding
	// generators, deliberately, because they are on no class and fk.call takes a
	// handle -- and a 0 that is WRITTEN DOWN is a decision somebody can come
	// back to, which is what happened. It is 3 since MemberGlobalFunc, whose
	// branch in M.invoke runs before the handle is resolved and never reads it.
	//
	// The row stays for the reason OperatorsBound's does: a description that
	// grew a fourth global function would move this number, and a number that
	// moves is a line in the version diff rather than a shape nobody looked at.
	GlobalFunctionsBound int `json:"global_functions_bound"`
	// CustomTableHandles is the SECOND member emitted over an attribute whose
	// described type is a LuaCustomTable -- `force.TechnologiesRaw()` beside
	// `force.Technologies()`. See MemberGetHandle.
	//
	// It is a row because it was the other half of the census's off-by-one, in
	// the direction nobody was looking: `gen-bindings`' accounting line
	// reconciles methods, both halves of every attribute and the class operators
	// against the member kinds they became, and kind 7 was in none of those
	// four buckets -- so the printed decomposition summed to 4784 against a
	// host_members_bound of 4842 and said nothing about the 58 missing. A member
	// kind that reaches no line of the accounting is exactly the F-IDX shape
	// (`operators_bound` above), met from inside the generator instead of
	// outside it.
	CustomTableHandles int `json:"custom_table_handle_members"`
	// CustomTableHandleMethods is the same twin over a METHOD that RETURNS a
	// LuaCustomTable -- `GetEntityFilteredRaw()` beside `GetEntityFiltered()`.
	// See MemberCallHandle.
	//
	// A SECOND ROW RATHER THAN A WIDENING OF THE ONE ABOVE, because the two
	// answer different questions about the same gap: kind 7 counts the
	// attributes that carry a LuaCustomTable and kind 10 the methods that return
	// one, and they were closed a round apart. Folding them would have made the
	// day the method half landed look like an attribute count that moved, which
	// is the reading a version diff would take.
	//
	// It is 11 at every committed pin -- the ten filtered prototype getters and
	// LuaSettings::get_player_settings -- and it read a written-down zero
	// nowhere at all before this round, which is the failure shape this file
	// exists to prevent.
	CustomTableHandleMethods int `json:"custom_table_handle_methods"`
	// IndexSetters is the WRITE half of an index operator -- `obj[k] = v`, a
	// second member over an operator `operators_bound` already counts. See
	// MemberIndexSet and indexWriteHalf.
	//
	// It is a row for the reason the one above is, plus one of its own: this is
	// the only member count here that comes from an ALLOWLIST rather than from
	// the description, because an operator carries no write_type for a generator
	// to read. A pin that adds an index operator moves it by nothing on its own;
	// a row flipped from false to true moves it by one, which is exactly the
	// event a version diff should make a reader look at.
	IndexSetters int `json:"index_setter_members"`
	// TypedArgMembers is how many members carry a SECOND, typed argument list
	// beside the tier-2 one -- a method whose parameter table is a discriminated
	// union, which is what Member.TypedArgs is for.
	//
	// A ROW BECAUSE THE POPULATION IS A MEASUREMENT AND NOT A CONSTANT: the
	// generator's own comment said "the four of these" and 2.1.17 has five
	// (LuaSimulation::get_widget_position joined them), which is exactly the
	// thing a prose count cannot notice. Its failure mode is silent in the other
	// direction too -- a shared parameter list that stops mapping drops the
	// typed form for that member and leaves the tier-2 one working, so nothing
	// breaks and nothing says so.
	TypedArgMembers int `json:"typed_arg_members"`
	// TypedVariantBindings is how many <Name>Typed / <name>_typed bindings each
	// backend emitted over those members.
	//
	// ONE ROW FOR BOTH LANGUAGES, like dyn_value_structs: the two walk one
	// Report and read one Member.TypedArgs, so a disagreement would be a defect
	// rather than a language difference, and the Rust count is compared against
	// this by a test instead of being written down twice.
	TypedVariantBindings int `json:"typed_variant_bindings"`
	// BulkReadMembers is how many members are BULK-ELIGIBLE: a readable
	// attribute returning exactly one fixed-width value, which is what
	// BulkEligible decides for both backends at once.
	//
	// A ROW FOR THE SAME REASON typed_arg_members is one, and for one more: the
	// eligible set is derived from HostAllocatesFor, which FAILS CLOSED, so a
	// wire kind added at a later pin is silently INELIGIBLE until somebody says
	// otherwise. A number that moves is how anybody finds out; a 0 nobody writes
	// down is how eleven class operators stayed invisible for five milestones.
	BulkReadMembers int `json:"bulk_read_members"`
	// BulkVariantBindings is how many <Class><Name>Bulk / <class>_<name>_bulk
	// bindings each backend emitted.
	//
	// EQUAL TO BulkReadMembers TODAY AND A ROW OF ITS OWN ANYWAY, which is the
	// same discipline typed_variant_bindings keeps: they answer different
	// questions and a single number could not say which one moved. Eligibility
	// is a property of the DESCRIPTION and emission is a property of the
	// GENERATORS, so a name collision or a refusal on one side would separate
	// them -- and a reader comparing two equal numbers learns that nothing was
	// lost between the two, which is the thing worth knowing.
	//
	// ONE ROW FOR BOTH LANGUAGES, like typed_variant_bindings: both walk one
	// Report and ask one BulkEligible, so a disagreement is a defect rather than
	// a language difference, and the Rust count is compared against this by a
	// test instead of being written down twice.
	BulkVariantBindings int `json:"bulk_variant_bindings"`

	TableConcepts int `json:"table_shaped_concepts"`
	StringEnums   int `json:"pure_string_enum_concepts"`

	// What the generators made of it.
	HostMembers int            `json:"host_members_bound"`
	HostSkipped int            `json:"host_members_skipped"`
	HostSkipsBy map[string]int `json:"host_skips_by_reason"`
	// FieldsOmitted is fields left OUT of a struct whose member still binds --
	// counted apart from the skips because it is a different event. A skip
	// costs the guest a member; an omission costs it a value the description
	// says never arrives (`UtilityConstants::frozen_color_lookup` is typed
	// `ColorLookupTable`, which 2.1 declares as `nil` and describes as "Does
	// not return the value at runtime.").
	//
	// The row exists because the alternative is invisibility. Omitting at field
	// level is right -- answering it at CONCEPT level is AD4, which took
	// CollisionMask, MapGenSettings and 17 members down -- but a field-level
	// omission leaves no trace anywhere else: the member binds, the struct
	// generates, every gate is green, and a description that grew a hundred of
	// these would look exactly like one that grew none. Counted here, it moves
	// in the version diff.
	FieldsOmitted   int            `json:"fields_omitted"`
	FieldsOmittedBy map[string]int `json:"fields_omitted_by_reason"`
	HostEvents      int            `json:"host_events_bound"`
	EventScratch    int            `json:"event_scratch_bytes"`
	// HookPayloadFields is how many fields of ConfigurationChangedData reached
	// the packaged layout, and 0 when the concept could not be expressed or the
	// description does not carry it.
	//
	// A ROW OF ITS OWN because it is in none of the others. It is not a member
	// and not an event, so host_members_bound and host_events_bound are both
	// blind to it -- and its whole failure mode is invisible: the hook still
	// fires, with no argument, exactly as it did for two milestones while
	// nothing anywhere said the payload was being discarded. A 0 nobody writes
	// down is how eleven class operators stayed invisible for five milestones.
	HookPayloadFields int `json:"hook_payload_fields"`
	GoMembers         int `json:"go_members_bound"`
	GoDeferred        int `json:"go_members_deferred"`
	// GoDeferralsBy is the half of GoDeferred that says what to build next.
	// Rolled up by shape rather than listed per struct, because the tail is
	// dozens of one-member structs that are each blocked by one of the three
	// shapes above them -- listing them would bury the three numbers that
	// actually rank the work.
	GoDeferralsBy map[string]int `json:"go_deferrals_by_reason"`
	// GoLiteralsDeferred is the string-enum CONSTANTS that got no name, counted
	// apart from the members so that the identity below closes:
	//
	//	host_members_bound == <lang>_members_bound + <lang>_members_deferred
	//
	// It did not close, by exactly one, in both languages, from the day the
	// string-enum constants landed: one literal in the description
	// (`LinkedGameControl`'s empty string) has no identifier name, and the loop
	// that found it called the MEMBER deferral counter. A constant is not a
	// member, and the arithmetic a reader uses to check that nothing fell
	// between the host walk and the guest walk was silently false while it
	// looked deliberate. Enforced by TestTheCensusMemberArithmeticCloses.
	GoLiteralsDeferred int            `json:"go_literals_deferred"`
	GoLiteralDeferBy   map[string]int `json:"go_literal_deferrals_by_reason"`
	// GoEventStructs is the event payloads that got a generated Go type, so a
	// guest reads a named field instead of a hand-derived byte offset. Counted
	// apart from the members: an event is not a member, and one number moving
	// for two unrelated causes is a diff nobody can read.
	// GoInherited is members a subclass reaches by forwarding to its parent.
	GoInherited    int `json:"go_members_inherited"`
	GoEventStructs int `json:"go_event_payload_structs"`
	// GoDefines is the generated defines.* accessors. Not a member count: a
	// define is tier 3 and has no signature to express or defer.
	GoDefines       int            `json:"go_define_accessors"`
	GoEventDeferred int            `json:"go_event_payloads_deferred"`
	GoEventDeferBy  map[string]int `json:"go_event_deferrals_by_reason"`

	// AND THE SAME SIX FOR RUST, which this file did not carry at all until the
	// ports round -- so the only place the Rust backend's coverage was written
	// down was a line `gen-bindings` printed and nobody diffed. Four separate
	// ports filed "Rust reports 47 deferrals against Go's 27" as a FINDING,
	// which is exactly the kind of number this file exists to make
	// unremarkable. The six mirror the Go six, because the whole claim of a
	// second backend is that it is a second RENDERING of one analysis: a row
	// where the two disagree is either a real language difference or a bug, and
	// having them side by side in one committed diff is what says which.
	RustMembers  int            `json:"rust_members_bound"`
	RustDeferred int            `json:"rust_members_deferred"`
	RustDeferBy  map[string]int `json:"rust_deferrals_by_reason"`
	// RustLiteralsDeferred mirrors GoLiteralsDeferred, and had the identical
	// off-by-one for the identical reason.
	RustLiteralsDeferred int            `json:"rust_literals_deferred"`
	RustLiteralDeferBy   map[string]int `json:"rust_literal_deferrals_by_reason"`
	RustInherited        int            `json:"rust_members_inherited"`
	RustEvents           int            `json:"rust_event_payload_structs"`
	RustDefines          int            `json:"rust_define_accessors"`

	// DynValueStructs is the generated structs that are a BOX AROUND ONE TIER-2
	// VALUE and therefore carry typed Bool/Num/Str/Obj readers -- ModSetting's
	// shape, matched by IsDynValueStruct rather than by a list of names.
	//
	// ONE ROW FOR BOTH LANGUAGES, unlike the six above, and that is a claim
	// rather than an economy: the predicate is one function both generators
	// ask, so a disagreement would be a defect and not a language difference.
	// TestBothBackendsEmitTheSameDynValueReaders is what says the two really
	// matched the same set, because AD5 is what happens when a shared analysis
	// is assumed rather than compared.
	DynValueStructs int `json:"dyn_value_structs"`

	MaxReturnArty int `json:"max_method_return_arity"`
}

// TakeCensus counts everything, from the API and from what the generators did
// with it.
func TakeCensus(a *API) (CensusData, error) {
	c := CensusData{
		ApplicationVersion: a.ApplicationVersion,
		APIVersion:         a.APIVersion,
		Classes:            len(a.Classes),
		Members:            a.Members(),
		Events:             len(a.Events),
		Concepts:           len(a.Concepts),
		Defines:            len(a.Defines),
		DefineValues:       len(GenerateDefines(a).Defines),
		GlobalObjects:      len(a.GlobalObjects),
		GlobalFunctions:    len(a.GlobalFunctions),
		HostSkipsBy:        map[string]int{},
		FieldsOmittedBy:    map[string]int{},
	}

	for _, cl := range a.Classes {
		c.Methods += len(cl.Methods)
		c.Attributes += len(cl.Attributes)
		c.Operators += len(cl.Operators)
		// SUBCLASS RESTRICTIONS ON ATTRIBUTES, which were counted NOWHERE.
		// `subclasses` is declared on both api.Method and api.Attribute and only
		// the method loop below asked, so the census read the methods alone and
		// missed most of the total -- a 78% undercount at the 2.0.77 pin, of the
		// one number this file exists to keep honest. Attributes are the larger
		// half several times over, LuaGuiElement being the biggest single
		// declarer. The live figures are subclass_restricted_methods and
		// subclass_restricted_attributes in census.json, which is the whole
		// point of the row: do not restate them here.
		for _, at := range cl.Attributes {
			if len(at.Subclasses) > 0 {
				c.SubclassAttributes++
			}
			// READABLE, because optionality is a statement about the value that
			// comes BACK. A write-only attribute has no return for `opt=` to
			// describe, so counting it would inflate the row past the members
			// the row is about.
			if at.ReadType != nil && at.Optional {
				c.OptionalReadAttributes++
			}
		}
		for _, o := range cl.Operators {
			if o.IsAttribute() {
				c.AttributeOperators++
			}
		}
		for _, m := range cl.Methods {
			if m.TakesTable() {
				c.TakesTableMethods++
			} else {
				c.PositionalMethods++
			}
			if len(m.VariantGroups) > 0 {
				c.VariantMethods++
			}
			if len(m.Subclasses) > 0 {
				c.SubclassMethods++
			}
			if n := len(m.ReturnValues); n > c.MaxReturnArty {
				c.MaxReturnArty = n
			}
		}
	}

	for _, cp := range a.Concepts {
		switch cp.Type.Complex {
		case "table":
			c.TableConcepts++
		case "union":
			allLiteral := len(cp.Type.Options) > 0
			for _, o := range cp.Type.Options {
				if o.Complex != "literal" {
					allLiteral = false
					break
				}
			}
			if allLiteral {
				c.StringEnums++
			}
		}
	}

	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	c.HostMembers, c.HostSkipped = len(r.Members), len(r.Skipped)
	for _, m := range r.Members {
		if m.IsOperator() {
			c.OperatorsBound++
		}
		if m.Kind == MemberGetHandle {
			c.CustomTableHandles++
		}
		if m.Kind == MemberCallHandle {
			c.CustomTableHandleMethods++
		}
		if m.Kind == MemberIndexSet {
			c.IndexSetters++
		}
		if len(m.TypedArgs) > 0 {
			c.TypedArgMembers++
		}
		if m.Kind == MemberGlobalFunc {
			c.GlobalFunctionsBound++
		}
	}
	for k, v := range r.Reasons {
		c.HostSkipsBy[k] = v
	}
	// Field omissions from BOTH the member walk and the event walk. They are one
	// row rather than two because it is one property of the description; the
	// dedup key carries the owner, and the two walks have disjoint owners (a
	// concept and an "event <name>"), so no field is counted twice.
	for _, o := range append(append([]OmittedField(nil), r.Omitted...), ev.Omitted...) {
		c.FieldsOmitted++
		c.FieldsOmittedBy[o.Reason]++
	}
	c.HostEvents, c.EventScratch = len(ev.Events), ev.MaxSize
	c.HookPayloadFields = len(ev.ConfChanged)

	g, err := GenerateGoWith(a, r, ev, "fkapi")
	if err != nil {
		return c, fmt.Errorf("generating Go for the census: %w", err)
	}
	c.GoMembers, c.GoDeferred = g.Emitted, g.Deferred
	c.GoInherited = g.Inherited
	c.GoEventStructs, c.GoEventDeferred = g.EventStructs, g.EventsDeferred
	c.GoDefines = g.Defines
	c.GoEventDeferBy = g.EventDeferBy
	c.GoLiteralsDeferred, c.GoLiteralDeferBy = g.LiteralsDeferred, g.LiteralDeferBy
	if c.GoLiteralDeferBy == nil {
		c.GoLiteralDeferBy = map[string]int{}
	}
	c.GoDeferralsBy = map[string]int{}
	for k, v := range g.DeferredBy {
		// The per-struct tail rolls into one bucket. Each of those is blocked
		// BY one of the three shapes, so counting them separately would say
		// "twenty-three problems" where there are three.
		if strings.HasPrefix(k, "struct ") {
			c.GoDeferralsBy["a struct blocked by one of the above"] += v
			continue
		}
		c.GoDeferralsBy[k] = v
	}

	rb, err := GenerateRust(a, r, ev)
	if err != nil {
		return c, fmt.Errorf("generating Rust for the census: %w", err)
	}
	c.RustMembers, c.RustDeferred = rb.Emitted, rb.Deferred
	c.RustInherited, c.RustEvents, c.RustDefines = rb.Inherited, rb.EventStructs, rb.Defines
	// ONE row, taken from the Go side. The Rust count is compared against it by
	// a test rather than written down twice: two numbers spelling one fact is
	// this repo's most-repeated failure shape.
	c.DynValueStructs = g.DynValueStructs
	c.TypedVariantBindings = g.TypedVariants
	c.BulkVariantBindings = g.BulkVariants
	for _, m := range r.Members {
		if _, _, ok := BulkEligible(m); ok {
			c.BulkReadMembers++
		}
	}
	c.RustLiteralsDeferred, c.RustLiteralDeferBy = rb.LiteralsDeferred, rb.LiteralDeferBy
	if c.RustLiteralDeferBy == nil {
		c.RustLiteralDeferBy = map[string]int{}
	}
	c.RustDeferBy = map[string]int{}
	for k, v := range rb.DeferredBy {
		if strings.HasPrefix(k, "struct ") {
			c.RustDeferBy["a struct blocked by one of the above"] += v
			continue
		}
		c.RustDeferBy[k] = v
	}
	return c, nil
}

// JSON renders the census the way it is committed: indented, newline-terminated,
// so a diff is line-oriented and a reviewer can see one number move.
func (c CensusData) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CensusPath is where a version's census lives, beside its description.
func CensusPath(apiDir, version string) string {
	return filepath.Join(apiDir, version, "census.json")
}

// LoadCensus reads a committed census.
func LoadCensus(path string) (CensusData, error) {
	var c CensusData
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Diff lists what moved, in a form meant to be read rather than parsed.
//
// It is the whole point of the file: a version bump should produce a short list
// of "this went from X to Y", not a wall of failing assertions in three test
// files.
func (c CensusData) Diff(old CensusData) []string {
	var out []string
	cmp := func(what string, was, now int) {
		if was != now {
			out = append(out, fmt.Sprintf("%-32s %d -> %d", what, was, now))
		}
	}
	if old.ApplicationVersion != c.ApplicationVersion {
		out = append(out, fmt.Sprintf("%-32s %s -> %s", "application_version",
			old.ApplicationVersion, c.ApplicationVersion))
	}
	cmp("api_version (schema)", old.APIVersion, c.APIVersion)
	cmp("classes", old.Classes, c.Classes)
	cmp("members", old.Members, c.Members)
	cmp("methods", old.Methods, c.Methods)
	cmp("attributes", old.Attributes, c.Attributes)
	cmp("subclass-restricted attributes", old.SubclassAttributes, c.SubclassAttributes)
	cmp("optional readable attributes", old.OptionalReadAttributes, c.OptionalReadAttributes)
	cmp("events", old.Events, c.Events)
	cmp("concepts", old.Concepts, c.Concepts)
	cmp("defines", old.Defines, c.Defines)
	cmp("define values", old.DefineValues, c.DefineValues)
	cmp("global objects", old.GlobalObjects, c.GlobalObjects)
	cmp("global functions", old.GlobalFunctions, c.GlobalFunctions)
	cmp("positional methods", old.PositionalMethods, c.PositionalMethods)
	cmp("takes_table methods", old.TakesTableMethods, c.TakesTableMethods)
	cmp("variant-group methods", old.VariantMethods, c.VariantMethods)
	cmp("subclass-restricted methods", old.SubclassMethods, c.SubclassMethods)
	cmp("operators", old.Operators, c.Operators)
	cmp("attribute-shaped operators", old.AttributeOperators, c.AttributeOperators)
	cmp("operators bound", old.OperatorsBound, c.OperatorsBound)
	cmp("global functions bound", old.GlobalFunctionsBound, c.GlobalFunctionsBound)
	cmp("LuaCustomTable handle members", old.CustomTableHandles, c.CustomTableHandles)
	cmp("LuaCustomTable handle methods", old.CustomTableHandleMethods,
		c.CustomTableHandleMethods)
	cmp("index-assign members", old.IndexSetters, c.IndexSetters)
	cmp("typed-arg members", old.TypedArgMembers, c.TypedArgMembers)
	cmp("typed-arg bindings", old.TypedVariantBindings, c.TypedVariantBindings)
	cmp("table-shaped concepts", old.TableConcepts, c.TableConcepts)
	cmp("pure string-enum concepts", old.StringEnums, c.StringEnums)
	cmp("host members bound", old.HostMembers, c.HostMembers)
	cmp("host members skipped", old.HostSkipped, c.HostSkipped)
	cmp("struct fields omitted", old.FieldsOmitted, c.FieldsOmitted)
	cmp("host events bound", old.HostEvents, c.HostEvents)
	cmp("event scratch bytes", old.EventScratch, c.EventScratch)
	cmp("hook payload fields", old.HookPayloadFields, c.HookPayloadFields)
	cmp("Go members bound", old.GoMembers, c.GoMembers)
	cmp("Go members deferred", old.GoDeferred, c.GoDeferred)
	cmp("Go literals deferred", old.GoLiteralsDeferred, c.GoLiteralsDeferred)
	cmp("Go members inherited", old.GoInherited, c.GoInherited)
	cmp("Go event payload structs", old.GoEventStructs, c.GoEventStructs)
	cmp("Go define accessors", old.GoDefines, c.GoDefines)
	cmp("Go event payloads deferred", old.GoEventDeferred, c.GoEventDeferred)
	cmp("Rust members bound", old.RustMembers, c.RustMembers)
	cmp("Rust members deferred", old.RustDeferred, c.RustDeferred)
	cmp("Rust literals deferred", old.RustLiteralsDeferred, c.RustLiteralsDeferred)
	cmp("Rust members inherited", old.RustInherited, c.RustInherited)
	cmp("Rust event payload structs", old.RustEvents, c.RustEvents)
	cmp("Rust define accessors", old.RustDefines, c.RustDefines)
	cmp("dyn-value structs", old.DynValueStructs, c.DynValueStructs)
	cmp("max method return arity", old.MaxReturnArty, c.MaxReturnArty)

	// Reason maps are the actionable half: a NEW reason means a generator met a
	// shape it had not met before, which is worth a look even when the totals
	// barely moved.
	out = append(out, diffReasons("skip", old.HostSkipsBy, c.HostSkipsBy)...)
	out = append(out, diffReasons("field omission", old.FieldsOmittedBy, c.FieldsOmittedBy)...)
	out = append(out, diffReasons("go", old.GoDeferralsBy, c.GoDeferralsBy)...)
	out = append(out, diffReasons("go event", old.GoEventDeferBy, c.GoEventDeferBy)...)
	out = append(out, diffReasons("go literal", old.GoLiteralDeferBy, c.GoLiteralDeferBy)...)
	out = append(out, diffReasons("rust", old.RustDeferBy, c.RustDeferBy)...)
	out = append(out, diffReasons("rust literal", old.RustLiteralDeferBy, c.RustLiteralDeferBy)...)
	return out
}

// diffReasons reports movement in one reason map, calling out reasons that
// appear or vanish rather than folding them into a count that moved.
func diffReasons(label string, old, now map[string]int) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range old {
		keys = append(keys, k)
		seen[k] = true
	}
	for k := range now {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []string
	for _, k := range keys {
		was, is := old[k], now[k]
		if was == is {
			continue
		}
		what := label + ": " + k
		switch {
		case was == 0:
			out = append(out, fmt.Sprintf("%-32s NEW, %d members", what, is))
		case is == 0:
			out = append(out, fmt.Sprintf("%-32s gone (was %d)", what, was))
		default:
			out = append(out, fmt.Sprintf("%-32s %d -> %d", what, was, is))
		}
	}
	return out
}
