package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// READING A TIER-2 VALUE, end to end, against a value the HOST built.
//
// The accessors live in the generated preamble -- a raw string in gogen.go and
// its twin in rustgen_rt.go -- so `census.json` cannot see them and no host-side
// unit test can call them: `//go:wasmimport` is rejected outside GOARCH=wasm,
// so the package they are in does not compile for a test binary. This is the
// only instrument that runs them.
//
// BOTH LANGUAGES AGAINST ONE STUB AND ONE SET OF EXPECTATIONS, which is
// TestBothDataGuestLibrariesMakeTheSameCalls' shape and for its reason: the two
// preambles are hand-written twins that nothing generates, and this repo has
// already run the single-language experiment -- the Rust generator fell four
// milestones behind and every gap was reported by a mod author rather than
// found here. The two families are spelled differently on purpose (Go's
// comma-ok is Rust's Option, and Go's As- prefix exists only to dodge Value's
// own field names), so what is compared is the ANSWERS.
//
// The stub returns one nested table from json_to_table and nothing else has to
// be modelled, which is why that member is the source: what is under test is
// the accessors rather than the plumbing around them.
func TestValueAccessorsReadWhatTheTagNames(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p := filepath.Join(tmp, "dynread.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/dynread", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkDynReadGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "dynread",
			filepath.Join(tmp, "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkDynReadGuest(t, p)
	})
}

// dynReadWant is the transcript both languages owe.
//
// EVERY LINE IS ORDER-INDEPENDENT. The host writes a map's pairs in pairs()
// order, which this ABI does not promise and which bin/lua52f varies between
// runs -- so an expectation over the pair slice would be flaky by construction.
// Key lookups and Len are order-free, and At runs over an array.
var dynReadWant = []string{
	// A lookup chains, and its miss is nil rather than a zero of some type.
	"LOG get: name=belt count=42 on=true",
	"LOG miss: str=<none> num=-1 nil=true",
	// Has answers what Get cannot, and answers it false for a scalar receiver
	// rather than raising.
	"LOG has: name=true nope=false onascalar=false",
	// Two levels of chaining, one miss at the second level, and one where the
	// FIRST level is a scalar -- which is the case that would panic in a
	// hand-written scan and is nil here.
	"LOG deep: hit=7 miss=-1 via-scalar=-1",
	// Zero-based, out of range both ways, and a map indexed as an array. The
	// map case carries is_nil as well as the string default, because the
	// default alone hides an At that walks the pair slice unless the first pair
	// in pairs() order happens to hold a string.
	"LOG at: 0=a 2=c 9=<oob> neg=<oob> map=<notarray> map-nil=true",
	// The one accessor with an answer for a scalar.
	"LOG len: map=7 arr=3 scalar=0 nil=0",
	// Read through the WRONG tag: not-ok, three times, rather than a plausible
	// zero. This is the line the whole family exists for.
	"LOG as: num-of-str=0/no str-of-num=''/no bool-of-str=false/no",
	// ...and through the RIGHT tag, which is the control: a family that
	// answered no to everything would satisfy the line above.
	"LOG as: num=42/ok str='belt'/ok bool=true/ok",
	// A number key, and the string "7" is a different key from the number 7.
	"LOG key: n7=seven s7=<none> n8=<none>",
	// A handle through tier 2 still resolves, and ObjOr's default is the null
	// handle rather than whatever was in the slot.
	"LOG obj: iron-chest zero=0",
}

func checkDynReadGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageDynReadGuest(t, wasmPath)
	// json_to_table's stub ignores its argument and returns the fixture. The
	// value therefore crosses through the real write_dyn, so what the guest
	// reads is what a real host call would have handed it.
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = {} }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-dynread",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local ent = { valid = true, name = "iron-chest", object_name = "LuaEntity" }
helpers = {
  json_to_table = function(_)
    return {
      name = "belt",
      count = 42,
      on = true,
      inner = { deep = 7 },
      list = { "a", "b", "c" },
      obj = ent,
      [7] = "seven",
    }
  end,
}
game = {}
require("control")
handlers.on_init()
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	got := strings.Split(strings.TrimSpace(out), "\n")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	if len(got) != len(dynReadWant) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(dynReadWant), len(got), out)
	}
	for i := range dynReadWant {
		if got[i] != dynReadWant[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], dynReadWant[i])
		}
	}
}

func packageDynReadGuest(t *testing.T, wasmPath string) string {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)
	used, complete := factorio.UsedMembers(im)
	if !complete {
		t.Fatal("a member id was not a compile-time constant, so the scan broke")
	}
	usedEv, evComplete := factorio.UsedEvents(im)
	if !evComplete {
		t.Fatal("an event id was not a compile-time constant, so the scan broke")
	}
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-dynread", Version: "0.1.0", Title: "FkLua tier-2 accessors",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, APITable: table,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
