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
//
// IT ARRIVED HERE LATE, and what it unblocked is a gate rather than a run.
// TestTheEventIdSurvivesTheFkipcSubscribeCallSite builds this example and
// proves fkipc's own event id still reaches fk.subscribe as a constant, and it
// could only ever build the LEAKING arm -- because without this file
// -gc=custom does not link (missing core function "runtime.free"). The arm it
// could not build is the one every real mod ships, and it is the arm where the
// generated subscribe wrappers stopped being inlined (see
// internal/factorio/gogen.go's SubscribeFiltered, filed by BetterBeltBalancer
// as item 30). Adding the import costs the leaking arm nothing, measured: the
// wasm is byte-identical either side of this file existing.
import _ "github.com/Techrocket9/fklua/guest/go/fkgc"
