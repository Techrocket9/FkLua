// Command migrate is the round-trip's REBUILD guest: the only one here that
// exports fk_migrate, and the reason it exists is that nothing ever had.
//
// fk_migrate is the hook a guest gets when its build stamp no longer matches the
// one in the save -- "your state was written by a build that is not you". It has
// been documented, host-side tested and reasoned about since M6, and until now
// it had never run inside a real Factorio. That mattered more after 2026-08-07:
// the declined-adoption fix moved its trigger off script.on_configuration_changed
// (which fires for a mod-VERSION change, and a rebuild keeps the version) and
// onto the FIRST OUTERMOST DISPATCH after the load. That path is reachable only
// from a running game, so the host-side suite can model it and cannot exercise
// it.
//
// The three things this guest is built to make visible in a log, each of which
// fails differently:
//
// told= is the old state version fk_migrate was handed. 7 is what
// fk_state_version reports, so the save carried it and the runtime read it back;
// 0 would mean the hook fired with nothing behind it.
//
// migrated= is how many times the hook has EVER fired into this heap. It is
// deliberately not a per-session count, because there is nowhere to reset one: a
// load runs no guest code of its own. So what it reports on a later load is
// whether the migrated heap was carried, and the question "did the hook fire
// again" is answered by the LOG LINE in onMigrate being absent rather than by
// this counter. Measured the other way round first: after a clean second load
// this reads 1, carried, while no second notification had happened.
//
// sentinel= is whether the heap is FRESH. fk_migrate is a notification on a
// fresh heap and fk_migrate_adopt is the one that hands the old bytes over; the
// difference is invisible to any assertion about the counter, because both
// restart it. This is set only in fk_on_init, which does not run on a load -- so
// it comes back as sentinelValue when the save's heap was adopted and as 0 when
// it was discarded, which is what this guest must see.
//
// `seen=` is the same counter every other leg reports, so the script's existing
// report-line machinery reads this guest unchanged.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o migrate.wasm ./examples/migrate
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// sentinelValue is written ONLY from fk_on_init, which Factorio runs for a new
// map and never for a load. So its presence after a load says the save's heap
// was adopted and its absence says the heap this guest is running on was rebuilt
// from the data segments -- which is the whole difference between fk_migrate and
// fk_migrate_adopt, and is not observable from the tick counter.
const sentinelValue = 0xFEED

// stateVersion is what fk_state_version reports, and it is deliberately not 0 or
// 1: it is the number that has to come back through the save and out of
// fk_migrate's argument, so a value that could be produced by a zeroed field or
// an off-by-one would prove less.
const stateVersion = 7

var (
	seen     uint32
	migrated uint32
	told     uint32
	sentinel uint32
)

//go:wasmexport fk_on_init
func onInit() {
	sentinel = sentinelValue
	fk.Log("migrate guest: new map, state version " + strconv.Itoa(stateVersion))
}

// fk_state_version is what the runtime writes into the save beside the build
// stamp, and what a later build's fk_migrate is handed.
//
//go:wasmexport fk_state_version
func stateVersionOf() uint32 { return stateVersion }

// fk_migrate is the NOTIFICATION half: this guest is running on a fresh heap and
// is being told which build's state it is replacing. A real guest rebuilds from
// the world here. This one records that it was told, and what it was told.
//
// THE LOG LINE IS THE EVIDENCE THAT THE HOOK RAN, and it is a separate line
// rather than a field on the tick report for a reason: every field on that
// report lives in the guest heap and is therefore CARRIED by an ordinary load,
// so none of them can distinguish "fired again just now" from "fired once, a
// save ago". A line in the log is emitted by this call and by nothing else.
//
// Exporting this and not fk_migrate_adopt is also what suppresses the runtime's
// "this mod was rebuilt ... Guest state has been reset" warning, which the
// script asserts is absent -- the warning is the arm for a guest that exports
// neither, and a guest that handled the rebuild should not be told off for it.
//
//go:wasmexport fk_migrate
func onMigrate(oldVersion uint32) {
	migrated++
	told = oldVersion
	fk.Log("migrate told=" + strconv.FormatUint(uint64(oldVersion), 10) +
		" sentinel=" + strconv.FormatUint(uint64(sentinel), 10))
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	seen++
	if tick%10 == 0 {
		fk.Log("tick " + strconv.FormatUint(uint64(tick), 10) +
			" seen=" + strconv.FormatUint(uint64(seen), 10) +
			" migrated=" + strconv.FormatUint(uint64(migrated), 10) +
			" told=" + strconv.FormatUint(uint64(told), 10) +
			" sentinel=" + strconv.FormatUint(uint64(sentinel), 10))
	}
}

// TinyGo builds this as a c-shared reactor, so main never runs.
func main() {}
