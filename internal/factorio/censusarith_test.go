package factorio

import (
	"fmt"
	"path/filepath"
	"testing"
)

// THE CENSUS'S THREE MEMBER ROWS MUST RECONCILE, and for five milestones they
// did not.
//
// `host_members_bound` is what GenerateMembers put in the table.
// `<lang>_members_bound` and `<lang>_members_deferred` are what each guest
// backend made of exactly that table -- one loop, one member per iteration,
// each iteration ending in an emit or a deferral. So
//
//	host_members_bound == <lang>_members_bound + <lang>_members_deferred
//
// is an identity rather than a coincidence, and it read 4842 against 4843 in
// BOTH languages. The extra one was not a member at all: the string-enum
// constant loop, which runs after the member loop and emits `pub const` /
// untyped constants rather than bindings, called the same defer1 the member
// loop does, and one literal in the 2.1.14 description (`LinkedGameControl`'s
// empty string) has no identifier name.
//
// It is worth a test rather than a corrected number because the number is not
// what was wrong -- the arithmetic was, silently, in the one committed file
// whose entire job is to make a version bump readable. A reader who added the
// three rows got 4843 and had no way to learn whether a member had been
// double-counted, dropped between the two walks, or (as here) joined by
// something that is not a member.
func TestTheCensusMemberArithmeticCloses(t *testing.T) {
	a, r, g, rb := genBoth(t)

	host := len(r.Members)
	for _, c := range []struct {
		lang              string
		emitted, deferred int
	}{
		{"go", g.Emitted, g.Deferred},
		{"rust", rb.Emitted, rb.Deferred},
	} {
		if c.emitted+c.deferred != host {
			t.Errorf("%s: %d bound + %d deferred = %d, against %d members in the "+
				"host table. Every member is bound or deferred exactly once, so a "+
				"difference is a member counted twice, a member counted in neither, "+
				"or something that is not a member reaching one of the counters "+
				"(which is what the string-enum constants used to do).",
				c.lang, c.emitted, c.deferred, c.emitted+c.deferred, host)
		}
	}

	// A DEFERRED MEMBER IS A REAL MEMBER, checked by name and not only by
	// count -- because a member counted twice and a member counted in neither
	// cancel in a total, which is the same reason
	// TestBothBackendsBindTheSameMembers compares id SETS.
	inTable := map[string]bool{}
	for _, m := range r.Members {
		inTable[memberKey(m)] = true
	}
	if len(inTable) != host {
		t.Fatalf("%d members in the table and %d distinct (class, name, kind) "+
			"keys: the host walk emitted a duplicate", host, len(inTable))
	}
	for k := range g.Names {
		if !inTable[k] {
			t.Errorf("Go bound %s, which is not in the host member table", k)
		}
	}
	for k := range rb.Names {
		if !inTable[k] {
			t.Errorf("Rust bound %s, which is not in the host member table", k)
		}
	}

	// AND THE ACCOUNTING LINE'S OWN DECOMPOSITION, which is the second half of
	// the same defect and pointed the other way. `gen-bindings` reconciles the
	// description's methods, both halves of every attribute and the class
	// operators against the member KINDS they became -- and kind 7,
	// MemberGetHandle, was in none of those buckets, so the printed sum was
	// 4784 against 4842 with the 58 missing named nowhere. A member kind that
	// reaches no line of the report is precisely the F-IDX shape.
	byKind := map[int]int{}
	for _, m := range r.Members {
		byKind[m.Kind]++
	}
	sum := byKind[MemberCall] + byKind[MemberGet] + byKind[MemberSet] +
		byKind[MemberGetEq] + byKind[MemberIndex] + byKind[MemberLen] +
		byKind[MemberSelf] + byKind[MemberGetHandle] + byKind[MemberIndexSet] +
		byKind[MemberGlobalFunc]
	if sum != host {
		t.Errorf("the nine accounted kinds sum to %d against %d members: a kind "+
			"exists that no line of the deferral report mentions", sum, host)
	}

	// The COMMITTED census says the same thing, because a rule that holds only
	// in the generator is a rule the one file a reviewer reads can still break.
	cen, err := LoadCensus(CensusPath(filepath.Join("..", "..", "api"), a.ApplicationVersion))
	if err != nil {
		t.Fatalf("%v -- run `fklua gen-bindings`", err)
	}
	if cen.HostMembers != cen.GoMembers+cen.GoDeferred {
		t.Errorf("census.json: host_members_bound %d, go %d + %d = %d",
			cen.HostMembers, cen.GoMembers, cen.GoDeferred,
			cen.GoMembers+cen.GoDeferred)
	}
	if cen.HostMembers != cen.RustMembers+cen.RustDeferred {
		t.Errorf("census.json: host_members_bound %d, rust %d + %d = %d",
			cen.HostMembers, cen.RustMembers, cen.RustDeferred,
			cen.RustMembers+cen.RustDeferred)
	}
	if cen.CustomTableHandles != byKind[MemberGetHandle] {
		t.Errorf("census.json: custom_table_handle_members %d, generator %d",
			cen.CustomTableHandles, byKind[MemberGetHandle])
	}
	if cen.IndexSetters != byKind[MemberIndexSet] {
		t.Errorf("census.json: index_setter_members %d, generator %d",
			cen.IndexSetters, byKind[MemberIndexSet])
	}
	// AND THE ROW THAT USED TO BE A WRITTEN-DOWN ZERO. It is the one census
	// count with a floor the description itself supplies: every global function
	// this generator can express reaches the table, so a row that fell BELOW
	// len(a.GlobalFunctions) would mean one had started deferring silently.
	if cen.GlobalFunctionsBound != byKind[MemberGlobalFunc] {
		t.Errorf("census.json: global_functions_bound %d, generator %d",
			cen.GlobalFunctionsBound, byKind[MemberGlobalFunc])
	}
	if cen.GlobalFunctionsBound != len(a.GlobalFunctions) {
		t.Errorf("global_functions_bound %d of %d described: one is being "+
			"skipped, and `fklua gen-bindings` names it under the host member "+
			"table's deferrals", cen.GlobalFunctionsBound, len(a.GlobalFunctions))
	}
	// The literals are counted and are counted APART. Zero would mean the row
	// had quietly become decoration; a non-zero total that no reason accounts
	// for would mean the split had drifted the way the member one did.
	for _, c := range []struct {
		lang  string
		total int
		by    map[string]int
	}{
		{"go", cen.GoLiteralsDeferred, cen.GoLiteralDeferBy},
		{"rust", cen.RustLiteralsDeferred, cen.RustLiteralDeferBy},
	} {
		n := 0
		for _, v := range c.by {
			n += v
		}
		if n != c.total {
			t.Errorf("%s: %d literal deferrals counted by reason, %d in total",
				c.lang, n, c.total)
		}
	}
	if cen.GoLiteralsDeferred != cen.RustLiteralsDeferred {
		t.Errorf("literals deferred: Go %d, Rust %d -- the two backends read the "+
			"same list through the same LiteralIdent", cen.GoLiteralsDeferred,
			cen.RustLiteralsDeferred)
	}
}

// memberKey is the key both backends put in Names: "Class::member/kind".
func memberKey(m Member) string {
	return fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)
}
