package factorio

import (
	"fmt"
	"go/format"
	"sort"
	"strings"
)

// Generating the guest-side Go bindings.
//
// This is what turns `fk.call(2, 1729, argp, retp)` into `game.Speed()`. The
// member table already says what each id means on the HOST side; this says the
// same thing on the guest side, in a form a Go author can call.
//
// THE PACKAGE IS COMPLETE AND THAT IS DELIBERATE. Emitting every member makes
// the file large, but TinyGo dead-code-eliminates what a guest never calls, and
// the surviving `fk.call` sites are exactly the ones the member id scan then
// finds -- so a guest that uses five members compiles to five call sites and
// ships a five-entry table. Generating a subset up front would mean guessing
// what the author will write.
//
// Scalars, handles, strings, optionals, structs, arrays and dictionaries at any
// nesting depth, and tier-2 dynamic values -- effectively everything the host
// carries, plus the Into variants over members already counted. THE COUNTS ARE
// NOT WRITTEN HERE: they live in api/<version>/census.json, which a version bump
// regenerates and a reviewer diffs. This comment said "4160 of the 4187" for
// four pins after that stopped being true, which is the drift the census exists
// to make impossible and a prose copy re-introduces.
//
// A member that cannot be bound is reported rather than emitted, for the same
// reason the host generator skips what it cannot express -- a binding that
// exists and does the wrong thing is worse than one that is absent.

// GoBindings is a generated Go package plus what it could not cover.
type GoBindings struct {
	Source string
	// Inherited counts the members a subclass got by FORWARDING to its parent.
	// LuaEntity's Position() is LuaControl's, and an inherited member appears in
	// neither the child's method list nor its attribute list.
	Inherited int
	// EventStructs counts the event payloads that got a generated Go type, so
	// a guest never has to derive a field offset by hand; EventsDeferred counts
	// the ones the Go layer cannot express yet, by reason. Kept APART from the
	// member counts below: an event is not a member, and folding them made one
	// number move for two unrelated causes.
	EventStructs   int
	EventsDeferred int
	EventDeferBy   map[string]int
	// Defines counts the generated defines.* accessors. Their VALUES are never
	// generated -- runtime-api.json does not carry them -- so what is emitted
	// is a per-build id and a resolver call.
	Defines int
	// Emitted and Deferred count members bound and members left to struct
	// support. Deferred is not a failure: the host side handles those shapes
	// already, and the Go side has to grow struct types to match.
	Emitted, Deferred int
	// DeferredBy counts deferrals by reason.
	//
	// A bare total says how much is missing but not what to build next, which
	// is the only question the number is ever asked. The host generator learned
	// this first -- its Reasons map is what the census diffs to notice a shape
	// the generator had never met -- and the Go side was counting blind.
	DeferredBy map[string]int
	// LiteralsDeferred counts the string-literal CONSTANTS that got no name, and
	// it is separate from Deferred because a constant is not a member.
	//
	// It was not separate, and that is the whole of the census's off-by-one:
	// `host_members_bound` read 4842 while `go_members_bound + go_members_deferred`
	// read 4843, because the string-enum loop below called the same defer1 the
	// member loop does. One literal in the 2.1.14 description has no identifier
	// name -- `LinkedGameControl`'s empty string -- so the discrepancy was
	// exactly one, in both languages, for as long as the constants have existed.
	//
	// A member deferral costs a guest a CALL; a nameless literal costs it one
	// spelling of a string it can still write out. Folding the two made the one
	// identity that reconciles the three census rows -- host bound = bound +
	// deferred -- silently false, which is the arithmetic a reader uses to
	// check that nothing fell between the generators.
	LiteralsDeferred int
	LiteralDeferBy   map[string]int
	// Names maps "Class::member/kind" to the Go method it became, for tests and
	// for documentation generation.
	Names map[string]string
	// IntoVariants counts the `<Name>Into(dst, ...)` bindings emitted for
	// members returning a container.
	//
	// SEPARATE FROM Emitted on purpose. Emitted counts MEMBERS bound, and this
	// is a second binding over a member already counted -- folding it in would
	// raise the coverage figure without covering anything, which is exactly the
	// mistake the EventStructs split above was made to avoid.
	IntoVariants int
	// TypedVariants counts the <Name>Typed bindings: a second binding over a
	// member whose parameter table is a discriminated union, taking its shared
	// parameters as a tier-1 struct and the variant tail as one tier-2 slot.
	// Separate from Emitted for IntoVariants' reason -- the member is already
	// counted, and folding this in would raise coverage without covering
	// anything.
	TypedVariants int
	// BulkVariants counts the <Class><Name>Bulk bindings: one attribute read off
	// N handles in ONE crossing, over the ORDINARY getter's member id. Separate
	// from Emitted for IntoVariants' reason, and a row of its own in the census
	// rather than folded into TypedVariants -- two second-bindings summed
	// together is a number that cannot say which one moved, which is the lesson
	// custom_table_handle_methods was split out for.
	//
	// It counts the INHERITED re-renderings too, because those are real
	// generated functions a guest calls: an inherited bulk read is emitted on
	// the inheriting class rather than forwarded, since a forwarder cannot
	// retype its own []Child parameter to the []Parent the parent declares.
	BulkVariants int
	// DynValueStructs counts the generated structs whose whole content is ONE
	// tier-2 value and which therefore got typed Bool/Num/Str/Obj readers --
	// ModSetting's shape. See IsDynValueStruct: it is a rule over the layout,
	// so the number is what the rule matched rather than a list somebody kept.
	DynValueStructs int
	// Collisions names every member whose bound name another member of the class
	// had already taken and which memberRename has NO ROW for, and StaleRenames
	// names every row that no longer describes the collision it was written for.
	//
	// IDENTITIES RATHER THAN COUNTS, and that is the whole point of the pair. A
	// collision was a number in a census diff and a member nobody could call;
	// with a name in hand a gate can fail saying WHICH member and telling the
	// maintainer to decide, which is TestEveryIndexOperatorHasAWriteVerdict's
	// shape over a different derivation. Both are empty at every committed
	// description and both are gate failures when they are not.
	Collisions   []string
	StaleRenames []string
}

// defer1 records one deferral under a reason.
func (g *GoBindings) defer1(why string) {
	g.Deferred++
	if why == "" {
		why = "unstated"
	}
	g.DeferredBy[why]++
}

// deferLiteral records one string-enum CONSTANT that could not be emitted. Not
// a member, and therefore not in Deferred -- see LiteralsDeferred.
func (g *GoBindings) deferLiteral(why string) {
	g.LiteralsDeferred++
	if g.LiteralDeferBy == nil {
		g.LiteralDeferBy = map[string]int{}
	}
	g.LiteralDeferBy[why]++
}

// deferEvent records one event payload the Go layer cannot express.
func (g *GoBindings) deferEvent(why string) {
	g.EventsDeferred++
	if g.EventDeferBy == nil {
		g.EventDeferBy = map[string]int{}
	}
	g.EventDeferBy[why]++
}

// goScalarReason names why a kind has no Go type yet, for the deferral census.
// It is the shape the Go layer would have to grow, phrased as work to do.
func goScalarReason(k Kind) string {
	switch k {
	case KindArray:
		return "returns or takes an array"
	case KindDict:
		return "returns or takes a dictionary"
	case KindDyn:
		return "returns or takes a dynamic (tier 2) value"
	}
	return "unhandled kind " + k.String()
}

// goScalar reports the Go type for a kind that needs no generated struct.
func goScalar(k Kind) (string, bool) {
	switch k {
	case KindBool:
		return "bool", true
	case KindI8:
		return "int8", true
	case KindU8:
		return "uint8", true
	case KindI16:
		return "int16", true
	case KindU16:
		return "uint16", true
	case KindI32:
		return "int32", true
	case KindU32:
		return "uint32", true
	case KindF32:
		return "float32", true
	case KindF64:
		return "float64", true
	case KindU64:
		return "uint64", true
	case KindString:
		return "string", true
	case KindHandle:
		return "Object", true
	case KindDyn:
		// Tier 2. One Go type for every union, LocalisedString and
		// anything else the API leaves open -- the same bet the Lua codec
		// makes, and for the same reason: generating 93 tagged union types
		// is where a generator drowns.
		return "Value", true
	}
	return "", false
}

// exportName turns an API name into a Go identifier: snake_case to CamelCase.
//
// ANY non-alphanumeric is a separator, not just underscore. Fifteen table
// fields are hyphenated -- "active-provider", "show-pipelines" -- and passing a
// hyphen through produces a subtraction where a field name should be, which
// fails to parse a long way from the name that caused it.
func exportName(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
		up = false
	}
	out := b.String()
	if out == "" {
		return "X"
	}
	// A leading digit is legal in the API and not in Go.
	if out[0] >= '0' && out[0] <= '9' {
		return "X" + out
	}
	return out
}

// goParamName keeps a parameter's name but makes it a legal, non-colliding Go
// identifier. Go keywords are the trap: the API has parameters called `type`
// and `range`.
// goKeywords is what a parameter name may not be: Go keywords, and the
// predeclared identifiers below, which are not keywords and shadow just as hard.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,

	// AND THE PREDECLARED IDENTIFIERS, which are not keywords and shadow just
	// as hard. `LuaHelpers::decode_string` takes a parameter called `string`,
	// and a body that then wants to declare a `*string` does not compile --
	// which is exactly what the multi-return work made it want to do, in a
	// function that had happened never to name a type before. Six parameters
	// across the API: five `string` and one `append`. A trailing underscore, the
	// same answer the keywords already get.
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true, "any": true, "comparable": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

func goParamName(s string, i int) string {
	out := strings.Map(func(r rune) rune {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = fmt.Sprintf("p%d", i)
	}
	if goKeywords[out] {
		out += "_"
	}
	return out
}

// GenerateGo emits the guest-side package for a member table.
//
// The API is needed as well as the report, for the nine global objects: their
// handles are fixed by the ABI but their TYPES come from the description, and
// without them a guest has no way to obtain a handle at all -- every other one
// is reached by calling something.
func GenerateGo(a *API, r Report, pkg string) (GoBindings, error) {
	return GenerateGoWith(a, r, GenerateEvents(a), pkg)
}

// GenerateGoWith is GenerateGo with an explicit event table, so a caller that
// already built one does not build it twice.
func GenerateGoWith(a *API, r Report, evs EventReport, pkg string) (GoBindings, error) {
	out := GoBindings{Names: map[string]string{}, DeferredBy: map[string]int{},
		EventDeferBy: map[string]int{}}
	structs := newGoStructs()
	var b strings.Builder
	// NO BACKTICK MAY REACH THIS BUFFER, and that constraint belongs here rather
	// than only in the test that checks it.
	//
	// Everything written through w lands in a .go file that this package's own
	// tests carry as raw string literals, and a backtick closes one. The hazard
	// is not prose somebody types here -- it is prose that arrives from DATA:
	// member names, type names and descriptions all come out of
	// runtime-api.json, and Wube can put a backtick in one at any release.
	// Nothing in the Go toolchain would notice, because the generated package
	// compiles on its own.
	//
	// So a doc comment built from a description must not quote it with
	// backticks, and any Markdown in one is passed through as it arrived rather
	// than re-fenced. TestNoBacktickReachesTheGeneratedSources is what says so
	// if that ever stops being true -- of the Go source AND of the Lua member
	// table, which has the same exposure for the same reason.
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	w("// Code generated by fklua from runtime-api.json. DO NOT EDIT.\n//\n")
	w("// Build with the flags in guest/go/fk.BuildFlags; nothing here works\n")
	w("// outside GOARCH=wasm, because //go:wasmimport is rejected elsewhere.\n")
	w("package %s\n\n", pkg)
	w("import \"unsafe\"\n\n")
	w("%s\n", goRuntime)

	// THE API PIN STAMP. internal/factorio/pin.go carries what it is for and
	// why it is an export name; what gets emitted beside it is the shorter
	// thing a reader of the generated file needs.
	w("\n// The API pin stamp: these bindings' ids were assigned over Factorio\n")
	w("// %s's description, and fklua mod reads this export name to prove the\n",
		a.ApplicationVersion)
	w("// member table it packages was generated over that same description. Ids\n")
	w("// are dense sorted indices per version, so a table from any other one\n")
	w("// answers this guest's calls with different members.\n")
	w("//\n")
	w("// EXPORTED RATHER THAN CALLED, because an export is a root: nothing here\n")
	w("// references it, and -opt=2 followed by wasm-opt deletes whatever is only\n")
	w("// defined. The NAME carries the version because a wasm result cannot.\n")
	w("//go:wasmexport %s\n", PinExport(a.ApplicationVersion))
	w("func fkAPIPin() {}\n")

	// ...AND THE ABI SIGNATURE, which is the half the pin cannot reach. The pin
	// proves the guest and the table came from one DESCRIPTION; this proves they
	// came from one GENERATION, and at one pin the ids move whenever the
	// generator grows. See internal/factorio/pin.go.
	w("\n// The ABI signature: a digest of the ID ASSIGNMENT AND LAYOUT these\n")
	w("// bindings were generated with, so fklua mod can say when a wasm built\n")
	w("// against OLDER bindings is being packaged with a fresh member table at\n")
	w("// the same pin -- every id in which resolves to a different member.\n")
	w("//\n")
	w("// Language-independent: a Rust guest generated from this description\n")
	w("// carries the same name.\n")
	w("//go:wasmexport %s\n", SigExport(APISignature(a)))
	w("func fkAPISig() {}\n")

	// Group by class so the file reads like the API does.
	//
	// bound and declared are what the inheritance pass below needs: the
	// signature of every member each class actually bound, and the set of names
	// each class declared itself.
	bound := map[string]map[string]goSig{}
	// bulkSeen guards the PACKAGE-LEVEL bulk function names, which live in the
	// same namespace as every generated type rather than inside a class's method
	// set.
	bulkSeen := map[string]bool{}
	declared := map[string]map[string]bool{}
	byClass := map[string][]Member{}
	var classes []string
	for _, m := range r.Members {
		if _, ok := byClass[m.Class]; !ok {
			classes = append(classes, m.Class)
		}
		byClass[m.Class] = append(byClass[m.Class], m)
	}
	sort.Strings(classes)

	for _, cls := range classes {
		// GLOBAL FUNCTIONS ARE ON NO CLASS, so there is no type to declare and
		// the bindings are package-level. This branch replaces a `continue`
		// whose comment read "not bound yet" and was true for as long as this
		// generator has existed; see MemberGlobalFunc.
		//
		// The typeName that reaches goMember is the EMPTY STRING rather than
		// exportName(""), which is "X": the empty string is what goMemberVariant
		// asks about, and an "X" would name three struct types after nothing.
		global := cls == ""
		typeName := ""
		if global {
			w("\n// Factorio's three GLOBAL FUNCTIONS, which belong to no class and are\n")
			w("// package-level here for that reason. fk.call's handle operand is\n")
			w("// unread for them and the bindings pass 0.\n\n")
		} else {
			typeName = exportName(cls)
			w("\n// %s wraps a handle to a %s.\ntype %s struct{ Object }\n\n",
				typeName, cls, typeName)
		}

		// A class can declare a method and an attribute with names that collide
		// once camel-cased. Emitting both would not compile, so the second is
		// deferred and counted rather than silently dropped.
		seen := map[string]bool{}
		for _, m := range byClass[cls] {
			// A MEMBER NO GUEST CAN CALL USEFULLY IS DEFERRED RATHER THAN BOUND.
			// See Member.Unfillable: the host binds all five, the marshalling is
			// right, and there is nothing a guest can put in the argument -- so
			// this turns a green function whose every call is a silent no-op into
			// a compile error naming the reason.
			if m.Unfillable != "" {
				out.defer1(m.Unfillable)
				continue
			}
			src, name, sig, why, ok := goMember(structs, typeName, m)
			if !ok {
				out.defer1(why)
				continue
			}
			if seen[name] || (global && structs.taken(name)) {
				// A class can declare a method and an attribute whose names
				// collide once camel-cased; emitting both would not compile.
				//
				// A GLOBAL FUNCTION IS PACKAGE-LEVEL, so its neighbours are not
				// a class's members but every generated TYPE -- hence the second
				// clause, which is the same question the string-enum constant
				// loop already asks. No pinned description collides (`Log`,
				// `LocalisedPrint`, `TableSize` name no concept), and a deferral
				// with a reason beats a package that does not compile.
				//
				// AND THE IDENTITY IS RECORDED, because a collision is a decision
				// somebody has to take and a count cannot say whose. memberRename
				// is where the two standing ones are taken; an unlisted one still
				// defers safely here and fails a gate by name.
				out.defer1("Go" + NameCollision)
				out.Collisions = append(out.Collisions,
					fmt.Sprintf("%s (would be %q)", MemberKey(m), name))
				continue
			}
			seen[name] = true
			out.Names[MemberKey(m)] = name
			w("%s", src)
			out.Emitted++
			if bound[cls] == nil {
				bound[cls] = map[string]goSig{}
			}
			bound[cls][name] = sig

			// AND THE DESTINATION-SLICE VARIANT, for a member that returns a
			// container. Same member id, same blocks, same host: only what the
			// guest does with the returned (ptr, count) differs. See the header
			// on goMemberVariant.
			//
			// Not counted in Emitted and not entered in Names, because both are
			// per-MEMBER and this is a second binding over the same member --
			// counting it would inflate the coverage figure with functions the
			// host does not know exist. It is counted separately.
			//
			// Not asked for a global function: none of the three returns a
			// container, so the variant would report "no" anyway, and asking
			// keeps a shape out of the emitted file that has no caller.
			if global {
				continue
			}
			isrc, iname, isig, iok := goMemberInto(structs, typeName, m)
			if iok && !seen[iname] {
				seen[iname] = true
				w("%s", isrc)
				out.IntoVariants++
				bound[cls][iname] = isig
			}

			// AND THE TYPED-ARGUMENT VARIANT, for a member whose parameter table
			// is a discriminated union. Same member id, same returns, and an
			// argument block that is a tier-1 struct plus one tier-2 slot rather
			// than one tier-2 map. Counted in its own row for the reason the
			// Into variant is: both are a second BINDING over a member Emitted
			// has already counted once.
			tsrc, tname, tsig, tok := goMemberTyped(structs, typeName, m)
			if tok && !seen[tname] {
				seen[tname] = true
				w("%s", tsrc)
				out.TypedVariants++
				bound[cls][tname] = tsig
			}

			// AND THE BULK VARIANT, for a readable attribute of a fixed-width
			// shape: one crossing for N handles over the same member id.
			//
			// NOT entered in bound[], because it is not a method: it is a
			// package-level function over a []Object, so there is nothing for
			// the inheritance loop to forward and nothing an inheriting class
			// needs -- a []Object is a []Object whichever class declared the
			// attribute. One function per eligible member, named after the class
			// that DECLARES it, which is where `fklua docs` lists it too.
			bsrc, bname, bok := goMemberBulk(structs, typeName, m)
			if bok && !bulkSeen[bname] && !structs.taken(bname) {
				bulkSeen[bname] = true
				w("%s", bsrc)
				out.BulkVariants++
			}
		}
		out.StaleRenames = append(out.StaleRenames,
			staleRenames(cls, byClass[cls], seen, func(r memberRenameRow) (string, string) {
				return r.WasGo, r.Go
			})...)
		declared[cls] = seen
	}

	// INHERITED MEMBERS, FORWARDED.
	//
	// 83 of the 156 classes have a parent, and an inherited member appears in
	// NEITHER the child's method list nor its attribute list -- so LuaEntity had
	// no Position() and no SurfaceIndex(), which are LuaControl's. Dispatch
	// never cared (it is name-based and the handle decides the object), so the
	// workaround was legal and undiscoverable: LuaControl{Object: h}.Position().
	//
	// A FORWARDER RATHER THAN AN EMBEDDED FIELD, and that is the whole design
	// decision. Embedding the parent would promote its methods in one generator
	// line, and it would also break every `fkapi.LuaEntity{Object: h}` in
	// existence -- a composite literal cannot name a promoted field, so the
	// idiom would become LuaEntity{LuaControl: LuaControl{Object: h}}. The
	// one-line forwarders (go_members_inherited in census.json -- do not
	// re-derive the count here, it moves with the pin and this line said 978
	// two pins after it stopped being true) cost ~30% more generated source,
	// which TinyGo's dead-code elimination removes again, and cost nobody a
	// rewrite.
	//
	// NEAREST ANCESTOR WINS, and a name the class declares itself always wins:
	// an override is a real thing in this API and a forwarder must never shadow
	// the member it is meant to complete.
	parentOf := map[string]string{}
	for _, c := range a.Classes {
		if c.Parent != "" {
			parentOf[c.Name] = c.Parent
		}
	}
	for _, cls := range classes {
		if cls == "" {
			continue
		}
		taken := declared[cls]
		var fwd []string
		for p := parentOf[cls]; p != ""; p = parentOf[p] {
			names := make([]string, 0, len(bound[p]))
			for n := range bound[p] {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if taken[n] {
					continue
				}
				taken[n] = true
				sig := bound[p][n]
				fwd = append(fwd, fmt.Sprintf(
					"func (o %s) %s(%s) %s {\n\treturn %s{o.Object}.%s(%s)\n}\n\n",
					exportName(cls), n, strings.Join(sig.Params, ", "), sig.RetType,
					exportName(p), n, strings.Join(sig.Args, ", ")))
				out.Inherited++
			}
		}
		if len(fwd) > 0 {
			w("\n// %s inherits these. The handle decides the object, so dispatch is\n",
				exportName(cls))
			w("// identical -- only the name had nowhere to live.\n")
			for _, f := range fwd {
				w("%s", f)
			}
		}
	}

	// EVENT PAYLOAD STRUCTS, registered before the declarations are emitted so
	// they come out with everything else.
	//
	// Until these existed a guest read event fields by casting the pointer
	// fk_on_event was handed and adding HAND-DERIVED BYTE OFFSETS -- FkLua's own
	// examples/api did it, with the offsets in a comment. The offsets move
	// whenever the API pin moves (one new optional field shifts everything after
	// it) and being wrong is SILENT: the guest reads a neighbouring handle and
	// quietly does nothing. The layout was already computed here and already
	// emitted for the Lua side; only the Go half was missing.
	//
	// An event's field list is a struct's field list, so this is the same
	// machinery, which is also why an event whose payload the Go layer cannot
	// express is deferred with a reason rather than emitted wrong.
	var eventStructs []string
	for _, e := range evs.Events {
		name := exportName(e.Name)
		if structs.taken(name) {
			out.deferEvent("the payload's name collides with a concept type")
			continue
		}
		if _, why, ok := structs.add(FieldSpec{Kind: KindStruct, TypeName: e.Name,
			Struct: e.Fields}, name); !ok {
			out.deferEvent(why)
			continue
		}
		eventStructs = append(eventStructs, name)
	}

	// ...AND THE ONE HOOK PAYLOAD, which is a struct by the same machinery and
	// is not an event.
	//
	// script.on_configuration_changed hands its handler a ConfigurationChangedData
	// and nothing in the API references the concept, so no generator had ever
	// emitted it and the hook dispatched with no argument at all. Registered
	// beside the event payloads because everything downstream is identical: the
	// host encodes it with H.write_struct into the same per-level buffer and the
	// guest decodes it with a generated reader.
	confChanged := ""
	if evs.ConfChanged != nil {
		n := exportName(ConfChangedConcept)
		if !structs.taken(n) {
			if _, _, ok := structs.add(FieldSpec{Kind: KindStruct,
				TypeName: ConfChangedConcept, Struct: evs.ConfChanged}, n); ok {
				confChanged = n
			}
		}
	}

	// Struct declarations last. Go does not care about order at package level,
	// and collecting them while the methods were generated is what makes the
	// set exactly what the methods reach.
	structs.emit(w)
	out.DynValueStructs = structs.dynValue

	// One reader per event. The pointer is into the event scratch buffer, which
	// is the host's and lives for exactly this dispatch, so the decoder copies
	// every string and slice out of it -- what comes back is the guest's.
	if len(eventStructs) > 0 {
		w("\n// Event payload readers. fk_on_event is handed an id and a pointer;\n")
		w("// switch on the id and call the matching reader.\n")
		for _, name := range eventStructs {
			w("\nfunc Read%s(p uint32) %s {\n", name, name)
			w("\treturn decode%s((*byte)(unsafe.Pointer(uintptr(p))))\n}\n", name)
		}
	}
	if confChanged != "" {
		w("\n// Read%s decodes what script.on_configuration_changed handed the\n", confChanged)
		w("// hook. fk_on_configuration_changed is called with a pointer into the\n")
		w("// host's event buffer, which lives for exactly this dispatch, so the\n")
		w("// decoder copies every string and slice out of it.\n")
		w("//\n")
		w("// A guest that exports fk_on_configuration_changed WITHOUT a parameter is\n")
		w("// unchanged: an extra argument to a wasm function of no parameters is\n")
		w("// discarded by the generated Lua, so the no-argument form still works and\n")
		w("// still means what it meant.\n")
		w("//\n")
		w("// ModChanges is the one most guests want -- one entry per mod ADDED,\n")
		w("// REMOVED or moved version, keyed by mod name, with OldVersion nil for an\n")
		w("// addition and NewVersion nil for a removal.\n")
		w("func Read%s(p uint32) %s {\n", confChanged, confChanged)
		w("\treturn decode%s((*byte)(unsafe.Pointer(uintptr(p))))\n}\n", confChanged)
	}
	out.EventStructs = len(eventStructs)

	// Event ids, as constants. The example used to hand-write them and drifted
	// the moment tier 2 added six events and renumbered the list -- subscribing
	// to whatever now sat at the old number. Ids are per-build by design, so
	// they have to be GENERATED beside the table that defines them.
	if len(evs.Events) > 0 {
		w("\n// Event ids. Per-build, like member ids: regenerated with the table\n")
		w("// they index, so never write one by hand.\nconst (\n")
		for _, e := range evs.Events {
			w("\tEvent%s = %d\n", exportName(e.Name), e.ID)
		}
		w(")\n")
	}

	// FIELD MASK BITS, one per maskable field, for SubscribeMasked.
	//
	// The bit is the field's index in the LAID-OUT order, which is the order
	// the host's own table is in -- so these are generated beside the layout
	// for the same reason the event ids are generated beside the table they
	// index. A hand-written `1 << 1` drifts the moment the API pin adds a
	// field, silently and in the direction of masking the wrong one.
	//
	// ONLY OPTIONAL AND CONTAINER FIELDS GET ONE, and the omission is the API.
	// A mandatory scalar has no reading that means "not sent", so masking one
	// would hand the guest a zero it cannot tell from a real value. The host
	// refuses such a bit at subscribe time too -- a guest can compute a mask,
	// and this is not a promise the type system can keep -- but not offering
	// the constant is what makes the rule discoverable instead of a runtime
	// surprise.
	var maskLines []string
	for _, e := range evs.Events {
		blk, err := LayoutStruct(e.Fields)
		if err != nil {
			continue
		}
		for i, p := range blk.Fields {
			if p.HasOffset < 0 && p.Kind != KindArray && p.Kind != KindDict {
				continue
			}
			maskLines = append(maskLines, fmt.Sprintf("\tSkip%s%s = 1 << %d\n",
				exportName(e.Name), exportName(p.Name), i))
		}
	}
	if len(maskLines) > 0 {
		w("\n// Field mask bits for SubscribeMasked, one per field a guest may\n")
		w("// decline. A masked optional reads as ABSENT and a masked container\n")
		w("// as EMPTY, and the layout does not move. OR them together.\nconst (\n")
		for _, l := range maskLines {
			w("%s", l)
		}
		w(")\n")
	}

	// STRING-LITERAL UNIONS, as untyped constants. See StringLiteralUnions in
	// gen.go, and pure_string_enum_concepts in census.json for how many; the
	// reason they are constants rather than an enum is that the value has to
	// stay a string.
	//
	// UNTYPED, so WaitConditionTypeInactivity is assignable wherever a string is
	// and every existing call site that passes a literal keeps compiling. A
	// defined type would be better documentation and a breaking change to all of
	// them, for a union the API can extend in a point release.
	litTaken := map[string]bool{}
	var litLines []string
	for _, u := range StringLiteralUnions(a) {
		prefix := exportName(u.Name)
		var block []string
		for _, lit := range u.Literals {
			part, ok := LiteralIdent(lit)
			if !ok {
				out.deferLiteral("a string literal with no identifier name")
				continue
			}
			nm := prefix + part
			if litTaken[nm] || structs.taken(nm) {
				out.deferLiteral("a string literal whose constant name collides")
				continue
			}
			litTaken[nm] = true
			block = append(block, fmt.Sprintf("\t%s = %q\n", nm, lit))
		}
		if len(block) == 0 {
			continue
		}
		litLines = append(litLines, fmt.Sprintf(
			"\n// %s: %d string literals, spelled once, here.\nconst (\n%s)\n",
			prefix, len(u.Literals), strings.Join(block, "")))
	}
	if len(litLines) > 0 {
		w("\n// The API's string enums. A union of nothing but string literals\n")
		w("// crosses as its string, which is the right wire answer and leaves the\n")
		w("// 26 names of WaitConditionType nowhere -- so a typo is an engine\n")
		w("// rejection at runtime, or silence, rather than a compile error.\n")
		w("// Reported by fklua-ports' fuel-train-stop as FTS2.\n")
		for _, l := range litLines {
			w("%s", l)
		}
	}

	// DEFINES, as accessors rather than constants -- because there IS no
	// constant to generate.
	//
	// runtime-api.json carries a define's NAME and an order and not its value,
	// so nothing here could bake one even if it wanted to; and it would not
	// want to, because a define's number is Factorio's own and is not stable
	// across versions. The generated table carries the dotted path,
	// control.lua resolves it at load, and the guest holds the per-build id --
	// the same shape defines.events has always used, generalised.
	//
	// THE VALUE IS CACHED ON FIRST USE, and the laziness is what makes the two
	// halves work together. Caching in a package-level initialiser would run
	// the host call whether or not the guest reads the define, so every mod
	// would name every id and the constant scan would prune nothing; caching
	// inside the accessor keeps the call site inside a function TinyGo deletes
	// when nobody calls it. One host call per define for the life of the mod.
	defs := GenerateDefines(a)
	seen := map[string]bool{}
	var defLines []string
	for _, d := range defs.Defines {
		name := "Defines"
		for _, part := range strings.Split(d.Path, ".") {
			name += exportName(part)
		}
		// Two paths can camel-case to one identifier ("a_b.c" and "a.b_c").
		// Emitting both would not compile, and picking one silently would give
		// a guest the wrong number under the right name.
		if seen[name] || structs.taken(name) {
			continue
		}
		seen[name] = true
		defLines = append(defLines, fmt.Sprintf(`
// %s is defines.%s, resolved BY NAME against the running Factorio at load.
// Its value is not stable across versions and is not in the API description,
// so there is no constant to write and nothing here bakes one.
func %s() uint32 {
	if !dok%d {
		d%d, dok%d = hostDefine(%d), true
	}
	return d%d
}
`, name, d.Path, name, d.ID, d.ID, d.ID, d.ID, d.ID))
		defLines = append(defLines, fmt.Sprintf("var (\n\td%d uint32\n\tdok%d bool\n)\n", d.ID, d.ID))
	}
	if len(defLines) > 0 {
		w("\n// defines.* accessors. See fk.define in agents/abi.md: the value is\n")
		w("// resolved by name at load and cached here on first use.\n")
		for _, l := range defLines {
			w("%s", l)
		}
	}
	out.Defines = len(seen)

	// The nine globals. Their handle numbers are the ABI's fixed 1..9 block, in
	// the order fk_abi.lua's GLOBAL_NAMES declares -- appending is safe there,
	// reordering is not, and this is the other half of that contract.
	w("\n// The objects a mod starts with. Their handles are fixed by the ABI.\n")
	w("//\n// Everything else is reached by calling something: these are the roots.\n")
	w("var (\n")
	byName := map[string]string{}
	for _, g := range a.GlobalObjects {
		byName[g.Name] = g.Type.Name
	}
	for i, name := range abiGlobalNames {
		typ, ok := byName[name]
		if !ok {
			continue
		}
		if _, generated := byClass[typ]; !generated {
			continue // its class had nothing bindable
		}
		w("\t%s = %s{ObjectAt(%d)}\n", exportName(name), exportName(typ), i+1)
	}
	w(")\n")

	// Run it through go/format rather than being careful by hand. The repo is
	// gofmt-clean and CI enforces it, so a generator that emits nearly-formatted
	// code would fail the lint step on output nobody wrote.
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		// The generated source did not parse, which is a generator bug rather
		// than a formatting one. Return it unformatted so the error message can
		// be read against real line numbers.
		return GoBindings{Source: b.String()}, fmt.Errorf("generated Go does not parse: %w", err)
	}
	out.Source = string(formatted)
	return out, nil
}

// abiGlobalNames mirrors M.GLOBAL_NAMES in runtime/lua/fk_abi.lua. Order IS the
// ABI: a guest compiled against it uses the numbers.
var abiGlobalNames = []string{
	"commands", "game", "helpers", "prototypes", "rcon",
	"remote", "rendering", "script", "settings",
}

// goMember renders one member, or reports that it needs struct support.
// goSig is everything a FORWARDER needs to redeclare a member on a subclass.
//
// Classes inherit -- 83 of the 156 have a parent -- and an inherited member
// appears in neither the child's method list nor its attribute list, so
// LuaEntity had no Position() and no SurfaceIndex(): they are LuaControl's. The
// legal workaround was LuaControl{Object: h}.Position(), which works and is
// undiscoverable.
type goSig struct {
	// Params are "name Type" pairs and Args are just the names, so a forwarder
	// declares the same signature and passes it straight on.
	Params, Args []string
	// RetType is "error" or "(T, error)".
	RetType string
}

// THE `Into` VARIANT — a caller-supplied destination for a container return.
//
// A member whose single return is an ARRAY builds `out := make([]T, n)` on every
// call, and under `-gc=leaking` every one of those is permanent. Downstream
// measured ~1.3 KB of permanent guest heap per network compile, most of it here:
// a `find_entities_filtered` on a hot path allocates a fresh slice per call and
// the guest reads it once. The elements themselves are already free -- the host
// writes them into the marshalling arena and the bracket reclaims it -- so the
// slice header's backing array is the whole of what is left.
//
// So the generator emits a SECOND binding for exactly those members:
//
//	ents, err := surface.FindEntitiesFiltered(filter)          // allocates
//	ents, err = surface.FindEntitiesFilteredInto(ents, filter) // reuses
//
// It is a second function rather than an option on the first because the first
// one's signature is what a mod author reaches for and should stay simple, and
// because a caller passing a destination is making a lifetime claim -- that
// nothing still holds the previous contents -- which a parameter makes visible
// at the call site.
//
// SCOPE IS "EVERY ARRAY RETURN", not just find_entities_filtered, and that costs
// nothing extra: the branch that builds the slice is one branch, so every member
// the same branch covers gets the variant for free. 240 members across 62
// classes, 110 of them arrays of objects.
//
// THE HOST SIDE IS UNTOUCHED. Both bindings pass the same member id and the same
// blocks; only what the guest does with the returned (ptr, count) differs. That
// is what makes this cheap -- no new member entries, no coverage change, and
// nothing to prune differently.
//
// WHAT IT IS WORTH UNDER `--gc=collected`, which is where downstream now is:
// less, and still positive. A collected guest reclaims the slice, so this stops
// being permanent heap and becomes garbage -- but garbage still has to be
// allocated, marked around and swept, and the pacer's step budget is spent on
// exactly that. Fewer allocations is fewer collections; it is no longer a leak.

func goMember(g *goStructs, typeName string, m Member) (src, name string, sig goSig, why string, ok bool) {
	return goMemberVariant(g, typeName, m, false, false)
}

// goMemberInto renders the destination-slice variant, or reports that this
// member has no container return to write into.
func goMemberInto(g *goStructs, typeName string, m Member) (src, name string, sig goSig, ok bool) {
	src, name, sig, _, ok = goMemberVariant(g, typeName, m, true, false)
	return
}

// goMemberTyped renders the TYPED-ARGUMENT variant, or reports that this member
// has no second argument list.
//
// SAME MEMBER ID, SAME RETURNS, DIFFERENT ARGUMENT BLOCK -- so it is the whole
// ordinary member body over a substituted Args, which is what makes it cheap:
// nothing about encoding, presence bytes, containers or return decoding is
// written twice. The one thing that has to differ inside is the import, and
// hostCallTyped is what says which block the host will read.
//
// The <Name>Into precedent one file over: a second BINDING over a member that is
// already counted, entered in bound[] so inheritance forwards it, and counted in
// a row of its own rather than in Emitted.
func goMemberTyped(g *goStructs, typeName string, m Member) (src, name string, sig goSig, ok bool) {
	if len(m.TypedArgs) == 0 {
		return "", "", goSig{}, false
	}
	m.Args = m.TypedArgs
	src, name, sig, _, ok = goMemberVariant(g, typeName, m, false, true)
	return
}

func goMemberVariant(g *goStructs, typeName string, m Member, into, typed bool) (src, name string, sig goSig, why string, ok bool) {
	args, rets, err := m.blocks()
	if err != nil {
		return "", "", goSig{}, "signature has no memory layout", false
	}

	type field struct {
		goType string
		off    int
		kind   Kind
		ident  string
		// has is the presence byte's offset, or -1 when mandatory.
		has int
		// elemType, elemKind, elemOff and stride describe one element when kind
		// is KindArray or KindDict. goType is then "[]elemType", or
		// "map[keyType]elemType" for a dictionary.
		elemType string
		elemKind Kind
		elemOff  int
		stride   int
		// keyType, keyKind and keyOff are the other half of a dictionary pair.
		keyType string
		keyKind Kind
		keyOff  int
		// entryType names the generated pair type when the dictionary is a
		// SLICE rather than a map, which is what a dyn key forces.
		entryType string
		// elemCtn is the codec of an element (or dictionary value) that is
		// ITSELF a container, empty otherwise. See gogen_nested.go.
		elemCtn string
	}
	// Argument and return specs, so a struct field keeps the concept name the
	// placed form drops.
	argSpec := namedSpecs(m.Args, "a")
	retSpec := namedSpecs(m.Rets, "r")

	// mk builds one field, resolving an array's element on the way.
	mk := func(f Placed, specs []FieldSpec, i int, fallback, ident string) (field, string, bool) {
		gt, why, okk := goFieldFor(g, f, specs, i, fallback)
		if !okk {
			return field{}, why, false
		}
		fl := field{goType: gt, off: f.Offset, kind: f.Kind, ident: ident, has: f.HasOffset}
		switch f.Kind {
		case KindArray:
			et, elem, ctn, why, okk := goArrayElem(g, f, specs, i, fallback)
			if !okk {
				return field{}, why, false
			}
			// elem.Offset rather than 0: a one-field pair block places it at 0
			// today, and reading the offset the layout actually assigned costs
			// nothing and does not assume that stays true.
			fl.elemType, fl.elemKind = et, elem.Kind
			fl.elemOff, fl.stride = elem.Offset, f.Stride
			fl.elemCtn = ctn
		case KindDict:
			kt, vt, key, val, vc, why, okk := goDictKV(g, f, specs, i, fallback)
			if !okk {
				return field{}, why, false
			}
			fl.keyType, fl.keyKind, fl.keyOff = kt, key.Kind, key.Offset
			fl.elemType, fl.elemKind, fl.elemOff = vt, val.Kind, val.Offset
			fl.stride, fl.elemCtn = f.Stride, vc
			// entryFor is idempotent, and goFieldFor has already asked for this
			// same pair above -- the two agree by construction because they ask
			// the same function the same question. Unconditional since Q3: every
			// dictionary is the pair slice, whatever its key.
			fl.entryType = g.entryFor(kt, vt)
		}
		return fl, "", true
	}

	var in, res []field
	// Optional arguments this layer cannot express, named in the doc comment so
	// a caller who wonders where a parameter went can read why.
	var omitted []string
	// POSITIONAL ARGUMENTS WHOSE DECLARED UNION COLLAPSED TO A HANDLE, paired
	// with the identifier the signature gives them. Collected here rather than
	// recovered from `in` afterwards, because an argument this layer cannot
	// express is skipped and the two lists stop lining up.
	type collapsedArg struct {
		ident string
		union CollapsedUnion
	}
	var collapsed []collapsedArg
	for i, f := range args.Fields {
		// An optional is a POINTER, and nil means absent rather than zero.
		// Factorio distinguishes the two throughout -- absent means leave it
		// alone, present-zero means set it to zero -- so a Go author needs to
		// be able to say which, and *T is how Go says it. A slice is the
		// exception: nil already says absent, and *[]T would make the caller
		// write &[]T{...} for no gain.
		fl, why, okk := mk(f, argSpec, i, typeName+name0(m)+exportName(f.Name), goParamName(f.Name, i))
		if !okk {
			// AN OPTIONAL ARGUMENT THE GO LAYER CANNOT EXPRESS IS OMITTED, NOT
			// FATAL -- and that is a different act from dropping a struct
			// FIELD, which this package refuses on the grounds that a struct
			// missing a field is a wrong value the guest cannot detect.
			//
			// Here nothing is wrong and nothing is hidden. An absent optional
			// is omitted rather than defaulted at every layer of this ABI, so
			// the call the host makes is exactly the call a Lua author writes
			// when they leave the argument out; the presence byte stays 0
			// because the block arrives zeroed; and the caller can SEE the
			// parameter is not there, at compile time, with the reason in the
			// doc comment above it.
			//
			// The alternative was the status quo: LuaGameScript::create_surface
			// deferred on its optional MapGenSettings, so a mod could not create
			// a surface at all -- and a whole genre (hidden-surface mods) sat
			// behind one optional argument nobody was passing anyway.
			if f.HasOffset >= 0 {
				omitted = append(omitted, f.Name+" ("+why+")")
				continue
			}
			return "", "", goSig{}, why, false
		}
		if i < len(argSpec) && argSpec[i].Collapsed != nil {
			collapsed = append(collapsed, collapsedArg{fl.ident, *argSpec[i].Collapsed})
		}
		in = append(in, fl)
	}
	for i, f := range rets.Fields {
		fl, why, okk := mk(f, retSpec, i, typeName+name0(m)+"Result", "")
		if !okk {
			return "", "", goSig{}, why, false
		}
		res = append(res, fl)
	}
	// A MEMBER RETURNING SEVERAL VALUES IS EMITTED NOW, and Go needed nothing
	// for it but the courage to write the signature: the language returns
	// several values natively, the host already sends them (M.invoke carries
	// four slots, against a measured maximum of three), and the layout has laid
	// out multi-field return blocks all along. What stood in the way was this
	// function's own shape -- a single `res[0]` threaded through five branches
	// -- which is what the deferral's own comment meant by "naming rules, not
	// marshalling". Thirteen members at this pin, and one of them,
	// LuaBootstrap::register_on_object_destroyed, is the ONLY way to arm
	// on_object_destroyed at all, so a Go guest could not subscribe to that
	// event in any useful sense. Reported by fklua-ports' nixie-tubes (G1),
	// which hand-wrote the binding into the generated package to ship.
	//
	// Two of the thirteen -- LuaEntity::revive and ::silent_revive -- return an
	// ARRAY first and two handles after it, so the decode below is per-field
	// rather than per-member and every local it declares is suffixed with the
	// field's index. That is what the single-return version was quietly relying
	// on not needing.

	// THE `Into` VARIANT EXISTS ONLY FOR A CONTAINER RETURN. Asking for it on
	// anything else is not an error the census should count -- the caller loops
	// over every member and asks -- so it reports "no" rather than deferring.
	if into && !(len(res) == 1 && res[0].kind == KindArray) {
		return "", "", goSig{}, "", false
	}

	name = exportName(m.Name)
	switch {
	case m.Kind == MemberIndex:
		// `Get`, not `Index`, and the rename is forced rather than chosen:
		// LuaInventory and LuaGuiElement each declare an ORDINARY attribute
		// called `index`, so an operator named after itself would lose the name
		// to it and be deferred -- on LuaInventory, which is the whole of
		// fluid-memory-storage's F-IDX. `Get` is what all three reports asked
		// for anyway. TestOperatorsBindOnEveryClassThatHasOne fails loudly if a
		// future pin puts something else in the way.
		name = "Get"
	case m.Kind == MemberIndexSet:
		// `Set`, pairing with the `Get` above, and BARE rather than suffixed:
		// a class declares at most one index operator, so there is nothing to
		// disambiguate against. The only thing it can collide with is an
		// attribute-write member on the same class, and those are `Set` plus a
		// name and so never bare -- or a method literally called `set`, which no
		// pinned description puts on a class with a writable index operator.
		// `seen` would defer this one if it did, and
		// TestTheWritableIndexOperatorsGetASetter fails rather than letting the
		// setter quietly vanish the way the operators themselves once did.
		name = "Set"
	case m.Kind == MemberLen:
		name = "Length"
	case m.Kind == MemberSelf:
		name = "Call"
	case m.Kind == MemberSet:
		name = "Set" + name
	case m.Kind == MemberGetHandle, m.Kind == MemberCallHandle:
		// The HANDLE variant of an attribute read, or of a METHOD call. Suffixed
		// rather than given a name of its own so the two sit together in the
		// generated file and in any listing: TechnologiesRaw is next to
		// Technologies and GetEntityFilteredRaw next to GetEntityFiltered, and a
		// reader looking for one finds the other.
		name += "Raw"
	case m.Kind == MemberGetEq:
		// `entity.NameIs("transport-belt")`. The suffix reads as the predicate
		// it is at the call site, which is the whole ergonomic argument for a
		// generated member over a `streq(handle, member, ptr)` import: the
		// latter would be correct and would look like ABI plumbing in mod code.
		name += "Is"
	}
	// A NAME COLLISION IS A DECISION, and memberRename is where it is written
	// down. Applied here rather than in the caller's loop so `src` and `name`
	// cannot disagree, and only when the computed name really is the one the row
	// says it is replacing -- a row that stops matching is stale and is reported
	// rather than silently applied to a name nobody meant.
	if r, ok := memberRename[MemberKey(m)]; ok && !into && name == r.WasGo {
		name = r.Go
	}
	if into {
		name += "Into"
	}
	// <Name>Typed, beside <Name> rather than instead of it. The tier-2 form is
	// what makes these members reachable at all and a guest already using one
	// keeps compiling; this is the same member id with its shared parameters
	// spelled out.
	if typed {
		name += "Typed"
	}

	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	// A slice and a map are already nilable, so an optional one keeps its own
	// type rather than becoming *[]T or *map[K]V -- nil then means absent while
	// an empty one means present-and-empty, which is the distinction the
	// pointer was there to preserve.
	optType := func(f field) string {
		if f.has >= 0 && f.kind != KindArray && f.kind != KindDict {
			return "*" + f.goType
		}
		return f.goType
	}

	var params []string
	if into {
		// FIRST, and named `dst`, so the call site reads as the copy it is.
		// Trailing would put it after a variadic-looking filter struct and hide
		// the one parameter whose lifetime the caller has to think about.
		params = append(params, "dst "+res[0].goType)
	}
	for _, f := range in {
		params = append(params, f.ident+" "+optType(f))
	}
	retType := "error"
	if len(res) > 0 {
		var rts []string
		for _, f := range res {
			rts = append(rts, optType(f))
		}
		retType = "(" + strings.Join(rts, ", ") + ", error)"
	}
	// THE DESCRIPTION'S OWN PROSE, FIRST, because it is what the reader came for
	// and every note below it is about a variant of the member rather than about
	// the member. Empty for the ~700 members the description says nothing about,
	// which is the honest rendering of nothing to say.
	//
	// `<Name>: <sentence>` rather than Go's `<Name> <verb>s ...`, and that is a
	// deliberate deviation: the convention wants the prose to continue the
	// identifier grammatically, and Factorio's descriptions are not written that
	// way. Bending them into it would mean rewriting somebody else's sentences
	// at generation time, which is exactly how a doc comment stops being the
	// description and starts being a claim.
	//
	// THE BACKTICKS ARE REPLACED, in the GO emission and not in Member.Doc,
	// because the constraint is Go's alone: the generated package is carried
	// through a raw string downstream, so a backtick arriving from data would
	// close it -- TestNoBacktickReachesTheGeneratedSources is the standing gate
	// and this is the first thing that ever fed it description prose. Rust's
	// /// is markdown and keeps them, which is one sentence rendered two ways
	// under a hard constraint on one side rather than a drift.
	//
	// A single quote rather than nothing, so `true` reads as 'true' and the
	// author's own emphasis survives.
	for _, l := range wrapComment(name+": "+strings.ReplaceAll(m.Doc, "`", "'"), 76) {
		w("// %s\n", l)
	}
	if typed {
		w("// %s is %s with its SHARED parameters spelled out instead of\n",
			name, exportName(m.Name))
		w("// hand-built as tier-2 keys. Same member, same result; the variant\n")
		w("// tail goes in extra, whose keys are applied over the block, and a\n")
		w("// nil extra means there is no tail. The block crosses as a flat\n")
		w("// struct, which the host reads about 3x faster than the map form.\n")
	}
	// WHICH KEYS THE TAIL TAKES, on both forms of a variant-group member.
	//
	// "the variant tail goes in extra" names a parameter and nothing that goes
	// in it, so a reader could not learn from the typed form that `position` is
	// a parameter of create_segmented_unit at all -- WormholeBelts' item 11,
	// whose first reading was that the typed path was unusable. The plain form
	// needs it for the same reason and one more: there the groups are keys of
	// the ONLY argument, so without this the member's whole tail is unnamed.
	if gl := VariantGroupLines(m.VariantGroups, 76); len(gl) > 0 && !into {
		intro := fmt.Sprintf(
			"%s takes ONE tier-2 table, and besides the shared parameters "+
				"these %d variant parameter group(s) are keys of it. A group is "+
				"selected by the table's discriminant:", name, len(m.VariantGroups))
		if typed {
			intro = fmt.Sprintf(
				"%s's %d variant parameter group(s) have no field in args: "+
					"their parameters are keys of extra, and a group is selected "+
					"by the discriminant among them:", name, len(m.VariantGroups))
		}
		w("//\n")
		for _, l := range wrapComment(intro, 76) {
			w("// %s\n", goDocText(l))
		}
		for _, l := range gl {
			w("// %s\n", goDocText(l))
		}
	}
	// A UNION THAT COLLAPSED TO A HANDLE IN AN ARGUMENT POSITION, one line per
	// such parameter. The signature says Object, which every other handle in the
	// package satisfies: `create_space_platform` accepts a LuaPlanet at compile
	// time and the engine refuses it at runtime. See collapseddoc.go, and
	// WormholeBelts' item 4.
	for _, c := range collapsed {
		w("//\n")
		for _, l := range CollapsedUnionLines(c.ident, c.union, 76) {
			w("// %s\n", goDocText(l))
		}
	}
	if into {
		w("// %s is %s writing into dst, reusing its capacity rather than\n",
			name, exportName(m.Name))
		w("// allocating. The returned slice aliases dst when it fit; otherwise a\n")
		w("// new one is allocated and dst is untouched, so ALWAYS use the return\n")
		w("// value. Under -gc=leaking a discarded make() here is permanent.\n")
	}
	if len(omitted) > 0 {
		w("// %s omits the optional argument(s) %s: the Go layer has no type\n",
			name, strings.Join(omitted, ", "))
		w("// for that shape yet. An absent optional is omitted rather than\n")
		w("// defaulted, so this is the call a Lua author writes without it.\n")
	}
	if doc := operatorDoc(typeName, m, name); doc != "" {
		w("%s", doc)
	}
	if len(res) == 1 && res[0].kind == KindArray && res[0].has >= 0 && !into {
		// AN OPTIONAL ARRAY RETURN, and the whole of what makes it optional in
		// Go is this comment plus one guaranteed property of make(): a slice
		// built with make([]T, 0) is NON-NIL, so absent (nil) and present-empty
		// (len 0, non-nil) really are distinguishable and always have been.
		// Reported by fklua-ports' fuel-train-stop (FTS1) against BOTH backends;
		// the Rust half was a genuine loss and is Option<Vec<T>> now, and the Go
		// half was a correct answer nobody could see. Saying it is the fix.
		w("// %s returns nil when the value is ABSENT and a non-nil empty slice\n", name)
		w("// when it is present and empty: r == nil and len(r) == 0 are\n")
		w("// different questions here, and the API distinguishes them.\n")
	}
	if len(res) == 1 && res[0].kind == KindDict && res[0].entryType != "" && !into {
		// RM2, from fklua-ports' resource-marker: the pair slice is right and
		// its KEY is not the one the type reads like.
		w("// %s is keyed by a UNION, so it comes back as an ordered slice of\n", name)
		w("// pairs rather than a map. WHICH ARM OF THE UNION arrives is Lua's\n")
		w("// choice, not this ABI's: the host walks the table with pairs(), and\n")
		w("// for the engine's own name-or-index dictionaries -- game.surfaces,\n")
		w("// game.forces, game.players -- pairs() yields the NAME. Filtering on\n")
		w("// TagNumber matches nothing there, silently. Read the index off the\n")
		w("// handle if that is what you want.\n")
	}
	// NO RECEIVER FOR A GLOBAL FUNCTION. `log`, `localised_print` and
	// `table_size` are on no class, so there is nothing for a method to hang
	// off; the handle operand below is a literal 0 for the same reason.
	if m.Kind == MemberGlobalFunc {
		w("func %s(%s) %s {\n", name, strings.Join(params, ", "), retType)
	} else {
		w("func (o %s) %s(%s) %s {\n", typeName, name,
			strings.Join(params, ", "), retType)
	}

	// The values a failed call returns beside the status, one per declared
	// return. A multi-return member needs one apiece, which is the only place
	// the several-values work touches the ERROR path.
	zero := ""
	for i, f := range res {
		switch {
		case into && i == 0:
			// dst[:0] and not nil: a failed call must not look different from a
			// call that found nothing, and it must not silently hand the
			// caller's buffer back with the previous call's contents still in
			// it. Length zero, capacity kept, so the next attempt reuses it.
			zero += "dst[:0], "
		case f.has >= 0 || f.kind == KindArray || f.kind == KindDict:
			zero += "nil, "
		case f.kind == KindStruct || f.kind == KindDyn:
			zero += f.goType + "{}, "
		default:
			zero += goZero(f.goType) + ", "
		}
	}

	// Does anything here allocate? One bracket covers all of them however deep
	// they nest, because releasing truncates the pin list rather than walking
	// a tree.
	//
	// The two directions are asked differently on purpose. Going OUT, the guest
	// allocates only for the shapes whose elements do not fit the argument
	// block. Coming BACK, the host allocates for anything that is not a
	// fixed-width scalar -- see HostAllocatesFor, which is a whitelist because
	// the enumerate-what-allocates form of this predicate is what let string
	// returns leak.
	allocs := false
	for _, f := range in {
		if f.kind == KindArray || f.kind == KindDict || f.kind == KindDyn ||
			f.elemKind == KindDyn || f.keyKind == KindDyn ||
			// KindStruct, which changes no emitted byte here and is not
			// decoration: a struct argument's encodeAt allocates when the struct
			// has a container field, and this loop -- the going-out half, the
			// enumerating form -- never said so. Go emits the bracket anyway
			// because of the `args.Size > 0` clause below, which is about the
			// blocks being arena memory: a member with a struct argument
			// necessarily has a block, so it was covered for a reason unrelated
			// to the struct.
			//
			// Coverage by accident is not coverage. rustgen's blocks are stack
			// arrays, so it has no such clause, and the identical loop there left
			// 301 members unbracketed until 2026-08-07. Keeping the two loops
			// character-for-character the same is what stops the next divergence
			// being invisible -- a reader comparing them must see one predicate,
			// not two that happen to agree through a third condition.
			f.kind == KindStruct {
			allocs = true
		}
	}
	for _, f := range res {
		if HostAllocatesFor(f.kind) {
			allocs = true
		}
	}
	// A BLOCK OPENS THE BRACKET TOO, not just a value that allocates. The
	// blocks come out of the same arena now, for a reason that is measured
	// rather than assumed: `var a [n]byte` whose address is taken does NOT stay
	// on the stack under TinyGo -- ptrtoint defeats the promotion -- so it was
	// 16 bytes of permanently leaked guest heap per block per call, whatever
	// the block's size. examples/heap measures it; the control with no block at
	// all is the line that says so.
	if allocs || args.Size > 0 || rets.Size > 0 {
		w("\tmark := allocMark()\n\tdefer allocRelease(mark)\n")
	}

	// The blocks. A *[N]byte rather than a [N]byte so that every use below --
	// a[0], &a[0], &r[4] -- reads exactly as it did when these were locals;
	// Go indexes through an array pointer without a deref.
	if args.Size > 0 {
		w("\ta := (*[%d]byte)(block(%d))\n", args.Size, args.Size)
	}
	if rets.Size > 0 {
		w("\tr := (*[%d]byte)(block(%d))\n", rets.Size, rets.Size)
	}
	for _, f := range in {
		if f.kind == KindDict {
			// A DICTIONARY ARGUMENT, which used to be refused outright.
			//
			// The refusal's own words were "would need a deterministic
			// iteration order", and it was right about a Go MAP: Go randomizes
			// map iteration per process, Factorio is lockstep, and a per-run
			// order reaching the game is a per-CLIENT difference. What changed
			// is that a dictionary is not a map here any more -- Q3 made every
			// one of them the ordered pair SLICE -- and a slice's order is the
			// guest's own, chosen once, identical on every client.
			//
			// So the seven that were counted are emitted, and among them are
			// the four halves fluid-memory-storage reported as F-TAGS: `tags`
			// is read_write on LuaEntity, LuaGuiElement and LuaItemCommon, and
			// only the getter existed. It was counted rather than silent, which
			// the deferral report now says out loud -- but counted is not the
			// same as available, and a mod that wanted to write a tag had to
			// smuggle the whole dictionary through an ItemStackDefinition.
			//
			// The wire is the ARRAY wire, over pairs. Same fk_alloc buffer, same
			// (ptr, count) in the block, one extra store per element for the
			// key -- which is the observation fk_abi.lua's shared decoder rests
			// on, met from the writing side.
			if f.has >= 0 {
				w("\tif %s != nil {\n\t\ta[%d] = 1\n\t}\n", f.ident, f.has)
			}
			w("\tp%s := fkAlloc(uint32(len(%s)) * %d)\n", f.ident, f.ident, f.stride)
			w("\tfor i := range %s {\n", f.ident)
			w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p%s)+uintptr(i)*%d)), %d)\n",
				f.ident, f.stride, f.stride)
			w("\t\t%s\n", goStore("d", f.keyOff, f.keyKind, f.ident+"[i].Key"))
			w("\t\t%s\n\t}\n", goStoreElem("d", f.elemOff, f.elemKind, f.elemCtn,
				f.ident+"[i].Val"))
			w("\t*(*uint32)(unsafe.Pointer(&a[%d])) = p%s\n", f.off, f.ident)
			w("\t*(*uint32)(unsafe.Pointer(&a[%d])) = uint32(len(%s))\n", f.off+4, f.ident)
			continue
		}
		if f.kind == KindArray {
			// The elements go somewhere the host can reach them, which means
			// the guest's own allocator: fk_alloc is exactly the export the ABI
			// requires for this. The host reads them during the call and never
			// retains them, so the buffer is freed the moment the call returns.
			if f.has >= 0 {
				w("\tif %s != nil {\n\t\ta[%d] = 1\n\t}\n", f.ident, f.has)
			}
			w("\tp%s := fkAlloc(uint32(len(%s)) * %d)\n", f.ident, f.ident, f.stride)
			w("\tfor i := range %s {\n", f.ident)
			w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p%s)+uintptr(i)*%d)), %d)\n",
				f.ident, f.stride, f.stride)
			w("\t\t%s\n\t}\n", goStoreElem("d", f.elemOff, f.elemKind, f.elemCtn,
				f.ident+"[i]"))
			w("\t*(*uint32)(unsafe.Pointer(&a[%d])) = p%s\n", f.off, f.ident)
			w("\t*(*uint32)(unsafe.Pointer(&a[%d])) = uint32(len(%s))\n", f.off+4, f.ident)
			continue
		}
		if f.has >= 0 {
			w("\tif %s != nil {\n", f.ident)
			w("\t\ta[%d] = 1\n", f.has)
			// Parenthesised. A handle stores through .h, and `*p.h` parses as
			// `*(p.h)` -- dereferencing the field rather than the pointer. The
			// generated package did not compile until this did.
			w("\t\t%s\n", goStore("a", f.off, f.kind, "(*"+f.ident+")"))
			w("\t}\n")
			continue
		}
		w("\t%s\n", goStore("a", f.off, f.kind, f.ident))
	}
	ap, rp := "0", "0"
	if args.Size > 0 {
		ap = "ptr(&a[0])"
	}
	if rets.Size > 0 {
		rp = "ptr(&r[0])"
	}
	// THE HANDLE, and 0 for a global function -- which every other kind answers
	// ERR_BAD_HANDLE and this one never reads, because M.invoke's GFUNC branch
	// runs before the handle is resolved at all. The constant scan that prunes
	// the shipped member table reads operand 1 and not operand 0, so a literal
	// here changes nothing about what a mod ships.
	recv := "o.h"
	if m.Kind == MemberGlobalFunc {
		recv = "0"
	}
	call := "hostCall"
	if typed {
		call = "hostCallTyped"
	}
	w("\tif st := %s(%s, %d, %s, %s); st != 0 {\n", call, recv, m.ID, ap, rp)
	w("\t\treturn %sStatus(st)\n\t}\n", zero)
	// ONE DECODE PER RETURN FIELD, into v0, v1, ... ONE ABSENT FIELD MUST NOT
	// RETURN EARLY, which is the whole structural change the multi-return work
	// needed: `if r[has] == 0 { return nil, nil }` is correct for a member with
	// one return and silently drops the other two on a member with three. So an
	// absent optional assigns nothing and leaves its declared zero, and every
	// local a container decode needs carries the field's index.
	var retNames []string
	for i, f := range res {
		v := fmt.Sprintf("v%d", i)
		retNames = append(retNames, v)
		guard := func(body func()) {
			if f.has < 0 {
				body()
				return
			}
			w("\tif r[%d] != 0 {\n", f.has)
			body()
			w("\t}\n")
		}
		switch {
		case f.kind == KindDict:
			// The same walk as an array, over pairs. Only the container built
			// at the end differs -- the observation that lets fk_abi.lua share
			// one decoder between the two.
			if f.entryType != "" {
				w("\tvar %s []%s\n", v, f.entryType)
			} else {
				w("\tvar %s map[%s]%s\n", v, f.keyType, f.elemType)
			}
			guard(func() {
				w("\tbase%d := uintptr(*(*uint32)(unsafe.Pointer(&r[%d])))\n", i, f.off)
				w("\tn%d := int(*(*uint32)(unsafe.Pointer(&r[%d])))\n", i, f.off+4)
				if f.entryType != "" {
					w("\t%s = make([]%s, n%d)\n", v, f.entryType, i)
				} else {
					w("\t%s = make(map[%s]%s, n%d)\n", v, f.keyType, f.elemType, i)
				}
				w("\tfor i := 0; i < n%d; i++ {\n", i)
				w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(base%d+uintptr(i)*%d)), %d)\n",
					i, f.stride, f.stride)
				val := goLoadElem("d", f.elemOff, f.elemKind, f.elemType, f.elemCtn)
				if f.entryType != "" {
					// The ordered pair slice, for a key a Go map cannot hold --
					// and, since Q3, for every other key too. See goFieldFor.
					w("\t\t%s[i] = %s{Key: %s, Val: %s}\n\t}\n",
						v, f.entryType, goLoad("d", f.keyOff, f.keyKind), val)
				} else {
					w("\t\t%s[%s] = %s\n\t}\n", v, goLoad("d", f.keyOff, f.keyKind), val)
				}
			})
		case f.kind == KindArray:
			// The host wrote these elements by calling back into fk_alloc, so
			// the guest owns the buffer and frees it here. Copying out first is
			// what makes that safe: the returned slice is the guest's own
			// memory, with no lifetime tied to a host allocation.
			if into {
				// dst[:0] up front, so an absent optional hands back an EMPTY
				// destination rather than the previous call's contents.
				w("\t%s := dst[:0]\n", v)
			} else {
				w("\tvar %s []%s\n", v, f.elemType)
			}
			guard(func() {
				w("\tbase%d := uintptr(*(*uint32)(unsafe.Pointer(&r[%d])))\n", i, f.off)
				w("\tn%d := int(*(*uint32)(unsafe.Pointer(&r[%d])))\n", i, f.off+4)
				if into {
					// Three lines rather than a generic helper. A helper would
					// have to be `func reuse[T any]([]T, int) []T`, and generics
					// in a package that is megabytes of generated code and
					// compiled by TinyGo for every guest is a dependency worth
					// not taking for two statements.
					//
					// dst[:n] and not append: the loop below writes every
					// element, so zeroing a grown slice would be work thrown
					// away, and append would have to start from dst[:0] and
					// re-check capacity n times.
					w("\tif cap(dst) >= n%d {\n\t\t%s = dst[:n%d]\n\t} else {\n\t\t%s = make([]%s, n%d)\n\t}\n",
						i, v, i, v, f.elemType, i)
				} else {
					w("\t%s = make([]%s, n%d)\n", v, f.elemType, i)
				}
				w("\tfor i := 0; i < n%d; i++ {\n", i)
				w("\t\td := unsafe.Slice((*byte)(unsafe.Pointer(base%d+uintptr(i)*%d)), %d)\n",
					i, f.stride, f.stride)
				load := goLoadElem("d", f.elemOff, f.elemKind, f.elemType, f.elemCtn)
				w("\t\t%s[i] = %s\n\t}\n", v, load)
			})
		default:
			load := goLoad("r", f.off, f.kind)
			if f.kind == KindStruct {
				load = fmt.Sprintf("decode%s(&r[%d])", f.goType, f.off)
			}
			if f.has >= 0 {
				// Absent comes back as nil, so a caller can tell "no value"
				// from "the value is zero" -- which the API means differently.
				w("\tvar %s *%s\n", v, f.goType)
				w("\tif r[%d] != 0 {\n\t\tt%d := %s\n\t\t%s = &t%d\n\t}\n",
					f.has, i, load, v, i)
			} else {
				w("\t%s := %s\n", v, load)
			}
		}
	}
	if len(retNames) == 0 {
		w("\treturn nil\n")
	} else {
		w("\treturn %s, nil\n", strings.Join(retNames, ", "))
	}
	w("}\n\n")
	var argNames []string
	if into {
		argNames = append(argNames, "dst")
	}
	for _, f := range in {
		argNames = append(argNames, f.ident)
	}
	return b.String(), name, goSig{Params: params, Args: argNames, RetType: retType},
		"", true
}

// operatorDoc renders OperatorProse as a Go doc comment, or nothing when the
// member is not an operator.
// wrapComment breaks one line of prose onto lines of at most `width` columns,
// on spaces, and answers nothing at all for an empty string.
//
// A WORD LONGER THAN THE WIDTH IS NOT BROKEN: the descriptions carry
// `defines.events.on_player_changed_surface` and URLs, and a hyphenated break
// inside one would be a doc comment that cannot be pasted back into code.
func wrapComment(s string, width int) []string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ": ") {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

func operatorDoc(typeName string, m Member, name string) string {
	lines := OperatorProse(typeName, m, name)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "// %s\n", l)
	}
	return b.String()
}

// name0 is the member's Go name without the Set prefix, for synthesising a
// struct type name from where the struct appears.
func name0(m Member) string { return exportName(m.Name) }

// namedSpecs is m.Args or m.Rets with blank names filled in, matching what
// blocks() laid out so the two line up by index.
func namedSpecs(fs []FieldSpec, prefix string) []FieldSpec {
	out := append([]FieldSpec(nil), fs...)
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = fmt.Sprintf("%s%d", prefix, i)
		}
	}
	return out
}

// goFieldFor names one argument or return, registering a struct type if that is
// what it is.
func goFieldFor(g *goStructs, p Placed, specs []FieldSpec, i int, fallback string) (string, string, bool) {
	if p.Kind == KindArray {
		t, _, _, why, ok := goArrayElem(g, p, specs, i, fallback)
		if !ok {
			return "", why, false
		}
		return "[]" + t, "", true
	}
	if p.Kind == KindDict {
		kt, vt, _, _, _, why, ok := goDictKV(g, p, specs, i, fallback)
		if !ok {
			return "", why, false
		}
		// EVERY DICTIONARY IS THE ORDERED PAIR SLICE, whatever its key.
		//
		// It used to be the pair slice only for a key a Go map cannot hold --
		// tier 2, which holds an f64 and a slice -- and a `map[string]T` for
		// everything else, on the reasoning that a member-level dictionary
		// return is decode-only and a map buys lookup for free.
		//
		// A GO MAP'S ITERATION ORDER IS DELIBERATELY RANDOMISED, PER PROCESS,
		// and Factorio is a lockstep simulation, so that reasoning was one step
		// short: a guest that walks the map does host-visible work in a
		// different order on each client, which is a desync found by players
		// rather than by any suite. qol-research filed it as Q3 -- `game.forces`
		// and `force.technologies` side by side, one slice and one map, one
		// generator line apart -- and resource-marker WIDENED it past the
		// version anyone could argue with: its loop only READS, and it still has
		// to sort first, because walk order decides the order the guest
		// ALLOCATES in and the guest heap is in the save under every --persist
		// mode but `none`. Two clients that walked in two orders hold two
		// different saves.
		//
		// So the shape that was chosen for determinism is now the only shape,
		// which also makes the STRUCT-field case (already a pair slice, always)
		// and the member case agree instead of differing by one line. A guest
		// that wants a map builds one in three lines and owns the decision.
		//
		// AND IT IS WHAT MAKES A DICTIONARY ARGUMENT EXPRESSIBLE. The reason
		// dictionary args were refused was, verbatim, "would need a
		// deterministic iteration order" -- which a slice HAS, by construction,
		// because the guest chose it. See the argument encoder below.
		return "[]" + g.entryFor(kt, vt), "", true
	}
	if p.Kind != KindStruct {
		t, ok := goScalar(p.Kind)
		if !ok {
			return "", goScalarReason(p.Kind), false
		}
		return t, "", true
	}
	if i >= len(specs) {
		return "", "a struct field with no concept to name it", false
	}
	t, why, ok := g.add(specs[i], fallback)
	return t, why, ok
}

// goArrayElem resolves the element type of an array field, and the codec name
// when that element is ITSELF a container.
//
// It needs BOTH forms of the field: Placed carries the stride, which is the
// element's padded size and cannot be recomputed from the Go type, and
// FieldSpec carries the element's TypeName, which is what a struct element is
// named after. Placed drops the name and FieldSpec has no offsets, so neither
// alone is enough.
//
// A NESTED ARRAY IS NO LONGER A REFUSAL. It used to return "an array of an
// array" here, which was true of this generator and never of the wire --
// LayoutStruct had already placed it and fk_abi.lua had always decoded it. See
// gogen_nested.go.
func goArrayElem(g *goStructs, p Placed, specs []FieldSpec, i int, fallback string) (goType string, elem Placed, codec, why string, ok bool) {
	if p.Elem == nil || p.Stride <= 0 {
		return "", elem, "", "an array with no element layout", false
	}
	elem = *p.Elem
	var sub *FieldSpec
	if i < len(specs) {
		sub = specs[i].Elem
	}
	t, codec, why, ok := g.goElemType(elem, sub, fallback+"Elem", "an array")
	if !ok {
		return "", elem, "", why, false
	}
	return t, elem, codec, "", true
}

// goDictKV resolves a dictionary's key and value types.
//
// A dictionary is an array of key/value PAIRS, so it reuses the array layout
// wholesale: Stride is the pair's padded size, and Key and Elem are placed
// WITHIN the pair, each carrying its own offset. Only the container built at
// the end differs, which is why fk_abi.lua shares one walk between the two.
//
// A NESTED CONTAINER VALUE IS NO LONGER A REFUSAL, which is what unblocks
// `LuaPrototypes::utility_constants`, the five `LuaFlowStatistics` quality
// tables and `LuaRemote::interfaces`. The value recurses through goElemType and
// comes back with the name of a generated codec; the KEY does not, and that
// refusal keeps its own reason. See gogen_nested.go.
func goDictKV(g *goStructs, p Placed, specs []FieldSpec, i int, fallback string) (keyType, valType string, key, val Placed, valCodec, why string, ok bool) {
	if p.Key == nil || p.Elem == nil || p.Stride <= 0 {
		return "", "", key, val, "", "a dictionary with no key/value layout", false
	}
	key, val = *p.Key, *p.Elem

	// A Go map key must be comparable, which every scalar here is -- Object is
	// a struct{uint32} and qualifies. A struct key would too, but the API has
	// none, and generating for a shape nothing exercises is how a branch rots.
	// A DYN KEY IS NOT REFUSED ANY MORE, and nothing here has to know: goScalar
	// gives tier 2 the name `Value`, and the caller picks the container. Value
	// holds slices, so it cannot be a Go map key -- but the ordered pair slice
	// the dictionary FIELD work introduced has no comparability requirement,
	// which turned "three members do not earn a second container shape" into a
	// container shape that already exists. `game.surfaces` is one of the three.
	kt, ok2 := goScalar(key.Kind)
	if !ok2 {
		return "", "", key, val, "", "a dictionary keyed by " +
			goScalarReason(key.Kind)[len("returns or takes "):], false
	}
	var sub *FieldSpec
	if i < len(specs) {
		sub = specs[i].Elem
	}
	vt, vc, why, ok3 := g.goElemType(val, sub, fallback+"Value", "a dictionary")
	if !ok3 {
		return "", "", key, val, "", why, false
	}
	return kt, vt, key, val, vc, "", true
}

func goZero(t string) string {
	switch t {
	case "bool":
		return "false"
	case "string":
		return `""`
	case "Object":
		return "Object{}"
	}
	return "0"
}

// goStore writes one field into a block. A string is (ptr, len) over the Go
// string's own bytes, which is safe precisely because the host copies them out
// before the call returns.
func goStore(buf string, off int, k Kind, v string) string {
	switch k {
	case KindStruct:
		return fmt.Sprintf("%s.encodeAt(&%s[%d])", v, buf, off)
	case KindString:
		return fmt.Sprintf(
			"putStr(&%s[%d], %s)", buf, off, v)
	case KindHandle:
		return fmt.Sprintf("*(*uint32)(unsafe.Pointer(&%s[%d])) = %s.h", buf, off, v)
	case KindBool:
		return fmt.Sprintf("*(*bool)(unsafe.Pointer(&%s[%d])) = %s", buf, off, v)
	case KindDyn:
		return fmt.Sprintf("writeDyn(&%s[%d], %s)", buf, off, v)
	}
	g, _ := goScalar(k)
	return fmt.Sprintf("*(*%s)(unsafe.Pointer(&%s[%d])) = %s", g, buf, off, v)
}

func goLoad(buf string, off int, k Kind) string {
	switch k {
	case KindString:
		return fmt.Sprintf("getStr(&%s[%d])", buf, off)
	case KindHandle:
		return fmt.Sprintf("Object{*(*uint32)(unsafe.Pointer(&%s[%d]))}", buf, off)
	case KindDyn:
		return fmt.Sprintf("readDyn(&%s[%d])", buf, off)
	}
	g, _ := goScalar(k)
	return fmt.Sprintf("*(*%s)(unsafe.Pointer(&%s[%d]))", g, buf, off)
}

// goRuntime is the hand-written part of the generated package: the import
// declarations, the handle type, the allocator the host calls back into, and
// the string helpers.
const goRuntime = `
//go:wasmimport fk call
func hostCall(handle, member, argp, retp uint32) uint32

// hostCallTyped is the same dispatch over a TYPED argument block: same handle,
// same member id, same return block, and an argument block laid out as a tier-1
// struct plus one optional tier-2 slot instead of one tier-2 map. Only a member
// whose parameter table is a discriminated union has one -- see the <Name>Typed
// bindings below.
//
//go:wasmimport fk call_typed
func hostCallTyped(handle, member, argp, retp uint32) uint32

// hostBulkGet reads ONE attribute off count handles in ONE crossing, which is
// what every <Class><Name>Bulk binding in this file is.
//
// member is the ORDINARY getter's id -- there is no bulk member and no new id
// anywhere, which is why a mod that reads only in bulk still prunes to the one
// member it named. handlep points at count u32 handles, which is exactly what a
// []LuaEntity already is; dstp at count copies of that getter's own return
// block; and retp at four bytes the host writes the number of elements it
// actually read into.
//
// AN ELEMENT THAT CANNOT BE READ IS SKIPPED, NOT FATAL. A dead handle, an object
// whose valid went false, or a read the engine raised on writes that element as
// the ZERO value -- never leaving the previous crossing's value there, which
// would be the plausible wrong answer -- and does not count toward the return.
// So a count below len(objs) says something was missed, and for an attribute the
// description marks OPTIONAL the presence byte on each element says which. For a
// mandatory one, a zero that was skipped and a zero that was read are the same
// bytes; that is the honest limit of a flat destination.
//
// The status is about the CALL: StatusNoMember for a member that is not a
// readable attribute of a fixed-width shape, StatusBadArgs for a span that does
// not fit in this guest's memory.
//
//go:wasmimport fk bulk_get
func hostBulkGet(member, handlep, count, dstp, retp uint32) uint32

// bulkRead is where hostBulkGet writes the number of elements it read.
//
// PACKAGE-LEVEL RATHER THAN A BLOCK, which is fk.LastError's own reasoning: a
// local whose address is taken does not stay on TinyGo's stack -- ptrtoint
// defeats the promotion -- so it would be a permanent heap allocation per call,
// and an arena block would be a bracket per call for four bytes. It is read on
// the line after the call returns and a host call cannot re-enter the guest
// between the two, so A BULK READ ALLOCATES NOTHING AT ALL.
var bulkRead uint32

// A HANDLE IS FOUR BYTES, AND EVERY GENERATED CLASS TYPE IS ONE HANDLE WIDE.
//
// That is what lets a bulk read take a []LuaEntity and hand the host &objs[0]
// as an array of u32 handles, with no copy and no conversion: the slice the
// search already wrote IS the handle array. It is held by the toolchain rather
// than by a comment, because being wrong about it is a wrong answer rather than
// a build failure -- the host would read every other word.
var _ [4]byte = [unsafe.Sizeof(Object{})]byte{}

//go:wasmimport fk retain
func hostRetain(handle uint32) uint32

//go:wasmimport fk release
func hostRelease(handle uint32) uint32

//go:wasmimport fk subscribe
func hostSubscribe(event, filterp, skip, namep, namelen uint32) uint32

//go:wasmimport fk define
func hostDefine(id uint32) uint32

//go:wasmimport fk register
func hostRegister(kind, descp uint32) uint32

//go:wasmimport fk remote_call
func hostRemoteCall(callp, retp uint32) uint32

// Subscribe asks to receive an event. Call it from an init function, which is
// what _initialize runs -- script.on_event is legal at load, and a subscription
// made in fk_on_init would vanish the first time the save was reloaded.
//
// The id must be a CONSTANT at the call site. The compiler scans for it and
// ships only the event descriptors a guest actually subscribes to; an id it
// cannot prove constant makes it ship all of them, which is a bigger mod rather
// than a broken one.
//
// //go:inline for the pruning reason every wrapper in this family carries one --
// see SubscribeFiltered, which is where that argument is written out.
//
//go:inline
func Subscribe(event uint32) Status { return Status(hostSubscribe(event, 0, 0, 0, 0)) }

// SubscribeNamed subscribes to an event addressed by NAME rather than by
// defines.events, which is how Factorio delivers a CUSTOM INPUT -- the keybind
// a mod declares with a custom-input prototype at the data stage.
//
//	fkapi.SubscribeNamed(fkapi.EventCustomInputEvent, "my-mod-hotkey")
//
// THE EVENT ID IS STILL THE PAYLOAD'S and is still a constant at the call site.
// It says what the handler will be handed -- CustomInputEvent's player_index,
// input_name, cursor_position and the rest -- and the NAME says what to register
// under. The two are separate jobs: defines.events.CustomInputEvent does not
// exist in any Factorio (measured: the table has 233 keys and that is not one of
// them), so without a name there is nothing to register and fk.subscribe logs
// that it could not resolve the event.
//
// SEVERAL CUSTOM INPUTS SHARE ONE HANDLER, because they all carry the same
// payload descriptor and therefore the same id. Read input_name out of the
// payload to tell them apart:
//
//	e := fkapi.ReadCustomInputEvent(p)
//	switch e.InputName { case "my-mod-hotkey": ... }
//
// A name no custom-input prototype in this game has is refused by the ENGINE at
// subscribe time. It comes back as a status here and as one line in the log
// naming the engine's own words; the mod keeps running, because a typo in a
// keybind name is not worth a mod that will not load.
//
// //go:inline for the pruning reason every wrapper in this family carries one --
// see SubscribeFiltered.
//
//go:inline
func SubscribeNamed(event uint32, name string) Status {
	p, n := namePtr(name)
	return Status(hostSubscribe(event, 0, 0, p, n))
}

// SubscribeNamedMasked is SubscribeNamed and SubscribeMasked at once: the
// registration is by name and the host does not encode the fields this guest
// will not read. CustomInputEvent has three maskable ones --
// SkipCustomInputEventCursorDirection, ...SelectedPrototype and ...Element.
//
// There is deliberately no named-and-FILTERED form. Factorio's event filters are
// declared per described event, as the Lua<Event>EventFilter concepts, and a
// custom input has none -- so the combination would be a binding that exists and
// always fails, which is this project's "a skipped member is skipped, never
// faked" pointed at a wrapper.
//
// //go:inline for the pruning reason every wrapper in this family carries one --
// see SubscribeFiltered.
//
//go:inline
func SubscribeNamedMasked(event, skip uint32, name string) Status {
	p, n := namePtr(name)
	return Status(hostSubscribe(event, 0, skip, p, n))
}

// namePtr is the (pointer, length) a named subscription sends.
//
// A RAW PAIR rather than a tier-2 string, which is fk_log's shape and not the
// filter's. It allocates nothing and writes no dyn, which keeps both named
// wrappers small -- and a small wrapper is a wrapper the toolchain is likelier
// to inline, which is what keeps the event id arriving at the import as a
// compile-time constant and therefore what prunes the packaged event table.
//
// SMALL IS NOT THE GUARANTEE, THOUGH, AND THIS COMMENT USED TO SAY IT WAS.
// Whether a call is inlined is LLVM's cost heuristic's decision, taken per call
// site against the whole module, so it can change its mind because something
// ELSE grew -- the collector's own weight, or how much of init was folded in
// before these were weighed. It did: see SubscribeFiltered, where the same
// wrapper family stopped inlining under -gc=custom and shipped every descriptor
// there is. Every entry point in the family carries //go:inline now, and the
// standing evidence is a gate over BOTH -gc arms rather than a sentence here.
// A wrapper that grew until it stopped being inlined is R6, measured downstream
// at 85 KB of Lua per load.
//
// The host reads the bytes inside the call, which is the standing rule for a
// (pointer, length) a guest hands over: nothing may buffer one.
func namePtr(name string) (uint32, uint32) {
	if len(name) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(unsafe.StringData(name)))), uint32(len(name))
}

// SubscribeMasked subscribes and declares the payload fields this guest never
// reads, so the host stops encoding them.
//
// The encode is EAGER and complete: every field of an event is marshalled into
// the scratch buffer before the handler is entered. That is right for a flat
// payload -- the mean event has 4.8 fields and a host call per field would cost
// more -- and wrong for the few that carry a container. on_undo_applied's
// actions field is an array of tier-2 values, so an undo step's whole
// BlueprintEntity list is deep-copied before a handler wanting one uint32 runs.
//
//	fkapi.SubscribeMasked(fkapi.EventOnUndoApplied, fkapi.SkipOnUndoAppliedActions)
//
// OR the Skip constants together. Only OPTIONAL and CONTAINER fields have one,
// and that restriction is the safety property: a masked optional reads as
// ABSENT and a masked container as EMPTY, both of which every generated decoder
// already produces, so a mask that is wrong costs a value you did not get
// rather than a zero you cannot tell from a real one. A bit naming anything
// else is logged at subscribe time and ignored.
//
// The LAYOUT DOES NOT MOVE. Fields keep the offsets they were compiled at; only
// their contents go away.
//
// //go:inline for the pruning reason every wrapper in this family carries one --
// see SubscribeFiltered.
//
//go:inline
func SubscribeMasked(event, skip uint32) Status {
	return Status(hostSubscribe(event, 0, skip, 0, 0))
}

// SubscribeFiltered subscribes with Factorio's own event filters, which the
// engine applies in C++ BEFORE the handler runs.
//
// Without them a guest that cares about one prototype is entered for every
// build and mine event on the map and pays a dispatch, a host call and a string
// crossing to read entity.name and reject it. With them it is not entered.
//
//	fkapi.SubscribeFiltered(fkapi.EventOnPlayerMinedEntity,
//	    fkapi.NameFilter("my-machine")...)
//
// Terms are OR-ed, which is Factorio's default. Filters are decoded once, at
// subscribe time, so none of this is a per-event cost.
//
// # The filter grammar, and how to write a term this package has no helper for
//
// A term is a MAP whose "filter" key names the condition and whose other keys
// are that condition's own parameters. NameFilter and TypeFilter build the two
// commonest; everything else is an ordinary OfMap and this takes them as they
// come. The three shapes side by side, all equivalent to the Lua tables the
// engine documents:
//
//	fkapi.NameFilter("iron-chest")                  {filter="name", name="iron-chest"}
//	fkapi.TypeFilter("tree")                        {filter="type", type="tree"}
//	fkapi.OfMap(                                    {filter="type", type="tree", invert=true}
//	    fkapi.KeyValue{fkapi.OfString("filter"), fkapi.OfString("type")},
//	    fkapi.KeyValue{fkapi.OfString("type"), fkapi.OfString("tree")},
//	    fkapi.KeyValue{fkapi.OfString("invert"), fkapi.OfBool(true)})
//
// WHICH conditions an event accepts is per event and is in the API description
// this package was generated from, as the Lua<Event>EventFilter concepts --
// LuaPlayerMinedEntityEventFilter, LuaEntityDiedEventFilter and 29 more. Read
// them out of api/<version>/runtime-api.json, or online at
// lua-api.factorio.com/<version>/concepts/Lua<Event>EventFilter.html. Most
// entity events take "name", "type", "ghost_name" and "ghost_type" plus a
// handful of category conditions ("rail", "turret", "transport-belt-
// connectable", ...) that carry no parameter of their own -- a bare
// OfMap(KeyValue{OfString("filter"), OfString("turret")}).
//
// Every term also takes the optional "mode" ("or", the default, or "and" --
// which binds tighter) and "invert". A filter the event does not accept is
// refused by the ENGINE at subscribe time, not here, so it surfaces as a Lua
// error in the log naming the term.
//
// Two subscriptions to the SAME event share one registration, so their filters
// are UNION-ed and an unfiltered one widens the pair to unfiltered. That is the
// only merge that cannot silently stop delivering an event somebody asked for.
// An event that takes no filters at all is logged and subscribed unfiltered
// rather than failing the mod's load.
//
// # //go:inline IS LOAD-BEARING AND IT IS ABOUT MOD SIZE, NOT SPEED
//
// fklua mod ships only the event descriptors it can PROVE a guest subscribes
// to, by scanning the wasm for an i32.const reaching fk.subscribe's first
// operand, and the scan is INTRAPROCEDURAL and all-or-nothing: if this wrapper
// survives as a real function, the id reaches the import as local.get $0, the
// scan gives up, and every descriptor there is ships.
//
// So the function that has to DISAPPEAR is the one the guest called -- the
// constant lives at the guest's call site and nowhere else. That is why the
// hint is on all six public entry points rather than on whichever one is
// biggest: marking only a callee moves its body up into a caller that is still
// a real function taking the id in a parameter, which is measured (//go:inline
// on SubscribeFilteredMasked alone leaves the scan red; on SubscribeFiltered
// alone it goes green).
//
// A HINT, NOT A DIRECTIVE, and the asymmetry with the Rust arm is worth
// knowing: TinyGo lowers //go:inline to LLVM's inlinehint, which the inliner
// weighs and may decline, where Rust's #[inline(always)] on the same six
// wrappers lowers to alwaysinline and is obeyed. The standing evidence here
// is therefore the gate rather than the pragma --
// TestTheEventIdSurvivesTheGeneratedSubscribeWrapper, which builds BOTH -gc
// arms, because they do not agree about inlining and an arm nobody builds is an
// arm nobody is gating.
//
// Filed by BetterBeltBalancer (item 30), and it is R6's shape one toolchain
// over. Without the hint, whether the id survives is LLVM's cost heuristic's
// decision taken per call site: measured on this repo's own examples/api under
// -gc=custom -opt=2, four filtered subscriptions sharing one mixed filter list
// were enough for this wrapper to stop being inlined, so the packaged mod went
// from "7 events subscribed, of 219" to "all 219 events -- an event id was not
// a compile-time constant" and fk_api_gen.lua from 8,425 bytes to 60,118. The
// -gc=leaking arm of the same source inlined it and proved all seven, which is
// how it stayed hidden. A guest crosses that line by GROWING rather than by
// changing anything.
//
//go:inline
func SubscribeFiltered(event uint32, filters ...Value) Status {
	return SubscribeFilteredMasked(event, 0, filters...)
}

// SubscribeFilteredMasked is SubscribeFiltered and SubscribeMasked at once:
// the engine drops the events this guest does not want, and the host does not
// encode the fields it will not read from the ones that survive.
//
// //go:inline for the pruning reason its siblings carry -- see
// SubscribeFiltered. This one is exported, so a guest may name it directly and
// it needs the hint on its own account.
//
//go:inline
func SubscribeFilteredMasked(event, skip uint32, filters ...Value) Status {
	if len(filters) == 0 {
		return Status(hostSubscribe(event, 0, skip, 0, 0))
	}
	mark := allocMark()
	b := (*[dynW]byte)(block(dynW))
	writeDyn(&b[0], OfArray(filters...))
	st := Status(hostSubscribe(event, ptr(&b[0]), skip, 0, 0))
	// RELEASED PLAINLY RATHER THAN BY defer, and the difference is inlining
	// cost. There is no early exit between the mark and the host call, and a
	// panic on this target traps the guest rather than unwinding out of it (see
	// agents/guests.md), so a defer here can only ever run on the one path the
	// statement below already covers -- while the machinery it lowers to was
	// most of what this wrapper cost the inliner to swallow.
	allocRelease(mark)
	return st
}

// NameFilter builds the commonest event filter there is: only these prototype
// names. Pass the result to SubscribeFiltered with "...".
func NameFilter(names ...string) []Value {
	out := make([]Value, len(names))
	for i := range names {
		out[i] = OfMap(
			KeyValue{OfString("filter"), OfString("name")},
			KeyValue{OfString("name"), OfString(names[i])},
		)
	}
	return out
}

// TypeFilter is NameFilter's twin over prototype TYPE: {filter="type",
// type="tree"} rather than {filter="name", name="tree-01"}. One term catches a
// whole family, which is what a guest that cares about "any tree" or "any
// assembling-machine" wants -- names are per prototype and there are hundreds.
//
// Terms are OR-ed with everything else in the same SubscribeFiltered call, so
// mixing the two is append(NameFilter(...), TypeFilter(...)...).
func TypeFilter(types ...string) []Value {
	out := make([]Value, len(types))
	for i := range types {
		out[i] = OfMap(
			KeyValue{OfString("filter"), OfString("type")},
			KeyValue{OfString("type"), OfString(types[i])},
		)
	}
	return out
}

// AddCommand declares a console command, handled by this guest's fk_on_call
// export.
//
// THE HANDLER DOES NOT CROSS THE BOUNDARY, and cannot: a wasm guest has no
// callable Lua value. What crosses is id, an integer this guest chooses; the
// host synthesises a Lua closure that captures it, hands THAT to
// commands.add_command, and dispatches back in through fk_on_call when the
// command is typed. So the shape a guest writes is a switch, exactly like the
// one fk_on_event needs:
//
//	const cmdHello = 1
//
//	func init() { fkapi.AddCommand(cmdHello, "hello", fkapi.OfString("says hello")) }
//
//	//go:wasmexport fk_on_call
//	func onCall(id, argp, retp uint32) uint32 {
//	    switch id {
//	    case cmdHello:
//	        // args[0] is the CustomCommandData table: name, tick,
//	        // player_index and parameter.
//	        d := fkapi.ReadDyn(argp).Array()[0].Map()
//	        _ = d
//	    }
//	    return 0
//	}
//
// CALL IT FROM AN init FUNCTION, for a reason stronger than the one Subscribe
// gives. A command registration is not saved: Factorio re-executes control.lua
// on every load, so it has to be made on every load. _initialize is the only
// place that happens by construction -- a registration made from fk_on_init
// would exist in the session that created the map and in no other.
//
// help is a LocalisedString, so either OfString("...") or an OfArray of a key
// and its parameters. The id space is this guest's own and the host keeps no
// table of it.
func AddCommand(id uint32, name string, help Value) Status {
	mark := allocMark()
	defer allocRelease(mark)
	b := (*[dynW]byte)(block(dynW))
	writeDyn(&b[0], OfMap(
		KeyValue{OfString("name"), OfString(name)},
		KeyValue{OfString("help"), help},
		KeyValue{OfString("id"), OfNumber(float64(id))},
	))
	return Status(hostRegister(regCommand, ptr(&b[0])))
}

// InterfaceMethod names one method of a remote interface and the id fk_on_call
// will be given for it.
//
// A SLICE OF THESE RATHER THAN A GO MAP KEYED BY NAME, which is what this was
// until TestNoGeneratedGoMapSurvives refused it -- and the refusal is right for a
// reason bigger than the invariant. Go randomises map walk order per process, so
// a map here would decide the order these names are ENCODED in, which decides
// the order the guest heap allocates in, and the guest heap is in the save. The
// same reasoning that made every dictionary return an ordered slice of pairs.
// The Rust arm takes a slice for the same reason and now has the same shape.
type InterfaceMethod struct {
	Name string
	ID   uint32
}

// AddInterface declares a remote interface whose methods are handled by this
// guest's fk_on_call export.
//
// Everything AddCommand's comment says about ids, closures and _initialize
// applies here unchanged; the one difference is that a remote method's RESULT is
// used, so fk_on_call should write one through WriteDyn(retp, ...).
//
//	func init() {
//	    fkapi.AddInterface("my-mod",
//	        fkapi.InterfaceMethod{"get_count", mGetCount},
//	        fkapi.InterfaceMethod{"set_count", mSetCount})
//	}
//
// The arguments a caller passed arrive as a tier-2 array at argp, with the arity
// the caller actually wrote -- a nil in the middle is preserved rather than
// truncating the list.
func AddInterface(name string, methods ...InterfaceMethod) Status {
	if len(methods) == 0 {
		return StatusBadArgs
	}
	kv := make([]KeyValue, len(methods))
	for i, m := range methods {
		kv[i] = KeyValue{OfString(m.Name), OfNumber(float64(m.ID))}
	}
	mark := allocMark()
	defer allocRelease(mark)
	b := (*[dynW]byte)(block(dynW))
	writeDyn(&b[0], OfMap(
		KeyValue{OfString("name"), OfString(name)},
		KeyValue{OfString("methods"), OfMap(kv...)},
	))
	return Status(hostRegister(regInterface, ptr(&b[0])))
}

// RegisterModEvent subscribes to an event ANOTHER MOD defined, handled by this
// guest's fk_on_call export.
//
// THE SUBSCRIBE HALF OF A PUBLISHED PROTOCOL. A mod publishes an event with
// script.generate_event_name() and hands the id out through its remote
// interface; every consumer subscribes to that number. All of the publishing
// side binds -- GenerateEventName, RaiseEvent, GetEventID -- and until this
// existed the consuming side did not: a runtime-minted id is a NUMBER where
// Subscribe wants a dense index into the generated event table, and a
// mod-defined event's payload is not in the API description at all, so there is
// no field descriptor to encode it with.
//
//	const evDelivery = 7 // this guest's own dispatch id, not the event's
//
//	func init() {
//	    v, st := fkapi.RemoteCall("logistic-train-network", "on_delivery_completed")
//	    if st == fkapi.StatusOK {
//	        if n, ok := v.AsNum(); ok {
//	            fkapi.RegisterModEvent(evDelivery, uint32(n))
//	        }
//	    }
//	}
//
//	//go:wasmexport fk_on_call
//	func onCall(id, argp, retp uint32) uint32 {
//	    if id == evDelivery {
//	        e := fkapi.ReadDyn(argp).At(0) // the payload, as the publisher sent it
//	        _ = e
//	    }
//	    return 0
//	}
//
// THE PAYLOAD IS ONE TIER-2 VALUE, because there is nothing to type it against:
// the other end is another mod's table and no description carries its shape.
// That is why this is fk_on_call rather than fk_on_event -- the id is this
// guest's own, the argument list is the seam's, and the whole of it is the
// machinery AddCommand already uses.
//
// CALL IT FROM AN init FUNCTION, exactly as with AddCommand and for the same
// reason: a registration is not saved, so it has to be made on every load, and
// nothing here writes an event id into the save -- an id minted by a mod that
// may not be installed next time is not a thing to carry across one.
//
// A publisher that is not installed is StatusCallFailed from the RemoteCall
// above, which is where a consumer finds out; this call is never reached.
func RegisterModEvent(id, event uint32) Status {
	return registerModEvent(id, OfNumber(float64(event)))
}

// RegisterModEventNamed is RegisterModEvent for an event declared as a
// custom-event PROTOTYPE at the data stage rather than minted at runtime.
//
//	fkapi.RegisterModEventNamed(evDelivery, "some-mod-delivery-done")
//
// Both are arms of Factorio's own LuaEventType and the host passes either
// through unchanged. A name no custom-event prototype in this game has is
// refused by the ENGINE at subscribe time and comes back as StatusNoMember, with
// the engine's own words in one log line; the mod keeps running, which is the
// same treatment SubscribeNamed gives a missing custom input.
func RegisterModEventNamed(id uint32, name string) Status {
	return registerModEvent(id, OfString(name))
}

func registerModEvent(id uint32, ev Value) Status {
	mark := allocMark()
	defer allocRelease(mark)
	b := (*[dynW]byte)(block(dynW))
	writeDyn(&b[0], OfMap(
		KeyValue{OfString("id"), OfNumber(float64(id))},
		KeyValue{OfString("event"), ev},
	))
	return Status(hostRegister(regModEvent, ptr(&b[0])))
}

// RemoteCall is remote.call: the outbound half of mod-to-mod interop.
//
// The member itself is unbindable -- it is the API's one variadic method, and
// the ABI's argument block has a fixed shape decided at generate time -- so the
// arguments are packed into one tier-2 array instead. That is the whole of the
// difference; there is no arity ceiling.
//
// A missing interface or method is StatusCallFailed rather than a raised error,
// because the other mod not being installed is an ordinary thing for a guest to
// have an opinion about.
func RemoteCall(iface, method string, args ...Value) (Value, Status) {
	mark := allocMark()
	defer allocRelease(mark)
	b := (*[dynW * 2]byte)(block(dynW * 2))
	writeDyn(&b[0], OfArray(
		OfString(iface), OfString(method), OfArray(args...),
	))
	if st := hostRemoteCall(ptr(&b[0]), ptr(&b[dynW])); st != 0 {
		return Value{}, Status(st)
	}
	return readDyn(&b[dynW]), StatusOK
}

// ReadDyn decodes the tier-2 value at a pointer the host handed this guest --
// fk_on_call's argp, and nothing else today.
func ReadDyn(p uint32) Value { return readDyn((*byte)(unsafe.Pointer(uintptr(p)))) }

// WriteDyn encodes a tier-2 value into a slot the host handed this guest --
// fk_on_call's retp, and nothing else today. A guest that writes nothing leaves
// the slot as the host cleared it, which reads back as nil.
func WriteDyn(p uint32, v Value) {
	writeDyn((*byte)(unsafe.Pointer(uintptr(p))), v)
}

// The descriptor kinds fk.register takes, mirroring fk_mod.lua's REG_COMMAND,
// REG_INTERFACE and REG_MODEVENT -- and rustgen_rt.go's, which is the fourth
// spelling of the same three numbers and is checked against the other three
// rather than trusted.
const (
	regCommand   = 1
	regInterface = 2
	regModEvent  = 3
)

// Status is a host-call result. It is never a Lua error: without coroutines
// there is no way to unwind a wasm frame, so a failure crossing that boundary
// would take down the whole mod rather than the one call.
type Status uint32

const (
	StatusOK          Status = 0
	StatusBadHandle   Status = 1
	StatusInvalid     Status = 2
	StatusNoMember    Status = 3
	StatusBadArgs     Status = 4
	StatusCallFailed  Status = 5
	StatusNoSpace     Status = 6
)

func (s Status) Error() string {
	switch s {
	case StatusBadHandle:
		return "fklua: not a live handle"
	case StatusInvalid:
		return "fklua: the object is no longer valid"
	case StatusNoMember:
		return "fklua: this Factorio version does not have that member"
	case StatusBadArgs:
		return "fklua: bad arguments"
	case StatusCallFailed:
		return "fklua: the Factorio API raised"
	case StatusNoSpace:
		return "fklua: out of space"
	}
	return "fklua: unknown status"
}

// Object is a handle to a LuaObject.
//
// Handles the host produces are TRANSIENT: they stop working when the event
// that produced them returns. That is the default that makes the dominant leak
// shape free -- take a handle, use it, drop it, and nothing accumulates. Retain
// promotes one that has to outlive the event.
type Object struct{ h uint32 }

// Retain makes a handle survive past the current event. Release it when done.
func (o Object) Retain() Object { return Object{hostRetain(o.h)} }

// Release frees a retained handle. Releasing a transient one is harmless.
func (o Object) Release() { hostRelease(o.h) }

// Valid reports a handle that is not the null one. It does not ask the game
// whether the object behind it still exists -- a call does that, and reports
// StatusInvalid.
func (o Object) Valid() bool { return o.h != 0 }

// ObjectAt wraps a raw handle. Needed for the fixed globals; a guest should
// otherwise get its handles by calling something.
func ObjectAt(h uint32) Object { return Object{h} }

// Handle is the raw index, and it is ObjectAt's inverse.
//
// A guest almost never wants it: the number is the host's bookkeeping, and a
// guest that stores one instead of an Object has taken on the job the two
// handle spaces exist to do for it. What it is for is DIAGNOSTICS -- logging
// which persistent slot a retain landed in, which is the only way a guest can
// observe that a save carried the handle table and that adopt rebuilt the free
// list around the holes. The Rust binding has always exposed this (Object is a
// public tuple struct there); this is the Go side catching up.
func (o Object) Handle() uint32 { return o.h }

func ptr(p *byte) uint32 { return uint32(uintptr(unsafe.Pointer(p))) }

// putStr writes a (pointer, length) pair over the Go string's own bytes.
//
// Safe because the host copies them out before the call returns: nothing keeps
// the pointer, so the string cannot be collected out from under it.
func putStr(dst *byte, s string) {
	slot := (*[2]uint32)(unsafe.Pointer(dst))
	if len(s) == 0 {
		slot[0], slot[1] = 0, 0
		return
	}
	slot[0] = uint32(uintptr(unsafe.Pointer(unsafe.StringData(s))))
	slot[1] = uint32(len(s))
}

// getStr copies a (pointer, length) the host wrote into a Go string.
//
// The bytes were allocated by fkAlloc below, and the copy is what lets them be
// forgotten immediately afterwards.
func getStr(src *byte) string {
	slot := (*[2]uint32)(unsafe.Pointer(src))
	if slot[0] == 0 || slot[1] == 0 {
		return ""
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(slot[0]))), slot[1])
	return string(b)
}

// THE MARSHALLING ARENA.
//
// Everything a host call needs somewhere to put -- its argument block, its
// return block, and whatever fk_alloc hands the host to write into -- has a
// lifetime of exactly one call. Every one of them used to be a separate
// allocation: the blocks because taking a local array's address defeats
// TinyGo's promotion of it to the stack, and fk_alloc because its body was
// make([]byte, n). Under -gc=leaking, which is mandatory, none of it is ever
// reclaimed.
//
// Measured through examples/heap before this existed: 16 B/call for an argument
// block, 16 for a return block whatever their size, and 96 for a three-entry
// tier-2 map -- 128 B/call for one create_entity, kept forever, in every save
// and every multiplayer join. It also fed a second pathology: --persist=packed
// tracks a dirty byte RANGE, and an allocator walking upward dirties a fresh
// page on every call, so one recompile repacked essentially the whole heap.
//
// Chunks rather than one buffer, because a POINTER HANDED OUT EARLIER IN THE
// SAME CALL MUST STAY VALID: growing by reallocation would move it. A chunk is
// never moved once allocated; running out means moving on to the next, which
// the release below undoes. Steady state is zero allocation once the high-water
// mark of one call is reached.
const arenaChunkBytes = 4096

var (
	arena    [][]byte
	arenaAt  int // index of the chunk being filled
	arenaOff int // bump offset within it
)

// allocState is where the arena stood when a call began.
type allocState struct{ at, off int }

// allocMark and allocRelease bracket one host call's allocations.
//
// A call can allocate on both sides -- the guest for its blocks and for an
// array or nested dynamic value going out, the host for anything coming back --
// and a dynamic value nests arbitrarily, so a tree walk to free it would have
// to mirror writeDyn exactly and would rot the first time either changed.
// Restoring the bump pointer is O(1) and covers every allocation either side
// made, however deep.
//
// RE-ENTRANCY, which is the whole of the difficulty: Factorio raises some
// events synchronously from inside the API call that caused them, so a nested
// dispatch's bindings take their own marks ABOVE this one and reclaim to their
// own -- never to zero, because everything below belongs to a call further out
// that has not finished. The same rule the string scratch region follows, for
// the same reason.
//
// THE INVARIANT: an allocation meant to outlive the call must be made outside
// one -- and that is now enforceable rather than accidental. It used to hold
// only because -gc=leaking never reclaimed anything, so releasing a pin was
// harmless; an arena really does hand the bytes to somebody else. The event
// scratch buffers are the only such allocation, and fk_mod.lua takes them
// through fk_alloc_static below, which never comes out of the arena.
func allocMark() allocState { return allocState{arenaAt, arenaOff} }

func allocRelease(m allocState) { arenaAt, arenaOff = m.at, m.off }

// block reserves n zeroed bytes of arena for an argument or return block.
//
// Zeroed because a plain [n]byte local was, and an absent optional writes its
// presence byte and leaves the value slot alone -- a block carrying the
// previous call's bytes there would be wrong in the silent direction.
func block(n int) unsafe.Pointer { return arenaAlloc(n) }

func arenaAlloc(n int) unsafe.Pointer {
	if n <= 0 {
		return nil
	}
	n = (n + 7) &^ 7 // 8 is the widest alignment the wire uses
	for {
		if arenaAt < len(arena) {
			c := arena[arenaAt]
			if arenaOff+n <= len(c) {
				b := c[arenaOff : arenaOff+n]
				for i := range b {
					b[i] = 0
				}
				arenaOff += n
				return unsafe.Pointer(&b[0])
			}
			// This chunk cannot hold it. Move on rather than splitting: a
			// pointer already handed out inside this same call points into the
			// chunk being abandoned and must stay where it is.
			arenaAt++
			arenaOff = 0
			continue
		}
		size := arenaChunkBytes
		for size < n {
			size *= 2
		}
		arena = append(arena, make([]byte, size))
	}
}

// Buffers that must outlive the call that asked for them. Held here so nothing
// can reclaim them, and so the arena never has to know about them.
//
// It grows without bound, which is correct rather than sloppy: the host asks
// for one buffer per DISPATCH NESTING LEVEL and caches it in storage beside the
// HEAP the buffer lives in, so this reaches the deepest nesting the mod ever
// performs and stops. That bound held only WITHIN A SESSION until P12 -- the
// cache was a Lua local, which a load rebuilds empty while the heap comes back
// from the save, so every load pinned a twin beside a buffer that was already
// here. See fk_mod.lua's publish_buffers.
var kept [][]byte

// THE SAME BRACKET, EXPORTED, FOR THE DIRECTION THAT HAS NO BINDING.
//
// allocMark/allocRelease above are called by the generated bindings, so a
// GUEST-initiated host call keeps nothing. A HOST-initiated dispatch -- an event
// Factorio raised, a console command somebody typed, a remote method another mod
// called -- has no binding, because nothing here made the call. The host still
// allocates through fk_alloc to get the payload in: a string field larger than
// the 4 KiB scratch region falls back to it, and every one of those advanced the
// bump pointer permanently. Under -gc=leaking that is a leak into every save;
// under the collector it is worse, because the arena above is a package-level
// root and a chunk it holds can never be reclaimed.
//
// So fk_mod.lua brackets the outermost dispatch through these two. Its
// "marshalling arena's outermost bracket" section is where the soundness argument
// lives -- the short version is that everything crossing inbound is copied out of
// arena memory by the decoders above (getStr makes a Go string, readDyn makes Go
// slices) before the handler returns.
//
// ONE SLOT, because only the OUTERMOST dispatch brackets: fk_mod.lua's two entry
// paths both test depth == 0 before marking, so a second mark before the first
// release cannot happen. A stack would be defensive against a caller that does
// not exist and would cost the very thing the next paragraph is about. What
// would break it is a mark taken at depth > 0, and the token is what makes that
// a change to both sides rather than a silent overwrite.
//
// NEITHER PATH WRITES WHEN IT HAS NOTHING TO SAY, AND THAT IS THE WHOLE COST
// STORY. A wasm export is a Lua function call, but a package-level var is
// LINEAR MEMORY, so a store here dirties a page -- and under --persist=packed a
// page nothing else touched is ~40 µs of repack at the end of the dispatch.
// Measured on a do-nothing dispatch, which is the case that has no other write
// to hide behind: an unguarded save cost 614 ns -> 51.4 µs, while every leg that
// allocates a block was flat, because arenaAlloc writes these same two words and
// dirties that page anyway.
//
// So the save is skipped when the state is already the saved one and the restore
// is skipped when nothing moved. Both are the DPLO/DPHI trick from MEMPACK.mark,
// for the same reason and against the same cost. They are honest as caching
// rather than as semantics: the depth-0 arena state is invariant precisely
// because everything that allocates from the arena gives it back, so the compare
// is true from the second dispatch onward and nothing is being assumed.
var arenaSaved allocState

//go:wasmexport fk_arena_mark
func fkArenaMark() uint32 {
	if arenaSaved.at != arenaAt || arenaSaved.off != arenaOff {
		arenaSaved = allocState{arenaAt, arenaOff}
	}
	return 1
}

//go:wasmexport fk_arena_release
func fkArenaRelease(tok uint32) {
	// 0 is "no mark was taken", which is what a host that never called
	// fk_arena_mark hands back. Releasing to a slot nobody filled would rewind
	// to the zero state, which is somebody's live bytes.
	if tok == 0 {
		return
	}
	if arenaAt != arenaSaved.at || arenaOff != arenaSaved.off {
		arenaAt, arenaOff = arenaSaved.at, arenaSaved.off
	}
}

// A tier-2 dynamic value: what the host sends where the API's type is a union,
// a LocalisedString, or anything else without a fixed layout.
type ValueTag uint32

const (
	TagNil    ValueTag = 0
	TagBool   ValueTag = 1
	TagNumber ValueTag = 2
	TagString ValueTag = 3
	TagObject ValueTag = 4
	TagArray  ValueTag = 5
	TagMap    ValueTag = 6
)

// Value is one dynamic value. Read Tag, then the field it names.
type Value struct {
	Tag    ValueTag
	Bool   bool
	Number float64
	Str    string
	Object Object
	Array  []Value
	// Map is a SLICE of pairs rather than a Go map, for two reasons: a Value
	// holds slices and so cannot be a Go map key, and a slice keeps the order
	// the host sent -- which matters in a lockstep game where table order is
	// insertion order.
	Map []KeyValue
}

// KeyValue is one entry of a dynamic map.
type KeyValue struct{ Key, Val Value }

func OfNil() Value                { return Value{Tag: TagNil} }
func OfBool(b bool) Value         { return Value{Tag: TagBool, Bool: b} }
func OfNumber(f float64) Value    { return Value{Tag: TagNumber, Number: f} }
func OfString(s string) Value     { return Value{Tag: TagString, Str: s} }
func OfObject(o Object) Value     { return Value{Tag: TagObject, Object: o} }
func OfArray(vs ...Value) Value   { return Value{Tag: TagArray, Array: vs} }
func OfMap(kv ...KeyValue) Value  { return Value{Tag: TagMap, Map: kv} }

// Reading one back.
//
// There were seven constructors and no accessors, so every read of a tier-2 map
// was a hand-written linear scan and a tag switch -- and the scans in this
// repo's own examples read kv.Val.Str without ever looking at kv.Val.Tag, which
// is the empty string for a number and for an absent key alike.
//
// TWO FAMILIES, AND THE SPLIT IS WHAT LETS ONE OF THEM CHAIN. A lookup (Get,
// GetKey, At) answers with a Value, and a miss is TagNil -- so
// v.Get("a").Get("b").NumOr(0) is one expression over a shape that may not be
// there. A read (AsBool, AsNum, AsStr, AsObj) answers with a comma-ok, so a
// caller who needs to know says so; the Or forms are the same read with the ok
// spent on a default, which is what most call sites want.
//
// NOTHING HERE COERCES. AsNum on a string is (0, false) rather than a parse:
// the tag is what the host said the value IS, and a codec that guessed would
// turn a wrong type into a plausible number. Len is the one asymmetry and it is
// deliberate -- it answers 0 for a scalar, because "how many" has an answer
// there and it is none.
//
// A MISS AND A PRESENT NIL ARE DIFFERENT, AND Get CANNOT TELL YOU WHICH. Has is
// what answers that. It is a separate call because the distinction matters at a
// small fraction of call sites: Factorio's own option tables read an absent key
// and a nil one identically.

// IsNil reports whether this is the nil value. The zero Value is nil.
func (v Value) IsNil() bool { return v.Tag == TagNil }

// Len is the number of elements in an array or of pairs in a map, and 0 for
// everything else.
func (v Value) Len() int {
	switch v.Tag {
	case TagArray:
		return len(v.Array)
	case TagMap:
		return len(v.Map)
	}
	return 0
}

// Get looks a string key up in a map. A value that is not a map, and a key that
// is not there, both answer TagNil -- so the result chains.
//
// A LINEAR SCAN, which is the honest shape for a pair slice: the maps the API
// carries are option tables and event payloads with a handful of keys, and an
// index would allocate on every lookup to shorten a walk that is over in ten
// compares. A guest reading one map many times builds its own.
func (v Value) Get(key string) Value {
	if v.Tag != TagMap {
		return Value{}
	}
	for i := range v.Map {
		if v.Map[i].Key.Tag == TagString && v.Map[i].Key.Str == key {
			return v.Map[i].Val
		}
	}
	return Value{}
}

// GetKey is Get for a key that is not a string -- a number-keyed map, which is
// what the API produces where the Lua table is indexed. Equality is by tag and
// payload, and a container key never matches: no described map is keyed by one,
// and comparing two slices elementwise would be a cost on every lookup.
func (v Value) GetKey(key Value) Value {
	if v.Tag != TagMap {
		return Value{}
	}
	for i := range v.Map {
		if sameScalar(v.Map[i].Key, key) {
			return v.Map[i].Val
		}
	}
	return Value{}
}

// Has reports whether a map carries this key, which is the one question Get
// cannot answer: a key present and nil reads exactly like a key that is absent.
func (v Value) Has(key string) bool {
	if v.Tag != TagMap {
		return false
	}
	for i := range v.Map {
		if v.Map[i].Key.Tag == TagString && v.Map[i].Key.Str == key {
			return true
		}
	}
	return false
}

// At indexes an array, ZERO-BASED -- this is Go, and the one-based Lua index the
// host read it out of is behind us. Out of range, or not an array, is TagNil.
func (v Value) At(i int) Value {
	if v.Tag != TagArray || i < 0 || i >= len(v.Array) {
		return Value{}
	}
	return v.Array[i]
}

// AsBool, AsNum, AsStr and AsObj read the payload the tag names. The ok is
// false for every other tag INCLUDING nil, which is what keeps an absent key
// and a present false apart.
func (v Value) AsBool() (bool, bool) { return v.Bool, v.Tag == TagBool }

func (v Value) AsNum() (float64, bool) { return v.Number, v.Tag == TagNumber }

func (v Value) AsStr() (string, bool) { return v.Str, v.Tag == TagString }

func (v Value) AsObj() (Object, bool) { return v.Object, v.Tag == TagObject }

// BoolOr, NumOr, StrOr and ObjOr are those reads with the ok spent on a default.
func (v Value) BoolOr(def bool) bool {
	if v.Tag == TagBool {
		return v.Bool
	}
	return def
}

func (v Value) NumOr(def float64) float64 {
	if v.Tag == TagNumber {
		return v.Number
	}
	return def
}

func (v Value) StrOr(def string) string {
	if v.Tag == TagString {
		return v.Str
	}
	return def
}

func (v Value) ObjOr(def Object) Object {
	if v.Tag == TagObject {
		return v.Object
	}
	return def
}

// Dump renders this value into dst and answers how many bytes it wrote.
//
// A DEBUGGER'S EYES, and until it existed a guest handed a tier-2 value had no
// way to find out what was in it: the accessors above answer a question you
// already knew to ask, and the recorded debugging loop for the other case was
// recompile, repackage, rerun and diff a transcript.
//
// INTO A BUFFER THE CALLER OWNS, AND NOT A string. Building one would allocate,
// and under -gc=leaking every byte of it is permanent -- which is exactly the
// cost guest/go/fklog exists to remove, so a dumper that allocated would undo
// it at the one call site most likely to be in a loop. fklog lends its own tail
// for this:
//
//	fklog.Start("v=")
//	fklog.Advance(v.Dump(fklog.Tail()))
//	fklog.End()
//
// TRUNCATION OVER GROWTH, like everything else on this path: a value bigger than
// dst is cut, and the return is what fits. A caller that must know can compare
// the count against len(dst).
//
// DETERMINISTIC BY CONSTRUCTION. A map's pairs are a SLICE and are rendered in
// the order the host sent them; nothing here iterates a Go map, so two guests
// on two clients render one value identically. That is the same property
// Value.Map is a slice of pairs for.
//
// The rendering is Lua-ish rather than JSON: {k=v, ...} for a map with string
// keys, [a, b] for an array, quoted strings, and #N for a handle. It is for a
// person reading a log, so it is not parsed back anywhere and is not a format
// anything may depend on.
func (v Value) Dump(dst []byte) int {
	d := dumper{dst: dst}
	d.value(v)
	return d.n
}

type dumper struct {
	dst []byte
	n   int
}

func (d *dumper) s(x string) { d.n += copy(d.dst[dmin(d.n, len(d.dst)):], x) }

func (d *dumper) value(v Value) {
	switch v.Tag {
	case TagNil:
		d.s("nil")
	case TagBool:
		if v.Bool {
			d.s("true")
		} else {
			d.s("false")
		}
	case TagNumber:
		d.num(v.Number)
	case TagString:
		d.s("\"")
		d.s(v.Str)
		d.s("\"")
	case TagObject:
		d.s("#")
		d.uint(uint64(v.Object.h))
	case TagArray:
		d.s("[")
		for i := range v.Array {
			if i > 0 {
				d.s(", ")
			}
			d.value(v.Array[i])
		}
		d.s("]")
	case TagMap:
		d.s("{")
		for i := range v.Map {
			if i > 0 {
				d.s(", ")
			}
			// A STRING KEY IS RENDERED BARE, which is what a Lua table literal
			// looks like and what the API's option tables all are. Anything else
			// takes the [k] form, so a number-keyed map cannot read as a
			// string-keyed one.
			if k := v.Map[i].Key; k.Tag == TagString {
				d.s(k.Str)
			} else {
				d.s("[")
				d.value(k)
				d.s("]")
			}
			d.s("=")
			d.value(v.Map[i].Val)
		}
		d.s("}")
	default:
		d.s("?")
	}
}

// num renders a number the way a diagnostic wants it: an integral value with no
// fractional part, and anything else to three decimal places.
//
// NOT strconv.FormatFloat, which links a large chunk of formatting code into a
// guest that has no other use for it -- the same trade fklog's F1 states. A
// value too large for an int64 renders as big, because printing it wrongly
// would be worse than saying so.
func (d *dumper) num(f float64) {
	if f < 0 {
		d.s("-")
		f = -f
	}
	if f >= 9.2e18 {
		d.s("big")
		return
	}
	whole := uint64(f)
	frac := uint64((f-float64(whole))*1000 + 0.5)
	if frac >= 1000 {
		whole++
		frac -= 1000
	}
	d.uint(whole)
	if frac == 0 {
		return
	}
	d.s(".")
	// Zero-padded to three places, so 0.5 is 0.500 and never 0.5.
	if frac < 100 {
		d.s("0")
	}
	if frac < 10 {
		d.s("0")
	}
	// TRAILING ZEROES TRIMMED, so 1.5 is 1.5 and not 1.500.
	for frac%10 == 0 {
		frac /= 10
	}
	d.uint(frac)
}

func (d *dumper) uint(v uint64) {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	d.n += copy(d.dst[dmin(d.n, len(d.dst)):], b[i:])
}

// dmin rather than min, which is a BUILTIN since Go 1.21: a package-level
// definition shadows it for the whole generated package, so a later generator
// change that emitted min over two floats would stop compiling for a reason
// nothing here names. Same family rule the emitter follows one layer down.
func dmin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sameScalar is equality for a map KEY. A tag mismatch is never equal, and a
// container is never equal to anything.
func sameScalar(a, b Value) bool {
	if a.Tag != b.Tag {
		return false
	}
	switch a.Tag {
	case TagNil:
		return true
	case TagBool:
		return a.Bool == b.Bool
	case TagNumber:
		return a.Number == b.Number
	case TagString:
		return a.Str == b.Str
	case TagObject:
		return a.Object.h == b.Object.h
	}
	return false
}

// The wire widths, fixed by fk_abi.lua: one dynamic value, and one pair of them.
const dynW = 16
const dynPW = 32

// readDyn decodes a dynamic value, copying everything into Go memory.
//
// The buffers it reads are the host's, and they are released by the
// allocRelease the calling binding deferred -- not here, because a nested value
// shares its parent's buffer and freeing per node would free the same region
// more than once.
func readDyn(p *byte) Value {
	d := unsafe.Slice(p, dynW)
	switch ValueTag(*(*uint32)(unsafe.Pointer(&d[0]))) {
	case TagBool:
		return Value{Tag: TagBool, Bool: d[8] != 0}
	case TagNumber:
		return Value{Tag: TagNumber, Number: *(*float64)(unsafe.Pointer(&d[8]))}
	case TagString:
		return Value{Tag: TagString, Str: getStr(&d[8])}
	case TagObject:
		return Value{Tag: TagObject, Object: Object{*(*uint32)(unsafe.Pointer(&d[8]))}}
	case TagArray:
		base := uintptr(*(*uint32)(unsafe.Pointer(&d[8])))
		n := int(*(*uint32)(unsafe.Pointer(&d[12])))
		out := make([]Value, n)
		for i := 0; i < n; i++ {
			out[i] = readDyn((*byte)(unsafe.Pointer(base + uintptr(i)*dynW)))
		}
		return Value{Tag: TagArray, Array: out}
	case TagMap:
		base := uintptr(*(*uint32)(unsafe.Pointer(&d[8])))
		n := int(*(*uint32)(unsafe.Pointer(&d[12])))
		out := make([]KeyValue, n)
		for i := 0; i < n; i++ {
			e := base + uintptr(i)*dynPW
			out[i] = KeyValue{
				Key: readDyn((*byte)(unsafe.Pointer(e))),
				Val: readDyn((*byte)(unsafe.Pointer(e + dynW))),
			}
		}
		return Value{Tag: TagMap, Map: out}
	}
	return Value{Tag: TagNil}
}

// writeDyn encodes a dynamic value where the host can read it.
//
// A string needs no allocation: putStr points at the Go string's own bytes, and
// the host copies them before the call returns. An array or a map does, and
// those come from fkAlloc.
func writeDyn(p *byte, v Value) {
	d := unsafe.Slice(p, dynW)
	for i := range d {
		d[i] = 0
	}
	*(*uint32)(unsafe.Pointer(&d[0])) = uint32(v.Tag)
	switch v.Tag {
	case TagBool:
		if v.Bool {
			d[8] = 1
		}
	case TagNumber:
		*(*float64)(unsafe.Pointer(&d[8])) = v.Number
	case TagString:
		putStr(&d[8], v.Str)
	case TagObject:
		*(*uint32)(unsafe.Pointer(&d[8])) = v.Object.h
	case TagArray:
		n := len(v.Array)
		base := fkAlloc(uint32(n) * dynW)
		for i := range v.Array {
			writeDyn((*byte)(unsafe.Pointer(uintptr(base)+uintptr(i)*dynW)), v.Array[i])
		}
		*(*uint32)(unsafe.Pointer(&d[8])) = base
		*(*uint32)(unsafe.Pointer(&d[12])) = uint32(n)
	case TagMap:
		n := len(v.Map)
		base := fkAlloc(uint32(n) * dynPW)
		for i := range v.Map {
			e := uintptr(base) + uintptr(i)*dynPW
			writeDyn((*byte)(unsafe.Pointer(e)), v.Map[i].Key)
			writeDyn((*byte)(unsafe.Pointer(e+dynW)), v.Map[i].Val)
		}
		*(*uint32)(unsafe.Pointer(&d[8])) = base
		*(*uint32)(unsafe.Pointer(&d[12])) = uint32(n)
	}
}

// The string scratch region. The host writes returned strings in here instead
// of calling fk_alloc for each one -- see bind_scratch in runtime/lua/fk_abi.lua
// for why that is sound and what it is worth.
//
// A package-level array rather than a make(): its address is fixed for the life
// of the module, so the host can be told about it once, and it needs no pin
// because nothing can collect it. It also sits in linear memory like everything
// else, so it is saved and restored with the rest of the heap and needs no
// persistence handling of its own -- its contents between calls are meaningless
// either way.
//
// 4 KiB is one packed-mode page. That is not decoration: in --persist=packed a
// write dirties the page it lands in, and a bump allocator moving through the
// heap dirties a DIFFERENT page on every call, where this dirties the same one.
// Anything longer than what is left falls back to fk_alloc, so the size is a
// tuning choice and never a correctness one.
var fkScratch [4096]byte

//go:wasmexport fk_scratch_base
func fkScratchBase() uint32 { return uint32(uintptr(unsafe.Pointer(&fkScratch[0]))) }

//go:wasmexport fk_scratch_size
func fkScratchSize() uint32 { return uint32(len(fkScratch)) }

//go:wasmexport fk_alloc
func fkAlloc(n uint32) uint32 {
	if n == 0 {
		return 0
	}
	// Out of the arena, so the bytes go back when the call that needed them
	// returns. Anything the host asks for through this export is call-scoped by
	// construction: the binding copies out of it and never looks again.
	return uint32(uintptr(arenaAlloc(int(n))))
}

//go:wasmexport fk_alloc_static
func fkAllocStatic(n uint32) uint32 {
	// THE ESCAPE HATCH FOR THE ONE BUFFER THAT OUTLIVES A CALL, and the reason
	// it has to exist: fk_mod.lua caches an event scratch buffer PER NESTING
	// LEVEL, and every level past the first is allocated from inside a nested
	// dispatch -- that is, while an outer binding's bracket is open. An arena
	// allocation made there is reclaimed when that binding returns and then
	// handed to something else, while Lua goes on writing event data into it.
	//
	// Nothing was wrong with the old code; it was correct only because
	// -gc=leaking never reclaimed anything, so releasing a pin was harmless.
	// Making the reclaim real turns that accident into a requirement, so the
	// requirement gets its own export rather than a comment.
	if n == 0 {
		return 0
	}
	b := make([]byte, n)
	kept = append(kept, b)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

//go:wasmexport fk_free
func fkFree(p uint32) {
	// A NO-OP, and honestly so since the arena landed. It used to drop a pin,
	// which mattered when every fk_alloc appended to a list nothing shortened;
	// now the whole call's arena goes back at the bracket, so there is nothing
	// per-buffer left to do. It stays exported because the host calls it on the
	// path where a later return field fails and an earlier one must be undone,
	// and because removing an export is a compatibility change for no gain.
	_ = p
}
`

// ---------------------------------------------------------------------------
// Struct types
//
// A struct field needs a NAME on the guest side or every use would be an
// anonymous Go struct repeated inline -- assignable, technically, and unusable
// in practice, because a mod author could not declare a variable of the type
// they were passing. Members that reach a shape this cannot name are deferred,
// and `go_deferrals_by_reason` in census.json is where that count lives. A
// literal here claimed 528 of them and named no pin, no query and no date, so
// there was nothing to re-derive it from once it stopped being true; it is
// deleted rather than refreshed.
//
// Named concepts keep their name (155 of them). An inline table in a method
// signature has none, so one is synthesised from where it appears -- 126 of
// those, nearly all `takes_table` argument tables.
// ---------------------------------------------------------------------------

// goStructs collects every struct type the bound members reach.
type goStructs struct {
	// byName is the emitted Go type name -> its fields, placed.
	byName map[string]StructBlock
	order  []string
	// dynValue counts the structs emitDynReaders matched -- see there.
	dynValue int
	// blocked records a type that cannot be emitted because a field needs a
	// shape the Go layer does not carry yet.
	blocked map[string]bool
	// elem describes an ARRAY field's element, keyed "Parent.field". Held here
	// for the same reason fieldType is: the stride and the element's offset
	// come from the layout, the element's TYPE NAME comes from the spec, and
	// neither is recoverable from the other afterwards.
	elem map[string]structArray
	// fieldType maps "Parent.field" to the Go type its nested struct became.
	// Recorded when the name is decided rather than recovered by matching
	// layouts afterwards -- two distinct concepts can share a shape, and
	// guessing between them would name the wrong type.
	fieldType map[string]string
	// dict describes a DICTIONARY field, keyed "Parent.field", for the same
	// reason elem does: half the description is in the layout and half in the
	// spec.
	dict map[string]structDict
	// entry holds the pair types a dictionary field is a slice of, deduplicated
	// by (key type, value type) so `Tags` appearing in five event payloads emits
	// one EntryStringValue rather than five identical types.
	entry      map[string]bool
	entryOrder []entryType
	// ctn holds the NESTED containers -- an array of arrays, a dictionary of
	// dictionaries -- keyed by the Go type, which determines the layout. Each
	// gets one generated decCtn/encCtn/valCtn triple. See gogen_nested.go.
	ctn      map[string]goContainer
	ctnOrder []string
	// note maps "Parent.field" to a doc comment the field cannot state for
	// itself. Only LuaLazyLoadedValue uses it: such a field crosses as a bare
	// Object, so without a line saying what the handle IS and what Get() yields,
	// a reader has a uint32 and no way to find out. See FieldSpec.LazyPayload.
	note map[string]string
	// variants maps a generated struct's NAME to the variant parameter groups
	// of the member whose shared parameters it carries. Only the typed form of
	// a variant-group method has one, and without it the struct's doc comment
	// is a complete-looking picture of an incomplete parameter list: the group
	// parameters are keys of `extra`, not fields here. See
	// FieldSpec.Variants and variantdoc.go.
	variants map[string]*VariantDoc
	// collapsed maps "Parent.field" to the union a shape-B field was declared
	// as, alongside note and for the same reason: the field crosses as a bare
	// Object, so without a line saying which class and what the description
	// offered instead, a reader has a handle that any other handle type-checks
	// against. See FieldSpec.Collapsed and collapseddoc.go.
	collapsed map[string]*CollapsedUnion
	// bulkOpts holds the destination-element structs a bulk read of an OPTIONAL
	// attribute needs, keyed by type name and rendered once. At most eleven can
	// exist -- one per fixed-width wire kind -- and they are registered on
	// demand, so a description with no optional f32 attribute emits no
	// BulkOptFloat32. See gogen_bulk.go.
	bulkOpts     map[string]string
	bulkOptNames []string
}

// structArray is one array-typed struct field: what its elements are and how
// far apart they sit.
type structArray struct {
	goType string
	kind   Kind
	off    int // the element's offset WITHIN its one-field block
	stride int
	// ctn is the codec of an element that is ITSELF a container, empty
	// otherwise. See gogen_nested.go.
	ctn string
}

// structDict is one dictionary-typed struct field. A dictionary is an array of
// key/value PAIRS on the wire, so this is structArray plus the key -- Stride is
// the pair's padded size and both offsets are placed WITHIN the pair.
type structDict struct {
	entryType        string
	keyType, valType string
	keyKind, valKind Kind
	keyOff, valOff   int
	stride           int
	// valCtn is the codec of a VALUE that is itself a container -- which is
	// what UtilityConstants::default_trigger_target_mask_by_type is, and what
	// took LuaPrototypes::utility_constants down with it.
	valCtn string
}

// entryType is one generated pair type.
type entryType struct{ name, keyType, valType string }

// entryFor registers the pair type a dictionary field is a slice of.
//
// A GO MAP WOULD BE A DESYNC. A struct crosses in BOTH directions -- the same
// type is an argument and a return -- and writing a Go map to the wire means
// choosing an order for its pairs, which Go deliberately randomizes. Factorio is
// a lockstep simulation where `pairs` follows insertion order, so a per-run
// ordering reaches the game as a per-CLIENT difference. A slice of pairs is
// deterministic by construction, which is the same reasoning that made tier 2's
// own Value.Map a slice, and it is why a dictionary ARGUMENT is refused at
// member level rather than sorted.
//
// It also costs nothing a map would not: a guest that wants lookup builds one
// itself, from an order it chose.
func (g *goStructs) entryFor(keyType, valType string) string {
	name := "Entry" + goTypeIdent(keyType) + goTypeIdent(valType)
	if !g.entry[name] {
		g.entry[name] = true
		g.entryOrder = append(g.entryOrder, entryType{name, keyType, valType})
	}
	return name
}

// goTypeIdent turns a Go type name into something that can sit inside another
// identifier. Dictionary keys and values are scalars or named structs here, so
// this only ever has to capitalize.
func goTypeIdent(t string) string {
	t = strings.NewReplacer("[]", "Slice", "*", "Ptr", ".", "").Replace(t)
	if t == "" {
		return "X"
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// taken reports a name already claimed by a concept type, so an event payload
// cannot quietly reuse someone else's layout.
func (g *goStructs) taken(name string) bool {
	_, done := g.byName[name]
	return done || g.blocked[name]
}

func newGoStructs() *goStructs {
	return &goStructs{
		byName:    map[string]StructBlock{},
		blocked:   map[string]bool{},
		fieldType: map[string]string{},
		elem:      map[string]structArray{},
		dict:      map[string]structDict{},
		entry:     map[string]bool{},
		ctn:       map[string]goContainer{},
		note:      map[string]string{},
		variants:  map[string]*VariantDoc{},
		collapsed: map[string]*CollapsedUnion{},
		bulkOpts:  map[string]string{},
	}
}

// add registers a struct type and everything it contains, walking the FieldSpec
// tree rather than the placed one -- Placed drops TypeName, and the name is the
// entire point here.
//
// ok is false when a field needs a shape the Go layer does not carry yet.
func (g *goStructs) add(f FieldSpec, fallback string) (string, string, bool) {
	name := exportName(f.TypeName)
	if f.TypeName == "" {
		name = fallback
	}
	// The variant-group listing, recorded BEFORE the early returns: it belongs
	// to the name, the name is decided above, and a struct reached a second
	// time carries the same spec. Recorded here rather than recovered at emit
	// time for the same reason fieldType is -- Placed does not carry it.
	if f.Variants != nil {
		g.variants[name] = f.Variants
	}
	if g.blocked[name] {
		return "", "struct " + name + " is itself deferred", false
	}
	if _, done := g.byName[name]; done {
		return name, "", true
	}

	blk, err := LayoutStruct(f.Struct)
	if err != nil {
		g.blocked[name] = true
		return "", "struct " + name + " has no memory layout", false
	}
	// Reserve the name before recursing so a type reachable from itself does
	// not spin. Go expresses recursion through a pointer, which this does not
	// emit, so a genuine cycle ends up blocked rather than looping.
	g.byName[name] = blk
	g.order = append(g.order, name)

	// blk.Fields is one Placed per FieldSpec, in the same order -- an optional
	// becomes a HasOffset on its own entry rather than a separate one -- so the
	// two can be walked together. The spec has the names, the placed form has
	// the offsets, and an array field needs both.
	fail := func(why string) (string, string, bool) {
		g.blocked[name] = true
		delete(g.byName, name)
		// AND OUT OF THE EMISSION ORDER. The name was reserved above so a type
		// reachable from itself does not spin; leaving it here made emit() read
		// a zero StructBlock out of byName and write `type X struct{}` under the
		// concept's REAL name, with a zero-size codec. Ten types shipped that
		// way -- MapGenSettings and TileBuildabilityRule among them -- which is
		// precisely the failure this package has a rule against: a struct
		// missing a field is a wrong value the guest cannot detect. It stayed
		// invisible because every member that would have used one was deferred
		// for the same reason, so nothing referenced the empty type.
		for i, n := range g.order {
			if n == name {
				g.order = append(g.order[:i], g.order[i+1:]...)
				break
			}
		}
		return "", why, false
	}
	for i, sub := range f.Struct {
		// A lazily-loaded value's payload type, which the field's own Go type
		// cannot state: it is an Object like any other handle. Recorded here
		// rather than recovered at emit time for the same reason fieldType is --
		// Placed does not carry it.
		if sub.LazyPayload != "" {
			g.note[name+"."+sub.Name] = sub.LazyPayload
		}
		// A UNION THAT COLLAPSED TO THIS FIELD'S HANDLE, recorded beside the
		// note above and for the same reason: Placed does not carry it, and by
		// emit time the field is an Object like every other.
		//
		// A COPY, because one field is added here: whether the member that owns
		// this struct also has a PLAIN tier-2 form, which still takes the arm
		// the collapse dropped. f.Variants is set on exactly the argument
		// struct of a variant-group method's typed form, and those are exactly
		// the members generated in two forms -- known HERE and nowhere
		// downstream, since emit() has a struct name and no member. The spec's
		// own CollapsedUnion is shared with the other backend and with every
		// member that reaches this concept, so it is read and not written.
		if sub.Collapsed != nil {
			c := *sub.Collapsed
			c.TierTwoTwin = f.Variants != nil
			g.collapsed[name+"."+sub.Name] = &c
		}
		if sub.Kind == KindStruct {
			child, why, ok := g.add(sub, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.fieldType[name+"."+sub.Name] = child
			continue
		}
		if sub.Kind == KindArray {
			if i >= len(blk.Fields) {
				return fail("a struct array field with no placed layout")
			}
			p := blk.Fields[i]
			et, elem, ctn, why, ok := goArrayElem(g, p, f.Struct, i, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.elem[name+"."+sub.Name] = structArray{
				goType: et, kind: elem.Kind, off: elem.Offset, stride: p.Stride,
				ctn: ctn,
			}
			continue
		}
		if sub.Kind == KindDict {
			if i >= len(blk.Fields) {
				return fail("a struct dictionary field with no placed layout")
			}
			p := blk.Fields[i]
			kt, vt, key, val, vc, why, ok := goDictKV(g, p, f.Struct, i, name+exportName(sub.Name))
			if !ok {
				return fail(why)
			}
			g.dict[name+"."+sub.Name] = structDict{
				entryType: g.entryFor(kt, vt),
				keyType:   kt, valType: vt,
				keyKind: key.Kind, valKind: val.Kind,
				keyOff: key.Offset, valOff: val.Offset, stride: p.Stride,
				valCtn: vc,
			}
			continue
		}
		if _, ok := goScalar(sub.Kind); !ok {
			return fail(goScalarReason(sub.Kind))
		}
	}
	return name, "", true
}

// goTypeOf names the Go type for a field, registering any struct it reaches.
func (g *goStructs) goTypeOf(f FieldSpec, fallback string) (string, string, bool) {
	if f.Kind == KindStruct {
		return g.add(f, fallback)
	}
	t, ok := goScalar(f.Kind)
	if !ok {
		return "", goScalarReason(f.Kind), false
	}
	return t, "", true
}

// emit writes the type declarations and their codecs, in registration order so
// the output is stable.
func (g *goStructs) emit(w func(string, ...any)) {
	// The bulk destination-element structs first: they name no concept and
	// contain nothing, so nothing below can depend on where they sit, and a
	// reader looking for BulkOptUint32 finds every one of them together.
	sort.Strings(g.bulkOptNames)
	for _, n := range g.bulkOptNames {
		w("\n%s", g.bulkOpts[n])
	}
	for _, e := range g.entryOrder {
		w("\n// %s is one entry of a dictionary. A SLICE of pairs rather than a\n", e.name)
		w("// Go map, for either of two reasons depending on where it appears.\n")
		w("// A struct field crosses in BOTH directions, and Go randomizes map\n")
		w("// iteration -- which in a lockstep game reaches the engine as a\n")
		w("// per-client ordering, i.e. a desync. And a tier-2 Value key holds\n")
		w("// slices, so it could not be a Go map key at all.\n//\n")
		w("// DO NOT BUILD A GO MAP FROM IT FOR LOOKUP, which is what this\n")
		w("// comment used to advise. Measured by fklua-ports on\n")
		w("// force.technologies: one read is 14,544 B of guest heap and the\n")
		w("// advised map adds 12,512 B on top -- 27,056 B, WORSE than the\n")
		w("// 24,576 B Go map the slice replaced. For a point lookup use the\n")
		w("// <Name>Raw accessor, which returns the LuaCustomTable HANDLE and\n")
		w("// costs one Get(key); for a scan, scan the slice, which allocates\n")
		w("// nothing. The order you send is the order the game sees, and the\n")
		w("// order you RECEIVE is the host's pairs() order -- deliberately\n")
		w("// unpromised, so sort by index where order matters.\n")
		w("type %s struct {\n\tKey %s\n\tVal %s\n}\n", e.name, e.keyType, e.valType)
	}
	// The nested-container codecs, after the pair types they are slices of and
	// before the structs that hold them. Go does not care about order at
	// package level; a reader does.
	g.emitContainers(w)
	for _, name := range g.order {
		blk := g.byName[name]
		w("\n// %s mirrors the API type of the same name, laid out to match the\n", name)
		w("// wire exactly: fields at fixed offsets, an optional as a pointer.\n")
		// AND WHAT IS NOT A FIELD HERE. A variant-group method's shared
		// parameters are these fields; its group parameters are keys of the
		// typed form's extra and have no field, no type and no identifier
		// anywhere in this package. A struct that lists only the shared half
		// and says nothing reads as the whole parameter list.
		if v := g.variants[name]; v != nil && len(v.Groups) > 0 {
			w("//\n")
			for _, l := range wrapComment(fmt.Sprintf(
				"%s holds the SHARED parameters of %s. Its %d variant parameter "+
					"group(s) are NOT fields here: their parameters are keys of "+
					"the tier-2 tail, which the typed form takes as extra and the "+
					"plain form takes as the whole of args.",
				name, v.Owner, len(v.Groups)), 76) {
				w("// %s\n", goDocText(l))
			}
			for _, l := range VariantGroupLines(v.Groups, 76) {
				w("// %s\n", goDocText(l))
			}
		}
		w("type %s struct {\n", name)
		for _, p := range blk.Fields {
			t := g.goFieldType(name, p)
			// An optional becomes a pointer so nil can mean absent -- except a
			// slice, which is nilable already. The same rule the member
			// signatures use, and forgetting it here broke six structs.
			if p.HasOffset >= 0 && p.Kind != KindArray && p.Kind != KindDict {
				t = "*" + t
			}
			// A lazily-loaded value is an Object and reads like every other
			// handle, so say what it is and what it yields. The engine builds
			// the payload only when Get() is called; not calling it is free.
			if note := g.note[name+"."+p.Name]; note != "" {
				w("\t// %s is a LuaLazyLoadedValue<%s>.\n", exportName(p.Name), note)
				w("\t//\n")
				w("\t// The payload is NOT crossed with the event: wrap this in\n")
				w("\t// LuaLazyLoadedValue and call Get() to make the engine build\n")
				w("\t// it, which is the only point at which it costs anything.\n")
				w("\t// Get() returns a tier-2 Value; for the type above that is a\n")
				w("\t// map whose values are Objects.\n")
				w("\t//\n")
				w("\t// It is valid ONLY during this dispatch. Retaining it gives a\n")
				w("\t// live handle over a dead LuaObject, which the next call\n")
				w("\t// reports as ErrInvalid.\n")
			}
			// WHICH HANDLE, AND WHAT THE DESCRIPTION OFFERED INSTEAD. A shape-B
			// union resolves to its class arm, so this field is an Object where
			// the API says SpaceLocationID -- and every other Object in the
			// package type-checks against it. See collapseddoc.go.
			if c := g.collapsed[name+"."+p.Name]; c != nil {
				for _, l := range CollapsedUnionLines(exportName(p.Name), *c, 72) {
					w("\t// %s\n", goDocText(l))
				}
			}
			w("\t%s %s\n", exportName(p.Name), t)
		}
		w("}\n\n")

		w("func (v %s) encodeAt(p *byte) {\n\td := unsafe.Slice(p, %d)\n", name, blk.Size)
		w("\tfor i := range d {\n\t\td[i] = 0\n\t}\n")
		for _, p := range blk.Fields {
			fn := exportName(p.Name)
			if e, ok := g.elem[name+"."+p.Name]; ok && p.Kind == KindArray {
				// A loop, not an expression. The buffer comes from fkAlloc and
				// is released by the allocMark the CALLING member opened --
				// encodeAt is only ever reached from one, so it does not need
				// a bracket of its own.
				if p.HasOffset >= 0 {
					w("\tif v.%s != nil {\n\t\td[%d] = 1\n\t}\n", fn, p.HasOffset)
				}
				w("\tp%s := fkAlloc(uint32(len(v.%s)) * %d)\n", fn, fn, e.stride)
				w("\tfor i := range v.%s {\n", fn)
				w("\t\te := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p%s)+uintptr(i)*%d)), %d)\n",
					fn, e.stride, e.stride)
				store := goStoreElem("e", e.off, e.kind, e.ctn, fmt.Sprintf("v.%s[i]", fn))
				if e.kind == KindStruct {
					store = fmt.Sprintf("v.%s[i].encodeAt(&e[%d])", fn, e.off)
				}
				w("\t\t%s\n\t}\n", store)
				w("\t*(*uint32)(unsafe.Pointer(&d[%d])) = p%s\n", p.Offset, fn)
				w("\t*(*uint32)(unsafe.Pointer(&d[%d])) = uint32(len(v.%s))\n", p.Offset+4, fn)
				continue
			}
			if e, ok := g.dict[name+"."+p.Name]; ok && p.Kind == KindDict {
				// The same walk as the array above, over PAIRS. Only what is
				// written into each element differs, which is the observation
				// that lets fk_abi.lua share one decoder between the two.
				if p.HasOffset >= 0 {
					w("\tif v.%s != nil {\n\t\td[%d] = 1\n\t}\n", fn, p.HasOffset)
				}
				w("\tp%s := fkAlloc(uint32(len(v.%s)) * %d)\n", fn, fn, e.stride)
				w("\tfor i := range v.%s {\n", fn)
				w("\t\te := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p%s)+uintptr(i)*%d)), %d)\n",
					fn, e.stride, e.stride)
				w("\t\t%s\n", goStore("e", e.keyOff, e.keyKind, fmt.Sprintf("v.%s[i].Key", fn)))
				if e.valKind == KindStruct {
					w("\t\tv.%s[i].Val.encodeAt(&e[%d])\n", fn, e.valOff)
				} else {
					w("\t\t%s\n", goStoreElem("e", e.valOff, e.valKind, e.valCtn,
						fmt.Sprintf("v.%s[i].Val", fn)))
				}
				w("\t}\n")
				w("\t*(*uint32)(unsafe.Pointer(&d[%d])) = p%s\n", p.Offset, fn)
				w("\t*(*uint32)(unsafe.Pointer(&d[%d])) = uint32(len(v.%s))\n", p.Offset+4, fn)
				continue
			}
			if p.HasOffset >= 0 {
				w("\tif v.%s != nil {\n\t\td[%d] = 1\n", fn, p.HasOffset)
				w("\t\t%s\n\t}\n", g.storeField(name, "d", p, "(*v."+fn+")"))
				continue
			}
			w("\t%s\n", g.storeField(name, "d", p, "v."+fn))
		}
		w("}\n\n")

		// A table concept with no fields exists in the API, and declaring the
		// slice for one leaves `d` unused -- which Go rejects. The encoder is
		// safe because its zeroing loop always reads it.
		w("func decode%s(p *byte) %s {\n\tvar v %s\n", name, name, name)
		if len(blk.Fields) > 0 {
			w("\td := unsafe.Slice(p, %d)\n", blk.Size)
		} else {
			w("\t_ = p\n")
		}
		for _, p := range blk.Fields {
			fn := exportName(p.Name)
			if e, ok := g.elem[name+"."+p.Name]; ok && p.Kind == KindArray {
				// An optional array stays a slice: nil says absent, so the
				// presence byte needs no pointer to express it.
				if p.HasOffset >= 0 {
					w("\tif d[%d] != 0 {\n", p.HasOffset)
				}
				w("\t{\n")
				w("\t\tbase := uintptr(*(*uint32)(unsafe.Pointer(&d[%d])))\n", p.Offset)
				w("\t\tn := int(*(*uint32)(unsafe.Pointer(&d[%d])))\n", p.Offset+4)
				w("\t\tv.%s = make([]%s, n)\n", fn, e.goType)
				w("\t\tfor i := 0; i < n; i++ {\n")
				w("\t\t\te := unsafe.Slice((*byte)(unsafe.Pointer(base+uintptr(i)*%d)), %d)\n",
					e.stride, e.stride)
				load := goLoadElem("e", e.off, e.kind, e.goType, e.ctn)
				w("\t\t\tv.%s[i] = %s\n\t\t}\n\t}\n", fn, load)
				if p.HasOffset >= 0 {
					w("\t}\n")
				}
				continue
			}
			if e, ok := g.dict[name+"."+p.Name]; ok && p.Kind == KindDict {
				if p.HasOffset >= 0 {
					w("\tif d[%d] != 0 {\n", p.HasOffset)
				}
				w("\t{\n")
				w("\t\tbase := uintptr(*(*uint32)(unsafe.Pointer(&d[%d])))\n", p.Offset)
				w("\t\tn := int(*(*uint32)(unsafe.Pointer(&d[%d])))\n", p.Offset+4)
				w("\t\tv.%s = make([]%s, n)\n", fn, e.entryType)
				w("\t\tfor i := 0; i < n; i++ {\n")
				w("\t\t\te := unsafe.Slice((*byte)(unsafe.Pointer(base+uintptr(i)*%d)), %d)\n",
					e.stride, e.stride)
				val := goLoadElem("e", e.valOff, e.valKind, e.valType, e.valCtn)
				w("\t\t\tv.%s[i] = %s{Key: %s, Val: %s}\n",
					fn, e.entryType, goLoad("e", e.keyOff, e.keyKind), val)
				w("\t\t}\n\t}\n")
				if p.HasOffset >= 0 {
					w("\t}\n")
				}
				continue
			}
			if p.HasOffset >= 0 {
				w("\tif d[%d] != 0 {\n\t\tx := %s\n\t\tv.%s = &x\n\t}\n",
					p.HasOffset, g.loadField(name, "d", p), fn)
				continue
			}
			w("\tv.%s = %s\n", fn, g.loadField(name, "d", p))
		}
		w("\treturn v\n}\n\n")

		g.emitToValue(w, name, blk)
		g.emitDynReaders(w, name, blk)
	}
}

// emitDynReaders writes typed readers over a struct whose whole content is ONE
// tier-2 value.
//
// THE SHAPE IS THE RULE, and the shape is ModSetting: a described table concept
// with exactly one mandatory field whose type is a union, so the field crosses
// as a Value and the struct is a box around a tagged union. A guest reading a
// mod setting therefore switched on a tag to learn whether its own boolean
// setting was on, which is the thing bindings exist to spare it.
//
// A RULE RATHER THAN A NAME, for this repo's usual reason: an allowlist keyed
// on "ModSetting" would be a decision nobody re-reads, and the same shape
// arriving at a later pin under another name would silently get nothing.
// DynValueStructs counts what the rule matched, so the census carries the
// number rather than this comment carrying a claim.
//
// The readers delegate to the Value accessors and are named after them, minus
// the As- prefix that only exists there to avoid colliding with Value's own
// fields. Str rather than String, because a String method with a non-standard
// signature is what go vet's printf checker complains about.
func (g *goStructs) emitDynReaders(w func(string, ...any), name string, blk StructBlock) {
	if !IsDynValueStruct(blk) {
		return
	}
	g.dynValue++
	fn := exportName(blk.Fields[0].Name)
	w("\n// Bool, Num, Str and Obj read the one tier-2 value %s carries,\n", name)
	w("// with the ok false for every other tag -- the same contract\n")
	w("// Value's own As- readers have, which is what these delegate to.\n")
	for _, m := range [][2]string{
		{"Bool", "AsBool"}, {"Num", "AsNum"}, {"Str", "AsStr"}, {"Obj", "AsObj"},
	} {
		var ret string
		switch m[0] {
		case "Bool":
			ret = "bool"
		case "Num":
			ret = "float64"
		case "Str":
			ret = "string"
		default:
			ret = "Object"
		}
		w("func (v %s) %s() (%s, bool) { return v.%s.%s() }\n", name, m[0], ret, fn, m[1])
	}
}

// emitToValue writes the tier-2 constructor for one generated struct.
//
// WHY IT EXISTS. A UNION-typed struct FIELD has no generated type -- a union
// has no fixed layout, so it crosses as a tier-2 Value -- and what a guest is
// therefore handed is a `Value` where the API says `SignalFilter`. The generator
// HAS a struct for `SignalID`; it emits one, four thousand lines away. So the
// mod author writes the Lua table out by hand:
//
//	Value: fkapi.OfMap(
//	    fkapi.KeyValue{Key: fkapi.OfString("type"), Val: fkapi.OfString("virtual")},
//	    fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString(n)},
//	)
//
// -- three key names as string literals, checked by nothing, where a typo is a
// filter Factorio silently rejects rather than a compile error. That is the one
// thing bindings exist to prevent. The downstream ports filed it four times
// (fklua-ports-samples): fuel-train-stop (FTS3, WaitCondition.condition),
// resource-marker (MineableProperties.Products, EntitySearchFilters.Name),
// nixie-tubes (ScriptRenderTarget), and a fourth on LogisticFilter.value.
//
// WHAT IT IS NOT. It is not a typed union -- there are 52 structural unions and
// generating a tagged type per union is where a generator drowns, which is the
// bet tier 2 was made to avoid and it still stands. This is the OTHER half of
// the pain, and the ports were explicit that WRITING is where it hurts: the
// typed struct already exists, so the missing piece was a way to spend it.
//
//	Value: fkapi.SignalID{Type: &kind, Name: &n}.ToValue()
//
// Every generated struct gets one, mechanically, because the generator already
// knows every field's kind and the marginal cost of the general case over the
// four the ports hit is nothing. An absent optional is OMITTED from the table
// rather than sent as nil, which is what an absent optional means everywhere
// else in this ABI.
func (g *goStructs) emitToValue(w func(string, ...any), name string, blk StructBlock) {
	w("// ToValue renders %s as the tier-2 table the engine expects, so a\n", name)
	w("// union-typed field can be filled from the typed struct instead of from\n")
	w("// hand-written key strings. An absent optional is omitted.\n")
	w("func (v %s) ToValue() Value {\n", name)
	if len(blk.Fields) == 0 {
		w("\treturn OfMap()\n}\n\n")
		return
	}
	w("\tkv := make([]KeyValue, 0, %d)\n", len(blk.Fields))
	for _, p := range blk.Fields {
		fn := exportName(p.Name)
		switch p.Kind {
		case KindArray:
			// A LOOP RATHER THAN A GENERIC HELPER, for the reason the Into
			// variant gives: generics in a package this size, compiled by TinyGo
			// for every guest, is a dependency worth not taking for four lines.
			e := g.elem[name+"."+p.Name]
			w("\tif v.%s != nil {\n", fn)
			w("\t\ta := make([]Value, len(v.%s))\n", fn)
			w("\t\tfor i := range v.%s {\n", fn)
			w("\t\t\ta[i] = %s\n", g.goValueElem(e.kind, e.ctn, fmt.Sprintf("v.%s[i]", fn)))
			w("\t\t}\n")
			w("\t\tkv = append(kv, KeyValue{Key: OfString(%q), Val: OfArray(a...)})\n\t}\n", p.Name)
		case KindDict:
			e := g.dict[name+"."+p.Name]
			w("\tif v.%s != nil {\n", fn)
			w("\t\tm := make([]KeyValue, len(v.%s))\n", fn)
			w("\t\tfor i := range v.%s {\n", fn)
			w("\t\t\tm[i] = KeyValue{Key: %s, Val: %s}\n",
				g.valueOfElem(e.keyKind, fmt.Sprintf("v.%s[i].Key", fn)),
				g.goValueElem(e.valKind, e.valCtn, fmt.Sprintf("v.%s[i].Val", fn)))
			w("\t\t}\n")
			w("\t\tkv = append(kv, KeyValue{Key: OfString(%q), Val: OfMap(m...)})\n\t}\n", p.Name)
		default:
			if p.HasOffset >= 0 {
				w("\tif v.%s != nil {\n", fn)
				w("\t\tkv = append(kv, KeyValue{Key: OfString(%q), Val: %s})\n\t}\n",
					p.Name, g.valueOfElem(p.Kind, "(*v."+fn+")"))
				continue
			}
			w("\tkv = append(kv, KeyValue{Key: OfString(%q), Val: %s})\n",
				p.Name, g.valueOfElem(p.Kind, "v."+fn))
		}
	}
	w("\treturn OfMap(kv...)\n}\n\n")
}

// valueOfElem renders one value as a tier-2 Value.
func (g *goStructs) valueOfElem(k Kind, acc string) string {
	switch k {
	case KindStruct:
		return acc + ".ToValue()"
	case KindString:
		return "OfString(" + acc + ")"
	case KindBool:
		return "OfBool(" + acc + ")"
	case KindHandle:
		return "OfObject(" + acc + ")"
	case KindDyn:
		return acc
	}
	return "OfNumber(float64(" + acc + "))"
}

func (g *goStructs) goFieldType(owner string, p Placed) string {
	if p.Kind == KindDict {
		if e, ok := g.dict[owner+"."+p.Name]; ok {
			return "[]" + e.entryType
		}
		return "[]struct{}"
	}
	if p.Kind == KindArray {
		if e, ok := g.elem[owner+"."+p.Name]; ok {
			return "[]" + e.goType
		}
		return "[]struct{}"
	}
	if p.Kind == KindStruct {
		if n, ok := g.fieldType[owner+"."+p.Name]; ok {
			return n
		}
		return "struct{}"
	}
	t, _ := goScalar(p.Kind)
	return t
}

func (g *goStructs) storeField(owner, buf string, p Placed, v string) string {
	if p.Kind == KindStruct {
		return fmt.Sprintf("%s.encodeAt(&%s[%d])", v, buf, p.Offset)
	}
	return goStore(buf, p.Offset, p.Kind, v)
}

func (g *goStructs) loadField(owner, buf string, p Placed) string {
	if p.Kind == KindStruct {
		return fmt.Sprintf("decode%s(&%s[%d])", g.goFieldType(owner, p), buf, p.Offset)
	}
	return goLoad(buf, p.Offset, p.Kind)
}
