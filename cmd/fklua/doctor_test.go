package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A DIAGNOSTIC THAT QUOTES THE DOCS HAS TO BE CHECKED AGAINST THEM.
//
// `fklua doctor` prints "docs say TinyGo 0.41.1" beside what it found, which is
// only useful while the docs say that. A version bump lands in README.md's
// prerequisites table -- that is the table a newcomer reads -- and a constant in
// doctor.go that nobody re-reads would go on quoting the old one forever, with
// the command that exists to reduce confusion becoming a source of it.
//
// This is the "mirror checked in one direction" lesson from the 2026-07-30
// audit, applied before it costs anything: factorio.Hooks matched control.lua
// for every hook it listed and had been missing one for two milestones.
//
// docFactorioVerion is deliberately NOT in the list: the README names no engine
// version in its prerequisites, because 2.0.x and 2.1.x both work and a pinned
// row read as a ceiling. doctor still prints "Factorio 2.0.x" as the baseline
// it probes for, and that quote is doctor's own rather than the README's.
func TestDoctorQuotesTheVersionsTheReadmeNames(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{
		docGoVersion, docTinyGoVersion, docBinaryen, docRustVersion,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("`fklua doctor` prints %q and README.md does not say it: "+
				"either the docs moved and doctor.go did not, or doctor.go is "+
				"quoting a version this project never named", want)
		}
	}
}

// The version scrape has to survive every shape the four tools print, and one
// of them is not dotted at all.
//
// binaryen versions ARE a bare integer -- "wasm-opt version 131" -- and the
// first cut of this required a dot, so it reported binaryen MISSING on a
// machine that had it while the verdict line below the table correctly said the
// Go toolchain was complete. A doctor whose table contradicts its own summary
// is worse than no doctor.
func TestTheVersionScrapeHandlesEveryToolsShape(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"go version go1.26.5 darwin/arm64", "1.26.5"},
		{"tinygo version 0.41.1 darwin/arm64 (using go version go1.26.5 and LLVM version 20.1.1)", "0.41.1"},
		{"wasm-opt version 131", "131"},
		{"cargo 1.97.1 (c980f4866 2026-06-30)", "1.97.1"},
		{"no numbers here", ""},
	} {
		if got := firstVersionLike.FindString(tc.line); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.line, got, tc.want)
		}
	}
}

// doctor takes no arguments, and says so rather than ignoring them: a reader
// who types `fklua doctor --fix` should not be told everything is fine.
func TestDoctorRefusesArguments(t *testing.T) {
	if err := runDoctor([]string{"--fix"}); err == nil {
		t.Error("`fklua doctor --fix` was accepted, which reads as a promise")
	}
}
