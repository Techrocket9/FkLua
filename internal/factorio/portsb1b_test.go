package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The rest of the cross-language generator round: what seven mod ports written
// outside this repo found the generators doing to a shape the description
// carries. Each test names its finding.

func genBoth(t *testing.T) (*API, Report, GoBindings, RustBindings) {
	t.Helper()
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	g, err := GenerateGoWith(a, r, ev, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := GenerateRust(a, r, ev)
	if err != nil {
		t.Fatal(err)
	}
	return a, r, g, rb
}

// G1 -- a method returning more than one value. Thirteen at this pin, and one
// of them is the ONLY way to arm on_object_destroyed, so nixie-tubes shipped a
// hand-written binding into the generated package to have it at all.
func TestMultipleReturnValuesAreEmittedInBothLanguages(t *testing.T) {
	_, r, g, rb := genBoth(t)
	multi := 0
	for _, m := range r.Members {
		if len(m.Rets) < 2 {
			continue
		}
		multi++
		key := fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)
		if _, ok := g.Names[key]; !ok {
			t.Errorf("Go still defers %s::%s (%d returns)", m.Class, m.Name, len(m.Rets))
		}
		if _, ok := rb.Names[key]; !ok {
			t.Errorf("Rust still defers %s::%s", m.Class, m.Name)
		}
	}
	if multi != 13 {
		t.Errorf("%d multi-return members at this pin, expected 13", multi)
	}
	for _, why := range []map[string]int{g.DeferredBy, rb.DeferredBy} {
		if n := why["multiple return values"]; n != 0 {
			t.Errorf("%d members still deferred as multiple return values", n)
		}
	}
	// The flagship, and the shape a caller sees. Three returns and an error/
	// Status, not a struct: the host already sends three values.
	want := "(uint64, uint64, uint32, error)"
	if got := goSignatureOf(t, g, "LuaBootstrap", "RegisterOnObjectDestroyed"); !strings.HasSuffix(got, want) {
		t.Errorf("RegisterOnObjectDestroyed is %q, want it to end %q", got, want)
	}
}

// goSignatureOf finds a generated Go method's declaration line.
func goSignatureOf(t *testing.T, g GoBindings, class, method string) string {
	t.Helper()
	needle := fmt.Sprintf("func (o %s) %s(", class, method)
	i := strings.Index(g.Source, needle)
	if i < 0 {
		t.Fatalf("no %s.%s in the generated package", class, method)
	}
	return strings.TrimSuffix(g.Source[i:i+strings.Index(g.Source[i:], "\n")], " {")
}

// FTS1 -- an optional ARRAY return. Rust said Vec, so absent and empty were one
// answer; Go said a slice, which really does distinguish them and said so
// nowhere. Each language's own optional convention, and the Go half is a doc
// comment because the behaviour was already right.
func TestOptionalArrayReturnsAreOptional(t *testing.T) {
	_, _, g, rb := genBoth(t)
	if !strings.Contains(rb.Source,
		"pub fn get_records(&self, interrupt_index: Option<u32>) -> Result<Option<Vec<ScheduleRecord>>, Status>") {
		t.Error("LuaSchedule::get_records does not return Option<Vec<..>> in Rust; " +
			"absent and empty are one answer again")
	}
	// The destination variant cannot be an Option without giving up the reuse,
	// so the presence comes back as the result.
	if !strings.Contains(rb.Source, "pub fn get_records_into(&self, dst: &mut Vec<ScheduleRecord>, "+
		"interrupt_index: Option<u32>) -> Result<bool, Status>") {
		t.Error("the Rust _into variant of an optional array does not report presence")
	}
	sig := goSignatureOf(t, g, "LuaSchedule", "GetRecords")
	if !strings.HasSuffix(sig, "([]ScheduleRecord, error)") {
		t.Errorf("GetRecords is %q; Go keeps the slice, nil meaning absent", sig)
	}
	i := strings.Index(g.Source, "func (o LuaSchedule) GetRecords(")
	if !strings.Contains(g.Source[max(0, i-600):i], "returns nil when the value is ABSENT") {
		t.Error("the Go binding does not SAY that nil is absent and empty is empty, " +
			"which is the whole of the Go half of FTS1")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// FTS2 -- a union of nothing but string literals, and what reached a guest was
// a bare string on the one field a schedule record cannot be written without.
//
// The COUNT comes from the committed census rather than a literal here: it is
// 46 at the 2.1.14 pin and was 41 at 2.0.77, and a number that moves with the
// description belongs in the one file a version bump already regenerates. What
// this test owns is that the shape still reaches both backends, which is a
// property and not a count.
func TestStringLiteralUnionsGetConstants(t *testing.T) {
	a, _, g, rb := genBoth(t)
	census, err := LoadCensus(CensusPath(filepath.Join("..", "..", "api"), a.ApplicationVersion))
	if err != nil {
		t.Fatalf("%v -- run `fklua gen-bindings`", err)
	}
	if n := len(StringLiteralUnions(a)); n != census.StringEnums {
		t.Errorf("%d string-literal unions at this pin, census says %d",
			n, census.StringEnums)
	}
	for _, want := range []string{
		`WaitConditionTypeInactivity`, `= "inactivity"`,
		`SignalIDTypeVirtual`,
		// The punctuation transliteration, without which every arithmetic
		// operator would be nameless at once -- and they are the ones a typo
		// actually hurts.
		`ArithmeticCombinatorParameterOperationStar`,
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the Go bindings do not carry %s", want)
		}
	}
	for _, want := range []string{
		`pub const WAIT_CONDITION_TYPE_INACTIVITY: &str = "inactivity";`,
		`pub const ARITHMETIC_COMBINATOR_PARAMETER_OPERATION_STAR: &str = "*";`,
	} {
		if !strings.Contains(rb.Source, want) {
			t.Errorf("the Rust bindings do not carry %s", want)
		}
	}
	// UNTYPED, so a call site that passes a plain literal keeps compiling. A
	// `WaitConditionType` DEFINED type would be better documentation and would
	// break every guest that already writes the string.
	if strings.Contains(g.Source, "type WaitConditionType ") {
		t.Error("a defined type for a string enum would break existing call sites")
	}
	// Only ONE literal in the whole description has no identifier name -- the
	// empty string in LinkedGameControl -- and it is counted rather than
	// silently dropped.
	//
	// COUNTED IN ITS OWN BUCKET, since the census's off-by-one. It used to land
	// in DeferredBy, the MEMBER deferrals, which is why
	// `host_members_bound == bound + deferred` read 4842 against 4843 in both
	// languages. A constant is not a member; see
	// TestTheCensusMemberArithmeticCloses.
	for _, c := range []struct {
		lang string
		by   map[string]int
	}{{"go", g.LiteralDeferBy}, {"rust", rb.LiteralDeferBy}} {
		if n := c.by["a string literal with no identifier name"]; n != 1 {
			t.Errorf("%s: %d nameless literals counted, expected 1", c.lang, n)
		}
	}
	if n := g.DeferredBy["a string literal with no identifier name"]; n != 0 {
		t.Errorf("%d nameless literals counted as MEMBER deferrals; a constant "+
			"is not a member and counting it as one is what broke the census "+
			"arithmetic", n)
	}
}

// Q3 -- a string-keyed dictionary RETURN came back as a Go map, whose iteration
// order Go randomizes per process, in a lockstep game. Its union-keyed
// neighbour was already the ordered pair slice, one generator line away.
func TestNoGeneratedGoMapSurvives(t *testing.T) {
	_, _, g, _ := genBoth(t)
	if i := strings.Index(g.Source, "map["); i >= 0 {
		line := g.Source[i:]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		t.Errorf("a Go map survives in the generated package, which is a walk "+
			"order Go randomizes per process: %s", strings.TrimSpace(line))
	}
	if !strings.Contains(g.Source,
		"func (o LuaForce) Technologies() ([]EntryStringObject, error)") {
		t.Error("force.technologies is not the ordered pair slice")
	}
}

// F-TAGS -- a read_write attribute whose SETTER did not generate, alone among
// its class's thirty-odd. It was counted as a deferral rather than silent, and
// counted is not available: what blocked it was the dictionary ARGUMENT, whose
// stated reason was "would need a deterministic iteration order" -- which the
// pair slice above HAS, by construction.
func TestEveryWritableAttributeHasItsSetter(t *testing.T) {
	a, r, g, rb := genBoth(t)
	inTable := map[string]bool{}
	for _, m := range r.Members {
		inTable[fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)] = true
	}
	var missing []string
	for _, c := range a.Classes {
		for _, at := range c.Attributes {
			if at.WriteType == nil {
				continue
			}
			key := fmt.Sprintf("%s::%s/%d", c.Name, at.Name, MemberSet)
			if !inTable[key] {
				t.Errorf("%s::%s is writable and has no SET member at all", c.Name, at.Name)
				continue
			}
			_, inGo := g.Names[key]
			_, inRust := rb.Names[key]
			if !inGo || !inRust {
				missing = append(missing, c.Name+"::"+at.Name)
			}
		}
	}
	// NONE remain, since the two that did -- genuine NAME collisions with a
	// method the class also declares, LuaControl::set_driving and
	// LuaPlayer::set_zoom_limits -- became a decision rather than an accident of
	// emission order. See memberRename and TestEveryNameCollisionHasARow; they
	// bind as WriteDriving and WriteZoomLimits.
	//
	// A ZERO ASSERTED AS A ZERO, deliberately. This read `!= 2` and a shape that
	// stopped generating would have taken the place of one of the collisions and
	// passed, which is the arithmetic-that-cancels shape this repo already
	// records against a total.
	if len(missing) != 0 {
		t.Errorf("writable attributes with no setter binding in one or both "+
			"languages: %v", missing)
	}
	for _, want := range []string{"LuaEntity::tags", "LuaItemCommon::tags", "LuaGuiElement::tags"} {
		key := want[:strings.Index(want, "::")] + "::" +
			want[strings.Index(want, "::")+2:] + fmt.Sprintf("/%d", MemberSet)
		if _, ok := g.Names[key]; !ok {
			t.Errorf("%s still has no setter: F-TAGS is not closed", want)
		}
	}
	for _, why := range []map[string]int{g.DeferredBy, rb.DeferredBy} {
		for k, v := range why {
			if strings.Contains(k, "dictionary ARGUMENT") {
				t.Errorf("%d dictionary arguments still deferred: %s", v, k)
			}
		}
	}
}

// R4 -- a union-typed struct FIELD degrades to a raw tier-2 Value, so a guest
// spells the Lua table out with string keys nothing checks. Four ports reported
// it. The typed struct already exists; this is the way to spend it.
func TestGeneratedStructsCanBecomeTierTwoValues(t *testing.T) {
	_, _, g, rb := genBoth(t)
	for _, want := range []string{
		"func (v SignalID) ToValue() Value {",
		`kv = append(kv, KeyValue{Key: OfString("name"), Val: OfString((*v.Name))})`,
		"func (v MapPosition) ToValue() Value {",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the Go bindings do not carry %q", want)
		}
	}
	for _, want := range []string{
		"pub fn to_value(&self) -> Value {",
		`kv.push((Value::Str(LuaStr::from("name")), Value::Str(x.clone())));`,
	} {
		if !strings.Contains(rb.Source, want) {
			t.Errorf("the Rust bindings do not carry %q", want)
		}
	}
	// The field this was reported against is still a raw Value, which is the
	// honest half: this pass gives a way to BUILD one, not a typed union.
	if !strings.Contains(g.Source, "Value Value") {
		t.Error("LogisticFilter.Value stopped being a tier-2 Value; if a typed " +
			"union landed, this test is the wrong shape rather than wrong")
	}
}

// The deferral REPORT's completeness, which is the lesson F-IDX taught: a
// member the generator TRIES and cannot express is counted, and a shape it never
// looks at is counted nowhere. Both halves have to be true for the printed
// report to mean anything.
func TestNothingTheDescriptionModelsIsUnaccountedFor(t *testing.T) {
	a, r, g, rb := genBoth(t)

	// Every member in the table is either emitted or deferred WITH A REASON, in
	// both languages. An empty reason string would land in "unstated", which is
	// itself a bug this asserts against.
	for _, b := range []struct {
		lang     string
		emitted  int
		deferred int
		by       map[string]int
	}{
		{"go", g.Emitted, g.Deferred, g.DeferredBy},
		{"rust", rb.Emitted, rb.Deferred, rb.DeferredBy},
	} {
		sum := 0
		for k, v := range b.by {
			if k == "unstated" {
				t.Errorf("%s: %d deferrals with no reason", b.lang, v)
			}
			sum += v
		}
		if sum != b.deferred {
			t.Errorf("%s: %d deferrals counted by reason, %d in total",
				b.lang, sum, b.deferred)
		}
	}

	// And the accounting: methods, both halves of every attribute, and the
	// class operators all reach the member table, or are in Skipped.
	//
	// A SKIP IS CHARGED TO THE KIND IT CAME FROM, and it did not have to be
	// until the 2.1.14 pin. Every skip the generator had ever produced was a
	// METHOD -- a callback parameter or a variadic one, both of which only a
	// method can have -- so "methods minus call members equals the skip list"
	// held by accident, and an attribute skip made both of those lines wrong at
	// once with nothing saying which. 2.1 added `nil` as the type of
	// UtilityConstants::frozen_color_lookup, which takes
	// LuaPrototypes::utility_constants down with it, and that is the shape.
	// Charging each skip to its declaring kind is what keeps the identity
	// readable: an unaccounted member is still a shape nobody looked at, which
	// is the F-IDX failure, and now it also says whether it was a method, an
	// attribute or an operator.
	methods, reads, writes, ops := 0, 0, 0, 0
	isMethod := map[string]bool{}
	attrHalves := map[string][2]bool{}
	for _, c := range a.Classes {
		methods += len(c.Methods)
		ops += len(c.Operators)
		for _, m := range c.Methods {
			isMethod[c.Name+"::"+m.Name] = true
		}
		for _, at := range c.Attributes {
			if at.ReadType != nil {
				reads++
			}
			if at.WriteType != nil {
				writes++
			}
			attrHalves[c.Name+"::"+at.Name] = [2]bool{at.ReadType != nil, at.WriteType != nil}
		}
	}
	bound := map[int]int{}
	for _, m := range r.Members {
		bound[m.Kind]++
	}
	nOps := bound[MemberIndex] + bound[MemberLen] + bound[MemberSelf]
	// Skipped covers the difference between what was modelled and what landed.
	// A member the mapper could not type is in Skipped with a reason; anything
	// else is a shape nobody looked at, which is the F-IDX failure exactly.
	opSkips, methodSkips, readSkips, writeSkips := 0, 0, 0, 0
	for _, sk := range r.Skipped {
		key := sk.Class + "::" + sk.Name
		switch {
		case strings.HasPrefix(sk.Name, "operator "):
			opSkips++
		case isMethod[key]:
			methodSkips++
		default:
			halves, ok := attrHalves[key]
			if !ok {
				t.Errorf("%s is skipped (%s) and the description declares no such "+
					"method, attribute or operator; the skip list has drifted from "+
					"what it is a list OF", key, sk.Reason)
				continue
			}
			if halves[0] {
				readSkips++
			}
			if halves[1] {
				writeSkips++
			}
		}
	}
	if got := methods - bound[MemberCall]; got != methodSkips {
		t.Errorf("%d methods modelled, %d call members, %d method skips: %d unaccounted for",
			methods, bound[MemberCall], methodSkips, got-methodSkips)
	}
	if nOps+opSkips != ops {
		t.Errorf("%d class operators, %d bound and %d skipped", ops, nOps, opSkips)
	}
	if bound[MemberGet]+readSkips != reads {
		t.Errorf("%d readable attributes, %d GET members and %d read halves skipped",
			reads, bound[MemberGet], readSkips)
	}
	if bound[MemberSet]+writeSkips != writes {
		t.Errorf("%d writable attributes, %d SET members and %d write halves skipped",
			writes, bound[MemberSet], writeSkips)
	}
}

// B1b's "known and not closed", and it was WIDER than it was recorded as.
//
// B1b bound LuaCustomTable's `index` and `length` operators and then noted that
// `force.technologies` still had no handle to call them on. The grep says the
// gap is universal rather than particular: NOTHING in the API returns a
// LuaCustomTable, so `Get` and `Length` were bound-and-unreachable from
// everywhere, not merely from that one attribute. The attributes carrying the
// type (custom_table_handle_members in census.json) span seven classes, nearly
// all of LuaPrototypes among them, and every one of them generated as a
// MATERIALISING dictionary
// read, because the description models the type structurally and mapType
// collapses it onto `dictionary`.
//
// What that cost, measured by fklua-ports' qol-research (Q2) rather than
// estimated: one read of force.technologies is 14,544 bytes of guest heap, for a
// guest that wanted one entry of 319.
//
// The fix is a second member over the same attribute -- a real member with its
// own id, because unlike the <Name>Into variant the HOST does different work
// (it writes a handle where it wrote a (ptr, count)). See MemberGetHandle.
func TestALuaCustomTableAttributeHasAHandleRoute(t *testing.T) {
	_, _, g, r := genBoth(t)
	if !strings.Contains(g.Source,
		"func (o LuaForce) TechnologiesRaw() (Object, error)") {
		t.Error("force.technologies has no handle accessor in Go, so the index " +
			"and length operators bound on LuaCustomTable are still unreachable")
	}
	if !strings.Contains(r.Source, "pub fn technologies_raw(&self) -> Result<Object, Status>") {
		t.Error("force.technologies has no handle accessor in Rust")
	}
	// The MATERIALISING read stays. Which one a guest wants is a question about
	// the guest -- iterating the whole table is what it is for -- and removing it
	// would break every existing call site to fix a cost only some of them pay.
	if !strings.Contains(g.Source,
		"func (o LuaForce) Technologies() ([]EntryStringObject, error)") {
		t.Error("the materialising read was removed rather than supplemented")
	}
	// ...and the operator it exists to reach.
	if !strings.Contains(g.Source, "func (o LuaCustomTable) Get(key Value) (Value, error)") {
		t.Error("LuaCustomTable.Get is gone, which is what the handle is FOR")
	}

	// A PLAIN `dictionary` MUST NOT GET ONE. The two map to the same field kind
	// and marshal identically; the only place the distinction survives is the
	// description's own complex_type tag, which is why isCustomTable asks the
	// description again rather than looking at the FieldSpec. If this ever fires,
	// the gate has been moved onto KindDict and every dictionary-returning
	// attribute in the API has grown a handle accessor to a Lua table that has
	// no handle behind it.
	if strings.Contains(g.Source, "func (o LuaEntity) TagsRaw()") {
		t.Error("a plain dictionary attribute got a handle accessor; the gate " +
			"is on the field kind rather than on the described type")
	}
}

// The advice in the pair-slice doc comment was a MEASURED TRAP.
//
// It said "build a map from it if you want lookup". fklua-ports measured what
// that costs on the attribute the advice is most likely to be read next to: one
// force.technologies read is 14,544 B of guest heap and the advised map adds
// 12,512 B on top -- 27,056 B, which is WORSE than the 24,576 B Go map the
// ordered slice was introduced to replace. The port's right answer was a
// zero-allocation linear scan, and since the handle route above there is a
// better one for a point lookup.
func TestThePairSliceDoesNotAdviseBuildingAMap(t *testing.T) {
	_, _, g, _ := genBoth(t)
	if strings.Contains(g.Source, "Build a map from") {
		t.Error("the pair-slice comment still advises building a Go map for " +
			"lookup, which fklua-ports measured as more guest heap than the " +
			"Go map the slice replaced")
	}
	for _, want := range []string{
		"DO NOT BUILD A GO MAP FROM IT FOR LOOKUP",
		"<Name>Raw accessor",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the pair-slice comment does not say %q", want)
		}
	}
	// The union-key half of the same comment family is a DIFFERENT statement --
	// which arm of the union arrives is Lua's choice and pairs() yields the NAME
	// -- and it must survive a rewrite of the advice next to it.
	if !strings.Contains(g.Source, "Filtering on") ||
		!strings.Contains(g.Source, "TagNumber matches nothing there, silently") {
		t.Error("the union-key warning (B1b's RM2) was lost")
	}
}
