//go:build !gbcollect

package main

// The LEAKING arm, and the default. Nothing is ever reclaimed, so the heap is
// exactly the blocks and TinyGo's own growHeap owns the growth law -- it
// DOUBLES. This is the arm a Rust guest and a wasip1 guest are stuck with, and
// the one an fkgc-side policy change cannot reach.
const mode = "leak"

func armCollector() {}

func tickCollect() {}
