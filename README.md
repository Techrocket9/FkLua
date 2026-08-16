# FkLua

FkLua compiles WebAssembly ahead-of-time into Lua 5.2 source and packages the result as a
Factorio mod. You write the mod in Go (via TinyGo) or in Rust, compile it to a wasm module
(the **guest**), and `fklua mod` turns that module into a directory Factorio loads like any
other mod: a generated `control.lua`, the runtime that hosts the guest, and the slice of the
Factorio API bindings the guest actually calls. The name is short for "Factorio, kein Lua".

```sh
fklua init my-mod          # a project, a guest, a collector, a manifest
fklua gen-bindings         # the Factorio API, for your language
fklua mod my-mod.wasm      # a directory Factorio loads
```

Factorio only loads Lua, and since the 2024 sandbox hardening it only loads Lua *source*:
`load()` rejects binary chunks, which ruled out earlier bytecode-emitting compilers. FkLua
emits ordinary, readable Lua source that Factorio parses like any hand-written mod. There is
no LLVM dependency: WebAssembly is the input format because it arrives already legalized
(structured control flow, four value types, explicit linear memory, a published ABI), and
anything that emits wasm can target it.

Both guest languages are supported at parity: the whole Factorio runtime API is bound member
for member in Go and in Rust from one API description, with events, `defines`, commands and
remote interfaces, and each has a guest-heap garbage collector, save/load persistence, and
an IPC library for talking to a process outside the game. Real mods have been built on both,
run in real Factorio games, and benchmarked against the hand-written Lua mods they replace.

---

## Status

FkLua is pre-release: there are no versioned releases yet and the CLI and the guest
libraries may change without notice. It is MIT licensed (see [License](#license)). Three
downstream projects build on it:

| Project | What it is |
|---|---|
| [BetterBeltBalancer](https://github.com/Techrocket9/BetterBeltBalancer) | A Factorio 2.0 mod written in Go and compiled with FkLua. Balancer parts are 1x1 tiles that form one balancer; the mod compiles each balancer into real splitters on a hidden surface, so no per-tick Lua runs. FkLua's first serious downstream user. |
| [fklua-ports-samples](https://github.com/Techrocket9/fklua-ports-samples) | Six open-source Factorio mods ported to FkLua guests (three Rust, three Go) as a validation campaign for FkLua's API coverage, with the findings ledgers that campaign produced. A showcase, not maintained. |
| [Vibetorio](https://github.com/Techrocket9/Vibetorio) | Voice-driven LLM control of Factorio. A macOS companion app with hold-to-talk speech recognition and a local model drives the player's character through a tool API served by a Go/FkLua mod over FkIPC. |

---

## Prerequisites

| | For what |
|---|---|
| **Go 1.26+** | building the `fklua` tool itself |
| **TinyGo 0.41.1** | a Go guest. TinyGo's `wasm-unknown` target, not standard Go (see [Guest languages](#guest-languages)) |
| **binaryen** (`wasm-opt`) | required by TinyGo, which shells out to it for every wasm target |
| **Rust 1.97+** with `wasm32-unknown-unknown` | a Rust guest (`rustup target add wasm32-unknown-unknown`) |
| **Factorio 2.0.x** | only to run what you built, and for this repo's in-game test scripts. Building needs neither the game nor the network: the API descriptions are committed under `api/<version>/`. The stable 2.0 release is the default target; 2.1.x is supported too (see [Factorio versions](#factorio-versions)), and FkIPC needs a 2.1.14 or newer engine |

`brew install tinygo binaryen` covers the middle two on macOS. You need one guest toolchain,
not both. `fklua doctor` reports each row as found or missing with the version it found, and
exits non-zero only when neither guest toolchain is complete.

---

## Quickstart

From a checkout of this repository. This is the Go path; Rust follows.

```sh
make                                # builds bin/fklua; put it on your PATH
fklua doctor                        # optional: is the toolchain complete?
cd .. && mkdir my-mod && cd my-mod
fklua init my-mod --guest-module /path/to/fklua
```

`init` writes into the **current directory** and creates no `my-mod/` of its own; the name
argument is the mod's identity. `--guest-module` points the scaffolded guest at a local FkLua
checkout; leave it off and run `go mod tidy` in `guest/go/` once the guest module is
fetchable where you are. It writes `fklua.toml` (the mod's identity, dependencies, API pin,
guest language and GC mode) and a guest that already builds under `guest/go/`: its own Go
module, the collector import in `gc.go`, and `fk_on_init` and `fk_on_tick` wired in
`main.go`. Then:

```sh
fklua gen-bindings && fklua lock          # the Factorio API lands at guest/go/fkapi/
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=custom -opt=2 -o ../../my-mod.wasm .)
fklua mod my-mod.wasm
```

`fklua mod` needs no flags; everything comes out of `fklua.toml`, and a flag is an override.
It prints the size of the Lua it wrote, the modes it used, and each guest export it wired to
a Factorio hook. Copy `my-mod_0.1.0/` into Factorio's `mods/` directory, or pass `--zip`.
Calling the API is an import away:

```go
import "my-mod-guest/fkapi"

speed, err := fkapi.Game.Speed()          // read
err = fkapi.Game.SetSpeed(speed * 2)      // write
```

and `fklua mod` then reports `API 2.0.77: 1 members, pruned from 4257`: the mod ships the
one member it calls. Every TinyGo flag above is required, and the scaffolded `main.go` says
why; `-opt=2` rather than TinyGo's `-opt=z` default is worth up to 1.7×, because `z`
optimises for size, which is not a cost here (Factorio parses 4 MB of Lua in about 106 ms).

### Rust

```sh
fklua init my-mod --lang rust --guest-module /path/to/fklua
fklua gen-bindings && fklua lock
(cd guest/rust && cargo build --release \
    --target wasm32-unknown-unknown --features fk/fkgc)
fklua mod guest/rust/target/wasm32-unknown-unknown/release/my_mod_guest.wasm
```

`init` scaffolds `guest/rust/` as a two-member cargo workspace, the generated `fkapi` crate
beside your guest, with `panic=abort`, `lto` and `opt-level="s"` already set. `--features
fk/fkgc` is the collector: no import and no second flag, because the `fk` crate owns the
single `#[global_allocator]` site. If a crate reaches a wasm feature FkLua does not compile
(`multivalue`, `reference-types`), `fklua compile` names it; the recipe for turning those off
is in [`agents/guests.md`](agents/guests.md).

---

## Where to go next

The scaffold uses the two simplest hooks: `fk_on_init` once per save and `fk_on_tick` every
tick. A real mod subscribes to events and decodes their payloads, and the worked example of
that is [`guest/go/examples/api/`](guest/go/examples/api/main.go) (Rust twin:
[`guest/rust/examples/api/`](guest/rust/examples/api/src/lib.rs)). It is one file:

| In the example | What it shows |
|---|---|
| `fkapi.Subscribe` / `SubscribeFiltered` from `func init()` | subscriptions run during `_initialize`, the one place they may go |
| a `fk_on_event` switch over `fkapi.EventXxx` | one export, every event, dispatched on a generated id rather than a hand-written number |
| `fkapi.ReadOnPlayerCreated(ptr)` | the payload as a generated struct, not a cast at an offset you derived |
| `fkapi.NameFilter("iron-chest")`, `fkapi.TypeFilter("container")` | Factorio's own filters, applied in C++ before the guest is entered |
| `surface.NameIs("nauvis")` | a host-side predicate: asks the question rather than copying the string into guest memory |
| `fkapi.LuaEntity{Object: e.Entity}` | a raw handle out of a payload, wrapped so its methods are callable |
| `fkapi.DefinesDirectionEast()` | a `defines` value asked for by name, because its number is per Factorio build |

`guest/go/examples/` holds nineteen more, each aimed at one thing: `array` and `dict` for
marshalling, `callback` for commands and remote interfaces, `retain` for a handle that
outlives its event, `gcsave` for the collector across a save, `migrate` for a rebuilt guest,
and [`ipc`](guest/go/examples/ipc/main.go) for [FkIPC](#fkipc-talking-to-a-process-outside-the-game).
`guest/rust/examples/` mirrors eight of them line for line.

---

## Verifying a mod headlessly

Factorio runs headless, so "does it load, and does it do the same thing twice" is a
scriptable check. Point `MOD` at what `fklua mod` wrote and `NAME` at your `[mod] name`. The
`FACTORIO` path and the `config.ini` source below are the macOS Steam locations; adapt both
to your install.

```sh
MOD=my-mod_0.1.0; NAME=my-mod
FACTORIO="$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio"

# A private write-data directory: Factorio locks its user directory, so a run
# sharing one with an open game dies at startup.
mkdir -p verify/mods verify/userdir/config
sed -e "s|^write-data=.*|write-data=$PWD/verify/userdir|" \
    "$HOME/Library/Application Support/factorio/config/config.ini" \
    > verify/userdir/config/config.ini

cp -R "$MOD" verify/mods/
echo "{\"mods\":[{\"name\":\"base\",\"enabled\":true},{\"name\":\"$NAME\",\"enabled\":true}]}" \
    > verify/mods/mod-list.json

# --create is where _initialize and fk_on_init run.
"$FACTORIO" -c verify/userdir/config/config.ini --mod-directory verify/mods \
    --create verify/map.zip --disable-audio > verify/create.log 2>&1

# --benchmark reloads that save and runs it, twice.
"$FACTORIO" -c verify/userdir/config/config.ini --mod-directory verify/mods \
    --benchmark verify/map.zip --benchmark-ticks 1200 --benchmark-runs 2 \
    --disable-audio > verify/run.log 2>&1

grep -E "Checksum for script|$NAME|[Ee]rror" verify/create.log
grep -E "$NAME|Performed|checksum:" verify/run.log
```

On the unmodified scaffold, that prints a script checksum, the `fk_on_init` line from the
create log, the `fk_on_tick` lines from the run log, and the same `checksum:` twice; two runs
disagreeing means a nondeterministic guest, which in a lockstep game is a desync. Two limits:
`--benchmark` never saves, so state that must survive a real save needs a headless server and
`game.auto_save()` ([`scripts/run-roundtrip.sh`](scripts/run-roundtrip.sh) is that shape),
and a headless `--create` has no player and no connected client, so any event that needs one
never fires. [`scripts/run-guest.sh`](scripts/run-guest.sh) is this recipe wired to the repo's
own example guests.

---

## The Factorio API from a guest

The whole runtime API is bound in both languages, member id for member id. Against the
default **2.0.77** API pin: 4,255 of 4,257 members, 219 event payload structs (every event the
description declares), 1,329 inherited forwarders (so `LuaEntity` has `LuaControl`'s
`position` and `get_inventory`), 1,137 `defines` accessors, 11 class operators, and 240
`<Name>Into(dst, …)` variants that let an array return land in a buffer you already own. Two
members are deferred, both a name that collides with another member of the same class. The
counts are committed data in `api/<version>/census.json`, regenerated with the bindings and
gated by `gen-bindings --check`; read them from there rather than from this page.

### Factorio versions

There are two version axes and they are worth keeping apart. The **API pin** is the
`runtime-api.json` version the bindings and the packaged member table come from; the
**engine** is the Factorio actually running, which a guest can ask about with
`helpers.game_version`. They meet in exactly one place: the packaged `info.json`'s
`factorio_version`, which defaults to the pin's `major.minor` (a 2.0 engine does not load a
mod declaring 2.1, and a 2.1 engine does not load one declaring 2.0) and can be overridden
with `[mod] factorio_version` in `fklua.toml` or `--factorio-version`.

The default pin is the general-availability release, **2.0.77**, because a default is what a
mod author who has pinned nothing ships to players, and players are on stable. Everything in
this repository builds and runs against a stock 2.0.x install; the in-game test scripts read
the installed engine's version and package for it. Two capabilities need more: **2.1.x**
API surface is one line away (`api = "2.1.14"` in `fklua.toml`, or `--api=2.1.14`, then
`fklua gen-bindings && fklua lock`; every supported description is committed, and at 2.1.14
the bindings cover 4,840 of 4,842 members with 224 events), and **FkIPC** requires a 2.1.14
or newer engine and is inert below it (see [FkIPC](#fkipc-talking-to-a-process-outside-the-game)).
On Steam, 2.1.x is the `2.1.14` entry under the game's Betas tab; the scripts pick it up
through `FACTORIO_BIN` or the default Steam path. The two migrations this project has done
between pins are written up in [`agents/versioning.md`](agents/versioning.md).

- **One generic `fk.call(handle, member, argp, retp)` import**, not one per method. A method
  Factorio removes in a point release would otherwise be an unresolved import, which fails
  the whole module at instantiation; here it degrades to one call returning `ERR_NO_MEMBER`.
- **A mod ships the members it calls, not the API.** `fklua mod` scans the compiled guest for
  the constant ids reaching `fk.call`/`fk.subscribe` and prunes the tables: the one-member
  example above ships a 646-byte member table where the full one is about 840 KB. An id the
  scan cannot prove constant ships the whole table (bigger, never broken), and the build
  output says so.
- **Handles come in two spaces**, split at `0x40000000`. Everything the host returns is
  transient and released when the event that produced it returns; `fk_retain` promotes what
  must outlive the event into `storage`, across saves.
- **Events are filtered in C++ before your handler runs.** A filtered subscription carries
  Factorio's own filter list, so an `on_entity_died` for a biter never enters the guest.
- **An expensive event field can be declined.** `on_undo_applied` carries an unbounded array
  of blueprint entities; a guest that wants one `uint32` out of it can mask the rest.
  Measured on that subscription with 200 actions: 7.49 ms → 2.7 µs per dispatch.
- **Commands and remote interfaces reach a guest.** A wasm guest has no callable Lua value,
  so the host synthesises the closure, hands it to Factorio, and dispatches back in by an id
  the guest chose: `fk.register` in, `fk_on_call` out, `remote.call` in both directions.
- **A host call costs about 12.5 µs**, cross-confirmed over 2,487 real calls in game. The
  cost model is calls, not bytes: batch at the boundary, not inside it.
- **A new Factorio version is a data drop, not a porting job.** `fklua api pull`, `api diff`
  and `api check GUEST.wasm --to <version>` say what moved and whether anything your mod
  calls broke; adding the 2.1.12 description (482 new members) needed no generator change.

Full detail, including what is not built: [`agents/abi.md`](agents/abi.md).

---

## FkIPC: talking to a process outside the game

FkIPC ("Factorio, kommunikativ (per IPC)") is a message-oriented link between a FkLua guest
and a companion process on the same machine, over Factorio's UDP surface: sessions, channels,
correlated request/response, gap detection, fragmentation, and bulk transfer by file plus
digest. Both guest languages have it (`guest/go/fkipc`, `guest/rust/fkipc`); the other end
is a Go SDK (`sdk/go/fkipc`). Three lines in the guest:

```go
func init() { fkipc.Open(fkipc.Config{Port: 29434, Name: "my-mod/1", ExpectPeer: "my-mod/1"}) }

//go:wasmexport fk_on_tick
func onTick(tick uint32) { fkipc.Pump(tick) }

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) { if fkipc.OnEvent(id, ptr) { return } /* your own events */ }
```

Your program owns those exports and routes in; `Open` goes in an initialiser because that
runs during `_initialize`, on every load, the only place a subscription may go. The other
end, from any Go program:

```go
s, _ := fkipc.Dial(fkipc.Options{GamePort: 29433, ListenPort: 29434})
s.Subscribe(1, func(m fkipc.Message) { fmt.Println("state:", string(m.Payload)) })
out, err := s.Request(context.Background(), 2, []byte("ping"))
```

**Give the pairing an identity.** Set `Config.ExpectPeer` in the guest and
`Options.ExpectedName` in the companion to one build-time token (convention:
`"<mod-name>/<schema-tag>"`, the tag being your claim about channel compatibility). Each end
then refuses a handshake with anything else, so a swapped port or a stale companion is a
session that never comes up rather than two ends silently disagreeing about what channel 1
means. It is a correctness check, not an authentication boundary.

Start the game with **`--enable-lua-udp 29433`** (a Steam launch option is the same flag).
That binds one socket, which is both the game's receive socket and the source port of
everything it sends, so the companion must listen on a different port; `Dial` refuses a
matching pair. What it costs, and the four numbers to design around:

| | |
|---|---|
| **frame cap 3,900 B** | the protocol maximum, negotiated down to 2,048 by default. An oversized `send_udp` fails silently, so the library enforces the cap. Above one frame, send a file |
| **inbound is about 6 kB/s** once one player is connected, and 1.5 kB/s at twenty | every inbound byte becomes an InputAction: replicated to every client, written into the replay, quantized to a tick. Talk a lot, listen a little, and make the listening idempotent |
| **outbound is free** | a local side effect that never enters game state |
| **pause is silent loss** | no ticks, no pump, and the OS buffer is 256 KB with no notification when it overflows. The guest drains on resume; the peer must go quiet on the guest's silence, which the SDK does by default |

**FkIPC requires Factorio 2.1.14 or newer, and below that it is inert.** On older engines a
headless `recv_udp` with a packet queued aborts the server (a C++ abort no `pcall` can catch;
measured on 2.0.77, delivered on 2.1.14). Below the floor `Open` returns `StatusDisabled` and
logs one line (`fkipc: disabled -- requires Factorio >= 2.1.14; this engine is X`), nothing
goes on the wire, and every `Send`, `Request`, `WriteBulk` and `NotifyFile` answers
`StatusDisabled`, counted in `Stats().Refusals`. The floor is about the **engine**, not the
API pin: every member FkIPC calls exists in the 2.0.77 description, so a mod built at the
default pin gets the whole library on a 2.1.14 engine with no rebuild, and the gate is
re-read once a second while shut.

**Before you write a handler, read the join-safety contract**: what a guest may branch on
(message payloads, session events, the tick, `Stats`, its own state, the world) and what it
must never store (whether an outbound host call succeeded; anything computed in
`fk_after_load`). Outbound is free and its outcome is per-peer, so attempt the call, drop the
answer, and log "did that work?" with `fk.Log`, which is not CRC'd. The contract is in the
package documentation of both libraries (`guest/go/fkipc/doc.go`,
`guest/rust/fkipc/src/lib.rs`) and in [`agents/ipc.md`](agents/ipc.md), with the protocol and
its cost model. [`scripts/run-ipc.sh`](scripts/run-ipc.sh) runs the whole thing end to end
against a real headless game in either language; its companion
[`sdk/go/cmd/ipcgate`](sdk/go/cmd/ipcgate/main.go) is the worked example of an SDK consumer.

---

## Guest languages

| Language | Target | Status |
|---|---|---|
| **Go** (TinyGo) | `-target=wasm-unknown -scheduler=none -gc=custom -opt=2` | supported |
| **Rust** | `wasm32-unknown-unknown`, `no_std` + `alloc` | supported |
| **Go** (TinyGo `wasip1`) | `-buildmode=c-shared`, a three-import WASI shim | supported; goroutines run in game; `-gc=leaking` only |
| **C** | `wasi-sdk`, `wasm32-unknown-unknown -nostdlib` | optional, not started |

Neither language is second-class: both backends are generated from one API description, and
a test compares the member id sets, so a feature added to one and not the other fails the
build. Where Rust says it better, the binding says it: `Result<T, Status>`, `Option<T>`,
`&str` arguments, `BTreeMap` for a dictionary (key-ordered, so its wire order is
deterministic), and the dynamic value as an `enum`. The `hello` guest is mirrored line for
line across the two and its output compared byte for byte, hash included:

```
hello from LANG, running as Lua inside Factorio
guest built with LANG: fnv64(fklua)=449d63cef97b1fda
tick 30 seen=30 fizz=8 buzz=4 fizzbuzz=2 sum=465 mean=15.50
```

Both flagship guests are 32-bit: 64-bit integers have no hardware equivalent in a Lua sandbox
where every number is a double, so each costs a `(lo, hi)` pair and roughly doubles the price
of arithmetic touching it. **Standard Go (the `go` toolchain's own `GOOS=wasip1`) is not
supported and will not be**: its `int` and every pointer are 64-bit on wasm, so all address
arithmetic pays that pair cost; modules start around 2 MB with a full GC and scheduler; and
its runtime blocks on `poll_oneoff`, which a Factorio tick cannot do, so any `time.Sleep`
becomes a busy spin that hangs the game. TinyGo's own `wasip1` target is supported.

### Breaking change for existing Rust guests: `LuaStr`

Every generated string position in the Rust bindings (event payload fields, string-returning
attributes, struct fields) is `LuaStr`, a byte type, rather than `String`: a Lua string is an
arbitrary byte sequence, and the previous `from_utf8_lossy` reader silently rewrote non-UTF-8
bytes and changed the length. Where you read one, call `.as_bytes()` for the bytes or
`.as_str()` for a checked `&str`; `.to_string_lossy()` is the old behaviour, now named.
`BTreeMap<LuaStr, V>` is looked up with `m.get("colour".as_bytes())`; nothing about the wire
changed. Separately, every dynamic-value argument is taken by reference (`&Value`), so a call
site passing an owned `Value` needs a `&`. Detail: [`agents/abi.md`](agents/abi.md).

---

## Memory, the collector and the save

The defaults are already chosen: `fklua init` writes `gc = "collected"` and scaffolds a guest
that carries the collector, and `fklua mod` defaults to `--persist=table`. For a first mod
there is nothing here to decide.

The guest heap is **collected**: a paced incremental conservative mark-sweep, cut into
bounded steps driven from a one-shot `on_tick` that exists only while a collection is in
flight, so an idle guest registers nothing and pays nothing. There is no heap cap; collector
metadata is about 31 KiB plus about 1% of the heap. Linear memory is **sharded** into
2¹⁹-word Lua tables, which keeps Lua's own collector flat at about 0.5 ms out to 40 MiB
instead of scaling with the whole memory, and `memory.grow`'s zero-fill is paced behind a
cursor. What bounds a guest is Factorio's own per-MiB bill, priced in
[`agents/guests.md`](agents/guests.md) under "the guest heap budget", and wasm32's 4 GiB.

The number behind the default: a **leaking** guest's linear memory doubles on every grow, and
every new word is zero-filled, so at a 40 MiB heap the worst grow tick is **974.5 ms leaking
against 24.6 ms collected** (measured in game, same guest, same allocation rate). That is one
freeze every client in a lockstep game feels at once, and it is about the growth law rather
than reclaiming bytes.

### The two decisions you might make later

| Symptom | The change | What it costs |
|---|---|---|
| your saves are large or multiplayer joins are slow, and the guest heap is the reason | `fklua mod --persist=packed`: the live table mirrored into `string.pack` pages, **0.44 B/word** saved against the default's 2.29 B/word, 5.2× smaller | about 40 µs per *dirty* page per guest call. A downstream mod on a large map measured 13.8× smaller saves and 2.6× faster loads |
| you have measured your own heap over a long session and it does not grow | `gc = "leaking"` in `fklua.toml`, and build without the collector: the expert opt-out for an allocation-disciplined guest, and the only option for wasip1 | it buys back the collector's emitted code (measured downstream: +32.4% of the generated Lua, +13.7% of the zip) and nothing about the growth law above |

Choose `leaking` on a measurement, not on a prediction. `--gc=collected` is checked: a module
that does not export the collector's pacing surface is refused. `--persist` has two more
modes: `auto` picks by declared heap size (threshold 1 MiB) and prints its choice, and `none`
rebuilds memory from the data segments every load (nothing survives; deterministic but
stateless). The collector's design is in [`agents/gc.md`](agents/gc.md) and the sharded
memory representation in [`agents/sharding.md`](agents/sharding.md).

### Recompiling

Recompiling a guest invalidates the heap in your users' saves, and so does repackaging it
against a different `--api` pin, which moves the member, event and define ids the heap was
written against; both move the build id a save records. On a mismatch the old heap is
discarded and the loss is logged, unless the guest exports `fk_migrate(old_version)`, a
notification on a fresh heap, which is what a rebuild-from-the-world needs.
`fk_migrate_adopt` is the separate opt-in that hands the old bytes over, and most guests
should never export it: linear memory is `.data` and `.rodata` as well as the heap, so a
rebuilt guest reading an adopted image reads the previous build's string constants. The
round trip is verified inside Factorio: a headless server honours `game.auto_save()`, so
[`scripts/run-roundtrip.sh`](scripts/run-roundtrip.sh) makes a real save mid-game and loads
it back, including mid-mark and mid-sweep with the collector on and a rebuilt guest that
exports `fk_migrate`.

---

## Correctness

The official WebAssembly conformance suite runs green under a Factorio-shaped interpreter:
**15,675 assertions across 48 files, zero failures**, and 15,777 under `--nan=exact`, the
extra 102 being exactly the ones canonical mode must skip. The pass rate
(`testdata/spec/PASSRATE`) may rise and never fall, and CI runs the suite at every `-opt`
level in both NaN modes.

The oracle is `bin/lua52f`, Lua 5.2.1 built from PUC source and patched to Factorio's
sandbox, checked against the game by `make check-lua52f`. It is not substitutable: Homebrew
has no `lua@5.2` and its `lua` is 5.5, which has an integer subtype, so `%`, overflow and
`string.pack` all behave differently from Factorio's doubles-only 5.2.1 and it silently
passes code that breaks in game. Everything above the interpreter is verified in a real
Factorio too: `run-guest.sh`, `run-roundtrip.sh`, `run-gcbench.sh`, `run-growbench.sh` and
`run-ipc.sh` under `scripts/` build, package and run a guest in whichever Factorio is
installed (2.0.x by default; the FkIPC gates need 2.1.14 and say so rather than start) and
read per-tick counters back out of it.

**One platform limit you may hit.** A Lua number cannot carry a NaN's sign bit or payload,
and `fklua compile` prints a warning naming each instruction and function that depends on
one (`f32.copysign in "mix": a Lua number cannot carry a NaN's sign bit, ...`), ending with
the remedy: recompile with `--nan=exact`, which preserves NaN bits at a substantial speed
cost. Almost every mod sees a few of these and they are benign; the ones naming
`fkapi.writeDyn` / `fkapi.readDyn` are the bindings moving a double through memory, and
nothing there compares or copysigns. If you cannot point at a place your program observes a
NaN's bits, there is nothing to do.

---

## Performance

The goal is to write in your language, with its optimizer, and land within a small constant
of hand-tuned Lua. Not "faster than Lua", though on bit-manipulation code it is, because the
lowerings avoid `bit32` where a human reaching for the obvious library call cannot.
`scripts/bench-guests.sh`, `-opt=3`, ratios against hand-written Lua, so below 1.00× means
FkLua wins:

| Kernel | Go/Lua | Rust/Lua |
|---|--:|--:|
| `pure_prng`: xorshift32, no memory | **0.68×** | **0.67×** |
| `pure_sum`: u32 array reduction | **1.88×** | **1.73×** |
| `real_names`: build and hash strings | 4.52× | 5.51× |
| `real_entities`: struct scan and filter | 5.47× | 5.38× |
| `pure_dot`: f64 dot product | 8.49× | 8.48× |
| `real_grid`: flood fill over a 2D grid | 8.57× | 8.17× |

Hand-written Lua is still faster than a compiled guest at everything except `prng`; the gap
has narrowed (a loop entry guard took `pure_sum` from 4.41× to 1.88×), but a factor of two is
not an inversion. It usually does not decide anything: most mod code is dominated by the
C++/Lua API boundary rather than by Lua execution, and a host call is about 12.5 µs,
thousands of interpreted instructions, so for ordinary event handlers what matters is how
many calls you make. Downstream mods that beat their Lua incumbents by an order of magnitude
did it by changing what work happens per tick.

`--opt=0..3`, default 3. Level 0 disables every pass and is the reference a miscompile is
bisected against. What each level does and measured is in
[`agents/optimizer.md`](agents/optimizer.md); how to read any performance number here is in
[`agents/benchmarks.md`](agents/benchmarks.md).

---

## Limitations

- **Memory costs 4×.** A Lua `TValue` is 16 bytes and linear memory is a table of 32-bit
  words, so a 1 MiB guest heap is about 4 MiB of Lua on every client.
- **Saves get bigger and multiplayer joins get slower.** Guest memory lives in `storage`,
  which Factorio serializes into every save and ships to every joining client. This is
  usually a larger practical cost than the raw memory. `--persist=packed` is 5.2× smaller
  than the default; `--persist=none` opts out entirely.
- **No coroutines, so no yielding.** A single call must finish inside one tick, and Factorio
  enforces no instruction budget, so an infinite guest loop hangs everyone's game. `--fuel=N`
  stops a runaway after N loop iterations per event and defaults off (it costs 1.98× on a
  bare counted loop and 1.21× on array code); turn it on if you ship to other people.
- **Recompiling invalidates the guest heap in your users' saves.** Any change moves the
  layout, so a heap written by one build and read by another is undefined, not merely stale.
  On a build-id mismatch the old heap is discarded and logged; export
  `fk_migrate(old_version)` to be told and rebuild your state from the world.
- **Subscriptions belong in `_initialize`, not `fk_on_init`.** `control.lua` runs
  `_initialize` on every load; `script.on_init` fires once, when a save is created. A
  subscription made in `fk_on_init` vanishes the first time the save is reloaded, and the API
  calls keep working while the events silently stop arriving.
- **A `(ptr, len)` you hand the host must be consumed before that call returns.** It is guest
  heap, and the collector cannot see the host holding it.
- **Determinism is on you.** Factorio is lockstep multiplayer; anything nondeterministic in
  guest code desyncs every client. No entropy, no wall clock, no iteration-order dependence
  in anything host-visible, and nothing only one peer can observe (`fk_after_load` fires on a
  joining client alone) may write guest state.

More traps, each found by a real mod: [`agents/guests.md`](agents/guests.md), "Six things a
guest author gets wrong".

---

## Working on FkLua itself

```sh
make               # build bin/fklua (and the oracle it depends on)
make lua52f        # Lua 5.2.1 patched to Factorio's sandbox: the test oracle
make check-lua52f  # assert it still matches Factorio
make test          # go test ./... plus the sdk/go, guest/go and Rust fkipc tests
make spectest      # the WebAssembly conformance suite
make bench         # the hand-written Lua kernels and the performance gate
make optbench      # what each -opt level is worth
make guest         # build, package and run a guest in a real Factorio
```

`make test` is the entry point, not `go test ./...`: about thirty tests measure against
`bin/lua52f` and skip when it is absent, and `go test` prints nothing for a skip. `make test`
builds the oracle first and also runs the modules `go test ./...` does not reach (`sdk/go`,
`guest/go`, and `cargo test -p fkipc` under `guest/rust`). The `agents/` directory holds the
maintainer design notes: measured decisions, the invariants the emitter and runtime rest on,
and the reasons behind them.

| File | Covers |
|---|---|
| [`agents/guests.md`](agents/guests.md) | Toolchain flags and why each is mandatory, the host ABI from the guest side, the guest heap budget, what a player experiences per MiB |
| [`agents/abi.md`](agents/abi.md) | The API census, the two handle spaces, status codes, dispatch, marshalling tiers, the binding generator |
| [`agents/gc.md`](agents/gc.md) | The incremental collector: design, pacing, the safe-point precondition |
| [`agents/ipc.md`](agents/ipc.md) | FkIPC: the wire format, sessions and epochs, the filter ladder, the determinism cost model |
| [`agents/sharding.md`](agents/sharding.md) | Linear memory's sharded representation, and what a large Lua table costs |
| [`agents/codegen.md`](agents/codegen.md) | The emitter: measured lowerings, the i32/i64/float representations, NaN modes |
| [`agents/optimizer.md`](agents/optimizer.md) | What each `-opt` level does, assumes and measured |
| [`agents/benchmarks.md`](agents/benchmarks.md) | What each benchmark measures and how to read the numbers |
| [`agents/versioning.md`](agents/versioning.md) | The version axes, `api pull`/`list`/`diff`, how a change is classified, moving the default pin |
| [`agents/sandbox.md`](agents/sandbox.md) | The Factorio Lua 5.2 sandbox limits, `string.pack`, `collectgarbage` |
| [`agents/testing.md`](agents/testing.md) | Running the suite, the corpus, the toolchain guard |

`CLAUDE.md` is the maintainers' working context and indexes all of them.

---

## License

FkLua is released under the [MIT License](LICENSE). Two committed inputs are third-party
work under their own terms and are not covered by it: `testdata/spec/` is generated from
the WebAssembly specification test suite at the commit named in `testdata/spec/SOURCE`,
and `third_party/lua-5.2.1/` fetches the Lua 5.2.1 source (MIT, PUC-Rio) at build time and
applies this repository's patches to it. Generated Factorio API bindings are derived from
the game's published `runtime-api.json`.
