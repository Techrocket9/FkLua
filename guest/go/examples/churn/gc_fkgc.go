//go:build !fkgcbump

package main

// The collector, when there is one.
//
// Under any -gc except custom this import is an empty package, so the
// -gc=leaking build of this guest is what it was: the same source tree builds
// all four arms of the stage-B allocation A/B, and a guest author changes one
// FLAG rather than an import.
//
// The fkgcbump build tag swaps this for guest/go/fkgc/bump, which is the
// measurement instrument that prices the -gc=custom plumbing on its own. See
// gc_bump.go.
import "github.com/Techrocket9/fklua/guest/go/fkgc"

func gcCollect()      { fkgc.Collect() }
func gcTick() bool    { return fkgc.CollectIfNeeded() }
func gcEnabled() bool { return fkgc.Enabled() }

func gcStat(which uint32) uint32 {
	s := fkgc.Stats()
	switch which {
	case 0:
		return s.HeapBytes
	case 1:
		return s.LiveBytes
	case 2:
		return s.FreeBytes
	case 3:
		return s.Collections
	case 4:
		return s.Grows
	case 5:
		return s.LiveObjects
	case 6:
		return s.FreedObjects
	case 7:
		return s.SinceGC
	case 8:
		return fkgc.MetaBytes()
	case 9:
		return s.Phase
	case 10:
		return s.Steps
	case 11:
		return fkgc.Budget()
	case 12:
		return fkgc.MaxStepWork()
	case 13:
		return fkgc.TotalWork()
	}
	return 0
}
