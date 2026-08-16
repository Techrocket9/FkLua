// The guest substrate is its own module for the same reason guest/go is:
// //go:wasmimport and //go:wasmexport are rejected outside GOARCH=wasm, so
// these files must never be built by the host toolchain.
//
// The dependency on guest/go exists for ONE import, in gc.go, and it is inert
// unless the guest is built with -gc=custom. Without it these kernels cannot be
// built with a collector at all, and the collector's allocation path is exactly
// what has to be measured on a REAL guest rather than only on churn.
module github.com/Techrocket9/fklua/bench/guests/go

go 1.24

require github.com/Techrocket9/fklua/guest/go v0.0.0

replace github.com/Techrocket9/fklua/guest/go => ../../../guest/go
