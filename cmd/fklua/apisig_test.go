package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// A STALE WASM AT ONE PIN, which the pin stamp cannot see.
//
// `fklua mod` packages a guest built against OLD bindings with a FRESH member
// table at the same pin, and until this landed it said nothing. The pin stamp
// proves both halves came from one DESCRIPTION and cannot prove they came from
// one GENERATION -- and at one pin the ids move whenever the generator grows: a
// member kind added, an operator's write half emitted, three global functions
// appended, a handle variant over an attribute. Every id then resolves to a
// different member, silently wherever the kinds line up, with the first symptom
// in a player's game. Reported by BetterBeltBalancer as FKLUA-GAPS item 18.

// The stamp is emitted by BOTH generators and is the SAME NAME, which is what
// lets one project with two guest languages have one answer.
//
// It is also the property that makes the packager's comparison meaningful at
// all: `APISignature` is called by both generators and by the packager, so a
// checker that computed a different digest would find no match and stay quiet --
// PinExport's own argument, one function and three callers.
func TestBothGeneratorsStampTheSameSignature(t *testing.T) {
	root := moduleRoot(t)
	goSrc, err := os.ReadFile(filepath.Join(root, "guest", "go", "fkapi", "fkapi.go"))
	if err != nil {
		t.Fatal(err)
	}
	rsSrc, err := os.ReadFile(filepath.Join(root, "guest", "rust", "fkapi", "src", "api.rs"))
	if err != nil {
		t.Fatal(err)
	}
	a := loadAPI(t, factorio.DefaultAPIVersion)
	want := factorio.SigExport(factorio.APISignature(a))
	for _, b := range []struct {
		lang string
		src  []byte
	}{
		{"go", goSrc}, {"rust", rsSrc},
	} {
		if !strings.Contains(string(b.src), want) {
			t.Errorf("the committed %s bindings do not carry %s: the stamp and the "+
				"digest the packager computes have drifted apart", b.lang, want)
		}
	}
}

// A GUEST WHOSE SIGNATURE DOES NOT MATCH IS NAMED, LOUDLY, AND PACKAGED.
//
// A WARNING and not a refusal, because the digest is conservative in the wrong
// direction: a generator change that only APPENDS members leaves every existing
// id meaning what it meant -- the three global functions were appended after
// every class precisely so that they would -- and a whole-table digest cannot
// tell that from a renumbering. Refusing would stop builds that are correct,
// which is what checkAPIPin's silence-on-absent rule avoids from the other side.
func TestAGuestBuiltAgainstOtherBindingsIsWarnedAbout(t *testing.T) {
	a := loadAPI(t, factorio.DefaultAPIVersion)
	real := factorio.SigExport(factorio.APISignature(a))
	stale := factorio.SigExport("000000000000")

	for _, arm := range []struct {
		name  string
		stamp string
		warn  bool
	}{
		{"the current generation", real, false},
		{"an older generation", stale, true},
		// An ABSENT stamp is silence, exactly as an absent pin is: bindings
		// older than the stamp carry none, and refusing or nagging about those
		// would be noise on every correct build made against an older checkout.
		{"no stamp at all", "", false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
			back := chdir(t, dir)
			defer back()

			var stamps []string
			stamps = append(stamps, factorio.PinExport(factorio.DefaultAPIVersion))
			if arm.stamp != "" {
				stamps = append(stamps, arm.stamp)
			}
			out := filepath.Join(dir, "out")
			stderr := captureStderr(t, func() {
				if err := runMod([]string{sigGuest(t, stamps),
					"--name", "sig-mod", "--version", "0.1.0", "--author", "x",
					"-o", out}); err != nil {
					t.Fatal(err)
				}
			})

			warned := strings.Contains(stderr, "DIFFERENT GENERATION")
			if warned != arm.warn {
				t.Errorf("warned=%v, want %v; stderr:\n%s", warned, arm.warn, stderr)
			}
			if arm.warn {
				for _, want := range []string{arm.stamp, real, "REBUILD THE GUEST"} {
					if !strings.Contains(stderr, want) {
						t.Errorf("the warning does not name %q:\n%s", want, stderr)
					}
				}
			}
			// PACKAGED EITHER WAY. The whole point of a warning is that the
			// build finishes; a mod that stopped here would be a refusal wearing
			// a warning's words.
			if _, err := os.Stat(filepath.Join(out, "sig-mod_0.1.0", "control.lua")); err != nil {
				t.Errorf("the mod was not packaged: %v", err)
			}
		})
	}
}

// THE DIGEST MOVES WHEN AN ID MOVES, and does not otherwise.
//
// The property the whole mechanism rests on, asserted against real descriptions
// rather than against a synthetic edit: two committed descriptions assign
// different ids to the same members, so their signatures must differ; and one
// description digested twice must not.
func TestTheSignatureIsStableAndVersionSpecific(t *testing.T) {
	a := loadAPI(t, factorio.DefaultAPIVersion)
	if s1, s2 := factorio.APISignature(a), factorio.APISignature(a); s1 != s2 {
		t.Errorf("the signature is not stable across two computations: %s then %s",
			s1, s2)
	}
	other := otherAPIVersion(t)
	b := loadAPI(t, other)
	if factorio.APISignature(a) == factorio.APISignature(b) {
		t.Errorf("%s and %s digest the same, and their member ids differ",
			factorio.DefaultAPIVersion, other)
	}
	if n := len(factorio.APISignature(a)); n != 12 {
		t.Errorf("the signature is %d characters; the export name is built from it "+
			"and a longer one is only harder to read in a warning", n)
	}
}

// sigGuest writes a guest that calls one member and carries the given stamps.
func sigGuest(t *testing.T, stamps []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	var b strings.Builder
	b.WriteString(`(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick")
    (drop (call $call (i32.const 1) (i32.const 1) (i32.const 0) (i32.const 64))))`)
	for _, s := range stamps {
		fmt.Fprintf(&b, "\n  (func (export %q) (nop))", s)
	}
	b.WriteString(")")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureStderr runs f with os.Stderr redirected into a pipe and returns what it
// wrote. The warning goes to stderr rather than stdout because stdout carries
// the build report a script may parse.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	f()
	w.Close()
	os.Stderr = old
	return <-done
}

// loadAPI reads a committed description by version.
func loadAPI(t *testing.T, version string) *factorio.API {
	t.Helper()
	a, err := factorio.LoadAPI(filepath.Join(moduleRoot(t), "api", version,
		"runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
