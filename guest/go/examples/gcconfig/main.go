// Command gcconfig asks one question of the collector: does the configuration a
// guest installs BEFORE THE COLLECTOR COMES UP survive?
//
// It exists because the answer was no -- in BOTH languages, for two different
// reasons, and in both of them the failure hides itself. `fkgc` brings the heap
// up in `initialize()`, which assigns `threshold` and `budget` their defaults
// unconditionally, so anything installed before it is silently overwritten and
// the guest is left asking for one pacing and getting another with nothing
// logged and no error anywhere.
//
// The shape a guest author writes is exactly the shape that broke:
//
//	func init() { fkgc.SetThreshold(128 << 10) }   // ...discarded
//
// AND THE ORDERING IS NOT WHAT THE TINYGO SOURCE READS LIKE, which is why this
// is a guest and not a comment. `runtime_wasmentry.go`'s `wasmEntryReactor`
// calls `initHeap()` and then `initAll()`, so a package initialiser looks like
// it lands after the heap is up. Measured through this guest at TinyGo 0.41.1,
// `-target=wasm-unknown -scheduler=none -gc=custom -opt=2`, it does not: a
// counter incremented inside `initialize()` read ZERO from `init()` and ONE
// from the first export, so `initialize()` really does run after the package
// initialisers on this target. The Rust arm gets there by a different route --
// `initialize()` is explicitly lazy, reached by the first allocation -- and
// arrives at the same defect.
//
// The downstream symptom is worse than a wrong number. The prescribed way to
// keep a collected guest's pacer fed is for the guest to compare
// `Stats().SinceGC` against its OWN threshold and ask for a deferred flush when
// it is reached (the starved-pacer fix in agents/gc.md, which the first
// downstream mod ships). If the collector's copy of the threshold is not the one
// the guest installed, those two decisions disagree by construction: the guest
// asks on every event and the collector declines every time. What a player sees
// is a mod slightly busier than it should be with a collector that appears wired
// and never runs. Found in the field by fklua-ports' AutoDeconstruct (Rust) and
// confirmed here for Go.
//
// The Rust mirror is guest/rust/examples/gcconfig and returns the same numbers.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
//	    -o gcconfig.wasm ./examples/gcconfig
package main

import "github.com/Techrocket9/fklua/guest/go/fkgc"

// Deliberately not the defaults, and deliberately far from them: 777 against a
// default budget of 1024, and 4 KiB against a default threshold of 256 KiB. A
// value that merely LOOKED plausible next to the default would let a clobber
// pass as rounding.
const (
	earlyBudget    = 777
	earlyThreshold = 4 << 10
)

// The allocation this guest makes is kept alive on purpose. A collection that
// reclaimed it would move `SinceGC` under our feet, and the question here is
// about configuration rather than about reclamation.
var kept [][]uint32

// The whole point of the guest, and it has to be an `init` rather than the body
// of an export: what is under test is a value installed before the collector
// comes up, and an export is far too late.
func init() {
	fkgc.SetBudget(earlyBudget)
	fkgc.SetThreshold(earlyThreshold)
}

// The budget the collector is actually pacing with.
//
//go:wasmexport config_budget
func configBudget() uint32 { return fkgc.Budget() }

// Whether the collector agrees that enough has been handed out.
//
// It returns 1 only if the collector's threshold is the one `init` installed:
// this allocates ~16 KiB, which is four times the early threshold and one
// sixteenth of the default, so the two answers are 1 and 0 rather than a
// difference in timing.
//
//go:wasmexport config_collects
func configCollects() uint32 {
	for i := 0; i < 64; i++ {
		kept = append(kept, make([]uint32, 64))
	}
	if fkgc.CollectIfNeeded() {
		return 1
	}
	return 0
}

// How many bytes the collector thinks have been handed out. Reported so that a
// failure of config_collects can be read: a zero here is a guest that allocated
// nothing, which is a different bug from a threshold that was clobbered.
//
//go:wasmexport config_since
func configSince() uint32 { return fkgc.Stats().SinceGC }

func main() {}
