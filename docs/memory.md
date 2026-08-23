# Memory, the collector and the save

How a guest's heap works: where it lives, why there is a garbage collector in both languages, how to tune or opt out of it, and what survives a save. The defaults are already chosen (`fklua init` writes `gc = "collected"` and scaffolds a guest that carries the collector), so for a first mod there is nothing here to decide; this page is what is behind the defaults.

## Where the heap lives

A guest's linear memory is not native memory. It is Lua tables inside Factorio's `storage`, which means it is serialized into every save, checksummed for multiplayer lockstep, and shipped to every joining client. Three consequences drive everything else on this page:

- **Growth is permanent.** WebAssembly memory never shrinks, so every byte the heap ever reaches is paid for the rest of the save's life.
- **Size is billed per tick, whether used or not.** Factorio's own Lua collector walks the memory's size, not its live part, at about 0.2 ms of worst tick per MiB of linear memory (measured in Factorio 2.0.77, flat from 8 MiB to 128). The full bill is priced in [`agents/guests.md`](../agents/guests.md) under "the guest heap budget".
- **Every client pays it.** The heap is in the save and the join, so a bloated guest costs every player, not just its author.

Linear memory is sharded into 2¹⁹-word Lua tables, which keeps Factorio's collector's worst tick flat at about 0.5 ms out to 40 MiB instead of scaling with the whole memory, and `memory.grow`'s zero-fill is paced behind a cursor so growing is not a stall. What bounds a guest is the per-MiB bill above and wasm32's 4 GiB.

## Why there is a garbage collector, in Rust too

A garbage collector in a Go project surprises nobody. In a Rust project it deserves an explanation, because Rust normally frees memory the moment an owner is dropped, and here it does not: **`dealloc` is deliberately a no-op in both build arms of the `fk` allocator**, so `Drop` never returns memory. Two reasons:

- **Some allocations have no owner.** The host writes into guest memory through the guest's exported allocator: event payloads too large for the scratch region, variable-length return values. No Rust (or Go) value owns those blocks, so no destructor will ever run for them. Only something that traces reachability can find and reclaim them.
- **The heap's layout must be reproducible.** Every client of a lockstep game must compute identical bytes at identical addresses, and a saved heap must come back exactly as written. The two allocator arms that ship are deterministic by construction: the bump arena never reuses an address, and the collector reclaims by tracing at safe points and sweeps in ascending address order, so layout is a function of what the guest did and nothing else.

So the choice per language is:

- **Rust**: the `fk` crate owns the single `#[global_allocator]`. Without the `fkgc` feature it is a bump arena that never frees; `cargo build --features fk/fkgc` swaps it for the collector. No import, no source change.
- **Go**: TinyGo is built with `-gc=custom`, and the `fkgc` import in the scaffolded `gc.go` supplies the runtime hooks that flag requires. TinyGo's own collectors are not used because they run a whole collection at allocation time, inside whatever tick the allocation landed in; `fkgc` collects only at safe points the guest names, in bounded steps. `-gc=leaking` is the bump-arena arm.

## What the collector is

A paced, incremental, conservative mark-sweep. A collection is cut into bounded steps driven from a one-shot `on_tick` that exists only while a collection is in flight, so an idle guest registers nothing and pays nothing. There is no heap cap: collector metadata is about 31 KiB plus about 1% of the heap, and a guest that allocates faster than its budget reclaims grows like a leaking one instead of stalling or trapping.

The number that decides the default, measured in game on the same guest at the same allocation rate: at a 40 MiB heap, the worst `memory.grow` tick is **24.6 ms collected against 974.5 ms leaking**. That is about the growth law, not about reclamation: a leaking guest's memory doubles, and nothing can bound what a doubling costs. The allocation path itself is not slower collected; on an allocating event handler it measured marginally faster than the leaking arm.

## Driving it

The scaffold already does all of this; the knobs exist for when measurement says the defaults are wrong for your mod.

- **Call `CollectIfNeeded()` once per tick, unconditionally.** It returns immediately unless the heap has grown past the threshold, and it is what advances an in-flight collection. A guest that only reaches it from a branch starves its own pacer on exactly the ticks that allocated; the symptom is `Stats().Deadlines` rising rather than a pause, which points you at the wrong knob.
- **`SetThreshold(bytes)`** is when a collection starts: how many bytes may be handed out between collections. Zero restores the default. Safe to call from an initializer.
- **`SetBudget(units)`** is how fast one runs, in granules of heap touched per step. The budget is the pause: a step charges every granule it touches, so raising it collects faster and pauses longer in a straight line. The default is calibrated to about 0.5 ms of host-side work per step; inside Factorio the same work costs roughly 2.5× that. Values below a few hundred are not useful, because a step's fixed costs stop being small next to the work it does.
- **`Stats()`** is the diagnostic surface. `Deadlines` is the sum of two escapes with different remedies: `StallEscapes` means the mark stopped converging, which is a dirty rate above the budget (raise `SetBudget`); `StepEscapes` alone means the mark finished slow for its heap size. If the guest's root set is larger than one step's budget, the collector floors the budget itself and logs one `fkgc:` line saying so.
- **`Start()` and `Collect()`** exist for a guest with its own idea of when a collection is due.

The Rust surface mirrors this under `fk::gc`: `collect_if_needed()`, `set_threshold`, `set_budget`, `stats`, and without the `fkgc` feature every one is a no-op shim, so guest code is identical in both arms.

One rule of engagement matters beyond tuning: a `(ptr, len)` handed to the host must be consumed before that call returns. It is guest heap, the conservative scan cannot see the host holding it, and a buffered one is a use-after-free the next time a collection runs.

## The leaking opt-out

`gc = "leaking"` in `fklua.toml`, and build without the collector (`-gc=leaking` for TinyGo, drop `--features fk/fkgc` for Rust). Changing the key or the build alone is a refusal at package time that names both sides, because the key is a claim about how the guest was built.

It is a real option for an allocation-disciplined guest, and the only option for a `wasip1` guest. What it buys back is the collector's own emitted code, measured downstream at +32.4% of the generated Lua and +13.7% of the zip. What it does not buy back is the growth law above, so choose it on a measurement of your own heap over a long session, not on a prediction about your own tidiness.

## What a save carries: `--persist`

`fklua mod --persist=MODE` decides how guest memory is carried in the save:

| Mode | What it does | Cost |
|---|---|---|
| `table` (default) | the saved structure is the live memory; stores land in it with no sync step | 2.29 bytes per 32-bit word in every save and every multiplayer join |
| `packed` | the live memory is mirrored into `string.pack` pages after each guest call, and only pages actually written are repacked | **0.44 bytes per word** saved (5.2× smaller); about 40 µs per dirty page per guest call. A downstream mod on a large map measured 13.8× smaller saves and 2.6× faster loads |
| `auto` | picks between the two by declared heap size (threshold 1 MiB) and prints its choice | the threshold is a proxy for write locality, which the compiler cannot know |
| `none` | memory is rebuilt from the module's data segments on every load | nothing survives; deterministic but stateless |

`--persist` is a packaging flag, not a manifest key, and the mode is independent of the collector: both `table` and `packed` run the guest on the same live tables.

## Recompiling, and telling the guest about it

A save records a build id, a hash of the guest wasm and the API pin it was packaged against. Recompiling the guest, or repackaging the same wasm against a different pin, moves it, and a heap written by one build and read by another is undefined rather than merely stale (every address and layout may have moved). So on a mismatch the old heap is discarded and the loss is logged, unless the guest exports a migration hook:

- **`fk_migrate(old_version)`** is a notification on a fresh heap: you are told the state version the save carried and rebuild your state from the world. This is what almost every guest wants, and rebuild-from-world needs nothing else.
- **`fk_migrate_adopt(old_version)`** hands the old bytes over instead, for a guest whose state is a fixed versioned region it interprets itself. Most guests should never export it: linear memory includes `.data` and `.rodata` as well as the heap, so a rebuilt guest reading an adopted image is reading the previous build's string constants and type descriptors by compiled-in address.

The handling runs on the first dispatch after the load, so it works for the commonest rebuild there is: a development rebuild that keeps the mod's version, which Factorio raises no configuration event for.

A change to the mod set around your guest is a different signal with its own hook: export `fk_on_configuration_changed()` (no arguments) and it is dispatched whenever Factorio raises `on_configuration_changed`, after any migration handling on a load that is both. It is replicated, so it may write guest state, and it also runs on the load that adds your mod to an existing save, right after `fk_on_init`, so make it idempotent over your init. A guest wanting to know what moved reads `script.active_mods` against what it saved.

The whole round trip is verified inside a real Factorio: a headless server honours `game.auto_save()`, so [`scripts/run-roundtrip.sh`](../scripts/run-roundtrip.sh) makes real saves mid-game and loads them back, including mid-mark and mid-sweep with the collector running, a guest that grows past its initial memory, and a rebuilt guest that exports `fk_migrate`.
