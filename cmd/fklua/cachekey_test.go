package main

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
)

// `fklua init`'s tests build the project this command scaffolds, in Go and in
// Rust, and the gc tests build a collected guest -- all by shelling out, which
// `go test`'s cache cannot see. See internal/guest/cachekey.go.
func TestTheGoTestCacheKeysOnTheGuestSources(t *testing.T) {
	sum, files, err := guest.SourceKey()
	if err != nil {
		t.Fatalf("reading the guest sources: %v", err)
	}
	if files == 0 {
		t.Fatal("read NO guest source files, so this package's cached result " +
			"is keyed on nothing a guest build depends on")
	}
	t.Logf("keyed on %d guest source files, fnv64a=%016x", files, sum)
}
