# Guest languages

What a guest toolchain has to be told, what it emits, and what it silently does
wrong. **TinyGo and Rust are both supported**, to the same depth: bindings,
a mirrored examples corpus that has to agree byte for byte, and since the
Rust collector a `--gc=collected` that names no toolchain. C is still M8.
(This line said "TinyGo is the only supported guest today; Rust and C are
M8" for several milestones after the Rust guest shipped, which is the drift
pattern CLAUDE.md records: a status sentence at the top of a file nobody
re-reads while editing the sections below it.)

Read `CLAUDE.md` first — the two invariants are the contract a guest's values
cross the host boundary under.

---

## TinyGo

### The build

```sh
cd guest/go
tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 -o hello.wasm ./examples/hello
fklua mod hello.wasm --name fk-hello --version 0.1.0 --author you --gc=collected
```

**`-gc=custom` is the recipe as of sharding stage C, and it used to read
`-gc=leaking`.** That is the `--gc=collected` default arriving in the one place a
reader copies from; see "What to do about it, in order" below for why the default
moved, and note that the flag has a second half — every example in this corpus
carries a `gc.go` whose only content is the `fkgc` import, because `-gc=custom`
without it does not LINK. **`-gc=leaking` (with `fklua mod` given no `--gc`) is
still a supported build and still the only one wasip1 can take**; it is the
expert arm now rather than the default one.

`internal/guest.BuildFlags` holds the leaking list and
`internal/guest.CollectedBuildFlags` the collected one, mirrored by
`guest/go/fk.BuildFlags` and `guest/go/fk.CollectedBuildFlags`; keep all four in
step (`internal/guest/buildflags_test.go` is what checks that they are).

**Every flag is load-bearing.** None is a preference:

| Flag | Why it is not optional |
|---|---|
| `-target=wasm-unknown` | The only target whose feature set FkLua compiles. `wasip1` enables bulk-memory, which is M10. `TestTinyGoEmitsNothingWeCannotCompile` checks that against TinyGo's own target JSON rather than against this table. |
| `-scheduler=none` | A Factorio tick cannot block. With a scheduler, any goroutine that parks becomes a busy spin inside the game loop, and the game hangs for every player. |
| `-gc=custom` **or** `-gc=leaking` | The one flag here that is a DECISION rather than a requirement, and the only reason both are listed. `custom` is the default and is what `fklua init` scaffolds: it hands TinyGo's custom-GC seam to `guest/go/fkgc`, whose pauses are BOUNDED by a budget the guest sets rather than being the stop-the-world pause a lockstep simulation cannot take. `leaking` is what this row used to say unconditionally, on the reasoning that any collector's pause desyncs everyone — true of an unpaced one, and what replaced it is that guest memory then only grows, so the pause moves into `memory.grow` where nothing can bound it (782 ms at one 16→32 MiB doubling, measured downstream). Whichever is chosen, **`fklua mod`'s `--gc` has to agree** — `--gc=collected` is refused for a module exporting no collector — and TinyGo's other `-gc` values are not supported at all. |
| `-opt=2` | TinyGo's default is **`-opt=z`**, which optimises for SIZE — the one cost this target does not have. Measured against `-opt=z` through the real compiler: `real_names` **0.577×**, `real_grid` 0.771×, `pure_sum` 0.770×, `pure_dot` 0.847×, `real_entities` 0.958×; `pure_prng` is ~2% slower and is the only kernel that does not gain. `-opt=0` is not a substitute — it fails to build under `-scheduler=none` (see below). |

### binaryen is a hard dependency

TinyGo shells out to `wasm-opt` for every wasm target and fails the build
without it:

```
error: could not find wasm-opt, set the WASMOPT environment variable to override
```

```sh
brew install binaryen
```

`guest.Available()` checks for it separately from TinyGo, because the message
above appears from deep inside a build and does not tell you what to install.

**`-opt=0` is not a way around it.** It fails earlier, and confusingly:

```
runtime/runtime_wasmentry.go:44:3: attempted to start a goroutine without a scheduler
```

That line is `go func() { initAll() }()` inside `if hasScheduler`, which is
`false` under `-scheduler=none` — dead code that only disappears once
optimization runs. At `-opt=0` it survives to the goroutine-lowering pass and
errors. `-opt=1` and up are fine; the default `-opt=z` is what we ship.

### What TinyGo actually emits

For `wasm-unknown`, TinyGo builds a **reactor**, not a command: buildmode is
`c-shared` and the linker gets `--no-entry`. That has one consequence that will
otherwise cost an afternoon:

> **`_initialize` must run before any `//go:wasmexport` function.** It sets up
> the heap and runs package initializers. Call an export first and TinyGo's own
> `wasmExportCheckRun` raises `//go:wasmexport function called before runtime
> initialization` — which arrives as an opaque trap, from a function you did not
> write, before your code has run at all.

`runtime/lua/fk_mod.lua` calls it at control.lua load time.

A module also carries things you did not write:

- **imports** in module `env`, one per `//go:wasmimport`
- **exports**: your `//go:wasmexport` names, `_initialize`, `memory`, and
  compiler-rt float helpers (`fminimum`, `fmaximumf`, …)
- **data segments** holding string literals and package-level initialised data
- **no start section** — initialisation is `_initialize`, not `(start)`

The float helpers are why `fklua compile` reports NaN diagnostics for
`fminimumf`/`fmaximumf` on a guest that never touches a float. They are library
code, not yours, and the warning is accurate about them.

`func main()` is required because the package is `package main`, and never runs.

### The feature string, as of TinyGo 0.41.1

```
+nontrapping-fptoint,+sign-ext,-bulk-memory,-multivalue,-reference-types
```

Do not trust this table — **`internal/guest` reads it from TinyGo's target JSON
on every test run** and fails when it moves outside `guest.Supported`. That
guard exists because "TinyGo emits nontrapping-fptoint unconditionally" sat in
prose for three milestones while `trunc_sat` went unimplemented. See
[`agents/testing.md`](testing.md).

### Sizes, measured

| guest | wasm | generated Lua | functions |
|---|---|---|---|
| one empty exported function | 29 KB | 48 KB (1,398 lines) | 7 |
| `examples/hello` — map, slice, `strconv`, u64, f64 | 105 KB | 162 KB (4,403 lines) | 33 |

At `-opt=2` (the default since M5) `examples/hello` is 3,703 lines, 16% smaller,
because the peephole collapses runs of stack operations into single expressions.
**Typed-slot promotion promotes nothing in it**: every shadow-stack frame TinyGo
emits has its frame pointer passed to a callee. See
[`agents/optimizer.md`](optimizer.md).

Roughly **1.5× the wasm size in Lua source**, on top of a fixed ~1,180-line
runtime prelude that every chunk carries. Chunk size is not a constraint worth
designing around: the day-0 probe measured Factorio parsing 4 MB of Lua in
106 ms.

The 29 KB floor is TinyGo's runtime, not FkLua's: a guest that does nothing
still ships a heap allocator and the compiler-rt helpers.

### Why `guest/go` is its own Go module

`//go:wasmimport` is rejected outside `GOARCH=wasm`. Inside the parent module it
would break `go build ./...` and `go vet ./...` for everyone, so
`guest/go/go.mod` keeps it out of the host build. Guests import
`github.com/Techrocket9/fklua/guest/go/fk`.

### Standard Go is not a guest and will not be

`GOOS=wasip1` makes `int` and every pointer 64-bit, so all address arithmetic
pays the (lo, hi) pair cost; modules start around 2 MB with a full GC and
scheduler; and `poll_oneoff` is unavoidable, so any `time.Sleep` becomes a busy
spin that hangs the game. TinyGo is the supported path. (M10 revisits TinyGo's
own `wasip1` target, which is a different thing.)

---

## The host boundary

wasm has four numeric types and nothing else, so **everything crosses as
numbers**:

- an **i32** is an unsigned double in [0, 2³²) — Invariant A applies to host
  functions exactly as it does to generated code
- an **i64** is a `(lo, hi)` pair of them, two Lua values in each direction
- a **string** is a `(pointer, length)` into the guest's linear memory, which
  the host follows with `instance.read_string`

The M4 ABI is two functions, and that is the whole of it:

| import | Lua side |
|---|---|
| `env.fk_log(ptr, len)` | `log(msg)` — works everywhere, including during load and in `on_load` |
| `env.fk_print(ptr, len)` | `game.print(msg)`, falling back to `log` when `game` does not exist yet |

Host → guest marshalling of anything but scalars is **not** available. The guest
would have to allocate a buffer for the host to write into, and that needs an
allocator interface, which is M7 along with the rest of the API surface. Until
then a guest receives scalars as call arguments — `fk_on_tick(tick)` — and
nothing more.

### Hooks control.lua registers

| export | wired to |
|---|---|
| `_initialize` | run once at load, before anything else |
| `fk_on_init` | `script.on_init` |
| `fk_on_tick(tick)` | `script.on_event(defines.events.on_tick)` |
| `fk_on_event(id, ptr)` | every `fk.subscribe(id)` the guest made |
| `fk_on_deferred()` | one shot on the tick after `fk.defer()`, then unregistered |
| `fk_after_load()` | one shot on the first tick after a save is LOADED, then unregistered. **PEER-LOCAL: it fires on a joining multiplayer client and on no other peer, so it must not write guest state** — see below |
| `fk_migrate(old_version)` | `script.on_configuration_changed`, only when the build id changed |
| `fk_on_configuration_changed()` | `script.on_configuration_changed`, **every** time Factorio raises it — the mod SET changed, a startup setting moved, the game version moved. Dispatched after `fk_migrate`. **REPLICATED, like `fk_migrate`: it may write guest state** — see below |
| `fk_state_version() -> i32` | not an event: read at save time, handed back to `fk_migrate` |

Registration is conditional on the export existing, because `on_tick` is not
free: registering it makes Factorio call the mod sixty times a second forever.

`fklua mod` prints what it wired and warns when a guest exports no event hook at
all — a mod that loads, initialises and is then never called again is almost
never what the author meant, and nothing else would say so.

**Adding a hook means editing two places**: `runtime/lua/fk_mod.lua` to register
it and `factorio.Hooks` to report it.
`TestEveryReportedHookIsActuallyRegistered` fails if only one of them changes.

### Subscribing, and event filters

```go
func init() {
    fkapi.Subscribe(fkapi.EventOnTick)
    fkapi.SubscribeFiltered(fkapi.EventOnPlayerMinedEntity,
        fkapi.NameFilter("my-machine")...)
}
```

Subscribe from an **init function**, not from `fk_on_init`: `_initialize` runs on
every load, `script.on_init` fires once when the save is *created*, so a
subscription made there vanishes the first time the save is reloaded — the API
calls keep working and the events silently stop arriving.

`SubscribeFiltered` passes Factorio's own filter list, which the engine applies
in **C++ before the handler runs**. Without it a guest that cares about one
prototype is entered for every build and mine event on the map and pays a
dispatch, a host call and a string crossing to read `entity.name` and reject it.
The list is a tier-2 dynamic value the guest builds, decoded once at subscribe
time; `NameFilter` and `TypeFilter` cover the two common cases and anything else
is an ordinary `OfMap(KeyValue{…})`.

**Reach for a CATEGORY filter before writing out names.** Factorio's filter
grammar has more than `name`, and the category terms are usually what a mod
actually means: `type`, `ghost_type`, and the entity-family predicates such as
`transport-belt-connectable`. The first downstream mod replaced **eleven** name
terms with **three** category ones — fewer terms to keep in step with the
prototypes it supports, and it stops being wrong the moment somebody adds a belt
tier it never heard of. `TypeFilter` builds the `type` one; the family
predicates carry no parameter of their own and are a one-pair map:

```go
fkapi.SubscribeFiltered(fkapi.EventOnBuiltEntity,
    fkapi.OfMap(fkapi.KeyValue{fkapi.OfString("filter"),
        fkapi.OfString("transport-belt-connectable")}))
```

**A term is a map whose `filter` key names the condition** and whose other keys
are that condition's parameters, plus the optional `mode` (`"or"` by default,
`"and"` binding tighter) and `invert`. Which conditions a given event accepts is
per event and is in the API description this repo already pins, as the
`Lua<Event>EventFilter` concepts — read them out of
`api/<version>/runtime-api.json`, or online at
`lua-api.factorio.com/<version>/concepts/`. A term the event does not accept is
refused by the **engine** at subscribe time, so it surfaces as a Lua error in the
log naming the term rather than as a filter that silently matches nothing. The
same paragraph is in `SubscribeFiltered`'s own doc comment, where somebody
reaching for a filter will actually be looking.

### Declining fields you never read — `SubscribeMasked`

```go
fkapi.SubscribeMasked(fkapi.EventOnUndoApplied,
    fkapi.SkipOnUndoAppliedActions)
```

The event encode is **eager and complete**: every field is marshalled before the
handler is entered. For a flat payload that is the right trade, and for the few
events carrying a container it is not — `on_undo_applied` deep-copies an undo
step's whole `BlueprintEntity` list to give a handler one `uint32`. A mask says
which fields to leave out, resolved **once at subscribe time**.

A masked **optional** reads as absent and a masked **container** as empty, and
those are the only two things maskable — which is why a `Skip…` constant exists
for exactly those fields. Being wrong about a mask therefore costs a value you
did not get, never a zero you cannot tell from a real one. **The layout does not
move**; the fields keep the offsets your guest was compiled against.
`SubscribeFilteredMasked` does both at once.

**The event id must be a constant at the call site.** The compiler proves it and
ships 2 event descriptors instead of 219; an id it cannot prove makes it ship all
of them — bigger, never broken, and nothing tells you. Both wrappers inline under
TinyGo `-opt=2`, which is gated by
`TestTheEventIdSurvivesTheGeneratedSubscribeWrapper` rather than assumed.

Two subscriptions to the same event share one registration and their filters are
**union**-ed; an unfiltered one widens the pair. An event that takes no filters
logs and subscribes unfiltered rather than failing the mod's load.

### `defines.*` — never write the number

```go
if dir == fkapi.DefinesDirectionEast() { ... }
```

A define's value is Factorio's own and is not stable across versions, and it is
**not in the API description at all** — so there is no constant to generate and
a hand-written `4` is a guess that happens to be right today. The accessor asks
the running game, through a table that carries the *name*; it caches on first
use, so it is one host call for the life of the mod and a plain load after that.

Naming is `Defines` + the dotted path: `defines.direction.east` is
`DefinesDirectionEast()`, `defines.inventory.chest` is `DefinesInventoryChest()`.
`defines.events` is deliberately **not** here — subscribe with the generated
`Event*` ids, which are per-build and are not the same numbers.

**And that bites when you DEBUG a subscription, which is the only time both
numbers are in front of you at once.** `fkapi.EventOnPlayerMinedEntity` is an
index into this build's generated event list; `defines.events.on_player_mined_entity`
is Factorio's own id, and a Lua-side log line, a `raise_event` refusal or
anything you read off the engine quotes the second. They disagree — **110 in the
bindings against the engine's 74**, for that one event on the 2.0.77 pin — so a
guest id that "does not match
what the game says" is the expected reading and not evidence of a wrong
subscription. `control.lua` translates between them; nothing above it should.

The id must be a **constant at the call site**, like a member id and an event
id, and for the same reason: the compiler scans for it and ships only the paths
you name, out of 1137. Calling the generated accessor is what makes that true —
computing an id makes the mod ship all of them.

### Noticing a load — `fk_after_load`

```go
//go:wasmexport fk_after_load
func afterLoad() { rebuildRegistryFromTheWorld() }
```

Factorio's own `on_load` **cannot touch `game`** — it runs on every client when
joining a multiplayer game and is read-only with respect to `storage` — so a
literal `fk_on_load` would not have helped a guest that wants to rebuild its
state by scanning the world. `fk_after_load` is a one-shot on the first tick
*after* a load, by which point the game exists and every API call is legal, and
it unregisters itself immediately: an idle guest pays nothing per tick.

It fires **only after a load, never on a new map** — `script.on_load` does not
run for one, and `fk_on_init` already covers that case. That makes
`--persist=none` plus rebuild-from-world a real option for the first time; before
this the only way to notice a load was to subscribe to `on_tick` forever, a
permanent per-tick cost to observe a once-per-session event. *Enforced by
`TestTheFirstTickAfterALoadIsAHook`, which checks the new-map leg too.*

#### AND IT IS A PEER-LOCAL SIGNAL, SO IT MUST NOT WRITE GUEST STATE

**This is the trap in this hook and it costs a multiplayer game, not a bug
report.** Factorio runs `script.on_load` on **every peer that loads the state**
— which includes a client joining a game already in progress. The server ran it
when it started and will not run it again; the joiner runs it on its first
simulated tick, and nobody else does. `fk_after_load` is armed from there, so it
inherits that exactly: **a one-shot armed from `on_load` is a write from
`on_load` with one tick of delay.** `fk_mod.lua` already says the rule for
`on_load` itself, above `state_load`:

> a write here is a desync waiting to happen

Under the default `--persist=table`, guest memory **is** `storage.fk_mem`, which
Factorio CRCs across every peer. So a guest that changes a counter, resets a
session, re-seeds a cache or allocates from `fk_after_load` changes it on the
JOINER ONLY, and the game logs `Multiplayer desynchronisation: crc test failed`
from the tick after the join, repeatedly, with a desync report generated. It is
measured, on 2.1.14, and it took fkipc's whole session-reset-on-load design
down — see [`agents/ipc.md`](ipc.md), "The rule the cost model implies".

**What `fk_after_load` is safe for is anything that is not guest state**: a host
call whose effect is local (a `log`), and reads. **What it is safe for and looks
unsafe is rebuild-from-world under `--persist=none`**, because there `storage`
holds no guest memory at all — nothing to CRC — and every peer rebuilds the same
state from the same world anyway.

If what you want is "notice that the session/world may have moved", drive it
from something REPLICATED instead: your own state plus the tick, or something
that arrived through an event. Those run identically on every peer by
construction, which is the whole test. *`internal/guest`'s
`TestAJoiningPeerStaysByteIdenticalToTheServer` is what a mod would use to prove
it: two module instances over one save, driven in lockstep, `storage` compared
after every dispatch.*

**And if what you want is "notice that the MOD was rebuilt", that is
`fk_migrate` and not this.** The two are easy to confuse and they sit on opposite
sides of the rule: `fk_after_load` is peer-local because it is armed from
whichever peers happen to load, while `fk_migrate` is dispatched off a comparison
of `storage.fk_build` against the running build — a function of the loaded state
alone, which every peer that loaded computes identically. So `fk_migrate` MAY
write guest state and `fk_after_load` may not. It fires on a dev rebuild since
2026-08-07; see "WHEN those rows happen" below for what it did before, which is
a case worth reading if you ever wondered why your migrate hook never ran.

### Noticing that the MOD SET moved — `fk_on_configuration_changed`

```go
//go:wasmexport fk_on_configuration_changed
func onConfigurationChanged() { adoptWhateverTheUninstalledModLeftBehind() }
```

Factorio raises `script.on_configuration_changed` when **the mod set changes** —
a neighbour added, **removed**, or moved to another version — when a startup
setting moves, and when the game version moves. It is replicated, it runs on the
peer that loaded, before the first tick, with `game` available.

**Until 2026-08-16 a guest could not observe any of that.** `fk_mod.lua`
registered exactly one thing on that hook, `finish_rebuild`, which returns
immediately unless *this mod's build stamp* moved — and a mod set changing does
not move it. So the event that reports your neighbours arrived, was consumed, and
told your guest nothing.

**The shape that asked for it**, from BetterBeltBalancer: a mod that adopts an
incumbent's entities when the incumbent is **uninstalled**. That is a
once-per-save conversion, and the only honest trigger for it is this event.
Without it the best available trigger is "the first event of the session", which
converts late and on a tick nobody chose.

**It takes no arguments.** What the engine hands the Lua handler is
`old_version`, `new_version`, `mod_startup_settings_changed`, `migration_applied`
and `mod_changes` — a **dictionary of tables**, which is tier-2 marshalling on a
path whose whole job is to say *something moved, go and look*. A guest that wants
detail reads `script.active_mods` — bound already, as `LuaBootstrap.ActiveMods`,
an ordered name/version slice — and compares it against what it saved last
session, which is what it has to do anyway: `mod_changes` describes the delta
since the last load and not since the last time your guest cared.

**It is REPLICATED, and it may write guest state.** Same argument as `fk_migrate`
and not an analogy to it: the event fires on the peer that *loaded the save*,
before the first tick, so its effects are already inside the state a joining
client downloads. A joiner never runs it and never needs to. That is what puts it
on the opposite side of the rule from `fk_after_load`, which is armed from
`script.on_load` — the thing a joiner *does* run, and nobody else.

Three more properties, each a leg of
`TestAModSetChangeReachesTheGuestWithoutARebuild`:

- **It fires after `fk_migrate`.** A load can be both a rebuild and a
  configuration change, and the heap has to be settled and republished before a
  guest is told the world around it moved. Both hooks are worth exporting; they
  answer different questions.
- **It does not fire on a new map, or on a load whose configuration did not
  move.** Factorio raises the event for neither. `fk_on_init` is the first case.
- **It is wired on the export**, so a `--persist=none` guest gets it — which is
  the guest with the strongest reason to want it, having nothing but the world to
  rebuild from.

**What it does NOT fire for is the mirror image of `fk_migrate`'s trap**: a dev
rebuild that keeps the mod's version raises no `on_configuration_changed` at all.
That case reaches `fk_migrate` through the first-outermost-dispatch path instead
(see "WHEN those rows happen" below), and this hook stays silent for it, which is
correct — nothing about the mod set moved.

### Batching — `fk.Defer()` and `fk_on_deferred`

**A blueprint paste is P separate dispatches in one tick.** Factorio raises one
`on_built_entity` per entity, each from the engine's own loop rather than from
inside another event, so `depth` in `fk_mod.lua` goes `0 → 1 → 0` P times.
That measurement decides the shape of this feature, and it is why the obvious
answer — a hook at `dispatch_done`, where the outermost dispatch already
returns — **does not batch anything**: it would fire P times too. Nesting
(`create_entity{raise_built=true}` raising from inside a handler) is a different
thing, and it is not what a paste is.

So a guest accumulates during the burst and asks for one flush:

```go
//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) { markDirty(...); fk.Defer() }

//go:wasmexport fk_on_deferred
func onDeferred() { rebuildWhateverGotDirty() }
```

`fk.Defer()` is idempotent within a tick — ten thousand calls register one
handler — and the handler **unregisters itself before it dispatches**, so an
idle guest pays zero registrations and zero per-tick calls. That teardown is
what `off_event` in `fk_mod.lua` exists for, and it is why the dispatcher list
is now walked by identity rather than by a count taken up front: `for i = 1,
#list` evaluates `#list` once, so a handler removing itself mid-walk would make
the last iteration index a nil and call it.

Two things about it are not free and are worth knowing before designing around
it:

- **The flush lands on the FOLLOWING tick.** Factorio has no end-of-tick hook,
  and `on_tick` for the current tick has already been raised by the time a build
  event arrives, so the earliest honest flush point is the next tick. A guest
  that needs its work visible within the same tick cannot use this.
- **Event registrations do not survive a save; `storage` does.** The armed flag
  is `storage.fk_deferred` and `on_load` re-arms from it. Without that, a save
  taken between the defer and the flush comes back with the guest's queue full
  and nothing registered to drain it. *Enforced by
  `TestDeferredWorkSurvivesASaveTakenBeforeItRuns`; the batching itself by
  `TestManyEventsInOneTickFlushOnce`, which asserts three dispatches in one tick
  produce one flush and leave nothing registered on `on_tick`.*

`script.on_load` **replaces** exactly the way `script.on_event` does, so there is
exactly one caller of it in `fk_mod.lua` and both the persistence adopt and the
defer re-arm hang off that one.

### Six things a guest author gets wrong, from six ports

Each of these was found by somebody writing a real mod against this runtime, and
each is one paragraph because that is all it needs. Filed as fklua-ports' AD8,
FTS10, G7, F-LOOP and Q1, plus the `Valid()` shadow at the end, which came out of
the first mod to exercise `LuaLazyLoadedValue`.

**There is no `on_nth_tick` binding, and the right answer is `fk.Defer()` calling
itself.** `LuaBootstrap::on_nth_tick` takes a Lua function and is unbindable for
the same reason `add_command` was (see "The callback seam" in
[`agents/abi.md`](abi.md)) — but unlike those two it needs no seam, because the
guest already has a one-shot timer. A guest that wants work every N ticks arms
`fk.Defer()` and re-arms it from inside its own `fk_on_deferred`, counting in
guest memory:

```go
//go:wasmexport fk_on_deferred
func onDeferred() {
    if ticks++; ticks%60 == 0 { doTheThing() }
    fk.Defer()  // re-arm: the flush unregisters before it dispatches
}
```

The unregister-before-dispatch ordering in `arm_deferred` is what makes that
safe — a guest re-arming from inside its own flush gets a fresh one-shot rather
than having it torn down by a teardown that has not happened yet. **The cost is
the one it looks like**: a guest that re-arms unconditionally has an `on_tick`
subscription for the life of the mod, which is exactly the permanent cost
`fk.Defer()` exists to avoid, so re-arm only while there is something to do.

**Exporting `fk_on_tick` IS the subscription.** All three of the first ports
exported it *and* subscribed to `on_tick` through `fk.subscribe`, which is
harmless and redundant: `fk_mod.lua` registers `script.on_event(on_tick)`
whenever the export is present. The redundant subscription costs a second
dispatch per tick and an event descriptor in the shipped table. The same is true
of `fk_on_init` and `fk_on_deferred` — **an export is a registration**, and the
only things `fk.subscribe` is for are the 219 events that have no export of their
own.

**A POLLING guest needs a bigger collector budget than the default, and this is
the first real data on it.** The default 1,024 granules is calibrated for a guest
that allocates in bursts around player actions. A guest that does work every tick
allocates every tick, so the collector is always behind: measured by
fklua-ports' nixie-tubes (G7), the default gave **15 outruns and 3 mark-
termination deadlines**, and `fkgc.SetBudget(4096)` gave a **clean plateau with
neither**. Read `Stats().Outruns` and `Stats().Deadlines` rather than guessing —
and read `EffectiveBudget()` against `Budget()` first, because a third cause with
the same symptom is the root set, which `SetBudget` cannot fix (see
[`agents/gc.md`](gc.md), "The root-scan floor").

**Subscribing in a LOOP over an array of event ids ships all 219 descriptors, and
`fklua mod` says so in as many words.** The event table is pruned to the ids it
can prove are compile-time constants at the `fk.subscribe` call site; a loop makes
none of them provable, so it ships the lot — a bigger mod, never a broken one,
which is why nothing fails at runtime. **The build output already names both the
outcome and the cause**, and this was filed (fklua-ports' F-LOOP) as having *no*
tell, which is worth correcting rather than acting on:

```
API: 4 events subscribed, of 219
API: all 219 events -- an event id was not a compile-time constant
```

The second line is the loop. The same pair exists for members and for defines. So
the remedy is not a new diagnostic, it is reading the one that is there: write the
subscribe calls out, one per event, however repetitive it looks. This is the last
event-pruning trap there is — the Rust `subscribe_filtered` inlining defect (R6)
was fixed on ports-round-a, so an `#[inline(always)]` wrapper no longer defeats
the scan.

**Re-entrant dispatch is LEGAL and is measured, and `on_init` raises nothing —
which is not the same statement.** Factorio raises some events synchronously from
inside the API call that caused them, so a guest handler that calls the API can be
called again before it returns. That works, deliberately: `dispatch` keeps a
depth, only the outermost one resets the scratch region or clears the transient
handle space, and fklua-ports' qol-research measured **depth 2 over 38 dispatches
with the arena intact** (its Q1). Nothing needs to be done to opt in.

What is NOT symmetric, and surprised that port: **the same write that raises an
event during play raises NOTHING during `on_init` or `on_configuration_changed`.**
Setting `technology.researched = true` from a handler raises
`on_research_finished`; setting it from `fk_on_init` raises nothing at all. That
is Factorio's rule rather than this runtime's — the engine does not raise script
events while a mod is initialising — and the consequence for a guest is that
`fk_on_init` must do explicitly whatever its own event handlers would have done.
*The depth-≥2 property is pinned by
`TestANestedDispatchLeavesTheOuterOneIntact`.*

**`Valid()` is TWO different questions with one spelling, and which one you get
depends on the wrapper you are holding.** `Object.Valid() bool` is the free null-
handle check — `o.h != 0`, no host call, "did anything come back at all". A
generated class wrapper embeds `Object`, so it inherits that — *unless* the class
has a `valid` attribute in the API description, in which case the generated
`Valid() (bool, error)` **shadows** it and is a real host call asking the engine
whether the object still exists. `LuaEntity`, `LuaPlayer`, `LuaSurface` and
`LuaLazyLoadedValue` are all in the second group; a bare `Object` out of an event
payload is in the first.

Same name, same receiver expression, different question and different cost —
about 12.5 µs of it. It fails **loudly**, on arity (`assignment mismatch: 1
variable but 2 values`), so nothing ships wrong; what it costs is a minute of
confusion, and the reading is: if the compiler wants two return values, you asked
the game. Reach past the shadow with `x.Object.Valid()` when what you meant was
the null check, and note that the engine's answer is the one you usually want
after a handle has crossed a tick — the null check cannot tell you an entity was
destroyed.

### Guest state across a save — `--persist`

`fklua mod --persist=table` (the default since M6) makes `storage.fk_mem` **be**
the word table the guest writes into, so a store lands in the structure Factorio
serializes with no sync step at all. Globals cannot alias — a wasm global is a
Lua local — so they are copied back after every guest call, into a buffer
allocated once. That is one table write per mutable global per event, and a
TinyGo guest has exactly one (the shadow-stack pointer).

`--persist=none` is the pre-M6 behaviour: memory is rebuilt from the module's
data segments every load. Deterministic — every client rebuilds identical bytes
— but anything accumulated during play is gone. It stays the right answer for a
stateless guest with a large heap, whose saves would otherwise carry megabytes
that mean nothing.

`--persist=packed` keeps the live word table OUTSIDE `storage` and mirrors it
into one `string.pack` page per 4 KiB. Strings serialize far better than a table
with one entry per word, at the cost of repacking whatever changed after each
guest call.

**Measured**, `examples/hello` with its 128 KiB (32,768-word) heap. Maps created
from the same `--map-gen-seed`, because map generation is random and its variance
is larger than the heap: the deltas below came out byte-identical on seeds 12345,
777 and 4242.

| mode | save delta vs `none` | per word | steady-state cost |
|---|---|---|---|
| `table` | +74,962 B | 2.29 | none — the guest writes into the saved table |
| `packed` | +14,420 B | **0.44** | ~40 µs per dirty 4 KiB page per guest call |

> **An earlier figure of 0.93 bytes/word for `table` was wrong.** It was measured
> without a fixed seed, so map-generation noise swamped the heap. Always pass
> `--map-gen-seed` when measuring anything about save size.

The page size is **4 KiB, not the 64 KiB the plan assumed**. Repacking is per
page, so page size is the granularity of the incremental cost: one store dirties
a whole page, and 64 KiB pages would mean 16,384 words repacked for one word
written. The extra `storage` entries are strings — 256 for a 1 MiB heap, against
the 262,144 numbers `table` mode would store.

What is recorded is a SET of dirty pages, not a min/max byte range. It was a
range until the range's cost was measured against the shape a real mod has: a
flush repacks the whole *span*, so one call touching a low static and a high heap
object repacked everything between (47× on that shape, and the reason the first
downstream mod shipped `table`). The marking lives inside `st8b`/`st16`/`st32` in
the runtime — `st64` is two `st32`s on the unaligned path and marks its own pair
otherwise, `st_f64` is an `st64`, `xst_f32` is an `st32` — **and, since the i32
store was inlined, in the emitter as well.** It is gated on a flag the generated
chunk sets only in packed mode; the other modes pay one upvalue read and one test
per store, measured at **3.5%** on a kernel that does nothing but store in a loop.

**The fast path is still two compares.** `DPLO`/`DPHI` bound the ONE page most
recently marked, so a store whose span lies inside it does nothing; only a store
that leaves that page calls `MEMPACK.mark`, which divides and adds. A division
per page change, not a division per store.

⚠️ **There are now FOUR places that mark, not three, and a fifth would be a
silent save corruption.** At `-opt=3` the emitter expands `i32.store` at its use
site instead of calling `st32`, so the generated Lua carries its own
`if MEMDIRTY and … then MEMPACK.mark(…) end` line — see `emitInlineStore32` in
`internal/luagen`. Any future lowering that writes `MEM` without going through
`st8b`/`st16`/`st32` has to do the same. What a missed update produces is not an error: the store lands
in the live word table, every read for the rest of the session sees it, and the
next `flush` simply does not repack that page — so the value is absent from the
save and the guest comes back with stale memory after a reload, which in
lockstep multiplayer is a desync. `TestTheInlinedStoreStillDirtiesItsPage`
replays the control.lua protocol through a stand-in `storage` and fails with
`dirty 0 / word 0` at `-opt=3` the moment the inline update is dropped.

**Its weakness used to be scattered writes** — the dirty record was a min/max
byte range, so a guest touching word 0 and the last word of its heap in one call
repacked everything in between. It is a **set of pages** since the audit, so a
flush costs the pages actually written and nothing else; see CLAUDE.md's
persistence section for the 47× that bought on the pathology.

`on_load` is read-only with respect to `storage` and has to stay that way —
Factorio runs it on every client joining a multiplayer game.
`TestOnLoadDoesNotWriteToStorage` freezes the table with a `__newindex` that
raises, so a write there fails a test rather than corrupting a save days later.

---

## The guest heap budget

**Budget the guest's linear memory at 0.2 ms of Lua-collector TOTAL per MiB per
cycle, and at a FLAT ~0.5 ms worst tick whatever the size.** That is the whole
rule since sharding; the rest of this section is why it is true, why it is the
memory's SIZE rather than its contents, and why no compiler flag gets you out of
it.

**EVERY NUMBER IN THIS SECTION IS A COST AND NONE OF THEM IS A CAP.** FkLua
imposes no memory limit of its own — not in the emitter, not in the runtime, and
since sharding stage C not in the collector either, where `fkgc.HeapCap` was a
hard 16 MiB a guest trapped against. What bounds a guest is Factorio's own bill,
priced below, and wasm32's 4 GiB.

> **THE 4 MiB WALL IS GONE, AND THE SLOPE ABOVE NEEDS RE-READING.** Until sharding
> stage B a word table was ONE Lua table, so 4 MiB was 2²⁰ = 1,048,576 keys — and
> a Lua table in Factorio holding more than 2²⁰ keys stops behaving like an array
> for ALL of its keys. Linear memory is a vector of 2¹⁹-word SHARDS now, so no
> table a guest runs on can reach 2²⁰ at any size, and neither the 20× on every
> access nor the 2,716 ms crossing tick nor the ~2.9 s rebuild on every LOAD is
> reachable. The notice `fk_mod.lua` used to print is deleted. The record of what
> the wall was, and why it took a bare Lua mod with no guest in it to find:
> [`agents/gc.md`](gc.md), "The 4 MiB wall", and [`agents/sharding.md`](sharding.md).
>
> **The 0.2 ms/MiB slope survives as a TOTAL and dies as a WORST TICK, and that
> distinction is the whole of what sharding changed here.** Lua still walks every
> word of the memory every cycle, so the total is what it was. But a `propagatemark`
> is one indivisible unit per TABLE, and the largest table is now one shard rather
> than the whole memory — so **the worst tick is bounded by 2 MiB of shard however
> large the memory gets**. Measured in game at stage B, `luaGarbageIncremental` in
> the `gcbench` arms:
>
> | guest heap | flat, worst tick | sharded, worst tick |
> |---|--:|--:|
> | 2.7 MiB | 0.843 ms | **0.426 ms** |
> | 6.7 MiB | 1.537 ms | **0.417 ms** |
>
> Flat in heap size, at about the **0.4–0.5 ms** `agents/sharding.md` §5 predicted
> by extrapolating the 0.202 ms/MiB slope to a 2 MiB shard. **RE-TAKEN AT STAGE C
> OUT TO 40 MiB and it is still flat** — 0.479 ms at 40 MiB against 6.61 ms for a
> single 32 MiB table. The full curve, and the cost table that replaces it, are
> below. Two ticks of a 128 MiB guest were 25.85 ms each when the memory was one
> table; the same guest now pays the same total in ~64 pieces the collector can
> put wherever it likes.

**AND THE HEADLINE RULE AT THE TOP OF THIS SECTION IS A COST, NOT A CAP.** FkLua
imposes no memory limit of its own — not in the emitter, not in the runtime, and
since sharding stage C not in the collector either. "Treat 16 MiB as the point
where players start to feel it" is a judgement about a frame budget on somebody
else's machine, and the numbers below are what it is made of.

### Where the pause lives

The live linear memory is a Lua array table with one slot per 32-bit word, in
**every** persist mode — `packed` and `table` differ in what enters `storage`,
not in what the guest runs on. Lua 5.2's collector traverses a table in a single
`propagatemark`: `traversestrongtable` loops over `h->sizearray` calling
`markvalue` on every slot, `singlestep` performs exactly one `propagatemark` per
step, and `incstep`'s debt loop cannot re-enter until that one returns. **One
gray object is one indivisible unit of work.** A heap of N words is therefore N
`markvalue`s inside one tick, no matter what the collector is asked to do.

**THE PRE-SHARDING CURVE, KEPT BECAUSE IT IS THE TOTAL AND NOT THE PAUSE.**
Measured in Factorio 2.0.77, an otherwise empty map, ONE word table in `storage`,
900 ticks of `--benchmark --benchmark-verbose luaGarbageIncremental`. The
right-hand column is what one table of that size cost in a single
`propagatemark`, and no guest has one table any more:

| linear memory | words | GC mean/tick | worst tick, ONE TABLE |
|---|--:|--:|--:|
| — (control) | 0 | 4.9 µs | 0.05 ms |
| 2 MiB | 524,288 | 5.7 µs | 0.56 ms |
| 8 MiB | 2,097,152 | 11.7 µs | 1.60 ms |
| 32 MiB | 8,388,608 | 37.5 µs | 6.61 ms |
| 64 MiB | 16,777,216 | 71.8 µs | 12.61 ms |
| 128 MiB | 33,554,432 | 140.0 µs | 25.85 ms |

The MEAN is the total and it survives: **0.202 ms per MiB per cycle**, flat from
8 MiB up, and sharding does not change how much of the memory Lua walks.

**RE-TAKEN UNDER SHARDS, IN GAME, AT STAGE C — and the worst tick is FLAT.**
Same instrument, `scripts/run-gcbench.sh`, real packaged mods whose linear memory
is a vector of 2¹⁹-word shards, `luaGarbageIncremental` per tick over 1,200-tick
runs:

| guest linear memory | ticks sampled | worst `luaGarbageIncremental` |
|---|--:|--:|
| 2.7 MiB (stage B) | 3,597 | 0.426 ms |
| 3.4 MiB | 3,597 | 0.525 ms |
| 6.7 MiB (stage B) | 3,597 | 0.417 ms |
| 8.4 MiB | 3,597 | 0.583 ms |
| 40 MiB | 3,597 | 0.479 ms |
| **52 MiB** | **23,999** | **4.178 ms** |
| **54.5 MiB** | **23,999** | **1.141 ms** |

**Flat at 0.42–0.58 ms from 2.7 MiB to 40 MiB on equal-length runs**, against
6.61 ms for a single 32 MiB table and 12.61 ms for a single 64 MiB one. That is
the shard, not the guest: one `propagatemark` is one TABLE, the largest table is
2 MiB of shard however large the memory gets, and ~0.5 ms is what 2 MiB of shard
costs. Stage B extrapolated 0.4–0.5 ms from the 0.202 ms/MiB slope and predicted
it would hold; at 40 MiB — fifteen times the size stage B measured — it does.

**AND THE LAST TWO ROWS WERE THE ONES TO READ HONESTLY.** They are 6.7× longer
runs of the same guest at the same size and they disagree by 3.7× — 4.178 ms
against 1.141 ms. Neither is a shard: 4.178 ms is eight shards' worth in one
tick, which a shard bound cannot explain. This file recorded that as "not a hard
bound" with a hypothesis (Lua's ATOMIC step) and no measurement behind it.

**THAT IS NOW MEASURED, AND THE HYPOTHESIS WAS RIGHT ABOUT THE MECHANISM AND
WRONG ABOUT WHEN YOU PAY IT.** See "the atomic tick" below, which replaces the
guesswork with a shape, a size and a period.

#### The atomic tick — the shard bound's one real exception

`scripts/run-gctail.sh` is a **bare Lua mod with no wasm in it**, holding a
configurable vector of live array tables, measured by Factorio's own
`--benchmark-verbose luaGarbageIncremental` over 24,000-tick runs. Same
instrument rule as the wall, the shard probe and the grow probe, and for a
second reason here: through a real guest the heap size, the table COUNT and the
table SIZE all move together, and deciding between them needs them varied one at
a time. They are three numbers in a config file there and are not expressible in
`run-gcbench.sh`.

**It reproduces the tail in a mod containing no FkLua at all**, which is what
makes it attributable to the representation:

| arm | live set | tables | worst tick | p99 | mean |
|---|---|--:|--:|--:|--:|
| `s16` | 32 MiB | 16 × 2¹⁹ | **8.909 ms** | 0.057 | 0.009 |
| `s26` | 52 MiB | 26 × 2¹⁹ | **11.428 ms** | 0.065 | 0.010 |
| `s32` | 64 MiB | 32 × 2¹⁹ | **14.084 ms** | 0.076 | 0.011 |
| `t52` | 52 MiB | **52 × 2¹⁸** | 14.216 ms | 0.083 | 0.012 |
| `q52` | 52 MiB | **104 × 2¹⁷** | 12.388 ms | 0.071 | 0.010 |

**It scales with BYTES and not with table count, so no shard size fixes it.**
The heap series is 8.9 / 11.4 / 14.1 ms at 32 / 52 / 64 MiB — and that is
**0.278 / 0.220 / 0.220 ms per MiB**, which is the 0.202 ms/MiB *per-cycle
TOTAL* from the table above. The atomic tick is one whole cycle's traversal
collapsed into a single indivisible step, which is exactly what
`propagateall` inside Lua 5.2's `atomic()` does. Cutting the same 52 MiB into
2× and 4× as many tables does **not** reduce it (11.4 → 14.2 → 12.4 ms), because
`atomic` drains the entire gray list in one call however many objects are on it.

**And it is not periodic — it is a ONE-OFF on a quiescent live set.** In every
`s*`/`t*`/`q*` arm the worst tick lands in the first ~350 ticks and never
recurs: `s26`'s second-worst tick over the remaining 23,700 is **0.527 ms** and
its third is 0.231. A table Lua has marked black stays black until something
writes it.

**A live set that IS written never shows the big tick at all**, which is the
finding that matters for a real guest, and it falsified the obvious refinement
on the way. Storing 256 words a tick into the same 52 MiB:

| arm | writes reach | worst tick | p99 | mean |
|---|---|--:|--:|--:|
| `w26a` | **all 26** shards | 1.761 ms | 0.789 | 0.062 |
| `w26h` | 6 shards | 1.909 ms | 0.794 | 0.063 |
| `w26n` | **1** shard | 2.047 ms | 0.806 | 0.061 |

A store into a black table fires Lua's back-barrier and puts it on `grayagain`,
so the expectation was that the atomic tick would track the WIDTH of the write
set. **It does not — 1, 6 and 26 shards give 2.05, 1.91 and 1.76 ms, and the
narrowest is the worst.** What writing changes is that the collector never goes
quiet: no tick in any write arm exceeds **2.05 ms** over 24,000, and the body of
the distribution rises instead (p99 0.065 → 0.79 ms).

**`collectgarbage` pacing is a real ~10% lever, and this file used to say it was
not.** One `collectgarbage("step", 2)` per tick: `w26p` against `w26a` is
**0.93× worst, 0.89× p99, 0.87× mean**; `p26` against `s26` is 7.711 ms against
11.428. The old claim — "moves the pause by less than its own noise, because
there is nothing to pace" — was measured when linear memory was ONE table and
there genuinely was nothing to pace. It is out of date, and it is still not a
fix: 10% off an 11 ms tick is 10 ms.

**So the budget is three numbers now, and they are COSTS, not caps.** Neither is
a limit and nothing in FkLua refuses a size:

| what a MiB of guest linear memory costs | |
|---|--:|
| Lua GC **total**, per cycle | 0.202 ms |
| Lua GC **worst tick**, steady state | flat at ~0.5 ms, whatever the size |
| Lua GC **worst tick**, guest actively writing memory | ~2 ms at 52 MiB, and flat-ish — the body rises, the tail does not |
| Lua GC **atomic tick** | **up to 0.22 ms/MiB of LIVE SET, in one indivisible step** |
| host RAM, `--persist=table` | 4.00 MiB |
| host RAM, `--persist=packed` | 5.00 MiB |
| save size, `--persist=table` | 586 KiB |
| save size, `--persist=packed` | 113 KiB |
| load time, sharded | ~26 ms flat, no cliff |
| fkgc metadata, if collected | 10,240 B (0.977%) + a 31.4 KiB floor |

**The atomic row is a RANGE WITH A SHAPE, not a ceiling, and quoting it as a
per-tick rate would be wrong in both directions.** Its shape: it is one whole
cycle's marking in one tick; it is bounded by the live set and not by the shard;
table count does not reduce it; a quiescent large live set pays it about once
and a continuously-written one appears never to pay it at all. At 52 MiB that is
**~11 ms once**, against ~2 ms recurring while the guest is busy. Re-measure
over a long run before promising anyone anything — the 1,200-tick runs above
never saw it, and a rare tail is not disproved by a short run.

A 60 UPS tick is 16.7 ms. Under shards, 128 MiB of guest memory is 26 ms of
collector work spread over a cycle in ~0.5 ms pieces, plus 512 MiB of host RAM
and a save that is worth choosing a `--persist` mode for — none of which is a
stutter, and all of which is a bill.

#### What a player actually experiences

**The worst-tick tables in this file read scarier than they are, and the missing
sentence is what a tick over budget looks like from a chair.** A 60 UPS frame is
**16.67 ms**. A tick that takes longer than that does not lag, stutter or
desync — it drops ONE FRAME. Every number in this section belongs against that,
and against the thing the game already does on its own:

| event | cost | how often | what a player sees |
|---|--:|---|---|
| steady paced collector step | ~0.2–1.3 ms | every tick, only while collecting | nothing — it is 1–8% of a frame |
| Lua GC, steady state | ~0.5 ms | every cycle | nothing |
| Lua GC, guest writing memory | ~2 ms | in the body of the distribution | nothing |
| **grow tick**, collected | **17–25 ms** | only while the heap is GROWING, and flat in its size | one frame, during active growth |
| **shard doubling tick** | **16–19 ms** | **once per 2 MiB of memory the guest EVER takes** | one frame, once, permanently |
| **Lua's atomic tick** | **~11 ms at 52 MiB** | about once for a large quiescent live set | not even one frame |
| mark-escape pause | bounded by the budget | when a guest outruns its collector | nothing — that is what the escape is for |
| *vanilla autosave* | *100s of ms* | *every few minutes, forever, with no mod installed* | *a visible hitch, routinely* |

**The last row is the scale, and it is not FkLua's.** Factorio stalls for
hundreds of milliseconds on an autosave on a mid-size map with no mods at all,
and players do not file bugs about it. Everything above it is smaller, rarer, or
both.

**Two of these are once-EVER rather than per-tick, and that is the distinction
the tables cannot show.** The shard doubling is one frame per 2 MiB of memory the
guest ever takes: a guest that grows to 40 MiB drops twenty frames **over its
entire lifetime**, not twenty per second. The grow tick only exists while the
heap is climbing, and stops when it stops.

**And the pre-arc numbers are the other half of the scale.** Before sharding and
the grow pacing, the same guest paid **254 ms** for a grow at 40 MiB, **2.8 s**
for a `memory.grow` crossing the 4 MiB wall, and **5.3 s** rebuilding its table
on every LOAD. Those were seconds — real, visible, "is it frozen" pauses. What
this section now argues about is whether one frame is dropped once per two
megabytes. That is the arc, and a reader meeting the worst-tick tables cold
should be told where they came from.

**The honest caveat: a dropped frame in a lockstep multiplayer game is dropped
for everyone.** That is why these are worth bounding at all rather than
shrugging at. It is not why they are worth being frightened of.

**`memory.grow` USED TO BE THE SPIKE, AND IT IS PACED NOW.** It is not in the
table because it is not a per-MiB rate — it is a per-GROW one. `mem_grow` writes
a zero into every new word, at about **107 ns a word** in Factorio's Lua, and
that is unavoidable: a Lua slot exists only once something writes it. What was
avoidable is WHICH TICK the writing lands on.

The model is confirmed rather than inherited. `scripts/run-growprobe.sh` is a
bare Lua mod with no wasm in it that times `mem_grow`'s fill loop directly, and
fitting `t = fixed + words × per_word` over four increments at three heap sizes
gives **109.7–127.7 ns a word and a NEGATIVE intercept at every size**. There is
no fixed cost to amortise a big increment against, and the aggregate says the
same thing: reaching 40 MiB in 640 grows of one wasm page costs **0.984×** what
reaching it in 10 grows of 4 MiB costs. The quarter law was buying nothing.

**THE MODEL UNDER-PREDICTS A GROW THAT CREATES SEVERAL SHARDS AT ONCE, by about
1.7×, and that correction is measured downstream rather than here.** BBB drove
the ladder for real — 3,400 4×4 teardown-and-rebuilds, per-tick
`--benchmark-verbose` — and clocked **48.7 ms at 2→4 MiB, 120.3 at 4→8, 226.1 at
8→16 and 782.4 ms at 16→32**. The middle rungs sit on 107 ns/word to within 7%;
the last is **1.74× over a ~450 ms projection**, and the excess is not the fill.
It is [`sharding.md`](sharding.md) §15's own residual arriving eight times at
once: a 16 MiB doubling creates eight new 2¹⁹-word shards and **each pays its
final array-part reallocation** on top of being filled. So read 107 ns/word as
the per-word floor and add §15's per-shard reallocation once for every 2 MiB the
grow crosses; a grow that stays inside one shard is the case the model was
fitted on. The projection's METHOD was right and its answer was optimistic,
which is the direction to be wrong in only if you say so.

So the fix is two things, and each covers what the other cannot:

- **The runtime keeps a FILL CURSOR ahead of `MEMSIZE`** and advances it in
  8,192-word pieces from a one-shot `on_tick` that a grow arms and an empty
  lookahead tears down. A grow into pre-built words costs **1.2–2.7 µs** instead
  of milliseconds, and pacing the fill costs 2–3% over doing it in one go. It
  serves BOTH growth laws, because it is in `mem_grow`.
- **`fkgc`'s quarter is capped at one wasm page** (`growCapSpans`, 64 KiB). A
  guest allocating faster than the pre-build's 32 KiB a tick outruns it, and the
  cap is what the fallback is bounded by. It serves only a COLLECTED guest.

Measured in game with `scripts/run-growbench.sh`, which reports the distribution
of the ticks the guest itself said it grew on, 6,000 ticks, one guest allocating
8 KiB a tick to each target:

| growth law | target | worst GROW tick | p90 | median |
|---|---|--:|--:|--:|
| **collected** (fkgc) | 4 MiB | 43.2 → **17.2** ms | 26.4 → **2.6** | 7.0 → **1.1** |
| | 16 MiB | 108.5 → **22.8** | 78.7 → **3.3** | 11.4 → **1.6** |
| | 40 MiB | **253.9 → 24.6** | **168.5 → 3.7** | 20.0 → **2.2** |
| **leaking** (TinyGo) | 4 MiB | 127.4 → **98.0** | — | 37.5 → **21.1** |
| | 16 MiB | 495.0 → **491.3** | — | 60.7 → **38.5** |
| | 40 MiB | **998.0 → 974.5** | — | 59.3 → **37.5** |

**The number that matters is not the ratio, it is that the collected column
stopped scaling.** 43 / 109 / 254 ms was proportional to the heap; 17 / 23 / 25
is flat in it. A collected guest's grow tick no longer depends on how big its
heap is.

**AND WHAT IS LEFT OF IT IS NOT THE FILL — IT IS LUA REALLOCATING A SHARD.** The
residual worst grow ticks are 16.2–19.1 ms at a 16,384-word grow, i.e. 1,090 to
1,503 ns a word against a 107 ns/word model, so the model does not explain them
and should not be stretched to. They are perfectly regular: **every odd megabyte,
1.00 through 41.00, and flat in heap size** — which is 2¹⁸ entries into a 2¹⁹-word
shard, the last array-part doubling a shard ever does. One indivisible
reallocation per 2 MiB of guest memory ever taken, and nothing in Lua can split
it. It was always there and the fill was simply bigger: the pre-pacing 43.2 ms
outlier at a 4 MiB heap is a 163,840-word grow crossing 3.00 MiB — 17.5 ms of
fill plus 25.7 ms of this.

**So the grow tail is bounded by the SHARD SIZE now**, exactly as Lua's own
collector's worst tick is, and for the same reason. `agents/sharding.md` chose
2¹⁹ words "for the access cost and the guard" and said the tail it leaves is a
measurement rather than an assumption; this is that measurement, and the knob is
recorded there.

### It is the SIZE of the memory, not the part in use

`mem_grow` writes a zero into every new word, so a page the guest has grown into
and never touched is as many live Lua slots as a page it uses constantly. And
TinyGo's wasm `growHeap` **doubles**: it grows by exactly the current size, so a
guest is always on the ladder 128 KiB, 256 KiB, … 32, 64, 128 MiB. A guest that
needs 65 MiB gets 128 and pays ~26 ms of worst tick for a heap that is half
untouched.

**THE DOUBLING IS ALSO WHY THE PACED PRE-BUILD BARELY HELPS A LEAKING GUEST, and
that is the honest half of the table above.** The lookahead is capped at 1 MiB
because a materialised word above `MEMSIZE` is a real cost — 16 B of host RAM,
2.29 B of save under `--persist=table`, its share of the 0.202 ms/MiB walk — and
a lookahead of "one grow" would be UNBOUNDED for a doubling guest: it would
permanently double the footprint of a guest that then stopped growing. So the
pre-build removes a fixed ~1 MiB, about 23–28 ms, from every grow tick. That is
a quarter of a 4 MiB doubling and **2.5% of a 32 MiB one**, which is exactly what
the leaking rows measure: 127.4 → 98.0 ms at 4 MiB, 998.0 → 974.5 at 40.

**Nothing in FkLua can bound a doubling guest's grow tick, and the thing that
does is not growing by doublings.** At a 40 MiB target that is `974.5 ms`
leaking against `24.6 ms` collected — a factor of **40**, on the same guest,
same allocation rate, same machine, in the same run of the same script. It is
the strongest number this repo has for `--gc=collected`, and it is a fact about
the GROWTH LAW rather than about collecting: neither arm reclaims anything.

This is why **the save size is not a proxy for the pause**. Untouched pages are
absent from a packed save and zero pages compress away, so the first downstream
mod's 3.6 MB save sat on a 64–128 MiB linear memory. Its three measurements —
3.0 ms at n=50, 15–27.8 ms at n=200, 88.5 ms at n=500 — land on 16, 128 and
512 MiB of the doubling ladder, and the apparent super-linearity in *its* scale
parameter is just the ladder: 4× the work crossed three doublings.

`fk_mod.lua` logs the number once per doubling from 16 MiB up, because nothing
else can see it:

```
fklua: this guest's linear memory is now 128 MiB, which is about 25.6 ms of
worst tick -- Lua's collector walks every word of it in one step it cannot split
across ticks. It is the SIZE of the memory, not the part in use, and no
--persist mode changes it.
```

### What to do about it, in order

**`--gc=collected` IS THE RECOMMENDED DEFAULT FOR A NEW GO GUEST, as of sharding
stage C, and the ordering below says so.** What changed is not the collector, it
is what happens when it is outrun: `fkgc.HeapCap` is gone, the collector's
metadata scales with the heap, and a collected guest that allocates faster than
its budget reclaims now GROWS exactly like a leaking one instead of trapping.
There is no size at which turning the collector on makes a guest worse than
leaving it off, so the advice that used to be item 0 is item 0 because it is
first, not because it is an alternative.

**`-gc=leaking` is the EXPERT path now**, and it is a real one: it is correct for
a guest whose heap is genuinely allocation-disciplined — nothing to reclaim means
nothing to pay a collector for, and what it buys back is the collector's own
emitted code, measured downstream at **+32.4% of `fk_module.lua` and +13.7% of
the zip**. It is also the only option for wasip1. What it is not any more is the safe default.

**This paragraph used to cite the first mod outside this repo as having measured
both arms and shipped leaking, and that citation is withdrawn rather than
corrected.** It had measured both arms and it had shipped leaking; it
re-measured on the sharded pin and **ships collected since 2026-08-02** — the
steady state could not separate the arms, and the doubling stall its discipline
was buying immunity from came out at 782 ms against 71. The lesson is not that
the opt-out is wrong, it is that **allocation discipline is not a substitute for
a growth law**: a guest that allocates a bounded amount per edit still climbs the
doubling ladder over a three-hundred-hour save, and the tick that doubles is
paid by every client at once. Choose `leaking` on a measurement of your own heap
over a long session, not on a prediction about your own tidiness.

**AND IT IS THE DEFAULT FOR A NEW PROJECT NOW, NOT A RECOMMENDATION.** A
recommendation is four lines of shell an author retypes correctly or does not;
what shipped as of this commit is that `fklua init` writes **`gc = "collected"`**
into `fklua.toml` and scaffolds a guest that carries a collector, so a fresh
project builds and packages collected with no hand edits at all:

```
fklua init my-mod                  # fklua.toml, guest/go/{go.mod,gc.go,main.go}
cd guest/go && go mod tidy && cd ../..
fklua gen-bindings && fklua lock   # bindings land at guest/go/fkapi/
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=custom -opt=2 -o ../../my-mod.wasm .)
fklua mod my-mod.wasm              # --gc comes from fklua.toml
```

`guest/go/gc.go` is the fkgc import — the same file every example in this repo
carries — and `guest/go/main.go` calls it from `fk_on_tick`. Both halves are
needed and neither is optional: `-gc=custom` without the import does not LINK,
and the import without `-gc=custom` is an empty package.

**`guest/go` rather than `guest`, and it is the bindings that decide it.**
`gen-bindings` hard-codes `guest/go/fkapi/fkapi.go` and `fklua lock` hashes that
exact name, so scaffolding the module at `guest/` put the generated bindings a
segment deeper than any document describes — importable as
`<mod>-guest/go/fkapi`, which BUILT, which is why it lasted: nothing failed, and
three independent mods each moved their guest to `guest/go` by hand instead. The
Rust arm had the same defect louder (`guest-rs/` against bindings at
`guest/rust/fkapi`, so the generated crate was orphaned outright) and was fixed
first. The two are siblings again — `guest/go` beside `guest/rust`, which is what
this repo's own tree looks like — and
`cmd/fklua.TestTheScaffoldIsWhereTheBindingsGo` asserts the RELATIONSHIP rather
than the two strings: whatever the scaffold picks, the bindings are a direct
subpackage of it.

**`gc = "leaking"` is the documented expert opt-out**, and changing it means
changing the tinygo flag too — `fklua mod` refuses `collected` for a module that
exports no collector, so a mismatch is a build error naming both halves rather
than a mod that silently fails to collect.

**The compile-FLAG default is UNCHANGED and that is deliberate.** `fklua mod`
with no manifest still defaults to `--gc=leaking`, because the flag is not
inert: it is refused unless the module exports the collector surface, so
flipping it would turn every existing `tinygo -gc=leaking` build into a compile
error naming a flag its author never chose. **A manifest with no `gc` key is
treated as absent rather than as "leaking"**, so every project written before
the key existed emits exactly what it emitted before. What is collected by
default is a PROJECT, which is the unit an author actually chooses; a bare
invocation over somebody's existing wasm is not.

0. **Collect the heap: `--gc=collected`, in EITHER LANGUAGE, and for an ordinary
   event handler it retires items 1 and 2 below.** It was Go only until
   `guest/rust/fkgc` landed; it is not any more, and the gate never mentioned a
   toolchain in the first place. `--gc=collected` is refused unless the module
   exports `fk_gc_step`/`fk_gc_dirty_base`/`fk_gc_dirty_cap`, since those are
   what `control.lua` binds and the flag is not inert without them — it takes the
   inlined 8-byte store back out of line and emits the barrier's arming surface,
   so a guest with no collector would pay for one.

   ```sh
   # Go: one import, one flag
   tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 ...
   # Rust: one flag, and no source change at all
   cargo build --release --target wasm32-unknown-unknown -p <guest> \
       --features fk/fkgc
   ```

   **`guest/rust/target/` is gitignored, and that is what makes it misleading.**
   `git status` stays clean, a branch switch leaves it alone, and cargo keeps ONE
   artifact path per (package, profile) whatever features built it — so the tree
   can hold rlibs from the other arm of an A/B, or from a crate that no longer
   exists. Stale symbols out of it have already sent one investigation the wrong
   way. **`make clean-rust`** deletes it. The tests and `scripts/run-roundtrip.sh`
   set `CARGO_TARGET_DIR` per arm for exactly this reason and are unaffected;
   this is the copy a human's `cargo build` and `scripts/run-guest.sh` leave in
   the checkout.

   `fklua init` writes `gc = "collected"` and scaffolds a guest that carries a
   collector in **either** language — `guest/` for Go, `guest-rs/` for Rust — so
   a new project IS collected rather than being told to be.

   The Rust side needs no import because `guest/rust/fk` owns the single
   `#[global_allocator]` site and the feature chooses what backs it — the bump
   arena or `guest/rust/fkgc`. **Do not declare that feature in a guest's own
   `Cargo.toml`**: Cargo's v2 resolver unifies features across a workspace build,
   so it would turn the collector on for every other crate in the same
   invocation. Pass it on the command line.
   `TestNoRustExampleDeclaresTheCollectorFeature` holds that.

   Two things differ between the two collectors and both are consequences of
   rustc rather than choices. **Allocation never collects at all in Rust**, with
   no exception: the Go collector keeps one last-resort synchronous `Collect()`
   for a refused `memory.grow`, and that is sound only because TinyGo's
   `markStack()` scans the live shadow stack it landed in. rustc keeps live
   references in wasm LOCALS, which nothing can scan, so a refused `memory.grow`
   traps instead. And **the useful budget floor is higher**, because a Rust
   guest's statics are bigger: the root re-scan is charged against the step
   budget, so a budget under `RootWords()×4/16` granules cannot terminate a mark
   under budget and escapes via the deadline instead. Measured on
   `examples/gctorture`: ~1,200 root words, so about 300 granules, against a
   default budget of 1,024. Below ~512 the deadline starts doing the work.

   Stage B of
   [`agents/gc.md`](gc.md) shipped a conservative mark-sweep collector for the
   guest's own heap. The number that makes the case belongs here rather than
   only there: `guest/go/examples/churn` — an ordinary allocating event handler,
   2,016 B/event — reaches **128 MiB of linear memory over 60,000 events under
   `-gc=leaking` and 768 KiB collected**, computing the same checksum in the
   same wall time. That is 171×, and it is the difference between 25.6 ms of
   permanent worst tick and nothing measurable.

   Three flags and one import, and the import is free under every other `-gc`:

   ```go
   import _ "github.com/Techrocket9/fklua/guest/go/fkgc"

   //go:wasmexport fk_on_tick
   func onTick(tick uint32) { /* ... */ fkgc.CollectIfNeeded() }
   ```
   ```sh
   tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 -o g.wasm .
   fklua mod g.wasm --gc=collected
   ```

   **What it costs**, so that none of it is discovered later:

   - **Allocation is not slower. It is slightly faster**: 0.96× on `churn` and
     1.03× on `real_names` against the same guest built `-gc=leaking`,
     measured with no collection running. See gc.md for why, which is not a
     fact about allocators.
   - **31.4 KiB of linear memory, plus 40 KiB per 4 MiB of heap** — the
     collector's metadata, and since sharding stage C it SCALES rather than
     being reserved. Exactly, and asserted by `TestTheMetadataSizeModelHolds`
     rather than written here and left to drift:

     ```
     MetaBytes = 32,116 + 40,960 x ceil(heap / 4 MiB)
     ```

     which is a **0.977% tax on the heap** on top of a 31.4 KiB floor. The floor
     was 163 KiB before, so a small guest pays a fifth of what it did; a guest
     with a 40 MiB heap pays 441 KiB, which no build tag could have bought.
     `fkgc.Stats().MetaBytes` reports it live. For a guest as small as
     `examples/hello` the floor is still more linear memory than the guest has,
     and the collector is still not worth it there.
   - **THERE IS NO HEAP CAP.** There was: 16 MiB, hard, `-tags fkgcheap4` and
     `fkgcheap64` moving it to 4 and 64 — and a guest that grew past it trapped
     with a bare `unreachable`, because wasm-unknown has no stderr. It existed
     only because the mark bitmap and the span table were statically reserved
     .bss, and it is deleted along with the tags. The bounds left are wasm32's
     4 GiB (less one span, where `uint32` span arithmetic would wrap) and
     whatever `memory.grow` refuses. Measured through the real collector: a
     36 MiB live set, a 37.6 MiB heap, checksum unmoved across collections.
   - **The pause is PACED, since stage C.** A collection is cut into bounded
     steps driven from a one-shot `on_tick` that exists only while one is in
     flight, so an idle guest still registers nothing and pays nothing. The
     price is LATENCY, not total cost: the same work is done either way.

     **Two figures, and quote the second one.** Host-side, at a 5.7 MiB heap
     where a stop-the-world collection costs 110 ms in one tick, the paced worst
     tick is about **0.2 ms** — 555× lower over 622 steps. **In real Factorio**
     (stage D, `examples/gcbench`, a 2.8 MB heap) the same comparison is
     **240 ms stopped against 32.6 ms paced — 7.4×**, with a median tick of
     0.10 ms and a p90 of 1.2 ms. The median tracks the calibration and the tail
     does not. The ratio is the part that travels between a harness and the game;
     the milliseconds are not.
   - **A mass-builder is still not covered, and the bill arrives somewhere
     specific.** Item 1 stays true for a guest whose work is bulk construction:
     the measured reclaim rate is ~190 KB/s at a 0.5 ms/tick budget, which
     covers an event handler with headroom, a blueprint paste with about two
     seconds of latency, and 3,200 entities in one go not at all.

     **And no paced step can run INSIDE a dispatch.** A guest that does its bulk
     work in one `on_init` or one event handler defers the entire collection to
     the next tick source — which for an event-only guest is the first ticks
     after a **LOAD**. Measured on two guests: the first downstream mod compiled
     200 networks in one `on_init` and paid **152 steps over ticks 0–151,
     105 ms of script**, on a mod whose headline is that a finished build runs
     no script at all; `examples/gcbench` builds a 2.3 MB live set the same way
     and pays **552 steps over ticks 0–542**. If your guest's cost seems to have
     vanished, it has not — look at the ticks after the next load.
   - **Stores cost 7–13% more WHILE A MARK IS RUNNING**, on a store-heavy guest,
     and nothing at all otherwise. That is the write barrier — the same
     `MEMDIRTY` page set `--persist=packed` maintains, armed only while the
     collector is marking, which is why an un-armed guest's emitted chunk is
     what it always was. Sweeping needs no barrier and runs unarmed, so the
     window is half a collection rather than all of it.
   - **wasip1 is refused**, with a diagnostic, even though such a guest can
     carry the surface: a parked goroutine's `csp` is a pointer one past the end
     of its stack block and this collector does not retain through one. See
     gc.md section 1.

   **WHAT THE FIRST REAL GUEST DECIDED — TWICE, IN OPPOSITE DIRECTIONS, AND THE
   SECOND ANSWER IS THE ONE THAT STANDS.** Read both, because what moved between
   them is the useful part and it was not the collector.

   *2026-08-01, and it shipped:* the first mod outside this repo built both arms
   and chose **`-gc=leaking`**. Its heap was already diet-bounded at 1.9 MiB, so
   there was nothing for a collector to reclaim; what collected cost instead was
   152 ticks of collector script after every load (above), +25.9% of
   `fk_module.lua`, +11.9% of the zip, +2.3 ms on every load and 163 KiB of
   permanent linear memory — a `.bss` reservation this collector no longer makes.

   *2026-08-02, and this is what ships:* the same mod re-measured on the sharded
   pin and **flipped to `--gc=collected`**. Three of its four reasons had moved
   underneath it. The 152 post-load ticks were 71. The 163 KiB was **73,112 B**,
   because stage C deleted the reservation and made the metadata scale. And the
   premise doing the real work — *there is nothing here for a collector to
   reclaim* — was true of a fresh save and false of a three-hundred-hour one:
   a bounded allocation per player edit still climbs the doubling ladder, and
   driven up it for real the leaking arm's grow ticks came out **48.7 ms at
   2→4 MiB, 120.3 at 4→8, 226.1 at 8→16 and 782.4 ms at 16→32**, against a
   collected worst tick of 71 ms over the same 3,400 operations. The steady
   state could not separate the arms in either direction. What was left on
   leaking's side of the scale was module size, which is what it still is.

   > **Allocation discipline is not a substitute for a growth law, and that is
   > the transferable lesson.** This section's advice — *allocate less, and
   > allocate it once* — is not retired: it is what makes a collected guest's
   > mark cheap and its steps invisible, and the mod above has a ~9 KB live set
   > precisely because it did that work. What the discipline cannot do is bound
   > `memory.grow`, because a leaking guest's memory is a doubling function of
   > everything it has ever allocated rather than of what it holds. A guest that
   > has already done the work still has that to buy.

   Where collected wins outright is unambiguous and is the reason the other arm
   stays buildable rather than the reason it is chosen: at 54× that mod's own
   churn suite, leaking climbed 182 → 4,115 KB and was still climbing while
   collected plateaued at 418 KB from t≈2.4 s — **7.0×, checksum-identical, same
   final audit, zero mark deadlines.** That is what an allocation regression in a
   compile path looks like, and the collected arm is the only one that survives
   it.

   **And one benefit that has nothing to do with collecting.** On the same mod's
   200-network create, `-gc=leaking` put 1.14 MB of allocations into a 1.92 MiB
   arena because TinyGo's `growHeap` DOUBLES; `fkgc` reached 1.50 MiB for the
   same work — **14% less linear memory with `cycles=0` on that line, nothing
   collected.** The `-gc=custom` seam buys a better growth policy on its own. It
   does not survive the 163 KiB of metadata above, so it is an effect to know
   about rather than a reason to switch.

   **The two knobs, and they are independent.** Both are optional and both
   default to something sensible; a guest that sets neither gets a collection
   when its heap has grown 256 KiB, paced at about 0.5 ms of collector time per
   tick.

   ```go
   fkgc.SetThreshold(bytes)  // WHEN a collection starts: bytes of heap
                             // FOOTPRINT taken since the last one. Default
                             // 256 KiB. Recycling reclaimed blocks registers no
                             // pressure, so a guest in steady state collects
                             // when it must and not on a schedule.
   fkgc.SetBudget(granules)  // HOW FAST it runs: 16-byte granules of heap
                             // touched per step. Default 1024, which is ~16 KiB
                             // and ~0.5 ms on the host-side 32.8 ms/MiB. In the
                             // GAME that default measured 623 us of median step
                             // on the first real guest and 0.10 ms on gcbench --
                             // but 2.1 ms and 32.6 ms of WORST step. The budget
                             // is the worst tick in a straight line only until
                             // the tail, and the tail is bigger than the
                             // calibration says.
   ```

   Turning the budget UP finishes the collection sooner and pauses longer.
   Turning it DOWN pauses less and takes more ticks — **but not below your own
   allocation rate, and that is a stricter floor than "a few hundred".** Each
   step first re-scans the marked objects in every 4 KiB page the guest wrote
   since the last step, and that costs about a span's walk. A guest that dirties
   pages faster than its budget can re-scan them spends every step on the
   backlog: **marking never terminates, the barrier stays armed, nothing is
   reclaimed, and the heap grows exactly as if there were no collector.**

   That is not hypothetical. `examples/gcbench` allocated 576 KB/s against a
   collector whose measured sustained reclaim at the default budget is
   190 KB/s, and sat in the mark phase for 600 straight ticks in a real
   Factorio while its heap climbed 2.85 → 8.68 MB. **Check the arithmetic
   before you tune anything else**: bytes per tick × 60 must be under the
   reclaim rate your budget buys.

   `fkgc.Phase()` stuck at 1 is the symptom, and it is silent. There is a
   forward-progress deadline that eventually gets a guest out — it is an
   unbudgeted step, i.e. a pause — and `fkgc.Stats().Deadlines` counts it. The
   expected value is zero forever, but zero is ALSO what you see while
   livelocked and still short of the deadline, so it is not on its own evidence
   of health.

   **WHAT HAPPENS WHEN THE PACER IS OUTRUN: THE HEAP GROWS. It does not
   pause.** This is the behaviour sharding stage C replaced the valve with, and
   it is the whole product position in one sentence — a collected guest under a
   storm degrades to `-gc=leaking` and recovers on the ticks after the storm.

   The valve it replaces was a full synchronous mark and sweep, run from inside
   the allocation that failed, per failing allocation, because the heap could
   not grow past the cap and the alternative was a trap. About **1.4 s at a time
   in a real Factorio, inside an event handler**. With no cap that trade is
   simply gone: an allocation that finds no span sweeps ONE bounded bite — one
   paced step's worth — and then grows.

   Measured (`TestAnAllocationStormGrowsInsteadOfCollectingSynchronously`): a
   burst of 96 KB/step against a budget that reclaims ~16 KB of it grows the
   heap to 13.9 MiB over 120 steps, with the worst collector burst inside any
   one guest call at **1.16× the step budget**; when the burst stops the paced
   collection finishes on its own and the heap does not grow again.

   The costs, stated: linear memory never shrinks, so a storm's growth is
   permanent for the session at about 0.2 ms of worst tick per MiB.
   `fkgc.Stats().Outruns` counts the grows that happened while a collection was
   running, and a line is logged once per guest through `env.fk_log` the first
   time it happens. A collected guest imports `env.fk_log` whether or not it
   logs; `control.lua` binds it unconditionally.

   The one place a collection still runs inside a guest call is when
   `memory.grow` itself is REFUSED — the runtime said no, or the address space
   ended — and there the alternative is not a pause, it is a dead mod.

   **What must not be done across a step**, and it is a rule about the HOST
   boundary rather than about the collector: a `(ptr, len)` handed out of the
   guest is guest heap, and the conservative scan cannot see the host holding
   it. It must be consumed before the call returns. `fk_mod.lua` and
   `fk_abi.lua` do; a harness that buffered logged pointers and read them later
   got mojibake the moment the collector really reclaimed. See gc.md's
   safe-point precondition.

   Everything below is what an author has to do without it — and what a
   mass-builder has to do anyway.
1. **Allocate less, and allocate it once.** Under `-gc=leaking` — still the
   default, and still mandatory for a wasip1 guest — every allocation is
   permanent, so a per-event allocation is a leak with a schedule. Parallel slices over slices-of-structs; ids over retained edge
   lists; one filter list built at subscribe time rather than per subscription.
2. **Stay under a doubling boundary.** The step from 33 MiB to 34 costs 6.6 ms
   of worst tick, because it is really the step from 32 MiB to 64. Knowing which
   side of the ladder you are on is worth more than any micro-optimisation, and
   the log line above is how you find out.
3. **`--persist` is not a lever here and never was.** It changes the save and the
   join, which are worth choosing for; it does not change the object the
   collector walks.

### The mitigations that were measured and lost

Recorded so they are not re-proposed. All numbers 2026-08-01, `bin/lua52f` for
the representation work and Factorio 2.0.77 for the pacing.

- **GC pacing — dead PRE-SHARDING, and superseded.** `collectgarbage` **is**
  present in Factorio's sandbox with every 5.2 option (`count`, `step`,
  `collect`, `isrunning`, `setpause`, `setstepmul`), which the day-0 probe had
  never checked; it does now. Against a 64 MiB heap the worst tick was 12.69 ms
  at the defaults, 12.57 with `setpause=1000`, 12.74 with `setstepmul=1000`,
  12.65 with `setstepmul=40`, 12.51 with both — a spread narrower than the same
  configuration's run-to-run noise. There was nothing to pace: the pause was one
  object's traversal, and pacing parameters choose how many objects a step
  covers.
  **THE PREMISE IS GONE AND THIS ROW IS KEPT AS HISTORY.** Linear memory is a
  vector of shards now, so there are many gray objects and pacing does
  something — **~10%**, measured above in this same file (`w26p` against `w26a`:
  0.93× worst, 0.89× p99, 0.87× mean). Two measurements a milestone apart, both
  correct for their own world; the sharded one is current. A reader who finds
  only one of them has the wrong half, which is what this note is for.
- **Wider slots (two words per Lua slot) — arithmetically impossible, and only
  2× if it were.** A double carries 53 exact bits, so it cannot hold two u32s;
  the honest version packs 52 bits and stops being word-addressable. Halving the
  slot count was measured anyway, as an upper bound on the prize: 3.15 ms →
  1.21 ms at 32 MiB. A 2.6× tail reduction, for a decompose on every load and a
  recompose on every store.
- **Sharding the live memory — wins enormously on the tail and loses on
  everything else.** As tables of 4,096 words the worst step at 128 MiB is
  **0.095 ms against 25.85** (272×) and, unlike everything else here, it is
  **flat in heap size** — the tail becomes a property of the shard, not of the
  guest. Full-cycle throughput costs 15–45%, which is affordable. What kills it
  is the access: every load and store becomes an index, a division, a modulo and
  a second table index. Measured on the shapes the emitter actually emits, with
  loop overhead subtracted — flat load 4.8 ns sequential / 10.2 ns strided
  against sharded 26.3 ns either way, and 14.4 ns even when the shard reference
  is hoisted out of a linear walk. **3–5.5× on the single most common operation
  in generated code**, to fix a tail that only exists at heaps a guest should not
  have. It stays written down rather than built: if the tail ever has to go for a
  guest that is heap-huge and compute-free, this is the design, and it belongs
  behind a flag with the access cost on the label.

---

## Running one in a real Factorio

```sh
./scripts/run-guest.sh          # TICKS=120, OPT=2 by default
OPT=3 ./scripts/run-guest.sh    # every optimization level has to load and run
```

**A `--create`d freeplay map STALLS single player at tick 750, and it is not a
pause anyone pressed.** `base/script/freeplay/freeplay.lua` shows the intro
message through `game.show_message_dialog` when the crash-site cutscene ends —
in single player only; a server takes the `player.print` branch — and a modal
dialog pauses the game until somebody clicks it away. Measured 2026-08-07: 60
ticks/s to tick 750, then 0.00, in every window-focus state, with the process
rendering normally at 10–15% CPU. A headless harness never sees this, which is
exactly how it masqueraded as a focus problem; any harness that loads a
freeplay map into a GRAPHICAL single-player session with nobody at the keyboard
must skip the intro first — `remote.call("freeplay", "set_skip_intro", true)`
from any mod's first tick, a no-op on non-freeplay maps.

Builds `examples/hello`, packages it, creates a throwaway map with only that mod
enabled, benchmarks it for N ticks and greps the guest's log lines back out.

This is the only check that the mod actually **loads**. lua52f models the
sandbox and the end-to-end test drives control.lua against stand-ins for the
game globals, but neither is Factorio: the mod format, `require` resolution and
the log plumbing are all outside what the oracle can speak to.

One difference to expect between the two: **Factorio's first tick is 0**, the
harness's is 1, so every count from a real run sits one ahead.

`fklua mod --zip` produces the distributable form, and Factorio loads that too —
verified the same way, with the zip dropped into a `--mod-directory` instead of
the unpacked directory. `require` resolves inside the archive unchanged.

---

## Rust — **the guard, the guest, and the collector**

`rustup` is installed (rustc 1.97.1, target `wasm32-unknown-unknown`), and
`internal/guest/rust.go` now guards rustc's feature surface the way
`toolchain.go` guards TinyGo's. What it found on its first run is why the
deliverable insisted the guard land *with* Rust rather than after it.

### rustc enables three features FkLua cannot compile

Measured, not read off a changelog:

```
$ rustc --print cfg --target wasm32-unknown-unknown | grep target_feature
bulk-memory          <-- NOT supported (planned M10)
multivalue           <-- NOT supported, not on the roadmap
mutable-globals
nontrapping-fptoint
reference-types      <-- NOT supported, not on the roadmap
sign-ext
```

A stock `cargo build --target wasm32-unknown-unknown` therefore produces a
module FkLua turns into raising stubs: it warns, compiles, and the mod loads and
dies whenever that path is reached.

### The flag does not do what it looks like it does

**`-C target-feature=-bulk-memory` does not remove `memory.copy`.** This is the
load-bearing difference from TinyGo and it took a measurement to see:

- TinyGo compiles everything it links from source with the target's feature
  string, so guarding the string is sufficient.
- Rust ships **precompiled** `core` and `compiler_builtins`. `RUSTFLAGS` governs
  the current crate's codegen only. `copy_nonoverlapping` lowers to a call to
  `memcpy`, which comes from an rlib built with bulk-memory on — so a `no_std`
  crate built with the feature explicitly disabled *still contains*
  `memory.copy` and `memory.fill`.

So the declared feature set says what LLVM **may** emit and the shipped rlibs
decide what it **does**. A guard that read only the feature set would bless a
module that raises at runtime.

### The recipe, both halves necessary

```sh
RUSTFLAGS="-C target-feature=-bulk-memory,-multivalue,-reference-types"   cargo build --release --target wasm32-unknown-unknown
wasm-opt --llvm-memory-copy-fill-lowering -o guest.wasm target/.../guest.wasm
```

`wasm-opt` is already mandatory for a TinyGo guest, so the second line is a step
rather than a dependency. Verified end to end: without it `fklua compile` warns
on `memory.copy` and `memory.fill` and stubs two functions; with it the module
compiles silently and the lowered copy/fill produce the right bytes.

`TestRustBuildRecipeRemovesWhatItClaims` asserts all three legs — that the flag
alone leaves the feature in place, that the pass removes it, and that the result
has no unsupported instruction left. The first of those is asserted rather than
assumed on purpose: if it ever stops being true, the pass is dead weight and the
reasoning above is wrong.

### The `fk` crate, and the corpus running under both

`guest/rust/` is a cargo workspace: `fk` is the substrate and `examples/hello`
mirrors `guest/go/examples/hello` **line for line**. That mirroring is the point
— M8's gate is that the corpus produces *identical* results under both
toolchains, and two programs that merely both worked would not test what the
milestone exists to test.

`TestBothToolchainsAgree` builds both, compiles both, runs both under lua52f and
compares byte for byte. Only the two lines naming their own toolchain are
normalised, and deliberately not skipped — dropping them would also drop the FNV
hash, the one value in the run computed by 64-bit arithmetic:

```
hello from LANG, running as Lua inside Factorio
guest built with LANG: fnv64(fklua)=449d63cef97b1fda
tick 30 seen=30 fizz=8 buzz=4 fizzbuzz=2 sum=465 mean=15.50
```

`LANG_=rust ./scripts/run-guest.sh` runs the same guest in real Factorio, where
it loads, produces the same numbers, and is identical across two benchmark runs.

Three choices in the crate worth knowing:

- **`no_std` + `alloc`.** `std` for `wasm32-unknown-unknown` assumes an OS this
  target does not have; `alloc` is what a guest actually wants and needs only a
  global allocator.
- **A bump allocator that never frees by default**, matching TinyGo's
  `-gc=leaking` and for the same reason — a collector's pauses land in a lockstep
  loop where one client stalling desyncs everyone. **`--features fk/fkgc` swaps
  it for `guest/rust/fkgc`**, a paced conservative mark-sweep collector; the
  `#[global_allocator]` site stays in `fk` either way, so a module can never link
  both. It is also deterministic by construction,
  which is the property that actually matters: every client must reach identical
  addresses, and a free-list allocator whose layout depended on drop order would
  not.
- **`panic=abort`, and the handler logs before it traps.** A panic cannot unwind
  out of wasm into Lua. A bare `unreachable` arriving in the game tells an author
  nothing, so the location goes to `factorio-current.log` first — without
  formatting, since the allocator may be the thing that just failed.

### The generated bindings

`fklua gen-bindings` writes **both** languages and `--check` checks both — the
same reasoning that put the census in that command, since two artifacts behind
separate commands is how one goes stale.

**Each set carries a PIN STAMP, so a guest built against the wrong one is
refused rather than shipped.** It is one exported function named
`fk_api_pin_<version>` — `//go:wasmexport` here, `#[no_mangle] pub extern "C"`
in Rust — and it is why a guest's export list now has an entry no hook
corresponds to. Exported rather than called because an export is a root:
`-opt=2` followed by `wasm-opt`, and Rust's `lto = true`, delete anything that
is merely defined. `fklua mod` compares it against the pin it is packaging at
and refuses a mismatch. The arrangement that produces one — a vendored checkout
whose committed bindings are at the default while the consumer pins something
else, which is what `fkipc` imports — and the `--into` flow that fixes it are in
[`agents/versioning.md`](agents/versioning.md), "The pin stamp, and repinning a
vendored checkout".

**4255 of 4257 members in each language at the default 2.0.77 pin, with the same
2 deferrals.** Agreeing
member-for-member is the point: it says the shared machinery is not Go-shaped.

Where Rust says it better, the binding says it:

| | Go | Rust |
|---|---|---|
| status | `(T, error)` | `Result<T, Status>` |
| optional | `*T` | `Option<T>` |
| optional CONTAINER | a slice, `nil` meaning absent | `Option<Vec<T>>` |
| several returns | several returns | a tuple |
| string argument | `string` | `&str` — the host copies during the call, so ownership would make every caller allocate for nothing |
| dictionary | an ordered slice of `Entry` pairs | `BTreeMap<K, V>` — ordered, therefore deterministic |
| tier-2 value | a struct with 6 fields, 5 always dead | an `enum` |

`Object` derives `Ord` so it can key a `BTreeMap`; five members return a
dictionary keyed by a handle.

**A DICTIONARY IS NEVER A GO MAP, and that is a determinism rule rather than a
taste.** Go randomizes map iteration per process and Factorio is lockstep, so a
guest that walks one does host-visible work — and, under every `--persist` mode
but `none`, ALLOCATES, which the save records — in a different order on every
client. Reported by fklua-ports' qol-research (Q3) with `game.forces` and
`force.technologies` side by side, one an ordered pair slice and the other a map,
one generator line apart. Both are the slice now; a guest that wants lookup
builds a map from it and owns the decision. Rust needs no equivalent: a
`BTreeMap` walks in key order.

**AND A DICTIONARY'S KEY IS THE ONE `pairs()` PRODUCES, which is not always the
one the type declares.** `game.surfaces` is `dictionary[uint32 | string ->
LuaSurface]`; a union key, so it crosses as `Vec<(Value, Object)>` /
`[]EntryValueObject` with a tier-2 key. **`pairs()` over the engine's own
name-or-index dictionaries yields the NAME**, and the numeric key the type
declares exists in Lua but `pairs` never produces it — so a guest filtering on
`Value::Number` / `TagNumber` matches nothing, with no error, no status and no
log line. `fk_abi.lua` walks a dictionary return with `pairs()`, and that is
where it comes from. Read the index off the HANDLE (`LuaSurface::index`) if that
is what you want. fklua-ports' resource-marker reported it as **RM2** after a
whole-world pass printed `surfaces=0 forces=0`; the generated doc comment on
every union-keyed member now says it at the call site.

**Class operators are ordinary members.** `inv[1]`, `#inv`, `t[name]` and
`it()` — Lua's `__index`, `__len` and `__call` — are bound as `Get(k)`,
`Length()` and `Call(...)` in Go and `get`/`length`/`call` in Rust, on the seven
classes that declare one. `Get`, not `Index`, because LuaInventory and
LuaGuiElement each declare an ordinary attribute already called `index`. An
INDEX operator's key is a `u32` where the class also answers `#` (a Lua sequence
indexes by position) and tier 2 where it does not, which is `LuaCustomTable` —
genuinely keyed by string at `force.technologies` and by number at
`game.players`. **Reading one entry of a `LuaCustomTable` is one host call now**,
where the whole-table attribute cost ~24 KB of guest heap per read.

**Every generated struct has a `ToValue()` / `to_value()`.** A UNION-typed struct
field has no fixed layout, so it crosses as a tier-2 value and a guest was
writing the Lua table out by hand — `LogisticFilter.value` needs `type`, `name`
and `quality` as string keys that nothing checks, and a typo is a filter the
engine silently rejects. The typed struct already exists; this is the way to
spend it:

```rust
value: Some(SignalID { r#type: Some(String::from(SIGNAL_ID_TYPE_VIRTUAL)),
                       name: Some(n), ..Default::default() }.to_value()),
```

An absent optional is OMITTED from the table, which is what an absent optional
means everywhere else in this ABI. It is not a typed union: 52 concepts are
structural unions and generating a tagged type per union is the trap tier 2
exists to avoid.

**The API's string enums are constants.** 41 concepts are a union of nothing but
string literals — `WaitConditionType` is 26 of them — and they crossed as a bare
string with the names discarded, so `"inactivty"` compiled, packaged, loaded and
produced a schedule the engine rejected at runtime. `WaitConditionTypeInactivity`
in Go and `WAIT_CONDITION_TYPE_INACTIVITY` in Rust, untyped/`&str` so every call
site that already passes a literal keeps compiling.

**What Rust's checker caught that Go's would not**, all in the generator rather
than in hand-written code: a `&Value` where an owned one was in hand and the
reverse; `put_str` wanting `&str` where a struct field holds `String`; `*e.0`
parsing as `*(e.0)` and dereferencing the u32 instead of the handle; `&mut d`
double-borrowing a slice that is already `&mut [u8]` (the fix is `&mut d[..]`,
which is the one spelling correct for both an array and a slice); and `extern
fk_alloc` needing its safe wrapper at every call site. The Go backend's
equivalents only ever surfaced by building with TinyGo.

**Exercised at runtime, not merely compiled.** `guest/rust/examples/array`
mirrors the Go one, and `TestArraysCrossInBothDirections` runs *both* against
the same host stub with the same expectations — a runtime check of the generated
bindings and a differential check in one. All eleven lines match, including the
tier-2 nesting and the variant-parameter-group `create_entity`.

It earned that immediately. The imports were emitted as `fk.fk_call` and
`fk.fk_subscribe` rather than `fk.call` and `fk.subscribe`, which **a compile
gate cannot see**: the crate built perfectly and the module refused to
instantiate. `#[link_name]` keeps the Rust identifier readable while the import
keeps the name `control.lua` binds.

`TestGeneratedRustBindingsCompile` still earns its place alongside it — it
type-checks all 4255 members at the default 2.0.77 pin, where a guest covers
only what it calls.

### Could TinyGo take advantage? Measured: not yet worth it

TinyGo's `wasm-unknown` target explicitly disables bulk-memory
(`-bulk-memory` in `features`, `-mno-bulk-memory` in `cflags`), and unlike Rust
it compiles its runtime and libc from source with those flags — so flipping it
is a complete custom target JSON away, dropped in `$TINYGOROOT/targets/`
(`inherits` MERGES cflags rather than overriding them, so a partial target
errors on duplicates, and `extra-files` resolves relative to TINYGOROOT, so the
file has to live there).

Done, the hello guest builds and behaves identically, and:

| | stock | +bulk-memory |
|---|---|---|
| wasm | 105,480 B | **96,708 B** (−8.3%) |
| generated Lua | 159,643 B | **149,277 B** (−6.5%) |
| µs/tick under lua52f | 22.95 | 23.14 (**0.99×**) |

The instructions land in exactly the places Go code lives in —
`runtime.hashmapSet` (9 of the 17), `stringConcat`, `sliceAppend`,
`hashmapGet`, `strconv.FormatUint`, `stringFromBytes` — and it still made no
measurable difference. The reason is that the 49× is **per byte**: hello's
copies are a map entry and a short string at a time, tens of bytes, where the
call overhead dominates and the throughput win has nothing to bite on.

**Redone properly, and the first answer was a bug in FkLua rather than a fact
about TinyGo.** The original attempt was invalid — the destination was never
read and LLVM deleted the copy. With a checksummed destination the copy
survives, and bulk-memory measured **3.5× SLOWER**, which is not a believable
thing for a word-wise path to be.

It was not word-wise. Instrumenting `mem_copy` said so exactly:

```
calls=66 bytes=1048584 fast=1 ragged=65
ragged because: d%4=1 s%4=0 n%4=0
```

TinyGo's allocator handed out a **destination at 1 mod 4** against an aligned
source, so 64 of 66 copies missed the fast path. The ragged path was a byte
loop, and in a word-table memory each byte is `ld8raw` (shift and mask) plus
`st8raw` (read-modify-write of a whole word) — about thirteen Lua operations to
move one byte, against TinyGo's own memmove which the emitter compiles
word-wise.

So the ragged path now aligns the destination by byte, then writes whole
destination words — assembling each from two source words with a shift when the
source alignment does not match:

| | ns/byte |
|---|---|
| aligned fast path | 4.11 |
| misaligned, before | ~100 |
| misaligned, after | **8.01** |

And the guest that started this: **5.78× faster with bulk-memory on**, where it
had been 3.5× slower. `TestBulkCopyIsCorrectAtEveryAlignment` walks all 16
alignment pairs, every length to 40 and overlap at every offset, reading back
the bytes around the copy as well as inside it; removing the head alignment
makes it report 10,756 wrong bytes.

**Whether to ship the custom TinyGo target is open again**, and it now turns on
the guest rather than on a defect: 5.78× for one that moves kilobytes, 0.99× for
`hello`, plus a real size win (wasm −8.3%, Lua −6.5%) and the packaging problem
described above.

## wasip1 — **built. Goroutines work.**

```sh
tinygo build -target=wasip1 -buildmode=c-shared -o guest.wasm ./examples/goroutine
```

`guest/go/examples/goroutine` runs goroutines, channels and a producer/consumer
pipeline, **verified in Factorio 2.0.77** and identical across two benchmark
runs. It spawns one every tick, not just at init: asyncify has to unwind and
rewind cleanly on each entry, and a state machine that only worked the first
time would pass an init-only test.

This works because asyncify rewrites the module into a resumable state machine
**inside the wasm**, so FkLua needs no host coroutines — which Lua 5.2 does not
have. Nothing in the compiler was needed for it.

### `-buildmode=c-shared` is not optional

wasip1 defaults to building a **command**, which exports `_start`: it runs main
and terminates, and calling an export afterwards is out of contract. A mod needs
a **reactor**, exporting `_initialize`. The symptom is the guest's own runtime
saying `//go:wasmexport function called before runtime initialization`, which
reads like an ordering bug in the host and is not.

### The WASI shim is three imports

`fd_write`, `proc_exit`, `random_get` — the whole surface unless a guest opens a
file, and there are no files here.

**`random_get` is determinism, not entropy**, and this is the one that would
have shipped a multiplayer-only bug. Factorio is a lockstep simulation: every
client must compute identical bytes, so a real random source would desync the
game and pass every single-player test. It is a counter-based PRNG whose state
lives in `storage`, identical on every client and carried across a save. A guest
wanting unpredictability must ask the *game* — `LuaRandomGenerator` is seeded
from the map and is the only source that is both varied and synchronised.

`proc_exit` traps rather than returning: taking the mod down loudly beats
letting a guest carry on past a point it believed was terminal.

### The bug wasip1 found in the persistence layer

**A grow was never written back to the save.** `storage.fk_memsize` was set once
at `on_init` and never again, so a guest that called `memory.grow` ran correctly
for the whole session, saved the OLD size, and then trapped on the next load —
inside guest code, on a tick that had worked a thousand times.

Nothing before wasip1 grew. `-gc=leaking` never returns memory and never asks
for more than its initial pages, so the bug sat in `sync_memory` from M6 until
the first guest with a precise GC arrived. It reproduced only in Factorio, never
under the oracle, because the oracle never saves and reloads.

### The cost is real

The goroutine guest averaged **27.5 ms/tick** over 30 ticks against `hello`'s
fraction of a millisecond — asyncify's control-flow rewrite plus a precise GC
plus a goroutine per tick. wasip1 is the right target for a guest that wants
concurrency and the wrong one for a guest that wants a tick budget.

## C

**M8, and not started.** Apple clang has no wasm32 target
(`No available targets are compatible with triple "wasm32-unknown-unknown"`), so
this needs `wasi-sdk` or Homebrew LLVM, neither of which is installed here.

### Recompiling, and `fk_migrate`

A save records the **build id** of the module that wrote its heap — stamped into
the generated chunk at package time. Hashing the whole input rather than just the
data segments is deliberate: a change anywhere in the guest can move how the heap
is *interpreted* even when the segments are byte-identical, and being wrong in
that direction corrupts a save rather than merely resetting one.

**A BUILD IS THE MODULE *AND* THE `--api` PIN IT WAS PACKAGED AGAINST**, and it
was the module alone until 2026-08-07. The stamp is

```
sha256( "fklua/build-id/v2\0" || sha256(wasm) || <resolved api version> )
```

truncated to 8 bytes, and the fold is unambiguous by the **width of its fields**
rather than by a separator: the domain tag is a constant, the module contributes
exactly 32 bytes at a constant offset whatever it contains, and the pin is
everything after — so there is one way to read the preimage back into (module,
pin), and a wasm that happens to *contain* the version bytes cannot move any
boundary, because the module's bytes never appear in this preimage at all. Only
its digest does. `sha256(wasm || sep || pin)` has no such property for free: its
boundary is found by scanning content, so its injectivity rests on an assumption
about which field may contain `sep` — and the pin is user-supplied.

The pin belongs in there because **the package carries pin-derived facts the
HEAP depends on.** `API.event_scratch` is the largest subscribed event's payload,
computed from the *packaged* event table, and it is the size a cached buffer
sitting in the guest heap was allocated at; a define id the guest resolves once
and caches is a per-build number in the same heap. And member, event and define
ids are dense sorted indices over one version's set, so a pin change shifts them
as a **class** — which is why this is unsound in general and not only in the two
places it can be pointed at. So one wasm packaged against two pins used to be two
mods with one identity, and `same_build()` adopted a heap straight across them.

One consequence to state rather than discover: the construction changed the value
for *every* build, including one at the default pin, so the first repackage after
it landed looks like a rebuild to a save written before it. That is a one-time
reset down the designed path below, and it is unavoidable — any stamp that can
tell two pins apart necessarily differs from one that could not.

`on_load` runs **before** `on_configuration_changed`, so the decision to adopt
the old heap is made on the build stamp alone. That is what keeps a multiplayer
join deterministic — every client has the same stamp and makes the same call.

| Situation | What happens |
|---|---|
| Same build | The heap is adopted. Ordinary load. |
| Changed, guest exports `fk_migrate` | The heap is **fresh** — what `_initialize` just built. `fk_migrate(old_version)` is called to *tell* the guest, which is what a rebuild-from-world needs. |
| Changed, guest exports `fk_migrate_adopt` | The old heap **is** adopted, rodata and all, and `fk_migrate_adopt(old_version)` is called. Opt-in, and read the hazard below before exporting it. |
| Changed, neither | The old heap is never adopted. The guest starts from its data segments and the loss is logged, naming both build ids. |

"Changed" reads the stamp, so **repackaging the same wasm against a different
`--api` pin is a change** and lands in exactly those rows — which is the correct
reading of shifted ids, not a side effect. An author who sees an unexpected reset
after a version bump gets both stamps out of the log line; the `API <version>:`
lines `fklua mod` prints say which pin each one was.

#### WHEN those rows happen, which is not when you would guess

The three "changed" rows used to be reached from `on_configuration_changed`
alone, and **Factorio does not raise that hook for a rebuild that keeps the mod's
version** — it raises it when the mod SET changes. A stamp moves for a dev
rebuild, a `--gc` or `--persist` change, or a repackage against another `--api`
pin, and not one of those touches `info.json`. So through two milestones the
commonest rebuild there is reached none of the rows: `state_load` declined the
heap and *nothing finished the job*. `storage.fk_mem` was never republished, so
the save kept the previous build's heap while the guest ran on the fresh one —
the guest's writes reached neither the save nor the multiplayer CRC; the stamp
was never republished, so every later load declined again; and **your migrate
hook was never called.** On a multiplayer join it desynced from the first joined
tick, silently, because the warning lives in the hook that did not fire.

Fixed 2026-08-07. The decision is still made at load, on the stamp alone; the
ACT now happens at the first **replicated** execution point after it — the first
outermost dispatch, which is before any of your guest's own code runs — with
`on_configuration_changed` kept as the earlier opportunity when the version did
move. **What this means for a guest author:** your `fk_migrate` fires on a dev
rebuild now, where before it fired only if you also bumped the version. If your
hook is expensive, that is a real change in when you pay for it; if your hook is
a rescan-from-world, it is the behaviour you thought you already had. The one
case that still reaches nothing is a guest that never dispatches at all — no
tick, no event, no command — which also never writes a word of its own memory,
so there is nothing for it to lose.

**And the converse gap was open until 2026-08-16**: these rows are about *your*
stamp, so a mod set change that leaves your build alone reaches none of them
either, and for two milestones nothing else was registered on that hook. If what
you want is "a neighbour was uninstalled", that is
`fk_on_configuration_changed` and not this — see "Noticing that the MOD SET
moved" above.

### `fk_migrate` does not adopt, and that split is the fix for a real hazard

`fk_migrate` used to mean "hand me the old build's memory and I will fix it up
in place", and **no guest could actually do that**. Adopting replaces the
module's *entire linear memory* with the saved one — and linear memory is not
just the heap. It is `.data` and **`.rodata`**. A rebuilt guest refers to its
string constants, its type descriptors and its static buffers by **compiled-in
address**, and after adoption every one of those addresses points at whatever
the previous build put there. So the first line of `fk_migrate` was already
undefined: `OfString(someConstant)` sends the host bytes out of somebody else's
rodata. The hook offered a choice between losing state silently and corrupting
it silently, and the first downstream consumer exported neither and rebuilt from
the world instead.

The two acts are now separate exports, and the safe one keeps the obvious name.
`fk_migrate` is a **notification on a fresh heap**; `fk_migrate_adopt` is the
opt-in that really hands the bytes over, for a guest whose state is a fixed
versioned region it interprets itself. *Enforced by
`TestMigrateIsToldAboutTheRebuildAndGetsAFreshHeap` and
`TestMigrateAdoptReallyGetsTheOldHeap`, which are the same guest under two
export names.*

**The rodata hazard is inherent to adoption, not fixed by the split.** A guest
that exports `fk_migrate_adopt` still runs against the previous build's
constants. The obvious repair — re-apply the NEW build's data segments over the
adopted memory — is recorded and *not built*, because it is not obviously
correct either: `.data` holds a Go or Rust guest's package-level variables,
which are the roots reaching everything on the heap, so resetting them leaves
the heap intact and unreachable. The shape that would work is a guest that keeps
its migratable state at a **known, versioned offset it does not need a root to
find**, and that is exactly the guest `fk_migrate_adopt` is for.

The default is the safe one on purpose. Losing state cleanly beats running a
guest on bytes laid out by a different build: in a lockstep game that is not
"slightly wrong data", it is every client desyncing on whatever the garbage
decodes as.

`fk_state_version() -> i32` is the guest's own format version. It is stamped
into the save alongside the build id and handed back to `fk_migrate`, so a guest
that has changed its layout three times knows which of the three it is looking
at. A guest that does not export it gets 0.

### Why a TinyGo guest should almost never export `fk_migrate_adopt`

Exporting it changes what `on_load` does: the old heap **is** adopted, because
the guest asked for it. For a Go guest that is more dangerous than it sounds —
and that is on top of the rodata hazard above, which applies to every language.

A Go guest's persistent state is not a struct at a known address. It is a map, a
slice header, an interface table — objects the allocator placed, reachable only
through pointers that also live in that heap. And the heap carries **TinyGo's own
allocator state** alongside them. After a rebuild every one of those addresses
may have moved, so the adopted heap is not "the old data in a slightly different
shape", it is a heap whose allocator bookkeeping describes a layout the running
code no longer has. Running Go code against it can corrupt memory long before it
produces a wrong answer.

So the default — discard, start clean, log it — is right for essentially every
TinyGo guest, and `examples/hello` deliberately does not export the hook. A Go
guest that wants to KNOW a rebuild happened exports `fk_migrate` and gets told,
on a heap it can trust.

`fk_migrate_adopt` is for a guest that keeps its migratable state somewhere it
controls: a fixed, versioned region it reads and writes explicitly, with a
layout it can interpret across builds, rather than in ordinary language objects.
That is a real pattern and the hook exists for it — but it is opt-in for a
reason, and a Go author who exports it without that discipline has made their
mod's save corruption their own problem rather than avoided it.

**What DOES survive without any of this**: everything, as long as the build is
unchanged. `--persist=table` carries the whole linear memory, so a Go map and a
growing slice come back exactly as they were. `scripts/run-roundtrip.sh` proves
it in a real Factorio save cycle.

### A LOG LINE IS API SURFACE

`fk_mod.lua`'s `log()` calls are the only channel this runtime has for telling a
mod author something went sideways, and downstream test suites grep them —
`run-guest.sh`'s own gate does. **So a changed log line is a downstream break,
not a wording tweak**, and it earns the same treatment as a renamed export:
change it deliberately, keep the identifying prefix stable, and say so in the
commit.

The first downstream consumer hit this on the `fk_migrate` redesign. The message
about a rebuilt mod still begins `fklua: this mod was rebuilt`, but *when* it
fires moved — a guest exporting `fk_migrate` is now dispatched rather than
logged, because the hook stopped meaning "adopt" — and a suite asserting on the
line was asserting on the old branch condition without knowing it.

Two rules follow:

- **The prefix is the contract.** Every line starts `fklua: ` and then a stable
  opening clause; the detail after it (build ids, field names, which event) is
  free to change. Match on the opening clause, never on the whole line.
- **A line whose CONDITION changes is a behaviour change even when the text does
  not.** That is the case above, and it is the one no diff of the string will
  catch. When a hook's semantics move, look for what used to log and no longer
  does.

The lines this runtime emits today, by opening clause: `this Factorio has no
event …`, `the filter passed to fk.subscribe for … could not be read`,
`fk.subscribe asked to skip …`, `this event takes no filters`, and `this mod was
rebuilt`.
