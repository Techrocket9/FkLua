# The data stage in Go and Rust

**Status: AS BUILT.** Designed and implemented 2026-08-25. Working notes; `agents/docs-style.md` does not apply here and does apply to `docs/data-stage.md`, which is the same feature written for a mod author.

FkLua compiles a mod's CONTROL stage from a guest. Its SETTINGS and DATA stages used to be hand-written Lua carried past the packager by `fklua mod --include`. This round makes a mod's Go or Rust run at those stages too, so that "the mod's brain is a compiled guest" stops having an exception.

The driver is BetterBeltBalancer, whose whole `mod-data/` tree is Lua and which has ruled that all of it is purged. So the bar was not "new logic in Go beside the old Lua"; it was that a real mod's entire data stage becomes Go, and that a sixteen-mod test estate's observer mods can follow.

---

## What was measured

Everything here was run against the **installed Factorio 2.0.77** (build 84539, mac-arm64, Steam, Space Age + quality + elevated-rails), headless, with a private `write-data` via `-c`, on 2026-08-25. Data-stage mechanics are identical on 2.0 and 2.1 -- the stage sequence, the globals, `data:extend`, `--dump-data` -- so 2.0.77 is a valid host for all of it. The golden dump hash is per engine and per mod set and says so.

### The stage environment, probed rather than quoted

Lua **5.2** at every stage.

| | settings | data / data-updates / data-final-fixes |
|---|---|---|
| `data`, `data.raw`, `data.extend` | table / table / function | same |
| `data.raw` type count | **0** (empty) | 224 at data, **251** at updates and final-fixes |
| `settings` | **nil** | table; `settings.startup` table, `.global` and `.player` **nil** |
| `mods` | table, `base=2.0.77` + every mod | same |
| `feature_flags` | table, 7 flags | same |
| `util` (global) | **nil**, but `require("util")` works | table (base sets it) |
| `log`, `require`, `load`, `loadstring` | function | function |
| `string.pack` / `unpack` / `packsize` | function (round-trip verified) | function |
| `bit32`, `debug`, `serpent`, `defines` | table | table |
| `helpers` | **userdata** | userdata |
| `os`, `io`, `coroutine` | **nil** | nil |
| `script`, `game`, `prototypes` | nil | nil |
| `package.path` / `.preload` / `.searchers` | **nil** (only `package.loaded`, 3 entries) | same |

Two rows are what the whole feature stands on and neither is guessable: **`bit32` and `string.pack` are present at both stages**, which is what a generated FkLua chunk needs; and `require` of a package-root file from a subdirectory works at every stage, which is what a mod's own `prototypes/` tree relies on.

**A real mod's control module loads and RUNS at every stage.** BBB's shipped `fk_module.lua` (3,136,956 B), copied into a scratch mod and required from `settings.lua`, `data.lua` and `data-final-fixes.lua`, gave `REQUIRE OK` / `BUILD OK` / `INITIALIZE true` at all three, demanding nothing those stages do not have -- no `game`, no `script`, no `storage`, no `defines`. `wrapChunk`'s FACTORY SHAPE is what makes that work: `require` returns a function, the chunk reads its imports from `...`, so instantiation is entirely under the caller's control.

**`fk_abi.lua` loads cleanly at both stages** with `read_value`, `write_value`, `read_dyn`, `write_dyn`, `read_struct`, `write_struct` intact, and `bind_globals(_G)` succeeds with no `game` present. That is what removed most of the marshalling work this round looked like it needed.

### The stage Lua states, and what `require` caches

A marker file incrementing a global, required from each stage:

```
SETTINGS    marker EXECUTED, global=1   package.loaded[marker]=false
DATA        marker EXECUTED, global=1   package.loaded[marker]=false
DATAUPDATES marker EXECUTED, global=2   package.loaded[marker]=false
FINALFIXES  marker EXECUTED, global=3   package.loaded[marker]=false
```

- **Settings is its own Lua state** (the global resets to 1 at data).
- **data, data-updates and data-final-fixes share ONE state** (1 -> 2 -> 3).
- **`require` re-executes the file at every stage.** Factorio's `require` is not `package.loaded`-backed under the plain name, and nothing carries across a stage boundary.

That last line is the whole of D1's cost model, and it is also why a data guest is instantiated FRESH per stage and keeps no state between them.

### Parse cost, interleaved against a no-module control

Six reps each, arms interleaved within every rep so session drift scales the group together. `--create` wall clock, medians.

| arm | median | delta |
|---|--:|--:|
| nothing | 1.675 s | -- |
| **306 KB module required at all four data-family stages** | **1.670 s** | **not separable from the control** |
| **3.1 MB module required at all four data-family stages** | **1.825 s** | **+150 ms, cleanly separated** |

**Read the middle row as a bound, not as a zero.** A rough model that fits both this and a separate single-parse isolation is **~5 ms fixed per `require` plus ~14.5 ns per byte**, which predicts ~38 ms for four parses of 306 KB -- at the edge of this harness's resolution. What the reps support is that the 306 KB arm is not separable from the control and the 3.1 MB arm plainly is; a defensible upper bound for a data guest across all four stages is ~40 ms against a control guest's measured 150.

### As built: module sizes through the real pipeline

`guest/{go,rust}/examples/datastage`, which reaches all seven imports:

| | wasm | generated Lua |
|---|--:|--:|
| the Go data guest | 83,990 B | **496,361 B** |
| the Rust data guest | 26,639 B | **535,311 B** |
| `fk_data.lua` (the shim, verbatim) | -- | 24,106 B |
| BBB's control guest, for scale | 1,293,396 B | 3,136,956 B |

The Rust module is a third of the wasm and slightly MORE Lua, which is the usual shape here: rustc emits fewer, larger functions.

### `--dump-data` is the acceptance gate

```sh
factorio -c "$USERDIR/config/config.ini" --mod-directory "$MODS" --dump-data
```

Exit 0 in **2.4 s**, writing `script-output/data-raw-dump.json` (27.8 MB, pretty-printed) and `mod-settings-dump.json`. It stops after the data stage, so it never runs `control.lua` -- a pure data-stage instrument.

| property | measured |
|---|---|
| **byte-identical across two runs** | yes |
| **key order is insertion-dependent** | **yes.** Two mods differing only in `data:extend` order produce dumps that `cmp` reports differing at char 1,458,907 |
| `jq -S` normalisation removes that difference | **yes, byte-identical after** |
| `jq -S` preserves a real field-value change | **yes** (`stack_size` 1 -> 42 survives normalisation) |
| the engine's own `Prototype list checksum` | **order-insensitive** (1401208605 for both orders) **but blind to field values** -- unchanged when `stack_size` went 1 -> 42 |

**So the gate is `jq -S . data-raw-dump.json` hashed, and the engine checksum is a cheap smoke test only.** The checksum is over the prototype LIST, exactly as its name says. Quoting it as an equivalence proof would be a gate that cannot fail on the defect class this feature is most likely to produce.

### The sizes that decided the clone question

Compact JSON bytes and scalar leaves, from a real dump of BBB's own data stage:

| prototype | base original | BBB's clone after strip+blank |
|---|--:|--:|
| `linked-belt` | 4,129 B / 160 leaves | 1,846 B / 78 |
| `express-transport-belt` | 12,036 B / 515 | 9,881 B / 436 |
| `express-splitter` | 13,614 B / 578 | 9,505 B / 423 |
| `lane-splitter` | 5,504 B / 211 | 1,394 B / 62 |
| **total** | **35,283 B / 1,464** | **22,626 B / 999** |

---

## Decisions

### D1 -- TWO wasm modules, ONE Go module

**A separate data-stage guest, compiled separately, packaged beside the control guest. The two live as two `main` packages inside the single existing `guest/go` module, not as a second Go module and not behind build tags.**

The measurement decides the first half. Under one module the control guest is parsed and instantiated at every data-family stage it hooks -- up to four times -- and a real mod's guest costs **+150 ms per game load** for parsing it does not need. Under two modules a data guest's cost across all four stages is not separable from a no-module control. That 150 ms is paid on every load, every settings change, every `--dump-data` and every multiplayer join.

Three architectural reasons stand behind the number and each would be sufficient alone.

- **The control guest's `_initialize` runs its package initialisers, and a real mod's initialisers call the API.** BBB's happened to survive stub imports in the probe, but a guest that reads `prototypes.entity` from `init()` would reach a handle table whose globals are all nil. That failure is silent and lands at data-stage load time, far from its cause.
- **Everything the control module carries is meaningless here.** `--persist`, the GC pacing surface, the API pin stamp, the arena bracket, `fk_api_gen.lua`'s member table. A data guest wants `--persist=none` and `-gc=leaking`: it runs once and dies with the Lua state.
- **The member-table pruning story is per module.** A data guest calls zero runtime API. Sharing the control guest's identity would attach a member table it never touches and a pin stamp `attachAPI` would then check.

**One Go module, two main packages** is the second half. `//go:wasmimport` needs `GOARCH=wasm` for both guests, so they belong in the same module by construction; `fklua lock` hashes exactly `guest/go/fkapi/fkapi.go`, one tree, one lock; and a mod's data stage and control stage share real logic (a version branch read by both), so a sibling package both mains import is the shape that retires the two-Lua-states problem outright.

Build tags were refused: they produce one binary that is two programs, which means the wrong `-gc`/`-target` flags for one of them and a `go vet` that only ever sees one arm.

**The data guest must not import `fkapi`, and it is enforced rather than advised.** `CheckDataModule` refuses a data module that imports any host module but `fkdata` and `env`, and one that exports `fk_api_pin_*`. The import check is the direct one and is also the honest report of a harder failure underneath: `fk_data.lua` binds those two and nothing else, so any other import is UNBOUND at instantiation. The pin-stamp check is the one that survives dead-code elimination -- it is a `//go:wasmexport`, so it is a root by definition, and a guest that imports `fkapi` carries it whether or not it calls a member.

### D2 -- the host ABI

**Seven imports under a new module name `fkdata`, plus the existing `env.fk_log` and `env.fk_print.`** Every argument is a pointer to one tier-2 dynamic value, which is the shape `fk.subscribe(id, filterp, mask)` and `fk.remote_call(callp, retp)` already have.

```
fkdata.stage()            -> u32      1 settings, 2 data, 3 data-updates, 4 data-final-fixes
fkdata.get(pathp, retp)   -> status   read data.raw at a path into one tier-2 slot
fkdata.set(pathp, valp)   -> status   write; valp == 0 means nil, i.e. delete
fkdata.extend(valp)       -> status   data:extend(array of prototypes)
fkdata.clone(pathp, dstp) -> status   deep-copy one data.raw entry to another name
fkdata.keys(pathp, retp)  -> status   the keys at a path, SORTED
fkdata.env(which, retp)   -> status   1 mods, 2 feature_flags, 3 settings.startup
```

A **path** is a tier-2 array of strings and numbers rooted at `data.raw`: `["technology","logistics","unit","count"]`, `["transport-belt","my-belt","collision_box",1,1]`. `clone`'s `dstp` is a path too, so cloning across types needs no second primitive.

**Not one generic import.** `fk.call` is one import because it fronts a 4,259-member table whose ids shift per Factorio version, so a removed member has to degrade to a status rather than break instantiation. `data.raw` is a plain Lua table with no version-skew surface and no id space, so that argument does not transfer. Seven purposeful imports is the same order as `fk`'s own seven and each one is greppable.

**Paths, not handles.** A handle space into data-stage tables would mirror the LuaObject handle table and make deep repeated access cheap. Refused: the data stage is a one-shot batch making order-hundreds of calls, not a steady state, and a handle space is lifetime state a transient stage does not need. The number that reopens this is a port making tens of thousands of `set` calls in one stage; BBB's is about forty.

**Errors RAISE, they do not return a status, and that is a deliberate deviation from the control ABI.** The three reasons `fk_abi.lua` never raises do not apply here: a data-stage failure has no lockstep simulation to keep consistent, the guest's state is discarded at the end of the stage anyway, and stopping the load loudly is Factorio's own convention and what a mod author wants at load time. So `fk_data.lua` raises with the STAGE NAME and the OFFENDING PATH in the message.

The status return carries exactly one thing: **`get` of a missing key returns ABSENT (1) rather than raising**, because "is this prototype already defined" is a normal question -- it is what a mod adopting an uninstalled neighbour's entities asks on every load. `keys` of a missing path answers the same way with an empty array.

### D-clone -- a host-side CLONE primitive, not whole-prototype marshalling

**`fkdata.clone` plus targeted patches. Reading a whole prototype into the guest, modifying it and extending it back is refused.**

1. **Fidelity by construction, which is not a cost argument.** BBB's four clones carry **999 scalar leaves after stripping**, every one of them base's own bytes deep-copied by the engine. Marshalling the prototype through the guest means reading 1,464 leaves in and re-serialising 999 out, so any field tier 2 cannot express, any float that does not round-trip through an f64, and any key the guest's value model quietly drops **silently changes the prototype and the mod still loads**.
2. **It is the same operation, so the port is a transliteration.** A hand-written data stage's `clone()` IS `util.table.deepcopy(data.raw[t][base])`. A host primitive is that call made on the guest's instruction. Ports that transliterate pass their gate on the first run; ports that reimplement do not.
3. **Cost.** `clone` plus patches crosses order 150 leaves for the whole of a real mod's hidden-prototype file. Whole-prototype marshalling crosses ~2,500, per load, per stage, plus a guest-side value tree of that size. (The 150-vs-2500 is arithmetic from the measured leaf counts; the per-leaf tier-2 microseconds are DERIVED from `agents/abi.md` and are not measured here.)

The corollary is that `set` must reach **arbitrary depth** and must express nil, and both are in the signature.

**One implementation note the design did not anticipate.** `clone` requires `util` LAZILY, inside the primitive, because `require("util")` assigns globals as a side effect and a guest that never clones should not provoke that -- particularly at the settings stage, where `util` is not a global at all.

### D3 -- packaging

Generated files, each gated on the data module actually having the export:

| file | when |
|---|---|
| `fk_data.lua` | a data module is declared. Verbatim, like `fk_abi.lua` |
| `fk_data_module.lua` | a data module is declared. The wrapped data chunk |
| `settings.lua` | the data module exports `fk_settings`, or `[stages] settings` is declared |
| `data.lua` | ...`fk_data` |
| `data-updates.lua` | ...`fk_data_updates` |
| `data-final-fixes.lua` | ...`fk_data_final_fixes` |

Feature-detection per hook is the same discipline `control.lua` already applies to `fk_on_tick` and the collector triple, and it matters: a mod with only a data stage must not get an empty `settings.lua`. The hook list is `factorio.StageHooks`, mirrored against `fk_data.lua` in BOTH directions -- which is the drift `factorio.Hooks` actually had for two milestones.

**Declared by a flag with a manifest default**, the shape every other setting here has:

```toml
[fklua]
data_module = "dist/bbbdata.wasm"      # --data-module IN.wasm overrides
```

**Back-compat is absolute and testable.** With no `data_module` and no `[stages]`, `Files()` returns exactly today's five entries. `TestAProjectWithNoDataModuleIsByteIdentical` is the gate.

**THE CONTROL MODULE IS OPTIONAL WHEN THERE IS A DATA MODULE**, and the gate is `Package.Chunk == ""` -- the same "empty means no guest" reading `DataChunk` has, so the two stages are symmetric rather than one being the trunk and the other a branch. A data-stage-only mod ships `info.json`, `fk_abi.lua`, `fk_data.lua`, `fk_data_module.lua`, the stage files and the included tree; the three that leave are `control.lua`, `fk_module.lua` and `fk_api_gen.lua`, which are the three that describe a running program. **`fk_abi.lua` IS NOT ONE OF THEM**, and the obvious reading of "drop the control stage's files" drops it: `fk_data.lua` opens with `require("fk_abi")` for the tier-2 codec, so a package without it is a mod that will not load, with a message about a Lua module rather than about anything fklua did. `TestTheDataStageShimRequiresTheABI` asserts it against the SHIM's own source rather than against a memorised list, and fails rather than skipping if the require ever goes.

Two things this obliged that are not the file list. `attachAPI` must not be reached, or a data-only package would demand an `fk_api_pin_` export from a module that by design carries none -- the same rule `CheckDataModule` enforces from the other side. And `--persist`, `--gc` and `--fuel` are REFUSED rather than ignored when there is no control module, because each names a property of a control guest's compilation and a data module is `--persist=none` / `-gc=leaking` whatever is asked for. **The typed flag, never the manifest key**: `gcFromFlag` already exists to draw exactly that line, and a key is a statement about the mod its manifest describes, so refusing on the key would stop one checkout packaging a data-only stand-in beside a collected mod -- which is the case this shape was asked for. `--api` is deliberately NOT in the refused set: it names the description the project is built against, which stays true of a package that has nothing to apply it to.

Not taken: unlocking the optional positional on a declared `[stages]` chain as well. A chain with no `@guest` generates a list of requires a hand author writes directly, so `fklua mod` would be doing nothing a copy does not, and "no input module" stays the honest answer for a mod with no guest of either kind. Revisit with a real use case rather than for symmetry.

**The mid-migration collision, and the four options.** A mod's port lands in phases, and while it does, its included tree still carries `settings.lua` / `data.lua` / `data-final-fixes.lua` -- exactly the names this now generates, so `Files()`' collision check fires.

1. *Included file wins.* Refused: a mod whose guest never runs.
2. *Generated file wins.* Refused: a mod whose data stage is silently not the one the author wrote.
3. *Error, plus a second `stage_chain` mechanism.* Two mechanisms for one job.
4. **PICKED -- a declarative stage chain in the manifest.** The generated stage file is an ordered sequence of requires, one entry of which is the guest hook.

```toml
[stages]
data             = ["prototypes.entity", "@guest", "prototypes.sprite"]
data_final_fixes = ["@guest"]
```

`@guest` is the stage hook. An absent key with the hook exported means `["@guest"]`. An absent key with no hook means the file is not generated. A key naming `@guest` where the hook is not exported is an error at package time. A key with no `@guest` is a pure-Lua stage file, which is the far end of the ramp and also what a data-stage-only test mod wants. A key DECLARED as an empty list is not the same as an absent one -- it is a stage file with no requires -- so the map is read for presence, never for length.

The collision stays an error and the message names `[stages]` as the remedy, with the concrete edit in it.

**Every new manifest key gets a flag form** -- `--data-module`, `--stage data=a,@guest,b` -- and `--stage` overrides PER KEY, so naming one chain does not discard the other three. Not for symmetry: one checkout that packages several mods drives them from one Makefile with one manifest describing the shipped one and flags describing the rest.

### D4 -- determinism

The data stage runs per client from mods and settings, and a divergent prototype set is a **join refusal**, not a desync -- Factorio checksums the prototype list and refuses. So the stakes are lower than the control stage's. The rule is still enforced, because a join refusal nobody can reproduce is worse than a desync that fails loudly.

**Everything crosses SORTED, at every nesting level, and that is a deviation UPWARD from the design.** The design said `fkdata.keys` and `fkdata.env(1)` sort. Implementation found the rest of the hole: `fk_abi.lua`'s `write_dyn` writes a table's pairs in `pairs()` order, so a `get` of a nested dictionary would hand the guest an order that is a fact about how the mods happened to load. And it cannot be fixed on the guest side by sorting after the fact without giving up the wire's own meaning.

So `fk_data.lua` carries `write_sorted`, a recursive mirror of `write_dyn` whose only difference is that a dictionary's pairs come out in sorted key order at every level. Scalars delegate to `write_dyn`, which stays the single statement of the tag numbering and of how a string is written; only the two CONTAINER branches are restated, because those are the ones with an order in them. **A key that is neither a number nor a string RAISES**, because two table keys can only be ordered by their addresses and that is a per-run order.

Both guest libraries sort a map on the way OUT too, so what a guest sends is a function of what it meant rather than of the order it happened to build it in.

`Mods()` returns a SORTED SLICE rather than the `map[string]string` the design sketched, and that is the same rule one layer up: Go randomizes a map's iteration order by construction, so a guest enumerating one would produce a different prototype set on different clients. `ModVersion(name)` is the lookup.

**Extend order is the guest's and it is visible in the dump but not to the engine.** Measured both ways: two `data:extend` orders give byte-different dumps and an identical prototype checksum. That is not a determinism bug -- it is why the acceptance gate normalises.

### D5/D6 -- the guest libraries, both languages

Hand-written `guest/go/fkdata` and `guest/rust/fkdata`, in the spirit of `guest/go/fk` and `fkipc` rather than generated. **No generated per-prototype types**: a typed model over 251 prototype types is a whole generator project, the description that would drive it is `prototype-api.json` rather than `runtime-api.json`, and nothing in the acceptance set needs it.

**BOTH LANGUAGES IN ONE ROUND, mirror-tested**, and the reasoning is this repo's own history rather than symmetry. The Rust generator fell four milestones behind, every one of six gaps was found by a mod author rather than here, and one was the same defect in the same function the Go side had already fixed. The fix was not a resolution, it was a diff: `census.json` gained Rust rows so a feature added to one backend is visible in a committed file. **`fkdata` is hand-written, so `census.json` cannot see it at all** -- which makes a Go-only round strictly worse than the generated case that went wrong. The precedent is `fkipc`.

`TestBothDataGuestLibrariesMakeTheSameCalls` drives a Go data guest and a Rust data guest through one `fk_data.lua` against one stand-in `data` table and requires an identical interleaved call-and-effect transcript: every `data:extend` with its argument serialised canonically, every `deepcopy` the clone primitive makes, every log line, in order, and then the whole of `data.raw`.

**It found a real divergence on its first run**, and it is worth carrying: **Go's untyped constants are arbitrary-precision and Rust's `const f64` arithmetic is IEEE f64**. `0.3 + 0.104` folds to `0.40400000000000003` in Go and `0.40399999999999997` in Rust -- two different doubles, from two languages doing the arithmetic correctly. The example guests use a runtime `var`/`let` so the mirror measures the LIBRARIES; the difference did not reach the game here only because a sprite's `shift` is a float32 in the prototype and both narrow to the same f32. It WOULD reach the game for any field the engine keeps as a double.

**One TinyGo constraint found by building.** TinyGo refuses a `//go:wasmimport` function as a value (`cannot use an exported function as value`), so every import is wrapped in an ordinary Go function. A guest author never sees it.

### D7 -- the tests, and the six red proofs

Host-side unit, `internal/factorio`: the byte-identity gate, per-hook stage-file generation, the stage chain's order as a TEXT assertion, the collision message, the `StageHooks` <-> `fk_data.lua` mirror in both directions, and `CheckDataModule`.

Codec, under `bin/lua52f`: `fk_data.lua` driven against a real compiled linear memory with a stand-in guest, covering every tier-2 shape a prototype contains, sorted keys, sorted nested dictionaries, nil-as-delete, deep and cross-type clone, absent-is-not-an-error, the three env reads, and every raising case's message.

End to end, `internal/guest`: TinyGo (and cargo) build the example data guests, the emitter lowers them, the packager writes the mod, and `lua52f` runs the generated stage files against an engine-shaped stand-in.

In game: `scripts/run-datastage.sh`.

**The six red proofs, each injected, observed and reverted on 2026-08-25:**

| injected defect | what fired |
|---|---|
| `fkdata.extend` implemented as a no-op | the golden mismatched; the prototype list checksum moved 1808937236 -> 1127369195; **one** of the guest's prototypes survived in the dump, the one the clone primitive makes through the engine's own extend |
| `fkdata.clone` made a SHALLOW copy | **the engine refused to load**: *"next_upgrade target (fast-transport-belt) must have the same bounding box"* -- the nested `collision_box` patch landed in BASE's own belt -- and no dump was written at all. Host side: the end-to-end `SOURCE box=-0.4/-0.4/0.4/0.4` assertion, and `TestCloneIsDeepAndKeepsUntouchedLeaves` on `shared false` and `frames src 16` |
| `fkdata.keys` returns `pairs()` order | `TestKeysAreSortedNotPairsOrder`, with `keys` and `pairs` reported identical and unsorted |
| a stage hook export removed (`fk_data_final_fixes`) | no `data-final-fixes.lua` in the packaged mod, `item/fkd-token` ABSENT from the dump, golden mismatched. Host side: "the packaged mod has no data-final-fixes.lua" |
| `set` with a nil value implemented as "write false" | `TestSetWithNilDeletesRatherThanWritingFalse`: `minable false`, `next_upgrade false`, `nested.drop false` |
| the back-compat branch removed | `TestAProjectWithNoDataModuleIsByteIdentical`: nine files where a mod with no data module has always shipped five |

**The example guests carry a NESTED patch for the second of those**, and it is the reason it is there rather than in a test: every other patch in them is top-level, and a shallow clone survives all of those. `collision_box` is an array of two arrays, so `Set(..., "collision_box", 1, 1)` reaches two levels down through NUMERIC path elements -- which nothing else in the corpus exercises either.

### The in-game gate

`scripts/run-datastage.sh`, 15 s for both languages, 2.4 s per `--dump-data`. Three assertions, in order of strength:

1. the normalised dump matches a committed golden;
2. the **Go and Rust** guests produce the SAME dump -- two hand-written libraries drift and a census cannot see either;
3. two runs of the same guest produce the same dump.

Plus: the guest's own log lines must be in the run (a dump that matched while the guest never ran is the vacuous pass this has to be able to fail on), and the engine's `Prototype list checksum` is printed and **labelled as a smoke test only**, because it is measured blind to field values.

**The golden line records the MOD SET** as well as the engine, and a mismatch there reads as SKIP rather than as a broken mod: the dump is a function of every mod that ran, and Factorio's bundled DLC data loads whatever `--mod-directory` says, so a machine that owns different DLC produces a different dump for a mod that is perfectly fine.

**The script builds `bin/fklua` itself, every run.** `runtime/lua/fk_data.lua` is embedded in the binary, so a stale `bin/fklua` packages a stale shim -- and that cost a red proof in this round: a defect injected, reverted, and measured again against a binary nobody had rebuilt, which reported the reverted defect as still present. Identical output from a changed input is a bug in the harness until proven otherwise.

### The oracle's `pairs()` order varies BETWEEN RUNS

Found while writing the sorted-keys red proof, and it belongs in `agents/testing.md` as much as here: **`bin/lua52f` seeds Lua 5.2's string hash from the clock, so `pairs()` over string keys comes out in a different order on every invocation.** Six consecutive runs over one four-key table gave four different orders.

It changes nothing about the ABI, which sorts either way, and nothing about Factorio, whose data stage is insertion-ordered and whose `--dump-data` is byte-identical across runs. It changes everything about a host-side test that reasons about a fixture's iteration order: the first draft of `TestKeysAreSortedNotPairsOrder` used four belt names, passed one run, and tripped its own vacuity guard the next. It uses twelve keys now, so a chance sort is one in 12!.

---

## What is NOT in this round

- **Migrating a real mod's data stage.** That is a separate program in a separate repository, gated by the dump.
- **`fklua init` scaffolding for a data guest.** Add it once the shape has survived one real port. Scaffolding a shape nobody has used is how `init` shipped a layout three mods each had to undo by hand.
- **Generated per-prototype Go/Rust types.**
- **A collector or `--persist` for the data guest.** It runs once and dies.
- **Multi-mod manifests.** A real design question, and not on the path. What was done instead is the one thing that keeps the door open: every new manifest key has a flag form.
- **`data-updates` as a distinct concern.** The hook exists and is generated; nothing exercises it beyond the packaging tests, because no example needs it.

---

## Open items

- **The golden has a line per engine and both series are recorded**: 2.0.77 and 2.1.16. base's own prototypes move between series, so a hash taken on one says nothing about the other; the mod-set column is what makes the difference legible, since 2.1 bundles `recycler` and 2.0.77 does not. Both arms produce an identical dump from the Go and the Rust guest, and both are deterministic across two runs.
- **The tier-2 per-leaf cost is derived, not measured.** D-clone's 150-vs-2500 leaf comparison is arithmetic over measured leaf counts; the microseconds are not. If a future round wants a number, measure `read_dyn`/`write_dyn` over a real prototype under `lua52f` before committing to any optimisation.
- **`helpers` is userdata at both stages** and was not enumerated. If a future data-stage need wants `helpers.*`, probe it before designing around it.
- **`load()` and `string.dump` are present at the data stage.** Nothing here uses them and nothing should; a future "just `load()` the chunk" shortcut would bypass the factory shape the whole thing rests on.
- **A guest that hooks several stages pays a fresh instantiation per stage.** That is the design (nothing carries across a stage boundary anyway) and it is also an unmeasured cost: the parse is what the table above bounds, and `_initialize` per stage is not separately measured.
