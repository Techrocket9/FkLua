# The Factorio API from a guest

How the generated bindings reach Factorio's runtime API, what a mod actually ships, and how the API version is pinned. The bindings themselves are generated files; [what `fklua gen-bindings` writes and how to regenerate it](generated-files.md) is its own page.

## Coverage

The whole runtime API is bound in both languages, member id for member id. Against the default **2.0.77** API pin: 4,257 of 4,259 members, 219 event payload structs (every event the description declares), 1,329 inherited forwarders (so `LuaEntity` has `LuaControl`'s `position` and `get_inventory`), 1,137 `defines` accessors, 11 class operators plus the write half of the two that have one, and 240 `<Name>Into(dst, ...)` variants that let an array return land in a buffer you already own. Two members are deferred, both a name that collides with another member of the same class. The counts are committed data in `api/<version>/census.json`, regenerated with the bindings and gated by `gen-bindings --check`; read them from there rather than from this page.

## One import, not thousands

Every method and attribute goes through one generic `fk.call(handle, member, argp, retp)` import rather than one import per member. A method Factorio removes in a point release would otherwise be an unresolved import, which fails the whole module at instantiation; here it degrades to one call returning `ERR_NO_MEMBER` while everything else keeps working.

A host call costs about 12.5 µs, cross-confirmed over 2,487 real calls in game. That is thousands of interpreted Lua instructions, so the cost model is calls, not bytes: batch at the boundary, not inside it.

## What a mod ships

A mod ships the members it calls, not the API. `fklua mod` scans the compiled guest for the constant ids reaching `fk.call` and `fk.subscribe` and prunes the packaged member and event tables: a one-member guest ships a 646-byte member table where the full one is about 840 KB. An id the scan cannot prove constant ships the whole table instead (a bigger mod, never a broken one), and the build output says so. Subscribing in a loop, or computing an event id, is the usual way to lose the pruning.

## Handles

Everything the host returns is a handle, and handles come in two spaces split at `0x40000000`. Transient handles are released when the event that produced them returns, which is why storing one across events is an error rather than a leak. `Retain()` promotes a handle into the persistent space, which lives in `storage` and survives saves; `Release()` gives one back when it is no longer needed. The `retain` example shows the round trip across a save.

Handles are why the bindings can offer host-side predicates such as `surface.NameIs("nauvis")`: the question is asked where the string already is, instead of copying it into guest memory to compare.

## Class operators

Some Factorio classes are used through Lua operators rather than named members: `inventory[1]`, `#inventory`, `chunkIterator()`. Those bind as `Get`, `Length` and `Call` (`get`, `length`, `call` in Rust), because an operator has no name for the ABI to resolve.

Reaching one on a `LuaCustomTable` takes two calls, and that is the cheap way round. An attribute such as `force.technologies` is a custom table, so reading it whole materializes every entry across the boundary; `TechnologiesRaw()` hands back the handle instead and `Get(key)` reads the one entry you wanted.

Two of those operators have a write half, `Set(key, value)`, because Factorio documents an assignment through them: a `LuaCustomTable` holding mod settings, and a `LuaFluidBox`. Writing a mod setting is the reason it exists, and it is the only way a mod changes its own runtime-global setting:

```go
raw, err := fkapi.Settings.GlobalRaw()
if err != nil {
    return
}
err = fkapi.LuaCustomTable{Object: raw}.Set(
    fkapi.OfString("my-setting"),
    fkapi.OfMap(fkapi.KeyValue{Key: fkapi.OfString("value"), Val: fkapi.OfBool(true)}),
)
```

The write replaces the whole `ModSetting` table, which is why the value is a map with a `value` key. Factorio accepts it on `settings.global`, `settings.player_default`, `player.mod_settings`, `settings.get_player_settings(player)` and `style.column_alignments`, and refuses it everywhere else: any other custom table answers `ERR_CALL_FAILED` carrying the engine's own "LuaCustomTable is read only", and a key that is not a defined setting answers it with "doesn't contain key". A mod can only change its own settings. The change is per save rather than per installation, it does not reach `mod-settings.dat`, and it raises `on_runtime_mod_setting_changed` before the call returns, so a handler for that event runs inside the write. Factorio refuses the write during `on_init`.

Writing an absent value to a `LuaFluidBox` clears it, which is Factorio's own behaviour for `fluidbox[n] = nil`. New fluid boxes cannot be added or removed this way and the index must be in bounds.

## Events

Subscriptions are made from `_initialize` (Go: `func init()`; Rust: `_initialize`), the one place they may go: `control.lua` runs it on every load, while `script.on_init` fires only when a save is created, so a subscription made in `fk_on_init` vanishes on the first reload with no error.

- **Filters run in C++ before your handler.** A filtered subscription carries Factorio's own filter list, so an `on_entity_died` for a biter never enters the guest. `NameFilter` and `TypeFilter` build the common cases; any filter shape the engine documents can be passed as a map.
- **Payloads are generated structs.** `Read<Event>(ptr)` decodes the payload; there is no reading fields at hand-derived offsets.
- **An expensive field can be declined.** Some events carry unbounded containers; a subscription can mask a field it does not read, and a masked field decodes as absent or empty. Measured on `on_undo_applied` with 200 actions: 7.49 ms to 2.7 µs per dispatch. Only optional and container fields are maskable, so a masked field can never be mistaken for a real zero.

## Commands and remote interfaces

A wasm guest has no callable Lua value, so callbacks work by id: the host synthesises the closure, hands it to Factorio, and dispatches back in through `fk_on_call` with an id the guest chose. Console commands, remote interfaces in both directions, and `remote.call` out of the guest all work this way. Registrations are made from `_initialize` for the same reason subscriptions are: Factorio does not save them, and `control.lua` re-runs on every load.

## defines

`defines.*` values are resolved by name at load time, because the API description carries define names and not their numbers, which are per Factorio build. The generated accessors (`DefinesDirectionEast()` and friends) ask once and cache; hardcoding the number is the bug the accessors exist to prevent.

## The two version axes

The **API pin** is the `runtime-api.json` version the bindings and the packaged member table come from; the **engine** is the Factorio actually running, which a guest can ask about with `helpers.game_version`. They meet in exactly one place: the packaged `info.json`'s `factorio_version`, which defaults to the pin's `major.minor` (a 2.0 engine does not load a mod declaring 2.1, and a 2.1 engine does not load one declaring 2.0) and can be overridden with `[mod] factorio_version` in `fklua.toml` or `--factorio-version`.

The default pin is the general-availability release, **2.0.77**, because a default is what a mod author who has pinned nothing ships to players, and players are on stable. Everything in the FkLua repository builds and runs against a stock 2.0.x install; the in-game test scripts read the installed engine's version and package for it. Two capabilities need more:

- **2.1.x API surface** is one line away: `api = "2.1.14"` in `fklua.toml` (or `--api=2.1.14`), then `fklua gen-bindings && fklua lock`. Every supported description is committed, so changing the pin needs neither the game nor the network; at 2.1.14 the bindings cover 4,841 of 4,843 members with 224 events. On Steam, 2.1.x is the `2.1.14` entry under the game's Betas tab; the in-game test scripts pick the engine up through `FACTORIO_BIN` or the default Steam path.
- **FkIPC** requires a 2.1.14 or newer engine and is inert below it; see [its README](../guest/go/fkipc/README.md).

## A new Factorio version

A new version is a data drop, not a porting job. `fklua api pull <version>` fetches and commits its description, `api list` shows what is cached, `api diff` classifies what moved between two versions, and `api check GUEST.wasm --to <version>` says whether anything your compiled mod actually calls changed. Adding the 2.1.12 description (482 new members) needed no generator change. The migration checklists are in [`agents/versioning.md`](../agents/versioning.md).
