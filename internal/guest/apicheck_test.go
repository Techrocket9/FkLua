package guest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// M9's other gate: `api check` catches a DELIBERATELY BROKEN fixture.
//
// A check that reports "clean" for every guest passes trivially and is worth
// nothing, which is exactly the failure mode here -- most guests really are
// unaffected by most releases, so a broken implementation looks identical to a
// working one on real input. The fixture is therefore a guest built to call a
// member the OTHER version does not have, and the assertion is that it is
// reported.
//
// The control matters as much: the same check against a guest that touches only
// surviving members must come back clean, or "reports everything" would pass
// the first half.
//
// FROM IS THE DEFAULT PIN, AND THAT IS A CORRECTNESS REQUIREMENT RATHER THAN A
// CONVENIENCE. Both guests here are built against the COMMITTED bindings, which
// are generated from factorio.DefaultAPIVersion; a member id is a dense sorted
// index per version, so resolving those ids against any other description names
// different members and the check answers a question nobody asked -- silently,
// since every id still resolves to something. It held by coincidence while the
// default pin happened to be the diff's `from`, and the 2.1.14 bump is what
// separated them: the fixture stopped compiling, which was the loud half, and
// the control would have gone on "passing" on misresolved ids, which was not.
// Named from the constant now, so the two cannot drift apart again.
//
// The direction is an UPGRADE from the GA pin to a 2.1 description, and it was
// a downgrade while the default pin was the newest thing committed. The
// machinery is direction-agnostic -- "does this guest still work on 2.1.12" is
// the same cross-reference as "will it survive 2.1.16" -- and it is the same
// mostly-nobody's-problem shape the feature exists for. What the direction
// change DID cost is the fixture's member, which has to be one the two
// descriptions disagree about: `burner_usage` was the 2.1.14-only one and
// `LuaRecipePrototype::is_parameter` is the 2.0.77-only one. The disagreement
// is re-derived from the descriptions below rather than trusted, so a later
// pin move fails here saying what to pick instead of building a fixture that
// compiles and reports nothing.
func TestAPICheckCatchesABrokenGuest(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	from, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	to, err := factorio.LoadAPI(filepath.Join(root, "api", "2.1.12", "runtime-api.json"))
	if err != nil {
		t.Skip("2.1.12 is not cached; run `fklua api pull 2.1.12`")
	}
	diff := factorio.DiffAPI(from, to)

	// THE FIXTURE'S MEMBER IS CHECKED AGAINST THE DESCRIPTIONS BEFORE IT IS
	// COMPILED. The whole test rests on it being present in `from` and absent
	// from `to`; get that wrong and the fixture either fails to build (loud, and
	// how the last pin move announced itself) or builds and reports nothing --
	// which reads exactly like a check that works and found nothing to say.
	const (
		fixClass = "LuaRecipePrototype"
		fixAttr  = "is_parameter"
	)
	hasAttr := func(a *factorio.API) bool {
		for _, c := range a.Classes {
			if c.Name != fixClass {
				continue
			}
			for _, at := range c.Attributes {
				if at.Name == fixAttr {
					return true
				}
			}
		}
		return false
	}
	if !hasAttr(from) || hasAttr(to) {
		t.Fatalf("the fixture calls %s::%s, which must exist at the %s pin and "+
			"not in 2.1.12 (present: from=%v to=%v). Pick another removed "+
			"attribute with a two-value getter and update the source below.",
			fixClass, fixAttr, factorio.DefaultAPIVersion, hasAttr(from), hasAttr(to))
	}

	// The fixture is written here rather than committed under examples/: it is
	// deliberately broken, and a broken example in the tree is something a
	// reader has to be told to ignore.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	const broken = `package main

import "github.com/Techrocket9/fklua/guest/go/fkapi"

//go:wasmexport fk_on_init
func onInit() {
	// is_parameter exists at the default pin and NOT in 2.1.12; the two
	// descriptions are asked to confirm that before this is compiled.
	p := fkapi.LuaRecipePrototype{}
	_, _ = p.IsParameter()
}

func main() {}
`
	// Built inside guest/go so it resolves fkapi from the same module.
	pkgDir := filepath.Join(root, "guest", "go", "examples", "_apicheckfixture")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(pkgDir)
	if err := os.WriteFile(filepath.Join(pkgDir, "main.go"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	wasmPath := filepath.Join(dir, "broken.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"),
		"./examples/_apicheckfixture", wasmPath); err != nil {
		t.Fatalf("building the fixture: %v", err)
	}

	res := checkOne(t, root, wasmPath, from, diff)
	if len(res.Hits) == 0 {
		t.Fatalf("the fixture calls a member 2.1.12 removed and the check "+
			"reported nothing; it ignored %d breaking change(s)", res.Ignored)
	}
	found := false
	for _, h := range res.Hits {
		if strings.Contains(h.What, fixAttr) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %s among the hits, got %v", fixAttr, res.Hits)
	}
	t.Logf("fixture: %d hit(s), %d ignored", len(res.Hits), res.Ignored)

	// The control. Without it, a check that reported every breaking change
	// would pass everything above.
	clean := filepath.Join(dir, "api.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/api", clean); err != nil {
		t.Fatal(err)
	}
	res = checkOne(t, root, clean, from, diff)
	if len(res.Hits) != 0 {
		t.Errorf("examples/api touches only members that survive 2.1.12, so the "+
			"check should be clean; it reported %v", res.Hits)
	}
	if res.Ignored == 0 {
		t.Error("the control ignored nothing, which means the diff was empty and " +
			"the clean result proves nothing")
	}
}

func checkOne(t *testing.T, root, wasmPath string, api *factorio.API,
	diff factorio.APIDiff) factorio.CheckResult {
	t.Helper()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	report := factorio.GenerateMembers(api)
	evs := factorio.GenerateEvents(api)
	defs := factorio.GenerateDefines(api)
	usedM, mOK := factorio.UsedMembers(im)
	usedE, eOK := factorio.UsedEvents(im)
	usedD, dOK := factorio.UsedDefines(im)
	if !mOK {
		t.Fatal("the member id scan was incomplete, so the check cannot be trusted")
	}
	s := factorio.SurfaceOf(report, usedM, mOK, usedE, eOK, evs, usedD, dOK, defs)
	return factorio.CheckGuest(s, diff)
}
