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

// THE HANDLE OWNERSHIP SURFACE, in both languages, against one expectation.
//
// Two things had no answer on the guest side. A guest could not ASK which space
// a handle was in, so a double retain was silent and an author reasoning about
// ownership had only the raw number; and on the Rust side retain and release
// had to be balanced by hand on every path, which the first Rust guest holding
// handles across events got wrong on three of them.
//
// The predicates are mirrored -- Persistent/Transient/Global in Go,
// is_persistent/is_transient/is_global in Rust -- and the guard is not: Rust
// gets Object::retained, Go gets `defer o.Release()`, because Go has no
// destructor and a wrapper type would only hide the same defer. That asymmetry
// is the reason this test exists in the shape it does: what it requires is that
// the two idioms produce the same OBSERVABLE, line for line, through the
// verbatim fk_mod.lua and fk_abi.lua.
//
// THE SLOT NUMBERS ARE THE ASSERTION, and that is deliberate rather than
// convenient. There is no accessor for the size of the host's handle table and
// this round did not add one: it would be a new host import -- an ABI change
// touching fk_mod.lua, both fk libraries and the report surface -- for a test's
// convenience. The slot index a retain hands back is a deterministic proxy
// instead. The persistent free list is LIFO during play, so a released slot is
// the very next one handed out; a release that did not happen shows up as the
// next retain taking a NEW number.
//
// "BOTH LANGUAGES" IS NOT "BOTH LANGUAGES COVER EVERYTHING", and the transcript
// is split so it says so rather than implying otherwise. handlesWant is the
// shared part and both arms owe every line of it. handlesWantRust is owed by the
// Rust arm alone, because what it asserts -- that retained() REFUSES every
// handle a guard cannot own, so a second owner of one slot is unrepresentable --
// is an API with nothing on the Go side to mirror. The same asymmetry already
// applied to into_object, which the Go example can only approximate with a plain
// retain whose release it keeps: the kept/after-keep pair asserts ownership
// transfer on the Rust arm and only "a retain allocates" on the Go one.
func TestHandleOwnershipReadsTheSameInBothLanguages(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p := filepath.Join(t.TempDir(), "handles.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/handles", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkHandlesGuest(t, p, handlesWant)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "handles",
			filepath.Join(t.TempDir(), "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkHandlesGuest(t, p, append(append([]string(nil), handlesWant...), handlesWantRust...))
		t.Run("a guard parked in a static survives a save", func(t *testing.T) {
			checkGuardSurvivesASave(t, p)
		})
	})
}

// handlesWant is the transcript both languages owe.
//
// Every number in it is derivable from fk_abi.lua alone: the persistent space
// starts at 10, a retain takes the top of the free list when there is one and
// the next fresh slot when there is not, and a release pushes.
var handlesWant = []string{
	// BOTH SPLIT CONSTANTS, over hand-built handles rather than over whatever
	// the host happened to hand out. 9/10 straddle the global boundary and
	// 1073741823/1073741824 straddle the transient one, so a constant off by one
	// in either direction moves this line -- which is what makes it the pin the
	// guest side owes, beside the host-side one against fk_abi.lua itself.
	"LOG handles: classify 0=none 1=global 9=global 10=persistent " +
		"1073741823=persistent 1073741824=transient 4294967295=transient",
	// What the API hands back is transient, and the space is the claim: the
	// number itself is the host's business.
	"LOG handles: fresh persistent=false transient=true global=false",
	// A retain moves it, and 10 is the first slot the persistent space has.
	"LOG handles: retained slot=10 persistent=true transient=false global=false",
	// IDEMPOTENCE. A second retain of a persistent handle hands the same number
	// back rather than allocating a second slot onto one object, so it does not
	// LEAK -- and it is what a guest with no predicate could not check for
	// itself. It buys no ownership either: there is still one slot, and the
	// release that pairs with the second retain frees whatever the next retain
	// took, which is TestReleasingOneSlotTwiceFreesAnotherOwnersObject in
	// internal/factorio. Release a slot exactly once.
	"LOG handles: idempotent slot=10",
	// The guard took the next free slot...
	"LOG handles: guard slot=11",
	// ...and gave it back when its scope ended. Rust's Drop and Go's defer, one
	// observable: the next retain gets 11 rather than 12.
	"LOG handles: after release slot=11",
	// into_object takes the handle out of the guard WITHOUT releasing it, so
	// this slot is nobody's until something releases it...
	"LOG handles: kept slot=12",
	// ...and the next retain therefore takes a NEW slot. A release hiding in
	// into_object would make this 12.
	"LOG handles: after keep slot=13",
	// A global outlives the dispatch and is still not a slot this guest owns:
	// releasing one is ERR_BAD_HANDLE, which is why is_persistent answers false.
	"LOG handles: global persistent=false transient=false global=true",
	// And the null handle is in no space at all.
	"LOG handles: null persistent=false transient=false global=false",
}

// handlesWantRust is what the RUST arm owes on top of the shared transcript,
// because retained() returning an Option is an API Go has no half of.
//
// THE SHAPE IT PINS IS THE ONE THE GUARD EXISTS TO MAKE UNREPRESENTABLE.
// Object is Copy and Deref hands one out, so `Retained::new(*guard)` compiled
// and produced a SECOND guard over ONE slot -- retain is idempotent for a handle
// already persistent, so it allocated nothing and both guards released the same
// number. That is not a benign double release: the free list is LIFO, so the
// slot a dropped guard just freed is the very next one handed out, and the
// second release then frees the slot an unrelated guard owns. Two live owners,
// one slot, one of them reading somebody else's object, and no status anywhere.
//
// retained() now takes ONLY a transient handle, for which the host mints a fresh
// slot on every retain, so both spellings of the second guard answer None and
// the unrelated guard keeps its slot and still answers a call. The host-level
// probe that shows what the raw release pair does when that None is bypassed is
// internal/factorio's TestReleasingOneSlotTwiceFreesAnotherOwnersObject.
var handlesWantRust = []string{
	// The four refusals, in one line and each a shape retained() used to
	// accept. `owned` is the into_object slot above, which somebody is already
	// managing by hand; `failed` is a retain that came back 0, which used to
	// hand back a guard over nothing -- the third leak in the report, rebuilt
	// inside the new API.
	"LOG guard: refuses owned=true global=true null=true failed=true",
	// A is the first slot after 13, so the arithmetic above is still the
	// arithmetic here: none of the four refusals allocates.
	"LOG aba: A owns slot=14",
	// Both spellings of the second guard over A's own slot.
	"LOG aba: second guard over A new=false retained=false",
	// A dropped, so C takes 14 back -- and C is a DIFFERENT object's guard,
	// which is what made the old shape ABA rather than a double free. C still
	// owns it, and proves so by CALLING something rather than by holding a
	// number.
	"LOG aba: C owns slot=14 reused=true nauvis=true",
	// C's scope ended, so the guard parked for the second session takes 14 in
	// its turn. Read back by checkGuardSurvivesASave after a load.
	"LOG save: parked slot=14",
}

func checkHandlesGuest(t *testing.T, wasmPath string, handlesWant []string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageHandlesGuest(t, wasmPath, luagen.Options{})
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
-- on_tick is here because the Rust guest exports fk_on_tick for the save leg
-- below; nothing in THIS session raises it, and the Go guest does not export it.
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-handles",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}

-- A FRESH stand-in surface per call, which is what makes each retain allocate
-- its own slot: the handle table wraps whatever it is handed in a NEW transient
-- handle every time, and a retain of each one is a separate promotion. Returning
-- one shared table would work too -- the host does not dedupe -- but a fresh one
-- says out loud that nothing here is relying on identity.
game = { valid = true, get_surface = function(_) return { valid = true, name = "nauvis" } end }
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
	if len(got) != len(handlesWant) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(handlesWant), len(got), out)
	}
	for i := range handlesWant {
		if got[i] != handlesWant[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], handlesWant[i])
		}
	}
}

// THE GUARD'S ACROSS-A-SAVE PARAGRAPH, EXERCISED RATHER THAN ARGUED.
//
// The Retained doc says a guard parked in a static comes back after a load still
// naming its slot, because the guest heap is what Factorio serializes and the
// host's persistent table is aliased into storage beside it. The transcript
// above cannot say anything about that: it runs entirely inside one on_init and
// never reaches M.adopt, which is the half of the model that rebuilds the free
// list ASCENDING from the saved table.
//
// So: two sessions in one interpreter, storage deepcopied between them -- the
// shape internal/factorio's retain_test.go uses, and for its reason. deepcopy
// models Factorio serializing the reference rather than letting a live Lua table
// cross a boundary the real game rebuilds, and a FRESH stand-in game per session
// means the surface the first session retained is reachable only through
// storage. --persist=table, because under `none` the heap is rebuilt from the
// data segments and the guard's own u32 would not survive either.
//
// The guest re-retains nothing in the second session: fk_on_tick reads the
// static and CALLS something on the handle, because a number that resolves to
// nothing would still be a number.
func checkGuardSurvivesASave(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageHandlesGuest(t, wasmPath, luagen.Options{
		Persist: luagen.PersistTable,
		BuildID: "handles-one",
	})
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_tick = 1 } }
helpers = {}

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local out = {}
  for k, x in pairs(v) do out[deepcopy(k)] = deepcopy(x) end
  return out
end

local function session(saved, tick)
  local handlers = {}
  script = {
    mod_name = "fk-handles",
    on_init = function(f) handlers.on_init = f end,
    on_load = function(f) handlers.on_load = f end,
    on_configuration_changed = function(f) handlers.on_config = f end,
    on_event = function(ev, f) handlers[ev] = f end,
  }
  storage = saved and deepcopy(saved) or {}
  game = { valid = true,
           get_surface = function(_) return { valid = true, name = "nauvis" } end }
  -- The reset a load performs. internal/factorio expands this from
  -- packagedLuaModules; this package writes it out, as ipcjoin_test.go does.
  package.loaded["control"] = nil
  package.loaded["fk_module"] = nil
  package.loaded["fk_abi"] = nil
  package.loaded["fk_api_gen"] = nil
  require("control")
  if saved then
    if handlers.on_load then handlers.on_load() end
  else
    handlers.on_init()
  end
  if tick then handlers[defines.events.on_tick]({ tick = tick }) end
  return storage
end

local first = session(nil, nil)
session(first, 7)
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("running the two sessions: %v\n%s", err, out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	// The first session has to have parked one, or the second session's line
	// would be about nothing. Slot 14 is the shared transcript's arithmetic
	// carried on, and it is asserted there too.
	const parked = "LOG save: parked slot=14"
	if len(lines) == 0 || lines[len(lines)-2] != parked {
		t.Fatalf("the first session did not park a guard (%q expected as the "+
			"second-to-last line):\n%s", parked, out)
	}
	// ...and the second session resolves that same slot through a handle table
	// M.adopt rebuilt from the save. slot=0 would be a guard that did not come
	// back; nauvis=error would be a number that came back naming nothing.
	const want = "LOG save: guard slot=14 nauvis=true"
	if got := lines[len(lines)-1]; got != want {
		t.Errorf("after the load:\n  got  %s\n  want %s\n(a guard parked in a "+
			"static is a u32 in the guest heap, and it means nothing unless the "+
			"host's persistent table came back with it)", got, want)
	}
}

func packageHandlesGuest(t *testing.T, wasmPath string, opts luagen.Options) string {
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
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("function %q did not compile: %v", f.Name, f.Unsupported)
		}
	}
	src, err := luagen.EmitModuleWith(im, opts)
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
			Name: "fk-handles", Version: "0.1.0", Title: "FkLua handle ownership",
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
