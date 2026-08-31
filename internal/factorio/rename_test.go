package factorio

import (
	"strconv"
	"strings"
	"testing"
)

// A NAME COLLISION IS A DECISION, and memberRename is where it is taken.
//
// A class can declare a method and an attribute whose bound names coincide.
// Emitting both would not compile, so one was deferred -- and WHICH one was
// decided by emission order, methods being emitted before attributes. That is an
// accident dressed as a policy, and in both standing instances the loser is a
// member a guest can legitimately want and could not reach from either language.
//
// See memberRename for the two rows and the sentences they were read from.

// EVERY COLLISION HAS A ROW, at every committed description.
//
// This is TestEveryIndexOperatorHasAWriteVerdict's shape over a different
// derivation, and it exists for the same reason: an unlisted collision still
// DEFERS safely, so nothing breaks and nothing says anything -- the member is
// simply absent, and the only tell is a count in a census diff. With the
// identity recorded, a pin that introduces one fails HERE, naming the member and
// the name it would have had, so somebody decides rather than inherits.
func TestEveryNameCollisionHasARow(t *testing.T) {
	// The map keys spell MemberSet as a string, because a Go map literal cannot
	// call strconv. Pinned rather than trusted.
	if memberSetKind != strconv.Itoa(MemberSet) {
		t.Fatalf("memberSetKind is %q and MemberSet is %d: every memberRename row "+
			"keyed on it names a member that does not exist", memberSetKind, MemberSet)
	}

	for _, v := range committedVersions(t) {
		gen := stdGen(t, v)
		g, rb := gen.Go, gen.Rust
		for _, b := range []struct {
			lang    string
			collide []string
			stale   []string
			by      map[string]int
		}{
			{"go", g.Collisions, g.StaleRenames, g.DeferredBy},
			{"rust", rb.Collisions, rb.StaleRenames, rb.DeferredBy},
		} {
			for _, c := range b.collide {
				t.Errorf("%s %s: %s collides and memberRename has no row for it. "+
					"Decide which member keeps the name -- the method's name is the "+
					"description's own and the attribute's is this generator's "+
					"construction -- and write the row down with the reasoning",
					v, b.lang, c)
			}
			for _, s := range b.stale {
				t.Errorf("%s %s: %s", v, b.lang, s)
			}
			if n := b.by[langWord(b.lang)+NameCollision]; n != 0 {
				t.Errorf("%s %s: %d deferrals still under the collision reason",
					v, b.lang, n)
			}
		}
	}
}

// ...AND THE ROWS PRODUCE THE MEMBERS THEY PROMISE, in both languages.
//
// The winner keeps the description's own name and the loser gets the written
// one, so both halves are asserted: a rename that quietly replaced the METHOD
// would satisfy a check that only looked for the new name.
func TestTheRenamedMembersAreBoundUnderBothNames(t *testing.T) {
	a, r, g, rb := genBoth(t)
	_ = a
	_ = r

	for _, want := range []struct{ goName, rustName string }{
		// the losers, under their written names
		{"func (o LuaControl) WriteDriving(value bool) error {",
			"pub fn write_driving(&self, value: bool) -> Result<(), Status> {"},
		{"func (o LuaPlayer) WriteZoomLimits(value ZoomLimits) error {",
			"pub fn write_zoom_limits(&self, value: ZoomLimits) -> Result<(), Status> {"},
		// ...and the winners, still under the description's own names
		{"func (o LuaControl) SetDriving(", "pub fn set_driving(&self,"},
		{"func (o LuaPlayer) SetZoomLimits(", "pub fn set_zoom_limits(&self,"},
	} {
		if !strings.Contains(g.Source, want.goName) {
			t.Errorf("the Go bindings have no %q", want.goName)
		}
		if !strings.Contains(rb.Source, want.rustName) {
			t.Errorf("the Rust bindings have no %q", want.rustName)
		}
	}

	// The Names map is what `fklua docs` and every downstream lookup reads, so
	// the rename has to reach it rather than only the emitted text.
	for key, want := range map[string][2]string{
		"LuaControl::driving/" + memberSetKind:    {"WriteDriving", "write_driving"},
		"LuaPlayer::zoom_limits/" + memberSetKind: {"WriteZoomLimits", "write_zoom_limits"},
	} {
		if got := g.Names[key]; got != want[0] {
			t.Errorf("Go named %s %q, want %q", key, got, want[0])
		}
		if got := rb.Names[key]; got != want[1] {
			t.Errorf("Rust named %s %q, want %q", key, got, want[1])
		}
	}
}

// langWord spells a language the way its own generator does in a deferral
// reason: "Go" and "Rust". strings.Title is deprecated and would title-case
// "rust" to "Rust" and "go" to "Go" by luck rather than by rule.
func langWord(lang string) string {
	if lang == "go" {
		return "Go"
	}
	return "Rust"
}
