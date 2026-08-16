package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// WHAT A HOST CALL KEEPS FOREVER, measured rather than argued.
//
// -gc=leaking is mandatory (a collector's pause lands in a lockstep game loop),
// so every byte the ABI allocates per call is a byte in every save and every
// multiplayer join. The first downstream mod measured ~180 B per host call and
// a ~350-call network compile, reaching ~2.4 MB of heap on a test map -- and
// that heap is also what makes --persist=packed repack the world, because a
// bump allocator walking upward dirties a new page every call.
//
// The number here is the ALLOCATOR's own answer -- its bump pointer, read by
// asking for one byte -- not an accounting of what the code looks like it
// should do. That distinction is the point: the marshalling path allocates in
// three places and only one of them is obvious from reading it.
//
// The gate is on the tier-2 argument probe, which allocates only inside the
// ABI. The string return is reported and deliberately NOT gated: the Go string
// it hands back belongs to the caller and outlives the call by design, so no
// arena on this side may touch it -- see the note in agents/abi.md.
func TestAHostCallKeepsNoHeap(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	root, tmp := repoRoot(t), t.TempDir()
	p := filepath.Join(tmp, "heap.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/heap", p); err != nil {
		t.Fatalf("building the Go guest: %v", err)
	}

	raw, err := os.ReadFile(p)
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
		t.Fatal("a member id was not a compile-time constant, so the id scan broke")
	}
	table, err := report.Only(used).LuaSourceWith(a, events)
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-heap", Version: "0.1.0", Title: "FkLua heap probe",
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

	out, err := h.RunString(heapStub(dir))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}
	got := map[string]int{}
	re := regexp.MustCompile(`^LOG (.+): (-?\d+) B/call$`)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			t.Fatalf("unexpected output line %q; whole run:\n%s", line, out)
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		got[m[1]] = n
	}
	for _, k := range []string{"tier2 arg", "string ret", "name cmp", "name is",
		"array ret", "array into", "scalar ret", "scalar arg", "no blocks"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("no measurement for %q; whole run:\n%s", k, out)
		}
		t.Logf("%-11s %4d B/call", k, got[k])
	}

	// The control.
	if got["no blocks"] != 0 {
		t.Errorf("a call with no argument and no return block kept %d B/call; the "+
			"control should be free, so the probe itself is allocating and no "+
			"other number here can be read", got["no blocks"])
	}
	// The gate. The argument is hoisted out of the loop, so every byte here was
	// allocated by the ABI for the duration of one call and released at the
	// bracket the binding already opens.
	if got["tier2 arg"] != 0 {
		t.Errorf("a host call with a hoisted tier-2 argument kept %d B/call of "+
			"guest heap; the marshalling arena should make it 0", got["tier2 arg"])
	}

	// THE TWO PREDICATES THE CLOSEOUT ROUND ADDED, and each is gated at ZERO
	// rather than at "less than before".
	//
	// A ratio would pass a variant that allocated a little, and "a little,
	// forever" is the whole complaint: the downstream cost is 32 B per build
	// event on a map where anyone can lay a belt. The point of asking the host
	// is that the guest never receives a value, and the point of a destination
	// slice is that the call reuses one. Neither has a legitimate byte.
	if got["name is"] != 0 {
		t.Errorf("NameIs kept %d B/call of guest heap; the string is compared "+
			"HOST-side and never crosses, so there is nothing for the guest to "+
			"keep. Something is copying it anyway", got["name is"])
	}
	if got["array into"] != 0 {
		t.Errorf("FindEntitiesFilteredInto kept %d B/call of guest heap after a "+
			"warm-up call; the destination's capacity is reused, so a non-zero "+
			"here means it is reallocating every call", got["array into"])
	}

	// AND THE BEFORE-COLUMN HAS TO BE NON-ZERO, or the after-column proves
	// nothing. A probe whose baseline is already free would report a saving
	// that was never there -- which is exactly how the string-return line in
	// this file first measured wrong, by discarding the result and letting the
	// optimizer delete the copy.
	if got["name cmp"] == 0 {
		t.Errorf("reading a name and comparing it kept 0 B/call, so the " +
			"NameIs comparison is against nothing. The copy in getStr should " +
			"be visible here; if TinyGo started eliding it, this probe needs a " +
			"shape that keeps it")
	}
	if got["array ret"] == 0 {
		t.Errorf("a container return kept 0 B/call, so the Into variant is " +
			"measured against nothing. make([]Object, n) should be visible here")
	}
}

// heapStub is the smallest game the probe needs: one object, a create_entity
// that returns it, a name and a health.
func heapStub(modDir string) string {
	return fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = {} }
local handlers = {}
script = {
  mod_name = "fk-heap",
  on_init = function(f) handlers.on_init = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local thing
thing = {
  valid = true,
  -- A name long enough that a per-call Go string is visible if one is made,
  -- and short enough to sit in the 4 KiB scratch region.
  name = "transport-belt-of-a-perfectly-ordinary-length",
  health = 100.0,
}
-- Methods are CLOSURES OVER THE OBJECT, the way Factorio's __index hands them
-- back. See TestAMethodIsCalledTheWayFactorioBindsIt.
thing.create_entity = function(_) return thing end
-- The control's member: it must EXIST, or the probe would be timing an
-- ERR_NO_MEMBER early return rather than a host call.
thing.create_global_electric_network = function() end
-- FOUR entities, so the container probe measures a slice with something in it.
-- The destination the guest supplies starts at capacity 8, which is what makes
-- "reused the caller's buffer" and "grew a new one" tell apart.
thing.find_entities_filtered = function(_) return { thing, thing, thing, thing } end
game = { valid = true, connected_players = { thing } }
require("control")
handlers.on_init()
`, filepath.Join(modDir, "?.lua"))
}
