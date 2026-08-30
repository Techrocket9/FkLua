package guest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The generated bindings have to COMPILE, not merely parse.
//
// internal/factorio parses them, which catches a malformed name or literal but
// says nothing about types -- and it happily accepted `*p.h`, which parses as
// `*(p.h)` and dereferences a uint32 field instead of the pointer. Seventy
// members were broken and the parse test was green. Only a real build finds
// that, so this is the gate that matters for generated Go.
func TestGeneratedBindingsCompile(t *testing.T) {
	ok, why := guest.Available()
	if !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	dir := filepath.Join(root, "guest", "go")
	if _, err := os.Stat(filepath.Join(dir, "fkapi", "fkapi.go")); err != nil {
		t.Fatalf("generated bindings are missing: %v (run `fklua gen-bindings`)", err)
	}
	out := filepath.Join(t.TempDir(), "api.wasm")
	if err := guest.Build(dir, "./examples/api", out); err != nil {
		t.Fatalf("the example does not build against the generated bindings: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("examples/api built against the bindings: %d bytes of wasm", st.Size())
}

// A generated wrapper must not hide the event id from the constant scan.
//
// `fklua mod` ships only the event descriptors a guest subscribes to -- 6 of
// however many the pinned description declares (`events` in census.json), and
// the assertion below is where the 6 lives -- and it finds them by looking for
// an i32.const feeding fk.subscribe's first operand. Guests used to
// hand-declare that import and call it directly, so the constant was obviously
// there. They now go through fkapi.Subscribe, and if TinyGo ever stopped
// inlining it the id would arrive as a parameter, the scan would give up, and
// every mod would silently start carrying all of them. Bigger, never broken --
// but bigger by about 55 KB at the 2.1.14 pin, and nothing else would say so.
//
// THAT IS THE EVENT TABLE'S SIZE AND NOT THE MEMBER TABLE'S. This comment used
// to charge it 600 KB, which is the member table's magnitude (about a megabyte
// at the same pin); the members are pruned by a separate scan over fk.call and
// an event id that stops inlining does not move them at all.
//
// THE EXAMPLE USES BOTH WRAPPERS, and that is deliberate rather than tidy.
// SubscribeFiltered is several times the size of Subscribe -- an early return,
// an allocation mark, a galloc and a write_dyn -- so "does it inline" is a real
// question about it and an obvious yes about its sibling. It was never asked
// until the Rust arm was measured downstream and the answer there was NO: 991,040
// bytes of Lua against 906,393 for the same mod with the filters taken out,
// 85 KB parsed by the game on every load (reported by a downstream Rust port). A
// guest cannot have both Factorio's own C++-side filters and a pruned event
// table unless this holds.
//
// BOTH FILTER HELPERS ARE IN THE EXAMPLE, three subscriptions by NameFilter and
// one by TypeFilter, and that is a calibration statement rather than a stylistic
// one: they build the same wire shape (one map term, two keys), so the counts
// here are unmoved by which one a call site uses, and the corpus would otherwise
// have had no caller of TypeFilter at all. A helper nothing calls is a helper
// nothing compiles.
//
// ...AND SO IS THE NAMED FORM, for the same reason and against the same risk.
// SubscribeNamed carries a string, so it is bigger than Subscribe, and a wrapper
// that grows until the toolchain stops inlining it is R6 exactly. The seventh
// id is the custom-input subscription, and it is the one whose absence from this
// count would mean every mod using a keybind ships all 219 descriptors.
func TestTheEventIdSurvivesTheGeneratedSubscribeWrapper(t *testing.T) {
	ok, why := guest.Available()
	if !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "api.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/api", out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
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
	ids, complete := factorio.UsedEvents(im)
	if !complete {
		t.Fatal("the event id scan gave up, so this mod would ship every " +
			"event descriptor there is: fkapi.Subscribe or fkapi.SubscribeFiltered " +
			"is no longer being inlined")
	}
	if len(ids) != 7 {
		t.Errorf("examples/api subscribes to exactly seven events -- two plain, "+
			"four filtered (three by NameFilter and one by TypeFilter) and one by "+
			"NAME, which is the only way a custom input can be reached; the scan "+
			"proved %d", len(ids))
	}
}

// The same assertion for the Rust guest, which is where it was actually needed.
//
// The Rust corpus had no pruning gate at all, so nothing said that
// `subscribe_filtered` was defeating the scan -- and it was, for every guest
// that used it, silently, at 85 KB of generated Lua a mod. The two examples are
// mirrors and subscribe to the same six events, by the same helpers in the same
// order, so the numbers are comparable on purpose.
func TestTheEventIdSurvivesTheGeneratedRustSubscribeWrapper(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	p, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "api",
		filepath.Join(t.TempDir(), "cargo"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
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
	ids, complete := factorio.UsedEvents(im)
	if !complete {
		t.Fatal("the event id scan gave up, so this mod would ship all 224 " +
			"event descriptors: fkapi::subscribe, fkapi::subscribe_filtered or " +
			"fkapi::subscribe_filtered_masked is no longer being inlined")
	}
	if len(ids) != 7 {
		t.Errorf("the Rust examples/api subscribes to exactly seven events -- two "+
			"plain, four filtered (three by name_filter and one by type_filter, one "+
			"of those also masked) and one by NAME; the scan proved %d", len(ids))
	}

	// ...AND THE SAME SCAN OVER fk.define, which is the other all-or-nothing
	// table and the one with the worse failure: the full set is 1185 dotted
	// paths, ~45 KB of Lua parsed on every load and carried in every save.
	//
	// The Rust corpus had no defines gate because it had no defines ACCESSOR --
	// three ports declared the import by hand and re-derived the ids from the Go
	// generator's source, and one of them recorded "the pruning works from Rust
	// unchanged" as a finding worth writing down, because it was the risk. The
	// generated accessor is what has to keep that true: the id is a literal in
	// its body, and a body rustc decided to build from a table instead would
	// compile, would work, and would ship the other 1184.
	defs, defComplete := factorio.UsedDefines(im)
	if !defComplete {
		t.Fatal("the define id scan gave up, so this mod would ship all 1185 " +
			"define paths: the generated accessor no longer reaches fk.define " +
			"with a constant")
	}
	// TWO since the class-operator round: examples/api reads
	// defines.direction.east at init and defines.inventory.chest to reach the
	// chest inventory its `#inv` / `inv[1]` probe needs. The number is what
	// matters -- 2 of 1185 is the pruning working, and 1185 is it having given
	// up -- but it is written out so that a THIRD accessor appearing is a
	// deliberate act rather than a drift.
	if len(defs) != 2 {
		t.Errorf("the Rust examples/api reads exactly two defines "+
			"(direction.east, inventory.chest); the scan proved %d", len(defs))
	}
}
