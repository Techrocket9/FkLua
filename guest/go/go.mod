// The guest substrate is its own module on purpose. It is compiled by TinyGo
// for wasm-unknown, never by the host toolchain: //go:wasmimport is rejected
// outside GOARCH=wasm, so keeping these files inside the parent module would
// break `go build ./...` and `go vet ./...` for everyone.
module github.com/Techrocket9/fklua/guest/go

go 1.24
