package factorio

import (
	"os"
	"path/filepath"
	"testing"
)

// EVERY COMMITTED DESCRIPTION CARRIES A CURRENT CENSUS, not only the default
// pin's.
//
// This is the standing gate for the class of defect BetterBeltBalancer reported
// as gap 24. A census is taken by whatever generation last ran against its
// description, and until 2026-08-24 the only thing that ever ran one was
// `gen-bindings` at DefaultAPIVersion -- so a generator gaining a row left the
// default pin's file current and every other committed version's file behind,
// with nothing anywhere saying so. `TestTheCensusMemberArithmeticCloses` above
// reads one census, the version bump recipe reads one census, and CI checked
// one census; a stale 2.1.14 was therefore invisible in this repo and became
// visible only downstream, as a mod moving its pin onto that version and
// failing a check no command could satisfy.
//
// Two versions were wrong when this test was written: 2.1.14's census was a
// generation behind (`index_setter_members` absent, three member counts one
// low) and 2.1.12 had NO CENSUS AT ALL -- `api pull` writes the description and
// the census used to arrive only if somebody moved the default pin onto it,
// which for 2.1.12 nobody ever did.
//
// It costs one full generation per committed description, which is the price of
// the property being structural rather than remembered.
func TestEveryCommittedDescriptionHasACurrentCensus(t *testing.T) {
	root := filepath.Join("..", "..", "api")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}

	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		v := e.Name()
		desc := filepath.Join(root, v, "runtime-api.json")
		if _, err := os.Stat(desc); err != nil {
			// A version directory with no description is not a committed
			// description; half a pull is not a thing to take a census of.
			continue
		}
		n++

		want := stdGen(t, v).Census
		got, err := LoadCensus(CensusPath(root, v))
		if err != nil {
			t.Errorf("%s: %v -- run `fklua gen-bindings`, which refreshes every "+
				"census this checkout owns", v, err)
			continue
		}
		for _, line := range want.Diff(got) {
			t.Errorf("%s: census.json is behind the generators: %s", v, line)
		}
	}

	// A LOOP OVER AN EMPTY DIRECTORY PASSES, which is the failure mode this
	// whole gap is an instance of. The repo commits more than one description
	// and the point of the test is the ones that are NOT the default pin, so a
	// run that found only the default found nothing worth finding.
	if n < 2 {
		t.Fatalf("found %d committed description(s) under %s: this test is about "+
			"the versions that are not the default pin, and with fewer than two "+
			"it asserts nothing", n, root)
	}
}
