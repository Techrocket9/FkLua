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

// fkipc's own subscribe call site must keep the event id an i32.const.
//
// This is TestTheEventIdSurvivesTheGeneratedSubscribeWrapper's obligation
// applied to a hand-written package, and fkipc owes it for a reason the
// generated bindings do not: the id is not at the guest author's call site at
// all. A guest writes fkipc.Open(...) and never names an event, so the constant
// has to travel from inside guest/go/fkipc's transport, through
// fkapi.Subscribe, to fk.subscribe -- two inlinings rather than one, in a
// package TinyGo has no reason to treat gently.
//
// If it stopped, every mod using fkipc would silently start shipping every
// event descriptor there is (`events` in census.json): about 55 KB of Lua
// parsed on every load at the 2.1.14 pin, in every save's neighbourhood, and
// nothing would fail. That is the same failure the Rust subscribe_filtered
// wrapper actually had (R6, measured downstream at 85 KB a mod -- larger than
// the descriptor table because it is a whole-mod delta between two builds of
// one guest), which is why this is a test and not a comment.
//
// THE COST IS THE EVENT TABLE'S, NOT THE MEMBER TABLE'S. This comment used to
// say ~600 KB, which is the MEMBER table's magnitude (about a megabyte at the
// 2.1.14 pin); that table is pruned by its own scan over fk.call and an event
// id that stops inlining does not move it by a byte.
//
// ONE, exactly. The example subscribes to nothing of its own, so the count is
// the library's whole event footprint and a second one appearing is a
// deliberate act rather than a drift.
//
// BOTH BACKENDS, and the Rust arm is not decoration: R6 was exactly this defect
// on that side -- `subscribe_filtered` lacked `#[inline]`, whether the id
// reached the import as a constant became rustc's cost heuristic's decision
// taken per call site, and a downstream mod measured 85 KB of extra Lua per
// load. fkipc's own call site is two inlinings deep in BOTH languages, because
// the guest author never names an event: the constant has to travel from inside
// the transport, through the generated `subscribe`, to `fk.subscribe`.
//
// # ...AND BOTH -gc ARMS ON THE GO SIDE
//
// The Go arm built LEAKING alone until this one was split, and leaking is not
// what FkLua tells a mod to ship: agents/gc.md's whole argument is for
// --gc=collected, and -gc=custom is the flag that carries it. The two arms are
// the same four flags with one word changed and they do NOT agree about
// inlining -- which stopped being a theoretical objection when the generated
// SubscribeFiltered wrapper was found shipping every descriptor there is under
// -gc=custom while the leaking arm of the same source proved all seven. Filed
// by BetterBeltBalancer (item 30); the write-up is on
// internal/factorio/gogen.go's SubscribeFiltered and in
// TestTheEventIdSurvivesTheGeneratedSubscribeWrapper.
//
// THIS GATE'S COLLECTED ARM WAS ALREADY GREEN WHEN IT WAS ADDED, measured
// against the pre-fix bindings: complete, one id, 207. That is the honest
// finding and it is worth writing down rather than implying a save -- fkipc
// reaches fkapi.Subscribe, the smallest wrapper in the family and the one every
// toolchain has always inlined, while item 30's defect was in the filtered one.
// What was wrong here was not the answer but that nobody was asking: the arm
// every real mod ships was ungated, on the one call site in this repo where the
// constant has two inlinings to survive rather than one.
//
// Building it at all needed guest/go/examples/ipc/gc.go, which did not exist --
// without the fkgc import -gc=custom does not link (missing core function
// "runtime.free"). That import is an EMPTY PACKAGE under -gc=leaking and the
// leaking wasm is byte-identical either side of the file, measured, so this arm
// cost the existing one nothing.
func TestTheEventIdSurvivesTheFkipcSubscribeCallSite(t *testing.T) {
	for _, arm := range []struct {
		name  string
		flags []string
		build func(dir, pkg, out string) error
	}{
		{"go-leaking", guest.BuildFlags, guest.Build},
		{"go-collected", guest.CollectedBuildFlags, guest.BuildCollected},
	} {
		t.Run(arm.name, func(t *testing.T) {
			ok, why := guest.Available()
			if !ok {
				t.Skipf("skipping: %s", why)
			}
			root := repoRoot(t)
			out := filepath.Join(t.TempDir(), "ipc.wasm")
			if err := arm.build(filepath.Join(root, "guest", "go"), "./examples/ipc", out); err != nil {
				t.Fatalf("the fkipc example does not build in the %s arm (%s): %v",
					arm.name, strings.Join(arm.flags, " "), err)
			}
			checkFkipcPruning(t, arm.name, out)
		})
	}
	t.Run("rust", func(t *testing.T) {
		ok, why := guest.RustAvailable()
		if !ok {
			t.Skipf("skipping: %s", why)
		}
		root := repoRoot(t)
		out, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "ipc",
			filepath.Join(t.TempDir(), "cargo"))
		if err != nil {
			t.Fatalf("the Rust fkipc example does not build: %v", err)
		}
		checkFkipcPruning(t, "rust", out)
	})
}

func checkFkipcPruning(t *testing.T, arm, wasmPath string) {
	t.Helper()
	root := repoRoot(t)
	raw, err := os.ReadFile(wasmPath)
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
		t.Fatalf("the event id scan gave up on the %s arm, so an fkipc mod would "+
			"ship every event descriptor there is: fkipc's fkapi.Subscribe call "+
			"site is no longer reached by a constant", arm)
	}
	if len(ids) != 1 {
		t.Fatalf("examples/ipc subscribes to exactly one event -- fkipc's own; "+
			"the %s arm's scan proved %d", arm, len(ids))
	}

	// ...and it must be the RIGHT one, read from the bindings rather than
	// written here. The runtime id of on_udp_packet_received is a different
	// number again (208 on 2.0.77, 212 on 2.1.14) and both namespaces are
	// correct, so a literal in this file would be wrong for one of them and
	// would go on being wrong through a version bump.
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := -1
	for _, e := range factorio.GenerateEvents(a).Events {
		if e.Name == "on_udp_packet_received" {
			want = e.ID
		}
	}
	if want < 0 {
		t.Fatal("this API pin has no on_udp_packet_received")
	}
	if !ids[want] {
		t.Errorf("the proven event id is not on_udp_packet_received (%d): %v", want, ids)
	}
}
