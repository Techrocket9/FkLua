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
// BOTH FILTER HELPERS ARE IN THE EXAMPLE, in one shared four-term list built as
// append(NameFilter(...), TypeFilter(...)...), and that is a calibration
// statement rather than a stylistic one: they build the same wire shape (one map
// term, two keys), so the counts here are unmoved by which one a term came from,
// and the corpus would otherwise have had no caller of TypeFilter at all. A
// helper nothing calls is a helper nothing compiles.
//
// ...AND SO IS THE NAMED FORM, for the same reason and against the same risk.
// SubscribeNamed carries a string, so it is bigger than Subscribe, and a wrapper
// that grows until the toolchain stops inlining it is R6 exactly. The seventh
// id is the custom-input subscription, and it is the one whose absence from this
// count would mean every mod using a keybind ships all 219 descriptors.
//
// # BOTH -gc ARMS, and the second one is where R6 came back
//
// This gate built the LEAKING arm alone for as long as it existed, and leaking
// is not what FkLua tells a mod to ship: agents/gc.md's whole argument is for
// --gc=collected, and -gc=custom is the flag that carries it. The two arms are
// the same four flags with one word changed and they do NOT agree about
// inlining, so an arm that is not built is an arm nobody is gating.
//
// Measured on examples/api under -gc=custom -opt=2 (TinyGo 0.41.1, LLVM
// 20.1.1): with the four filtered subscriptions sharing one list built as
// append(NameFilter(...), TypeFilter(...)...), the toolchain stops inlining
// fkapi.SubscribeFiltered, which then survives as a standalone wasm function
// whose interior fk.subscribe calls pass the event id as local.get $0 --
// usedIDs is intraprocedural, sees a non-constant operand at the import, and
// gives up on the whole table. The leaking arm of the same source inlines it
// and proves all seven. That is R6's exact shape one toolchain over: a wrapper
// that grew until the compiler's cost heuristic changed its mind, and the
// heuristic here is the collector's own weight in the module plus how much of
// init was already inlined before the wrappers were weighed.
//
// It was reported from the field before it was reproduced here: a downstream
// Go guest with eleven filtered call sites through one shared five-term filter
// slice packaged `all 225 events -- an event id was not a compile-time
// constant`, with every id at every call site a literal. Packaging the two arms
// of THIS example says the same thing and prices it: `7 events subscribed, of
// 219` against `all 219 events -- an event id was not a compile-time constant`,
// and fk_api_gen.lua 8,425 bytes against 60,118 -- 50 KB of extra Lua parsed by
// the game on every load, at the 2.0.77 pin.
//
// WHAT DOES AND DOES NOT MOVE IT, because the obvious repair does not and the
// obvious place for the working one is the wrong wrapper.
//
// SubscribeFilteredMasked carried a `defer allocRelease(mark)` and a defer
// lowers to real machinery, so it was the natural suspect -- and replacing it
// with a plain call after the host call changed nothing here: the mixed form
// stayed out of line at one, two and three name terms alike. Nor is a smaller
// callee the lever. (The defer is a plain call in the shipped prelude anyway,
// on its own merits: there is no early exit between the mark and the host call
// and a panic on this target traps rather than unwinding, so it was cost with
// no path to pay for it. Headroom, not the fix.)
//
// What restores it is an INLINING HINT, and which function carries it decides
// whether it works at all, measured one at a time:
//
//	//go:inline on SubscribeFilteredMasked alone   -- still RED
//	//go:inline on SubscribeFiltered alone         -- GREEN, all seven i32.const
//
// The reason is the whole shape of this defect: the constant lives at the
// GUEST'S call site, so the function that has to disappear is the one the guest
// called. Marking the callee only moves its body up into a caller that is still
// a real function taking the id in a parameter. Which means the hint belongs on
// every public subscribe entry point a guest can name -- Subscribe,
// SubscribeNamed, SubscribeMasked, SubscribeNamedMasked, SubscribeFiltered and
// SubscribeFilteredMasked, that last one because it is exported and a guest may
// call it directly -- rather than on whichever one happens to be biggest today.
// All six carry it. A structural split that keeps the id at a call site the
// scan can see would do as well; a diet would not.
//
// A HINT AND NOT A DIRECTIVE, which is why this gate is the evidence rather
// than a formality: TinyGo lowers //go:inline to LLVM's inlinehint, which the
// inliner weighs and may decline, where the Rust arm's #[inline(always)] on the
// same six wrappers lowers to alwaysinline and is obeyed. So the Rust side is
// safe by construction and this side is safe by measurement, and the
// measurement is here.
//
// The arms are a table rather than two tests so that the assertion is written
// once and cannot drift; the arm's name is in every failure message, because
// which one broke is the whole diagnosis.
func TestTheEventIdSurvivesTheGeneratedSubscribeWrapper(t *testing.T) {
	ok, why := guest.Available()
	if !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	for _, arm := range []struct {
		name  string
		flags []string
		build func(dir, pkg, out string) error
	}{
		{"leaking", guest.BuildFlags, guest.Build},
		{"collected", guest.CollectedBuildFlags, guest.BuildCollected},
	} {
		t.Run(arm.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "api.wasm")
			if err := arm.build(filepath.Join(root, "guest", "go"), "./examples/api", out); err != nil {
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
				t.Fatalf("the event id scan gave up on the %s arm (%s), so this "+
					"mod would ship every event descriptor there is: "+
					"fkapi.Subscribe, fkapi.SubscribeFiltered or "+
					"fkapi.SubscribeNamed is no longer being inlined, and the id "+
					"reaches fk.subscribe as a parameter rather than as a "+
					"constant", arm.name, strings.Join(arm.flags, " "))
			}
			if len(ids) != 7 {
				t.Errorf("examples/api subscribes to exactly seven events -- two "+
					"plain, four filtered through one shared four-term list and one "+
					"by NAME, which is the only way a custom input can be reached; "+
					"the %s arm's scan proved %d", arm.name, len(ids))
			}
		})
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
