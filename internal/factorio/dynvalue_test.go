package factorio

import (
	"strings"
	"testing"
)

// A STRUCT THAT IS A BOX AROUND ONE TIER-2 VALUE GETS TYPED READERS, in both
// languages, over the same set.
//
// The shape is ModSetting's, and it is the reason the item exists: a bool
// setting arrived as a tagged union to switch on, so reading one's own runtime
// setting was a match rather than a question. IsDynValueStruct is the rule, and
// it is a rule over the LAYOUT rather than a list of names for this repo's usual
// reason -- a name list is a decision nobody re-reads, and the same shape under
// another name at a later pin would silently get nothing.
//
// THE COUNT IS ASSERTED NON-ZERO AT EVERY COMMITTED DESCRIPTION, because a
// predicate that matched nothing would emit nothing, move no golden and pass
// every other gate here. That is this repo's standing rule about a zero nobody
// writes down, applied to a rule nobody re-derives.
//
// AND THE TWO BACKENDS ARE COMPARED RATHER THAN ASSUMED. They walk one Report
// and ask one predicate, so they cannot disagree -- which is precisely the
// reasoning AD5 disproved, where a defect fixed in one generator stood in the
// other for two milestones because the test was written against one backend.
func TestBothBackendsEmitTheSameDynValueReaders(t *testing.T) {
	vers := committedVersions(t)
	for _, v := range vers {
		t.Run(v, func(t *testing.T) {
			gen := stdGen(t, v)
			g, rb := gen.Go, gen.Rust
			if g.DynValueStructs != rb.DynValueStructs {
				t.Fatalf("go emitted readers for %d structs and rust for %d: "+
					"one predicate, two renderings -- a disagreement is a defect",
					g.DynValueStructs, rb.DynValueStructs)
			}
			if g.DynValueStructs == 0 {
				t.Fatalf("no struct matched IsDynValueStruct at %s, and ModSetting "+
					"has that shape in every published description -- a predicate "+
					"matching nothing emits nothing and moves no golden", v)
			}
			// ModSetting BY NAME as well as by count, because the count alone
			// cannot say that what matched is the shape this exists for.
			if !strings.Contains(g.Source, "func (v ModSetting) Bool() (bool, bool)") {
				t.Errorf("%s: the Go bindings carry no typed reader on ModSetting", v)
			}
			if !strings.Contains(rb.Source, "pub fn as_bool(&self) -> Option<bool> { self.value.as_bool() }") {
				t.Errorf("%s: the Rust bindings carry no typed reader on ModSetting", v)
			}
		})
	}
}

// The predicate itself, over the three ways a struct can fail to be one.
//
// A unit test rather than a description walk, because what it pins is the
// DECISION -- mandatory, single, dyn -- and no committed description happens to
// carry the near misses.
func TestIsDynValueStructIsSingleMandatoryAndDyn(t *testing.T) {
	dyn := FieldSpec{Name: "value", Kind: KindDyn}
	optDyn := FieldSpec{Name: "value", Kind: KindDyn, Optional: true}
	str := FieldSpec{Name: "name", Kind: KindString}

	for _, c := range []struct {
		what  string
		specs []FieldSpec
		want  bool
	}{
		{"one mandatory dyn", []FieldSpec{dyn}, true},
		{"one OPTIONAL dyn", []FieldSpec{optDyn}, false},
		{"one non-dyn", []FieldSpec{str}, false},
		{"a dyn beside another field", []FieldSpec{dyn, str}, false},
		{"no fields at all", nil, false},
	} {
		blk, err := LayoutStruct(c.specs)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if got := IsDynValueStruct(blk); got != c.want {
			t.Errorf("%s: IsDynValueStruct = %v, want %v", c.what, got, c.want)
		}
	}
}
