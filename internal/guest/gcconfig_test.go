package guest_test

import (
	"strings"
	"testing"
)

// CONFIGURATION INSTALLED BEFORE THE FIRST ALLOCATION, IN BOTH LANGUAGES.
//
// `fkgc` brings the heap up in `initialize()`, which assigns `threshold` and
// `budget` their defaults unconditionally. Anything that runs before it is
// silently overwritten -- and the two arms reach `initialize()` at completely
// different moments, which is why one of them was broken and the other was not:
//
//	Go     TinyGo calls initHeap from _initialize, BEFORE any package
//	       initialiser, so a guest's init() lands after it and stands.
//	Rust   there is no such call. initialize() is LAZY, funnelled through
//	       alloc_spans, so it runs on the guest's FIRST ALLOCATION -- after any
//	       set_threshold the guest made, which it then overwrote.
//
// Found downstream (fklua-ports' AutoDeconstruct, finding 3) as a collector that
// appeared wired and never ran: `since_gc=135168` against a requested 131,072
// with `cycles=0` for a whole verification run, because the collector's own copy
// of the threshold was still the 256 KiB default.
//
// It fails in the direction that hides itself, and the downstream shape is worse
// than a wrong number. agents/gc.md prescribes that a collected guest keep its
// own pacer fed by comparing `stats().since_gc` against ITS OWN threshold and
// asking for a deferred flush when it is reached. If the collector's copy is not
// the one the guest installed, those two decisions disagree by construction: the
// guest asks on every event and the collector declines every time. A player sees
// a mod slightly busier than it should be.
//
// The guests are guest/{go,rust}/examples/gcconfig, mirrors, with the one place
// they cannot mirror written down in the Rust one's header.

// The numbers the two guests install, far from the defaults on purpose: 777
// against a default budget of 1024 and 4 KiB against a default threshold of
// 256 KiB, so a clobber cannot read as rounding.
const (
	gcConfigEarlyBudget = "777"
	// ~16 KiB is four times the early threshold and one sixteenth of the
	// default, so CollectIfNeeded answers 1 or 0 rather than differing in
	// timing.
	gcConfigAllocated = 16384
)

// The Go arm, which was always correct -- and this is what says so rather than
// an argument about somebody else's runtime ordering.
//
// It is also the arm a downstream mod depends on: BetterBeltBalancer installs
// its threshold from init() and then arms its own flush against the same number,
// on the reasoning that "the two cannot drift". That reasoning is only sound if
// this passes.
func TestGoConfigurationInstalledBeforeTheFirstAllocationSurvives(t *testing.T) {
	h := needGuest(t)
	got := gcRun(t, h, "./examples/gcconfig", true, `
print("budget " .. K.config_budget())
print("collects " .. K.config_collects())
print("enough " .. tostring(K.config_since() >= `+itoa(gcConfigAllocated)+`))
`)
	want := strings.Join([]string{
		"budget " + gcConfigEarlyBudget, // not the 1024 default
		"collects 1",                    // the early 4 KiB threshold, not 256 KiB
		"enough true",                   // and it really allocated past it
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a value installed from init() must survive "+
			"the collector coming up; TinyGo calls initHeap from _initialize before "+
			"any package initialiser, so this is the arm that was already right)",
			got, want)
	}
}

// The Rust arm, which was NOT correct, and each export gets its own instance.
//
// There is only one first allocation per module, so whichever export runs first
// is the only one whose setter is exposed to it: calling both in one Lua state
// would find the second passing for the wrong reason, because the first one's
// allocation would already have run initialize(). Two runs, two fresh states.
func TestRustConfigurationInstalledBeforeTheFirstAllocationSurvives(t *testing.T) {
	h := needRustGuest(t)

	if got, want := rustGCRun(t, h, "gcconfig", true, `
print("budget " .. K.config_budget())
`), "budget "+gcConfigEarlyBudget; got != want {
		t.Errorf("got %q, want %q -- set_budget before the first allocation was "+
			"discarded by heap::initialize()", got, want)
	}

	if got, want := rustGCRun(t, h, "gcconfig", true, `
print("collects " .. K.config_collects())
print("enough " .. tostring(K.config_since() >= `+itoa(gcConfigAllocated)+`))
`), "collects 1\nenough true"; got != want {
		t.Errorf("got %q, want %q -- set_threshold before the first allocation was "+
			"discarded by heap::initialize(), so the collector kept the 256 KiB "+
			"default and declined a guest that had asked for 4 KiB", got, want)
	}
}

// And the two agree, which is the corpus-mirror bar: the guests are transcribed
// from each other, so a language whose collector disagreed about its own
// configuration would show up here as a diff rather than as two separately
// recorded numbers.
func TestTheTwoCollectorsAgreeAboutEarlyConfiguration(t *testing.T) {
	h := needGuest(t)
	hr := needRustGuest(t)
	body := "print(K.config_budget())\n"
	if a, b := gcRun(t, h, "./examples/gcconfig", true, body), rustGCRun(t, hr, "gcconfig", true, body); a != b {
		t.Errorf("go said %q and rust said %q for the same early set_budget", a, b)
	}
}
