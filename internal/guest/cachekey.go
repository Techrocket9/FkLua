package guest

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// THE `go test` CACHE DOES NOT KEY ON guest/**, AND THIS IS THE KEY.
//
// `go test` caches a package's result against the Go inputs it can see: the
// package's own sources, its dependencies, the command line, the environment,
// and THE FILES THE TEST BINARY ITSELF OPENED. Everything a guest-dependent
// test actually depends on falls outside all of those, because the guest is
// built by shelling out -- `tinygo build`, `cargo build` -- and a subprocess's
// file opens are invisible to the cache. So editing `guest/rust/fkgc/src/*.rs`
// and re-running the same `-run` filter replays the PREVIOUS run's output
// verbatim, exit status and all.
//
// That has cost this repo real time twice, and the second one is why this file
// exists rather than another paragraph:
//
//   - The Rust collector's first defect (a size-class table read before lazy
//     initialisation) was nearly missed twice, because each attempted fix
//     re-ran green against the binary built before it. `agents/gc.md` records
//     it as "the same cache lesson in a new dress".
//   - Sharding stage C cost FOUR wrong conclusions in a row to the shell
//     script's version of the same bug, which is what `agents/testing.md`'s
//     "A build cache keyed on nothing is not a cache" was written about.
//
// The fix there was to delete the cache. That is not available here -- the Go
// test cache is `go test`'s, not this repo's -- so the other half of the same
// lesson applies instead: **a cache needs a key.** Reading the guest sources
// from inside the test binary is exactly that. The cache already hashes the
// content of every file a test opened, which is why a `testdata/` fixture
// invalidates a cached result and a subprocess's inputs do not; opening
// guest/** puts it back on the right side of that line.
//
// -count=1 was the alternative and it is strictly worse in two ways: it turns
// the cache OFF rather than making it correct, so an unrelated edit re-runs
// minutes of TinyGo and cargo for nothing, and it only helps somebody who went
// through the entry point that carries the flag. A contributor running
// `go test ./internal/guest -run Rust` by hand -- which is exactly what the
// collector work was doing when it was bitten -- gets nothing from a Makefile.
//
// A PACKAGE OPTS IN BY CALLING THIS FROM A TEST, and the three that need to
// (internal/guest, internal/luagen, cmd/fklua) each have a one-assertion test
// that does. It cannot be done for them from here: the cache tracks the opens
// of each package's own test binary, so a call in one package says nothing
// about another's.

// GuestSourceRoots are the trees whose contents a guest-dependent test result
// really depends on, relative to the repo root.
var GuestSourceRoots = []string{
	filepath.Join("guest", "go"),
	filepath.Join("guest", "rust"),
}

// guestSourceSkipDirs are directories under those roots that are build output
// rather than input. `guest/rust/target` is the one that matters: it is
// gitignored, it is hundreds of megabytes, and hashing it would key the cache on
// the artifacts instead of the sources -- the exact inversion this is here to
// prevent.
var guestSourceSkipDirs = map[string]bool{
	"target": true,
	"build":  true,
}

// SourceKey opens every source file under GuestSourceRoots and returns a hash of
// their contents together with the number of files read.
//
// The hash is not the point and nothing compares it across runs; OPENING the
// files is the point, because that is what the `go test` cache records. The
// value is returned anyway so a caller can log it and so the read cannot be
// optimised away.
//
// The file COUNT is returned because a corpus walk that matched nothing passes
// forever -- the habit `agents/testing.md` states as "count what you audited and
// fail on zero". A caller that does not check it has a cache key of the empty
// set, which is the state this whole file exists to rule out.
func SourceKey() (sum uint64, files int, err error) {
	root, err := repoRoot()
	if err != nil {
		return 0, 0, err
	}
	h := fnv.New64a()
	for _, rel := range GuestSourceRoots {
		dir := filepath.Join(root, rel)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return 0, 0, fmt.Errorf("%s does not exist: GuestSourceRoots is stale", rel)
		}
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if guestSourceSkipDirs[name] || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || strings.HasPrefix(name, ".") {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			// The path goes into the hash as well as the bytes, so a rename
			// with no edit still moves the key.
			fmt.Fprintf(h, "\x00%s\x00", filepath.ToSlash(p[len(root)+1:]))
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
			files++
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
	}
	return h.Sum64(), files, nil
}

// repoRoot walks up from the working directory to the module root. Every test
// binary runs in its own package directory, so the depth differs per caller and
// a relative path would not.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
