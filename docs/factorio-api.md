# The Factorio API from a guest

How the generated bindings reach Factorio's runtime API, what a mod actually ships, and how the API version is pinned. The bindings themselves are generated files; [what `fklua gen-bindings` writes and how to regenerate it](generated-files.md) is its own page.

## Coverage

The whole runtime API is bound in both languages, member id for member id. Against the default **2.0.77** API pin: 4,257 of 4,262 members, 219 event payload structs (every event the description declares), 1,331 inherited forwarders (so `LuaEntity` has `LuaControl`'s `position` and `get_inventory`), 1,137 `defines` accessors, 11 class operators plus the write half of the two that have one, the three global functions, and 240 `<Name>Into(dst, ...)` variants that let an array return land in a buffer you already own. Five members are deferred, all of them taking a Lua function as an argument, which a compiled guest has no way to supply. The counts are committed data in `api/<version>/census.json`, regenerated with the bindings and gated by `gen-bindings --check`; read them from there rather than from this page.

## One import, not thousands

Every method and attribute goes through one generic `fk.call(handle, member, argp, retp)` import rather than one import per member. A method Factorio removes in a point release would otherwise be an unresolved import, which fails the whole module at instantiation; here it degrades to one call returning `ERR_NO_MEMBER` while everything else keeps working.

A host call costs about 12.5 µs, cross-confirmed over 2,487 real calls in game. That is thousands of interpreted Lua instructions, so the cost model is calls, not bytes: batch at the boundary, not inside it.

## What a mod ships

A mod ships the members it calls, not the API. `fklua mod` scans the compiled guest for the constant ids reaching `fk.call` and `fk.subscribe` and prunes the packaged member and event tables: a one-member guest ships a 646-byte member table where the full one is about 840 KB. An id the scan cannot prove constant ships the whole table instead (a bigger mod, never a broken one), and the build output says so. Subscribing in a loop, or computing an event id, is the usual way to lose the pruning.

## Handles

Everything the host returns is a handle, and handles come in two spaces split at `0x40000000`. Transient handles are released when the event that produced them returns, which is why storing one across events is an error rather than a leak. `Retain()` promotes a handle into the persistent space, which lives in `storage` and survives saves; `Release()` gives one back when it is no longer needed. The `retain` example shows the round trip across a save.

Handles are why the bindings can offer host-side predicates such as `surface.NameIs("nauvis")`: the question is asked where the string already is, instead of copying it into guest memory to compare.

## Reading one attribute off many entities

A poll pays one host call per entity, and a host call is the unit this ABI's cost model is denominated in. `fk.bulk_get` pays the crossing once: a readable attribute whose value is fixed-width (a number, a boolean, a handle) gets a second binding that reads it off a whole slice of handles.

```go
ents, err := surface.FindEntitiesFiltered(filter)
if err != nil {
    return
}
surfaces := make([]uint32, len(ents))
n, err := fkapi.LuaControlSurfaceIndexBulk(ents, surfaces)
```

The `[]Object` a search returns is already the array the host reads, so nothing is copied and nothing is marshalled in between. The destination is one copy of that attribute's ordinary return block per element, which is why the element type is the plain Go value the single getter returns.

It is a package-level function rather than a method, because there is no receiver: the receivers are the slice. It is named after the class that DECLARES the attribute, which is where the generated reference lists it: `surface_index` is `LuaControl`'s, so a slice of entities reads it as `LuaControlSurfaceIndexBulk`.

An attribute the description marks optional has a presence byte per element, so its destination is a small generated struct instead:

```go
temps := make([]fkapi.BulkOptFloat64, len(ents))
n, err := fkapi.LuaEntityTemperatureBulk(ents, temps)
for i := 0; i < n; i++ {
    if temps[i].Has {
        use(temps[i].V)
    }
}
```

**What the returned count means.** An element whose handle is dead, whose object is no longer valid, or whose read raised is written as the zero value and is not counted, and the walk continues to the next element. A search result going stale between the search and the read is the ordinary case for a poll, not an error. A count below `len(objs)` says something was missed; for an optional attribute the presence byte says which, and for a mandatory one a zero that was skipped and a zero that was read are the same bytes.

`dst` must be at least `len(objs)` long or the call is refused with `StatusBadArgs` before anything is written. Passing an empty slice of handles reads nothing and returns 0.

**What it costs.** Measured through a compiled guest on Factorio's Lua 5.2.1, against the same attribute read one handle at a time, with `--persist=table`: 0.31x per element at four handles and 0.25x at 256. Under `--persist=packed` the difference is larger, because a per-call return block dirties a fresh page and a bulk destination is contiguous: 0.008x per element at 256 handles. There is no size at which the bulk form is worse, so there is no threshold to remember.

Strings and containers have no bulk form. A string element is a pointer and a length into a fixed scratch region, so a thousand of them would fall through to the guest allocator once per element; a container element would be a nested pointer and count, which would stop the destination being a flat array. `NameIs` already answers the dominant string question host-side.

## Class operators

Some Factorio classes are used through Lua operators rather than named members: `inventory[1]`, `#inventory`, `chunkIterator()`. Those bind as `Get`, `Length` and `Call` (`get`, `length`, `call` in Rust), because an operator has no name for the ABI to resolve.

Reaching one on a `LuaCustomTable` takes two calls, and that is the cheap way round. An attribute such as `force.technologies` is a custom table, so reading it whole materializes every entry across the boundary; `TechnologiesRaw()` hands back the handle instead and `Get(key)` reads the one entry you wanted.

**A method that returns a custom table has the same twin.** The filtered prototype getters and `get_player_settings` return one, so `GetEntityFilteredRaw(filters)` (Rust: `get_entity_filtered_raw`) hands back the handle and the lookup is a second call, where the plain form copies every matching prototype into the guest first. Both forms stay: the materializing read is the right answer for iterating the whole result, and the handle is the right answer for a point lookup.

```go
raw, err := fkapi.Prototypes.GetEntityFilteredRaw(filters)
if err != nil {
    return
}
proto, err := fkapi.LuaCustomTable{Object: raw}.Get(fkapi.OfString("iron-chest"))
```

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

## Tier-2 values, and reading one back

Where the API's type is a union, a `LocalisedString`, or anything else with no fixed layout, the bindings use one self-describing type: `Value` in Go, `Value` in Rust. Seven constructors build one (`OfString`, `OfNumber`, `OfBool`, `OfObject`, `OfArray`, `OfMap`, `OfNil`; `Value::Str` and its siblings in Rust), and a matching set of accessors reads one back.

There are two families and the split is what lets one of them chain. A lookup answers with a value whose miss is nil, so a chain over a shape that may not be there is one expression:

```go
count := v.Get("filters").At(0).Get("count").NumOr(0)
```

A read answers with a comma-ok in Go and an `Option` in Rust, so a caller who needs to know whether the value was there says so:

```go
name, ok := v.Get("name").AsStr()
```

| what | Go | Rust |
|---|---|---|
| look up, chains | `Get(key)`, `GetKey(value)`, `At(i)` | `get(key)`, `get_key(value)`, `at(i)` |
| read, may fail | `AsBool`, `AsNum`, `AsStr`, `AsObj` | `as_bool`, `as_num`, `as_str`, `as_obj` |
| read with a default | `BoolOr`, `NumOr`, `StrOr`, `ObjOr` | `bool_or`, `num_or`, `str_or`, `obj_or` |
| the rest | `Has(key)`, `Len()`, `IsNil()` | `has(key)`, `len()`, `is_empty()`, `is_nil()` |

Nothing coerces. `AsNum` on a string answers not-ok rather than parsing it, because the tag is what the host said the value is. `Len` is the one exception and it answers 0 for a scalar. `Get` cannot tell a missing key from a key that is present and nil; `Has` is what answers that, and it matters at a small fraction of call sites because Factorio's own option tables read the two the same way.

`At` is zero-based. The one-based Lua index the host read the array out of does not come with it.

## Option tables, and typed arguments

Most methods that take an option table get a generated struct: `surface.CreateSurface(name, &fkapi.MapGenSettings{...})`. A handful do not, because their table is a discriminated union: the fields it accepts depend on the value of one of them. `LuaGuiElement::add` is the largest, with 21 variant groups at the default pin.

Those methods take one `Value`, and they also have a second form whose shared parameters are a typed struct:

```go
// The tier-2 form. Every key is a string you got right or did not.
btn, err := screen.Add(fkapi.OfMap(
    fkapi.KeyValue{Key: fkapi.OfString("type"), Val: fkapi.OfString("button")},
    fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("launch")},
))

// The typed form. Same member, same result, and the compiler knows the fields.
btn, err := screen.AddTyped(fkapi.LuaGuiElementAddArgs{
    Type: "button",
    Name: str("launch"),
}, nil)
```

The second argument is the variant tail: the keys that belong to one variant group and therefore have no field. Pass `nil` when there are none.

```go
icon, err := screen.AddTyped(fkapi.LuaGuiElementAddArgs{
    Type: "sprite-button",
    Name: str("icon"),
}, val(fkapi.OfMap(
    fkapi.KeyValue{Key: fkapi.OfString("sprite"), Val: fkapi.OfString("item/iron-plate")},
)))
```

The tail is applied over the struct, so it can also override a field the struct set. That is deliberate: a shared parameter and a variant-group parameter are allowed to share a name (`LuaSurface::create_entity` has one), and a tail that could not override would not be an escape hatch.

The typed form is also cheaper on the host, because a flat block is read at known offsets where a tier-2 map is a walk over tagged key and value pairs. Measured host-side on one GUI element built both ways, on Lua 5.2.1: 0.74x for a row carrying two `LocalisedString` fields, and 0.40x for one whose five fields all have a fixed layout. The spread is the reason: a shared parameter whose type is itself a union stays a tier-2 slot inside the block, so it costs what it cost before.

Which shared parameters a method has, and what each variant group accepts, are in the generated reference (`fklua docs`), with the types spelled the way Factorio spells them.

## Building a GUI

A GUI is built by adding elements to `player.gui.screen` (or `.top`, `.left`, `.center`, `.relative`, `.goal`), reading events back through the `on_gui_*` family, and finding elements again by name.

```go
package main

import (
    "github.com/Techrocket9/fklua/guest/go/fk"
    "github.com/Techrocket9/fklua/guest/go/fkapi"
)

func str(s string) *string           { return &s }
func val(v fkapi.Value) *fkapi.Value { return &v }

func init() {
    fkapi.Subscribe(fkapi.EventOnGuiClick)
}

// openWindow builds a titled frame with one button in it.
func openWindow(playerIndex uint32) {
    p, err := fkapi.Game.GetPlayer(fkapi.OfNumber(float64(playerIndex)))
    if err != nil || p == nil {
        return
    }
    gui, err := fkapi.LuaPlayer{Object: *p}.Gui()
    if err != nil {
        return
    }
    screen, err := fkapi.LuaGui{Object: gui}.Screen()
    if err != nil {
        return
    }
    root := fkapi.LuaGuiElement{Object: screen}

    frame, err := root.AddTyped(fkapi.LuaGuiElementAddArgs{
        Type:    "frame",
        Name:    str("my-window"),
        Caption: val(fkapi.OfString("Status")),
    }, nil)
    if err != nil {
        fk.Log("gui: " + err.Error())
        return
    }
    _, err = fkapi.LuaGuiElement{Object: frame}.AddTyped(fkapi.LuaGuiElementAddArgs{
        Type:    "button",
        Name:    str("my-window-close"),
        Caption: val(fkapi.OfString("Close")),
        Style:   str("red_back_button"),
    }, nil)
    if err != nil {
        fk.Log("gui: " + err.Error())
    }
}

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
    if id != fkapi.EventOnGuiClick {
        return
    }
    ev := fkapi.ReadOnGuiClick(ptr)
    name, err := fkapi.LuaGuiElement{Object: ev.Element}.Name()
    if err != nil || name != "my-window-close" {
        return
    }
    // The frame is found again by name under the same parent it was added to.
    // Nothing is stored between events: a handle from one event is not valid in
    // the next unless it was retained.
    p, err := fkapi.Game.GetPlayer(fkapi.OfNumber(float64(ev.PlayerIndex)))
    if err != nil || p == nil {
        return
    }
    gui, err := fkapi.LuaPlayer{Object: *p}.Gui()
    if err != nil {
        return
    }
    screen, err := fkapi.LuaGui{Object: gui}.Screen()
    if err != nil {
        return
    }
    // The index operator, `screen["my-window"]`. It answers an absent element
    // with no object rather than with an error.
    w, err := fkapi.LuaGuiElement{Object: screen}.Get(fkapi.OfString("my-window"))
    if err != nil {
        return
    }
    if w != nil {
        _ = fkapi.LuaGuiElement{Object: *w}.Destroy()
    }
}

func main() {}
```

Three things about that are worth stating rather than inferring.

Element count equals call count. `add` takes no children, so a window of N elements is N host calls at about 12.5 µs each. A 50-row table rebuilt every tick is not affordable; rebuild on change, and update the elements you have rather than destroying the window.

Handles do not survive the event. `ev.Element` is valid inside the handler that received it and not afterwards. `Retain()` promotes one if a window really has to be remembered across events, and `Release()` gives it back; finding the element again by name costs a call and no bookkeeping.

Restyling at runtime goes through the `style` attribute, which takes a style prototype's name as a tier-2 string:

```go
err := fkapi.LuaGuiElement{Object: btn}.SetStyle(fkapi.OfString("green_button"))
```

## Global functions

Three of Factorio's functions belong to no class: `log`, `localised_print` and `table_size`. They bind as package-level functions, `Log`, `LocalisedPrint` and `TableSize` in Go and `log`, `localised_print` and `table_size` in Rust, and each takes one tier-2 value.

`log` is the one worth knowing about, because it is the only way to read a profiler. `LuaProfiler` has no member that returns its duration; the engine renders one only when the profiler is an element of a localised string, and the rendered line lands in the game log:

```go
p, err := fkapi.Helpers.CreateProfiler(nil)
if err != nil {
    return
}
work()
if err := (fkapi.LuaProfiler{Object: p}).Stop(); err != nil {
    return
}
err = fkapi.Log(fkapi.OfArray(
    fkapi.OfString(""),
    fkapi.OfString("[mymod] rebuild "),
    fkapi.OfObject(p),
))
```

The empty first element is a localised string's concatenate-the-rest form, and what appears in `factorio-current.log` is the message followed by the engine's own `Duration: 12.368959ms`. For a plain string with no localisation and no object in it, `fk.Log` is one import and no host call.

A duration is a wall clock, so it differs on every machine that runs the same tick. Logging one is safe, because the log file is not part of the game's lockstep state. Letting one influence what the mod does next is not: two clients timing the same work read different numbers, and a decision made from either desyncs a multiplayer game. Time things, log the timings, and never branch on them.

`localised_print` writes to standard output rather than to the log file, for a tool that launched the game as a child process. `table_size` counts the keys of a plain Lua table; it does not work on a `LuaCustomTable`, whose `Length()` operator answers that question without the table crossing at all.

## When a call fails

A host call returns a status and never raises into the guest, so a binding that fails hands back an error describing the kind of failure and not the engine's own words. `fk.LastError()` (`fk::last_error()` in Rust) is those words:

```go
if _, err := something(); err != nil {
    fk.Log("refused: " + fk.LastError())
}
```

It describes the call that just returned. The slot is cleared as each host call begins, so it is empty after a call that succeeded rather than carrying an earlier failure, and it should be read where the error is still in hand. The Rust form returns bytes rather than a string, because a Lua string is an arbitrary byte sequence and the engine promises nothing about encoding.

Log it, do not branch on it. The wording is an engine implementation detail a point release may change, and a mod that behaved differently because of a wording would behave differently on two versions of Factorio. A test asserting the exact text is the honest exception: an engine that stops refusing something should fail a suite rather than quietly widen it.

## Events

Subscriptions are made from `_initialize` (Go: `func init()`; Rust: `_initialize`), the one place they may go: `control.lua` runs it on every load, while `script.on_init` fires only when a save is created, so a subscription made in `fk_on_init` vanishes on the first reload with no error.

- **Filters run in C++ before your handler.** A filtered subscription carries Factorio's own filter list, so an `on_entity_died` for a biter never enters the guest. `NameFilter` and `TypeFilter` build the common cases; any filter shape the engine documents can be passed as a map.
- **Payloads are generated structs.** `Read<Event>(ptr)` decodes the payload; there is no reading fields at hand-derived offsets.
- **An expensive field can be declined.** Some events carry unbounded containers; a subscription can mask a field it does not read, and a masked field decodes as absent or empty. Measured on `on_undo_applied` with 200 actions: 7.49 ms to 2.7 µs per dispatch. Only optional and container fields are maskable, so a masked field can never be mistaken for a real zero.
- **A keybind is subscribed to BY NAME.** A custom input is delivered to the name of the `custom-input` prototype that declares it, and has no `defines.events` entry at all, so the numeric form cannot reach it. Use `SubscribeNamed(EventCustomInputEvent, "my-mod-hotkey")` in Go or `subscribe_named(EVENT_CUSTOMINPUTEVENT, "my-mod-hotkey")` in Rust, with `SubscribeNamedMasked` / `subscribe_named_masked` when the payload's optional fields are not wanted. Several custom inputs share one handler because they all carry the same payload; read `input_name` to tell them apart. A name no loaded prototype has is refused by the engine and comes back as a status with the engine's own message in the log, so a typo costs the keybind and not the mod.

## When the mod set changes

`fk_on_configuration_changed` fires whenever Factorio raises `on_configuration_changed`: a neighbouring mod added, removed or moved version, a startup setting changed, prototype migrations applied, or the game version moved. It is handed a pointer to a `ConfigurationChangedData`, which `ReadConfigurationChangedData` (Go) or `read_configuration_changed_data` (Rust) decodes.

`mod_changes` is the field most guests want: one entry per mod that moved, keyed by mod name, with `old_version` absent for a mod that was just added and `new_version` absent for one that was just removed. The payload also carries `mod_startup_settings_changed`, `migration_applied`, the map's own `old_version` and `new_version`, and `migrations`, a dictionary of prototype renames per prototype type.

Exporting the hook with no parameter still works and still means what it meant: the extra argument is discarded. The payload's layout is packaged only for a mod that exports the hook, so a mod that does not pays nothing for it.

This hook runs on the peer that loaded the save, before the first tick, so its effects are already in the state a joining client downloads. A guest may write its own state here.

## Commands, remote interfaces, and another mod's events

A wasm guest has no callable Lua value, so callbacks work by id: the host synthesises the closure, hands it to Factorio, and dispatches back in through `fk_on_call` with an id the guest chose. Console commands, remote interfaces in both directions, `remote.call` out of the guest, and subscriptions to events another mod defined all work this way. Registrations are made from `_initialize` for the same reason subscriptions are: Factorio does not save them, and `control.lua` re-runs on every load.

**An event another mod defined is subscribed to through the same seam, not through `Subscribe`.** A publisher mints an id with `script.generate_event_name()` and hands it out through its own remote interface, so the id is a number the consumer learns at runtime rather than a constant in the generated event table; and the payload is that mod's own table, which no API description carries, so there is no struct to decode it into. Both facts point at the same answer: ask for the id, register it against a dispatch id of your own, and read the payload as one tier-2 value.

```go
const evDelivery = 7 // this guest's dispatch id, not the event's

func init() {
    v, st := fkapi.RemoteCall("logistic-train-network", "on_delivery_completed")
    if st == fkapi.StatusOK {
        if n, ok := v.AsNum(); ok {
            fkapi.RegisterModEvent(evDelivery, uint32(n))
        }
    }
}
```

The payload arrives at `fk_on_call` as the single element of `argp`'s array, exactly as a command's `CustomCommandData` does. `RegisterModEventNamed` (Rust: `register_mod_event_named`) is the other spelling, for an event declared as a `custom-event` prototype at the data stage rather than minted at runtime. A publisher that is not installed shows up as a failed `RemoteCall`, and a prototype name this game does not have is refused by the engine with its own message in the log, so neither costs more than the subscription.

## defines

`defines.*` values are resolved by name at load time, because the API description carries define names and not their numbers, which are per Factorio build. The generated accessors (`DefinesDirectionEast()` and friends) ask once and cache; hardcoding the number is the bug the accessors exist to prevent.

## The two version axes

The **API pin** is the `runtime-api.json` version the bindings and the packaged member table come from; the **engine** is the Factorio actually running, which a guest can ask about with `helpers.game_version`. They meet in exactly one place: the packaged `info.json`'s `factorio_version`, which defaults to the pin's `major.minor` (a 2.0 engine does not load a mod declaring 2.1, and a 2.1 engine does not load one declaring 2.0) and can be overridden with `[mod] factorio_version` in `fklua.toml` or `--factorio-version`.

The default pin is the general-availability release, **2.0.77**, because a default is what a mod author who has pinned nothing ships to players, and players are on stable. Everything in the FkLua repository builds and runs against a stock 2.0.x install; the in-game test scripts read the installed engine's version and package for it. Two capabilities need more:

- **2.1.x API surface** is one line away: `api = "2.1.17"` in `fklua.toml` (or `--api=2.1.17`), then `fklua gen-bindings && fklua lock`. Every supported description is committed, so changing the pin needs neither the game nor the network; at 2.1.17 the bindings cover 4,857 of 4,859 members with 225 events. On Steam, 2.1.x is the `2.1.17` entry under the game's Betas tab; the in-game test scripts pick the engine up through `FACTORIO_BIN` or the default Steam path.
- **FkIPC** requires a 2.1.14 or newer engine and is inert below it; see [its README](../guest/go/fkipc/README.md).

## A new Factorio version

A new version is a data drop, not a porting job. `fklua api pull <version>` fetches and commits its description, `api list` shows what is cached, `api diff` classifies what moved between two versions, and `api check GUEST.wasm --to <version>` says whether anything your compiled mod actually calls changed. Adding the 2.1.12 description (482 new members) needed no generator change. The migration checklists are in [`agents/versioning.md`](../agents/versioning.md).

### Checking one guest from a script

`api check` is built to be gated on, so it has three exit codes:

| Exit | Meaning |
|---:|---|
| 0 | nothing this guest uses breaks between the two versions |
| 1 | something does, or the scan could not see everything the guest reaches |
| 2 | the check could not be run: a bad flag, an unreadable module, a version this installation does not have |

Codes 0 and 1 are both successful runs. The third is what separates "your mod is fine" from "you typed the version wrong".

`--json` writes one verdict object to standard output instead of the report, so a build script does not have to read prose:

```sh
fklua api check dist/my-mod.wasm --from 2.0.77 --to 2.1.16 --json
```

```json
{
  "from": "2.0.77",
  "to": "2.1.16",
  "guest": "dist/my-mod.wasm",
  "verdict": "impacted",
  "complete": true,
  "exit_code": 1,
  "surface": { "members": 12, "events": 3, "defines": 4, "concepts": 8 },
  "breaking_total": 241,
  "ignored": 240,
  "findings": [
    {
      "what": "LuaAssemblingMachineControlBehavior::include_fuel",
      "kind": "breaking",
      "match": "member",
      "detail": "attribute removed"
    }
  ]
}
```

`verdict` is `clean`, `impacted` or `unproven`, and it is the field to branch on. `unproven` means a member, event or define id was not a compile-time constant, so the scan could not see everything the guest reaches; it exits 1 because unproven is not a pass, and `complete` carries the same fact for a caller that wants both. `match` on a finding says why that change reaches this guest: `member` and `event` are things the guest calls or subscribes to, `define` is a `defines.*` value it reads, `concept` is a named type reachable from a signature it uses or from the payload of an event it subscribes to, `class` is a class-level change that takes every member on that class with it, and `schema` is the description format itself moving. `findings` is always an array, empty on a clean verdict.

The surface the check cross-references is everything a compiled guest touches: the members it calls, the events it subscribes to, the `defines.*` values it reads, and every named type reachable from those members' signatures and those events' payloads. All of it is recovered from the module itself, so nothing has to be declared.

Payload types count for the same reason signature types do. An event's data block is laid out from its fields, so a concept one of them names gaining a field moves every offset after it in the reader your guest was compiled against, while nothing is reported against the event's own name. The signature half does not cover it either: in the 2.0.77 description, five concepts are named by event payloads and by no member signature anywhere, `OldTileAndPosition` among them.

`api diff` classifies `defines` at two levels, and `api check` reads both. A whole group going away is reported as `defines.<group>`; an individual value going away is reported as `defines.<group>.<value>`, including values nested under a subkey. The value level is the one a guest depends on, because a generated define accessor resolves one dotted value path by name at load: a value that a release removed reads back as `0` at runtime rather than failing, so a guest holding one has a wrong constant and no error. Values under `defines.events` are not diffed here, since events are compared as events.

**Pass `--from` explicitly.** It defaults to the FkLua binary's own pin, and that value moves between FkLua releases, so a script that omits it can silently start asking a different question. The document echoes the versions it resolved for exactly that reason.

Every field name in the document is stable. The report printed without `--json` is not: it is presentation and free to change wording, so parse the JSON rather than the report.
