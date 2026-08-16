//go:build gcstw

package main

import "github.com/Techrocket9/fklua/guest/go/fkgc"

// The STOP-THE-WORLD arm: stage B's collector, unchanged, driven by the same
// trigger. fkgc.Collect() runs a whole mark and sweep before it returns, inside
// the fk_on_tick dispatch -- which is exactly the pause stage C exists to break
// up, and the number this arm is here to produce.
const mode = "stw"

func collect() {
	if fkgc.Stats().SinceGC >= threshold {
		fkgc.Collect()
	}
}
