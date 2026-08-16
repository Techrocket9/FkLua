//go:build !gcstw

package main

import "github.com/Techrocket9/fklua/guest/go/fkgc"

// The PACED arm, and the default: CollectIfNeeded starts a collection when heap
// pressure says one is due, and control.lua runs one bounded step per tick from
// a one-shot on_tick until it finishes. Under -gc=leaking every call here is a
// no-op on an empty package, which is what makes this the baseline arm too.
const mode = "paced"

func collect() { fkgc.CollectIfNeeded() }
