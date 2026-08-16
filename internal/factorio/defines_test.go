package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// DEFINES ARE RESOLVED BY NAME AT LOAD, and a guest never sees a baked number.
//
// The downstream report's premise for this was wrong and worth restating,
// because it is the reason the obvious shape does not exist: runtime-api.json
// carries define NAMES and an order and NOT their values, so a constant cannot
// be baked from the pin at all. This ABI already had the right pattern for it,
// in defines.events -- the generated table carries the NAME and control.lua
// resolves it against the running game.
//
// Everything but defines.events, which keeps its own resolved table: its values
// are not what fk.subscribe takes, and offering both spellings of "on_tick"
// would be a trap rather than a convenience.
func TestDefinesAreGeneratedAsNamesNotValues(t *testing.T) {
	a := loadTestAPI(t)
	d := GenerateDefines(a)
	if len(d.Defines) == 0 {
		t.Fatal("no defines generated")
	}
	byPath := map[string]int{}
	for _, v := range d.Defines {
		if v.ID == 0 {
			t.Errorf("%s got id 0, which the ABI reserves for absent", v.Path)
		}
		byPath[v.Path] = v.ID
	}
	if _, ok := byPath["direction.east"]; !ok {
		t.Error("defines.direction.east did not generate; it is the constant the " +
			"first downstream mod hand-wrote as 4")
	}
	for p := range byPath {
		if strings.HasPrefix(p, "events.") {
			t.Errorf("%s is in the defines table; defines.events has its own "+
				"resolved table and its numbers are not fk.subscribe's", p)
		}
	}

	// And the generated Lua carries the NAME. A number here would mean the
	// generator had invented a value the API description does not contain.
	src, err := d.luaDefines()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `"direction.east"`) {
		t.Errorf("the generated table does not carry the path by name:\n%s",
			src[:min(len(src), 400)])
	}
}

// The Go side gets an accessor per define, and the guest calls it instead of
// writing the number.
func TestDefinesGenerateGuestAccessors(t *testing.T) {
	g, _ := goBindings(t)
	for _, want := range []string{
		"func DefinesDirectionEast() uint32",
		"func hostDefine(",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the generated bindings have no %s", want)
		}
	}
	if g.Defines == 0 {
		t.Error("no define accessors were generated")
	}
	// The value must not be in the source. A generator that baked one would be
	// inventing it: the API description does not carry define values.
	i := strings.Index(g.Source, "func DefinesDirectionEast() uint32")
	if i < 0 {
		t.Fatal("no DefinesDirectionEast")
	}
	if strings.Contains(g.Source[i:i+200], "return 4") {
		t.Error("a define value was baked into the bindings")
	}
}

// End to end: a guest asks for defines.direction.east through fk.define, and
// what comes back is what THIS Factorio says, resolved by name at load.
func TestADefineIsResolvedAgainstTheRunningGame(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	defs := GenerateDefines(a)

	var eastID, absentID int
	var absentPath string
	for _, v := range defs.Defines {
		if v.Path == "direction.east" {
			eastID = v.ID
		}
		// A path that IS in the generated table and is NOT in the stand-in
		// game below -- i.e. a define this Factorio does not have, which is
		// the case the resolver has to report rather than guess at.
		if absentID == 0 && !strings.HasPrefix(v.Path, "direction.") {
			absentID, absentPath = v.ID, v.Path
		}
	}
	if eastID == 0 || absentID == 0 {
		t.Fatal("no direction.east, or nothing outside direction")
	}

	// The guest reads two defines and logs them as ASCII digits: the only
	// observation channel a packaged guest has that needs nothing bound is
	// env.fk_log, which control.lua routes to Factorio's own log().
	wat := fmt.Sprintf(`(module
		(import "fk" "define" (func $def (param i32) (result i32)))
		(import "env" "fk_log" (func $log (param i32 i32)))
		(memory 1)
		(func (export "fk_alloc") (param i32) (result i32) (i32.const 4096))
		(func (export "fk_alloc_static") (param i32) (result i32) (i32.const 4096))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init")
			(i32.store8 (i32.const 0x100)
				(i32.add (i32.const 48) (call $def (i32.const %d))))
			(i32.store8 (i32.const 0x101)
				(i32.add (i32.const 48) (call $def (i32.const 999999))))
			(i32.store8 (i32.const 0x102)
				(i32.add (i32.const 48) (call $def (i32.const %d))))
			(call $log (i32.const 0x100) (i32.const 3))))`, eastID, absentID)

	im := buildIR(t, wat)
	usedDef, complete := UsedDefines(im)
	if !complete {
		t.Fatal("the define id scan did not prove the constants")
	}
	if len(usedDef) != 3 {
		t.Errorf("the scan found %d define ids, want 3", len(usedDef))
	}

	// PRUNING IS THE POINT, and it is why a define read is an import call at
	// all: the scan that finds a constant reaching an import is the only
	// pruning machinery here, and the full table is every define there is
	// (define_values in census.json).
	report := full.Only(map[int]bool{})
	report.Defines = defs.Only(usedDef)
	apiSrc, err := report.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	fullReport := full.Only(map[int]bool{})
	fullReport.Defines = defs
	fullSrc, err := fullReport.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(apiSrc) > len(fullSrc)/50 {
		t.Errorf("one define renders as %d bytes against %d for all %d of them",
			len(apiSrc), len(fullSrc), len(defs.Defines))
	}

	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-defines", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk:    chunk,
		Exports:  []string{"fk_on_init", "fk_alloc", "fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// defines.direction.east is 4 in this Factorio, and the whole point of the
	// mechanism is that the guest did not have to know that.
	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
local logged = {}
function log(s) logged[#logged+1] = s end
defines = { events = {}, direction = { east = 4, north = 0 } }
storage = {}
local handlers = {}
script = {
  mod_name = "t",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) end,
  on_configuration_changed = function(f) end,
  on_event = function() end, set_event_filter = function() end,
}
require("control")
handlers.on_init()
for i = 1, #logged do print("log " .. logged[i]) end
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(out)
	// "4" for east; "0" for an id outside this build's table, which reads zero
	// with nothing to say about it; "0" for a path the table HAS and this
	// Factorio does not, which is reported.
	if !strings.Contains(got, "log 400") {
		t.Errorf("the define did not resolve to what THIS build says:\n%s", got)
	}
	if !strings.Contains(got, "no defines.") {
		t.Errorf("defines.%s is absent from this game and was not reported:\n%s",
			absentPath, got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
