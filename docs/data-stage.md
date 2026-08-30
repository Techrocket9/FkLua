# The data stage from a guest

Factorio loads a mod in two halves. The **control stage** is a running program with events, ticks and saved state, and it is what `fklua mod` has always compiled. The **settings** and **data** stages are declarative: they run once at load, in their own Lua states, and their whole job is to fill in `data.raw`. This page is how to write those in Go or Rust instead of in Lua.

A data stage written this way is a **second wasm module**, compiled from its own `main` package or crate and packaged beside the control guest. It reaches `data.raw` through the `fkdata` library and nothing else: there is no `game`, no `script`, no `storage` and no runtime API at these stages, so `fkapi` has nothing to reach there and packaging refuses a data module that imports it.

## The four hooks

One export per stage. `fklua mod` writes a stage file for each hook the module actually exports, and for no others, so a mod with only a data stage gets no `settings.lua`.

| export | file Factorio loads | what it is for |
|---|---|---|
| `fk_settings` | `settings.lua` | mod settings, before `data.raw` exists |
| `fk_data` | `data.lua` | prototypes |
| `fk_data_updates` | `data-updates.lua` | patching another mod's prototypes |
| `fk_data_final_fixes` | `data-final-fixes.lua` | the last word |

## A first data stage, in Go

```go
package main

import "github.com/Techrocket9/fklua/guest/go/fkdata"

//go:wasmexport fk_data
func onData() {
    fkdata.Extend(fkdata.Obj(
        fkdata.KVs("type", fkdata.Str("item")),
        fkdata.KVs("name", fkdata.Str("my-item")),
        fkdata.KVs("icon", fkdata.Str("__base__/graphics/icons/iron-plate.png")),
        fkdata.KVs("icon_size", fkdata.Num(64)),
        fkdata.KVs("stack_size", fkdata.Num(50)),
    ))
}
```

and in Rust:

```rust
#![no_std]
extern crate alloc;
use fkdata::{num, obj, str_};

#[no_mangle]
pub extern "C" fn fk_data() {
    fkdata::extend(&[obj(&[
        ("type", str_("item")),
        ("name", str_("my-item")),
        ("icon", str_("__base__/graphics/icons/iron-plate.png")),
        ("icon_size", num(64.0)),
        ("stack_size", num(50.0)),
    ])]);
}
```

Build it exactly as a control guest is built, and package the two together:

```sh
tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
    -o dist/data.wasm ./datastage
fklua mod dist/control.wasm --data-module dist/data.wasm
```

or put it in the manifest and drop the flag:

```toml
[fklua]
api = "2.0.77"
data_module = "dist/data.wasm"
```

## A mod with no control stage at all

The control guest is optional. A mod that is only prototypes is an ordinary Factorio mod, so leave the positional argument out and package the data module on its own:

```sh
fklua mod --data-module dist/data.wasm --name my-shim --version 1.0.0 --author me
```

The result ships `info.json`, `fk_abi.lua`, `fk_data.lua`, `fk_data_module.lua` and one file per stage hook, plus whatever `--include` carries. There is no `control.lua`, no `fk_module.lua` and no `fk_api_gen.lua`; `fk_abi.lua` stays because `fk_data.lua` requires it for the codec.

`--persist`, `--gc` and `--fuel` are refused here rather than ignored, because each describes how a control guest is compiled and there is none. A data module is always `--persist=none` and `-gc=leaking` whatever else is asked for. The refusal is on the typed flag only, so a project whose manifest sets `gc` can still package a data-only mod from the command line. Giving neither module is an error.

## The API

Seven operations. Everything that is a failure raises at the stage, naming the stage and the path, because a broken data stage should stop the load rather than produce a mod that is quietly wrong.

| Go | Rust | |
|---|---|---|
| `Stage()` | `stage()` | which of the four stages is running |
| `Get(path ...any) (V, bool)` | `get(&[P]) -> Option<V>` | read `data.raw` at any depth |
| `Set(value V, path ...any)` | `set(&V, &[P])` | write at any depth; `Nil()` deletes |
| `Extend(protos ...V)` | `extend(&[V])` | `data:extend` |
| `Clone(typ, from, to string)` | `clone_(typ, from, to)` | deep-copy one prototype under another name |
| `Keys(path ...any) []string` | `keys(&[P]) -> Vec<String>` | the string keys at a path, sorted |
| `Mods()`, `ModVersion(name)` | `mods()`, `mod_version(name)` | installed mods and versions |
| `FeatureFlag(name)` | `feature_flag(name)` | `space_travel`, `quality`, and the rest |
| `StartupSetting(name) (V, bool)` | `startup_setting(name)` | one startup setting's value |
| `Log(s string)` | `log(s)` | a line in `factorio-current.log` |

A **path** is strings and numbers rooted at `data.raw`, so a field two levels inside an array is reachable:

```go
count, ok := fkdata.Get("technology", "logistics", "unit", "count")
fkdata.Set(fkdata.Num(-0.35), "transport-belt", "my-belt", "collision_box", 1, 1)
```

`Get` returning `false` means the path is not there. That is a normal answer rather than an error, and it is what "has anybody already defined this" looks like:

```go
if _, taken := fkdata.Get("item", "my-item"); !taken {
    fkdata.Extend(myItem())
}
```

`Set` with `Nil()` **deletes** the key rather than writing `false`. That matters for the common shape of stripping a cloned prototype, where a dozen fields have to be absent rather than present-and-false.

## Cloning a prototype

`Clone` is the engine's own `util.table.deepcopy`, made on the guest's instruction. Patch the copy afterwards with `Set`:

```go
fkdata.Clone("transport-belt", "express-transport-belt", "my-belt")
fkdata.Set(fkdata.Num(0.25), "transport-belt", "my-belt", "speed")
fkdata.Set(fkdata.Nil(), "transport-belt", "my-belt", "minable")
fkdata.Set(fkdata.Nil(), "transport-belt", "my-belt", "next_upgrade")
```

It is a primitive rather than a `Get` plus an `Extend`, and the reason is fidelity rather than speed. Reading a whole prototype into the guest and writing it back re-serialises every leaf, so any field the value model cannot express, any float that does not round-trip and any key it drops would change the prototype while the mod still loads. Under a clone the untouched leaves are literally the bytes the source shipped. A real belt prototype has around 500 scalar leaves and a patch touches a handful of them.

`CloneTo(srcType, srcName, dstType, dstName)` is the same thing across prototype types.

## Ordering, and mixing with hand-written Lua

By default a generated stage file calls the guest and nothing else. `[stages]` says what order the file loads things in, with `"@guest"` standing for the guest's own hook:

```toml
[stages]
data             = ["prototypes.entity", "@guest", "prototypes.sprite"]
data_final_fixes = ["@guest"]
```

The same thing on the command line, one flag per stage: `--stage data=prototypes.entity,@guest,prototypes.sprite`.

**This section is a ramp, and an empty one is the destination.** It exists so a mod can move its data stage into Go one file at a time, with the guest and the remaining Lua sitting in one stage file in an order the author states, which is all `data.lua` has ever been. When the last `require` goes, the key goes: an absent key with the hook exported means `["@guest"]`, and an absent key with no hook means the file is not generated at all.

Three rules follow from that:

- A chain with no `"@guest"` in it is a pure-Lua stage file, which is also what a data-stage-only mod wants.
- A chain naming `"@guest"` for a hook the module does not export is an error at package time.
- An included file called `data.lua` collides with the generated one and is an error. That is the halfway house: rename the hand-written file and name it in the chain.

## What a data guest does not have

- **No runtime API.** `fkapi` is refused, and it would not work: the settings and data stages have no `game`, no `script` and no entities.
- **No state across stages.** The module is instantiated fresh for each stage it hooks, because Factorio's settings stage is its own Lua state and `require` re-executes a file at every stage. A package-level variable set in `fk_data` is back at its initial value in `fk_data_updates`. Keep things in `data.raw`, which is what Factorio's own stages do.
- **No `settings` at the settings stage.** A mod's startup settings are not readable while they are being declared, so `StartupSetting` answers `false` for everything there.
- **No collector and no persistence.** A data module runs once and dies with the Lua state, so it is compiled `--persist=none` and `-gc=leaking` whatever the control guest uses.

## Sharing one config between the two stages

A mod's two stages are two wasm modules in two Lua states, so a value both of them need has to reach both. There are three channels, and which one you want is decided by WHEN the value is known.

**A config you know when you write the mod goes in a package both guests import.** The data module and the control guest are two `main` packages in ONE Go module (or two crates in one Cargo workspace), so this is an ordinary import and needs no mechanism at all. The one constraint is the reason it works: `fklua mod` refuses a data module that imports `fkapi`, and a control guest cannot import `fkdata`, **so the shared package must import neither**.

```go
// guest/go/cfg/cfg.go -- imports nothing
package cfg

type Category struct {
    Name  string
    Rungs int
}

var Categories = []Category{{"mining-speed", 8}, {"lab-speed", 6}}

const SettingPrefix = "my-mod-"
```

Both `main` packages import `cfg`, and one edit moves both stages together. Writing the table twice is what this replaces, and a disagreement between two copies is a mod that toggles technologies that do not exist.

**A config the data stage COMPUTES goes in a `mod-data` prototype.** When the value depends on a startup setting, or on what other mods defined, the shared package cannot hold it: it is not known until the data stage runs. Factorio's own answer is a prototype whose whole purpose is to carry a block of arbitrary data across into the runtime.

```go
// the data guest, after it has computed whatever it computed
fkdata.Extend(fkdata.Obj(
    fkdata.KVs("type", fkdata.Str("mod-data")),
    fkdata.KVs("name", fkdata.Str("my-mod-config")),
    fkdata.KVs("data_type", fkdata.Str("config")),
    fkdata.KVs("data", fkdata.Obj(
        fkdata.KVs("rungs", fkdata.Num(8)),
        fkdata.KVs("label", fkdata.Str("mining-speed")),
    )),
))
```

```go
// the control guest, at load
raw, err := fkapi.Prototypes.ModDataRaw()
if err != nil {
    return
}
md, err := fkapi.LuaCustomTable{Object: raw}.Get(fkapi.OfString("my-mod-config"))
if err != nil || !md.Object.Valid() {
    return
}
blob, err := fkapi.LuaModData{Object: md.Object}.Data()
```

`data_type` is a free-form string a mod declares so another mod can find its block, and `DataTypeIs` compares it on the host. The blob is `AnyBasic`: numbers, strings, booleans and nested tables of them, so the guest reads it as tier-2 values rather than as a typed struct. That is the residual, and it is much smaller than encoding a config into a settings string and decoding it back.

Determinism is the data stage's, and it is already settled: every client runs the data stage, and a client whose prototype set differs from the server's is refused at join rather than desyncing. So a blob computed from startup settings is identical on every peer that could join at all, which is exactly the property a control guest branching on it needs.

**A startup setting is readable from both stages and is not a channel.** The data guest reads one with `StartupSetting` and the control guest reads `settings.startup` at runtime, so a value the PLAYER chose needs nothing here. What a setting cannot carry is a value DERIVED from it, which is what sends people to smuggling data through the `order` fields of hidden prototypes. Use `mod-data` for that.

The Rust side of the shared-crate pattern is the same shape and is unverified here: a crate declaring no features is outside the workspace feature-unification hazard that makes the collector a command-line flag rather than a manifest one, but nothing has built it.

## Keeping your section functions

A data stage is usually a few hundred straight-line prototype definitions, and if you split them across section functions for readability the optimizer will inline those sections back into one. Past a certain size that produces a function Lua cannot parse, because a single jump inside it exceeds what Lua can encode. Packaging catches it and names the function, but the error is easiest to avoid up front: put `//go:noinline` (Go) or `#[inline(never)]` (Rust) on each section function. This is the guest shape most likely to reach that limit; see [Limits a generated guest can reach](lua-limits.md).

## Determinism

Every mod's data stage runs on every client, and a client whose prototype set differs from the server's is refused at join. So nothing the library hands a guest carries an iteration order it could branch on: `Keys` is sorted, `Mods` is a sorted slice rather than a map, and every dictionary a `Get` returns is sorted by key at every nesting level. Maps a guest sends are sorted on the way out too, so what reaches the engine is a function of what the guest meant rather than of the order it built the table in.

Two things follow for guest code. Enumerate with `Keys` rather than by walking a returned dictionary's pairs, when a tie has to break the same way everywhere. And take care with floating-point constants if you keep both a Go and a Rust version of the same mod: Go's untyped constants are arbitrary-precision and Rust's `const f64` arithmetic is IEEE f64, so `0.3 + 0.104` folds to two different doubles. Use a runtime variable when the two have to agree exactly.

## Verifying it

Factorio can run the data stage and dump the result, without ever reaching `control.lua`:

```sh
factorio -c "$USERDIR/config/config.ini" --mod-directory "$MODS" --dump-data
```

That writes `script-output/data-raw-dump.json` and `script-output/mod-settings-dump.json` in about two and a half seconds, and it is the strongest check available for a data stage: it is the game's own prototype table, after every mod has run.

Two notes on comparing dumps. **Key order in the dump is insertion order**, so two builds differing only in the order they call `Extend` produce byte-different dumps that describe the same game. Normalise before hashing:

```sh
jq -S . script-output/data-raw-dump.json | shasum -a 256
```

And **the engine's own `Prototype list checksum` is not an equivalence proof.** It is order-insensitive, which is convenient, and it is blind to field values: it does not move when a prototype's `stack_size` goes from 1 to 42. Use it as a smoke test and hash the normalised dump for anything that matters.

A hash is per engine and per mod set, because the dump contains every mod that ran, bundled DLC data included.

## Where to look next

`guest/go/examples/datastage` and `guest/rust/examples/datastage` are the worked examples, written as line-for-line mirrors: a setting, prototypes from a computed loop, a read-then-extend, a clone and its patches, a sorted enumeration, and an "is this already defined" check. [`docs/generated-files.md`](generated-files.md) lists every file `fklua mod` writes and which are yours to edit.
