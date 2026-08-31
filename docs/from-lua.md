# Coming from Lua

Where the habits of a Lua mod author land in a guest. Most of them land on the language you are already writing in, a few land on something FkLua provides, and two land on nothing at all, which is said here rather than discovered.

## The standard library

The ecosystem's shared library is a set of Lua modules that every mod copies into its own state. There is nothing to interoperate with: a mod's Lua state has its own copy of the source, exactly as a Go import has its own copy of a package. So the answer to "how do I use flib from Go" is a Go package, and most of what those modules do is already one.

| what you used it for | where it is now |
|---|---|
| migration: run something when the mod set changes | `fk_on_configuration_changed`, which is handed the real `ConfigurationChangedData`: which neighbour appeared, disappeared or moved version, and from what |
| a tick scheduler: run something once, after this burst | `fk.Defer()`, a one-shot `on_tick` the runtime registers and tears down again, so an idle guest pays nothing per tick |
| a tick scheduler: run something every N ticks | `fk.OnNthTick(n)` and the `fk_on_nth_tick` export. The engine schedules the period, so the guest is entered once per N rather than on every tick to decide it has nothing to do. Several periods at once; the one that fired is handed back |
| table helpers: deep copy, deep compare, filter, map | the language. Go and Rust both have them, and neither costs a boundary crossing |
| geometry: bounding boxes, positions, areas | the language, over the generated `MapPosition` and `BoundingBox` structs |
| event dispatch: several handlers for one event | the language. One exported `fk_on_event` dispatches on the id you were given |
| a GUI templating layer | the typed argument structs, plus `fklua docs` for what each element type accepts. See [factorio-api.md](factorio-api.md) |
| string formatting for log lines | `fklog`, which builds a line in one reused buffer. See [debugging.md](debugging.md) |
| position and direction constants | the generated `defines` accessors, resolved by name against the running game |

**The one residual is prototype fragments.** Helper modules that build a piece of a prototype and hand it back to be spliced in do not fit the data ABI's model: a data guest reads and patches `data.raw` through the host and clones a prototype without marshalling it, which is what keeps the untouched fields exactly as the source shipped them. A helper that returns a table for you to merge would have to marshal that table out and back. Building the fragment in the guest and emitting it as a prototype works; adopting somebody else's Lua fragment library does not. See [data-stage.md](data-stage.md).

## The `remote.interfaces` guard, which you no longer need

A Lua mod that talks to a neighbour guards every call, because `remote.call` into an interface that is not there RAISES and takes the handler down with it:

```lua
if remote.interfaces["some-mod"] and remote.interfaces["some-mod"]["get_thing"] then
  remote.call("some-mod", "get_thing")
end
```

**A guest cannot be taken down that way.** `RemoteCall` returns a status: a missing interface, a missing method on an interface that is there, and a method that raises are all `StatusCallFailed`, and the guest carries on. So the guard is the call:

```go
v, st := fkapi.RemoteCall("some-mod", "get_thing")
if st == fkapi.StatusOK {
    use(v)
}
```

That matters more than it looks, because reading `remote.interfaces` is expensive in a way the Lua version is not. It is a dictionary of dictionaries, so every check copies every interface name AND every method name in the save across the boundary into the guest heap, where a Lua mod was indexing a table it already had. One audited overhaul guards seventeen call sites; on a guest, seventeen of those reads cost more than the calls they were protecting.

Two cases the status does not cover, and what to use instead. **"Is that mod installed at all", asked without wanting to call anything**, is `script.active_mods` (`LuaBootstrap.ActiveMods`), which is an ordered name/version list and is the right question anyway. **Enumerating** what a neighbour offers really does want the whole table, and the materializing read is the right shape for it. `fk.LastError()` carries the engine's own sentence after a failed call, which tells the three failure kinds apart when a log line needs to say which; do not branch on it.

## Randomness that does not desync

`math.random` is per peer and is a desync waiting for a multiplayer game. Factorio's own answer is a random generator seeded from the map, saved with it, and replicated; it binds like anything else.

```go
rng, err := fkapi.Game.CreateRandomGenerator(nil) // nil seeds from the map
if err != nil {
    return
}
r := fkapi.LuaRandomGenerator{Object: rng}
lo, hi := int32(1), int32(10)
n, _ := r.Call(&lo, &hi) // 1..10, the same on every peer
```

Retain the handle if you want the same generator next tick, or create one per use and accept that it restarts from the map seed. What you must not do is reach for a modulo of something peer-local to avoid the question: a mod shipped that, on the belief that no synchronized source existed, and the correct answer was bound the whole time.

## Handles survive a save

Factorio's own advice is that a `LuaEntity` reference is not something to keep. In FkLua the split is explicit, and the persistent half is a normal thing to use rather than a last resort.

Everything the host returns is **transient**: it stops working when the event that produced it returns, which is what makes the dominant case free (take a handle, use it, drop it, nothing accumulates). `Retain()` promotes one into the **persistent** space, which lives in `storage` alongside the guest's memory and survives saves and multiplayer joins; `Release()` gives it back.

```go
e := fkapi.LuaEntity{Object: ev.Entity.Retain()}
// ...next tick, next session, after a reload:
if !e.Object.Valid() { return }  // a null handle; the object may still be gone
name, err := e.Name()            // ERR_INVALID if the entity has since died
```

A retained handle is not a promise that the object still exists. Ask, or make a call and read the status.

**Re-finding everything from the world on load is a valid design and it is not the required one.** The first mod built on FkLua rebuilds its whole registry by scanning surfaces at load, and that is an architectural choice it made for reasons of its own; later ports retain handles and keep them across saves. Newcomers read the exemplar and assume the scan is a rule. It is not.

## Errors do not unwind

`error()` in a Lua mod aborts the current handler and Factorio reports it. A guest has no equivalent: there are no coroutines, so an error crossing back into wasm could not unwind the frame it came from, and every host call answers with a status instead.

So the idiom is log and gate rather than raise:

```go
e, err := surf.CreateEntity(spec)
if err != nil {
    fk.Log("create_entity: " + err.Error() + ": " + fk.LastError())
    return // and leave the rest of this handler undone, deliberately
}
```

A Go `panic` in a guest is worse than useless: it links a large amount of formatting machinery into the module (one downstream mod measured 73 KB from a single `panic` in a package initializer) and there is nothing to catch it. Return a status and decide.

## Two entry points that are not reachable

Said out loud, because both are things a Lua author does and neither has a guest form.

**`migrations/*.lua` is not FkLua's state-migration mechanism.** A mod's own state is the guest's heap, and the heap is migrated by `fk_migrate`, which the runtime calls on a fresh heap when the build changed. A Lua migration runs before the heap has been adopted, so anything dispatched into a guest from one would run against a heap the adoption then overwrites. The file type stays available through the include tree as a hand-written escape hatch; JSON migrations are data rather than Lua and are unaffected.

**A cross-mod protocol whose extension point is Lua source cannot host a guest.** If another mod's contract is "give me a function" or "give me a chunk to `load()`", there is nothing a compiled guest can hand it. The remote interface seam works in both directions and is the sanctioned surface; if you are designing such a protocol, make the payload data.

## A simulation, and the one line of Lua it needs

A `SimulationDefinition` -- what a tips-and-tricks entry, a Factoriopedia entry or a main-menu scene runs -- carries its script as an `init` string that the engine executes as a **silent console command**. A console command has no `require`, so that string can never load a compiled module. That is a property of the entry point rather than a gap in the compiler, and it is not going to change.

What it can do is CALL into the mod, and `SimulationDefinition.mods` is the field that makes that possible: it is documented as an array of mods whose runtime scripts are loaded for the simulation. So a mod that lists itself there and registers a remote interface keeps the whole screenplay in the guest and leaves one line of Lua in the prototype:

```go
// the data guest
fkdata.KVs("factoriopedia_simulation", fkdata.Obj(
    fkdata.KVs("mods", fkdata.Arr(fkdata.Str("my-mod"))),
    fkdata.KVs("init", fkdata.Str(`remote.call("my-mod", "run_demo")`)),
    fkdata.KVs("length", fkdata.Num(600)),
))
```

with `run_demo` registered from the control guest's `init` the way any other remote method is. See [factorio-api.md](factorio-api.md).

**What is verified, at every hop.** The prototype loads: a `factoriopedia_simulation` carrying `mods` and `init` is accepted by a real engine and reaches the prototype dump verbatim. The init string is executable and reaches the seam: the exact text above, loaded as a bare chunk with no `require` available to it, calls through to the guest's handler. **And a real simulation runs it**: nothing headless runs a simulation at all -- there is no command-line flag for one and a headless log never mentions one -- so the last hop is field-verified rather than gated, 2026-08-30 on Factorio 2.1.17. A tips-and-tricks entry built to exactly this recipe played its simulation in the tips panel, the mod's runtime scripts were loaded into the simulation state, and the guest's handler staged the scene, with `seam reached ... in-simulation=true` in the client's own log where a console-driven control run of the same string reads `in-simulation=false`. The headless wall stays a wall; re-verifying is a data guest defining the tips entry, a control guest registering the method, and minutes in a client.

**And the engine does not check the string when the prototype loads**, measured: an `init` with an unbalanced parenthesis loads with a clean exit and is stored as written. So a mod that loads is no evidence that its simulation will run; test the init string on its own before shipping it.
