package factorio

import (
	"strings"
	"testing"
)

// LuaLazyLoadedValue: the last deliberately-refused runtime type.
//
// It cost exactly one event. `mapType` had a `case "LuaLazyLoadedValue"` that
// returned an error, `mapFields` fails a whole struct when one field fails, and
// so `on_player_setup_blueprint` -- whose `mapping` field is the type's ONLY
// occurrence in the description -- was skipped entirely: not bound on the host,
// no Go struct, no Rust struct, 218 of 219 everywhere.
//
// WHAT THE REFUSAL WAS FOR, and why it is no longer needed. The type is a
// wrapper the engine returns "for performance reasons": the value inside is
// constructed only when `get` is called. There is no fixed layout for "a
// value that does not exist yet", so a generator that tried to MARSHAL one had
// nothing to write -- and marshalling it eagerly would have been worse than
// refusing, because it would defeat the only reason the type exists.
//
// The resolution is that there was never anything to marshal. The description
// declares LuaLazyLoadedValue as an ordinary CLASS (one method, `get`; two
// attributes, `valid` and `object_name`), so the type usage
// `LuaLazyLoadedValue<T>` is an object-handle field like the other 261
// class-typed event fields in this pin. One handle crosses, the engine builds
// nothing, and a guest that wants the value calls `Get()` -- which generates as
// an ordinary bound member returning tier-2 dyn, correct since `Any` stopped
// mistyping as a handle (see TestAnyIsTierTwoAndForceIDIsStillAHandle).
//
// So laziness is preserved BY CONSTRUCTION rather than by machinery: the
// marshaller never touches the value, because a handle is all it is given.

// The occurrence census, written out so a pin that adds a second one fails
// HERE -- with a message about what to think about -- rather than silently
// widening a claim this file makes about "the one event".
//
// Checked against 2.1.12 as well when this landed: same single occurrence, same
// event, same inner type. A pin that moves it should say so.
func TestLuaLazyLoadedValueOccursExactlyOnce(t *testing.T) {
	a := loadTestAPI(t)

	var found []string
	var walk func(tp Type, where string)
	walk = func(tp Type, where string) {
		if tp.Complex == "LuaLazyLoadedValue" {
			found = append(found, where)
		}
		if tp.Value != nil {
			walk(*tp.Value, where)
		}
		if tp.Key != nil {
			walk(*tp.Key, where)
		}
		for _, o := range tp.Options {
			walk(o, where)
		}
	}
	for _, e := range a.Events {
		for _, f := range e.Data {
			walk(f.Type, e.Name+"::"+f.Name)
		}
	}
	for _, c := range a.Classes {
		for _, at := range c.Attributes {
			if at.ReadType != nil {
				walk(*at.ReadType, c.Name+"::"+at.Name)
			}
		}
	}

	if len(found) != 1 || found[0] != "on_player_setup_blueprint::mapping" {
		t.Fatalf("LuaLazyLoadedValue occurs at %v; this file's whole design rests "+
			"on it being exactly [on_player_setup_blueprint::mapping]. A new "+
			"occurrence is not a broken test -- read the new site and decide "+
			"whether a handle is still the right crossing for it", found)
	}
}

// The type usage crosses as a handle, which is the entire fix.
func TestALazyLoadedValueCrossesAsAHandle(t *testing.T) {
	a := loadTestAPI(t)
	m := newTypeMapper(a)

	// LuaLazyLoadedValue<dictionary<uint32, LuaEntity>> -- the shape the pin has.
	inner := Type{Complex: "dictionary", Key: &Type{Name: "uint32"}, Value: &Type{Name: "LuaEntity"}}
	f, err := m.mapType(Type{Complex: "LuaLazyLoadedValue", Value: &inner}, 0)
	if err != nil {
		t.Fatalf("LuaLazyLoadedValue<...> still refused: %v", err)
	}
	if f.Kind != KindHandle {
		t.Fatalf("LuaLazyLoadedValue<...> maps to %v, want KindHandle -- the "+
			"whole point is that the dictionary is NOT crossed", f.Kind)
	}
	// The parameterised payload must NOT have been expanded into the layout:
	// a dict field here would mean the host built the value to marshal it,
	// which is the one thing this type exists to avoid.
	if f.Elem != nil || f.Key != nil || len(f.Struct) != 0 {
		t.Errorf("the handle carries the payload's layout (elem=%v key=%v struct=%d); "+
			"that would make the host construct the value it was told not to",
			f.Elem, f.Key, len(f.Struct))
	}
	// And the class name is carried, for the doc comment the generators write.
	if f.TypeName != "LuaLazyLoadedValue" {
		t.Errorf("TypeName is %q, want %q -- the generators name the class in "+
			"the field's doc comment so a reader knows what to call Get() on",
			f.TypeName, "LuaLazyLoadedValue")
	}
}

// The event the refusal cost, end to end through the host generator.
func TestOnPlayerSetupBlueprintIsBound(t *testing.T) {
	a := loadTestAPI(t)
	evs := GenerateEvents(a)

	for _, s := range evs.Skipped {
		if strings.Contains(s.Reason, "LuaLazyLoadedValue") {
			t.Errorf("event %s skipped for %q", s.Name, s.Reason)
		}
	}

	var got *EventDef
	for i := range evs.Events {
		if evs.Events[i].Name == "on_player_setup_blueprint" {
			got = &evs.Events[i]
		}
	}
	if got == nil {
		t.Fatal("on_player_setup_blueprint is not bound")
	}

	var mapping *FieldSpec
	for i := range got.Fields {
		if got.Fields[i].Name == "mapping" {
			mapping = &got.Fields[i]
		}
	}
	if mapping == nil {
		t.Fatal("on_player_setup_blueprint has no `mapping` field")
	}
	if mapping.Kind != KindHandle {
		t.Errorf("mapping crosses as %v, want KindHandle", mapping.Kind)
	}
	if mapping.Optional {
		t.Error("mapping is declared mandatory in the pin; an optional here " +
			"would change what a guest must check")
	}
}

// EVERY event is bound now, and this is the census claim the round moved.
//
// It is written as "none skipped" rather than as the number 219, because the
// number lives in api/<version>/census.json and duplicating it here is exactly
// what api_test.go's header says not to do.
func TestNoEventIsUnbindable(t *testing.T) {
	a := loadTestAPI(t)
	evs := GenerateEvents(a)
	if len(evs.Skipped) != 0 {
		for _, s := range evs.Skipped {
			t.Errorf("event %s skipped: %s", s.Name, s.Reason)
		}
	}
	if len(evs.Events) != len(a.Events) {
		t.Errorf("%d of %d events bound", len(evs.Events), len(a.Events))
	}
}

// `get` is what makes the handle worth having, and its return type is the thing
// that was wrong until the operator round: `Any` used to canonicalise to a
// handle, which would have typed this as returning an OBJECT and mistyped every
// string and number the method really returns.
func TestLazyLoadedValueGetReturnsTierTwo(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)

	var get *Member
	for i := range r.Members {
		m := &r.Members[i]
		if m.Class == "LuaLazyLoadedValue" && m.Name == "get" && m.Kind == MemberCall {
			get = m
		}
	}
	if get == nil {
		t.Fatal("LuaLazyLoadedValue::get is not bound")
	}
	if len(get.Rets) != 1 || get.Rets[0].Kind != KindDyn {
		t.Fatalf("get returns %v, want one KindDyn -- the value is `Any`, and a "+
			"handle here would mistype every string and number it returns",
			get.Rets)
	}
	if len(get.Args) != 0 {
		t.Errorf("get takes %d args, want none", len(get.Args))
	}
}

// The Go binding: an Object field on the event struct, and a typed wrapper to
// call Get() on. Neither is new machinery -- the point of the test is that the
// generator reaches both without a special case.
func TestGoBindsTheLazyEvent(t *testing.T) {
	a := loadTestAPI(t)
	g, err := GenerateGo(a, GenerateMembers(a), "fkapi")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	src := g.Source

	for _, want := range []string{
		"type OnPlayerSetupBlueprint struct",
		"func ReadOnPlayerSetupBlueprint(",
		"type LuaLazyLoadedValue struct{ Object }",
		"func (o LuaLazyLoadedValue) Get() (Value, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated Go lacks %q", want)
		}
	}

	// The field itself, typed as a handle rather than as a map.
	body := structBody(t, src, "type OnPlayerSetupBlueprint struct")
	if !fieldIs(body, "Mapping", "Object") {
		t.Errorf("OnPlayerSetupBlueprint.Mapping is not an Object:\n%s", body)
	}
	if strings.Contains(body, "map[") {
		t.Errorf("OnPlayerSetupBlueprint carries a map, so the host built the "+
			"lazy value to marshal it:\n%s", body)
	}
	// The payload type belongs in the doc comment: a guest needs to know what
	// Get() yields, and per-site generic typing is not on the table.
	if !strings.Contains(body, "LuaLazyLoadedValue") {
		t.Errorf("nothing in OnPlayerSetupBlueprint names LuaLazyLoadedValue, so "+
			"a reader cannot tell what to call Get() on:\n%s", body)
	}
}

// Rust parity, id for id. Both generators read one description; a backend that
// lagged here would be the regression B1a closed.
func TestRustBindsTheLazyEvent(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	rb, err := GenerateRust(a, r, GenerateEvents(a))
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	src := rb.Source

	for _, want := range []string{
		"pub struct OnPlayerSetupBlueprint",
		"pub fn read_on_player_setup_blueprint(",
		"pub struct LuaLazyLoadedValue",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated Rust lacks %q", want)
		}
	}
	body := structBody(t, src, "pub struct OnPlayerSetupBlueprint")
	if !strings.Contains(body, "pub mapping: Object,") {
		t.Errorf("OnPlayerSetupBlueprint.mapping is not an Object:\n%s", body)
	}
	if strings.Contains(body, "mapping: BTreeMap") || strings.Contains(body, "mapping: Vec") {
		t.Errorf("mapping carries the payload, so the host built the lazy "+
			"value to marshal it:\n%s", body)
	}
	if !strings.Contains(body, "LuaLazyLoadedValue") {
		t.Errorf("nothing in the Rust OnPlayerSetupBlueprint names "+
			"LuaLazyLoadedValue:\n%s", body)
	}
}

// The two generators must agree about the event ID, because a guest bakes it in
// and the host resolves it by name from ONE table. A Go/Rust split here is a
// guest subscribed to the wrong event.
func TestBothBackendsAgreeOnTheEventID(t *testing.T) {
	a := loadTestAPI(t)
	evs := GenerateEvents(a)

	var id int
	for _, e := range evs.Events {
		if e.Name == "on_player_setup_blueprint" {
			id = e.ID
		}
	}
	if id == 0 {
		t.Fatal("no id for on_player_setup_blueprint")
	}

	g, err := GenerateGo(a, GenerateMembers(a), "fkapi")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	rb, err := GenerateRust(a, GenerateMembers(a), evs)
	if err != nil {
		t.Fatalf("GenerateRust: %v", err)
	}
	// gofmt aligns the Go constant block, so match on the tokens rather than on
	// the spacing -- otherwise this passes or fails on the longest event name.
	if got := constValue(g.Source, "EventOnPlayerSetupBlueprint"); got != itoa(id) {
		t.Errorf("Go has EventOnPlayerSetupBlueprint = %q, want %d", got, id)
	}
	rsWant := "pub const EVENT_ON_PLAYER_SETUP_BLUEPRINT: u32 = " + itoa(id) + ";"
	if !strings.Contains(rb.Source, rsWant) {
		t.Errorf("generated Rust lacks %q", rsWant)
	}
}

// constValue returns the right-hand side of `name<spaces>= value` in src.
func constValue(src, name string) string {
	for _, ln := range strings.Split(src, "\n") {
		f := strings.Fields(ln)
		if len(f) == 3 && f[0] == name && f[1] == "=" {
			return f[2]
		}
	}
	return ""
}

// `mapping` gets NO mask bit, and the reason is that it is MANDATORY -- not
// that it is a handle.
//
// The distinction is worth pinning, because this same event proves the other
// half: `stack` and `record` are OPTIONAL handles and both DO get a Skip
// constant, since an optional's presence byte gives a masked field the "absent"
// reading the rule requires. A mandatory field has no such reading -- masking
// one would hand the guest a zero it cannot tell from a live handle, and the
// failure would surface later, as ERR_BAD_HANDLE on a call, rather than as an
// absence at decode time.
//
// So a guest subscribing to this event pays one transient handle whether or not
// it wants the mapping. That is the honest cost and it is the right one: a
// handle is a table store and an integer increment on the host, while the
// dictionary of every entity in the blueprint -- the thing actually worth
// avoiding -- is not built either way.
func TestMappingIsNotMaskableBecauseItIsMandatory(t *testing.T) {
	a := loadTestAPI(t)
	g, err := GenerateGo(a, GenerateMembers(a), "fkapi")
	if err != nil {
		t.Fatalf("GenerateGo: %v", err)
	}
	if strings.Contains(g.Source, "SkipOnPlayerSetupBlueprintMapping") {
		t.Error("a mandatory handle got a mask bit; a masked handle reads as 0, " +
			"which is not an absence any decoder reports")
	}
	// The same event's OPTIONAL handles do get one. This is what stops the test
	// above passing because the mask machinery quietly stopped emitting, and it
	// is what makes "mandatory" rather than "handle" the operative word.
	for _, want := range []string{
		"SkipOnPlayerSetupBlueprintStack",
		"SkipOnPlayerSetupBlueprintRecord",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("no %s: an optional handle IS maskable, and if that stopped "+
				"being true the rule this test states has changed", want)
		}
	}
}

// THE WALL, NAMED EXACTLY: Factorio's event trigger, and nothing else.
//
// Everything about this binding is verified -- the crossing, the dispatch, the
// dyn decode, both backends, the lifetime -- except that a real
// `on_player_setup_blueprint` has never been observed carrying a real
// LuaLazyLoadedValue. That is not a gap in the machinery. It is that the event
// fires only when a PLAYER sets up a blueprint at the UI, and it cannot be
// raised from script.
//
// TWO SOURCES SAY SO, and this test asks both, because they fail in opposite
// directions. `LuaBootstrap::raise_event` carries the whitelist VERBATIM in its
// `lists` block -- ten events, on_console_chat through script_raised_set_tiles
// -- which is the description's own primary statement and the thing to believe.
// Separately the description carries one `raise_<name>` HELPER per raiseable
// event, which is the inference: it is a stronger signal when present (a helper
// is a bound method, not prose) and a weaker one when absent (a helper could be
// dropped while the event stayed raiseable). The whitelist alone would miss an
// event that gained a helper without the prose being updated; the helpers alone
// would miss one added to the whitelist and given no helper. Asking both is the
// difference between pinning the wall and pinning one description of it.
//
// So this test asserts the wall rather than working around it. If a pin ever
// moves either source, this fails and says to go and drive the event for real
// instead of documenting why it cannot be -- which is the standing rule about
// unverifiable paths: ask which half is actually behind the wall, and check
// again when something moves.
func TestOnPlayerSetupBlueprintCannotBeRaisedFromScript(t *testing.T) {
	a := loadTestAPI(t)

	var boot *Class
	for i := range a.Classes {
		if a.Classes[i].Name == "LuaBootstrap" {
			boot = &a.Classes[i]
		}
	}
	if boot == nil {
		t.Fatal("no LuaBootstrap in the pin")
	}

	// SOURCE ONE: the whitelist itself, as `raise_event` states it.
	var raiseEvent *Method
	for i := range boot.Methods {
		if boot.Methods[i].Name == "raise_event" {
			raiseEvent = &boot.Methods[i]
		}
	}
	if raiseEvent == nil {
		t.Fatal("no LuaBootstrap::raise_event in the pin")
	}
	whitelist := strings.Join(raiseEvent.Lists, "\n")
	if !strings.Contains(whitelist, "on_console_chat") {
		t.Fatalf("raise_event's whitelist does not name on_console_chat, so this "+
			"test is no longer reading the whitelist and cannot tell a raiseable "+
			"event from an unraiseable one. Got:\n%s", whitelist)
	}
	if strings.Contains(whitelist, "on_player_setup_blueprint") {
		t.Errorf("raise_event's own whitelist NAMES on_player_setup_blueprint: it "+
			"IS raiseable on this pin. The only thing this binding could not "+
			"verify is now reachable -- drive the event headlessly and assert the "+
			"real mapping instead of asserting this wall. Whitelist:\n%s", whitelist)
	}

	// SOURCE TWO: the per-event helpers, which are methods rather than prose.
	raisable := map[string]bool{}
	for _, m := range boot.Methods {
		if strings.HasPrefix(m.Name, "raise_") && m.Name != "raise_event" {
			raisable[strings.TrimPrefix(m.Name, "raise_")] = true
		}
	}
	if len(raisable) == 0 {
		t.Fatal("no raise_* helpers found; this test's premise has changed and " +
			"it can no longer tell a raiseable event from an unraiseable one")
	}

	// The helpers name the event without its on_/script_raised_ prefix, so try
	// the forms the description actually uses.
	for _, form := range []string{
		"on_player_setup_blueprint",
		"player_setup_blueprint",
	} {
		if raisable[form] {
			t.Errorf("LuaBootstrap has raise_%s: on_player_setup_blueprint IS "+
				"raiseable on this pin. The only thing this binding could not "+
				"verify is now reachable -- drive the event headlessly and "+
				"assert the real mapping instead of asserting this wall", form)
		}
	}
}

// structBody returns the text of the struct declaration starting at `head`.
func structBody(t *testing.T, src, head string) string {
	t.Helper()
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("no %q in generated source", head)
	}
	j := strings.Index(src[i:], "\n}")
	if j < 0 {
		t.Fatalf("unterminated %q", head)
	}
	return src[i : i+j]
}

// fieldIs reports whether a gofmt-aligned struct body declares `name` as
// exactly `typ`. Written out because gofmt pads the gap between the two and a
// literal "Mapping Object" would pass or fail on the longest field name in the
// struct, not on this field's type.
func fieldIs(body, name, typ string) bool {
	for _, ln := range strings.Split(body, "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && f[0] == name {
			return f[1] == typ
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
