# Factorio, kein Lua

Factorio, kein Lua, (FkLua for short) is a tool to enable the authorship of [Factorio](https://www.factorio.com/) mods in non-Lua programming languages (currently GoLang and Rust are supported).

FkLua compiles WebAssembly ahead-of-time into Lua 5.2 source and packages the result as a Factorio mod. You write your mod in Go (via TinyGo) or in Rust, compile it to a wasm module (the **guest**), and `fklua mod` turns that module into a package that Factorio loads like any other mod: a generated `control.lua`, the runtime that hosts the guest, and the slice of the Factorio API bindings the guest actually calls.

```sh
fklua init my-mod          # a project, a guest, a collector, a manifest
fklua gen-bindings         # the Factorio API, for your language
fklua mod my-mod.wasm      # a module Factorio loads
```

Factorio only loads Lua *source*; `load()` rejects binary chunks. FkLua emits ordinary, readable Lua source that Factorio parses like any hand-written mod. There is no LLVM dependency: WebAssembly is the input format because it arrives already legalized (structured control flow, four value types, explicit linear memory, a published ABI), and anything that emits wasm can target it.

Rust and Go are supported at parity: the whole Factorio runtime API is bound member for member in Go and in Rust from one API description, with events, `defines`, commands and remote interfaces, and each has a guest-heap garbage collector, save/load persistence, and an IPC library for talking to a process outside the game. Real mods have been built on both, run in real Factorio games, and benchmarked against hand-written Lua implementations.

---

## The Dream

After proliferation of FkLua mods, Wube adds a WASM sandbox natively to Factorio. WASM mod performance then matches or exceeds the performance of Lua mods.

Eventually, a Lua-interpreter WASM module is used to shim legacy Lua-based mods into the new WASM runtime.

The native Factorio Lua interpreter is retired.

---

## Status

Two known public projects currently (August 2026) utilize FkLua:

| Project | What it is |
|---|---|
| [BetterBeltBalancer](https://github.com/Techrocket9/BetterBeltBalancer) | A Factorio 2.0 mod written in Go and compiled with FkLua. Intended to be a drop-in replacement for [Belt Balancer 3](https://mods.factorio.com/mod/belt-balancer-3). |
| [fklua-ports-samples](https://github.com/Techrocket9/fklua-ports-samples) | Six open-source Factorio mods ported to FkLua guests (three Rust, three Go) as a validation campaign for FkLua's API coverage, with the findings ledgers that campaign produced. A showcase, not maintained. |

---

## Prerequisites

| | For what |
|---|---|
| **Go 1.26+** | building the `fklua` tool itself |
| **TinyGo 0.41.1** | a Go guest. TinyGo's `wasm-unknown` target, not standard Go (see [Guest languages](#guest-languages)) |
| **binaryen** (`wasm-opt`) | required by TinyGo, which shells out to it for every wasm target |
| **Rust 1.97+** with `wasm32-unknown-unknown` | a Rust guest (`rustup target add wasm32-unknown-unknown`) |

`brew install tinygo binaryen` covers the middle two on macOS. You need one guest toolchain, not both. `fklua doctor` reports each row as found or missing with the version it found, and exits non-zero only when neither guest toolchain is complete.

---

## Prior Art

Believe it or not, I'm not the first to build a WASM → Lua compiler for Factorio. phiresky [did it first](https://github.com/phiresky/NetHack-in-Factorio), though I was not aware of his effort when I began work on FkLua.

FkLua does not utilize any code or IP of phiresky's.

---

## Quickstart

From a checkout of this repository. This is the Go path; Rust follows.

```sh
make                                # builds bin/fklua; put it on your PATH
fklua doctor                        # optional: is the toolchain complete?
cd .. && mkdir my-mod && cd my-mod
fklua init my-mod --guest-module /path/to/fklua
```

`init` writes into the **current directory** and creates no `my-mod/` of its own; the name argument is the mod's identity. `--guest-module` points the scaffolded guest at a local FkLua checkout; leave it off and run `go mod tidy` in `guest/go/` once the guest module is fetchable where you are. It writes `fklua.toml` (the mod's identity, dependencies, API pin, guest language and GC mode) and a guest that already builds under `guest/go/`: its own Go module, the collector import in `gc.go`, and `fk_on_init` and `fk_on_tick` wired in `main.go`. What every generated file is for, key by key, is [`docs/generated-files.md`](docs/generated-files.md). Then:

```sh
fklua gen-bindings && fklua lock          # the Factorio API lands at guest/go/fkapi/
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=custom -opt=2 -o ../../my-mod.wasm .)
fklua mod my-mod.wasm
```

`fklua mod` needs no flags; everything comes out of `fklua.toml`, and a flag is an override. It prints the size of the Lua it wrote, the modes it used, and each guest export it wired to a Factorio hook. Copy `my-mod_0.1.0/` into Factorio's `mods/` directory, or pass `--zip`. Calling the API is an import away:

```go
import "my-mod-guest/fkapi"

speed, err := fkapi.Game.Speed()          // read
err = fkapi.Game.SetSpeed(speed * 2)      // write
```

and `fklua mod` then reports `API 2.0.77: 1 members, pruned from 4259`: the mod ships the one member it calls. Every TinyGo flag above is required, and the scaffolded `main.go` says why; `-opt=2` rather than TinyGo's `-opt=z` default is worth up to 1.7×, because `z` optimises for size, which is not a major cost here (Factorio parses 4 MB of Lua in about 106 ms).

### Rust

```sh
fklua init my-mod --lang rust --guest-module /path/to/fklua
fklua gen-bindings && fklua lock
(cd guest/rust && cargo build --release \
    --target wasm32-unknown-unknown --features fk/fkgc)
fklua mod guest/rust/target/wasm32-unknown-unknown/release/my_mod_guest.wasm
```

`init` scaffolds `guest/rust/` as a two-member cargo workspace, the generated `fkapi` crate beside your guest, with `panic=abort`, `lto` and `opt-level="s"` already set. `--features fk/fkgc` is the collector: no import and no second flag, because the `fk` crate owns the single `#[global_allocator]` site. Yes, a Rust project with a garbage collector; [`docs/memory.md`](docs/memory.md) explains why. If a crate reaches a wasm feature FkLua does not compile (`multivalue`, `reference-types`), `fklua compile` names it; the recipe for turning those off is in [`agents/guests.md`](agents/guests.md).

---

## Where to go next

The scaffold uses the two simplest hooks: `fk_on_init` once per save and `fk_on_tick` every tick. A real mod subscribes to events and decodes their payloads, and the worked example of that is [`guest/go/examples/api/`](guest/go/examples/api/main.go) (Rust twin: [`guest/rust/examples/api/`](guest/rust/examples/api/src/lib.rs)). It is one file:

| In the example | What it shows |
|---|---|
| `fkapi.Subscribe` / `SubscribeFiltered` from `func init()` | subscriptions run during `_initialize`, the one place they may go |
| a `fk_on_event` switch over `fkapi.EventXxx` | one export, every event, dispatched on a generated id rather than a hand-written number |
| `fkapi.ReadOnPlayerCreated(ptr)` | the payload as a generated struct, not a cast at an offset you derived |
| `fkapi.NameFilter("iron-chest")`, `fkapi.TypeFilter("container")` | Factorio's own filters, applied in C++ before the guest is entered |
| `surface.NameIs("nauvis")` | a host-side predicate: asks the question rather than copying the string into guest memory |
| `fkapi.LuaEntity{Object: e.Entity}` | a raw handle out of a payload, wrapped so its methods are callable |
| `fkapi.DefinesDirectionEast()` | a `defines` value asked for by name, because its number is per Factorio build |

`guest/go/examples/` holds twenty more, each aimed at one thing: `array` and `dict` for marshalling, `callback` for commands and remote interfaces, `retain` for a handle that outlives its event, `gcsave` for the collector across a save, `migrate` for a rebuilt guest, [`ipc`](guest/go/examples/ipc/main.go) for [FkIPC](guest/go/fkipc/README.md), and [`datastage`](guest/go/examples/datastage/main.go) for a mod's settings and data stages. `guest/rust/examples/` mirrors nine of them line for line.

A mod's **settings and data stages** can be a guest too, as a second wasm module packaged beside the control one. It defines prototypes, reads and patches `data.raw`, and clones a base prototype without marshalling it through the guest, which is what keeps the untouched fields exactly as the source shipped them. There is no runtime API at those stages, so a data module imports `fkdata` and never `fkapi`, and packaging refuses one that does. See [`docs/data-stage.md`](docs/data-stage.md).

---

## Documentation

Documentation for mod authors lives under [`docs/`](docs/):

| Page | Covers |
|---|---|
| [`docs/generated-files.md`](docs/generated-files.md) | every file `fklua init`, `gen-bindings`, `lock` and `mod` write: which are yours to edit, which are regenerated, and how |
| [`docs/memory.md`](docs/memory.md) | guest memory, the garbage collector and why Rust has one, the tuning knobs, the leaking opt-out, `--persist`, and migrating a recompiled guest |
| [`docs/factorio-api.md`](docs/factorio-api.md) | calling the API: handles, events, filters and field masks, commands and remote interfaces, `defines`, and the version axes |
| [`docs/data-stage.md`](docs/data-stage.md) | writing a mod's settings and data stages in Go or Rust: the four hooks, reading and patching `data.raw`, cloning a prototype, ordering against hand-written Lua, and verifying with `--dump-data` |
| [`docs/lua-limits.md`](docs/lua-limits.md) | the two Lua limits a generated guest can reach, what each packaging error means, and the `//go:noinline` / `#[inline(never)]` remedy |
| [`docs/verifying.md`](docs/verifying.md) | the headless create-and-benchmark check for any mod |

The `agents/` directory holds the maintainer design notes; see [Working on FkLua itself](#working-on-fklua-itself).

---

## The Factorio API from a guest

The whole runtime API is bound in both languages, member id for member id: against the default **2.0.77** API pin, 4,260 of 4,262 members, with a payload struct for every event, 1,329 inherited forwarders and 1,137 `defines` accessors. The counts are committed data in `api/<version>/census.json`, regenerated with the bindings; read them from there rather than from this page.

The shape of it: one generic `fk.call` import rather than one per method, so a member Factorio removes degrades to a status instead of failing the module; a mod ships the member and event tables pruned to the ids it provably calls (about 0.6 KB instead of about 840 KB); events are filtered in C++ before the guest is entered, and an expensive payload field can be masked out; commands and remote interfaces dispatch back in by id; and a host call costs about 12.5 µs, so the cost model is calls, not bytes.

There are two version axes worth keeping apart: the **API pin** (which `runtime-api.json` the bindings and the packaged tables come from; default 2.0.77, changed in one `fklua.toml` line) and the **engine** (whichever Factorio is running). How they meet in `info.json`, what 2.1.x adds, handles, events, masks, `defines`, and what `api pull`/`diff`/`check` do when a new Factorio version lands: [`docs/factorio-api.md`](docs/factorio-api.md).

---

## FkIPC: talking to a process outside the game

FkIPC is a message-oriented link between a FkLua guest and a companion process on the same machine, over Factorio's UDP surface: sessions, channels, correlated request/response, gap detection, fragmentation, and bulk transfer by file plus digest. Both guest languages have it, the other end is a Go SDK, and it needs a Factorio 2.1.14 or newer engine (below that it is inert). Three lines in the guest and three in the companion get a session; the wiring, the cost model, the pairing identity and the join-safety contract every handler must follow are in **[`guest/go/fkipc/README.md`](guest/go/fkipc/README.md)**.

---

## Guest languages

| Language | Target | Status |
|---|---|---|
| **Go** (TinyGo) | `-target=wasm-unknown -scheduler=none -gc=custom -opt=2` | supported |
| **Rust** | `wasm32-unknown-unknown`, `no_std` + `alloc` | supported |
| **Go** (TinyGo `wasip1`) | `-buildmode=c-shared`, a three-import WASI shim | supported; goroutines run in game; `-gc=leaking` only |
| **C** | `wasi-sdk`, `wasm32-unknown-unknown -nostdlib` | optional, not started |

Neither language is second-class: both backends are generated from one API description, and a test compares the member id sets, so a feature added to one and not the other fails the build. Where Rust says it better, the binding says it: `Result<T, Status>`, `Option<T>`, `&str` arguments, `BTreeMap` for a dictionary (key-ordered, so its wire order is deterministic), and the dynamic value as an `enum`. The `hello` guest is mirrored line for line across the two and its output compared byte for byte, hash included:

```
hello from LANG, running as Lua inside Factorio
guest built with LANG: fnv64(fklua)=449d63cef97b1fda
tick 30 seen=30 fizz=8 buzz=4 fizzbuzz=2 sum=465 mean=15.50
```

Both flagship guests are 32-bit: 64-bit integers have no hardware equivalent in a Lua sandbox where every number is a double, so each costs a `(lo, hi)` pair and roughly doubles the price of arithmetic touching it. **Standard Go (the `go` toolchain's own `GOOS=wasip1`) is not supported and will not be**: its `int` and every pointer are 64-bit on wasm, so all address arithmetic pays that pair cost; modules start around 2 MB with a full GC and scheduler; and its runtime blocks on `poll_oneoff`, which a Factorio tick cannot do, so any `time.Sleep` becomes a busy spin that hangs the game. TinyGo's own `wasip1` target is supported.

---

## Memory, the collector and the save

The defaults are already chosen: `fklua init` writes `gc = "collected"` and scaffolds a guest that carries the collector, and `fklua mod` defaults to `--persist=table`. For a first mod there is nothing here to decide.

If a garbage collector in a **Rust** project raises an eyebrow: it is there because of where the heap lives, not because of the language. Guest memory is Lua tables inside Factorio's save, identical on every client of a lockstep game and billed per MiB per tick, and the allocator's `dealloc` is deliberately a no-op, so `Drop` never returns memory; some blocks (payloads the host writes into guest memory) have no owner to free them at all. Reclamation is tracing at safe points or nothing, in Go and Rust alike.

The collector is a paced incremental mark-sweep cut into bounded steps driven from a one-shot `on_tick` that exists only while a collection is in flight, so an idle guest registers nothing and pays nothing. There is no heap cap, and a guest that outruns its budget grows instead of stalling. Two decisions do exist, each worth making on a measurement:

| Symptom | The change | What it costs |
|---|---|---|
| your saves are large or multiplayer joins are slow, and the guest heap is the reason | `fklua mod --persist=packed`: the live table mirrored into `string.pack` pages, **0.44 B/word** saved against the default's 2.29 B/word, 5.2× smaller | about 40 µs per *dirty* page per guest call. A downstream mod on a large map measured 13.8× smaller saves and 2.6× faster loads |
| you have measured your own heap over a long session and it does not grow | `gc = "leaking"` in `fklua.toml`, and build without the collector: the expert opt-out for an allocation-disciplined guest, and the only option for wasip1 | it buys back the collector's emitted code (measured downstream: +32.4% of the generated Lua, +13.7% of the zip) and nothing about the growth law |

Everything else, including the tuning knobs, what recompiling does to the heap in your users' saves (`fk_migrate`), and reacting to mod-set changes (`fk_on_configuration_changed`), is in [`docs/memory.md`](docs/memory.md).

---

## Verifying a mod headlessly

Factorio runs headless, so "does it load, and does it do the same thing twice" is a scriptable check: create a map (which is where `_initialize` and `fk_on_init` run), then `--benchmark` it twice and compare script checksums; two runs disagreeing means a nondeterministic guest, which in a lockstep game is a desync. The full recipe, generic over any mod, is [`docs/verifying.md`](docs/verifying.md).

---

## Correctness

The official WebAssembly conformance suite runs green under a Factorio-shaped interpreter: **15,675 assertions across 48 files, zero failures**, and 15,777 under `--nan=exact`, the extra 102 being exactly the ones canonical mode must skip. The pass rate (`testdata/spec/PASSRATE`) may rise and never fall, and CI runs the suite at every `-opt` level in both NaN modes.

The oracle is `bin/lua52f`, Lua 5.2.1 built from PUC source and patched to Factorio's sandbox, checked against the game by `make check-lua52f`. It is not substitutable: Homebrew has no `lua@5.2` and its `lua` is 5.5, which has an integer subtype, so `%`, overflow and `string.pack` all behave differently from Factorio's doubles-only 5.2.1 and it silently passes code that breaks in game. Everything above the interpreter is verified in a real Factorio too: `run-guest.sh`, `run-roundtrip.sh`, `run-gcbench.sh`, `run-growbench.sh` and `run-ipc.sh` under `scripts/` build, package and run a guest in whichever Factorio is installed (2.0.x by default; the FkIPC gates need 2.1.14 and say so rather than start) and read per-tick counters back out of it.

**One platform limit you may hit.** A Lua number cannot carry a NaN's sign bit or payload, and `fklua compile` prints a warning naming each instruction and function that depends on one (`f32.copysign in "mix": a Lua number cannot carry a NaN's sign bit, ...`), ending with the remedy: recompile with `--nan=exact`, which preserves NaN bits at a substantial speed cost. Almost every mod sees a few of these and they are benign; the ones naming `fkapi.writeDyn` / `fkapi.readDyn` are the bindings moving a double through memory, and nothing there compares or copysigns. If you cannot point at a place your program observes a NaN's bits, there is nothing to do.

---

## Performance

The goal is to write in your language, with its optimizer, and land within a small constant of hand-tuned Lua. `scripts/bench-guests.sh`, `-opt=3`, ratios against hand-written Lua, so below 1.00× means FkLua wins:

| Kernel | Go/Lua | Rust/Lua |
|---|--:|--:|
| `pure_sum`: u32 array reduction | **1.88×** | **1.73×** |
| `real_names`: build and hash strings | 4.52× | 5.51× |
| `real_entities`: struct scan and filter | 5.47× | 5.38× |
| `pure_dot`: f64 dot product | 8.49× | 8.48× |
| `real_grid`: flood fill over a 2D grid | 8.57× | 8.17× |

Hand-written Lua is still faster than a compiled guest. It usually does not decide anything: most mod performance is dominated by the C++/Lua API boundary rather than by Lua execution, and a host call is about 12.5 µs, thousands of interpreted instructions, so for ordinary event handlers what matters is how many calls you make. Downstream mods that beat their Lua incumbents by an order of magnitude did it by changing what work happens per tick.

`--opt=0..3`, default 3. Level 0 disables every pass and is the reference a miscompile is bisected against. What each level does and measured is in [`agents/optimizer.md`](agents/optimizer.md); how to read any performance number here is in [`agents/benchmarks.md`](agents/benchmarks.md).

---

## Limitations

- **Memory costs 4×.** A Lua `TValue` is 16 bytes and linear memory is a table of 32-bit words, so a 1 MiB guest heap is about 4 MiB of Lua on every client.
- **Saves get bigger and multiplayer joins get slower.** Guest memory lives in `storage`, which Factorio serializes into every save and ships to every joining client. This is usually a larger practical cost than the raw memory. `--persist=packed` is 5.2× smaller than the default; `--persist=none` opts out entirely.
- **No coroutines, so no yielding.** A single call must finish inside one tick, and Factorio enforces no instruction budget, so an infinite guest loop hangs everyone's game. `--fuel=N` stops a runaway after N loop iterations per event and defaults off (it costs 1.98× on a bare counted loop and 1.21× on array code); turn it on if you ship to other people.
- **Recompiling invalidates the guest heap in your users' saves.** Any change moves the layout, so a heap written by one build and read by another is undefined, not merely stale. On a build-id mismatch the old heap is discarded and logged; export `fk_migrate(old_version)` to be told and rebuild your state from the world.
- **Subscriptions belong in `_initialize`, not `fk_on_init`.** `control.lua` runs `_initialize` on every load; `script.on_init` fires once, when a save is created. A subscription made in `fk_on_init` vanishes the first time the save is reloaded, and the API calls keep working while the events silently stop arriving.
- **A `(ptr, len)` you hand the host must be consumed before that call returns.** It is guest heap, and the collector cannot see the host holding it.
- **Determinism is on you.** Factorio is lockstep multiplayer; anything nondeterministic in guest code desyncs every client. No entropy, no wall clock, no iteration-order dependence in anything host-visible, and nothing only one peer can observe (`fk_after_load` fires on a joining client alone) may write guest state.

More traps, each found by a real mod: [`agents/guests.md`](agents/guests.md), "Six things a guest author gets wrong".

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

`make test` is the entry point, not `go test ./...`: about thirty tests measure against `bin/lua52f` and skip when it is absent, and `go test` prints nothing for a skip. `make test` builds the oracle first and also runs the modules `go test ./...` does not reach (`sdk/go`, `guest/go`, and `cargo test -p fkipc` under `guest/rust`). The `agents/` directory holds the maintainer design notes: measured decisions, the invariants the emitter and runtime rest on, and the reasons behind them.

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

FkLua is released under the [MIT License](LICENSE). Two committed inputs are third-party work under their own terms and are not covered by it: `testdata/spec/` is generated from the WebAssembly specification test suite at the commit named in `testdata/spec/SOURCE`, and `third_party/lua-5.2.1/` fetches the Lua 5.2.1 source (MIT, PUC-Rio) at build time and applies this repository's patches to it. Generated Factorio API bindings are derived from the game's published `runtime-api.json`.
