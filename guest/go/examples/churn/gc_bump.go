//go:build fkgcbump

package main

// The measurement arm: -gc=custom's plumbing with a bump allocator that never
// frees. Selected with `-tags fkgcbump`, and only by the stage-B harness.
import _ "github.com/Techrocket9/fklua/guest/go/fkgc/bump"

func gcCollect()                 {}
func gcTick() bool               { return false }
func gcEnabled() bool            { return false }
func gcStat(which uint32) uint32 { return 0 }
