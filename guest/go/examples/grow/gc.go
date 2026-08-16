package main

// The collector, when there is one.
//
// Under any -gc except custom this import is an EMPTY PACKAGE: no symbols, no
// state, no init. So the -gc=leaking build of this example is what it was, and
// turning the collector on is genuinely one flag --
// tinygo build -gc=custom, or fklua's --gc=collected.
//
// It is here in every example on purpose. agents/gc.md's stage-B gate is that
// the whole examples corpus produces the same output under -gc=leaking and
// under the collector, and an example that cannot be BUILT with the collector
// cannot answer that. These examples were not written for this feature and do
// not know it exists, which is what makes them worth asking.
import _ "github.com/Techrocket9/fklua/guest/go/fkgc"
