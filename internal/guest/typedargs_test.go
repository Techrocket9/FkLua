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

// THE TYPED ARGUMENT FORM LANDS ON THE SAME ENGINE CALL AS THE DYN FORM, which
// is the whole correctness argument for it.
//
// LuaGuiElement::add takes one tier-2 map because its parameter table is a
// discriminated union. AddTyped is the SAME MEMBER ID over a tier-1 struct plus
// one optional tier-2 slot, decoded by fk_abi.lua's M.call_typed into the same
// Lua table -- so what has to be proved is an EQUALITY and not a behaviour. The
// stub renders the table it was handed with its keys sorted, so the rendering is
// a function of the table's contents and not of pairs() order, and the assertion
// is that the dyn leg and the typed leg render the same string.
//
// BOTH LANGUAGES AGAINST ONE STUB AND ONE SET OF EXPECTATIONS, which is the
// shape TestValueAccessorsReadWhatTheTagNames and
// TestBothDataGuestLibrariesMakeTheSameCalls both have, and for AD5's reason:
// the two generators are separate code, and a defect fixed in one has stood in
// the other for two milestones before now.
func TestATypedArgumentBlockCallsTheEngineTheSameWayTheDynMapDoes(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p := filepath.Join(t.TempDir(), "typedargs.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/typedargs", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkTypedArgsGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "typedargs",
			filepath.Join(t.TempDir(), "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkTypedArgsGuest(t, p)
	})
}

// typedArgsWant is the transcript both languages owe.
//
// The two `add` lines under `leg dyn` and `leg typed` are THE ASSERTION: same
// keys, same values, same rendering, one built as a pair list of key strings and
// one out of a typed struct through a flat block.
var typedArgsWant = []string{
	"LOG leg dyn",
	"ADD caption=Launch enabled=true name=row-7 style=green_button type=button",
	"LOG leg typed",
	"ADD caption=Launch enabled=true name=row-7 style=green_button type=button",
	// The tail carries what the block cannot: `sprite` and `number` are in a
	// variant group and have no field.
	"LOG leg tail",
	"ADD name=icon number=42 sprite=item/iron-plate type=sprite-button",
	// ...and the tail WINS over a key the block already set, which is what makes
	// it an escape hatch rather than a supplement.
	"LOG leg override",
	"ADD name=tail-said-this type=label",
	// An absent optional is ABSENT. The block is 248 bytes of mostly-optional
	// fields, so a presence byte read wrongly shows up here as a key nobody set.
	"LOG leg minimal",
	"ADD type=flow",
	"LOG gui done",
	// AND create_entity, THE OTHER VARIANT-DEFEATED MEMBER, which unlike `add`
	// a headless Factorio can reach -- a surface exists at tick 0 and a player
	// does not. That is what makes this example the in-game proof as well as
	// the host-side one, and why the two legs live in one program.
	"CREATE iron-chest at 8,8",
	"LOG entity dyn: iron-chest",
	"CREATE iron-chest at 12,8",
	"LOG entity typed: iron-chest",
	"LOG done",
}

func checkTypedArgsGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageTypedArgsGuest(t, wasmPath)
	// The stub renders every key it was handed, SORTED, so the line is a
	// function of the table's contents. Values are rendered by type rather than
	// by tostring, so `true` and the string "true" cannot read alike -- the
	// distinction the whole tier-2 tag machinery is about.
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
-- on_tick has a number here because EXPORTING fk_on_tick IS THE SUBSCRIPTION and
-- control.lua resolves defines.events by name at load. The handler itself is
-- never called below: this fixture drives on_init, and the tick leg exists for
-- the in-game run, where a benchmark phase needs a line of ours to count.
defines = { events = { on_tick = 60 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-typedargs",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local function render(t)
  local keys = {}
  for k in pairs(t) do keys[#keys + 1] = tostring(k) end
  table.sort(keys)
  local out = {}
  for i = 1, #keys do
    local v = t[keys[i]]
    local s
    if type(v) == "boolean" then s = tostring(v)
    elseif type(v) == "number" then s = string.format("%%g", v)
    elseif type(v) == "string" then s = v
    elseif type(v) == "table" then s = "{table}"
    else s = "<" .. type(v) .. ">" end
    out[#out + 1] = keys[i] .. "=" .. s
  end
  print("ADD " .. table.concat(out, " "))
end
local screen = {
  valid = true, object_name = "LuaGuiElement",
  add = function(spec) render(spec) return nil end,
}
local gui = { valid = true, object_name = "LuaGui", screen = screen }
local player = { valid = true, object_name = "LuaPlayer", gui = gui }
local function newEntity(n) return { valid = true, object_name = "LuaEntity", name = n } end
local surface = {
  valid = true, object_name = "LuaSurface",
  create_entity = function(spec)
    local p = spec.position
    print(string.format("CREATE %%s at %%g,%%g", tostring(spec.name),
      p.x or p[1], p.y or p[2]))
    return newEntity(spec.name)
  end,
}
game = {
  get_player = function(_) return player end,
  get_surface = function(_) return surface end,
}
helpers = {}
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
	if len(got) != len(typedArgsWant) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(typedArgsWant), len(got), out)
	}
	for i := range typedArgsWant {
		if got[i] != typedArgsWant[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], typedArgsWant[i])
		}
	}
}

// packageTypedArgsGuest packages the guest THROUGH THE REAL PRUNING SCAN, which
// is half of what this test is for.
//
// A member reached only through fk.call_typed is reached by a constant in
// operand 1 of a DIFFERENT import, so a pruner that scanned fk.call alone would
// leave `add` out of the shipped table and the typed leg would answer
// ERR_NO_MEMBER. Nothing about that failure is visible in the generated source.
func packageTypedArgsGuest(t *testing.T, wasmPath string) string {
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
	// THE PACKAGED TABLE MUST CARRY THE TYPED LAYOUT, and saying so here rather
	// than only through the transcript is what turns a wrong-sounding
	// ERR_BAD_ARGS into a named failure.
	if !strings.Contains(table, "targs=") {
		t.Fatal("the pruned member table carries no typed argument layout, so " +
			"every typed call would answer ERR_BAD_ARGS")
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-typedargs", Version: "0.1.0", Title: "FkLua typed args",
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
