//go:build !fkgcbump

package main

// The collector, when there is one.
//
// Under any -gc except custom this is an empty package, so the -gc=leaking
// build these kernels are normally measured with is byte-for-byte what it was
// and scripts/bench-guests.sh is unaffected. Under -gc=custom it supplies the
// seven runtime hooks, which is what makes an allocation-path A/B on a REAL
// guest possible: real_names allocates a buffer per iteration and real_entities
// allocates nothing in its hot loop, which is exactly the pair agents/gc.md's
// stage-B kill criterion names.
import _ "github.com/Techrocket9/fklua/guest/go/fkgc"
