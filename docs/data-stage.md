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

## Scaffolding one

`fklua init <mod-name> --data` writes the whole shape, in every language the project declares: the data guest itself, its place in the build, and the `data_module` key that names its artifact. For Go that is a second `main` package at `guest/go/data/`, inside the control guest's own module, so a package both stages need is an ordinary import from a sibling. For Rust it is a third crate in `guest/rust/`, depending on `fkdata` and nothing else, with `fkdata` and `fklog` added to the workspace dependencies. `init` then prints the whole recipe, a build line per module and the packaging command that takes them both, and scaffolds it into the data guest's own header; the generated comments carry the two rules below that are easy to get wrong.

Adding a data stage to a project that already exists means writing those files by hand and declaring `data_module` yourself; `init` refuses to overwrite anything.

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

### Building it

A data module is compiled `--persist=none` and `-gc=leaking` whatever the control guest uses, so its build line is not the control guest's. Packaging refuses a data module that carries a collector, and names the fix.

Each language has a recipe of three commands, and they are the ones `fklua init --data` prints and scaffolds into the data guest's header, here for a mod named `my-mod`. Build both modules, then package them together: `data_module` names the DATA module, so the packaging command takes the CONTROL guest's wasm as its positional argument and needs no flag. There is no manifest key for the control module.

In Go the two builds differ in one flag, `-gc=leaking` against `-gc=custom`, and the data guest's package imports no `fkgc`:

```sh
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 \
    -o ../../my-mod.wasm .)
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
    -o ../../my-mod-data.wasm ./data)
fklua mod my-mod.wasm
```

with the key naming the artifact the second line wrote:

```toml
[fklua]
api = "2.0.77"
data_module = "my-mod-data.wasm"
```

In Rust the data crate is a release build with no `--features fk/fkgc`, **in its own cargo invocation**, and both artifacts land under the workspace's target directory rather than at the project root. A cdylib is named after the `[lib]` name with dashes mapped to underscores, which is the one part of these paths nobody guesses:

```sh
(cd guest/rust && cargo build --release --target wasm32-unknown-unknown \
    -p my-mod-guest --features fk/fkgc)
(cd guest/rust && cargo build --release --target wasm32-unknown-unknown -p my-mod-data)
fklua mod guest/rust/target/wasm32-unknown-unknown/release/my_mod_guest.wasm
```

with:

```toml
[fklua]
api = "2.0.77"
data_module = "guest/rust/target/wasm32-unknown-unknown/release/my_mod_data.wasm"
```

The separate invocation is the load-bearing part. Cargo's v2 resolver unifies features across every package built in one command, so a control guest built `--features fk/fkgc` in the same invocation turns the collector on for the data crate too, silently and only for that build. Two commands, one per module, keeps the two apart. There is no source difference between the two arms: the `fk` crate owns the single `#[global_allocator]` site and the feature chooses what backs it.

A project that declares both languages still has one data stage, so `data_module` takes the first language in `lang` order and the other artifact rides beside it as a comment. Both packaging commands then package the module the key names, whichever control guest they name. Each language's recipe therefore builds the module the key names rather than the one that language produces, so the same data build appears in both recipes and either three-line recipe works on its own. To ship the other language's data module, point the key at the path in the comment beside it.

Without the key, `--data-module` says the same thing on the command line. In the Go layout:

```sh
fklua mod my-mod.wasm --data-module my-mod-data.wasm
```

and in the Rust one, where both paths sit under the target directory:

```sh
fklua mod guest/rust/target/wasm32-unknown-unknown/release/my_mod_guest.wasm \
    --data-module guest/rust/target/wasm32-unknown-unknown/release/my_mod_data.wasm
```

## A mod with no control stage at all

The control guest is optional. A mod that is only prototypes is an ordinary Factorio mod, so leave the positional argument out and package the data module on its own:

```sh
fklua mod --data-module my-shim-data.wasm --name my-shim --version 1.0.0 --author me
```

The result ships `info.json`, `fk_abi.lua`, `fk_data.lua`, `fk_data_module.lua`, `fk_data_module.map.json` and one file per stage hook, plus whatever `--include` carries. There is no `control.lua`, no `fk_module.lua` and no `fk_api_gen.lua`; `fk_abi.lua` stays because `fk_data.lua` requires it for the codec. The map is the data module's [debug map](generated-files.md), on the same terms as the control stage's, and `--no-map` leaves it out.

`--persist`, `--gc` and `--fuel` are refused here rather than ignored, because each describes how a control guest is compiled and there is none. A data module is always `--persist=none` and `-gc=leaking` whatever else is asked for. The refusal is on the typed flag only, so a project whose manifest sets `gc` can still package a data-only mod from the command line. Giving neither module is an error.

## The API

Eight operations. Everything that is a failure raises at the stage, naming the stage and the path, because a broken data stage should stop the load rather than produce a mod that is quietly wrong. That includes failures only your own code can detect: `Raise` stops the load with your message, stage-prefixed like every other, where a panic would surface as an opaque trap with your diagnostic lost.

| Go | Rust | |
|---|---|---|
| `Stage()` | `stage()` | which of the four stages is running |
| `Get(path ...any) (V, bool)` | `get(&[P]) -> Option<V>` | read `data.raw` at any depth |
| `Set(value V, path ...any)` | `set(&V, &[P])` | write at any depth; `Nil()` deletes |
| `Extend(protos ...V)` | `extend(&[V])` | `data:extend` |
| `Clone(typ, from, to string)` | `clone_(typ, from, to)` | deep-copy one prototype under another name |
| `Keys(path ...any) []string` | `keys(&[P]) -> Vec<LuaStr>` | the string keys at a path, sorted |
| `Mods()`, `ModVersion(name)` | `mods()`, `mod_version(name)` | installed mods and versions |
| `FeatureFlag(name)` | `feature_flag(name)` | `space_travel`, `quality`, and the rest |
| `StartupSetting(name) (V, bool)` | `startup_setting(name)` | one startup setting's value |
| `ModName() string` | `mod_name()` | this mod's own name, from the packager |
| `DerivedTypes(base)`, `BaseType(derived)` | `derived_types(base)`, `base_type(derived)` | the engine's `defines.prototypes` map, both directions |
| `V.String() string` | `V::bytes() -> &[u8]` | a string value's bytes, empty for anything else |
| (the same method) | `V::string() -> Option<&str>` | those bytes as text, `None` when they are not UTF-8 |
| `Str(s string) V` | `str_(&str) -> V`, `bytes_(&[u8]) -> V` | build a string value |
| `Log(s string)` | `log(s)` | a line in `factorio-current.log` |
| `Raise(msg string)` | `raise(msg) -> !` | stop the load with your own message; never returns |

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

`Raise` hands your message to the host, and the host puts `fklua: at the <stage> stage, ` in front of it, verbatim and nothing else, so the message must not name the stage itself or the player reads it twice. `fkdata.Raise("two technologies share the name hardened-tips")` at the data stage reads:

```
fklua: at the data stage, two technologies share the name hardened-tips
```

Rust's `raise` reads the same.

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
- **No "current mod" in the engine's environment.** `mods` lists every installed mod with no marker for this one, so `ModName` is answered by the packager instead: `fklua mod` writes the manifest's name into the generated stage files. Use it as the prefix for anything you generate. Setting and prototype names are global namespaces, and when two mods declare the same setting name with the same type the engine keeps whichever loaded last, without a warning.
- **No collector and no persistence.** A data module runs once and dies with the Lua state, so it is compiled `--persist=none` and `-gc=leaking` whatever the control guest uses. Packaging refuses a data module that was BUILT with a collector, which is the same check from the other side: the three exports `fk_gc_step`, `fk_gc_dirty_base` and `fk_gc_dirty_cap` are what "there is a collector in here" means to the host, and nothing at any data-family stage drives one. Such a module is several percent larger, all of it parsed at every load, and it also runs the stage under a heavier allocator: the collector is what backs `#[global_allocator]` in Rust and what `-gc=custom` routes allocation through in Go. The refusal names the fix in both languages.

## What the engine checks

A prototype your guest builds is validated by Factorio, not by FkLua, and a value the engine will not take is a mod that does not load. The rules below are not in `runtime-api.json` and each one is a load failure in a player's hands, so they are measured against a real engine rather than quoted: everything here was run on Factorio 2.0.77 (build 84539) with `base` alone, one engine run per case, on 2026-09-07. [`testdata/dataprobe`](../testdata/dataprobe) is the case set and [`scripts/run-dataprobe.sh`](../scripts/run-dataprobe.sh) runs it, so any of it can be re-asked of a newer engine.

**Names.** A prototype name may contain only `A-Z`, `a-z`, `0-9`, `_` and `-`, and may be at most 200 characters. Anything else is `Invalid prototype name. Only characters A-Z a-z 0-9 _- are allowed.` or `Name field is too large. Max allowed size is: 200.`. A dot, a space, a colon, a comma, a slash, an asterisk, brackets and any non-ASCII letter are all outside the set. Since `ModName` is your prefix and a player never types a prototype name, the usual way to hit this is deriving a name from something a player did type.

**Localised strings.** One localised string may carry at most 20 parameters and may nest 19 tables deep. Past either it is `Too many parameters for localised string: 21 > 20 (limit).` or `Too deep recursion for localised string: 21 > 20 (limit).`. Both limits were measured at the settings stage and again at the data stage, on a mod setting's `localised_description` and on an item prototype's, and the engine gives the same sentence for each. Note the engine's own depth counter reads one higher than the number of tables, so its "21" is 20 nested tables. The two limits do not add up: 19 parameters plus a nested group carrying 20 more loads, which is how a description with many entries has to be built.

**Ingredient amounts.** An item ingredient amount is an unsigned 16-bit number above zero: 0 is `Item ingredient can't have count of 0.`, 65535 loads, and 65536 is `Value (65536) outside of range. The data type allows values from 0 to 65535`. Naming one item twice in a recipe is `Duplicate item ingredients are not allowed`.

**Fluids.** A fluid amount must be above zero, and a fluid ingredient needs a category that takes one: in the default `crafting` category it is `Recipe is in 'crafting' category but has a non-item ingredient 'water' (fluid).`, and in `crafting-with-fluid` the same recipe loads. **A fluid amount past 1.0715e301 aborts the engine rather than refusing.** 1.0715e301 loads and 1.0716e301 does not: the fixed-point conversion scales by 2^24, so the wall is where that multiplication overflows a double to infinity. What the player sees is Factorio's crash dialog and a stack trace, not a message naming the mod. That is the one rule here worth checking in your own code, because it is the only one whose failure is not a readable refusal.

**Research units.** A research unit takes tool items only: an ordinary item is `Invalid research unit (iron-plate). Research unit(s) can only be tool type items at the moment.`. A fluid name fails earlier and differently, `Error in assignID: item with name 'water' does not exist.`, because the name is resolved against items and no fluid exists there. A unit with an empty ingredient list loads; whether such a research completes in a running game is a question a headless dump cannot answer.

**Settings, from the two sides that surprise people.** Setting prototypes are not in `data.raw` at the data stage: `data.raw["string-setting"]` is nil and indexing into it is a Lua error, so anything that needs to know which settings your mod declared has to carry the declarations itself. And a stored value is not the value you declared: the engine resets some of what a player's `mod-settings.dat` carries before any stage runs and passes the rest through untrimmed. The table under "Running a stage on a value a player chose" below lists what it does with each shape. The one thing to add here is that only a value of the wrong type says so, by logging a line and carrying on, while a number outside the declared range and a dropdown value outside `allowed_values` are reset in silence, so a stage cannot tell a reset value from a value a player chose.

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

An over-long jump is relayed rather than refused, so a large data guest usually packages without saying anything, and the number worth watching is not the jump. It is **block room**: how much of one relay hop is left over the longest straight-line run that sits inside a single `if` body or loop body. A station in the relay's ladder is a label, and a label only goes at the function's own body level, so a nested run longer than one hop is what the relay cannot break up. Packaging prints the distance under the data module's own line and `--report` carries it as `jumps.data.block_room_bytes`, measured over every function in the module, with `jumps.data.widest_block_function` naming the one it is in.

Two things follow for a data guest that is getting large. A long straight-line run of `Extend` calls at the top level of a section function is free at any size, because it has a statement boundary every few bytes. The same run wrapped in one loop or one `if` is what eventually stops packaging, so keep the branch or the loop around the small part rather than around the section.

## Strings are bytes

A Lua string is an arbitrary byte sequence, and so is every string that crosses this library: a stored setting value, a prototype field another mod wrote, a path element. **They cross byte-exact in both directions and the library never rewrites one.** That matters because the engine carries arbitrary bytes through: measured on Factorio 2.0.77, a startup setting whose stored value holds invalid UTF-8 or control characters arrives at the data stage unchanged.

In Go there is nothing to opt into, because a Go `string` is already a byte string: `V.String()` gives you the engine's bytes and `len` gives you the engine's length. The obligation is the other way round. A string out of `V.String()`, `Keys`, `Mods` or `ModName` is not necessarily valid UTF-8, so check with `utf8.ValidString` before treating one as text, and carry it as-is when you are only passing it through.

In Rust a `String` cannot hold those bytes, so `V::Str` holds a `LuaStr`, which owns a `Vec<u8>`. Two accessors split the question, and the types make you pick:

```rust
let v = fkdata::startup_setting("my-mod-label").unwrap_or_default();

// Carrying it: the bytes, which never fails.
fkdata::set(&fkdata::bytes_(v.bytes()), &[P::S("item"), P::S("my-item"), P::S("label")]);

// Reading it as text: Some only when the bytes are UTF-8.
match v.string() {
    Some(text) => fkdata::log(text),
    None => fkdata::raise("the label setting is not valid text"),
}
```

`str_("item")` builds a string value from text and `bytes_(b"\xff\x00")` builds one from bytes; `LuaStr` also offers `as_str()` for the checked conversion and `to_string_lossy()` when a replacement character is genuinely what you want, such as in a log line. `Keys` returns `Vec<LuaStr>` for the same reason: a prototype name is a key in a namespace any mod can write. A path element is a string crossing the other way and takes bytes too: `P::B(name.as_bytes())`, or `(&name).into()`, spells a name `keys` handed back exactly as the engine wrote it, so a prototype whose name is not UTF-8 can be reached as well as listed. In Go the path takes the string as it is.

The exceptions are the conveniences over names the engine itself constrains, which stay `String` in Rust: `mods`, `mod_version`, `mod_name`, `feature_flags`, `derived_types` and `base_type`. If one of those is ever not valid UTF-8 the library stops the load, naming the surface and printing the bytes in hex, rather than rewriting it.

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

### Running a stage on a value a player chose

A dump taken as above runs both stages on the values your guest declares, which is the load a player who never opens the settings screen gets. The other half needs a `mod-settings.dat`, the file that carries a player's chosen values into the settings and data stages. The engine writes that file and reads it back at every load, and nothing else produces it: `fklua modsettings` is the writer and the reader.

```sh
cat > chosen.json <<'JSON'
{
  "comment": "read and ignored, because JSON has no comment syntax",
  "version": [2, 0, 77, 0],
  "startup": {
    "my-mod-hard-mode": true,
    "my-mod-belt-speed": 20.0,
    "my-mod-stack-size": 40,
    "my-mod-recipe": "1 steel-plate, 10 water"
  },
  "runtime-global": {},
  "runtime-per-user": {}
}
JSON

fklua modsettings write --from chosen.json --out "$MODS/mod-settings.dat"
factorio -c "$USERDIR/config/config.ini" --mod-directory "$MODS" --dump-data
fklua modsettings read "$MODS/mod-settings.dat"
```

The file goes in the mod directory beside the mod folders, not inside one. A section you leave out is written empty, which is what the engine's own file carries for a stage nothing has set; setting names are the full prototype names your guest declared; and the order settings appear in is the order they are written in.

**The type of a number is its spelling.** `40` is the signed 64-bit type, and `40.0` and `1e3` are the double, because an int setting and a double setting are different prototypes. The engine reads either encoding as the same number, so this is about the file saying which kind of setting a row is for.

**Read the file back after the run rather than assuming what ran.** The engine rewrites `mod-settings.dat` when the load finishes, with the values it settled on, and it does not keep everything. Measured on Factorio 2.0.77:

| what the file stores | what the engine does with it |
|---|---|
| a value of the wrong type for the setting | resets it to the default, and this is the one reset that logs a line |
| a number outside the setting's range | resets it to the default; it is not clamped |
| a dropdown value outside `allowed_values` | resets it to the default, before any stage runs |
| a string with leading or trailing spaces | keeps both space runs, `auto_trim` or not: trimming is a GUI behaviour |
| an empty string | resets it to the default |
| a value under a name no mod declares | ignores it, and leaves it in the file |

So the third command above is what says which values the stages actually saw, and it prints them in the same JSON shape `write` reads.

**Both halves refuse rather than guess.** `write` names the key it cannot encode, and reads back what it encoded before it writes anything, so a file the engine would reject is a sentence naming the byte instead of a failed load. It also refuses a document holding a byte that is not UTF-8, or a `\u` escape naming a lone surrogate half, before it reads anything else: JSON cannot carry either, and a JSON decoder puts U+FFFD in their place without a word, so the check is on the file's own bytes and names the offset. `read` names what it cannot describe in JSON and exits non-zero: a setting name or a stored value holding bytes that are not UTF-8 is refused rather than printed with each bad byte replaced, because that printout would write back a different file. A key named twice, at any level, is refused for the same reason: the later one would win in silence.

A guest library that generates settings can add the same check to its own in-game gate by writing its own JSON. There is no encoder to write and no second tool to build: the compiler already owns the format.

## Where to look next

`guest/go/examples/datastage` and `guest/rust/examples/datastage` are the worked examples, written as line-for-line mirrors: a setting, prototypes from a computed loop, a read-then-extend, a clone and its patches, a sorted enumeration, and an "is this already defined" check. [`docs/generated-files.md`](generated-files.md) lists every file `fklua mod` writes and which are yours to edit.
