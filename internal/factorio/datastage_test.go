package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
	luart "github.com/Techrocket9/fklua/runtime"
)

// The data-stage ABI, driven through the real fk_data.lua against a real
// linear memory.
//
// WHAT IS REAL HERE AND WHAT IS NOT. The memory is a compiled wasm module's,
// with the real bounds checks, because the whole question a codec test asks is
// whether the two ends agree about bytes. The SHIM is the shipped file,
// verbatim. What is stubbed is the guest: a Lua table standing in for the
// module's exports, whose fk_data is the test body. That is deliberate rather
// than a shortcut -- it lets one test drive one import with one shape, where a
// compiled guest would have to be rebuilt per case, and the compiled guests get
// their own end-to-end test next door.
//
// THE STAND-IN `data` IS BUILT THE WAY THE ENGINE BUILDS ONE. agents/testing.md
// records the shape this repo has already been bitten by -- a harness that is
// more forgiving than the thing it stands in for tests one branch and hides the
// other -- so data:extend here VALIDATES: a table, entries with a string type
// and a string name, written into data.raw[type][name]. A stand-in that
// accepted anything would let a guest emitting a prototype with no name pass
// here and fail in the game.

// dataMemWAT is a module that exists for its linear memory. Two pages, because
// the codec allocates out of it and one page is 64 KiB.
const dataMemWAT = `(module (memory 2)
	(func (export "f") (result i32) (i32.const 0)))`

// dataStageEnv is the Lua that stands in for a data stage: the globals P2
// measured in a real Factorio, and a data:extend that validates.
const dataStageEnv = `
function log(s) print("LOG " .. s) end

mods = { base = "2.0.77", ["z-last"] = "1.0.0", ["a-first"] = "0.1.0" }
feature_flags = { space_travel = false, quality = true }

data = { raw = {} }
function data:extend(list)
  if type(list) ~= "table" then error("data:extend takes a table", 0) end
  for i, p in ipairs(list) do
    if type(p) ~= "table" then error("data:extend entry " .. i .. " is not a table", 0) end
    if type(p.type) ~= "string" then error("data:extend entry " .. i .. " has no type", 0) end
    if type(p.name) ~= "string" then error("data:extend entry " .. i .. " has no name", 0) end
    data.raw[p.type] = data.raw[p.type] or {}
    data.raw[p.type][p.name] = p
  end
end

-- Factorio's own util, near enough: a deep copy with no cycle handling, which
-- is what the engine's has too.
local realrequire = require
function require(name)
  if name == "util" then
    local function cp(v)
      if type(v) ~= "table" then return v end
      local out = {}
      for k, x in pairs(v) do out[cp(k)] = cp(x) end
      return out
    end
    return { table = { deepcopy = cp } }
  end
  return realrequire(name)
end
`

// runDataStage writes the shipped fk_data.lua plus a stand-in guest module into
// a temp directory and runs one stage.
//
// `body` is the guest: Lua with `D` bound to the fkdata import table, `H` to
// fk_abi, and `slot()` / `dyn(v)` for putting a tier-2 value somewhere the host
// can read it.
func runDataStage(t *testing.T, stage int, setup, body string) (string, error) {
	t.Helper()
	// The name the packager would have written into the stage file; env(4)
	// hands it back, and TestTheModNameReachesTheGuestThroughEnv reads it.
	return runDataStageVia(t,
		fmt.Sprintf("require(%q).run(%d, %q)\n", "fk_data", stage, "fkd-test"),
		setup, body)
}

// runDataStageVia is runDataStage with the final run() call spelled by the
// test, for the cases that exercise the call's own shape (an older stage file
// passing no mod name).
func runDataStageVia(t *testing.T, runLine, setup, body string) (string, error) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(dataMemWAT)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(ABIFile, luart.ABI())
	write(DataStageFile, luart.DataStage())
	// The stand-in guest module. It has the shape fk_data.lua expects -- a
	// factory returning exports, memio and read_string -- and its stage export
	// is the test body.
	//
	// The scratch region is deliberately SMALL (512 bytes), so a test that
	// crosses more than that exercises the fk_alloc fallback in the same run
	// rather than only the bump.
	write(DataModuleFile, `
return function(imports)
  FKD_IMPORTS = imports
  local NEXT = 1024
  return {
    memio = FKD_MEMIO,
    read_string = FKD_READSTRING,
    exports = {
      fk_alloc = function(n)
        local p = NEXT
        NEXT = NEXT + n
        if NEXT % 8 ~= 0 then NEXT = NEXT + (8 - NEXT % 8) end
        return p
      end,
      fk_free = function() end,
      fk_scratch_base = function() return 512 end,
      fk_scratch_size = function() return 512 end,
      fk_settings = function() return FKD_BODY() end,
      fk_data = function() return FKD_BODY() end,
      fk_data_updates = function() return FKD_BODY() end,
      fk_data_final_fixes = function() return FKD_BODY() end,
    },
  }
end
`)

	src := "package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		dataStageEnv +
		"local H = require(\"fk_abi\")\n" +
		"local M = (function(...)\n" + chunk + "\nend)({})\n" +
		"FKD_MEMIO, FKD_READSTRING = M.memio, M.read_string\n" +
		// A slot allocator for the TEST's own tier-2 values, above the guest's
		// bump so the two cannot collide.
		"local TOP = 40960\n" +
		"local function slot() local p = TOP; TOP = TOP + 16; return p end\n" +
		"local function dyn(v) local p = slot(); H.bind_memory(M.memio); H.bind_read_string(M.read_string)\n" +
		"  H.bind_alloc(function(n) local q = TOP; TOP = TOP + n; if TOP % 8 ~= 0 then TOP = TOP + (8 - TOP % 8) end; return q end, function() end)\n" +
		"  local st = H.write_dyn(p, v); if st ~= H.OK then error(\"write_dyn \" .. st, 0) end; return p end\n" +
		setup + "\n" +
		"FKD_BODY = function()\n  local D = FKD_IMPORTS.fkdata\n" + body + "\nend\n" +
		runLine

	out, err := h.RunString(src)
	if err != nil {
		// THE MESSAGE IS THE ASSERTION SURFACE for every raising case here, and
		// lua52f writes it to stderr, which RunString folds into the error
		// rather than into stdout. Folded back so a test can look at one string
		// -- otherwise "the message does not contain X" is reported against an
		// empty string, which is what the first draft of these did.
		out += "\n" + err.Error()
	}
	return strings.TrimSpace(out), err
}

// mustRunDataStage is runDataStage for a case that must succeed.
func mustRunDataStage(t *testing.T, stage int, setup, body string) string {
	t.Helper()
	out, err := runDataStage(t, stage, setup, body)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	return out
}

// Every tier-2 shape a prototype actually contains, out and back.
//
// A prototype is nested maps, arrays of maps, floats and strings, and the codec
// has to carry all of it in BOTH directions -- extend goes in, get comes out --
// or a mod that loads produces a prototype nobody wrote.
func TestTheDataStageCodecRoundTripsEveryShape(t *testing.T) {
	out := mustRunDataStage(t, 2, "", `
  D.extend(dyn({{
    type = "furnace", name = "shapes",
    stack_size = 42,
    speed = 0.09375,
    negative = -1.5,
    truthy = true, falsy = false,
    empty_string = "",
    nested = { a = { b = { c = "deep" } } },
    list_of_maps = { { x = 1, y = 2 }, { x = 3, y = 4 } },
    mixed = { "one", 2, true },
    empty_table = {},
  }}))
  local got = slot()
  D.get(dyn({"furnace", "shapes"}), got)
  local v = H.read_dyn(got)
  print("stack_size " .. tostring(v.stack_size))
  print("speed " .. string.format("%.5f", v.speed))
  print("negative " .. tostring(v.negative))
  print("truthy " .. tostring(v.truthy) .. " falsy " .. tostring(v.falsy))
  print("empty_string [" .. tostring(v.empty_string) .. "]")
  print("deep " .. tostring(v.nested.a.b.c))
  print("list " .. tostring(v.list_of_maps[1].x) .. "," .. tostring(v.list_of_maps[2].y))
  print("mixed " .. tostring(v.mixed[1]) .. "," .. tostring(v.mixed[2]) .. "," .. tostring(v.mixed[3]))
  print("empty #" .. tostring(#v.empty_table) .. " next=" .. tostring(next(v.empty_table)))
`)
	want := []string{
		"stack_size 42",
		"speed 0.09375",
		"negative -1.5",
		"truthy true falsy false",
		"empty_string []",
		"deep deep",
		"list 1,4",
		"mixed one,2,true",
		"empty #0 next=nil",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// The determinism rule, and it is an IMPOSSIBILITY rather than a refusal: the
// host sorts, so a guest cannot see a pairs() order however it asks.
//
// TWELVE KEYS, AND THE NUMBER IS THE TEST'S OWN ANTI-VACUITY ARGUMENT. `keys`
// sorting and `keys` handing pairs() straight over are indistinguishable
// whenever pairs() happens to come out sorted, which is the shape this repo
// calls a gate that cannot fail -- and it is not hypothetical here:
//
//	bin/lua52f's pairs() ORDER OVER STRING KEYS VARIES BETWEEN RUNS. Lua
//	5.2 seeds its string hash from the clock, so the same four-key table
//	iterated in four different orders across six consecutive invocations,
//	and the first draft of this test (four belt names) passed one run and
//	tripped its own vacuity guard the next.
//
// That is a property of the ORACLE and not of Factorio, whose data stage is
// insertion-ordered and whose --dump-data is byte-identical across runs. It
// makes no difference to the ABI, which sorts either way; it makes every
// difference to a host-side test that reasons about a fixture's order. With
// twelve keys a chance sort is one in 12!, so the guard below is a real
// tripwire rather than a coin toss.
func TestKeysAreSortedNotPairsOrder(t *testing.T) {
	setup := `
local names = { "turbo-belt", "fast-belt", "belt", "express-belt", "ultra-belt",
                "slow-belt", "green-belt", "red-belt", "blue-belt", "gold-belt",
                "iron-belt", "copper-belt" }
for i, n in ipairs(names) do
  data:extend{ { type = "transport-belt", name = n, speed = i / 32 } }
end
`
	out := mustRunDataStage(t, 2, setup, `
  local got = slot()
  D.keys(dyn({"transport-belt"}), got)
  print("keys " .. table.concat(H.read_dyn(got), " "))
  -- What pairs() itself yields, so the assertion below compares the sorted
  -- answer against a REAL iteration order rather than against an idea of one.
  local raw = {}
  for k in pairs(data.raw["transport-belt"]) do raw[#raw+1] = k end
  print("pairs " .. table.concat(raw, " "))
`)
	const wantSorted = "keys belt blue-belt copper-belt express-belt fast-belt gold-belt " +
		"green-belt iron-belt red-belt slow-belt turbo-belt ultra-belt"
	if !strings.Contains(out, wantSorted) {
		t.Errorf("keys are not sorted:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "pairs ") {
			continue
		}
		if strings.TrimPrefix(line, "pairs ") == strings.TrimPrefix(wantSorted, "keys ") {
			t.Fatalf("this run's pairs() order IS sorted order over twelve keys, so "+
				"this test could not have failed and proves nothing about keys(). "+
				"One run in 12! -- re-run it, and if it repeats, the fixture is "+
				"wrong.\n%s", out)
		}
	}
}

// A dictionary a GET returns is sorted too, at every level, for the same reason
// keys() is: a guest that walks one must not see an order that is a fact about
// how the mods happened to load.
func TestAReturnedDictionaryIsSortedAtEveryLevel(t *testing.T) {
	out := mustRunDataStage(t, 2, `
data:extend{ { type = "furnace", name = "n", zulu = 1, alpha = 2,
               inner = { zebra = 1, aardvark = 2, middle = 3 } } }
`, `
  local got = slot()
  D.get(dyn({"furnace", "n"}), got)
  -- Read the wire directly rather than through read_dyn, which rebuilds a Lua
  -- table and loses the order the host wrote.
  local IO = FKD_MEMIO
  local function keysAt(p)
    local n = IO.ld32(p + 12)
    local base = IO.ld32(p + 8)
    local out = {}
    for i = 0, n - 1 do
      out[#out+1] = tostring(H.read_dyn(base + i * H.DYNPW))
    end
    return table.concat(out, " ")
  end
  print("top " .. keysAt(got))
  local n = IO.ld32(got + 12)
  local base = IO.ld32(got + 8)
  for i = 0, n - 1 do
    if H.read_dyn(base + i * H.DYNPW) == "inner" then
      print("inner " .. keysAt(base + i * H.DYNPW + H.DYNW))
    end
  end
`)
	if !strings.Contains(out, "top alpha inner name type zulu") {
		t.Errorf("the top level is not sorted:\n%s", out)
	}
	if !strings.Contains(out, "inner aardvark middle zebra") {
		t.Errorf("a nested dictionary is not sorted:\n%s", out)
	}
}

// Setting nil DELETES, and it has to: stripping a cloned prototype is a list of
// deletions, and a "write false" reading of an absent value leaves every one of
// them present-and-false in the dump.
func TestSetWithNilDeletesRatherThanWritingFalse(t *testing.T) {
	out := mustRunDataStage(t, 2, `
data:extend{ { type = "transport-belt", name = "b", speed = 1, minable = { x = 1 },
               next_upgrade = "other", nested = { keep = 1, drop = 2 } } }
`, `
  D.set(dyn({"transport-belt", "b", "minable"}), 0)
  D.set(dyn({"transport-belt", "b", "next_upgrade"}), 0)
  D.set(dyn({"transport-belt", "b", "nested", "drop"}), 0)
  D.set(dyn({"transport-belt", "b", "speed"}), dyn(0.25))
  local b = data.raw["transport-belt"]["b"]
  print("minable " .. tostring(b.minable))
  print("next_upgrade " .. tostring(b.next_upgrade))
  print("nested.drop " .. tostring(b.nested.drop) .. " nested.keep " .. tostring(b.nested.keep))
  print("speed " .. tostring(b.speed))
`)
	for _, w := range []string{
		"minable nil", "next_upgrade nil",
		"nested.drop nil nested.keep 1", "speed 0.25",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// Clone is the engine's own deep copy, and the leaves it does not touch are
// literally the bytes the source had.
//
// THIS IS THE DEFECT CLASS THE PRIMITIVE EXISTS TO PREVENT, so the fixture is
// built to catch a SHALLOW copy specifically: the patch mutates a field of a
// nested table, and under a shallow copy that mutation lands in the SOURCE.
func TestCloneIsDeepAndKeepsUntouchedLeaves(t *testing.T) {
	out := mustRunDataStage(t, 2, `
data:extend{ { type = "transport-belt", name = "src", speed = 0.03125,
               icon = "__base__/x.png", icon_size = 64,
               animation = { frame_count = 16, filename = "__base__/b.png",
                             layers = { { tint = { r = 1, g = 0.5 } } } } } }
`, `
  D.clone(dyn({"transport-belt", "src"}), dyn({"transport-belt", "dst"}))
  D.set(dyn({"transport-belt", "dst", "speed"}), dyn(0.25))
  D.set(dyn({"transport-belt", "dst", "animation", "frame_count"}), dyn(32))
  local src, dst = data.raw["transport-belt"]["src"], data.raw["transport-belt"]["dst"]
  print("name " .. dst.name .. " type " .. dst.type)
  print("speed src " .. tostring(src.speed) .. " dst " .. tostring(dst.speed))
  print("frames src " .. tostring(src.animation.frame_count) ..
        " dst " .. tostring(dst.animation.frame_count))
  print("shared " .. tostring(rawequal(src.animation, dst.animation)))
  print("leaves icon=" .. tostring(dst.icon) .. " size=" .. tostring(dst.icon_size) ..
        " file=" .. tostring(dst.animation.filename) ..
        " tint=" .. tostring(dst.animation.layers[1].tint.g))
`)
	for _, w := range []string{
		"name dst type transport-belt",
		"speed src 0.03125 dst 0.25",
		"frames src 16 dst 32",
		"shared false",
		"leaves icon=__base__/x.png size=64 file=__base__/b.png tint=0.5",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// Cloning across TYPES needs no second primitive, because the destination is a
// path rather than a name.
func TestCloneCrossesPrototypeTypes(t *testing.T) {
	out := mustRunDataStage(t, 2, `
data:extend{ { type = "transport-belt", name = "src", speed = 1 } }
`, `
  D.clone(dyn({"transport-belt", "src"}), dyn({"underground-belt", "dst"}))
  local d = data.raw["underground-belt"]["dst"]
  print("type " .. d.type .. " name " .. d.name .. " speed " .. tostring(d.speed))
`)
	if !strings.Contains(out, "type underground-belt name dst speed 1") {
		t.Errorf("a cross-type clone did not land:\n%s", out)
	}
}

// The one non-failure the status return exists for.
//
// "Is this prototype already defined" is a NORMAL question -- it is what a mod
// adopting an uninstalled neighbour's entities asks on every load -- so it comes
// back as a status rather than as a raise, and the value is nil.
func TestAGetOfAMissingKeyIsAbsentRatherThanAnError(t *testing.T) {
	out := mustRunDataStage(t, 2, `
data:extend{ { type = "item", name = "there", stack_size = 1 } }
`, `
  local got = slot()
  print("there " .. tostring(D.get(dyn({"item", "there"}), got)) ..
        " value " .. type(H.read_dyn(got)))
  print("missing " .. tostring(D.get(dyn({"item", "not-there"}), got)) ..
        " value " .. tostring(H.read_dyn(got)))
  print("deep-missing " .. tostring(D.get(dyn({"item", "there", "nope", "deeper"}), got)))
  print("no-type " .. tostring(D.get(dyn({"no-such-type", "x"}), got)))
`)
	for _, w := range []string{
		"there 0 value table",
		"missing 1 value nil",
		"deep-missing 1",
		"no-type 1",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// The three env reads, and the sorting that makes mods enumerable.
func TestTheEnvironmentReadsAreSorted(t *testing.T) {
	out := mustRunDataStage(t, 2, `
settings = { startup = { ["b-second"] = { value = 7 }, ["a-first"] = { value = "x" } } }
`, `
  local IO = FKD_MEMIO
  local function pairsAt(p)
    local n, base = IO.ld32(p + 12), IO.ld32(p + 8)
    local out = {}
    for i = 0, n - 1 do
      out[#out+1] = tostring(H.read_dyn(base + i * H.DYNPW)) .. "=" ..
                    tostring(H.read_dyn(base + i * H.DYNPW + H.DYNW))
    end
    return table.concat(out, " ")
  end
  local got = slot()
  D.env(1, got) print("mods " .. pairsAt(got))
  D.env(2, got) print("flags " .. pairsAt(got))
  D.env(3, got) print("startup " .. pairsAt(got))
`)
	for _, w := range []string{
		"mods a-first=0.1.0 base=2.0.77 z-last=1.0.0",
		"flags quality=true space_travel=false",
		"startup a-first=x b-second=7",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// At the SETTINGS stage `settings` does not exist -- a mod's startup settings
// are not readable while they are being declared -- and that answers with an
// empty map rather than raising, because there being none is a fact rather than
// a fault.
func TestStartupSettingsAreEmptyAtTheSettingsStage(t *testing.T) {
	out := mustRunDataStage(t, 1, "settings = nil", `
  local got = slot()
  D.env(3, got)
  local v = H.read_dyn(got)
  print("startup type " .. type(v) .. " n " .. tostring(#v) .. " next " .. tostring(next(v)))
  print("stage " .. tostring(D.stage()))
`)
	if !strings.Contains(out, "startup type table n 0 next nil") {
		t.Errorf("settings.startup should read empty at the settings stage:\n%s", out)
	}
	if !strings.Contains(out, "stage 1") {
		t.Errorf("the stage id did not reach the guest:\n%s", out)
	}
}

// The mod's OWN name reaches the guest through env(4), and it comes from the
// PACKAGER rather than the engine: the data-stage environment has no "current
// mod" anywhere -- `mods` is a flat all-mods dictionary with no self marker --
// so the generated stage file's run() call carries it. What it is FOR is
// namespacing: a same-type setting-name collision between two mods is silent
// last-writer-wins in the engine, so a library that generates settings must
// prefix them, and this is the one source of the prefix that cannot drift from
// the packaged mod.
func TestTheModNameReachesTheGuestThroughEnv(t *testing.T) {
	out := mustRunDataStage(t, 2, "", `
  local got = slot()
  print("st " .. tostring(D.env(4, got)) .. " modname " .. tostring(H.read_dyn(got)))
`)
	if !strings.Contains(out, "st 0 modname fkd-test") {
		t.Errorf("env(4) did not answer the packaged name:\n%s", out)
	}
}

// A stage file that passes no name -- one written by an fklua older than the
// argument -- reads as nil rather than raising. The stage files and the shim
// ship together, so the pair cannot actually skew; a shim that raised on the
// old spelling would turn "cannot happen" into a load failure if it ever did.
func TestAnAbsentModNameReadsAsNil(t *testing.T) {
	out, err := runDataStageVia(t, `require("fk_data").run(2)`+"\n", "", `
  local got = slot()
  print("st " .. tostring(D.env(4, got)) .. " modname " .. tostring(H.read_dyn(got)))
`)
	if err != nil {
		t.Fatalf("an old-style run() call must not raise: %v\n%s", err, out)
	}
	if !strings.Contains(out, "st 0 modname nil") {
		t.Errorf("an absent mod name should read as nil:\n%s", out)
	}
}

// Errors RAISE at the stage, with the stage's name and the offending path.
//
// That is the deliberate deviation from the control ABI, and the message is
// what makes it worth having: a data-stage failure should stop the load and
// name the thing to fix, which is Factorio's own convention.
func TestAFailureRaisesNamingTheStageAndThePath(t *testing.T) {
	cases := []struct {
		name, stage, setup, body string
		want                     []string
	}{
		{
			name:  "an intermediate that is not there",
			setup: `data:extend{ { type = "item", name = "i", stack_size = 1 } }`,
			body:  `D.set(dyn({"item", "i", "missing", "deeper"}), dyn(1))`,
			want:  []string{"data stage", `data.raw["item"]["i"]["missing"]`, "is nil"},
		},
		{
			name: "extend given one prototype rather than an array",
			body: `D.extend(dyn({ type = "item", name = "i" }))`,
			want: []string{"data stage", "array of them"},
		},
		{
			name: "clone of something that is not defined",
			body: `D.clone(dyn({"item", "nope"}), dyn({"item", "copy"}))`,
			want: []string{"data stage", `data.raw["item"]["nope"]`, "nothing to clone"},
		},
		{
			name:  "clone given a path that is not {type, name}",
			setup: `data:extend{ { type = "item", name = "i", stack_size = 1 } }`,
			body:  `D.clone(dyn({"item", "i", "stack_size"}), dyn({"item", "copy"}))`,
			want:  []string{"data stage", "exactly {type, name}"},
		},
		{
			name: "a path element that is not a string or a number",
			body: `D.get(dyn({"item", true}), slot())`,
			want: []string{"data stage", "element 2 is a boolean"},
		},
		{
			name: "env asked for something that is not one of the three",
			body: `D.env(9, slot())`,
			want: []string{"data stage", "env was asked for 9"},
		},
		{
			name:  "keys of something that has none",
			setup: `data:extend{ { type = "item", name = "i", stack_size = 1 } }`,
			body:  `D.keys(dyn({"item", "i", "stack_size"}), slot())`,
			want:  []string{"data stage", "is a number, which has no keys"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runDataStage(t, 2, c.setup, c.body)
			if err == nil {
				t.Fatalf("expected a raise, got:\n%s", out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("the message does not contain %q:\n%s", w, out)
				}
			}
		})
	}
}

// A table key that is neither a number nor a string cannot be ordered the same
// way on every client -- two table keys can only be compared by their addresses
// -- so it raises rather than being sorted by something that is not stable.
func TestAnUnorderableKeyRaisesRatherThanBeingSorted(t *testing.T) {
	out, err := runDataStage(t, 2, `
local weird = {}
weird[{}] = 1
data:extend{ { type = "item", name = "i", stack_size = 1, weird = weird } }
`, `D.get(dyn({"item", "i"}), slot())`)
	if err == nil {
		t.Fatalf("expected a raise, got:\n%s", out)
	}
	if !strings.Contains(out, "key of type table") {
		t.Errorf("the message does not name the key's type:\n%s", out)
	}
}

// A stage whose export the guest does not have says so rather than doing
// nothing. It cannot happen through the packager -- the stage file is generated
// only for an exported hook -- so this is about a hand-edited mod and about a
// guest rebuilt without a hook whose stage file survived.
func TestAMissingStageExportSaysSo(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		ABIFile:       luart.ABI(),
		DataStageFile: luart.DataStage(),
		DataModuleFile: `return function(imports)
  return { memio = { st32 = function() end }, read_string = function() return "" end,
           exports = { fk_alloc = function() return 0 end } }
end`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := h.RunString("package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		"function log(s) print(s) end\n" +
		`require("fk_data").run(3)`)
	if err == nil {
		t.Fatalf("expected a raise, got:\n%s", out)
	}
	out += "\n" + err.Error()
	for _, w := range []string{"data-updates stage", "fk_data_updates"} {
		if !strings.Contains(out, w) {
			t.Errorf("the message does not contain %q:\n%s", w, out)
		}
	}
}

// The bidirectional mirror, which is the one that actually drifted for the
// CONTROL hooks: factorio.Hooks matched control.lua for every hook it listed and
// had been missing one for two milestones, so only the listed->registered
// direction was ever checked and the other never was.
func TestEveryStageHookIsRegisteredByTheShim(t *testing.T) {
	shim := luart.DataStage()
	for _, h := range StageHooks {
		if !strings.Contains(shim, `"`+h.Export+`"`) {
			t.Errorf("%s is in StageHooks and %s never names it, so a guest "+
				"exporting it would be reported as wired and never called",
				h.Export, DataStageFile)
		}
	}
	if len(StageHooks) == 0 {
		t.Fatal("StageHooks is empty, so this test audited nothing")
	}
}

func TestEveryExportTheShimCallsIsListedInStageHooks(t *testing.T) {
	// The shim names its exports in one table, as quoted strings; anything of
	// the fk_ family in there has to be a stage hook or the two have drifted.
	shim := luart.DataStage()
	start := strings.Index(shim, "local STAGE_EXPORT = {")
	if start < 0 {
		t.Fatalf("%s no longer has a STAGE_EXPORT table, so this mirror cannot "+
			"be checked -- fix the test rather than deleting it", DataStageFile)
	}
	end := strings.Index(shim[start:], "}")
	if end < 0 {
		t.Fatal("STAGE_EXPORT is not closed")
	}
	block := shim[start : start+end]
	known := map[string]bool{}
	for _, h := range StageHooks {
		known[h.Export] = true
	}
	n := 0
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if !strings.HasPrefix(line, `"fk_`) {
			continue
		}
		name := strings.Trim(line, `"`)
		n++
		if !known[name] {
			t.Errorf("%s calls %s and StageHooks does not list it, so no stage "+
				"file would ever be generated for it", DataStageFile, name)
		}
	}
	if n != len(StageHooks) {
		t.Errorf("%s names %d exports and StageHooks has %d", DataStageFile, n,
			len(StageHooks))
	}
}

// luaQuote is defined in marshal_test.go; this is a compile-time reminder that
// these tests share it.
var _ = fmt.Sprintf
