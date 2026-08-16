package luagen

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
)

// This package's corpus audits build EVERY guest this repo ships, in both
// toolchains -- TestEveryGuardAGuestReadsIsAlsoSeeded and the shard census --
// so its cached result depends on `guest/**` and `go test` cannot see that.
// See internal/guest/cachekey.go for why this is a call rather than a -count=1
// in the Makefile.
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
