//go:build fkgcbump

package main

// The measurement arm: -gc=custom's plumbing with a bump allocator that never
// frees, which prices the plumbing on its own. See guest/go/fkgc/bump.
import _ "github.com/Techrocket9/fklua/guest/go/fkgc/bump"
