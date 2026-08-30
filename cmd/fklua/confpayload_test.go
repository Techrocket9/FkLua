package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// THE HOOK PAYLOAD'S LAYOUT IS PRUNED BY AN EXPORT, and it is the one entry in
// the packaged table that no constant scan can decide.
//
// Members, events and defines are pruned by looking for an i32 constant reaching
// an import: the guest asks for them, so the ASK is in the wasm. Nothing asks
// for ConfigurationChangedData -- Factorio raises the hook and hands it over --
// so what says whether the layout can ever be used is whether the guest exports
// the hook at all. A guest that does not can never be handed one, and packaging
// the layout for it would be bytes in every save and every multiplayer join for
// a dispatch that cannot happen.
func TestTheHookPayloadIsPackagedOnlyForAGuestThatExportsTheHook(t *testing.T) {
	for _, arm := range []struct {
		name    string
		exports []string
		want    bool
	}{
		{"exports the hook", []string{"fk_on_configuration_changed"}, true},
		{"does not", nil, false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
			back := chdir(t, dir)
			defer back()

			out := filepath.Join(dir, "out")
			if err := runMod([]string{hookGuest(t, arm.exports),
				"--name", "hook-mod", "--version", "0.1.0", "--author", "x",
				"-o", out}); err != nil {
				t.Fatal(err)
			}
			table := packagedAPITable(t, out, "hook-mod_0.1.0")
			has := strings.Contains(table, "confchanged = {size=")
			if has != arm.want {
				t.Errorf("confchanged present=%v, want %v: the layout is keyed on "+
					"the %s export, because there is no id to scan for",
					has, arm.want, factorio.ConfChangedHook)
			}
		})
	}
}

// hookGuest writes a guest that calls one member (so a table is attached at all)
// and exports whatever it is told to.
func hookGuest(t *testing.T, exports []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	var b strings.Builder
	b.WriteString(`(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick")
    (drop (call $call (i32.const 1) (i32.const 1) (i32.const 0) (i32.const 64))))`)
	for _, e := range exports {
		fmt.Fprintf(&b, "\n  (func (export %q) (param i32))", e)
	}
	b.WriteString(")")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
