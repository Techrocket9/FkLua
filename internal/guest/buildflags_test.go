package guest_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
)

// THE TWO BUILD-FLAG LISTS ARE MIRRORED BY COMMENT, and a mirror checked in one
// direction drifts in the other.
//
// `internal/guest.BuildFlags` is what this repo's own harnesses build guests
// with; `guest/go/fk.BuildFlags` is what a guest AUTHOR reads, in the package
// they already import, and it is the one that reaches a downstream mod's build
// script. Each says "kept in step with" the other in a comment and nothing has
// ever checked.
//
// That is exactly the shape the 2026-07-30 audit named as its third outliving
// lesson -- `factorio.Hooks` matched control.lua for every hook it listed and
// had been missing one for two milestones -- and the fix here is the same one:
// read the other side's source and compare, in both directions.
//
// It has to be read from SOURCE rather than imported. guest/go is its own Go
// module, because //go:wasmimport needs GOARCH=wasm, so this module cannot link
// against it at all. Parsing is enough: these are literal string slices and the
// file does not have to type-check for its declarations to be readable.
//
// The consequence of drift is not cosmetic. A guest built with -gc=leaking and
// packaged --gc=collected imports a collector that is an empty package, so
// nothing arms the barrier, nothing steps, and the heap grows exactly as it did
// before -- silently. The reverse -- -gc=custom packaged --gc=leaking -- inlines
// the 8-byte store past the page mark, which is a live object swept.
func TestTheBuildFlagListsMatchTheGuestModule(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "guest", "go", "fk", "fk.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	got := map[string][]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			var out []string
			for _, e := range lit.Elts {
				b, ok := e.(*ast.BasicLit)
				if !ok || b.Kind != token.STRING {
					out = nil
					break
				}
				out = append(out, b.Value[1:len(b.Value)-1])
			}
			if out != nil {
				got[vs.Names[0].Name] = out
			}
		}
	}

	// Both directions, and the missing-name direction is the one the audit says
	// gets skipped: a list added on one side and not the other is drift too.
	want := map[string][]string{
		"BuildFlags":          guest.BuildFlags,
		"CollectedBuildFlags": guest.CollectedBuildFlags,
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("guest/go/fk has no %s, but internal/guest does (%v). A "+
				"guest author reads the guest module's copy; a list only this "+
				"module has is a list nobody building a mod will ever see",
				name, w)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s has drifted:\n  internal/guest: %v\n  guest/go/fk:    %v\n"+
				"These are the flags a guest is BUILT with, and every one of them "+
				"is load-bearing -- a guest built with the wrong -gc against a "+
				"chunk emitted for the right one is a live object swept, with no "+
				"error anywhere", name, w, g)
		}
		delete(got, name)
	}
	for name, g := range got {
		if len(g) > 0 && g[0] == "-target=wasm-unknown" {
			t.Errorf("guest/go/fk declares %s (%v) and internal/guest does not. "+
				"A flag set this repo's own harnesses cannot build with is a flag "+
				"set nothing tests", name, g)
		}
	}
}

// And the collected list differs from the plain one in EXACTLY ONE FLAG.
//
// That is not tidiness. Stage B's whole allocation A/B -- the measurement that
// decided the feature's first kill criterion, and reversed its sign -- is only
// meaningful if the two arms differ in one thing. A second difference would
// make "the allocation path is 0.962x -gc=leaking" a statement about two
// changes at once, and nobody would know which.
func TestTheCollectedFlagsDifferInExactlyTheGC(t *testing.T) {
	a, b := guest.BuildFlags, guest.CollectedBuildFlags
	if len(a) != len(b) {
		t.Fatalf("the two flag sets are different lengths: %v against %v", a, b)
	}
	var diffs []string
	for i := range a {
		if a[i] != b[i] {
			diffs = append(diffs, a[i]+" -> "+b[i])
		}
	}
	if len(diffs) != 1 || diffs[0] != "-gc=leaking -> -gc=custom" {
		t.Errorf("the collected build differs from the plain one in %v; it must "+
			"differ in exactly -gc=leaking -> -gc=custom, or stage B's allocation "+
			"A/B was measuring two changes at once", diffs)
	}
}
