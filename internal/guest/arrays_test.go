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

// Arrays, end to end, with values crossing in both directions.
//
// TestGeneratedBindingsCompile cannot reach this: TinyGo removes every member
// the guest does not call, so it proves the array encoders type-check and stops
// there. Every array bug that matters -- a stride off by the element's padding,
// a count read from the pointer's offset, a free of the wrong address -- lives
// past the type checker and is only visible when the numbers come back.
//
// The host side is a stub, deliberately. It is the real generated member table
// and the real fk_abi.lua dispatching into it; only the Factorio objects at the
// far end are fake, because a headless game has no connected players and an
// empty array is exactly the case that proves nothing.
func TestArraysCrossInBothDirections(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		root, tmp := repoRoot(t), t.TempDir()
		p := filepath.Join(tmp, "array.wasm")
		if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/array", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkArrayGuest(t, p)
	})
	// The SAME stub and the SAME expectations drive the Rust guest, which makes
	// this a runtime exercise of the generated bindings and a differential
	// check at once. The compile gate type-checks every bound member
	// (`rust_members_bound` in census.json) and cannot see one wrong offset;
	// only this can.
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		root, tmp := repoRoot(t), t.TempDir()
		p, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "array",
			filepath.Join(tmp, "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkArrayGuest(t, p)
	})
}

func checkArrayGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	root := repoRoot(t)
	tmp := t.TempDir()
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
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("function %q did not compile: %v", f.Name, f.Unsupported)
		}
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
		t.Fatal("a member id was not a compile-time constant, so this guest " +
			"would ship the whole table -- which means the id scan broke")
	}
	report = report.Only(used)
	table, err := report.LuaSourceWith(a, events)
	if err != nil {
		t.Fatal(err)
	}

	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-array", Version: "0.1.0", Title: "FkLua arrays",
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

	out, err := h.RunString(arrayStub(dir))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	// Every number here is checkable against the stub below.
	want := []string{
		"LOG handles: 3",
		"LOG strings: 2 wheat barley",
		// An OPTIONAL array observed ABSENT rather than empty, on an object that
		// simply does not carry it. Go says nil and Rust says None, and the two
		// print the same line -- which is what makes this a differential check
		// of FTS1 rather than two independent claims.
		"LOG optional array: absent",
		"LOG structs: 3 (1.5,-2.5) (10.0,20.0) (0.0,0.0)",
		"LOG dict: 3 water=250.0 steam=0.0",
		// The guest's two writes, echoed by the stub. The second is the empty
		// case: nil must arrive as a zero-length table, not as two categories
		// read off a stale pointer.
		"LOG host saw categories: basic-solid,hard-solid",
		"LOG host saw categories: (empty)",
		// A LocalisedString: a table of a string and a nested table. Tier 2
		// carries it with no generated type for the shape.
		"LOG dyn in: ['entity-name.iron-chest' 3 ['x' true]]",
		// The guest sent [true nil] and Lua received [true]. That is not a
		// marshalling bug: a nil inside a Lua sequence IS the end of the
		// sequence, so the hole and everything after it are unreachable. Tier 2
		// cannot fix it and does not pretend to.
		"LOG host saw ghost: ['item-name.iron-plate' 42 [true]]",
		// Two array fields at different offsets in one struct: a decoder that
		// read both headers from the first field's slot would say 2 and 2.
		"LOG struct arrays: inputs=2 outputs=1",
		// A variant-parameter-group method: the argument table is a
		// discriminated union, so it crosses as one tier-2 value. The stub
		// echoes the keys it received, sorted, because a Lua table has no
		// order a test may rely on.
		"LOG host saw create_entity: bar=4 force='player' name='iron-chest'",
		"LOG create_entity returned an entity",
		// THE DESTINATION-SLICE VARIANT. Same member, same host call: what is
		// asserted is that the elements are the ones the allocating form
		// returns AND that a destination big enough is not reallocated. Either
		// half alone passes for a variant that is wrong in the other way.
		"LOG into strings: 2 wheat barley",
		"LOG into: 3 same-buffer=yes",
		// ...and a destination too small has to grow. Only the count is
		// asserted: Go returns a new slice and Rust grows the caller's Vec in
		// place, so "who owns the new buffer" is the one thing the two mirrors
		// cannot say in the same words.
		"LOG into grown: 3",
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], want[i])
		}
	}
}

// arrayStub is factorioStub plus a `game` whose members return arrays.
//
// The three objects behind connected_players are the same table, which is what
// lets one array of handles reach members of three different classes: a handle
// carries no type, and the class a guest wraps it in only decides which member
// id it sends.
func arrayStub(modDir string) string {
	return fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_tick = 1 } }
local handlers = {}
script = {
  mod_name = "fk-array",
  on_init = function(f) handlers.on_init = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

-- render mirrors the guest's own printer, so the two sides are compared in one
-- notation rather than by eye.
local function render(v)
  local t = type(v)
  if v == nil then return "nil" end
  if t == "boolean" then return tostring(v) end
  -- tostring, not string.format: this whole stub goes through fmt.Sprintf, so
  -- a %%d here would be read as a Go verb. Lua 5.2 formats a number with
  -- "%%.14g", which prints 3.0 as "3" -- the same as Go's FormatFloat(-1).
  if t == "number" then return tostring(v) end
  if t == "string" then return "'" .. v .. "'" end
  if t == "table" then
    local out, n = {}, 0
    for _, e in ipairs(v) do n = n + 1 out[n] = render(e) end
    return "[" .. table.concat(out, " ") .. "]"
  end
  return "?"
end

-- A SECOND OBJECT WITH NOTHING ON IT, so that an OPTIONAL array can be observed
-- ABSENT rather than empty. accepted_seeds is declared optional, so nil here
-- is a value (M.invoke's opt path) and not ERR_NO_MEMBER -- which is the whole
-- of what fklua-ports' fuel-train-stop reported as FTS1, and what the two
-- guests now print one identical line about.
local bare = { valid = true }

local thing
thing = {
  valid = true,
  -- The handle the absent-optional probe reaches bare through. Any member
  -- returning one object would do; last_user is optional, so the guests also
  -- have to unwrap it, which is the ordinary optional-HANDLE shape beside the
  -- optional-ARRAY one.
  last_user = bare,
  -- []string
  accepted_seeds = { "wheat", "barley" },
  -- []struct, including a zero element: a decoder that skips falsy values
  -- would drop it, and (0,0) is a real position.
  autopilot_destinations = {
    { x = 1.5, y = -2.5 }, { x = 10.0, y = 20.0 }, { x = 0.0, y = 0.0 },
  },
  -- A dictionary, and a method rather than an attribute. steam is present and
  -- ZERO: the guest looks keys up by name, so a decoder that dropped a falsy
  -- value would read the same 0.0 -- which is why the COUNT is checked too.
  get_fluid_contents = function()
    return { water = 250.0, steam = 0.0, ["heavy-oil"] = 12.5 }
  end,
  -- A tier-2 value nested two deep, with a boolean and a number beside the
  -- strings so the tag dispatch is exercised rather than just the string case.
  ghost_localised_name = { "entity-name.iron-chest", 3, { "x", true } },
}
-- A struct holding two arrays, deliberately of DIFFERENT lengths. Assigned
-- AFTER the constructor, because the local is still nil inside its own
-- initialiser -- writing this inline built two empty tables and read as a
-- decoder bug for a good ten minutes.
rawset(thing, "belt_neighbours", { inputs = { thing, thing }, outputs = { thing } })
-- The setter echoes what actually arrived, which is the only way to see an
-- array that crossed OUT. __newindex fires only for keys the table does not
-- have, so this deliberately never rawsets: the key stays absent and the
-- SECOND write is observed too, which is the empty case.
setmetatable(thing, { __newindex = function(tbl, k, v)
  if k == "cursor_ghost" then
    log("host saw ghost: " .. render(v))
    return
  end
  if k == "character_additional_mining_categories" then
    if #v == 0 then
      log("host saw categories: (empty)")
    else
      log("host saw categories: " .. table.concat(v, ","))
    end
    return
  end
  rawset(tbl, k, v)
end })

rawset(thing, "create_entity", function(args)
  local keys, n = {}, 0
  for k in pairs(args) do n = n + 1 keys[n] = k end
  table.sort(keys)
  local out = {}
  for i, k in ipairs(keys) do out[i] = k .. "=" .. render(args[k]) end
  log("host saw create_entity: " .. table.concat(out, " "))
  return thing
end)

game = { valid = true, connected_players = { thing, thing, thing } }

require("control")

if handlers.on_init then handlers.on_init() end
`, filepath.Join(modDir, "?.lua"))
}
