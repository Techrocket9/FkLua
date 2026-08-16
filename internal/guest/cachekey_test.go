package guest_test

import (
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
)

// THE CACHE KEY THIS PACKAGE'S RESULTS DEPEND ON.
//
// Everything expensive here builds a guest by shelling out to tinygo or cargo,
// and `go test`'s cache cannot see a subprocess's inputs -- so without this
// call, editing `guest/rust/fkgc/src/heap.rs` and re-running the collector suite
// replays the previous run's verdict. `internal/guest/cachekey.go` has the whole
// argument and the two occasions it has already cost this repo.
//
// Opening the files is the entire mechanism; the hash is logged rather than
// asserted, because there is nothing to compare it against and nothing needs to
// be. What IS asserted is the file count, for the reason a corpus test always
// asserts one: a walk that matched nothing would key the cache on the empty set
// and pass forever.
func TestTheGoTestCacheKeysOnTheGuestSources(t *testing.T) {
	sum, files, err := guest.SourceKey()
	if err != nil {
		t.Fatalf("reading the guest sources: %v", err)
	}
	if files == 0 {
		t.Fatal("read NO guest source files, so this package's cached result " +
			"is keyed on nothing that a guest build depends on -- check " +
			"guest.GuestSourceRoots against the tree")
	}
	t.Logf("keyed on %d guest source files, fnv64a=%016x", files, sum)
}
