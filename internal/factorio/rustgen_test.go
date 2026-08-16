package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Rust backend's own gates.
//
// EVERY TEST HERE IS A TWIN OF ONE IN gogen_test.go, and that is the point
// rather than duplication for its own sake: the claim this repo makes about
// having two backends is that they are two RENDERINGS of one analysis, and the
// only way that claim stays true is if the same properties are asserted of
// both. The 2026-08-03 ports round is what proved the cost of not doing it --
// four independent mod ports reported the Rust generator missing inherited
// members, event payload structs, dyn-keyed dictionary returns and defines
// accessors, and one of them found a struct the generator emitted EMPTY, which
// is a defect the Go side had already fixed and had a test for.

func rustBindings(t *testing.T) (RustBindings, *API) {
	t.Helper()
	a := loadTestAPI(t)
	r, err := GenerateRust(a, GenerateMembers(a), GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	return r, a
}

// The committed Rust bindings must match what the generator produces, for the
// same reason the Go ones must: a golden file makes a regeneration a reviewable
// diff, and a stale checkout a build failure rather than a guest author finding
// a method missing.
func TestCommittedRustBindingsAreUpToDate(t *testing.T) {
	r, _ := rustBindings(t)
	path := filepath.Join("..", "..", "guest", "rust", "fkapi", "src", "api.rs")
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v -- run `fklua gen-bindings`", err)
	}
	if string(have) != r.Source {
		t.Errorf("%s is out of date; run `fklua gen-bindings`", path)
	}
	t.Logf("%d members bound, %d deferred, %d inherited, %d event structs, "+
		"%d defines, %d bytes", r.Emitted, r.Deferred, r.Inherited, r.EventStructs,
		r.Defines, len(r.Source))
}

// THE TWO BACKENDS BIND THE SAME MEMBERS, and any row where they disagree is a
// finding rather than a fact about a language.
//
// This is the assertion the census rows now carry, made where it can say WHICH
// member moved. It was 4140 against 4160 and 47 deferrals against 27 until this
// round, and every one of the twenty was a branch the Rust generator had not
// grown rather than something Rust could not express.
func TestBothBackendsBindTheSameMembers(t *testing.T) {
	a := loadTestAPI(t)
	rep, evs := GenerateMembers(a), GenerateEvents(a)
	g, err := GenerateGoWith(a, rep, evs, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	r, err := GenerateRust(a, rep, evs)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what     string
		go_, rs_ int
	}{
		{"members bound", g.Emitted, r.Emitted},
		{"members deferred", g.Deferred, r.Deferred},
		{"string-enum constants deferred", g.LiteralsDeferred, r.LiteralsDeferred},
		{"members inherited", g.Inherited, r.Inherited},
		{"Into variants", g.IntoVariants, r.IntoVariants},
		{"event payload structs", g.EventStructs, r.EventStructs},
		{"event payloads deferred", g.EventsDeferred, r.EventsDeferred},
		{"define accessors", g.Defines, r.Defines},
	} {
		if c.go_ != c.rs_ {
			t.Errorf("%s: Go %d, Rust %d -- one backend grew a branch the other "+
				"did not, which is what the ports round found four times",
				c.what, c.go_, c.rs_)
		}
	}
	// And the same member ids, not merely the same count. A missing member and
	// an extra one cancel in a total.
	for k := range g.Names {
		if _, ok := r.Names[k]; !ok {
			t.Errorf("%s binds in Go and not in Rust", k)
		}
	}
	for k := range r.Names {
		if _, ok := g.Names[k]; !ok {
			t.Errorf("%s binds in Rust and not in Go", k)
		}
	}
}

// A DEFERRED STRUCT MUST NOT BE EMITTED AS AN EMPTY ONE. (fklua-ports, AD5.)
//
// add() reserves the name in the emission order before recursing -- so a type
// reachable from itself does not spin -- and its failure path deleted the entry
// from byName and left it in order. emit() then read a zero StructBlock and
// wrote `pub struct CollisionMask {}` under the concept's real name, with a
// codec that reads and writes ZERO bytes: `CollisionMask::decode_at(&r[..])`
// compiles, runs, and silently returns a default value while sixteen bytes of
// wire sit there unread.
//
// It is the SAME defect the Go generator carried and fixed, in the same
// function, and this backend kept it for two more milestones because the test
// that caught it was written against the other one. A port found it by grepping
// the committed bindings.
func TestADeferredRustStructIsNotEmittedAsAnEmptyType(t *testing.T) {
	r, a := rustBindings(t)
	fieldless := map[string]bool{}
	for _, c := range a.Concepts {
		if c.Type.Complex == "table" && len(c.Type.Parameters) == 0 {
			fieldless[exportName(c.Name)] = true
		}
	}
	re := regexp.MustCompile(`(?m)^pub struct (\w+) \{\n\}`)
	for _, m := range re.FindAllStringSubmatch(r.Source, -1) {
		if fieldless[m[1]] {
			continue
		}
		t.Errorf("%s is emitted with no fields; either the concept really has "+
			"none, or its layout was deferred and the name was left in the "+
			"emission order -- a guest cannot tell the difference", m[1])
	}
	// The two the port named, by name, so a regression says which.
	for _, want := range []string{
		"pub struct CollisionMask {\n    pub layers:",
		"pub struct MapGenSettings {",
	} {
		if !strings.Contains(r.Source, want) {
			t.Errorf("missing from the generated bindings:\n\t%q", want)
		}
	}
}

// ...AND THE REGISTRATION-ORDER FAILURE ITSELF, which the source scan above
// can only catch while some struct is actually deferred.
//
// That is the trap in the test the Go side has: it greps the OUTPUT for an
// empty type, so the day the last deferral closes it starts passing vacuously
// and stops defending the invariant. Both backends' add() now reserve a name in
// the emission order before recursing and both have to remove it again on the
// failure path, and this asks each collector directly with a shape that really
// fails.
//
// THE FIXTURE WAS A DICTIONARY OF A DICTIONARY, and that day arrived: both
// backends recurse into a nested container now, so the shape binds. What is
// still refused is a dictionary keyed BY a container -- a Lua table key that is
// a table -- which is deliberate rather than pending (see gogen_nested.go) and
// is therefore a fixture that will not quietly start binding. It is a shape the
// description does not contain, which is fine HERE precisely because this test
// asks the collector directly instead of grepping the output: a synthetic spec
// is the only way to reach the failure path once the real ones are all bound.
func TestARefusedStructLeavesTheEmissionOrder(t *testing.T) {
	bad := FieldSpec{Name: "Outer", Kind: KindStruct, TypeName: "Outer", Struct: []FieldSpec{
		{Name: "ok", Kind: KindU32},
		{Name: "bad", Kind: KindDict,
			Key:  &FieldSpec{Name: "k", Kind: KindArray, Elem: &FieldSpec{Kind: KindU32}},
			Elem: &FieldSpec{Name: "v", Kind: KindU32}},
	}}
	// The shape has to LAY OUT and be REFUSED by the collector; a spec the
	// layout rejects would exercise a different branch entirely.
	if _, err := LayoutStruct(bad.Struct); err != nil {
		t.Fatalf("the fixture no longer lays out, so it is testing the wrong "+
			"branch: %v", err)
	}

	t.Run("rust", func(t *testing.T) {
		g := newRustStructs()
		if _, why, ok := g.add(bad, "Outer"); ok {
			t.Fatal("the collector accepted a dictionary keyed by a container")
		} else {
			t.Logf("refused: %s", why)
		}
		var b strings.Builder
		g.emit(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) })
		if strings.Contains(b.String(), "pub struct Outer") {
			t.Errorf("a refused struct was emitted anyway, with a codec that "+
				"reads and writes zero bytes:\n%s", b.String())
		}
		if !g.taken("Outer") {
			t.Error("the name is not recorded as blocked, so a second reference " +
				"would try again and get a different answer")
		}
	})
	t.Run("go", func(t *testing.T) {
		g := newGoStructs()
		if _, _, ok := g.add(bad, "Outer"); ok {
			t.Fatal("the collector accepted a dictionary keyed by a container")
		}
		var b strings.Builder
		g.emit(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) })
		if strings.Contains(b.String(), "type Outer struct") {
			t.Errorf("a refused struct was emitted anyway:\n%s", b.String())
		}
	})
}

// AN INHERITED MEMBER HAS TO BE REACHABLE UNDER THE CHILD'S NAME. (R1, found
// independently by four ports.)
//
// 83 of the 156 classes have a parent, and an inherited member appears in
// NEITHER the child's method list nor its attribute list -- so LuaEntity had no
// position(), no surface(), no force() and no get_inventory(), all of which are
// LuaControl's. Dispatch never cared, because it is name-based and the handle
// decides the object, which is what made the workaround legal and
// undiscoverable at once: LuaControl(e.0).position().
func TestARustSubclassReachesItsParentsMembers(t *testing.T) {
	r, _ := rustBindings(t)
	for _, want := range []string{
		"pub fn position(&self) -> Result<MapPosition, Status> {",
		"LuaControl(self.0).position()",
		"pub fn surface_index(&self) -> Result<u32, Status> {",
		"LuaControl(self.0).get_inventory(",
	} {
		if !strings.Contains(r.Source, want) {
			t.Errorf("missing from the generated bindings:\n\t%s", want)
		}
	}
	if r.Inherited == 0 {
		t.Error("no member was forwarded at all")
	}
	// The forwarder must be on the CHILD, so `LuaEntity(h).position()` compiles.
	// Finding the call expression anywhere is not enough -- it is in every
	// forwarder of every descendant.
	i := strings.Index(r.Source, "/// `LuaEntity` inherits these.")
	if i < 0 {
		t.Fatal("LuaEntity got no inherited-member impl block")
	}
	if !strings.Contains(r.Source[i:i+400], "impl LuaEntity {") {
		t.Error("the LuaEntity forwarders are not in an impl LuaEntity block")
	}
}

// A forwarder must never SHADOW a member the class declares itself, and in Rust
// the consequence is sharper than in Go: two inherent methods of one name on
// one type is a compile error, so this would take the whole crate down rather
// than quietly picking one.
//
// The parse gate cannot say this -- there is no Rust parser here -- and the
// cargo build would report it as E0592 a long way from the cause.
func TestARustForwarderNeverShadowsTheClassesOwnMember(t *testing.T) {
	r, _ := rustBindings(t)
	// Walk the source tracking which type each `impl X {` block belongs to, so
	// a name is only a duplicate within one type.
	implRe := regexp.MustCompile(`(?m)^impl (\w+) \{`)
	fnRe := regexp.MustCompile(`(?m)^    pub fn (\w+)\(`)
	type span struct {
		typ string
		at  int
	}
	var spans []span
	for _, m := range implRe.FindAllStringSubmatchIndex(r.Source, -1) {
		spans = append(spans, span{r.Source[m[2]:m[3]], m[0]})
	}
	typeAt := func(pos int) string {
		lo, hi := 0, len(spans)
		for lo < hi {
			mid := (lo + hi) / 2
			if spans[mid].at <= pos {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo == 0 {
			return ""
		}
		return spans[lo-1].typ
	}
	seen := map[string]bool{}
	for _, m := range fnRe.FindAllStringSubmatchIndex(r.Source, -1) {
		key := typeAt(m[0]) + "::" + r.Source[m[2]:m[3]]
		if seen[key] {
			t.Errorf("%s is declared twice; a forwarder shadowed a real member, "+
				"which in Rust is E0592 rather than a silent choice", key)
		}
		seen[key] = true
	}
}

// EVENT PAYLOADS GET A GENERATED STRUCT AND A READER. (R2, found by three
// ports; between them they carried 45 hand-derived offsets and two scripts to
// re-derive them from the GO bindings, because nothing else could check them.)
//
// Fields are placed by the API's `order`, not by the order the JSON lists them,
// and an optional's presence byte shifts everything after it -- so
// on_built_entity puts `entity` at 0 and `name` at 24 while script_raised_built
// puts `name` at 4. Being wrong is silent: the guest reads a neighbouring
// handle and quietly does nothing.
func TestRustEventPayloadsGetAStructAndAReader(t *testing.T) {
	r, _ := rustBindings(t)
	if r.EventsDeferred != 0 {
		t.Errorf("%d event payloads deferred, by reason %v", r.EventsDeferred, r.EventDeferBy)
	}
	if r.EventStructs == 0 {
		t.Fatal("no event payload struct was generated at all")
	}
	for _, want := range []string{
		"pub fn read_on_built_entity(p: u32) -> OnBuiltEntity {",
		"pub fn read_on_robot_built_entity(p: u32) -> OnRobotBuiltEntity {",
		"pub fn read_on_entity_died(p: u32) -> OnEntityDied {",
	} {
		if !strings.Contains(r.Source, want) {
			t.Errorf("the generated bindings have no %s", want)
		}
	}
	// The reader is named off the EVENT's snake_case name. rustName lower-cases
	// but does not re-split, so naming it from the struct would produce
	// `read_onbuiltentity` -- which compiles, and is not the name anybody looks
	// for.
	if strings.Contains(r.Source, "read_onbuiltentity") {
		t.Error("a reader was named from the struct rather than from the event")
	}

	// THE FIELD MASK BITS, which need the third subscribe argument the Rust
	// import did not declare. The ABI has carried it since the mask landed;
	// this backend's import took two parameters, so a Rust guest could not
	// decline on_undo_applied's `actions` however expensive it was.
	for _, want := range []string{
		"pub const SKIP_ON_UNDO_APPLIED_ACTIONS: u32 = 1 << 1;",
		"pub const SKIP_ON_BUILT_ENTITY_TAGS: u32 = 1 << 3;",
		"fn fk_subscribe(event: u32, filterp: u32, skip: u32) -> u32;",
		"pub fn subscribe_masked(event: u32, skip: u32) -> Status {",
		"pub fn subscribe_filtered_masked(event: u32, skip: u32, filters: &[Value]) -> Status {",
	} {
		if !strings.Contains(r.Source, want) {
			t.Errorf("the generated bindings have no %s", want)
		}
	}
	// A MANDATORY SCALAR HAS NO BIT, and the omission is the safety property: a
	// masked optional reads as ABSENT and a masked container as EMPTY, both of
	// which the decoder already produces, while a masked mandatory scalar would
	// be a zero the guest cannot tell from a real value.
	if strings.Contains(r.Source, "SKIP_ON_BUILT_ENTITY_ENTITY") {
		t.Error("a mandatory handle field was offered a mask bit")
	}
}

// A DICTIONARY FIELD INSIDE A STRUCT. (AD4: 17 of the 47 Rust deferrals were
// this one missing branch, `LuaEntityPrototype::collision_mask` among them.)
//
// A top-level dictionary RETURN rendered fine as a BTreeMap, so only the
// NESTING refused -- which is what made it look small and made it worth 17.
func TestADictionaryFieldInsideARustStructGenerates(t *testing.T) {
	r, _ := rustBindings(t)
	if n := r.DeferredBy["returns or takes a dictionary"]; n != 0 {
		t.Errorf("%d members still defer on a dictionary field inside a struct", n)
	}
	for _, gone := range []string{
		"struct CollisionMask is itself deferred",
		"struct MapGenSettings is itself deferred",
	} {
		if n := r.DeferredBy[gone]; n != 0 {
			t.Errorf("%d members still blocked by %q", n, gone)
		}
	}
	i := strings.Index(r.Source, "pub struct OnBuiltEntity {")
	if i < 0 {
		t.Fatal("no OnBuiltEntity struct")
	}
	decl := r.Source[i : i+400]
	// A BTreeMap, not a Vec of pairs, because the KEY here is a LuaStr and
	// therefore Ord -- which is the one place this backend is straightforwardly
	// better than the Go one. A struct field crosses in BOTH directions, and a
	// BTreeMap iterates in key order, so its wire order is deterministic by
	// construction where a Go map's is randomized. LuaStr rather than String
	// since the byte-exactness fix, and the order is the same: both compare
	// byte-lexicographically over the same bytes.
	if !regexp.MustCompile(`tags: BTreeMap<LuaStr, Value>`).MatchString(decl) {
		t.Errorf("OnBuiltEntity.tags is not an ordered map:\n%s", decl)
	}
}

// A DICTIONARY RETURN KEYED BY A TIER-2 VALUE BINDS. (R3, found by three
// ports; two of them walked get_surface(1), get_surface(2), ... instead and had
// to choose how many consecutive misses meant "stop".)
//
// `game.surfaces` is dictionary[uint32 | string -> LuaSurface], so the key is a
// union and therefore KindDyn. Value holds an f64 and a Vec, so it is neither
// Ord nor Hash and cannot key a BTreeMap -- but a Vec of pairs has no such
// requirement, which is what turned the refusal into a container choice.
func TestARustDictionaryKeyedByADynamicValueBinds(t *testing.T) {
	r, _ := rustBindings(t)
	if n := r.DeferredBy["a dictionary keyed by a value that is not Ord"]; n != 0 {
		t.Errorf("%d members still defer on a non-Ord dictionary key", n)
	}
	name, ok := r.Names[fmt.Sprintf("LuaGameScript::surfaces/%d", MemberGet)]
	if !ok {
		t.Fatal("game.surfaces did not bind at all")
	}
	i := strings.Index(r.Source, "pub fn "+name+"(&self) ->")
	if i < 0 {
		t.Fatalf("no %s method in the generated source", name)
	}
	decl := r.Source[i : i+700]
	if !strings.Contains(decl, "Result<Vec<(Value, Object)>, Status>") {
		t.Errorf("%s does not return an ordered pair vector:\n%s", name, decl)
	}
	if strings.Contains(decl, "BTreeMap::new()") {
		t.Errorf("%s builds a BTreeMap, whose key would have to be Ord:\n%s", name, decl)
	}
	// The pair's stride is the thing no compiler checks: the value sits at the
	// key's PADDED size, so a (dyn, handle) pair strides by 24 and its value is
	// at 16 -- exactly the numbers the Go side pins.
	if !strings.Contains(decl, "* 24) as *const u8, 24)") ||
		!strings.Contains(decl, "Object(rd_u32(&d[..], 16))") {
		t.Errorf("%s does not read a (dyn, handle) pair at stride 24 / value 16:\n%s",
			name, decl)
	}
	// A COMPARABLE-KEYED RETURN KEEPS THE MAP, and the asymmetry is a fact
	// about Ord rather than a preference: that one is decode-only, and a
	// BTreeMap buys lookup at no determinism cost.
	if !strings.Contains(r.Source, "BTreeMap::new()") {
		t.Error("no dictionary return builds a BTreeMap any more; the container " +
			"choice collapsed onto the pair vector")
	}
}

// DEFINES GENERATE RUST ACCESSORS. (R5, found by three ports; each declared the
// fk.define import by hand and re-derived the ids from the GO generator's
// source, which is the shape a binding exists to remove.)
//
// There is no constant to bake as a substitute: a define's VALUE is Factorio's
// own and is not in the API description at all -- only its dotted path and a
// sort order are. defines.train_state was RENUMBERED between 1.1 and 2.0, so
// transcribing one fails silently.
func TestDefinesGenerateRustAccessors(t *testing.T) {
	r, _ := rustBindings(t)
	if r.Defines == 0 {
		t.Fatal("no define accessor was generated")
	}
	for _, want := range []string{
		`fn fk_define(id: u32) -> u32;`,
		`pub fn defines_direction_east() -> u32 {`,
		`pub fn defines_inventory_fuel() -> u32 {`,
	} {
		if !strings.Contains(r.Source, want) {
			t.Errorf("the generated bindings have no %s", want)
		}
	}
	i := strings.Index(r.Source, "pub fn defines_direction_east() -> u32 {")
	body := r.Source[i : i+320]
	// The value must not be in the source. A generator that baked one would be
	// inventing it.
	if strings.Contains(body, "return 4") || strings.Contains(body, "= 4;") {
		t.Errorf("a define value was baked into the bindings:\n%s", body)
	}
	// TWO STATICS, NOT A ZERO SENTINEL. defines.direction.north IS zero, so a
	// cache that treated 0 as "not resolved yet" would make one host call per
	// read for exactly the defines a mod reads most.
	if !strings.Contains(body, "static OK: AtomicBool") {
		t.Errorf("the accessor has no resolved flag, so a define whose value is "+
			"0 would be re-fetched forever:\n%s", body)
	}
	// The id must reach the import as a LITERAL. `fklua mod` prunes the 1185
	// define paths to the ids it can prove constant, and that scan is over an
	// i32.const feeding the import: an id computed from a table would compile,
	// would work, and would ship ~45 KB of paths into every save.
	if !regexp.MustCompile(`fk_define\(\d+\)`).MatchString(body) {
		t.Errorf("the define id does not reach the import as a literal:\n%s", body)
	}
	// A path fragment that is a Rust keyword must not become a raw identifier
	// in the MIDDLE of a name -- `defines_r#type_x` does not parse.
	if strings.Contains(r.Source, "defines_r#") {
		t.Error("a raw identifier escaped into the middle of a define accessor name")
	}
}

// BOTH GENERATORS STAMP THE SAME NAME, which is the whole load-bearing property
// of the pin stamp and the one a language-shaped mechanism would quietly lose.
//
// The stamp is what `fklua mod` compares against the pin it is packaging at, so
// a Go-only stamp would leave every Rust guest unchecked -- silently, because an
// absent stamp is treated as "cannot prove" and stays quiet by design. There is
// no scoping argument available for doing this in one language: the generators
// are at member-id parity precisely because a guest in either language calls the
// same ids out of the same description, so a mismatched pin is the same defect
// in both.
//
// It asserts the NAME comes from PinExport rather than spelling it, because a
// literal here would keep passing if the mangling changed under it -- and it
// asserts each language's own export spelling too, since a name in a comment is
// not an export.
func TestBothGeneratorsStampTheSameName(t *testing.T) {
	a := loadTestAPI(t)
	rep, evs := GenerateMembers(a), GenerateEvents(a)
	g, err := GenerateGoWith(a, rep, evs, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	r, err := GenerateRust(a, rep, evs)
	if err != nil {
		t.Fatal(err)
	}

	stamp := PinExport(a.ApplicationVersion)
	if !strings.HasPrefix(stamp, PinExportPrefix) || stamp == PinExportPrefix {
		t.Fatalf("PinExport(%q) produced %q, which carries no version",
			a.ApplicationVersion, stamp)
	}
	for _, c := range []struct{ lang, want, src string }{
		{"go", "//go:wasmexport " + stamp + "\nfunc fkAPIPin() {}", g.Source},
		{"rust", "#[no_mangle]\npub extern \"C\" fn " + stamp + "() {}", r.Source},
	} {
		if !strings.Contains(c.src, c.want) {
			t.Errorf("the %s bindings do not export the pin stamp for API %s.\n"+
				"`fklua mod` proves the packaged table and the guest's bindings came "+
				"from one description by reading this export name; without it every "+
				"%s guest is unprovable and the guard stays silent.\nwanted:\n%s",
				c.lang, a.ApplicationVersion, c.lang, c.want)
		}
	}
}
