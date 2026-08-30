// Command confchanged is the in-game fixture for the CONFIGURATION-CHANGED
// PAYLOAD: what script.on_configuration_changed hands its handler, which the
// FkLua hook discarded for two milestones.
//
// The hook told a guest that SOMETHING about the mod set moved and never what.
// ConfigurationChangedData carries `mod_changes` -- one entry per mod added,
// removed or moved version, keyed by mod name, with the old and new versions --
// plus `mod_startup_settings_changed`, `migration_applied`, `migrations` and the
// map's own old and new version. Nothing in the API references the concept, so
// no generator had ever emitted it; the encode machinery was there all along.
//
// scripts/run-confchanged.sh runs it in a real Factorio by packaging ONE wasm at
// TWO mod versions and loading a save written by the first with the second
// installed. A version bump is the cheapest real mod-set change there is, and it
// is the one a player experiences every time a mod updates.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o confchanged.wasm ./examples/confchanged
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

//go:wasmexport fk_on_init
func onInit() { fk.Log("confchanged: on_init") }

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	if tick == 3 {
		fk.Log("confchanged: still running at tick 3")
	}
}

// THE PARAMETER IS THE WHOLE FEATURE. A guest that declares none is called with
// an argument it discards and behaves exactly as it did, which is what makes
// this a widening of the existing hook rather than a second export.
//
//go:wasmexport fk_on_configuration_changed
func onConfigChanged(p uint32) {
	d := fkapi.ReadConfigurationChangedData(p)

	fk.Log("confchanged: told" +
		" changes=" + strconv.Itoa(len(d.ModChanges)) +
		" startup=" + yn(d.ModStartupSettingsChanged) +
		" migrated=" + yn(d.MigrationApplied) +
		" migrations=" + strconv.Itoa(len(d.Migrations)) +
		" oldmap=" + opt(d.OldVersion) +
		" newmap=" + opt(d.NewVersion))

	// ONE LINE PER MOD CHANGE, which is what four of the thirteen audited mods
	// branch on directly. An absent old_version is a mod that was ADDED and an
	// absent new_version one that was REMOVED, which is the distinction a guest
	// adopting an uninstalled neighbour's entities is entirely about.
	for _, c := range d.ModChanges {
		fk.Log("confchanged: mod " + c.Key +
			" old=" + opt(c.Val.OldVersion) +
			" new=" + opt(c.Val.NewVersion))
	}
}

func opt(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func yn(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
