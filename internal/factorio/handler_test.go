package factorio

import (
	"strings"
	"testing"
)

// The five members that BIND and can never work.
//
// LuaBootstrap's on_init, on_load, on_event, on_configuration_changed and
// on_nth_tick each take `union(function, nil)`. canonicalUnion cannot type that
// -- a `function` option disqualifies the union -- so mapType renders it as tier
// 2 and all five bound as `handler Value`. Bound, required, and unfillable: a
// wasm guest has no callable Lua value, and the only argument it can express is
// nil, which is Factorio's UNREGISTER.
//
// Four of the five are harmlessly shadowed by FkLua's own hooks. on_nth_tick is
// not. It presented as a green, plausible member whose every possible call was a
// silent no-op, and no census row, compile check or document said so -- the one
// member in the whole API a guest could call, get OK from, and never hear from
// again. Seven of thirteen audited mods use `on_nth_tick`.
//
// See Member.Unfillable for why this is a mark rather than a skip, and why the
// HOST table keeps all five.
const handlerClass = "LuaBootstrap"

var handlerMembers = []string{
	"on_configuration_changed",
	"on_event",
	"on_init",
	"on_load",
	"on_nth_tick",
}

// Exactly those five, at every description this checkout owns.
//
// AN EQUALITY RATHER THAN A FLOOR, over every committed version: the predicate
// walks a whole type, so a pin that grows a sixth handler-taking member -- or
// that retires one -- is a NUMBER that moves here rather than a shape classified
// by a rule nobody re-read. That is TestEveryIndexOperatorHasAWriteVerdict's own
// discipline over a different derivation.
func TestOnlyTheFiveHandlerMembersAreUnfillable(t *testing.T) {
	for _, v := range committedVersions(t) {
		a := loadShapeAPI(t, v)
		var got []string
		for _, m := range GenerateMembers(a).Members {
			if m.Unfillable == "" {
				continue
			}
			if m.Unfillable != UnfillableHandler {
				t.Errorf("%s: %s::%s is unfillable for an unexpected reason %q",
					v, m.Class, m.Name, m.Unfillable)
			}
			if m.Class != handlerClass {
				t.Errorf("%s: %s::%s is unfillable and is not on %s",
					v, m.Class, m.Name, handlerClass)
			}
			got = append(got, m.Name)
		}
		if strings.Join(got, ",") != strings.Join(handlerMembers, ",") {
			t.Errorf("%s: the unfillable members are %v; the five that take a Lua "+
				"function are %v. A pin that moved this set wants a look at "+
				"typeCanBeAFunction before the list here is edited",
				v, got, handlerMembers)
		}
	}
}

// ...and the HOST still binds all five, so no member id moved.
//
// This is the half that makes the change safe for a mod already in the field.
// Ids are dense sorted indices over the member table, so REMOVING a member from
// that table would shift every later id and answer an existing guest's calls
// with different members -- the defect checkAPIPin exists to refuse. Deferring
// is guest-side only: the table is what it was, `fk.call` still resolves
// on_nth_tick, and what changed is that no generated binding names it.
func TestTheHostTableStillCarriesTheHandlerMembers(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)

	byName := map[string]Member{}
	for _, m := range r.Members {
		if m.Class == handlerClass && m.Kind == MemberCall {
			byName[m.Name] = m
		}
	}
	for _, name := range handlerMembers {
		m, ok := byName[name]
		if !ok {
			t.Fatalf("%s::%s is not in the host member table: a deferral is "+
				"guest-side and must not remove a member id", handlerClass, name)
		}
		if m.ID == 0 {
			t.Errorf("%s::%s has no id", handlerClass, name)
		}
	}

	// And it is really in the packaged table, not merely in the Go slice.
	src, err := r.LuaSourceWith(a, GenerateEvents(a))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `name="on_nth_tick"`) {
		t.Error("the rendered member table has no on_nth_tick: the host binding " +
			"was removed along with the guest one")
	}
}

// Neither generator emits one, and both count five under one reason.
//
// The reason string is a CENSUS KEY (`go_deferrals_by_reason`), which is why
// UnfillableHandler is a constant: two generators spelling one bucket
// differently would split the row in half and the version diff would report two
// small movements instead of one real one.
func TestNeitherGuestBindsAHandlerMember(t *testing.T) {
	_, _, g, rb := genBoth(t)

	for _, b := range []struct {
		lang string
		by   map[string]int
		src  string
		// probe is a fragment the generated source must NOT contain.
		probe string
	}{
		{"go", g.DeferredBy, g.Source, "func (o LuaBootstrap) OnNthTick("},
		{"rust", rb.DeferredBy, rb.Source, "pub fn on_nth_tick("},
	} {
		if n := b.by[UnfillableHandler]; n != len(handlerMembers) {
			t.Errorf("%s: %d deferrals under %q, want %d",
				b.lang, n, UnfillableHandler, len(handlerMembers))
		}
		if strings.Contains(b.src, b.probe) {
			t.Errorf("%s: the generated bindings still declare %q, which is a "+
				"function whose every call is a silent no-op", b.lang, b.probe)
		}
	}
}
