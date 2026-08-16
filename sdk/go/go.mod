// The external half of fkipc: a companion process on the same machine as the
// game, talking to a guest built against guest/go/fkipc.
//
// ITS OWN MODULE, and not part of guest/go, because that module is compiled by
// TinyGo for a wasm target and its fkapi package carries //go:wasmimport
// declarations the host toolchain rejects. Go builds per package, so depending
// on guest/go/fkipc/wire from here compiles the codec and nothing else -- which
// is the point of the codec having no build tags and no imports outside the
// standard library. ONE CODEC, TWO CONSUMERS: a copy in each module is the
// shape this repo has already been burned by twice.
//
// PUBLISHING: the replace below points at this checkout, and a replace
// directive does not travel to anyone who imports this module. Cutting this
// SDK loose for external use means tagging guest/go and replacing the replace
// with a real version requirement -- the directive is here so the two halves
// can be developed and tested together, not as the shipping arrangement.
module github.com/Techrocket9/fklua/sdk/go

go 1.24

require github.com/Techrocket9/fklua/guest/go v0.0.0

replace github.com/Techrocket9/fklua/guest/go => ../../guest/go
