// Package luart embeds the hand-written Lua runtime that generated chunks
// depend on.
//
// The Lua lives in runtime/lua/ as ordinary readable source rather than inside
// a Go string, because it is real code that gets debugged, benchmarked and read
// by humans. This package exists only because go:embed cannot reach outside its
// own directory, so the embed declaration has to sit next to the files.
package luart

import (
	_ "embed"
)

//go:embed lua/fk_rt.lua
var prelude string

//go:embed lua/fk_mod.lua
var modGlue string

//go:embed lua/fk_abi.lua
var abi string

//go:embed lua/fk_data.lua
var dataStage string

// Prelude returns the runtime source that is inlined at the top of every
// generated chunk.
//
// It is inlined rather than required: Factorio allows `require` only at the top
// level of control.lua, and a self-contained chunk is also what makes the
// generated Lua runnable under lua52f in tests.
func Prelude() string { return prelude }

// ModGlue returns the hand-written control.lua a packaged mod ships: it binds
// the host ABI, instantiates the generated module and registers whichever event
// handlers the guest exports.
//
// Copied out verbatim rather than generated, so a packaged mod's control.lua
// can be read and edited in place, and diffed against runtime/lua/fk_mod.lua.
func ModGlue() string { return modGlue }

// ABI returns the host-call ABI: the handle table a packaged mod requires as
// fk_abi.lua.
//
// A separate file rather than part of the prelude, because it belongs to the
// HOST side. The prelude is inlined into the generated chunk and is what guest
// code calls into; this is what control.lua uses to hand LuaObjects across the
// boundary, and it never appears inside a compiled function.
func ABI() string { return abi }

// DataStage returns the data-stage ABI: the shim a packaged mod requires as
// fk_data.lua, from the settings.lua / data.lua / data-updates.lua /
// data-final-fixes.lua files fklua generates.
//
// A third hand-written file rather than part of fk_mod.lua, because the two
// stages share nothing: this one has no `game`, no `script`, no `storage`, no
// events and no persistence, and the module it instantiates is a different wasm
// module compiled from a different main package. It requires fk_abi.lua for the
// tier-2 codec and binds only what that needs.
func DataStage() string { return dataStage }
