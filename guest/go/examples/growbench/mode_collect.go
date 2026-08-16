//go:build gbcollect

package main

import "github.com/Techrocket9/fklua/guest/go/fkgc"

// The COLLECTED arm. The GROWTH LAW is what differs from the leaking arm --
// fkgc grows by a capped quarter where TinyGo doubles -- and it is the only
// thing that may differ, so no collection is allowed to run inside the window.
//
// THE THRESHOLD IS RAISED RATHER THAN THE COLLECTOR LEFT OFF, and the reason is
// worth writing down because the first version did leave it on and the numbers
// were unreadable. Every block this guest allocates is RETAINED, so a collection
// here marks the whole live set and reclaims nothing; at the default 1,024-
// granule budget a 40 MiB live set is tens of thousands of steps of pure
// marking, and the mark-phase forward-progress escape then finishes a phase
// unbudgeted -- a 21 ms tick that has nothing to do with growing. That is a
// real collector shape and agents/gc.md prices it; it is simply not what this
// benchmark measures. run-gcbench.sh is where a collection belongs.
const mode = "collect"

func armCollector() { fkgc.SetThreshold(1 << 30) }

func tickCollect() { fkgc.CollectIfNeeded() }
