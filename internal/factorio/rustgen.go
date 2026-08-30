package factorio

import (
	"fmt"
	"sort"
	"strings"
)

// Generating the guest-side Rust bindings.
//
// A SECOND BACKEND OVER THE SAME REPORT, which is the point of doing it at all.
// The member ids, the wire layouts, the marshalling tiers and the census are
// shared with the Go generator; only the rendering differs. If that turned out
// to need new analysis, the analysis had been Go-shaped all along.
//
// Where Rust's type system is better, the binding says so rather than
// transliterating the Go one:
//
//   - Result<T, Status> instead of (T, error). A caller cannot read the value
//     without handling the status.
//   - Option<T> instead of a pointer, for an optional. Absent is a type, not a
//     nullable.
//   - BTreeMap for a dictionary rather than HashMap. ORDERED, and therefore
//     deterministic -- which matters in a lockstep game and is exactly what
//     forced the Go side to refuse dictionary ARGUMENTS. Rust can express what
//     Go could not, and since the dictionary-inside-a-struct branch landed
//     that is cashed rather than claimed: a `tags` field is a BTreeMap here and
//     an ordered pair SLICE in Go, and both are deterministic for different
//     reasons. Where the key cannot be Ord -- tier 2 holds an f64 and a Vec --
//     it is a Vec of pairs, which is the same shape Value::Map already had.
//   - Value as an enum rather than a tagged struct with six dead fields. This
//     is the real test of whether the shared machinery was Go-shaped, and it
//     was not: the same Kind and the same offsets drive both.

// RustBindings is a generated Rust module plus what it could not cover.
type RustBindings struct {
	Source            string
	Emitted, Deferred int
	DeferredBy        map[string]int
	// LiteralsDeferred counts the string-literal CONSTANTS that got no name --
	// not members, and so not in Deferred. See GoBindings.LiteralsDeferred: the
	// two backends had the identical off-by-one for the identical reason, which
	// is why both census rows read one more than the host member table.
	LiteralsDeferred int
	LiteralDeferBy   map[string]int
	// Names maps "Class::member/kind" to the Rust method it became.
	Names map[string]string
	// IntoVariants counts the `<name>_into(dst, ...)` bindings emitted for
	// members returning a container. Separate from Emitted, which counts
	// MEMBERS bound: this is a second binding over one already counted.
	IntoVariants int
	// TypedVariants is gogen.go's, mirrored: the `<name>_typed` bindings over a
	// member whose parameter table is a discriminated union.
	TypedVariants int
	// DynValueStructs is gogen.go's, mirrored: the generated structs that are a
	// box around one tier-2 value and got typed readers. Counted in both
	// because two backends walking one Report is exactly the assumption AD5
	// disproved -- the equality is a test rather than a construction.
	DynValueStructs int
	// Collisions and StaleRenames are gogen.go's, mirrored. See there: a name
	// collision is a decision somebody has to take, so the IDENTITY is recorded
	// and not only the count, and a memberRename row that stops describing one
	// is a claim about a member that is not there.
	Collisions   []string
	StaleRenames []string
	// Inherited counts the members a subclass got by FORWARDING to its parent.
	// LuaEntity's position() is LuaControl's, and an inherited member appears in
	// neither the child's method list nor its attribute list.
	Inherited int
	// EventStructs counts the event payloads that got a generated Rust type, so
	// a guest never derives a field offset by hand; EventsDeferred counts the
	// ones this layer cannot express yet, by reason. Kept APART from the member
	// counts: an event is not a member.
	EventStructs   int
	EventsDeferred int
	EventDeferBy   map[string]int
	// Defines counts the generated defines::* accessors. Their VALUES are never
	// generated -- runtime-api.json does not carry them -- so what is emitted is
	// a per-build id and a resolver call.
	Defines int
}

func (r *RustBindings) defer1(why string) {
	r.Deferred++
	if why == "" {
		why = "unstated"
	}
	r.DeferredBy[why]++
}

// deferLiteral records one string-enum CONSTANT that could not be emitted.
func (r *RustBindings) deferLiteral(why string) {
	r.LiteralsDeferred++
	if r.LiteralDeferBy == nil {
		r.LiteralDeferBy = map[string]int{}
	}
	r.LiteralDeferBy[why]++
}

// deferEvent records one event payload this layer cannot express.
func (r *RustBindings) deferEvent(why string) {
	r.EventsDeferred++
	if r.EventDeferBy == nil {
		r.EventDeferBy = map[string]int{}
	}
	r.EventDeferBy[why]++
}

// rustSig is everything a FORWARDER needs to redeclare a member on a subclass:
// the parameter list with types, the same names again as arguments, and the
// return type. The Go backend carries the identical three fields for the
// identical reason -- see gogen.go's goSig.
type rustSig struct {
	Params, Args []string
	RetType      string
}

// rustScalar is the Rust type for a kind needing no generated struct.
func rustScalar(k Kind) (string, bool) {
	switch k {
	case KindBool:
		return "bool", true
	case KindI8:
		return "i8", true
	case KindU8:
		return "u8", true
	case KindI16:
		return "i16", true
	case KindU16:
		return "u16", true
	case KindI32:
		return "i32", true
	case KindU32:
		return "u32", true
	case KindF32:
		return "f32", true
	case KindF64:
		return "f64", true
	case KindU64:
		return "u64", true
	case KindString:
		// LuaStr, NOT String. A Lua string is arbitrary bytes and a Rust String
		// is not; the binding used to reconcile that with from_utf8_lossy, which
		// mangled every non-UTF-8 byte and changed the length. See LuaStr in
		// rustgen_rt.go.
		return "LuaStr", true
	case KindHandle:
		return "Object", true
	case KindDyn:
		return "Value", true
	}
	return "", false
}

// rustName turns an API name into a snake_case Rust identifier, avoiding
// keywords.
//
// Rust is snake_case where Go is CamelCase, and the API is already snake_case,
// so this is mostly a keyword check plus the same non-alphanumeric handling
// exportName does -- fifteen table fields are hyphenated.
var rustKeywords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "crate": true,
	"dyn": true, "else": true, "enum": true, "extern": true, "false": true,
	"fn": true, "for": true, "if": true, "impl": true, "in": true, "let": true,
	"loop": true, "match": true, "mod": true, "move": true, "mut": true,
	"pub": true, "ref": true, "return": true, "self": true, "static": true,
	"struct": true, "super": true, "trait": true, "true": true, "type": true,
	"unsafe": true, "use": true, "where": true, "while": true, "async": true,
	"await": true, "box": true, "final": true, "macro": true, "override": true,
	"priv": true, "try": true, "typeof": true, "unsized": true, "virtual": true,
	"yield": true, "abstract": true, "become": true, "do": true,
}

// rustIdentPart is rustName without the keyword escape or the leading-digit
// fix, for a fragment that is being JOINED into a longer identifier.
//
// A `r#` in the middle of `defines_type_north` would not compile, and a part
// that starts with a digit is harmless once something precedes it.
func rustIdentPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// rustScreamingSnake turns a CamelCase concept name into the SCREAMING_SNAKE a
// Rust constant wants: WaitConditionType -> WAIT_CONDITION_TYPE.
//
// rustIdentPart alone lower-cases without re-splitting, so it produces
// WAITCONDITIONTYPE -- which compiles and is unreadable at exactly the call
// sites these constants exist to make readable.
func rustScreamingSnake(s string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		up := r >= 'A' && r <= 'Z'
		switch {
		case up && prevLower:
			b.WriteByte('_')
			b.WriteRune(r)
		case up:
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		prevLower = (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	}
	return b.String()
}

// rustDedent removes one level of indentation from an emitted member.
//
// Every member body here is written for life inside an `impl` block, and the
// three global functions are not in one. Nothing about correctness turns on it
// -- rustc reads whitespace as separation and nothing else -- but a generated
// file this repo asks people to read beside its Go twin should not have three
// functions floating four spaces to the right of everything else.
//
// It removes exactly four leading spaces and only from lines that have them, so
// a blank line stays blank. There are no raw string literals in generated Rust,
// which is what makes a whole-block dedent safe rather than merely convenient.
func rustDedent(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "    ")
	}
	return strings.Join(lines, "\n")
}

func rustName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "x"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "x" + out
	}
	if rustKeywords[out] {
		// r# is the raw-identifier escape and keeps the API's own name, which
		// beats renaming `type` to `type_` and leaving an author to guess.
		return "r#" + out
	}
	return out
}

// GenerateRust emits the guest-side Rust module for a member table.
func GenerateRust(a *API, r Report, evs EventReport) (RustBindings, error) {
	out := RustBindings{Names: map[string]string{}, DeferredBy: map[string]int{}}
	structs := newRustStructs()
	var b strings.Builder
	// NO BACKTICK MAY REACH THIS BUFFER -- see the same note in gogen.go, and
	// note that the reason is about GO rather than about Rust. Rust is perfectly
	// happy with a backtick in a `///` comment; what cannot survive one is the
	// raw string literal in this package's tests that carries the generated
	// source. `///` prose is written with backticks HERE, deliberately and by
	// hand, because rustdoc renders them -- but a backtick that arrives from
	// runtime-api.json data (a member name, a type name, a description) would
	// close a test's literal, and nothing downstream would notice because the
	// generated crate compiles on its own.
	//
	// TestNoBacktickReachesTheGeneratedSources holds it. It scans the GO
	// backend's output, so the shared source of the hazard -- the description --
	// is covered; a backtick reaching only the Rust rendering would not be, and
	// that is a known and accepted narrowness rather than an oversight.
	w := func(f string, args ...any) { fmt.Fprintf(&b, f, args...) }

	w("// Code generated by fklua from runtime-api.json. DO NOT EDIT.\n//\n")
	w("// Build for wasm32-unknown-unknown; nothing here works elsewhere.\n")
	w("#![allow(dead_code, non_snake_case, clippy::all)]\n\n")
	w("use alloc::borrow::Cow;\nuse alloc::collections::BTreeMap;\n")
	w("use alloc::string::String;\nuse alloc::vec::Vec;\n\n")
	w("%s\n", rustRuntime)

	// THE API PIN STAMP, and it is the Go generator's line for line -- one
	// name, one meaning, two spellings. internal/factorio/pin.go carries the
	// reasoning; `PinExport` is the only place the name is built, in either
	// language, because a checker that mangled differently from a generator
	// would find no stamp and say nothing.
	//
	// It survives out of an rlib nothing references, which is not obvious and
	// was measured rather than assumed: `#[no_mangle]` items are externally
	// reachable, so the release profile's `lto = true` keeps this one while
	// deleting the unused bindings around it.
	w("\n// The API pin stamp: these bindings' ids were assigned over Factorio\n")
	w("// %s's description, and fklua mod reads this export name to prove the\n",
		a.ApplicationVersion)
	w("// member table it packages was generated over that same description. Ids\n")
	w("// are dense sorted indices per version, so a table from any other one\n")
	w("// answers this guest's calls with different members.\n")
	w("//\n")
	w("// EXPORTED RATHER THAN CALLED, because an export is a root: nothing here\n")
	w("// references it, and lto deletes whatever is only defined. The NAME\n")
	w("// carries the version because a wasm result cannot.\n")
	w("#[no_mangle]\n")
	w("pub extern \"C\" fn %s() {}\n", PinExport(a.ApplicationVersion))

	// ...AND THE ABI SIGNATURE, the Go generator's line for line. The pin proves
	// one DESCRIPTION; this proves one GENERATION, and at one pin the ids move
	// whenever the generator grows. See internal/factorio/pin.go.
	w("\n// The ABI signature: a digest of the ID ASSIGNMENT AND LAYOUT these\n")
	w("// bindings were generated with, so fklua mod can say when a wasm built\n")
	w("// against OLDER bindings is being packaged with a fresh member table at\n")
	w("// the same pin. Language-independent: a Go guest generated from this\n")
	w("// description carries the same name.\n")
	w("#[no_mangle]\n")
	w("pub extern \"C\" fn %s() {}\n", SigExport(APISignature(a)))

	byClass := map[string][]Member{}
	var classes []string
	for _, m := range r.Members {
		if _, ok := byClass[m.Class]; !ok {
			classes = append(classes, m.Class)
		}
		byClass[m.Class] = append(byClass[m.Class], m)
	}
	sort.Strings(classes)

	// bound and declared are what the inheritance pass below needs: the
	// signature of every member each class actually bound, and the set of names
	// each class declared itself.
	bound := map[string]map[string]rustSig{}
	declared := map[string]map[string]bool{}

	for _, cls := range classes {
		// GLOBAL FUNCTIONS ARE ON NO CLASS, so there is no handle type and no
		// `impl` -- they are free functions in this module. This branch replaces
		// a `continue` that had been correct for as long as this generator has
		// existed; see MemberGlobalFunc.
		global := cls == ""
		typeName := ""
		if global {
			w("\n// Factorio's three GLOBAL FUNCTIONS, which belong to no class and are\n")
			w("// free functions here for that reason. fk_call's handle operand is\n")
			w("// unread for them and the bindings pass 0.\n\n")
		} else {
			typeName = exportName(cls)
			w("\n/// A handle to a `%s`.\n", cls)
			w("#[derive(Copy, Clone, PartialEq, Eq, Debug)]\n")
			w("pub struct %s(pub Object);\n\n", typeName)
			w("impl %s {\n", typeName)
		}

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
			src, name, sig, why, ok := rustMember(structs, typeName, m)
			if !ok {
				out.defer1(why)
				continue
			}
			if seen[name] {
				// NO structs.taken() CLAUSE HERE, and gogen has one: Rust puts
				// types and values in SEPARATE NAMESPACES, so `pub fn log`
				// beside `pub struct Log` is legal and a check for it could
				// never fire. Go has one package-level namespace and therefore
				// really can collide, which is why the two loops differ here
				// rather than by oversight.
				//
				// AND THE IDENTITY IS RECORDED -- see gogen.go's twin of this
				// branch and memberRename, where the two standing collisions are
				// decided rather than left to emission order.
				out.defer1("Rust" + NameCollision)
				out.Collisions = append(out.Collisions,
					fmt.Sprintf("%s (would be %q)", MemberKey(m), name))
				continue
			}
			if global {
				// The body below is written for life inside an `impl`, which
				// this is not. Cosmetic only, and it keeps the generated file
				// readable beside its Go twin.
				src = rustDedent(src)
			}
			seen[name] = true
			out.Names[MemberKey(m)] = name
			w("%s", src)
			out.Emitted++
			if bound[cls] == nil {
				bound[cls] = map[string]rustSig{}
			}
			bound[cls][name] = sig

			// The destination-vector variant, for a member returning a
			// container. Same member id and same blocks -- only what the guest
			// does with the returned (ptr, count) differs. Counted apart from
			// Emitted, which counts MEMBERS bound.
			//
			// Not asked for a global function: none of the three returns a
			// container, so the variant would report "no" anyway.
			if global {
				continue
			}
			isrc, iname, isig, iok := rustMemberInto(structs, typeName, m)
			if iok && !seen[iname] {
				seen[iname] = true
				w("%s", isrc)
				out.IntoVariants++
				bound[cls][iname] = isig
			}

			// AND THE TYPED-ARGUMENT VARIANT, for a member whose parameter table
			// is a discriminated union. Same member id, same returns, and an
			// argument block that is a tier-1 struct plus one tier-2 slot rather
			// than one tier-2 map.
			tsrc, tname, tsig, tok := rustMemberTyped(structs, typeName, m)
			if tok && !seen[tname] {
				seen[tname] = true
				w("%s", tsrc)
				out.TypedVariants++
				bound[cls][tname] = tsig
			}
		}
		if !global {
			w("}\n")
		}
		out.StaleRenames = append(out.StaleRenames,
			staleRenames(cls, byClass[cls], seen, func(r memberRenameRow) (string, string) {
				return r.WasRust, r.Rust
			})...)
		declared[cls] = seen
	}

	// INHERITED MEMBERS, FORWARDED.
	//
	// 83 of the 156 classes have a parent, and an inherited member appears in
	// NEITHER the child's method list nor its attribute list -- so LuaEntity had
	// no position(), surface(), force() or get_inventory(), every one of which
	// is LuaControl's. Dispatch never cared (it is name-based and the handle
	// decides the object), so the workaround was legal and undiscoverable:
	// LuaControl(e.0).position(). FOUR of the seven ports found this
	// independently, which is what made it the round's first item.
	//
	// A FORWARDER RATHER THAN A Deref IMPL, and that is the whole decision.
	// `impl Deref for LuaEntity { type Target = LuaControl }` would be 79 impls
	// instead of ~1300 methods and would get override precedence for free, since
	// an inherent method always beats one reached through a deref. It is
	// rejected for three reasons, in order of weight: it is the "Deref
	// polymorphism" anti-pattern, and a `*entity` that yields a LuaControl is a
	// semantic claim these handles should not make; it needs #[repr(transparent)]
	// on all 148 handle types plus an unsafe reference cast per class, because
	// Deref must return a REFERENCE and there is no parent value to borrow; and
	// the inherited set would stop being countable, where a forwarder is one
	// number in the census beside the Go one. The generated source is bigger and
	// rustc's DCE removes it again, which is the same trade gogen took.
	//
	// NEAREST ANCESTOR WINS, and a name the class declares itself always wins:
	// an override is a real thing in this API, and two inherent methods of one
	// name on one type is a compile error rather than a silent shadow.
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
					"    #[inline]\n    pub fn %s(&self%s%s) -> %s {\n        %s(self.0).%s(%s)\n    }\n\n",
					n,
					map[bool]string{true: "", false: ", "}[len(sig.Params) == 0],
					strings.Join(sig.Params, ", "), sig.RetType,
					exportName(p), n, strings.Join(sig.Args, ", ")))
				out.Inherited++
			}
		}
		if len(fwd) > 0 {
			w("\n/// `%s` inherits these. The handle decides the object, so dispatch is\n",
				exportName(cls))
			w("/// identical -- only the name had nowhere to live. A second inherent\n")
			w("/// impl block is legal and keeps the class's own members together.\n")
			w("impl %s {\n", exportName(cls))
			for _, f := range fwd {
				w("%s", f)
			}
			w("}\n")
		}
	}

	// EVENT PAYLOAD STRUCTS, registered before the declarations are emitted so
	// they come out with everything else.
	//
	// Until these existed a Rust guest read event fields by casting the pointer
	// fk_on_event was handed and adding HAND-DERIVED BYTE OFFSETS -- this repo's
	// own examples/api did it, with the offsets in a comment, and every Rust port
	// in the campaign carried between twelve and seventeen of them plus a script
	// to re-derive them from the GO bindings, because nothing else could check
	// them. Fields are placed by the API's `order`, so one new optional field
	// upstream shifts everything after it, and being wrong is SILENT: the guest
	// reads a neighbouring handle and quietly does nothing.
	//
	// An event's field list is a struct's field list, so this is the same
	// machinery -- which is also why an event whose payload this layer cannot
	// express is deferred with a reason rather than emitted wrong.
	type eventStruct struct {
		typ, fn string
	}
	var eventStructs []eventStruct
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
		// The reader is named from the EVENT's own snake_case name, not from the
		// struct's: rustName lower-cases but does not re-split, so
		// rustName("OnBuiltEntity") is `onbuiltentity`.
		eventStructs = append(eventStructs, eventStruct{name, "read_" + rustName(e.Name)})
	}

	// ...AND THE ONE HOOK PAYLOAD, which is a struct by the same machinery and
	// is not an event. See gogen.go's twin of this block.
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

	structs.emit(w)
	out.DynValueStructs = structs.dynValue

	// One reader per event. The pointer is into the event scratch buffer, which
	// is the host's and lives for exactly this dispatch, so the decoder copies
	// every string and Vec out of it -- what comes back is the guest's, and it
	// borrows nothing.
	if len(eventStructs) > 0 {
		w("\n/// Event payload readers. `fk_on_event` is handed an id and a pointer;\n")
		w("/// match on the id and call the matching reader.\n")
		for _, es := range eventStructs {
			w("\n/// Decodes an `%s` payload out of the host's event buffer.\n", es.typ)
			w("///\n/// `p` is the pointer `fk_on_event` was handed for THIS event id, and\n")
			w("/// is valid only for the duration of that dispatch. Everything the\n")
			w("/// returned value holds is copied out of it and is the guest's.\n")
			w("pub fn %s(p: u32) -> %s {\n", es.fn, es.typ)
			w("    %s::decode_at(unsafe { core::slice::from_raw_parts(p as *const u8, %d) })\n}\n",
				es.typ, structs.byName[es.typ].Size)
		}
	}
	if confChanged != "" {
		w("\n/// Decodes what `script.on_configuration_changed` handed the hook.\n")
		w("///\n/// `fk_on_configuration_changed` is called with a pointer into the\n")
		w("/// host's event buffer, which lives for exactly this dispatch, so the\n")
		w("/// decoder copies every string and container out of it.\n")
		w("///\n/// A guest that exports `fk_on_configuration_changed` WITHOUT a\n")
		w("/// parameter is unchanged: an extra argument to a wasm function of no\n")
		w("/// parameters is discarded by the generated Lua.\n")
		w("///\n/// `mod_changes` is the one most guests want -- one entry per mod ADDED,\n")
		w("/// REMOVED or moved version, keyed by mod name, with `old_version` None\n")
		w("/// for an addition and `new_version` None for a removal.\n")
		w("pub fn read_configuration_changed_data(p: u32) -> %s {\n", confChanged)
		w("    %s::decode_at(unsafe { core::slice::from_raw_parts(p as *const u8, %d) })\n}\n",
			confChanged, structs.byName[confChanged].Size)
	}
	out.EventStructs = len(eventStructs)

	if len(evs.Events) > 0 {
		w("\n/// Event ids. Per-build, like member ids: regenerated with the table\n")
		w("/// they index, so never write one by hand.\n")
		for _, e := range evs.Events {
			w("pub const EVENT_%s: u32 = %d;\n", strings.ToUpper(rustName(e.Name)), e.ID)
		}
	}

	// FIELD MASK BITS, one per maskable field, for subscribe_masked.
	//
	// The bit is the field's index in the LAID-OUT order, which is the order the
	// host's own table is in -- so these are generated beside the layout for the
	// same reason the event ids are generated beside the table they index. A
	// hand-written `1 << 1` drifts the moment the API pin adds a field, silently
	// and in the direction of masking the wrong one.
	//
	// ONLY OPTIONAL AND CONTAINER FIELDS GET ONE, and the omission is the API. A
	// mandatory scalar has no reading that means "not sent", so masking one would
	// hand the guest a zero it cannot tell from a real value. The host refuses
	// such a bit at subscribe time too -- a guest can compute a mask -- but not
	// offering the constant is what makes the rule discoverable instead of a
	// runtime surprise.
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
			maskLines = append(maskLines, fmt.Sprintf("pub const SKIP_%s_%s: u32 = 1 << %d;\n",
				strings.ToUpper(rustName(e.Name)), strings.ToUpper(rustName(p.Name)), i))
		}
	}
	if len(maskLines) > 0 {
		w("\n/// Field mask bits for `subscribe_masked`, one per field a guest may\n")
		w("/// decline. A masked optional reads as ABSENT and a masked container as\n")
		w("/// EMPTY, and the layout does not move. OR them together.\n")
		for _, l := range maskLines {
			w("%s", l)
		}
	}

	// STRING-LITERAL UNIONS, as `pub const &str`. See StringLiteralUnions in
	// gen.go for why constants rather than an enum: the value has to stay a
	// string, every generated field holding one is a String, and an enum would
	// be a breaking change to every call site for a union the API can extend in
	// a point release. `#[non_exhaustive]` exists to admit exactly that problem;
	// a constant does not have to.
	litTaken := map[string]bool{}
	var litLines []string
	for _, u := range StringLiteralUnions(a) {
		prefix := rustScreamingSnake(u.Name)
		var block []string
		for _, lit := range u.Literals {
			part, ok := LiteralIdent(lit)
			if !ok {
				out.deferLiteral("a string literal with no identifier name")
				continue
			}
			nm := prefix + "_" + rustScreamingSnake(part)
			if litTaken[nm] {
				out.deferLiteral("a string literal whose constant name collides")
				continue
			}
			litTaken[nm] = true
			block = append(block, fmt.Sprintf("pub const %s: &str = %q;\n", nm, lit))
		}
		if len(block) == 0 {
			continue
		}
		litLines = append(litLines, fmt.Sprintf(
			"\n/// `%s`: %d string literals, spelled once, here.\n%s",
			u.Name, len(u.Literals), strings.Join(block, "")))
	}
	if len(litLines) > 0 {
		w("\n/// The API's string enums. A union of nothing but string literals\n")
		w("/// crosses as its string, which is the right wire answer and leaves the\n")
		w("/// 26 names of WaitConditionType nowhere -- so a typo is an engine\n")
		w("/// rejection at runtime, or silence, rather than a compile error.\n")
		w("/// Reported by fklua-ports' fuel-train-stop as FTS2.\n")
		for _, l := range litLines {
			w("%s", l)
		}
	}

	// DEFINES, as accessors rather than constants -- because there IS no
	// constant to generate.
	//
	// runtime-api.json carries a define's NAME and an order and not its value,
	// so nothing here could bake one even if it wanted to; and it would not want
	// to, because a define's number is Factorio's own and is not stable across
	// versions. `defines.train_state` was renumbered between 1.1 and 2.0, which
	// is a port's own reason (fklua-ports' fuel-train-stop, FTS8) for why
	// transcribing one fails SILENTLY. The generated table carries the dotted
	// path, control.lua resolves it at load, and the guest holds the per-build
	// id.
	//
	// THE VALUE IS CACHED ON FIRST USE, and the laziness is what makes the two
	// halves work together. Caching in an initialiser would run the host call
	// whether or not the guest reads the define, so every mod would name every id
	// and the constant scan would prune nothing; caching inside the accessor
	// keeps the call site inside a function rustc deletes when nobody calls it.
	// One host call per define for the life of the mod.
	//
	// TWO STATICS RATHER THAN A ZERO SENTINEL, because a define's value can BE
	// zero -- defines.direction.north is 0 -- so "not resolved yet" needs a flag
	// of its own.
	defs := GenerateDefines(a)
	seenDef := map[string]bool{}
	var defLines []string
	for _, d := range defs.Defines {
		name := "defines"
		for _, part := range strings.Split(d.Path, ".") {
			name += "_" + rustIdentPart(part)
		}
		// Two paths can snake-case to one identifier ("a_b.c" and "a.b_c").
		// Emitting both would not compile, and picking one silently would give a
		// guest the wrong number under the right name.
		if seenDef[name] {
			continue
		}
		seenDef[name] = true
		defLines = append(defLines, fmt.Sprintf(`
/// defines.%s, resolved BY NAME against the running Factorio at load.
/// Its value is not stable across versions and is not in the API description,
/// so there is no constant to write and nothing here bakes one.
pub fn %s() -> u32 {
    static V: AtomicU32 = AtomicU32::new(0);
    static OK: AtomicBool = AtomicBool::new(false);
    if !OK.load(Ordering::Relaxed) {
        V.store(unsafe { fk_define(%d) }, Ordering::Relaxed);
        OK.store(true, Ordering::Relaxed);
    }
    V.load(Ordering::Relaxed)
}
`, d.Path, name, d.ID))
	}
	if len(defLines) > 0 {
		w("\n/// defines.* accessors. See fk.define in agents/abi.md: the value is\n")
		w("/// resolved by name at load and cached here on first use.\n")
		w("use core::sync::atomic::{AtomicBool, AtomicU32, Ordering};\n")
		for _, l := range defLines {
			w("%s", l)
		}
	}
	out.Defines = len(seenDef)

	// The nine globals, at the ABI's fixed 1..9. Consts rather than statics: a
	// handle is a plain u32 and a const needs no initialiser to run, which
	// matters because a wasm reactor has no place to run one.
	w("\n/// The objects a mod starts with. Their handles are fixed by the ABI;\n")
	w("/// everything else is reached by calling something.\n")
	byName := map[string]string{}
	for _, g := range a.GlobalObjects {
		byName[g.Name] = g.Type.Name
	}
	for i, name := range abiGlobalNames {
		typ, ok := byName[name]
		if !ok {
			continue
		}
		w("pub const %s: %s = %s(Object(%d));\n",
			strings.ToUpper(rustName(name)), exportName(typ), exportName(typ), i+1)
	}

	out.Source = b.String()
	return out, nil
}

// rustMember renders one member, or reports why it could not.
func rustMember(g *rustStructs, typeName string, m Member) (src, name string, sig rustSig, why string, ok bool) {
	return rustMemberVariant(g, typeName, m, false, false)
}

// rustMemberInto renders the destination-vector variant for a member returning a
// container, or reports that this member has none.
//
// THE RUST SIGNATURE IS NOT A TRANSLATION OF THE GO ONE, and that is deliberate.
// Go has no out-parameter, so its variant takes `dst []T` and RETURNS the slice
// -- the caller must use the return value, because a grown slice is a different
// header. Rust has `&mut Vec<T>`, which reallocates in place, so the value comes
// back through the parameter and the result is `Result<(), Status>`. Forcing the
// Go shape onto Rust would mean handing a Vec in and out by value for no reason.
//
// `dst.clear()` happens before the call, so every early return -- a failed call,
// an absent optional -- leaves dst empty rather than holding the PREVIOUS call's
// entities. That is the same rule as Go's `dst[:0]` and it exists for the same
// reason: a stale container is a wrong answer that looks like a right one.
func rustMemberInto(g *rustStructs, typeName string, m Member) (src, name string, sig rustSig, ok bool) {
	src, name, sig, _, ok = rustMemberVariant(g, typeName, m, true, false)
	return
}

// rustMemberTyped renders the TYPED-ARGUMENT variant, or reports that this
// member has no second argument list. gogen.go's goMemberTyped is the twin and
// carries the argument; the short form is that this is the ordinary member body
// over a substituted Args and a different import.
func rustMemberTyped(g *rustStructs, typeName string, m Member) (src, name string, sig rustSig, ok bool) {
	if len(m.TypedArgs) == 0 {
		return "", "", rustSig{}, false
	}
	m.Args = m.TypedArgs
	src, name, sig, _, ok = rustMemberVariant(g, typeName, m, false, true)
	return
}

func rustMemberVariant(g *rustStructs, typeName string, m Member, into, typed bool) (src, name string, sig rustSig, why string, ok bool) {
	args, rets, err := m.blocks()
	if err != nil {
		return "", "", sig, "signature has no memory layout", false
	}
	type field struct {
		rsType   string
		off      int
		kind     Kind
		ident    string
		has      int
		elemType string
		elemKind Kind
		elemOff  int
		stride   int
		keyType  string
		keyKind  Kind
		keyOff   int
		// elemCtn is the codec of an element (or dictionary value) that is
		// ITSELF a container. See rustgen_nested.go.
		elemCtn string
	}
	argSpec := namedSpecs(m.Args, "a")
	retSpec := namedSpecs(m.Rets, "r")

	mk := func(f Placed, specs []FieldSpec, i int, fallback, ident string) (field, string, bool) {
		rt, why, okk := rustFieldFor(g, f, specs, i, fallback)
		if !okk {
			return field{}, why, false
		}
		fl := field{rsType: rt, off: f.Offset, kind: f.Kind, ident: ident, has: f.HasOffset}
		switch f.Kind {
		case KindArray:
			et, elem, ctn, why, okk := rustArrayElem(g, f, specs, i, fallback)
			if !okk {
				return field{}, why, false
			}
			fl.elemType, fl.elemKind = et, elem.Kind
			fl.elemOff, fl.stride = elem.Offset, f.Stride
			fl.elemCtn = ctn
		case KindDict:
			kt, vt, key, val, vc, why, okk := rustDictKV(g, f, specs, i, fallback)
			if !okk {
				return field{}, why, false
			}
			fl.keyType, fl.keyKind, fl.keyOff = kt, key.Kind, key.Offset
			fl.elemType, fl.elemKind, fl.elemOff = vt, val.Kind, val.Offset
			fl.stride, fl.elemCtn = f.Stride, vc
		}
		return fl, "", true
	}

	var in, res []field
	for i, f := range args.Fields {
		fl, why, okk := mk(f, argSpec, i, typeName+name0(m)+exportName(f.Name), rustName(f.Name))
		if !okk {
			return "", "", sig, why, false
		}
		if fl.ident == "" || fl.ident == "x" {
			fl.ident = fmt.Sprintf("a%d", i)
		}
		in = append(in, fl)
	}
	for i, f := range rets.Fields {
		fl, why, okk := mk(f, retSpec, i, typeName+name0(m)+"Result", "")
		if !okk {
			return "", "", sig, why, false
		}
		res = append(res, fl)
	}
	// A MEMBER RETURNING SEVERAL VALUES IS A TUPLE, which is what Rust has and
	// Go does not -- so this is one of the places the second backend says the
	// thing rather than transliterating `(T, U, error)`. Thirteen members at
	// this pin; the gate was never marshalling (M.invoke carries four slots and
	// the layout has laid out multi-field return blocks all along) but this
	// function's own single-`res[0]` shape. fklua-ports' nixie-tubes (G1).
	//
	// Result<(A, B), Status> rather than (Result<A>, Result<B>): the status is
	// the CALL's, not any one value's, so there is exactly one of it.

	// The Into variant exists only for an ARRAY return, matching the Go side.
	// Asking for it anywhere else is a "no", not a deferral: the caller asks
	// every member.
	//
	// Not dictionaries, and the reason is `alloc`: `BTreeMap` has no `reserve`,
	// so "reuse the allocation" has no expression there -- clearing one frees
	// its nodes. A `HashMap` would, and `alloc` does not have one. The Go side
	// excludes maps for the mirror-image reason: `make(map[K]V, n)` into an
	// existing map means clearing it key by key.
	if into && !(len(res) == 1 && res[0].kind == KindArray) {
		return "", "", sig, "", false
	}

	name = rustName(m.Name)
	switch {
	case m.Kind == MemberIndex:
		// `get`, not `index`: LuaInventory and LuaGuiElement each declare an
		// ordinary attribute called `index`, so an operator named after itself
		// would lose the name to it -- on LuaInventory, which is the whole of
		// fluid-memory-storage's F-IDX. See gogen.go, same rename, same reason.
		name = "get"
	case m.Kind == MemberIndexSet:
		// `set`, pairing with the `get` above. Bare for the reason gogen.go
		// gives: an attribute-write member is `set_` plus a name and so cannot
		// be this, and nothing else on these classes is called `set`.
		name = "set"
	case m.Kind == MemberLen:
		name = "length"
	case m.Kind == MemberSelf:
		name = "call"
	case m.Kind == MemberSet:
		name = "set_" + name
	case m.Kind == MemberGetHandle, m.Kind == MemberCallHandle:
		// The HANDLE variant of an attribute read, or of a METHOD call. Suffixed
		// rather than given a name of its own so the two sit together in the
		// generated file: see gogen.go, same suffix, same reason.
		name += "_raw"
	case m.Kind == MemberGetEq:
		// `entity.name_is("transport-belt")`, reading as the predicate it is.
		name += "_is"
	}
	// A NAME COLLISION IS A DECISION -- memberRename, and gogen.go's twin of
	// this line. Applied here so `src` and `name` cannot disagree, and only when
	// the computed name really is the one the row says it replaces.
	if r, ok := memberRename[MemberKey(m)]; ok && !into && name == r.WasRust {
		name = r.Rust
	}
	// <name>_typed, beside <name> rather than instead of it: the tier-2 form is
	// what makes these members reachable at all and a guest already using one
	// keeps compiling.
	if typed {
		name += "_typed"
	}
	if into {
		name += "_into"
	}

	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	// An optional is Option<T>, CONTAINERS INCLUDED.
	//
	// They used not to be, on the reasoning that a Vec is already empty-able and
	// Option<Vec<T>> makes a caller unwrap twice to say the same thing. That
	// reasoning holds for a Vec that is only ever the guest's own; it does not
	// hold for one the API declares optional, because Factorio means the two
	// differently -- `LuaSchedule::get_records` returns nil for a train with NO
	// SCHEDULE and an empty array for a train with an empty one, and the old
	// shape answered both with `vec![]`. fklua-ports' fuel-train-stop (FTS1)
	// filed it, and noted that the port survived only because its question
	// happened to have the same answer either way.
	//
	// Go keeps its slice, and that is not an inconsistency: a Go slice is
	// nilable, so `nil` versus `make([]T, 0)` already carries the distinction --
	// what the Go side gained is a doc comment saying so. Rust's Vec has no
	// absent value, so it needs the Option. Each language's own optional-scalar
	// convention, which is the rule this generator has followed since Option<T>
	// replaced a pointer.
	optType := func(f field) string {
		if f.has >= 0 {
			return "Option<" + f.rsType + ">"
		}
		return f.rsType
	}

	// A string ARGUMENT is &str, not LuaStr. The host copies the bytes during
	// the call and never retains them, so taking ownership would make every
	// caller allocate for nothing -- `e.set_name("chest")` against
	// `e.set_name(LuaStr::from("chest"))`. A string RETURN is an owned LuaStr,
	// because the buffer it came from is the host's.
	//
	// AND IT STAYS &str RATHER THAN &[u8], which is the one residual asymmetry
	// with Go's `string` argument. put_str writes a &str's bytes verbatim, so
	// what crosses is byte-exact for anything a &str can hold; what a &str
	// cannot hold is a byte sequence that is not UTF-8, and for that the tier-2
	// path -- `Value::Str(LuaStr::from(bytes))` -- is the escape hatch and is
	// where the binary-carrying members already live (helpers.write_file's data
	// is a LocalisedString, not a string). Widening 1,258 argument positions to
	// &[u8] would cost `e.set_name(b"chest")` at every one of them, and
	// `impl AsRef<[u8]>` would monomorphise the same body once per caller type
	// in a target where code size is Lua the game parses.
	paramType := func(f field) string {
		switch {
		case f.kind == KindString && f.has >= 0:
			return "Option<&str>"
		case f.kind == KindString:
			return "&str"
		// A TIER-2 ARGUMENT BY REFERENCE. Taking it by value forces the caller
		// to give up the tree, so a guest sending on a schedule has to rebuild
		// it -- and rebuilding a `Value::Str` copies the payload, which under
		// the default bump allocator is a per-send leak proportional to bytes
		// sent. By reference the caller keeps it, refills the LuaStr in place
		// with LuaStr::set, and allocates nothing. write_dyn already borrows,
		// and the hand-written surface (add_command, write_dyn_at, remote_call)
		// already took &Value -- this makes the generated half agree.
		case f.kind == KindDyn && f.has >= 0:
			return "Option<&Value>"
		case f.kind == KindDyn:
			return "&Value"
		case f.kind == KindArray && f.elemKind == KindString:
			return "&[&str]"
		case f.kind == KindArray:
			return "&[" + f.elemType + "]"
		case f.kind == KindDict:
			// A DICTIONARY ARGUMENT, by reference. `&BTreeMap` where the key is
			// Ord and `&[(K, V)]` where it is not, which is exactly the pair the
			// RETURN side already builds -- so a guest can hand back what it was
			// handed. This is where Rust's choice of BTreeMap pays: the order is
			// the key order, deterministic without the guest doing anything,
			// which is the property Go had to reach by making every dictionary
			// an ordered pair slice.
			return "&" + f.rsType
		}
		return optType(f)
	}
	// An optional ARRAY argument stays a plain slice -- paramType above says so
	// -- because an empty slice already leaves the presence byte clear, which is
	// the encoder's own rule. Only the RETURN side gained the Option.

	var params, callArgs []string
	if into {
		params = append(params, "dst: &mut "+res[0].rsType)
		callArgs = append(callArgs, "dst")
	}
	for _, f := range in {
		params = append(params, f.ident+": "+paramType(f))
		callArgs = append(callArgs, f.ident)
	}
	ret := "Result<(), Status>"
	switch {
	case len(res) > 0 && !into:
		var rts []string
		for _, f := range res {
			rts = append(rts, optType(f))
		}
		if len(rts) == 1 {
			ret = "Result<" + rts[0] + ", Status>"
		} else {
			ret = "Result<(" + strings.Join(rts, ", ") + "), Status>"
		}
	case into && res[0].has >= 0:
		// The destination variant of an OPTIONAL array. `dst` cannot be
		// Option<Vec<T>> without giving up the reuse that is the variant's whole
		// reason to exist, so the presence comes back as the result instead:
		// false means the API said nothing, and `dst` is empty either way.
		ret = "Result<bool, Status>"
	}
	// What a forwarder on a SUBCLASS needs to redeclare this member. Recorded
	// here rather than re-derived there: the parameter types are the product of
	// three overlapping rules (optType, paramType, the Into destination) and
	// stating them twice is how the two copies drift.
	sig = rustSig{Params: params, Args: callArgs, RetType: ret}
	// THE DESCRIPTION'S OWN PROSE, FIRST. gogen.go's twin carries the argument;
	// what differs here is only that a Rust doc comment does not want the
	// identifier repeated in it, so the sentence stands alone.
	for _, l := range wrapComment(m.Doc, 72) {
		w("    /// %s\n", l)
	}
	if typed {
		w("    /// `%s` with its SHARED parameters spelled out instead of\n", rustName(m.Name))
		w("    /// hand-built as tier-2 keys. Same member, same result; the\n")
		w("    /// variant tail goes in `extra`, whose keys are applied over the\n")
		w("    /// block, and `None` means there is no tail. The block crosses as\n")
		w("    /// a flat struct, which the host reads about 3x faster.\n")
	}
	if into {
		w("    /// `%s` writing into `dst`, reusing its allocation rather than\n", rustName(m.Name))
		w("    /// making a fresh one. `dst` is cleared first, so it is empty on\n")
		w("    /// every error path as well as when the call finds nothing.\n")
	}
	for _, l := range OperatorProse(typeName, m, name) {
		w("    /// %s\n", l)
	}
	if len(res) == 1 && res[0].kind == KindDict && !rustDictOrd(res[0].keyKind) && !into {
		// RM2, from fklua-ports' resource-marker.
		w("    /// Keyed by a UNION, so this is an ordered `Vec` of pairs rather\n")
		w("    /// than a map. WHICH ARM arrives is Lua's choice: the host walks\n")
		w("    /// the table with `pairs()`, and for the engine's own\n")
		w("    /// name-or-index dictionaries -- `game.surfaces`, `game.forces`,\n")
		w("    /// `game.players` -- `pairs()` yields the NAME. Matching on\n")
		w("    /// `Value::Number` finds nothing there, silently.\n")
	}
	// NO `&self` FOR A GLOBAL FUNCTION. `log`, `localised_print` and
	// `table_size` are on no class, so there is nothing to borrow; the handle
	// operand below is a literal 0 for the same reason. The four-space indent
	// stays and the caller dedents the whole block, so this line does not have
	// to know whether it is inside an `impl`.
	if m.Kind == MemberGlobalFunc {
		w("    pub fn %s(%s) -> %s {\n", name, strings.Join(params, ", "), ret)
	} else {
		w("    pub fn %s(&self%s%s) -> %s {\n", name,
			map[bool]string{true: "", false: ", "}[len(params) == 0],
			strings.Join(params, ", "), ret)
	}
	if into {
		w("        dst.clear();\n")
	}

	// Asked in two directions, as in gogen: going out the guest allocates only
	// for the shapes that do not fit the argument block; coming back the host
	// allocates for anything that is not a fixed-width scalar. HostAllocatesFor
	// is a whitelist because the other form of this predicate let string
	// returns leak their pin.
	allocs := false
	for _, f := range in {
		if f.kind == KindArray || f.kind == KindDict || f.kind == KindDyn ||
			f.elemKind == KindDyn || f.keyKind == KindDyn ||
			// KindStruct, and it is HostAllocatesFor's own lesson arriving on
			// the OTHER side of the call. That predicate is a whitelist of
			// fixed-width scalars precisely because the enumerate-what-allocates
			// form drifts from the question, and its comment says why a struct
			// belongs on the allocating side: its fields are encoded one by one,
			// so a struct with a container in it allocates and nothing here can
			// see that without resolving the concept. The going-out loop is
			// still the enumerating form, and KindStruct fell through it exactly
			// as KindString once fell through the other one.
			//
			// Measured at the 2.1.14 pin before this clause: 375 members take a
			// struct argument, 301 of them carried no bracket, and 64 of those
			// have a struct whose encode_at really does call galloc --
			// LuaTrain::set_schedule (TrainSchedule.records: Vec<ScheduleRecord>)
			// is the plainest, and LuaSurface::request_path is the one that says
			// a one-level check would not have been enough, since its container
			// is a field of a field (LuaSurfaceRequestPathArgs.collision_mask
			// .layers). Go never had the hole because its blocks come out of the
			// arena and `args.Size > 0` brackets every member with any argument
			// at all -- coverage by accident, from a clause about something else.
			//
			// FAILS CLOSED, deliberately: this brackets all 375 rather than the
			// 64, because deciding precisely means walking the concept
			// transitively and a walk that is subtly wrong under-brackets, which
			// is the defect being fixed. The 311 over-brackets cost nothing --
			// AllocMark is an empty struct with an empty Drop.
			f.kind == KindStruct {
			allocs = true
		}
	}
	for _, f := range res {
		if HostAllocatesFor(f.kind) {
			allocs = true
		}
	}
	if allocs {
		w("        let _mark = AllocMark::new();\n")
	}
	if args.Size > 0 {
		w("        let mut a = [0u8; %d];\n", args.Size)
	}
	if rets.Size > 0 {
		w("        let mut r = [0u8; %d];\n", rets.Size)
	}

	for _, f := range in {
		switch {
		case f.kind == KindDict:
			// The ARRAY wire, over pairs -- same fk_alloc buffer, same
			// (ptr, count) in the block, one extra store for the key. A
			// BTreeMap and a slice of pairs both `iter()` into `(&k, &v)` and
			// `&(k, v)` respectively, so the destructure differs by one line and
			// nothing else does.
			//
			// Four of the seven members this unblocks are `tags` setters --
			// read_write attributes whose getter generated alone, which
			// fluid-memory-storage reported as F-TAGS.
			if f.has >= 0 {
				w("        a[%d] = 1;\n", f.has)
			}
			w("        let p%s = galloc((%s.len() * %d) as u32);\n", f.ident, f.ident, f.stride)
			if rustDictOrd(f.keyKind) {
				w("        for (i, (k, v)) in %s.iter().enumerate() {\n", f.ident)
			} else {
				w("        for (i, (k, v)) in %s.iter().map(|p| (&p.0, &p.1)).enumerate() {\n", f.ident)
			}
			w("            let d = unsafe { core::slice::from_raw_parts_mut((p%s as usize + i * %d) as *mut u8, %d) };\n",
				f.ident, f.stride, f.stride)
			w("            %s\n", rustStore("d", f.keyOff, f.keyKind, "k", true))
			w("            %s\n        }\n",
				rustStoreElem("d", f.elemOff, f.elemKind, f.elemCtn, "v", true))
			w("        wr_u32(&mut a[..], %d, p%s);\n", f.off, f.ident)
			w("        wr_u32(&mut a[..], %d, %s.len() as u32);\n", f.off+4, f.ident)
		case f.kind == KindArray:
			if f.has >= 0 {
				w("        a[%d] = 1;\n", f.has)
			}
			w("        let p%s = galloc((%s.len() * %d) as u32);\n", f.ident, f.ident, f.stride)
			w("        for (i, e) in %s.iter().enumerate() {\n", f.ident)
			w("            let d = unsafe { core::slice::from_raw_parts_mut((p%s as usize + i * %d) as *mut u8, %d) };\n",
				f.ident, f.stride, f.stride)
			w("            %s\n        }\n",
				rustStoreElem("d", f.elemOff, f.elemKind, f.elemCtn, "e", true))
			w("        wr_u32(&mut a[..], %d, p%s);\n", f.off, f.ident)
			w("        wr_u32(&mut a[..], %d, %s.len() as u32);\n", f.off+4, f.ident)
		case f.has >= 0:
			// byRef for a tier-2 argument, because paramType made it Option<&Value>
			// and `if let Some(v)` binds the reference write_dyn wants. Every other
			// kind arrives by value here.
			w("        if let Some(v) = %s {\n            a[%d] = 1;\n", f.ident, f.has)
			w("            %s\n        }\n", rustStore("a", f.off, f.kind, "v", f.kind == KindDyn))
		default:
			w("        %s\n", rustStore("a", f.off, f.kind, f.ident, f.kind == KindDyn))
		}
	}

	ap, rp := "0", "0"
	if args.Size > 0 {
		ap = "a.as_ptr() as u32"
	}
	if rets.Size > 0 {
		rp = "r.as_mut_ptr() as u32"
	}
	// THE HANDLE, and 0 for a global function -- which every other kind answers
	// ERR_BAD_HANDLE and this one never reads, because M.invoke's GFUNC branch
	// runs before the handle is resolved at all. The constant scan that prunes
	// the shipped member table reads operand 1, not operand 0.
	recv := "self.0.0"
	if m.Kind == MemberGlobalFunc {
		recv = "0"
	}
	call := "fk_call"
	if typed {
		call = "fk_call_typed"
	}
	w("        let st = unsafe { %s(%s, %d, %s, %s) };\n", call, recv, m.ID, ap, rp)
	w("        if st != 0 {\n            return Err(Status(st));\n        }\n")

	// ONE DECODE PER RETURN FIELD, into v0, v1, ... An absent optional must not
	// return early once there can be several: `return Ok(None)` is right for a
	// member with one return and drops the other two on a member with three.
	// So absent leaves the binding at its declared empty value, and every local
	// a container decode needs is suffixed with the field's index.
	var retNames []string
	for i, f := range res {
		v := fmt.Sprintf("v%d", i)
		if !into {
			retNames = append(retNames, v)
		}
		// The container walk, shared by the mandatory and the optional shapes.
		// `acc` is where the elements land: the accumulator's own name where it
		// is bound directly, and `out` where it has to be moved into an Option.
		walk := func(acc string, indent string) {
			w("%slet base%d = rd_u32(&r[..], %d) as usize;\n", indent, i, f.off)
			w("%slet n%d = rd_u32(&r[..], %d) as usize;\n", indent, i, f.off+4)
			switch {
			case into:
				// `dst` is already clear. Reserving against the cleared vector
				// is what makes the reuse real: a Vec that was big enough last
				// call reserves nothing and the loop writes into the allocation
				// it already had.
				w("%slet %s = &mut *dst;\n%s%s.reserve(n%d);\n", indent, acc, indent, acc, i)
			case f.kind == KindArray, !rustDictOrd(f.keyKind):
				w("%slet mut %s = Vec::with_capacity(n%d);\n", indent, acc, i)
			default:
				w("%slet mut %s = BTreeMap::new();\n", indent, acc)
			}
			w("%sfor i in 0..n%d {\n", indent, i)
			w("%s    let d = unsafe { core::slice::from_raw_parts((base%d + i * %d) as *const u8, %d) };\n",
				indent, i, f.stride, f.stride)
			switch {
			case f.kind == KindArray:
				w("%s    %s.push(%s);\n%s}\n", indent, acc,
					rustLoadElem("d", f.elemOff, f.elemKind, f.elemType, f.elemCtn), indent)
			case rustDictOrd(f.keyKind):
				w("%s    %s.insert(%s, %s);\n%s}\n", indent, acc,
					rustLoad("d", f.keyOff, f.keyKind, f.keyType),
					rustLoadElem("d", f.elemOff, f.elemKind, f.elemType, f.elemCtn), indent)
			default:
				// A KEY THAT CANNOT BE Ord, so the pair vector -- which is what
				// binds game.surfaces, game.players and game.forces, keyed by
				// `uint32 | string` and therefore KindDyn.
				w("%s    %s.push((%s, %s));\n%s}\n", indent, acc,
					rustLoad("d", f.keyOff, f.keyKind, f.keyType),
					rustLoadElem("d", f.elemOff, f.elemKind, f.elemType, f.elemCtn), indent)
			}
		}
		switch {
		case (f.kind == KindArray || f.kind == KindDict) && into && f.has >= 0:
			// The destination variant of an optional container: `dst` was
			// cleared above, so absent leaves it empty and the bool carries it.
			w("        let mut %s = false;\n", v)
			w("        if r[%d] != 0 {\n", f.has)
			walk("out", "            ")
			w("            %s = true;\n        }\n", v)
			retNames = append(retNames, v)
		case (f.kind == KindArray || f.kind == KindDict) && into:
			walk("out", "        ")
		case (f.kind == KindArray || f.kind == KindDict) && f.has >= 0:
			w("        let mut %s: %s = None;\n", v, optType(f))
			w("        if r[%d] != 0 {\n", f.has)
			walk("out", "            ")
			w("            %s = Some(out);\n        }\n", v)
		case f.kind == KindArray || f.kind == KindDict:
			walk(v, "        ")
		case f.has >= 0:
			w("        let mut %s: Option<%s> = None;\n", v, f.rsType)
			w("        if r[%d] != 0 {\n            %s = Some(%s);\n        }\n",
				f.has, v, rustLoad("r", f.off, f.kind, f.rsType))
		default:
			w("        let %s = %s;\n", v, rustLoad("r", f.off, f.kind, f.rsType))
		}
	}
	switch {
	case len(retNames) == 0:
		w("        Ok(())\n")
	case len(retNames) == 1:
		w("        Ok(%s)\n", retNames[0])
	default:
		w("        Ok((%s))\n", strings.Join(retNames, ", "))
	}
	w("    }\n\n")
	return b.String(), name, sig, "", true
}

// STATUS: the SAME numbers as the Go backend, member id for member id, with the
// same deferrals under the same reasons -- members, Into variants, inherited
// forwarders, event payload structs and defines accessors alike. THE NUMBERS
// THEMSELVES ARE NOT WRITTEN HERE, and that is the lesson of the paragraph
// below rather than laziness: this comment carried a stale five-count tuple for
// four pins. They live in api/<version>/census.json, which a version bump
// regenerates and a reviewer diffs. The generated crate compiles clean with no
// warnings. `fklua gen-bindings` writes both languages and `--check` checks
// both, and `census.json` carries the Rust rows beside the Go ones.
//
// THAT PARITY IS A 2026-08-03 RESULT, NOT A STANDING ONE, and how it was lost
// is the useful part. This file's own header claimed "the SAME coverage as the
// Go backend with the same 47 deferrals" while the Go backend was at 27 -- true
// when written and false for four milestones, because every feature the Go
// generator grew afterwards (inherited forwarders, event payload structs, a
// dictionary field inside a struct, dyn-keyed dictionary returns, defines
// accessors, the subscribe field mask) was added there and nowhere else. None
// of the six was a thing Rust could not express; four of them Rust expresses
// BETTER. What was missing was a reason to look, and it arrived as seven mod
// ports written outside this repo, four of which independently reported the
// inherited members alone.
//
// The census rows are the standing answer: a Go number and a Rust number in one
// committed file, so a feature added to one backend shows up as a diff nobody
// has to notice.
//
// EXERCISED AT RUNTIME, not merely compiled. guest/rust/examples/array mirrors
// the Go one and TestArraysCrossInBothDirections runs BOTH against the same
// host stub with the same expectations; guest/rust/examples/dict does the same
// for an event payload carrying a nested dictionary. So each is a runtime check
// of the generated bindings and a differential check at once.
//
// That mattered immediately: the imports were emitted as fk.fk_call and
// fk.subscribe rather than fk.call, which a compile gate cannot see. The crate
// built perfectly and the module simply refused to instantiate.
