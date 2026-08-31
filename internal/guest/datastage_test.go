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

// The data stage end to end: a Go program becomes a Factorio mod's data.lua,
// and the data.lua runs.
//
// Every stage is real. TinyGo compiles guest/go/examples/datastage, the decoder
// reads the wasm TinyGo actually emitted, the emitter lowers it, the packager
// writes the mod's settings.lua / data.lua / data-final-fixes.lua, and lua52f
// runs those files against a stand-in for the data stage. Nothing in the middle
// is stubbed, which is the point.
//
// IT IS STILL NOT FACTORIO. `require` resolution inside a real mod, `util`,
// data:extend's own validation and the prototype loader are outside what the
// oracle can speak to -- scripts/run-datastage.sh runs the real thing and hashes
// the dump.

func TestAGoProgramBecomesAModsDataStage(t *testing.T) {
	dir := buildDataStageMod(t, "go")
	out := runDataStageMod(t, dir)

	// The guest's own log lines say it ran at all, which is the check a
	// state assertion cannot make: a data module that exported nothing would
	// leave the stand-in untouched and every "is this absent" check would pass.
	for _, w := range []string{
		"LOG fkdata example: settings stage",
		"LOG fkdata example: data stage, base 2.0.77",
		// env(4): the packager's own name, through the REAL generated stage
		// file -- which is what proves `fklua mod` wrote it into run().
		"LOG fkdata example: mod name is fkd-example",
		// env(5): defines.prototypes, both accessors, against the stand-in's
		// three item deriveds.
		"LOG fkdata example: transport-belt is an entity; item derives 3 types",
		// The settings -> data round trip: the setting fk_settings declared,
		// read back through env(3) at the data stage.
		"LOG fkdata example: startup fkd-enabled is true",
		"LOG fkdata example: fastest belt is fkd-belt",
		"LOG fkdata example: data-final-fixes stage",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	// What it actually built. Each of these is one of the seven imports.
	for _, w := range []string{
		// extend, at the settings stage
		`RAW bool-setting: fkd-enabled`,
		// extend, from a computed loop
		`RAW sprite: fkd-arrow-in-e fkd-arrow-in-n fkd-arrow-in-s fkd-arrow-in-w ` +
			`fkd-arrow-out-e fkd-arrow-out-n fkd-arrow-out-s fkd-arrow-out-w`,
		// the ART_BIAS arithmetic: an input points inwards so the bias ADDS to
		// the shift, an output points outwards so it SUBTRACTS. Two numbers
		// that are wrong in opposite directions is exactly what a magic-constant
		// data stage gets wrong, and it is what doing it in Go fixes.
		`SPRITE x=0 shift=0,-0.196`,
		`SPRITE2 x=192 shift=0,0.404`,
		// get + extend: the technology's cost is base's own, and its enabled
		// field is the startup setting's value -- the round trip, landed in
		// the built prototype rather than only in a log line
		`TECH count=20 time=15 ingr=automation-science-pack/1 enabled=true`,
		// clone + set: the patched fields moved and the untouched leaves did not
		`CLONE speed=0.25 minable=nil next_upgrade=nil icon=__base__/x.png ` +
			`coeff=32 anim=16 flags1=not-on-map box=-0.35/-0.35/0.35/0.35`,
		// THE DEEP-COPY ASSERTION. The four collision_box writes above are two
		// levels down; under a shallow clone they would land in the SOURCE, and
		// this is the line that would move.
		`SOURCE box=-0.4/-0.4/0.4/0.4`,
		// get, answering ABSENT, then extend
		`TOKEN stack=42`,
		`SETTING default=true`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// THE MIRROR, and it is the reason both languages were built in one round.
//
// `fkdata` is HAND-WRITTEN in both languages, so `census.json` -- which is what
// finally made the generated bindings' backends stay in step -- cannot see
// either of them. This repo has already run the single-language experiment:
// the Rust generator fell four milestones behind, every gap was reported by a
// mod author rather than found here, and one was the same defect in the same
// function the Go side had already fixed. The precedent that worked is fkipc,
// which shipped both arms at once with one test requiring identical behaviour
// from the two example guests.
//
// WHAT IS COMPARED is the interleaved CALL AND EFFECT transcript: every
// data:extend with its argument serialised canonically, every deepcopy the
// clone primitive makes, every log line the guest writes, in order -- and then
// the whole of data.raw, serialised the same way. A difference in how either
// library sorts a map, encodes a float or orders its extends moves one of them.
func TestBothDataGuestLibrariesMakeTheSameCalls(t *testing.T) {
	goDir := buildDataStageMod(t, "go")
	rsDir := buildDataStageMod(t, "rust")
	goOut := runDataStageMod(t, goDir)
	rsOut := runDataStageMod(t, rsDir)

	if goOut != rsOut {
		gl := strings.Split(goOut, "\n")
		rl := strings.Split(rsOut, "\n")
		for i := 0; i < len(gl) || i < len(rl); i++ {
			g, r := "", ""
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(rl) {
				r = rl[i]
			}
			if g != r {
				t.Fatalf("the two data guest libraries diverge at line %d:\n"+
					"  go   %s\n  rust %s", i+1, g, r)
			}
		}
		t.Fatalf("the two transcripts differ in length: go %d lines, rust %d",
			len(gl), len(rl))
	}
	// ANTI-VACUITY. Two transcripts that are both empty are identical, and a
	// build that produced no stage file at all would be exactly that.
	if !strings.Contains(goOut, "TRANSCRIPT extend") {
		t.Fatalf("the transcript records no extend call, so this compared "+
			"nothing:\n%s", goOut)
	}
	if !strings.Contains(goOut, "TRANSCRIPT deepcopy") {
		t.Fatalf("the transcript records no clone, so the primitive the whole "+
			"design turns on was never reached:\n%s", goOut)
	}
}

// buildDataStageMod compiles the example data guest in one language and
// packages it, returning the mod directory.
func buildDataStageMod(t *testing.T, lang string) string {
	t.Helper()
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	tmp := t.TempDir()

	var wasmPath string
	switch lang {
	case "go":
		wasmPath = filepath.Join(tmp, "datastage.wasm")
		if err := guest.Build(filepath.Join(root, "guest", "go"),
			"./examples/datastage", wasmPath); err != nil {
			t.Fatalf("building the Go data guest: %v", err)
		}
	case "rust":
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping the Rust arm: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "datastage",
			filepath.Join(tmp, "rust-target"))
		if err != nil {
			t.Fatalf("building the Rust data guest: %v", err)
		}
		wasmPath = p
	default:
		t.Fatalf("unknown language %q", lang)
	}

	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding the data guest: %v", err)
	}
	var imports, exports []string
	for _, im := range mod.Imports {
		imports = append(imports, im.Module+"."+im.Name)
	}
	for _, e := range mod.Exports {
		exports = append(exports, e.Name)
	}
	// THE ENFORCEMENT, on the real thing rather than on a list a test wrote:
	// a data guest that had picked up fkapi would be refused here.
	if err := factorio.CheckDataModule(imports, exports); err != nil {
		t.Fatalf("the %s data guest is not a data module: %v", lang, err)
	}
	if len(imports) == 0 {
		t.Fatal("the data guest reaches data.raw, so the module must import " +
			"fkdata; a module with no imports means the toolchain optimised the " +
			"host boundary away and this test is no longer testing it")
	}

	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("function %q did not compile: %v", f.Name, f.Unsupported)
		}
	}
	// --persist=none and -gc=leaking: a data module runs once and dies with the
	// Lua state that built it, so there is nothing to save and nothing worth
	// collecting. Same flags `fklua mod` uses for it.
	src, err := luagen.EmitModuleWith(im, luagen.Options{
		Persist: luagen.PersistNone, GC: luagen.GCLeaking,
		Roots: factorio.StageExportNames(),
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fkd-example", Version: "0.1.0", Title: "FkLua data stage",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		// A data-stage mod still needs a control chunk, because control.lua
		// requires one unconditionally. The smallest one that parses will do:
		// nothing here ever runs it.
		Chunk:       "return { exports = {} }",
		DataChunk:   src,
		DataExports: exports,
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	for _, f := range []string{"settings.lua", "data.lua", "data-final-fixes.lua"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("the packaged mod has no %s: %v", f, err)
		}
	}
	// The guest exports no fk_data_updates, so nothing should have generated one.
	if _, err := os.Stat(filepath.Join(dir, "data-updates.lua")); err == nil {
		t.Error("the guest exports no fk_data_updates and a data-updates.lua was " +
			"generated anyway")
	}
	return dir
}

// runDataStageMod runs the packaged mod's three stage files under lua52f
// against an engine-shaped stand-in, and returns the transcript.
func runDataStageMod(t *testing.T, modDir string) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	out, err := h.RunString(dataStageHarness(modDir))
	if err != nil {
		t.Fatalf("running the data stage: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// dataStageHarness is the stand-in, and every decision in it is about being the
// engine rather than being convenient.
//
//   - data:extend VALIDATES -- a table, entries with a string type and a string
//     name -- because agents/testing.md's recorded trap is a harness more
//     forgiving than the thing it stands in for, and a guest emitting a
//     prototype with no name would otherwise pass here and fail in the game.
//   - data.raw is PREPOPULATED with the base prototypes the guest reads, in the
//     shape the real ones have (a nested belt_animation_set, a technology whose
//     unit.ingredients is an array of arrays), because a clone of a flat table
//     proves nothing about a deep copy.
//   - `require("util")` works and `util` is NOT a global, which is what P2
//     measured at the settings stage.
//   - The canonical serialiser sorts keys and prints numbers at %.17g, so the
//     transcript is a function of the VALUES and not of any table's iteration
//     order or of a float's default formatting.
func dataStageHarness(modDir string) string {
	return fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end

mods = { base = "2.0.77", ["fkd-example"] = "0.1.0" }
feature_flags = { space_travel = false, quality = true }

-- defines.prototypes in the engine's own shape: base -> { derived -> 0 }.
defines = { prototypes = {
  item = { item = 0, ammo = 0, tool = 0 },
  entity = { ["transport-belt"] = 0, furnace = 0, container = 0 },
} }

-- The canonical serialiser. Sorted keys, %%.17g numbers: the transcript has to
-- be a function of the values, never of a table's iteration order.
local function ser(v)
  local t = type(v)
  if t == "number" then return string.format("%%.17g", v) end
  if t == "string" then return string.format("%%q", v) end
  if t ~= "table" then return tostring(v) end
  local ks = {}
  for k in pairs(v) do ks[#ks+1] = k end
  table.sort(ks, function(a, b)
    local ra = type(a) == "number" and 1 or 2
    local rb = type(b) == "number" and 1 or 2
    if ra ~= rb then return ra < rb end
    return a < b
  end)
  local parts = {}
  for _, k in ipairs(ks) do
    parts[#parts+1] = ser(k) .. "=" .. ser(v[k])
  end
  return "{" .. table.concat(parts, ",") .. "}"
end

data = { raw = {} }
local nextend = 0
function data:extend(list)
  nextend = nextend + 1
  if type(list) ~= "table" then error("data:extend takes a table", 0) end
  print("TRANSCRIPT extend#" .. nextend .. " " .. ser(list))
  for i, p in ipairs(list) do
    if type(p) ~= "table" then error("data:extend entry " .. i .. " is not a table", 0) end
    if type(p.type) ~= "string" then error("data:extend entry " .. i .. " has no type", 0) end
    if type(p.name) ~= "string" then error("data:extend entry " .. i .. " has no name", 0) end
    data.raw[p.type] = data.raw[p.type] or {}
    data.raw[p.type][p.name] = p
  end
end

local realrequire = require
local ndeep = 0
function require(name)
  if name == "util" then
    local function cp(v)
      if type(v) ~= "table" then return v end
      local out = {}
      for k, x in pairs(v) do out[cp(k)] = cp(x) end
      return out
    end
    return { table = { deepcopy = function(t)
      ndeep = ndeep + 1
      print("TRANSCRIPT deepcopy#" .. ndeep .. " " .. ser(t))
      return cp(t)
    end } }
  end
  return realrequire(name)
end

-- Base's own prototypes, in the shape the real ones have: nested tables and an
-- array of arrays, so a clone of one is a real deep copy.
data:extend{
  { type = "transport-belt", name = "transport-belt", speed = 0.03125,
    minable = { mining_time = 0.1, result = "transport-belt" },
    next_upgrade = "fast-transport-belt", flags = { "placeable-neutral" },
    icon = "__base__/x.png", icon_size = 64,
    animation_speed_coefficient = 32,
    collision_box = { { -0.4, -0.4 }, { 0.4, 0.4 } },
    belt_animation_set = { animation_set = { filename = "__base__/b.png", frame_count = 16 } } },
  { type = "transport-belt", name = "fast-transport-belt", speed = 0.0625,
    icon = "__base__/x.png", icon_size = 64 },
  { type = "transport-belt", name = "express-transport-belt", speed = 0.09375,
    icon = "__base__/x.png", icon_size = 64 },
  { type = "technology", name = "logistics", icon = "__base__/t.png", icon_size = 64,
    unit = { count = 20, time = 15,
             ingredients = { { "automation-science-pack", 1 } } } },
}

print("--- SETTINGS ---")
settings = nil
require("settings")

print("--- DATA ---")
settings = { startup = { ["fkd-enabled"] = { value = true } } }
require("data")

print("--- FINAL FIXES ---")
require("data-final-fixes")

local types = {}
for t in pairs(data.raw) do types[#types+1] = t end
table.sort(types)
for _, t in ipairs(types) do
  local names = {}
  for n in pairs(data.raw[t]) do names[#names+1] = n end
  table.sort(names)
  print("RAW " .. t .. ": " .. table.concat(names, " "))
end
local belt = data.raw["transport-belt"]["fkd-belt"]
print("CLONE speed=" .. tostring(belt.speed) ..
      " minable=" .. tostring(belt.minable) ..
      " next_upgrade=" .. tostring(belt.next_upgrade) ..
      " icon=" .. tostring(belt.icon) ..
      " coeff=" .. tostring(belt.animation_speed_coefficient) ..
      " anim=" .. tostring(belt.belt_animation_set.animation_set.frame_count) ..
      " flags1=" .. tostring(belt.flags[1]) ..
      " box=" .. table.concat({belt.collision_box[1][1], belt.collision_box[1][2],
                               belt.collision_box[2][1], belt.collision_box[2][2]}, "/"))
local srcbelt = data.raw["transport-belt"]["transport-belt"]
print("SOURCE box=" .. table.concat({srcbelt.collision_box[1][1], srcbelt.collision_box[1][2],
                                     srcbelt.collision_box[2][1], srcbelt.collision_box[2][2]}, "/"))
local tech = data.raw.technology["fkd-marker"]
print("TECH count=" .. tostring(tech.unit.count) .. " time=" .. tostring(tech.unit.time) ..
      " ingr=" .. tostring(tech.unit.ingredients[1][1]) .. "/" ..
      tostring(tech.unit.ingredients[1][2]) ..
      " enabled=" .. tostring(tech.enabled))
local sp = data.raw.sprite["fkd-arrow-in-n"]
print("SPRITE x=" .. tostring(sp.x) .. " shift=" .. tostring(sp.shift[1]) .. "," ..
      tostring(sp.shift[2]))
local sp2 = data.raw.sprite["fkd-arrow-out-s"]
print("SPRITE2 x=" .. tostring(sp2.x) .. " shift=" .. tostring(sp2.shift[1]) .. "," ..
      tostring(sp2.shift[2]))
print("TOKEN stack=" .. tostring(data.raw.item["fkd-token"].stack_size))
print("SETTING default=" .. tostring(data.raw["bool-setting"]["fkd-enabled"].default_value))
print("FINAL " .. ser(data.raw))
`, filepath.Join(modDir, "?.lua"))
}
