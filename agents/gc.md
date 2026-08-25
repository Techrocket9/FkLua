# An incremental collector for guest heaps — stage A: design and measurement

**Verdict: GO, on a barrier nobody has to build.** The write barrier this feature needs already exists, is already maintained by every writer, and is already read on every store in every persistence mode. Arming `MEMDIRTY` while a collection is marking makes the dirty-page set the collector's card table at **exactly zero idle cost** — the emitted chunk of a guest that is not collecting is byte-for-byte what it is today — and the measured mutator cost while marking is 1.00×–1.13× depending on how store-heavy the guest is.

That is not what the sketch this stage was asked to stress-test expected, and the difference matters: the sketch weighed "always-on page-set maintenance" (a) against "helper swap plus a flag-checked inlined store" (b). (a) costs 8–15% on store-heavy guests and fails the go/no-go on its own terms. (b) is unnecessary, because the helpers do not need swapping — they already test the flag — and the inlined 4-byte store already carries the mark in every mode. What is left after subtracting what already exists is **one hole and one flag**, and both are small and precisely located.

Read `CLAUDE.md`, [`agents/guests.md`](guests.md) ("the guest heap budget"), [`agents/optimizer.md`](optimizer.md) (the inlined stores and the loop guard) and the dirty-page-set block in `runtime/lua/fk_rt.lua` before this file. This one assumes all four.

**THE RUST COLLECTOR IS BUILT, and `--gc=collected` is no longer Go-only.** `guest/rust/fkgc` is the same design in the other language; read "The Rust collector, as built" at the very bottom for the go/no-go answer (the root set — the recorded objection's premise was true and its conclusion was not), the two places the two collectors genuinely differ, and the cross-language mirror table.

**Stage D is BUILT, and the milestone is CLOSED.** The in-game instrument is a real one — per TICK, not per run — and the first honest run of it found stage C's paced arm **livelocked in a real Factorio**. The paced-vs-stop-the-world comparison is now reproduced in the game (7.4× on the worst tick), the first guest outside this repo measured both arms and shipped **leaking** — and then re-measured on the sharded pin and **flipped to collected**, which is the verdict that stands; see "The first real guest, and its verdict" below for both, because what moved between them was not the collector — and three numbers came out worse than this document predicted. Read "Stage D, as built" at the BOTTOM first if you are here about what a collection costs — and **do not quote stage B's ms/MiB band as an in-game figure**, because stage D measures 2.7×–6.4× more than it.

**Stage C is BUILT.** The collector is INCREMENTAL: marking runs behind the armed `MEMDIRTY` barrier, sweeping runs in bounded steps and lazily on allocation, and both are paced from a one-shot `on_tick` that exists only while a collection is in flight. The results are in "Stage C, as built" — read that if you are here about pacing. Everything from "Stage B, as built" down to it is stage B as written.

**Stage B is BUILT.** `guest/go/fkgc` is a working conservative mark-sweep collector, `--gc=collected` is on `compile`, `mod` and `spectest`, and the results are in "Stage B, as built" below — read that before the staged plan, because two of stage A's estimates moved and one of its recommendations was kept for a reason it did not anticipate. Everything above it is stage A as written, left unedited: it is the reasoning the GO verdict was given on, and editing it after the fact would make the plan look better than it was.

---

## Why this exists

`-gc=leaking` is mandatory today and [`agents/guests.md`](guests.md) says why: a collector's pauses land in a lockstep simulation. So a guest's memory is an arena that only grows, and a guest author's job is to make sure it grows slowly — the advice in "the guest heap budget" is *allocate less, and allocate it once*, and the first downstream mod's whole idle GC tail turned out to be log lines built with `+`.

That advice works and it should not be necessary. `guest/go/examples/churn` is an ordinary allocating event handler — a per-event slice, a per-event map keyed by a per-event string, a formatted line — written the way Go is normally written. It keeps **2,016 bytes per event, forever**. At one build event per tick that is 121 KB/s; a 200-entity blueprint paste is 403 KB in a single tick. The downstream mod's pre-diet figure was ~8.8 KB/event, so `churn` is not a worst case, it is a *restrained* case.

**The cost of that is not the bytes, it is the ladder.** TinyGo's `growHeap` doubles, `mem_grow` zeroes every new word, and Lua 5.2 walks the whole word table in one `propagatemark` it cannot split — 0.2 ms of worst tick per MiB, measured flat from 8 MiB to 128. And **linear memory never shrinks**: wasm has no `memory.shrink`, `MEMSIZE` is authoritative on the Lua side, and a table that has held 16 million slots is a table that will be walked as 16 million slots for the rest of the session.

That last fact sets the collector's success metric, and it is not the obvious one:

> **The collector's job is to prevent `memory.grow`, not to free bytes.** Every doubling avoided is 0.2 ms per MiB of permanent worst tick that no later collection can give back. A collector that reclaims everything but lets the heap double first has bought nothing.

Two consequences fall straight out and both are design decisions, made here:

- **Allocation is free-list-first, bump-second.** The usual argument for bump-first is locality, and it is real. It loses to the argument above: a bump pointer that walks past the end of the heap grows it permanently, so a free block that fits is always the better answer even when it is worse for cache.
- **Fragmentation is a heap-SIZE problem, not a wasted-bytes problem.** Without compaction, a heap that fragments grows; a heap that grows never un-grows. Size classes are therefore not an optimisation here, they are the mechanism that keeps the ladder from being climbed. Stage D's acceptance test is a `churn` guest that runs indefinitely with **no doubling logged**.

---

## Point by point against the sketch

### 1. Collections run only between guest calls — **CONFIRMED, and it is worth more than it looks**

The wasm operand stack is empty between exported calls, and under `-scheduler=none` there is no other frame. What that leaves as roots is: mutable globals, the statics/data region, and the shadow stack — and the shadow stack is **empty**.

Verified rather than argued, in two independent ways.

*The module has exactly one mutable global and it is the stack pointer.*

```
$ wasm-objdump -x k-go.wasm
Memory[1]:  - memory[0] pages: initial=2
Global[1]:  - global[0] i32 mutable=1 <__stack_pointer> - init i32=65536
```

*And it is back at its initial value after every exported call.* Driving the real bench guest under `bin/lua52f` and reading the persist surface's `globals()` mirror between calls:

```
before _initialize: 65536
after  _initialize: 65536
after  pure_setup : 65536
after  pure_sum   : 65536
after  real_names : 65536
```

`__stack_pointer`'s initial value **is** `stackTop`, because `arch_tinygowasm.go` sets `stackTop = __global_base` and `wasm-unknown.json` passes `--stack-first`, so the shadow stack occupies `[0, 65536)` and grows down from the top. TinyGo's own `markStack()` scans `[getCurrentStackPointer(), stackTop)`. Between calls that range is empty, and a build with `-gc=custom` says so directly — a probe guest that calls `runtime.markStack` from an exported function and records what `runtime.markRoots` is handed reports:

```
roots reported: 2
range0: 65536 65536   len 0        <- the stack: EMPTY
range1: 65536 327744  len 262208   <- the globals, [__global_base, __heap_base)
```

**So the root set at collection time is one contiguous byte range**, `[__global_base, __heap_base)`, and TinyGo hands it over itself via `findGlobals`. For a real guest that range is small: the bench guest's data section is 21 bytes plus `.bss`, and the downstream mod's 73 data segments end at address 69,869 — about 4 KiB above `__global_base`. (The 262 KiB above is the probe's own static heap array, which a real design would not put there. See the metadata question in §6.)

The mutable-global root is a non-issue for a second reason: the one global holds a stack pointer, not a heap pointer, and the runtime already mirrors it into `storage` after every call.

**The consequence the sketch under-sells.** Because a collection cannot begin during a guest call and cannot be interrupted by one, the barrier does not have to be an incremental-update or a snapshot-at-the-beginning barrier in the technical sense. It has no ordering obligation with respect to the marker at all. It only has to answer one question, asked at a point where the mutator is not running: *which cards did the calls since the last step dirty?* Everything about ordering, publication and the tricolour invariant that makes a real incremental barrier subtle is absent here, and that is a property of Factorio's dispatch model rather than of anything this project built. **Write it down as a precondition**: if a guest ever gains a way to be re-entered mid-collection, this simplification dies and the barrier's obligations change.

#### wasip1 — **supportable in principle, GATED for stages B–D**

The sketch's caveat asks whether a parked goroutine's stack is findable. It is, and by the ordinary mechanism:

```go
// internal/task/task_asyncify.go
func (s *state) initialize(fn uintptr, args unsafe.Pointer, stackSize uintptr) {
	stack := runtime_alloc(stackSize, nil)
```

A goroutine's stack is an ordinary heap allocation from the app's own `alloc`, and the task holding it is reachable from `currentTask` and `runqueue` — both package-level globals, both inside the range `findGlobals` already reports. TinyGo's own comment in `gc_stack_portable.go` says as much: *goroutine stacks are heap allocated and always reachable in some way (for example through internal/task.currentTask) so they will always be scanned*. A conservative collector that scans every reachable block's contents therefore reaches a parked goroutine's roots without knowing goroutines exist. `tinygo build -target=wasip1 -buildmode=c-shared -gc=custom` builds, with goroutines and channels, and the asyncify trio (`tinygo_unwind`/`launch`/`rewind`) survives.

Two hazards, both specific and both reasons to gate rather than to claim support:

- **Interior and one-past-the-end pointers.** The task's `stackState` holds `asyncifysp = stack + 8` and `csp = stack + stackSize` — one interior pointer and one pointer *one past the end of the block*. A conservative mark that only accepts base pointers keeps the block alive only through `canaryPtr`, which happens to be the base; that is luck, not design. **The collector must handle interior pointers**, and stage B should assert the one-past-the-end case explicitly rather than inheriting it.
- **Nothing here has been run under a collector.** wasip1's own cost is already 27.5 ms/tick for a goroutine-per-tick guest, so it is not the target this feature is for.

**The decision: `--gc=incremental` is refused for a wasip1 guest in stages B–D, with a diagnostic naming this section.** Not because it cannot work — the evidence above says it can — but because the acceptance vehicle is a `-scheduler=none` event handler and shipping an untested second root-discovery path is how a soundness bug gets into a lockstep game.

**And it is the NARROWER of the two refusals, which is worth stating because it was the only one for a while.** This section is about a guest that *has* a collector and whose roots are hard to find. The general precondition is prior to it and much duller: the module has to carry the collector at all, i.e. export `fk_gc_step`/`fk_gc_dirty_base`/`fk_gc_dirty_cap`. Checking only the wasip1 half accepted `--gc=collected` for every guest with no collector in it — every Rust guest, and every Go guest built the default `-gc=leaking` way — which is not harmless, because the flag gates the emitter and arms `control.lua`. `checkGC` now asks both, wasip1 first (fkgc links under wasip1, so the surface check would wave such a guest through), and reads the export list from `factorio.CollectorSurface()` so it and `fk_mod.lua` cannot drift.

### 2. `-gc=custom` — **CONFIRMED, and it is the right integration point**

TinyGo 0.41.1 has it (`-gc` accepts `none, leaking, conservative, custom, precise, boehm`), and `src/runtime/gc_custom.go` is the contract. It builds for this target with these flags — verified by building it, not by reading the flag list:

```
tinygo build -target=wasm-unknown -scheduler=none -gc=custom -opt=2 -o out.wasm .
```

The module is a reactor (`_initialize` exported, no `_start`), keeps the single `__stack_pointer` global, and `fklua compile --opt=3` accepts it unchanged.

**The hook surface is seven functions the application provides**, all by `//go:linkname` into `runtime`:

| hook | signature | called when |
|---|---|---|
| `initHeap` | `func()` | from `wasmEntryReactor()`, i.e. inside `_initialize`, after `heapStart`/`heapEnd` are set |
| `alloc` | `func(size uintptr, layout unsafe.Pointer) unsafe.Pointer` | every heap allocation; must return **zeroed** memory |
| `free` | `func(ptr unsafe.Pointer)` | nothing on this target calls it, but it must exist |
| `markRoots` | `func(start, end uintptr)` | from `markStack()` and from `findGlobals(markRoots)` |
| `GC` | `func()` | only from user code |
| `SetFinalizer` | `func(obj, finalizer interface{})` | only from user code |
| `ReadMemStats` | `func(ms *runtime.MemStats)` | only from user code |

and **two the runtime provides**, which is the half that makes this cheap: `runtime.markStack()` scans the shadow stack, and `runtime.findGlobals(found)` hands over `[globalsStart, heapStart)`. Both are compiled for `gc.custom && tinygo.wasm` — `gc_stack_portable.go`'s build tag names `gc.custom` explicitly. `runtime.trackPointer` and the `stackChainStart` chain are compiler contracts that come along for free.

Three things about the contract are load-bearing and none is obvious:

- **The runtime never calls your collector.** There is no allocation-failure callback. `alloc` returning is the only contract; deciding to collect is entirely the application's, which is exactly what a tick-paced collector wants.
- **`setHeapEnd` is a documented no-op under `gc.custom`** (`// Heap is in custom GC so ignore`). `growHeap()` will still grow linear memory and then throw the new bound away, so **the allocator owns `memory.grow` and its own heap bound**. Under `gc.leaking` the runtime owns both; that is the whole migration.
- **`layout` is unused** and passed as an opaque pointer. This is what forces the marking to be conservative — see §3 — and it is a fact about TinyGo, not a choice this project gets to make at `-opt` time.

The alternatives in the sketch are worse and can be closed. *Replacing the allocator by linkname/build tags* is what `-gc=custom` **is**, done without the supported seam. *A runtime-side collector with guest-exported layout info* needs the layout information TinyGo does not give the application in the first place, and would put a mark bitmap in a Lua table — which is more of the object whose size is the problem this feature exists to solve.

### 3. Conservative marking — **CONFIRMED, and it is forced rather than chosen**

`alloc`'s `layout` argument is unused under `gc.custom`, and `gc_blocks.go` — where TinyGo's own precise marking lives — is `gc.conservative || gc.precise` and is not compiled here. So the application has no map of which words in a block are pointers. Conservative is the only option, and false retention is the price.

No compaction, therefore: a conservative collector cannot move what it cannot prove is a pointer. Mark-sweep into size-class free lists, as the sketch says.

**The fragmentation story, stated rather than waved at.** Because memory never shrinks, a free list that cannot satisfy a request costs a permanent doubling. Three commitments follow, and they should be measured in stage C rather than assumed: size classes to a power-of-two-ish ladder so a small request never splits a large block it cannot re-coalesce; free-list-first allocation; and adjacent-block coalescing during sweep, which is cheap because sweep already walks the bitmap in address order.

### 4. Pacing via the one-shot `on_tick` — **CONFIRMED, and the machinery is already built and already proven**

`fk.defer` is exactly this: `arm_deferred` registers a one-shot `on_tick`, the flush calls `off_event` on **itself** before dispatching so a re-arm inside the flush is not torn down by a teardown that has not happened yet, and steady state is no registration at all. `TestManyEventsInOneTickFlushOnce` and `TestDeferredWorkSurvivesASaveTakenBeforeItRuns` already hold both halves.

So an idle guest with a small heap pays literally zero: no registration, no per-tick call, and the barrier flag it reads on every store is the same `false` it reads today. **A collection step is a `fk.defer` with a different payload**, and the only new machinery is a second armed flag in `storage` beside `storage.fk_deferred`.

One correction to the sketch's phrasing: the flush lands on the **following** tick, because `on_tick` for the current one has already been raised. That is already documented for `fk.defer` and it is not a problem for a collector — a step that runs one tick later is still a step — but a pacing calculation that assumes "this tick" is off by one at every step.

### 5. The barrier — **the sketch's framing is wrong, and the right answer is cheaper than any of its candidates**

This is the section the stage exists for.

#### What already exists

Every writer of guest memory already tests a flag and marks its own span. That is not new work for this feature; it is the invariant `--persist=packed` has been built on since M6 and audited into its current form in `79f2318`:

| writer | marks? | where |
|---|---|---|
| `st8b` / `st16` / `st32` | yes | `if MEMDIRTY and (a < DPLO or … > DPHI) then MEMPACK.mark(…) end` |
| `st64` aligned path | yes | same, in `fk_rt.lua` |
| `mem_copy` / `mem_fill` / `fk_wstr` | yes | one mark for the whole span |
| `-opt=3` inlined **i32 store** | yes | `emitInlineStore32` emits the line, **in every mode** |
| `-opt=3` **guarded loop store** | yes | the loop guard hoists one unconditional mark to loop entry, **in every mode** |
| `-opt=3` inlined **8-byte store** | **NO, except under `--persist=packed`** | `inlineWideStores` is gated on the mode instead |
| `grow` | exempt | appends zeros, and an absent page restores as zeros |
| `MEMPACK.restore` | exempt | installs an image wholesale and clears the set |

Confirmed against the emitted chunk rather than against the source. Scanning the real TinyGo bench guest at `-opt=3 --persist=table` for a `MEM[…] = …` with no `MEMDIRTY` test above it returns exactly nine sites: one is the module's initial zero-fill, six are inlined 8-byte stores, one is a guarded arm whose hoisted mark is further than six lines back — and one is `g95`, a loop guard declared and read but **never assigned**, so its guarded arm was dead and no hoisted mark was emitted for it.

**That last one was an unrelated pre-existing defect in the loop-guard pass, and it was fixed on `master` in `4c83390` while this stage was in flight.** The tables below were taken at `519dcb7`, so the question is whether they moved. They did not, and it was checked rather than assumed: on `master` the same guest emits the same 8 guarded store arms and the same 26 inlined i32 stores, gains one hoisted mark (3 → 4), and **produces byte-identical `MEMPACK.mark` and store-leaf counts in every timed window** — 1,113,721/2,800,000 for `real_grid` and 799,747/2,577,778 for `real_names` under both. The revived loop is in a setup function, which the harness's zero-run subtracts. A stage-B re-measure should still re-run `run-all.sh` against whatever `master` is by then rather than trusting this paragraph.

So in table mode today, with `MEMDIRTY = false`, **every store already pays one upvalue read and one short-circuited `and`** and the whole marking apparatus is one boolean away from being live. The sketch's candidate (b) proposes to build that from scratch: swap the helpers out of the function table, add a flag test to the inlined store. Both are already there.

#### The measurements

`scratchpad/gc/` holds the harness; `run-all.sh` reproduces every table below. Variants are produced by rewriting the **emitted chunk**, which is the instrument CLAUDE.md's M12 row records for the five ideas that were "prototyped by hand-editing real emitted Lua and timed". Every cell is checksum-gated, paired, interleaved, and reported as a ratio of medians with a bootstrap CI; `aa` is the same variant against itself and **a ratio inside the A/A interval is not a measurement**.

The cells:

| cell | what it is |
|---|---|
| `pageset` | sketch (a)/(c): `MEMDIRTY` armed — 4 KiB pages, the runtime's own marking. Also **the during-marking cost of the recommended design.** |
| `card64k` | the same with a 64 KiB card, i.e. what a GC-only card table could choose |
| `pageset2` | the same with a two-entry cached-page test instead of one |
| `flagstore` | sketch (b1): a **second** flag-checked branch in every inlined store, guarded arms included, flag false |
| `flagunguard` | (b1) without the guarded arms, to price them separately |
| `callstore` | sketch (b2), partial: unguarded inlined i32 stores back to `st32` calls |
| `widegate` | sketch (b3): the wide-store gate, which is not a rewrite — it is what `--persist=packed` already emits |
| `aa` | the noise floor |

**Idle cost — the real guest benchmarks, `-opt=3`, `--persist=table`, reps=2, 15 interleaved samples.** Ratios against the unmodified chunk; below 1.00× is faster. `bin/lua52f`, quiet machine.

| kernel | base ms | `pageset` | `card64k` | `pageset2` | `flagstore` | `flagunguard` | `callstore` | `widegate` | **A/A** |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| `pure_sum` | 44.50 | 0.988 | 0.986 | 0.994 | 0.989 | 0.980 | 0.998 | 0.984 | **0.992** |
| `pure_prng` | 164.12 | 1.010 | 1.000 | 1.002 | 1.001 | 1.009 | 1.016 | 1.020 | **1.003** |
| `pure_dot` | 168.95 | 1.001 | 1.002 | 1.008 | 1.002 | 1.003 | 1.002 | 1.001 | **1.001** |
| `real_entities` | 149.84 | 1.014 | 1.016 | 1.022 | 1.023 | 1.005 | 1.017 | 1.017 | **1.012** |
| `real_grid` | 853.26 | **1.130** | 1.085 | 1.060 | 0.999 | 0.993 | 0.998 | 0.997 | **0.995** |
| `real_names` | 1254.75 | **1.073** | 1.067 | 1.074 | 0.996 | 0.994 | 0.991 | 1.013 | **0.992** |

**The A/A floor is ±1.2% on the point estimate**, and the widest 95% interval any A/A cell produced is `pure_sum`'s [0.973, 1.026]; on the two long kernels, where the signal is, it is [0.979, 1.011] and [0.981, 1.000]. So this table resolves about 2% on `real_grid`/`real_names` and about 3% elsewhere, and everything inside that is reported as "at the floor" rather than as a number.

The intervals for the two cells that are not at the floor: `real_grid` `pageset` [1.114, 1.147], `real_names` `pageset` [1.063, 1.084].

**Idle cost — the allocation-churn guest**, 5,000 events, 9 samples:

| | base ms | `pageset` | `card64k` | `pageset2` | `flagstore` | `flagunguard` | `callstore` | `widegate` | **A/A** |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| `churn_events` | 1219.80 | **1.098** | 1.088 | 1.093 | 1.009 | 1.002 | 1.018 | 1.015 | **0.998** |

Intervals: `pageset` [1.090, 1.106], `flagstore` [0.996, 1.016], A/A [0.989, 1.009].

**Idle cost — the `bench --opt` kernels**, `-opt=3`, 15 samples. Every cell on every kernel is inside its own A/A interval, and the A/A intervals here are ±2–3% because these kernels are short and the zero-subtraction is a larger fraction of them. **This table resolves nothing below about 3% and is reported for completeness rather than as evidence**: `sum` 0.984–0.995 across all cells against an A/A of 0.988, `chase` 0.994–1.021 against 1.000, `prng` 0.993–1.006 against 0.997, `dot` 0.986–1.008 against 1.003, `fib` 0.989–1.015 against 1.002, `frame` 0.966–1.024 against 1.013, `count` 1.001–1.021 against 1.009, `constdiv` 1.008–1.031 against 1.014. The real guests carry the verdict.

#### What the numbers say

**Sketch candidate (a) — always-on page-set maintenance — fails, by 13%.** And the failure is attributable rather than mysterious. Counting `MEMPACK.mark` calls against store-leaf entries:

| kernel | `MEMPACK.mark` calls | store-leaf entries | ratio |
|---|--:|--:|--:|
| `pure_sum` / `pure_prng` / `pure_dot` | 0 | 1 | — |
| `real_entities` | 81 | 60,001 | 1 in 740 |
| `real_grid` | 1,113,721 | 2,812,001 | **1 in 2.5** |
| `real_names` | 799,747 | 2,577,779 | **1 in 3.2** |
| `churn` (5,000 events) | 1,049,872 | 866,545 | **more than one per leaf store** |

The two-compare fast path is not the cost. **`DPLO`/`DPHI` are a one-entry direct-mapped cache, and a Go allocator's write pattern has a working set of several regions** — a byte store into a grid, the next into a header; a string buffer, then the map bucket that indexes it. `real_grid` leaves the cached page on two stores in five. `churn` exceeds one mark per leaf store because the inlined i32 stores also mark and they are not counted as leaf entries.

That is a finding about the **shipping** `--persist=packed` mode as much as about this feature: a packed-mode guest pays 7–13% on store-heavy work today, and nothing in the repo says so. Two caveats on transferring it, and both point the same way. These kernels are one long call, where real packed flushes after every guest call and `dirty_clear` resets `DPLO`/`DPHI` — so a real packed guest misses the cache at least once more per call than this measures, and **7–13% is a lower bound rather than the figure**. And this is the store side only; the flush's ~40 µs per dirty page is on top and is already documented. It also says why the obvious mitigations do not rescue candidate (a): a 64 KiB card (`card64k`) takes `real_grid` from 1.130 to 1.085 and does nothing for `real_names` or `churn`, and a two-entry cache (`pageset2`) takes `real_grid` to 1.060 and does nothing for the other two. The distances are larger than any small cache.

**Sketch candidate (b1) — a flag-checked branch in the inlined store — costs at or below the floor.** `flagstore` adds a *second* `if FLAG then …` to all 26 inlined i32 stores **and to all 8 guarded loop arms**, and lands at 0.989/1.001/1.002/ 1.023/0.999/0.996 on the six guests and 1.009 on `churn`. The only cell outside its A/A interval is `real_entities` (1.023 against an A/A of 1.012), i.e. about 1%. `flagunguard` — the same check without the guarded arms — is 1.005 there, so what little there is comes from putting a test back into the loops the guard emptied, which is exactly where it should come from.

**Sketch candidate (b2) — gating inlining off — surrenders nothing measurable on real guests, which retires it as a live option rather than recommending it.** `callstore` (unguarded inlined i32 stores back to `st32`) is at the floor on all six; `widegate` (the wide-store gate `--persist=packed` already applies) is at the floor except `real_names` at 1.013. That is consistent with what CLAUDE.md already records — the inlined i32 store is "the weakest of the three", 0.988× on `chase` — and it means **b3, the packed-mode precedent, is available and cheap.**

#### The design, and why it is not any of the sketch's candidates

The sketch asked which barrier to build. The answer is that the barrier exists, and the only decision left is when to turn it on.

> **Arm `MEMDIRTY` while a collection is marking. Disarm it when the collection finishes. That is the barrier.**

- **Idle cost is zero, not "≤1%".** A guest that is not collecting emits and runs a chunk that is byte-for-byte what it runs today: `MEMDIRTY` is `false`, every store short-circuits on it after one upvalue read, and that read is a cost the project already pays in every mode and has already accepted.
- **The helpers need no swap.** Sketch candidate (b)'s function-table swap exists to give the helpers a barrier they do not have. They have one.
- **The guarded loop stores need nothing either, and this is the part that makes the design work at `-opt=3` rather than in spite of it.** The loop guard already hoists one unconditional `MEMPACK.mark` over the loop's whole proven span, in every persistence mode, and emits no per-store marking inside the guarded arm. A per-store barrier — any per-store barrier, including (b1) — would put work back into precisely the loops that pass exists to empty. The page-set barrier is the only candidate that inherits the hoist instead of fighting it, and over-marking a span a loop exits early from is safe in the same direction it is safe for packed: a re-scanned card, not a missed write.
- **The mark is already sound under early exit and under re-entrancy**, because §1 says a collection cannot begin inside a call.

**One hole, and it is the inlined 8-byte store.** In `--persist=packed` it is gated out of line; in every other mode it is expanded and writes `MEM` with no mark at all. A collector armed in table mode would silently miss every `i64`/`f64` store — the same failure shape as the three the persistence layer has already been bitten by, and undetectable except as a wrong answer later. Two fixes, both with a precedent in this repo and both measured:

| fix | precedent | cost |
|---|---|---|
| emit the mark line into the inlined wide store | the 4-byte store, which does this | `flagstore`: at the floor, ≤1% on one kernel |
| gate `inlineWideStores` on `--gc=incremental` too | the packed mode gate | `widegate`: at the floor except `real_names` 1.013 |

**Take the second.** The numbers are indistinguishable, so the tie is broken by the argument `fk_rt.lua` already makes at the `MEMDIRTY` declaration and CLAUDE.md repeats: *an invariant maintained in two places is an invariant that drifts*, and the objection to a second copy of the marking rule in generated Lua "was never mechanical". Gating costs a real but small win in one mode, for one flag, and `TestPackedModeKeepsTheEightByteStoreOutOfLine` shows what the gate's test looks like. It also has a property the first fix does not: **a guest that does not opt in to the collector is bit-identical to today**, because `--gc=incremental` is a compile-time flag and the gate only applies under it.

That is the whole emitter change this feature needs: **one existing gate, widened by one condition.**

### 6. Persistence and determinism — **sound, with three named obligations**

**Sweep and free-list writes go through the store funnel by construction**, because the collector is guest Go compiled by the same emitter: its stores are ordinary guest stores and mark their own cards exactly like any other. Round 3's invariant ("every writer marks") is not weakened, it is inherited.

That is also a problem, and it is the one genuinely new hazard this design has. **The collector marking cards while it marks objects would make it chase its own tail**: the mark bitmap and the gray list are guest memory, so writing them dirties the very cards the next step would re-scan. Three ways out, and the choice is made here:

- Put the bitmap and the work list in a **statically reserved region** — a package-level Go array, which lands in `.bss` below `__heap_base`, outside the heap range the re-scan is intersected with. A 16 MiB heap at a 16-byte granule needs a 128 KiB bitmap; 512 KiB covers 64 MiB. `.bss` costs nothing in the wasm binary but it *is* linear memory, so it is 0.1 ms of worst tick — which is the right trade against re-scanning forever, and it is a number that has to appear in the budget rather than hide.
- Exclude `[globalsStart, heapStart)` from re-scan, which is free: the globals are re-scanned wholesale at every step anyway, and they are **576 bytes** for the `churn` guest and ~4 KiB for the downstream mod — 144 words, measured, not an estimate.
- Do **not** try to filter the card set by "is this card collector metadata" at mark time. That is a test per card in the hot path to avoid a layout decision.

**A save mid-collection carries the collection**, for free, in `table` and `packed` alike — the bitmap, the gray list, the free lists and the allocator's bookkeeping are all guest memory. What is *not* guest memory is two things, and both need explicit carriage:

- **`MEMDIRTY` is a chunk local**, armed by `control.lua`. After a load it is `false` again. If a collection was marking when the save was taken, the guest resumes marking with the barrier off and the collector loses every write made after the load. `storage.fk_gc_marking` must exist and `fk_mod.lua` must re-arm from it — the same shape `storage.fk_deferred` already has, and `fk_after_load` is the hook.
- **The one-shot `on_tick` registration**, for the same reason `fk.defer` needs `storage.fk_deferred`: Factorio does not serialize event registrations.

**Determinism holds, and the one place to be careful is already solved.** The collector is entirely in-sim, so nothing about it varies by client — with one exception, which is the same exception the dirty-page set already found: the page set is a Lua table, and **the GC must consume `DPQ` (the first-touch-order list), never `pairs(DPG)`**. `fk_rt.lua` says why for the flush — insertion order shapes a Lua table and what lands in `storage` is CRC'd and multiplayer-synchronised — and the reason applies unchanged to a collector whose free-list layout depends on the order it swept. The free list itself is deterministic because sweep walks the bitmap in index order.

Nothing host-side feeds back into guest-visible state. `collectgarbage("count")` is a host-memory number that differs between machines and must not reach the pacing decision; pacing is by tick count, and there is no clock in the sandbox to be tempted by.

**The two consumers of one page set need separate clear points.** In `packed`, `MEMPACK.flush` clears the set after every guest call. A collector armed in the same session would find it empty. The fix is small and costs nothing per store: when the GC is armed, `flush` appends the page list it is about to clear to a GC queue instead of dropping it. That is a runtime change with no emitter consequence, and it is stage D's work, not stage C's.

---

## Throughput, and the race

### The reference implementation already exists, and running it is the strongest result in this document

TinyGo ships `-gc=conservative`: a real conservative mark-sweep, on this target, today. It builds for `wasm-unknown -scheduler=none`, `fklua compile --opt=3` accepts it, and it runs the `churn` guest under `bin/lua52f`.

| `churn`, 5,000 events | `-gc=leaking` | `-gc=conservative` |
|---|--:|--:|
| linear memory at the end | 4,194,304 words = **16 MiB** | 32,768 words = **0.125 MiB** |
| allocator bump pointer | 10,148,736 | 128,016 |
| time | 1256.8 ms | 2852.2 ms (**2.27×**) |
| checksum | 442938 | **442938** |

At 2,000 events the ratio is 2.34× and the checksums agree again (179694). **The same computation, the same answer, 128× less linear memory** — which in Factorio's terms is the difference between 3.2 ms of worst tick per Lua collection, forever, and nothing measurable.

That single table is the feature's premise, demonstrated rather than argued, and it settles the risk this design was most exposed to. **Conservative marking on this target does not over-retain.** The independent measurement agrees: reading the `churn` guest's live word table straight out of the persist surface after 5,000 events, only **5.9% of heap words fall inside the heap range at all**, 5.0% are granule-aligned, and they point at 10.4% of granules. The heap is dominated by string bytes and small integers, not by plausible pointers.

The conservative root region is 576 bytes (`__global_base` 65536, `__heap_base` 66112) — 144 words to scan per step, which rounds to nothing.

**What the reference cannot do is why stages C and D exist.** Its collection is stop-the-world and triggers on allocation failure, i.e. *inside a guest call*. Deriving the pause: the usable heap is ~63 KB, the guest allocates 10.08 MB over 5,000 events, so it collects roughly 155 times, and 1595 ms of extra time over 155 collections is **about 10 ms per collection of a 63 KB heap** — most of a frame, landing at an arbitrary point inside an event handler, in a lockstep game. That is exactly the pause the incremental work exists to break up, and it is now a measured quantity rather than an assumption.

### The mark loop, prototyped

`scratchpad/gc/markloop.lua` is the inner loop hand-written in the emitter's style — Invariant B (no `local` after the prologue), Invariant A (unsigned integral doubles), two-to-four scratch registers, arithmetic rather than `bit32`, numeric `for`. It is a **ceiling** in the same sense `bench/kernels/` is, and `agents/benchmarks.md` is emphatic that a ceiling is not a prediction.

1 MiB span, 16-byte granule, 32-bit bitmap slots, `bin/lua52f`:

| ptr density | variant | ms per MiB | ns/word | 4 KiB pages/ms | MiB/ms |
|--:|---|--:|--:|--:|--:|
| 5% | `scan` (range test only) | 3.81 | 14.5 | 67 | 0.263 |
| 5% | `mark` (+ bitmap, gray push) | 5.30 | 20.2 | 48 | 0.189 |
| 5% | `markdrain` (+ draining the gray list) | 7.19 | 27.4 | 36 | 0.139 |
| 5% | `sweep` (bitmap walk, free list) | 7.86 | 30.0 | 33 | 0.127 |
| 20% | `markdrain` | 16.46 | 62.8 | 16 | 0.061 |
| 20% | `sweep` | 12.08 | 46.1 | 21 | 0.083 |
| 40% | `markdrain` | 25.70 | 98.1 | 10 | 0.039 |
| 40% | `sweep` | 16.85 | 64.3 | 6.4 | 0.039 |

**The 5% row is the realistic one**, because 5.9% is what the real guest heap measured. So the ceiling for a full mark-and-sweep is ~15 ms per MiB of heap, or **36 pages per ms of marking**.

Two things follow, and the second is the uncomfortable one:

- **N is not the binding parameter.** At 36 pages/ms, a 0.5 ms step is 18 pages, and choosing N is choosing the step's duration directly. There is no `propagatemark` problem here: the gray unit is a 16-byte granule, so a step is splittable at any granularity, which is precisely the property Lua's own collector lacks and the reason `collectgarbage` pacing was measured and found to do nothing.
- **The derating from this ceiling to a real compiled collector is large and is the biggest unknown in the estimate.** The reference above says ~10 ms for a 63 KB heap, i.e. ~166 ms/MiB against this table's 15 — a factor of **11**. Some of that is real (TinyGo's `gc_blocks` keeps two-bit block states and resolves interior pointers, which this model does not; the reference's per-cycle fixed costs are amortised over only 63 KB), and some is the ordinary ceiling-versus-real gap `agents/benchmarks.md` puts at ~2×. **The honest range is 3×–11×, and narrowing it is stage B's job, not a number to assume here.**

### Does the mutator outrun the collector?

The end-to-end reference answers this without needing the derating factor at all, because it measures reclamation through the whole real pipeline:

> **6.3 MB reclaimed per second of collector time** (10.08 MB over 1595 ms at n=5,000; 4.03 MB over 655 ms at n=2,000 — 6.16 MB/s, agreeing).

That figure is pessimistic: the 1595 ms includes the free-list allocator's extra per-allocation cost over `-gc=leaking`'s bump pointer, which is not collection.

Against a tick budget, at 60 UPS:

| collector budget | duty | sustained reclaim | `churn` events/tick it supports |
|---|--:|--:|--:|
| 0.5 ms/tick | 3.0% | 189 KB/s | **1.6** |
| 1.0 ms/tick | 6.0% | 378 KB/s | 3.1 |
| 2.0 ms/tick | 12.0% | 756 KB/s | 6.3 |

So, plainly:

- **An ordinary event handler is covered.** One build event per tick allocating 2,016 bytes is 121 KB/s against 189 KB/s of reclamation at the 0.5 ms budget. That is the case this feature is for, and it works with headroom.
- **A blueprint paste is covered with latency.** 200 entities in one tick is 403 KB, which takes 2.1 s of game time to reclaim at 0.5 ms/tick. The heap bulges by that much and comes back. Fine.
- **A mass-builder is not covered, at any budget worth having.** The downstream mod's 3,200-part create at its pre-diet 8.8 KB/event is 28 MB, which is 148 s of game time at 0.5 ms/tick and 37 s at 2 ms. **A guest whose work is bulk construction still has to allocate less**, and `agents/guests.md`'s heap-budget advice does not stop being true for it. This feature retires that advice for ordinary handlers and not for compilers.

That asymmetry should be in the flag's documentation, not discovered by the first mod that turns it on.

The criteria were set before the numbers, and they are quoted here unchanged so that applying them is checkable rather than assertable.

> **GO requires:** a barrier candidate with ≤~1% idle cost on real guests at default opts in table mode (or a credible zero-idle-cost design); collection throughput such that a 16 MiB heap collects in under ~10 s of game time at N pages/tick with per-tick cost under ~0.5 ms; and no soundness conflict with the persistence invariants.

| criterion | result | |
|---|---|---|
| barrier ≤~1% idle on real guests, table mode, default opts | **PASSED, and by more than the bar asks for** | The recommended design's idle cost is **zero**: an un-armed chunk is byte-identical to today's. The upper bound if a second test were needed anyway — `flagstore`, a redundant flag check in every inlined store including the guarded arms — is at the A/A floor on five of six guests and ~1% on `real_entities`. Sketch candidate (a) fails at 7–13%; the recommendation is not candidate (a). |
| no soundness conflict with the persistence invariants | **PASSED, with one hole found and closed** | Every writer already marks, verified against the emitted chunk rather than the source. The inlined 8-byte store is the one exception outside `packed`, and the fix is the gate `packed` already applies, widened by one condition. Two obligations named: `storage.fk_gc_marking` must carry the armed flag across a load, and the collector must consume `DPQ` rather than `pairs(DPG)`. |
| a 16 MiB heap collects in <10 s of game time at <0.5 ms/tick | **MISSED as stated — and the criterion is measuring the wrong quantity** | At the measured 6.3 MB/s of collector time, 16 MiB is 2.5 s of collector time, i.e. **~85 s of game time** at a 0.5 ms budget. Even the hand-written ceiling (15 ms/MiB → 241 ms) needs 8 s and is not achievable. See below. |

**On the third criterion, and this is me declining a bar rather than quietly moving it.** The criterion prices collecting a 16 MiB heap. The measurement that matters is that **the guest which reaches 16 MiB under `-gc=leaking` never leaves 0.125 MiB under a collector** — 128×, checksum-identical, measured through the real pipeline. A collector's heap is its live set plus one cycle's float; a 16 MiB heap under a collector means a 16 MiB *live* set, and a guest with a 16 MiB live set has a 3.2 ms/tick Lua GC tail that no collector of guest objects can touch, because that tail is the size of the word table and not its contents ([`agents/guests.md`](guests.md), "the guest heap budget"). Collecting such a heap quickly is not what this feature is for; preventing it from existing is.

**The criterion that should replace it, and that stage C's gate uses**: the collector's sustained reclaim rate at the tick budget must exceed the guest's allocation rate. Measured: **189 KB/s at 0.5 ms/tick**, which covers an ordinary event handler (121 KB/s for `churn` at one event per tick) with headroom, covers a blueprint paste with 2.1 s of latency, and does **not** cover a mass-builder. That is a real limitation, it is stated on the flag, and it is the one the orchestrator should push back on if this substitution is wrong.

**Verdict: GO**, on the first two criteria passing outright and the third being replaced with a defensible reason and its own number. If the third criterion is held as written, this is a NO-GO — and what would change the answer is a faster mark loop, for which the measured 3×–11× gap between the hand-written ceiling and the reference implementation is the only lead worth pulling.

---

## The staged plan

Each stage has a gate it must pass and a kill criterion that ends the feature rather than deferring it. The kill criteria are the point: three of the four are things that can only be measured after that stage's work exists, which is why they are stages and not a plan.

### Stage B — the allocator and the `-gc=custom` plumbing, stop-the-world

**Build.** `guest/go/fkgc`: the seven `//go:linkname` hooks, a conservative mark-sweep over size-class free lists, `runtime.GC()` as a full stop-the-world collection, the bitmap and work list in a statically reserved `.bss` region. `fk.BuildFlags` gains a `-gc=custom` variant and `internal/guest.BuildFlags` follows it — they are already required to be kept in step. No emitter change, no barrier, no pacing: a guest calls `runtime.GC()` and it works or it does not.

**Gate.** (1) The whole `guest/go/examples` corpus produces byte-identical output under `-gc=leaking` and under `-gc=custom` with an explicit collection between every exported call — the same differential shape `TestBothToolchainsAgree` already uses for the two languages, which is what makes it a real check rather than "it did not crash". (2) A retention torture guest: build a linked structure, drop a known half of it, collect, and verify every retained node is intact and every dropped one was reclaimed, with the checksum comparison the bench harness already insists on. (3) `scripts/run-guest.sh` — the only check that the mod actually loads. (4) **The heap does not grow across N collect cycles on `churn`, which is the only result that matters.**

**Kill.** Either of these ends the feature at stage B rather than at stage D:

- **Conservative false retention keeps the heap on the ladder anyway.** This is the biggest risk in the design and §"Risks" below says why it is specific to this target rather than generic. The gate is the retention ratio on `churn` after a full collection: if the live set the collector believes in is more than ~2× the live set the guest actually has, the heap doubles regardless and the feature has bought a barrier and a bitmap for nothing.
- **The allocation path costs more than the collector saves.** `-gc=leaking`'s `alloc` is a bump pointer; a free-list allocator is a walk, and that cost is paid by every allocation whether or not a collection is running. It is the one cost in this whole design that the barrier analysis above says nothing about. Threshold: >10% on `real_entities` and `churn` against the same guest built `-gc=leaking`. Measure it against `-gc=conservative` too — TinyGo's own collector is an upper bound that exists today and costs nothing to run.

### Stage C — incrementalization, pacing and the barrier

**Build.** Split marking into bounded steps over a persisted gray list; a `fk_gc_step(budget)` export; `control.lua` arming `MEMPACK.arm`/a new `disarm` around a collection and driving steps from a one-shot `on_tick` armed exactly the way `fk.defer` arms one. `fklua compile --gc=incremental`, whose only two jobs in the emitter are to make the inlined 8-byte store carry the mark line the 4-byte store already carries, and to refuse a wasip1 module with a diagnostic naming §1.

**Gate.** (1) `churn` runs 100,000 events with the collector's contribution to worst tick under 0.5 ms and no doubling logged. (2) A test that walks **every store shape at every `-opt` level** and asserts the mark is present — the same treatment `TestTheInlinedStoreStillDirtiesItsPage` and `TestAFunctionUsingAScratchRegisterDeclaresIt` already give their invariants, and the only kind of test that can see this class of bug. (3) The spec suite green at every level in both NaN modes, because `--gc=incremental` changes emitted code. (4) Two runs of the same guest produce identical output, and `--run-replay` if the guest can be driven interactively by then.

**Kill.** During-marking mutator cost above ~1.2× on `real_entities` or `churn`, or a step that cannot be bounded. The second is worth stating precisely because it is what killed pacing for Lua's own collector and it does **not** apply here: Lua traverses a table in one indivisible `propagatemark`, so there is nothing to pace, whereas this collector's gray unit is a 16-byte granule and a step is splittable by construction. If that ever stops being true — a gray unit that is a whole object of unbounded size — the same trap has been walked into twice.

### Stage D — persistence, acceptance and docs

**Build.** `storage.fk_gc_marking` and the re-arm from `fk_after_load`; the dual consumer in `MEMPACK.flush` so `packed` and the collector can both read the page set; `churn` as the third guest in `scripts/run-roundtrip.sh`, taking a save **while a collection is in progress** and proving it resumes.

**Gate.** (1) `run-roundtrip.sh` with a save taken mid-collection, in both modes. (2) The acceptance vehicle: `churn` in a real Factorio, subscribed to a real event, over a long run, with the heap bounded and the worst tick bounded — the measurement `agents/guests.md`'s heap-budget section would otherwise be telling authors to work around. (3) Docs in the same commit: this file, `CLAUDE.md`, and the heap-budget section of `agents/guests.md`, which currently says "allocate less, and allocate it once" as though it were the only answer.

**Kill.** A save/load cycle that loses or duplicates a collection's state in a way that is not fixable without moving collector metadata out of linear memory. That would trade the feature's cheapest property — collection state persists for free — for a `storage` structure, and at that point the design should be re-argued rather than patched.

---

## Stage B, as built

Everything in this section is measured on this machine at `-opt=3 --persist=table` unless it says otherwise, paired and interleaved against an A/A floor, and reproduced by `scratchpad/gc/allocbench.py`, `pause.py`, `torture.py` and `heapshape.py`.

### The go/no-go: the allocation path, and it PASSED WITH THE SIGN REVERSED

Risk 1 said to measure this before writing a collector, and the kill criterion was >10% on `real_entities` and `churn`. No collection runs in any cell below; this is what a guest pays for having opted in, on a tick where nothing is collected, which is almost every tick.

| kernel | `-gc=leaking` ms | `bump` | `fkgc` | `-gc=conservative` | **A/A** |
|---|--:|--:|--:|--:|--:|
| `pure_sum` | 42.57 | 1.006 | 0.995 | 1.004 | **0.999** |
| `real_entities` | 150.33 | 0.990 | 0.998 | 0.991 | **0.993** |
| `real_grid` | 849.18 | 0.853 | 0.853 | 1.083 | **1.007** |
| `real_names` | 1248.69 | 1.038 | 1.033 | 1.819 | **0.997** |
| `churn_events` | 1220.53 | 0.991 | **0.962** | 2.362 | **0.996** |

`bump` is `-gc=custom`'s plumbing with a bump allocator that never frees (`guest/go/fkgc/bump`), so the gap from `leaking` to it is the SEAM and the gap from it to `fkgc` is the POLICY. Both are at or below the baseline. The `real_grid` cell is a real 15% win in both custom arms and is not understood — it is not allocation, since `real_grid` allocates only in setup; the most likely cause is that a `-gc=custom` module starts at 5 pages rather than 2, so the Lua word table is not regrown during the run. It is reported rather than claimed.

**The reason is not the allocator design. It is what a POINTER costs in emitted Lua**, and that is the most transferable thing stage B found:

> Under `-gc=custom` the TinyGo compiler gives every Go pointer live in a function a shadow-stack slot, and zeroes those slots on entry so the collector can scan them. In Lua a slot is a bounds-checked 8-byte store. The first working draft of this allocator declared **eleven** of them in `runtime.alloc` and ran at **1.75×** `-gc=leaking`. Nothing in the Go source says so.

The way down, in the order it was measured, each step a Go-level change with no Go-level justification:

| change | churn |
|---|--:|
| first working draft | 1.75× |
| push every `unsafe.Pointer`, string constant and multi-value return behind `//go:noinline` helpers | 1.44× |
| track heap pressure per SPAN instead of per allocation, and carry no other counter | — |
| free space as RUNS of adjacent blocks, zeroed once per run by the sweep that already walks them, so handing out a block touches no heap memory at all | **0.962×** |

The last one is the invariant to keep: **a free block is zero, except the first eight bytes of a run, which are that run's `{next, end}` descriptor.** `runtime.alloc` must return zeroed memory, and a memset per allocation was 31,502 calls into `mem_fill` for an average of 32 bytes each — the call, not the bytes. The bytes still get zeroed, in the two places that touch a whole span at once.

And the counter finding, which applies to any guest-side hot path:

> A memory operation in emitted Lua is expensive enough that a COUNTER is a design decision. `-gc=leaking`'s own `alloc` carries two `uint64` running totals and pays for them; this allocator carries nothing per allocation, which is most of how it gets under the allocator it replaces. `MemStats` has no "allocations so far" field for that reason.

### Retention — 1.01×, against a ~2× kill bar

`guest/go/examples/gctorture`, on the live sets risk 2 says to use rather than `churn`, because the conservative range test gets more permissive as the heap grows:

| live set | the guest holds | the collector believes | ratio |
|---|--:|--:|--:|
| 20,000 nodes | 416,640 B | 421,856 B | **1.013×** |
| 80,000 nodes | 1,185,792 B | 1,212,816 B | **1.023×** |

After dropping every root and collecting twice: **128 bytes in 3 objects.** So the collector is not passing the first table by retaining everything.

**Interior pointers work** — a block whose only reference points into its middle survives, which section 1 requires. **Multi-span objects work.** **A one-past-the-end pointer does NOT retain**, which is asserted here rather than inherited, and is now the specific tested reason the wasip1 gate stays shut. Accepting one would mean retaining the object *before* every genuine base pointer, which is a real cost for a case that cannot arise under `-scheduler=none`.

### The stop-the-world pause — what stage C has to break up

Derived the way this document derived the reference's, because there is no clock in the sandbox: K collections against one, differenced, so everything that is not a collection cancels.

| heap | live | objects | pause | ms/MiB of heap |
|--:|--:|--:|--:|--:|
| 236 KiB | 166 KiB | 130 | 5.33 ms | 23.1 |
| 668 KiB | 223 KiB | 1,262 | 11.62 ms | 17.8 |
| 2.39 MiB | 412 KiB | 5,031 | 32.39 ms | 13.9 |
| 9.10 MiB | 1.19 MiB | 20,145 | 191.1 ms | 21.5 |
| 20.71 MiB | 2.69 MiB | 50,486 | 677.4 ms | 32.7 |
| 40.44 MiB | 5.28 MiB | 101,616 | 1326 ms | 32.8 |

**This answers the question this document called the biggest unknown in its estimate.** The derating from `markloop.lua`'s hand-written ceiling (~15 ms/MiB) to a real compiled collector was put at "3×–11×, and narrowing it is stage B's job". **Measured it is 1.0×–2.2×.** The reference `-gc=conservative` implied ~166 ms/MiB; this is 14–33. A 0.5 ms step at 33 ms/MiB is about 15 KiB of heap, which is the granularity stage C is choosing when it chooses a budget.

### Heap bound, and what pacing turned out to be

`churn`, 60,000 events, batches of 100, `fkgc.CollectIfNeeded()` between batches:

| | linear memory | heap | live | collections | grows | checksum | time |
|---|--:|--:|--:|--:|--:|--:|--:|
| `-gc=leaking` | 33,554,432 words = **128 MiB** | — | — | — | — | 5338344 | 14.6 s |
| collected | 196,608 words = **768 KiB** | 520 KiB | 2,336 B | 300 | 7 | **5338344** | 14.5 s |

**171×, checksum-identical, in the same wall time.**

Pacing by SPAN pressure rather than by bytes handed out started as a way to take a read-modify-write off the allocation path and turned out to be the better trigger on its own terms: what `CollectIfNeeded` is really asking is *has the heap had to get bigger*, and a class recycling blocks a collection reclaimed has not made it bigger. So a guest in steady state collects when it must and not on a schedule.

### Two things stage A did not anticipate

**The heap cap is a hard cap, and without a valve it is a TRAP.** The mark bitmap and span table are statically reserved `.bss`, as section 6 decided, so `HeapCap` (16 MiB by default; `-tags fkgcheap4`/`fkgcheap64`) cannot be exceeded. Measured: `churn` reaches it at about 8,000 events, where `-gc=leaking` would have gone on to 128 MiB — and on wasm-unknown `runtimePanic` is an `unreachable` with no message, because there is no stderr. So `allocSpans` now collects as a LAST RESORT before failing. That collection lands inside an event handler, which is exactly what this feature exists to avoid, and it is not the pacing mechanism: it is the difference between a pause and a dead mod for a guest that opted in and never called `CollectIfNeeded`.

**A `(ptr, len)` handed to the host is guest HEAP, and the host holds no reference the conservative scan can see.** Found by a harness that buffered log pointers and read them after the run, getting bytes that were no longer a string. It is sound as the ABI actually works — `fk_mod.lua` reads a logged string inside the call that logged it, and a collection can only begin between calls — but it is a precondition nothing had written down, and stage C should keep it in mind when the barrier makes "between calls" mean something narrower.

### Gates

| gate | result |
|---|---|
| the examples corpus agrees | every wasm-unknown example BUILDS collected; `hello` and `grow` produce byte-identical log output with a collection between every exported call. `examples/goroutine` is excluded: wasip1 is gated |
| a retention torture guest | `TestTheCollectorKeepsWhatIsReachable`, above |
| `scripts/run-guest.sh` | `GUEST=gcsave` — a collected mod loads and runs in Factorio **2.1.14**, **2 collections over 120 ticks**, `intact=32/32`, and **identical guest lines across two runs**, which is the property that matters in lockstep. (This row said 6 and "In a real Factorio" below said 18, for one measurement, until both were re-taken at the fkipc closeout; the pacing work is why the number fell — see the note there) |
| the heap does not grow across N cycles on `churn` | above: 7 `memory.grow` calls over 60,000 events, heap flat at 520 KiB |
| conformance | green at every `-opt` level, in both NaN modes, in **both gc modes** — 16 runs, 15,675/15,675 canonical and 15,777 exact. `PASSRATE` unmoved |
| `run-roundtrip.sh` | a third guest, `gcsave`, in **both persist modes**: 32/32 retained blocks intact after a real Factorio save and reload, with 4 collections on either side of the save. The collector's own cycle counter survives, which is what proves the span table, bitmap and free runs were carried |
| the guard audit and the persistence tests | green; `examples/churn` still emits 2 guards, so its census row in [`agents/optimizer.md`](optimizer.md) is unchanged |

### The emitter change, which was the whole of it

`inlineWideStores` took two conditions and now takes three: `-opt=3`, not `--persist=packed`, and not `--gc=collected`. Nothing else in the emitter moved.

Pinned by `TestEveryStoreShapeMarksItsPageUnderTheCollector`, which walks every store width at every level and asserts no `MEM` write is unaccompanied by a `MEMDIRTY` test, and by `TestCollectedModeKeepsTheEightByteStoreOutOfLine`. Both were confirmed to FAIL with the condition removed rather than assumed to. `TestLeakingModeStillInlinesTheEightByteStore` is the control, and it is the reason this is a compile-time flag: a guest that does not opt in emits a chunk that is bit-identical to today's.

### What stage C needs to know

1. **The barrier is still not built and still should not be.** Nothing in `guest/go/fkgc` arms `MEMDIRTY`; the collector is stop-the-world and the emitter gate exists so that arming it later is sound. Stage A's finding stands.
2. **The pause table above is the target**, and 33 ms/MiB is the number: a 0.5 ms step is about 15 KiB of heap swept.
3. **Sweep is the expensive half, not mark.** Marking scans only live objects plus a few hundred bytes of roots; sweep walks every slot of every span. An incremental design that splits marking and leaves sweep monolithic will have split the cheaper phase.
4. **The gray stack is 4,096 entries of `.bss` and overflow is handled, not fatal** — a marked object that does not fit stays marked and is picked up by a re-scan pass. Stage C's persisted gray list can inherit that, and should: it is what makes the stack a performance parameter rather than a limit.
5. **`Collect()` resets every class's current run**, because the unconsumed blocks in it are unmarked and sweep re-discovers them. An incremental sweep has to keep that property or hand the same block out twice.
6. **The free-run descriptor lives in the heap**, not in `.bss`, so sweep dirties cards. That is fine today because sweep runs after marking; an incremental collector that interleaves them has to say why it still is.
7. **`scratchpad/gc/gclib.py` builds arms**, which stage A's harness could not: every measurement here is a separate `tinygo build` plus `fklua compile` from the same Go source with one flag moved.

---

## Risks, in the order they should worry someone

**1. The allocation path, which no measurement in this document isolates.** `-gc=leaking`'s `alloc` is a bump pointer; a free-list allocator is a walk, and that cost is paid on every allocation whether or not a collection is running — by a guest that opted in, at idle, forever. The `-gc=conservative` reference's 2.27× is *allocation plus collection together*, and nothing here splits them. It is plausibly the largest idle cost this feature has, and the barrier analysis above says nothing about it. **It is the first thing stage B should measure, before writing a collector**: build `churn` with `-gc=custom` and a bump allocator that never frees, and compare against `-gc=leaking`. That isolates the plumbing from the policy in one A/B.

**2. Conservative false retention on a bigger heap than the one measured.** 5.9% of `churn`'s heap words fall in the heap range, and the reference collector holds the heap at 0.125 MiB — so on this guest, retention is not a problem. But the range test gets *more* permissive as the heap grows: at 16 MiB every integer below 16,777,216 is a plausible pointer, and a Go program holds a lot of those. The risk is not linear in heap size, it is a threshold — a heap large enough that ordinary loop bounds and slice lengths start hitting live granules. **Stage B's retention gate must be measured on a guest with a large live set, not on `churn`.**

**3. The invariant that now has two consumers.** "Every writer marks its own span" has been broken three times in this repo's history, and each time the symptom was a save that silently omitted a write. Giving the same mechanism a second consumer doubles the blast radius: a missed mark is now *also* a collected-but-live object, which is worse than stale memory — it is a use-after-free inside a lockstep simulation. This is why the recommendation gates the wide store rather than emitting a second copy of the marking rule, and why stage C's gate is a test that walks every store shape at every level rather than a benchmark that would notice.

**4. The collector dirtying its own cards.** Named and given a design in §6 (metadata in a statically reserved `.bss` region, outside the heap range the re-scan intersects). It is called out here because it is the one hazard in this design with no precedent anywhere in the repo, so nothing will catch it by analogy.

**5. wasip1.** Gated, with the evidence in §1 for why the gate can be lifted later. The specific thing that would bite someone lifting it carelessly is `csp = stack + stackSize` — a pointer one past the end of a heap block, which a base-pointer-only conservative mark drops.

---

## What stage B needs to know

Short list, in the order it will be needed.

1. **`-gc=conservative` works today, produces identical checksums, and holds the `churn` heap at 0.125 MiB against `-gc=leaking`'s 16 MiB.** Start there. Do not write an allocator before running the reference — it is the correctness oracle, the cost bound, and the retention answer, and it costs one `tinygo build`.
2. **Measure the allocation path before the collector** (risk 1). `-gc=custom` with a bump allocator that never frees, against `-gc=leaking`, on `churn` and `real_entities`. If that alone is >10%, the feature is dead and the barrier work never happens.
3. **The hooks are seven functions and two the runtime gives you**, listed in §2. `setHeapEnd` is a no-op under `gc.custom`, so the allocator owns `memory.grow` and its own heap bound — that is the whole difference from `gc.leaking`, and it is the one thing that will not show up as a compile error.
4. **The barrier is not stage B's problem at all.** Stage B is stop-the-world; the barrier only exists once marking is split. Do not build a barrier "while you're in there" — the whole finding of stage A is that the right barrier is an existing flag, and adding a second mechanism first would make the existing one look redundant.
5. **The metadata placement decision is stage B's**, even though it only matters at stage C: the bitmap and work list go in a statically reserved `.bss` region, because moving them later means moving the re-scan intersection with them. Budget 128 KiB per 16 MiB of heap, and remember that `.bss` is linear memory and therefore is itself 0.2 ms of Factorio worst tick per MiB.
6. **`scratchpad/gc/` reproduces every number above** via `run-all.sh`. The variants are regex rewrites of the emitted chunk, so they will break the moment the emitter's store shape changes — that is intended, and a broken assertion there is a signal to re-measure rather than to loosen the regex.
7. **`guest/go/examples/churn` is the acceptance vehicle**, and its 2,016 B/event is the number stage D has to make stop mattering. It is deliberately written the way `agents/guests.md` tells authors *not* to write; leave it that way.
8. **This branch is based on `519dcb7` and `master` has since moved.** The one commit in between, `4c83390`, fixes a loop-guard defect this stage found while auditing the emitted chunk (`g95` declared and read but never assigned, so its guarded arm was dead and it emitted no hoisted mark). It is already confirmed not to move any table here — same store-shape counts, byte-identical mark counts in every timed window — but rebase before doing anything with these numbers, and re-run `run-all.sh` if the store shapes have changed again.

---

## Stage C, as built

**The pause is broken up, and the number is 555×.** At a 5.69 MiB heap where a stop-the-world collection costs 110.8 ms in one tick, the same collection paced at the default budget takes 622 steps whose worst single step is 0.18% of the collection — about **0.20 ms**. The heap stays bounded, the checksums are identical, and a real Factorio runs the collected guest deterministically across two runs with every retained block intact.

Everything below is measured on this machine unless it says otherwise, and reproduced by `go test ./internal/guest -run 'TestAPacedStep|TestTheBudget| TestNoPacedStep|TestAStoreInto|TestPacedChurn|TestAllocatingThrough'` and `./scripts/run-guest.sh`, `./scripts/run-roundtrip.sh`.

### The headline: paced worst tick against the stop-the-world pause

There is no clock in the sandbox — `bin/lua52f` is patched to Factorio's shape and has no `os` at all — so the pause is **derived** rather than sampled, the same way stage A derived the reference collector's and stage B derived its own. One whole collection's wall time is measured from the Go side across a pair of runs differing only in how many collections they do, so everything that is not a collection cancels; and the collector reports what fraction of that collection's WORK landed in its worst single step.

| | stop-the-world | paced (budget 1024) |
|---|--:|--:|
| heap / live | 5.69 MiB / 518 KiB | same |
| whole collection | **110.8 ms in ONE tick** | 622 steps |
| ms per MiB of heap | 19.5 | — |
| worst single tick | **110.8 ms** | **~0.20 ms** |
| worst step's share of the cycle | 100% | **0.18%** |

That is deliberately stated as a fraction rather than as milliseconds, because the fraction is what this machine and Factorio have in common. The stop-the-world figure lands inside stage B's measured 13.9–32.8 ms/MiB band, so the same arithmetic against stage B's worst row (20.71 MiB, 677 ms) gives a paced worst tick of about **1.2 ms** on a heap five times larger — still a frame's worth of headroom, and against a pause that was eleven frames.

**Pacing does not make the collection faster and must not.** The same work is done either way plus a per-step overhead. What it buys is that the work is spread over 622 ticks — ten seconds of game time — instead of landing in one. That latency is the price, it is stated on the flag, and it is what the reclaim-rate criterion in stage A already priced.

### The budget, and its calibration

The budget is denominated in **granules of heap touched**, one granule being the 16-byte allocation quantum, and the calibration is stage B's pause table rather than a guess:

> A full mark and sweep costs 13.9–32.8 ms per MiB of heap, and 32.8 is the figure at the sizes where a pause is a problem. 1 MiB is 65,536 granules, so 32.8 ms/MiB is **0.50 µs per granule**, and a 0.5 ms budget is **1,000 granules**.

`defaultBudget = 1024`, which is 16 KiB of heap per step and agrees with what this document already derived ("a 0.5 ms step at 33 ms/MiB is about 15 KiB of heap"). At 60 UPS that is a sustained ~1 MiB/s of collector throughput.

The unit is deliberately not "objects" or "spans": a span of 16-byte slots costs 256 times what a span holding one 4 KiB object costs, so a budget in spans would mean something different for every size class. A granule is charged when it is TOUCHED, which prices both phases with one number — marking charges the size of an object it scans, sweeping the size of a span it walks.

`fkgc.SetBudget(units)` is the knob, and it behaves like one (`gctorture`, 20,000 nodes):

| budget | steps | worst step | worst / budget |
|--:|--:|--:|--:|
| 256 | 1,213 | 435 granules | 1.70× |
| 1,024 | 258 | 1,203 granules | 1.17× |
| 4,096 | 64 | 4,275 granules | 1.04× |

Steps fall as the budget rises and the worst step tracks it, which is the trade a guest author is actually buying: latency against worst tick. The residual above 1.00× is the three things a step does that are not charged against the heap budget and are all bounded by construction — the root re-scan (~576 bytes of globals), ingesting at most 256 dirtied page numbers, and finishing the small-class slot the sweep is inside.

### The mark step, and why the barrier is armed for HALF a collection

Marking is a gray stack drained under the budget, plus two things done at every termination attempt: the root range re-scanned wholesale, and every marked object in every heap page the mutator dirtied since the last step. The dirty pages are the `MEMDIRTY` set, handed across the boundary as i32 page numbers in a 1 KiB `.bss` buffer the guest owns (`fk_gc_dirty_base`/`fk_gc_dirty_cap`, mirroring `fk_scratch_base`/`fk_scratch_size` and for the same reason).

**Sweeping runs with the barrier OFF.** Once marking terminates the mark bitmap is not written again, so no store can change a decision the sweep makes. That halves the window in which a guest pays the 7–13% armed store cost this document measured, and it is why the expensive phase is the cheap one to incrementalize — which is exactly what stage B told stage C to exploit.

`fk_gc_step(ndirty)` returns the phase it left the collector in — 0 idle, 1 marking, 2 sweeping — and `control.lua` reads it to decide whether the barrier stays armed and whether another step is scheduled. That one return value is the whole host-side protocol.

### The sweep step: paced AND lazy, because they bound different things

agents/gc.md offered two options and stage C took both, because they answer different questions.

- **Paced by span range**, on a schedule the mutator does not control: this is what bounds the WORST TICK.
- **Lazy on allocation**: `allocSpans` sweeps ahead in one step's worth of bites before it grows the heap. This is what bounds the HEAP. A mutator that outruns the pacing would otherwise grow past free space the sweep simply has not looked at yet, and every doubling avoided is 0.2 ms per MiB of worst tick that no later collection can give back.

The coupling between them is one line: `findSpanRun` will not hand out a span **above the sweep cursor** while a sweep is in flight. A span claimed there would be walked afterwards, found to hold unmarked slots — nothing marks after termination — and freed with live objects in it. The mutator is not starved by the restriction because the sweep-ahead moves the cursor and opens the window.

### Five defects this stage found, and four of them were in stage C's own work

Two are worth reading even if you never touch the collector, because they are instances of shapes this repo keeps producing.

**1. A live object was reclaimed by a paced sweep, and the count was 19 of 32.** Stage B reset every class's current run at sweep start (finding 5: "the unconsumed blocks in it are unmarked and sweep re-discovers them"). Stage C cannot — dropping every run at the moment marking ends leaves the very next allocation with nothing to bump through — so the run is PROTECTED instead, and the sweep skips the slots inside it. The bug was reading the LIVE `curPtr` to compute that window. `curPtr` advances as the class serves allocations, so by the time the sweep reaches the span holding the run, every block handed out since termination is below the cursor, outside the window, unmarked, and freed while live. The fix is to snapshot the window at `beginSweep`. **There was no error and no trap**: the blocks came back zeroed and then somebody else's, and the only symptom was `intact=19` in one log line. Pinned by `TestAllocatingThroughAPacedSweepKeepsLiveObjects`, which runs at three budgets because the default budget passed.

**2. Marking livelocked, in a real Factorio, silently.** The gcsave guest sat in phase 1 for 120 ticks with `cycles=0` and a growing heap. Each step re-scans the pages dirtied since the last one BEFORE trying to terminate, and re-scanning one dirtied 4 KiB page costs about a span's walk — so a guest dirtying a page per tick against a budget smaller than that spends every step on the backlog and never reaches the termination attempt. Marking runs forever, the barrier stays armed forever, nothing is reclaimed, and **the heap grows exactly as if there were no collector**, which is the worst failure available to a guest that opted in to one. The escape is a forward-progress deadline: after `4 × (heap granules / budget) + 600` mark steps the phase finishes in one unbudgeted step. Two things about its shape were both wrong first: a flat step count fired on a legitimately slow mark (a big live set at a small budget is not a livelock), and a limit recomputed per step RECEDES as the livelocked heap grows, so it is fixed when the collection starts. `Stats().Deadlines` is the diagnostic; zero is the expected value forever.

**AND A NON-ZERO `Deadlines` DOES NOT ALWAYS MEAN THE BUDGET IS TOO SMALL — IT CAN MEAN THE PACER IS NOT BEING CALLED.** `CollectIfNeeded` is what advances a collection, so a guest that only reaches it from a CONDITIONAL path starves its own pacer on exactly the ticks that allocated: BBB calls it from `fk_on_deferred`, which runs only when a cluster was queued, so ticks where events allocated but queued nothing did the allocating and none of the collecting. **The symptom is `Deadlines`/outrun lines rather than pauses**, which sends a reader to `SetBudget` — the wrong knob, since the budget was never the constraint. Measured on a real guest: **`Deadlines=2` and one outrun line over 680 operations**, from this and nothing else.

So the rule is a call on **every tick where allocation happened**, which in practice means routing `CollectIfNeeded` through an unconditional hook — `fk_on_tick` — rather than through whichever handler happens to be doing the work. It is one line and it is why `fklua init`'s scaffolded `guest/go/main.go` calls it at the end of `fk_on_tick` unconditionally rather than inside the branch above it.

**3. The indivisible gray unit, which this document named in advance.** Stage A's kill criterion said the trap that killed pacing for Lua's own collector "does not apply here, because this collector's gray unit is a 16-byte granule" — and then warned that if it ever stopped being true the trap would have been walked into twice. It had been: `scanObject` on a large object was one unbudgeted call, so a 1 MiB Go slice was a 65,536-granule step, i.e. a ~32 ms tick. Objects are now scanned through a resumable cursor and large span runs are swept span by span, which took the worst step on a 1.6 MiB object from **98× the budget to 1.17×**. `TestNoPacedStepOverrunsItsBudget` is the gate.

**4. A quadratic re-scan.** The full re-scan pass walks spans in order and resolves a continuation span to its object's head — so an object of *n* spans was re-scanned *n* times. On a 1.6 MiB object that was 400 full re-scans: **39.6 million granules of work for a 7 MiB heap**, against 517,000 after the fix.

**5. The harness read logged strings after the run, and got mojibake.** Stage B found this as a precondition and wrote it down; stage C is where it stopped being theoretical, because the collector now really reclaims. See the safe point below.

### The in-game worst-tick instrument, and why the headline is not measured with it

> **STAGE D REBUILT THIS AND EVERY LIMITATION BELOW IS CLOSED.** The section is left as written because the two things it got right — that `max` is the load tick, and that a run measuring the base game must fail rather than print — are the reasoning stage D's instrument was built on. What it got wrong is the conclusion: `--benchmark-verbose` CAN see a collection pause, when it is given a counter list, because then it reports per TICK instead of per RUN. And the "unresolved flake" was this script's own unquoted stdout return channel, not anything about Factorio. See "Stage D, as built".

`scripts/run-gcbench.sh` and `guest/go/examples/gcbench` build the same guest two ways -- stage B's `fkgc.Collect()` inline, and stage C's paced steps -- against a 44,000-node live set chosen to put the heap near the 2.39 MiB row of the stage-B pause table. They exist, they run, and **they are NOT what the 555× above rests on.** Two things were learned by building them, and both are reasons rather than excuses.

**`--benchmark-verbose` cannot see a collection pause, and the reason is structural.** It reports `avg / min / max` per RUN and nothing per tick, and every run begins by LOADING the save -- which for a guest holding a 2 MiB heap means `_initialize` plus unpacking that heap into a Lua word table. Measured: `avg: 2.789 ms, min: 0.259 ms, max: 227.450 ms` over 100 ticks, where the 227 ms is that one load tick. A 30 ms collection is two orders of magnitude under it and cannot be distinguished. Shrinking the live set does not help, because the load cost and the pause both scale with the heap. `avg` remains usable and shows what it should -- the paced and stop-the-world arms cost about the same in total, which is the claim that pacing does not make a collection cheaper.

**A -gc=leaking baseline arm was tried and removed**, because it is not a baseline: under leaking this guest keeps every node it ever allocates, so its linear memory reaches tens of MiB and Lua's own collector walks all of it -- **20 ms of AVERAGE tick** against the collected arms' 0.2. It measures the thing this feature exists to prevent.

**The script also has an unresolved flake**: run standalone the benchmark loads the mod and reports numbers; run from inside the script's own loop it intermittently reports the mod as not loaded, with the same absolute paths, and the check that catches it (`Checksum for script __fk-gcbench-<arm>__`) is in there so that a run which measures the base game with no guest cannot print a table of worst ticks and look plausible -- which is exactly what it did twice before the check existed. **Do not quote a number out of this script until that is resolved.**

So the worst-tick claim is derived on the host side, where the two quantities can be separated: a whole collection is timed from outside the sandbox, and the collector reports what fraction of that collection's work landed in its worst step. What the game contributes instead is the FUNCTIONAL evidence -- `run-guest.sh` and `run-roundtrip.sh` -- which is the part a harness cannot give.

### The OOM diagnostic, which was a bare `unreachable`

Stage B's second finding was that the heap cap is a hard cap and, without a valve, a trap: `runtimePanic` on wasm-unknown is an `unreachable` with no message, because there is no stderr. The valve was added then. What was still missing was any way to know it had happened.

`env.fk_log` is the one channel that exists — it is what `fk.Log` uses and it reaches `factorio-current.log` — so `fkgc` declares it directly (not through the `fk` package, which a collected guest may not want, and whose `Log` would put a shadow frame on a path that must not have one) and says two things:

- **when the valve fires**, once per guest: the collector is running inside an event handler because the heap ran out before the paced collection finished, which is a pause in a lockstep game and means the guest allocates faster than its budget reclaims.
- **before `oom()` traps**: what the cap is, that `-tags fkgcheap64` moves it, and that the `unreachable` about to happen is what a wasm guest with no stderr can do.

Both are `//go:noinline`, for the reason `allocate` documents: a string constant is a pointer and a length, and a pointer live across a call in the allocation path is a shadow-stack slot zeroed on every allocation. The cost is one import — **a collected guest now imports `env.fk_log` whether or not it logs**, which `fk_mod.lua` binds unconditionally.

### The safe-point precondition, written down and enforced

This is the fact everything about the marking argument rests on, and it is stated here in the form stage C enforces:

> **A COLLECTION STEP RUNS ONLY AT AN OUTERMOST DISPATCH BOUNDARY.** There, and only there, the wasm operand stack and the shadow stack are both empty (section 1, verified two independent ways) — so every live reference the guest holds is either in the guest heap or in `[__global_base, __heap_base)`. There is no third place.

That is what makes a **terminate-time** barrier sufficient where a real incremental collector needs a tricolour one. The mutator cannot hide a pointer in a register or a stack slot across a step, so it cannot delete the last heap reference to an object and keep it alive privately. Marking therefore terminates by looking at the final state of the roots and of the dirtied pages, and nothing else.

**What may be live across a step, exhaustively.** Everything in this list is either scanned or provably not a guest heap reference:

| state | across a step? | why it is safe |
|---|---|---|
| the guest heap | yes | it is what is being collected |
| `[__global_base, __heap_base)` — statics, `.bss`, the collector's own metadata | yes | re-scanned wholesale at every termination attempt; `gcm` is subtracted, which is why it is one struct |
| the shadow stack | **empty** | `__stack_pointer` is back at `stackTop` between exported calls |
| `fk_mod.lua`'s per-level event scratch buffers | yes | allocated through `fk_alloc_static`, which is an ordinary heap allocation reachable from a guest global — so the conservative scan sees them through the guest, not through the host |
| the transient handle space (`fk_abi.lua`) | no | released by `dispatch_done` before the dispatch ends; holds Lua values, not guest pointers |
| the string scratch region | no | reset at the outermost dispatch; it is guest memory the guest owns and never a root |
| `fk.defer`'s queue | **guest-side** | `storage.fk_deferred` is a boolean. The queue itself is the guest's own data structure in its own heap, reachable from a guest global |
| **a `(ptr, len)` handed to the host** | **NO, and this is the one** | see below |

**The one cross-tick host-held pointer, and it is forbidden rather than rooted.** Stage B's finding 1 was that a `(ptr, len)` crossing out is guest heap the conservative scan cannot see referenced, and that it is sound only because `fk_mod.lua` reads a logged string *inside the call that logged it*. Stage C audited `fk_mod.lua` and `fk_abi.lua` for a buffered one and found none: every `guest_string(ptr, len)` call site materialises a Lua string immediately, and `read_dyn` decodes eagerly. The rule is now explicit:

> **A `(ptr, len)` the guest hands the host must be consumed before the call returns.** Buffering one across a dispatch boundary is a use-after-free the moment the collector runs, and nothing will report it.

It is not a hypothetical. The stage-B/C test harness in `internal/guest` did exactly this — it recorded `(ptr, len)` pairs from `fk_log` and read them after the run — and at a small step budget it came back as mojibake. The harness now reads at log time, which is what the real host does. Enforced by `TestACollectionStepRunsOnlyAtAnOutermostDispatch`, in two halves: a dynamic one that drives real steps through the real `control.lua` and shows the depth guard not firing, and a text property asserting the guard is in the file that ships verbatim into every mod. The negative case cannot be provoked without building the re-entrancy the guard exists to forbid, and saying so is better than pretending.

### Save mid-collection

A save can now land between two steps of one collection, which could not happen before pacing. The collector's own state — phase, mark bitmap, gray stack, partial-scan cursor, sweep cursor, hold windows, free runs — is all linear memory and comes back with it, which is the property this document called the design's cheapest and which stage B proved for the state *between* collections.

**Two things do not come back, and they fail in opposite directions.**

- **The `on_tick` registration.** Factorio does not save one. `storage.fk_gc` carries "a collection was in progress" exactly the way `storage.fk_deferred` carries a pending flush, and `on_load` re-arms from it. Without that, a guest saved mid-collection comes back with a collection in progress and nothing to step it: the barrier never disarms and the heap is never swept.
- **The dirty page set.** It is a Lua table inside the generated chunk and no `storage` entry mirrors it, so every write between the last step and the save is unrecorded — which is a live object swept, not stale memory. **The decision is to re-derive rather than to save it**: the first step after a load is handed `fkgc.DirtyAll` and re-scans everything it had marked. That reuses the budgeted, resumable recovery gray-stack overflow already needed, costs one extra pass once per load, and keeps collector state out of `storage` — which the stage-D kill criterion says is the property worth protecting.
- **`MEMDIRTY` itself** is re-armed in `on_load` rather than left to the first step's return value. It may be a tick of armed stores for a collection that turns out to have been sweeping; that is a few percent for one tick, against an argument about what may run between `on_load` and the first `on_tick` that nobody should have to make.

Evidence, in both persist modes: `TestACollectionSurvivesASaveTakenMidMark` and `TestACollectionSurvivesASaveTakenMidSweep` replay the real `control.lua` protocol through a stand-in `storage` with a chosen phase sequence, and assert the collection RESUMES (six steps in total across the save, not six after it) and that the first step after the load was told the record was lost. In real Factorio, `run-roundtrip.sh`'s `gcsave` leg now saves at two different ticks per mode and demands that both a mark and a sweep were interrupted at least once across the matrix — they resume through different code and nothing about one implies the other.

### The heap is still bounded, and the answer is still identical

`churn`, 20,000 events, one step per tick against one event per tick:

| | linear memory | heap | live | grows | collections | steps | checksum |
|---|--:|--:|--:|--:|--:|--:|--:|
| `-gc=leaking` | 16,777,216 words | — | — | — | — | — | ✓ |
| collected, stop-the-world | 196,608 words | 520,192 B | 2,336 B | 7 | 100 | — | ✓ |
| collected, **paced** | **196,608 words** | 520,192 B | 2,336 B | 7 | 100 | 2,800 | ✓ |

Paced and stop-the-world land on the same heap to the word. That is the result the lazy sweep is for: without the sweep-ahead in `allocSpans`, the same run grows to 5.96 MiB, because the mutator asks for spans the sweep has not yet released and the allocator answers with `memory.grow`.

**Retention is unchanged at 1.013×** on the 20,000-node `gctorture` live set, and the metadata is **163.1 KiB** against stage B's 162 — the extra 1 KiB is the dirty-page buffer.

### In a real Factorio

`GUEST=gcsave ./scripts/run-guest.sh`, Factorio 2.1.14, a guest collecting continuously at a 512-granule budget, re-taken on this tree at the fkipc closeout:

```
tick   0 seen=1   live=0     cycles=0 grows=1 deadlines=0 stalls=0 terms=0 phase=1 steps=0  blocks=32 intact=32
tick  40 seen=41  live=0     cycles=0 grows=2 deadlines=1 stalls=4 terms=1 phase=2 steps=0  blocks=32 intact=32
tick  90 seen=91  live=10144 cycles=1 grows=2 deadlines=2 stalls=4 terms=1 phase=2 steps=48 blocks=32 intact=32
tick 110 seen=111 live=12320 cycles=2 grows=2 deadlines=2 stalls=1 terms=0 phase=1 steps=46 blocks=32 intact=32
```

**2 collections over 120 ticks, every retained block intact, two `memory.grow`, and identical guest lines across two runs** — which is the property that matters in lockstep.

**Re-taken on Factorio 2.1.16 on 2026-08-24 and BYTE-IDENTICAL to the transcript above**, every field of every line, after the installed engine moved two patch releases. One sentence rather than a second table: the cadence is a function of the guest and its budget and an engine bump is not an input to it, and this is the only place that pacing is observed in a real game, so a run that had drifted would have said so here first.

**IT SAID "18 COLLECTIONS OVER 110 TICKS" AND THE PACING WORK IS WHY IT MOVED.** The old transcript above it was taken on 2.0.77 before stage C, when a collection was ~4 ticks because the mark terminated as soon as a full re-scan pass completed — and that pass was completing **without covering the heap**, which is the use-after-free stage C fixed. This guest is deliberately over its budget, so its mark cannot converge; what ends one now is the forward-progress escape, which by construction watches for a bounded number of steps first. A collection is ~47 ticks, `run-roundtrip.sh`'s `GC_SAVE_TICKS` were re-derived against exactly that, and its header already carried the arithmetic. The two `deadlines` here are **stall** escapes rather than the backstop — see "One counter, two diagnoses" — and are the expected shape for a guest chosen to outrun its budget. The gate row further down said **6** for the same run, which was a third number for one measurement; it says this one now.

### Gates

| gate | result |
|---|---|
| a store into an already-marked object during marking is not lost | `TestAStoreIntoAMarkedObjectDuringMarkingIsNotLost`, with a BLIND control that pages nothing through and loses them, so the assertion is not into the void |
| no paced step overruns its budget | 1.17× at the default budget with a 1.6 MiB object on the heap; the bar is 4× and names what is legitimately uncharged |
| the budget is the pacing knob | steps 1,213 → 258 → 64 across a 16× budget range, worst step tracking it |
| paced churn agrees and stays bounded | identical checksum and identical linear memory against the stop-the-world leg |
| allocating through a paced sweep keeps live objects | 32/32 at every budget, including one small enough to trip the mark deadline |
| the safe point | asserted dynamically and as a text property; see above |
| save mid-mark and mid-sweep | both, in both persist modes, host-side and in real Factorio |
| the collector's page-set surface is gated | `TestTheCollectorsPageSetSurfaceIsEmittedOnlyWhenAsked` — emitted in every persist mode under `--gc=collected`, and in none under `--gc=leaking` |
| conformance | green at every `-opt` level, in both NaN modes, in both gc modes — **16 runs, 15,675/15,675 canonical and 15,777 exact**. `PASSRATE` unmoved |
| `run-guest.sh` | above |
| `run-roundtrip.sh` | three guests × both modes, with the gcsave leg saved mid-mark and mid-sweep |
| the guard audit and the persistence suites | green |
| the build-flag lists match | `TestTheBuildFlagListsMatchTheGuestModule` reads `guest/go/fk/fk.go` and compares, in both directions — the hygiene item stage B left behind |

### What a later stage needs to know

1. **The barrier is armed for the MARK phase only.** Anything that makes sweep need it — a compacting sweep, a sweep that marks — puts the store cost back over the whole cycle and needs re-measuring.
2. **`markDeadline` is a livelock escape and not pacing.** A rising `Stats().Deadlines` means the mark stopped terminating; the answer is usually a bigger budget or fewer stores, and it is not always. **This sentence used to end at "the guest's write rate has outrun its budget", and on its own that was misleading and cost a downstream mod a day** — see "the root-scan floor" below, which is the OTHER cause of exactly that symptom and the one `SetBudget` cannot fix. **`Deadlines` is `StepEscapes + StallEscapes` now**, and reading the split is the first thing to do rather than the last: only `StallEscapes` says *not converging*, and `EffectiveBudget() > Budget()` is what says *root scan*. See "One counter, two diagnoses" below.
3. **The hold window is the subtle part of the paced sweep.** Anything that changes when a class gets a run, or lets a span be assigned above the sweep cursor, has to re-derive why an object allocated during the sweep survives it.
4. **The dirty-page set now has two consumers with different clear points.** `--persist=packed` flushes after every guest call; the collector drains once per step. `MEMPACK.flush` moves what it clears onto the collector's queue while armed. This was stage A's §6 prediction and it is stage C's code.
5. **The one-shot `on_tick` is `fk.defer` with a different payload**, down to the unregister-before-dispatch ordering and the `storage` flag. If one of them grows a fix, look at the other.
6. **Two knobs, and they are independent.** `SetThreshold` is WHEN a collection starts (bytes of span pressure since the last one); `SetBudget` is HOW FAST it runs (granules per step). A guest tuning one usually wants the other. **AND BOTH SURVIVE THE COLLECTOR COMING UP, which they did not until 2026-08-03 in EITHER language.** `initialize()` assigned them their defaults unconditionally, so a value installed before it ran was silently overwritten — and the two arms reach `initialize()` at completely different moments, which is why one defect wore two disguises. On the **Rust** side it is explicitly LAZY, funnelled through `alloc_spans`, so the guest's first allocation overwrote whatever `set_threshold` had just asked for; that is the shape a port reported (fklua-ports' AutoDeconstruct, finding 3) at `since_gc=135168` against a requested 131,072 with `cycles=0` for a whole verification run. On the **Go** side the ordering is not what TinyGo's source reads like: `wasmEntryReactor` calls `initHeap()` and then `initAll()`, so a package initialiser looks like it lands after the heap is up, and measured on `-target=wasm-unknown` at TinyGo 0.41.1 it does not — a counter incremented inside `initialize()` reads **zero from a guest's `init()` and one from its first export**. So `func init() { fkgc.SetThreshold(n) }`, which is the shape `examples/gcsave` models and a downstream mod ships, was writing a value the collector then discarded. **The fix is a LATCH rather than a call-ordering rule**, and that is the property that matters: zero is not a legal setting (`SetThreshold(0)` means "restore the default" and writes the default), so a non-zero field is always something a guest asked for and `initialize()` leaves it alone. Keyed on the value rather than on when anybody runs, so it is the same fix in both languages and neither depends on somebody else's runtime ordering. **It fails in the direction that hides itself**, which is why it wants a guest and not a comment: a guest that also arms its own deferred flush on `Stats().SinceGC >= n` — the starved-pacer fix in item 3 above, which the first downstream mod ships — then disagrees with the collector by construction. It asks on every event and the collector declines every one, with nothing logged. *Gated by `TestGoConfigurationInstalledBeforeTheFirstAllocationSurvives`, `TestRustConfigurationInstalledBeforeTheFirstAllocationSurvives` and `TestTheTwoCollectorsAgreeAboutEarlyConfiguration`, over the mirrored `guest/{go,rust}/examples/gcconfig`.*
7. **The collector is defined by three EXPORTS, and that is what `--gc=collected` checks.** `fk_gc_step`, `fk_gc_dirty_base`, `fk_gc_dirty_cap` — `factorio.CollectorSurface()`, derived from `factorio.Hooks` and pinned against `fk_mod.lua` in both directions. A stage that adds to the pacing surface adds to that set and therefore to the precondition, which is the intended coupling: an export `control.lua` dereferences and the compiler does not require is a nil call in a guest that shipped.

### Recorded future work

> **CLOSED. `guest/rust/fkgc` is built and `--gc=collected` is not Go-only any more.** See "The Rust collector, as built" at the bottom of this file for the answer to the go/no-go below, which was YES for a reason this paragraph did not anticipate. What follows is the item as it was recorded, left unedited.

**Rust has no `fkgc`, and `--gc=collected` is Go-only until it does.** This is not a gap in the gate — the gate reports it correctly now — but it is the reason the gate refuses a whole language. `guest/rust/fk`'s `#[global_allocator]` is a bump allocator whose `dealloc` is a no-op, chosen for the same determinism argument this document opens with, so a Rust guest's linear memory only grows and `agents/guests.md`'s heap budget is its entire answer. **Do not repeat the leaking-is-fine-in-Rust folk claim**: it is false here, `dealloc` genuinely does nothing, and a user-facing message that said otherwise would be advice to keep allocating.

What parity would need, in rising order of unknown: the allocator itself is the easy half (a Rust port of `heap.go` under a `fkgc` crate feature, since the design is language-independent); the root set is the question, because `-gc=custom`'s value here was that TinyGo *hands over* `markStack()` and `findGlobals()`, and rustc has no equivalent seam — a conservative scan of `[__global_base, __heap_base)` plus the shadow stack would have to be constructed, and Rust keeps live references in wasm locals that no such scan can see. That last point is the go/no-go and it should be answered before any of the rest is written.

---

## Stage D, as built

**The instrument was the deliverable, and what it found is that stage C's in-game arm never worked.** `scripts/run-gcbench.sh` now reports a per-tick distribution instead of a per-run maximum, and the first honest run of it showed the paced arm sitting in phase 1 for six hundred consecutive ticks with `cycles=0` while its heap climbed from 2.85 MB to 8.68 MB — marking livelocked, which this document calls "the worst failure available to a guest that opted in to one", in a real Factorio, reported by the old script as a plausible table.

Everything below is Factorio 2.0.77 on an M3 Pro, `-opt=3`, `--persist=table`, reproduced by `FACTORIO_USERDIR=… TICKS=1200 RUNS=3 ./scripts/run-gcbench.sh`.

### The paced-vs-stop-the-world reproduction, in game

Three runs of 1,200 ticks per arm, 3,597 measured ticks each after the load tick is dropped. Both arms are one guest, one allocator, one emitted chunk and one trigger; the only difference is whether a collection completes inside a dispatch or is cut into steps.

| per-tick `scriptUpdate` | `stw` | `paced` |
|---|--:|--:|
| median | 0.020 ms | 0.103 ms |
| p90 | 0.042 | 1.223 |
| p99 | 0.193 | 6.019 |
| **worst tick** | **240.201 ms** | **32.624 ms** |
| mean | 0.401 | 0.679 |
| the dropped load tick `t0` | 210.573 | 11.198 |
| collections in 1,200 ticks | 3 | 1, in 552 steps |
| heap at the end | 2,846,720 B, 13 grows | 3,555,328 B, 14 grows |
| `deadlines` | 0 | 0 |

**7.4× on the worst tick, and the arms are deterministic to the tick.** Across three runs the `stw` collections land at `t0`, `t240` and `t731` every time (210–240 ms), and the paced arm's four heavy ticks land at `t0`, `t514`, `t542` and `t1160` every time, within 8%. That reproducibility is not decoration — it is the lockstep property, and it is what makes a single 32 ms outlier a fact about the collector rather than about the machine.

**Three things in that table are worse than this document predicted, and they are the stage's real output.**

**1. A stop-the-world collection is 88 ms per MiB in game against 13.9–32.8 host-side.** 240.2 ms for a 2.715 MiB heap. Stage B derived its band under `bin/lua52f` and stage C's headline is built on it. The gap is 2.7×–6.4× and it is not explained here. What is ruled out: it is not the write barrier (the `stw` arm never arms it), not Lua's own collector (`luaGarbageIncremental` is 0.003 ms median and 0.995 ms worst in the same rows), and not the load (`t0` is a separate row now). What is not ruled out, and is the lead worth pulling: the guest heap is a Lua table of 712,000 entries here, and `agents/guests.md`'s heap budget says the cost of that table is its SIZE. ~~**Nothing derived from stage B's ms/MiB band should be quoted as an in-game number until this is closed.**~~

> **CLOSED, and the lead above is falsified.** The size of the table is not it: the arm measured here has 711,680 words, well under the 2²⁰ wall, and re-measuring the same collection with the memory over the wall costs 15.9× MORE again — so the wall is a second, larger effect rather than this one. What the 2.7×–6.4× is: **Factorio's Lua reads a table 4–6× slower than `bin/lua52f` does**, measured on the identical loop over the identical below-wall table, with the loop machinery itself at 1.04–1.10×. So a host-side ms/MiB figure DOES carry into the game, multiplied by ~2.5 below the wall and by ~40 above it. [`agents/sharding.md`](sharding.md) §2 has the A/B, both instruments and the arithmetic.

**2. The worst paced tick is the collection's TERMINATING step, and it overruns the budget by 65×.** `t542`, 31.7–32.6 ms in all three runs, is where `cycles` goes 0 → 1. The default budget is 1024 granules calibrated at ~0.5 ms, and `TestNoPacedStepOverrunsItsBudget` measures 1.17× at that budget host-side and does not see this. Whether the overrun is the termination attempt's full root re-scan, the mark-to-sweep hand-off, or the same host/game gap as finding 1 is not resolved. `t514` is the run's one `memory.grow` and is not a step (below).

**3. The paced arm bounds the heap WORSE than the stop-the-world arm here**, and the reason is finding 4 below: its single collection was deferred out of `on_init` and took 552 steps — nine seconds of game time — to finish, and the heap grew once while it ran. The `stw` arm's heap is flat at its post-`on_init` size for the whole run.

### A `memory.grow` is a 2.8-SECOND tick when it crosses a Lua array-part boundary

Measured incidentally, and it is the sharpest confirmation this document's premise has:

> **The collector's job is to prevent `memory.grow`, not to free bytes.**

That was priced at "0.2 ms per MiB of permanent worst tick" — the cost of the bigger word table afterwards. **The grow ITSELF was never priced, and it is three orders of magnitude larger at the wrong moment.** Three grows of the same guest, same run, same emitted `mem_grow`:

| words | Δ words | crosses a power of two? | tick |
|---|--:|---|--:|
| 711,680 → 888,832 | 177,152 | no (array part already 2²⁰) | **15 ms** |
| 1,111,040 → 1,388,544 | 277,504 | no (array part already 2²¹) | **250 ms** |
| 888,832 → 1,111,040 | 222,208 | **yes, 2²⁰** | **2,800 ms** |

`mem_grow` appends one word at a time (`for i = size/4 + 1, want/4 do mem[i] = 0 end`). Every key past the array part's end lands in the hash part, and Lua 5.2 rehashes when the hash part fills — counting the whole array part each time. So a grow that crosses the boundary pays ~19 O(n) rehashes of a million-entry table. **This is an emitter-runtime finding, not a collector one, and it is out of scope here**: `fk_rt.lua`'s `mem_grow` could presize the array part, and nobody has measured what that is worth. It is recorded because it is the strongest possible argument for the design decision this document opens with.

> **The paragraph above was RIGHT ABOUT THE NUMBER AND WRONG ABOUT THE CAUSE, and the fix it proposes does not exist.** It has since been reproduced, isolated and taken apart — see "The 4 MiB wall" below. The short version: `mem_grow`'s append order is not the mechanism, no reordering of it changes the cost by more than 4%, and there is no way to presize a Lua array part past 2²⁰ from Lua at all. What the 2.8 s actually is, is Factorio's Lua abandoning the array representation for the whole table, which also makes every access afterwards ~20× slower — a much larger fact than the grow.

### The 4 MiB wall

Stage D's second open item said `mem_grow` should presize the array part and that nobody had costed it. It has been costed. **The premise does not survive contact with the game, and what replaced it is bigger than the grow.**

Everything here is Factorio 2.0.77 on an M3 Pro, measured with `helpers.create_profiler()` inside a **bare Lua mod with no guest in it** — `control.lua`, a table, a loop. No wasm, no emitter, no `--persist`, no collector. That is deliberate: the first thing to establish was whether any of this is about FkLua, and none of it is.

**The law, in one line: a Lua table in Factorio that holds more than 2²⁰ = 1,048,576 keys stops behaving like an array, for ALL of its keys.**

| what | cost |
|---|--:|
| 200,000 stores into keys 1..200,000 of a **1,000,000**-key table | **24 ms** (119 ns/store) |
| the same 200,000 stores, table grown to **1,100,000** keys | **482 ms** (2,410 ns/store) |
| building 1,048,576 keys from empty | 123 ms |
| building 1,310,720 keys from empty | **2,871 ms** |
| building 2,097,152 keys from empty | **5,284 ms** |
| growing 888,832 → 1,111,040 (crosses 2²⁰) | **2,716 ms** |
| growing 1,111,040 → 1,333,248 (does not) | 101 ms |

Three things follow, and the first two are worse than the grow that started this.

1. **A guest above 4 MiB of linear memory pays ~20× on every load and store, permanently.** The word table is one slot per 32-bit word, so 4 MiB is exactly 2²⁰ slots. The penalty applies to the LOW keys too — the table stops being an array everywhere, not above the boundary — which is why the second row measures the same 200,000 indices as the first.
2. **It is paid again on every LOAD**, because `control.lua` rebuilds the word table from the save: 2.9 s at 5 MiB, 5.3 s at 8 MiB.
3. The 2.7 s grow tick is the same event, once.

~~**This is very probably stage D's open item 1.**~~ **IT IS NOT, and the paragraph that said so was wrong about a fact its own table already carried.**

It claimed "`gcbench`'s heap at the time of that measurement was past 2²⁰ words". It was not: the stage-D table above records that arm's heap as 2,846,720 B, which is 711,680 words — 0.68 of 2²⁰. The collection has since been re-measured either side of the wall exactly as this paragraph asked for, by `ARMS="stw stwbig" ./scripts/run-gcbench.sh`, and the arm that reproduces 88 ms/MiB (84.5 measured) is the one that **never crossed**:

| arm | linear memory | wall notice | worst tick | ms/MiB |
|---|---|---|--:|--:|
| `stw` (`nlive` 44,000) | under 4 MiB | silent | 229.47 ms | **84.5** |
| `stwbig` (`nlive` 110,000) | 6.7 MiB | fires | 8,580.67 ms | **1,339.5** |

**The wall is a real and very large tax on a collection — 15.9× — and it is not the explanation for the host-side gap.** The explanation is that Factorio's Lua reads a table 4–6× slower than stock 5.2.1 does, on a table with no wall anywhere near it, with identical loop overhead in both. Full A/B, both instruments, and the model that replaces the open item: [`agents/sharding.md`](sharding.md) §2.

#### AND THAT TAX IS NOW GONE — sharding stage B, measured on the same arms

`agents/sharding.md` §8 predicted that the conservative scan would fall toward the below-wall rate under shards, at "a predicted ~16×", and said stage B must measure it rather than quote the sentence. It did. Both arms re-run paired on one machine, one day, `TICKS=1000 RUNS=2` — a flat build and a sharded one, same guest, same allocator, same trigger:

| arm | guest heap | flat, worst tick | sharded | flat ms/MiB | sharded ms/MiB |
|---|---|--:|--:|--:|--:|
| `stw` | 2.715 MiB | 190.03 ms | **182.68 ms** | 70.0 | **67.3** |
| `stwbig` | 6.406 MiB | **5,299.70 ms** | **446.72 ms** | 827.3 | **69.7** |

**11.9× on the worst tick, and the ms/MiB rate lands ON the below-wall band** — 69.7 against 67.3, a 3.5% difference where it was 11.8×. That is the honest statement: the wall's tax on a collection is not reduced, it is *gone*, and what is left is the below-wall rate the `stw` arm always paid. The load tick moves with it, 5,252.17 → 434.06 ms (12.1×).

The flat `stwbig` figure here is 5,299.70 ms where the table above records 8,580.67 for the same arm. Both are real and the difference is the machine on the day; the paired number is the one to quote, because it is the only one measured against its own baseline in the same session. Against the recorded 8,580.67 the improvement would read 19.2×.

**Two smaller things fell out of the same run and both are recorded rather than inferred.** The below-wall arm got *faster* (0.96×), because the conservative scan walks the heap sequentially, which is exactly the shape that binds one shard to a local and steps a within-shard index. And Lua's OWN collector — the `luaGarbageIncremental` counter, which is not the guest collector and is printed so nobody attributes it to one — stopped scaling with memory: **0.843 → 1.537 ms flat across the two arms, 0.426 → 0.417 ms sharded.** One `propagatemark` is one indivisible unit per TABLE, and the largest table is now one shard.

**The wall notice is gone too, and this run is also the last evidence for why.** `run-gcbench.sh` used the notice as its discriminator, and in the flat baseline above it printed "silent" for BOTH arms — including `stwbig`, whose linear memory is 6.7 MiB. That is stage A's triage defect reproducing exactly: `note_wall` was reached from the chunk-level `P.memory()`, which sees the DECLARED size, and from `sync_memory`, which fires only when the size CHANGES. The script now reports the guest's own `heap=` line instead, which is a better instrument anyway — both arms report a number rather than one reporting silence.

#### The same thing through the real runtime, which is what closes it

The table above is a bare Lua mod, on purpose. The confirmation is a real packaged guest — a `.wat` at `-opt=3`, `--persist=table`, through the real emitted `mem_grow` and the shipped `fk_mod.lua`, one 8-page `memory.grow` on tick 30 and nothing else in the mod:

| declared → grown | crosses 4 MiB? | tick 30 | notice |
|---|---|--:|---|
| 60 pages → 68 (3.75 → 4.25 MiB) | **yes** | **2,959 ms** | fires |
| 20 pages → 28 (1.25 → 1.75 MiB) | no | **9.6 ms** | silent |

**308× for the identical grow, eight pages either way.** `luaGarbageIncremental` is 0.01 ms on the heavy tick, so Lua's own collector is not in it. That is the A/B to quote, and it is the pair the shipped notice discriminates.

#### Why `bin/lua52f` cannot see any of it

**The oracle is stock Lua 5.2.1, whose array part grows to 2³⁰.** It prices the same crossing grow at **3.0 ms against 1.3 ms** for a non-crossing grow of the same size — a 2.3× slope where the game has a 27× cliff — and shows exactly one array doubling (16 MiB → 32 MiB) where the game shows a representation change. The 15/250/2,800 ms table above was taken in the game; **the ~19-rehash explanation attached to it was reasoned from the vendored `ltable.c`, and the vendored `ltable.c` does not describe Factorio's build.**

This is a real hole in the oracle and it is worth stating in the same terms `CLAUDE.md` uses for the homebrew-Lua rule: **`lua52f` is Factorio-shaped for the SANDBOX — the missing libraries, the doubles-only numerics, `string.pack`'s truncating cast — and not for table internals.** Anything about the cost of a large table measured host-side does not transfer, in either direction.

#### Six ways to grow, and none of them helps

Measured in game, same table, same 222,208 words across 2²⁰:

| shape | tick |
|---|--:|
| ascending — what `mem_grow` does | 2,632 ms |
| descending | 2,543 |
| top index first, then ascending | 2,669 |
| 4,096-word chunks | 2,720 |
| `rawset` ascending | 2,771 |
| binary spread, then fill | 5,439 |

**Five within 4% of each other and one twice as bad.** The order the keys arrive in cannot matter, because the cost is Lua rebuilding the table's representation and not the inserts. Building the same words from scratch costs the same (2,871 ms), so filling a fresh table and swapping buys nothing either — and the swap would have to respect the fact that in table mode `storage.fk_mem` **is** `MEM`, which `sync_memory` already handles but which is a hazard for no gain.

And **Lua 5.2 has no way to presize an array part past 2²⁰ regardless.** The one constructor form that presizes at all is `{table.unpack(t, 1, n)}` — `OP_SETLIST` calls `luaH_resizearray` directly — and it refuses `n` at 1,000,000 with "too many results to unpack" (`LUAI_MAXSTACK`). It is also slow where it does work: 250,000 words that way took 571 ms against 22 ms for a plain loop.

#### What DOES work, and what it would cost

Keeping any one table under 2²⁰ keys. Measured in game, covering the same 8 MiB:

| | build | 200k stores |
|---|--:|--:|
| one flat table of 2,097,152 | 5,284 ms | 2,410 ns each |
| **four shards of 524,288** | **215 ms** | **84 ns each** |

**24.6× on the build and 29× on the store**, with the shard index (a division and a modulo, in Lua) already paid inside the 84 ns.

`CLAUDE.md`'s heap-budget row says sharding is "3–5.5× on every load and store" and rejects it. **That measurement is right and its conclusion is only right below the wall.** 84 ns against a small flat table's 26 ns is the same ~3×; the row was taken under `bin/lua52f`, where a table has no wall, so it measured the cost and could not measure the benefit. The honest statement is: **sharding costs ~3× below 4 MiB and wins ~29× above it**, and a guest's declared memory is known at compile time.

That is not this document's change to make — it is every emitted memory access, the loop guard's word index, `mem_copy`/`mem_fill`/`fk_wstr`, `MEMPACK`, and both persistence modes. It is recorded here with numbers so that whoever does it is not re-deriving them, and so that nobody re-runs the host-side version and concludes the opposite again.

> **That work is now designed and measured: [`agents/sharding.md`](sharding.md).** Two things there supersede the framing above. The "~3× below the wall" cost is the price of the form the design does NOT take — folding the shard test into the bounds check every access already carries makes the below-wall cost **0.93–1.01×**, measured paired and end to end on a real mod. And the wall is not the only thing sharding fixes below it: the flat table degrades continuously from 1 MiB, so always-sharding starts paying at about **3 MiB**, a megabyte before the cliff.

#### What shipped instead

`fk_mod.lua` now says so, once, when a guest's linear memory first passes 4 MiB — the same channel and the same reasoning as the heap-budget notice four doublings above it, which exists because "the first downstream mod spent two milestones attributing the pause to `--persist` with no way to see its own heap". A guest crossing this wall has no other way to find out: the budget notice does not fire until 16 MiB, and nothing host-side can see the wall at all. *Enforced by `TestAGuestPastTheFourMiBWallSaysSo` (both modes) and `TestAGuestUnderTheFourMiBWallSaysNothingAboutIt`.* `mem_grow` and `TestAGrowAcrossTheFourMiBWallIsStillCorrect` carry the "do not re-try a presize" record, the latter as the correctness half a reshaped `mem_grow` would be most likely to break.

### What the instrument was, and why the old one could not see any of it

`--benchmark --benchmark-verbose` reports `avg / min / max` **per run**, and every run begins by loading the save — which for a guest holding a 2 MiB heap is `_initialize` plus unpacking that heap into a Lua word table, measured at 211 ms. Stage C read `max` as the worst tick; it was the load tick in both arms. Its own caveat said so and told the reader not to quote the script.

`--benchmark-verbose <counters>` reports one CSV row per tick instead:

```
tick,wholeUpdate,luaGarbageIncremental,scriptUpdate,
t0,214855083,1398667,211534666,          <- NANOSECONDS
t1,545667,56459,239459,
```

so the load tick becomes a row to DROP and what remains is a distribution. The technique — `--benchmark-verbose`, header-driven column parsing, a steady-state window — is **ported from the first downstream mod's `bench/run.sh`**, which hit this identical wall first and solved it. Its commit says the same thing in the same words: *`max_ms` is not a worst tick — every `--benchmark` run loads the save inside the measured window.*

**Read the header, never count the columns.** Factorio emits the counters in ITS OWN canonical order, not the order asked for: the command line above requests `wholeUpdate,scriptUpdate,luaGarbageIncremental` and the header comes back `tick,wholeUpdate,luaGarbageIncremental,scriptUpdate`. A positional parser reads Lua's GC time as the guest's. This is the downstream file's warning, confirmed here on the first run.

`scriptUpdate` is the column, not `wholeUpdate`: the collector is Lua in `on_tick`, and `wholeUpdate` carries the entity and belt updates as noise.

### The two defects in the script itself

**The unquoted expansion, fixed at the root rather than quoted harder.** `build_arm` returned its mod directory on **stdout**, so any stray line there became part of a `--mod-directory` argument; Factorio silently ignored the two-line path and the script printed a full table of worst ticks measured against the base game with no guest in it — twice, and neither table looked wrong. The same expansion created three directories NAMED BY SHELL OUTPUT that rode into `331090f` unnoticed (`8b917b9`). There is no stdout return channel any more: paths are a pure function of the arm name, computed identically by builder and runner, and `check_path` REFUSES any path containing a newline before it reaches the game or a JSON file. This also retires stage C's "unresolved flake", which was this and not something about Factorio.

**Did the mod load, and did the arm do its job.** Four fatal checks: the guest must log during `--create` (a map made without the mod saves an empty heap); `Checksum for script __fk-gcbench-<arm>__` must appear; the guest must log inside the benchmark; and there must be no script error. Plus the one stage D had to add — **an arm that completed no collection FAILS** rather than printing numbers, naming the livelock, the `Phase()`/`Deadlines` symptom and `fkgc.SetBudget` as the knob. That check is the only reason the livelock below was found rather than published.

### The livelock, and the guest that was configured outside its own envelope

`examples/gcbench` allocated 200 nodes per tick. At 48 bytes and 60 UPS that is **576 KB/s**, against the **189 KB/s** of sustained reclaim this document measured for the default 0.5 ms budget and then published as the replacement acceptance criterion the whole GO verdict rests on. The benchmark was running at three times its own stated envelope and the arithmetic had never been done.

The consequence was not a slow arm. Every mark step re-scans the pages dirtied since the last one before attempting to terminate, so at that rate the budget went entirely to the backlog: 600 ticks in phase 1, `cycles=0`, `steps=0`, the heap climbing 2.85 → 8.68 MB with five `memory.grow` calls — **exactly as if there were no collector, which is the thing a guest opts in to avoid.** `deadlines=0` throughout, and that is correct rather than a second bug: `markDeadline` is `4 × (heap granules / budget) + 600`, which at this heap is 1,296 steps, and the benchmark was shorter than its own escape hatch.

The fix is the guest's configuration, not the collector: `perTick` is 20 (57.6 KB/s, inside the envelope with the same 3× headroom this document claims for an ordinary event handler) and the trigger is 256 KiB, shared by both arms. The budget is left at the default **on purpose** — what the benchmark is for is what a guest gets without tuning.

> **The number to carry forward: a paced collector needs `budget > the guest's per-tick dirty-page re-scan cost`, and that is a different and stricter constraint than "the budget is the pause".** `SetBudget`'s documentation now says so. Stage C found this as defect 2 and fixed the symptom with `markDeadline`; stage D is the second time the same guest fell over it, which makes it a property of the design rather than an accident.

### The first real guest, and its verdict

**It took this decision twice, in opposite directions, and the second one is what ships.** Everything from here to the end of (d) is the **2026-08-01** pass, kept as measured because the losing arm's numbers are the useful half in both directions; **"The same guest, re-measured" below it is the 2026-08-02 pass that flipped it to `--gc=collected`**, and what moved between them was not the collector.

The first mod outside this repo to build both arms measured them and, on that day, shipped **leaking** — the expected and correct outcome for a guest whose heap is already diet-bounded, rather than a failure of the feature. Its record — `../BetterBeltBalancer/CLAUDE.md`, "The collected-mode postscript" — is the first evidence any of this has from a guest nobody here wrote. Four things it establishes:

**(a) The budget calibration gap, in the direction that matters.** 1024 granules, calibrated host-side at ~0.5 ms, measured **623 µs median / 1.30 ms p90 / 2.1 ms worst** through that mod's emitted Lua in real Factorio. The median is the calibration; the tail is 4.2× it. `gcbench` above is the same shape and worse. `SetBudget` carries both rows now.

**(b) Where the deferred collection LANDS, which is a placement and not just a coverage gap.** This document already said a mass-builder is not covered at any budget worth having. What it did not say is *when the bill arrives*. That mod compiles 200 networks inside ONE `on_init` dispatch — and **no paced step can run inside a dispatch** — so the create allocates 1.3 MB with `cycles=0` and the entire collection is deferred to the next tick source. For an event-only guest with no `on_tick` of its own, that is the first ticks after a **LOAD**: 152 collector steps over ticks 0–151, 105 ms of script, on a mod whose headline since M4 is that a finished build runs no script at all.

`gcbench` reproduces the shape independently and larger: its `on_init` builds a 2.3 MB live set in one dispatch, and the paced arm's first collection is **552 steps over ticks 0–542**, nine seconds of game time after the load. Two guests, two heaps, same placement.

> **State it as placement.** A guest whose bulk work is one dispatch does not pay for it during that dispatch. It pays on the first ticks after the next load, in a lump, and that is where to look for it.

**(c) The growth policy beat TinyGo's doubling by 14%, with zero collections run.** On the n=200 create, `-gc=leaking`'s 1.14 MB of allocations sat in a 1.92 MiB arena because `growHeap` doubles; `fkgc` reached 1.50 MiB for the same work, with `cycles=0` on that line — **nothing had been collected.** That is a benefit of the `-gc=custom` seam that has nothing to do with collecting, it was unrecorded, and it is the only measured argument for the seam that survives a guest deciding not to collect. It does not survive the collector's 163 KiB of static metadata, which is linear memory in every save and every join whether or not anything is ever collected — so it is a real effect and not a recommendation.

**(d) The verdict AS TAKEN ON 2026-08-01, and why shipping leaking was the right answer to the question as it stood.** That mod's rule was: flip if collected costs nothing the steady state can see AND bounds a heap that is actually growing. Neither half passed — it gained 152 ticks of script per load on a mod whose headline is that it has none, and the heap it would bound is 1.9 MiB that its own diet had already made invisible to Lua's collector (`luaGarbageIncremental` at 8–21 µs of mean tick in all three arms, the no-mod control included). It costs +25.9% of `fk_module.lua`, +11.9% of the zip, +2.3 ms on every load and 163 KiB of permanent linear memory.

And on the workload the feature was designed for, it wins outright — 54× the mod's own churn suite, 5,400 mutations:

| | `-gc=leaking` | `--gc=collected` |
|---|--:|--:|
| linear memory at the end | **4,114,800 B** | **585,032 B** |
| the curve | 182 → 445 → 969 → 2,018 → **4,115 KB**, still climbing | 115 → 218 → **418 KB, flat from t≈2.4 s** |
| `memory.grow` | on the doubling ladder | **6, none after t≈2.4 s** |
| collections / mark deadlines | — | 8 / **0** |
| items in / recovered | 16,000 / 12,075 | 16,000 / **12,075** |
| final audit | `clusters=14 parts=29 nets=14 drift=0 unbuilt=0` | **identical** |

**7.0×, checksum-identical, a heap that plateaus against one that only climbs.** That is the acceptance evidence, and it came from a guest nobody here wrote.

> **A well-disciplined guest measuring both arms and shipping leaking is the EXPECTED outcome, not a failure of the feature.** The collector exists so that a guest whose heap is actually growing has an answer, and so that a contributor who should not have to know about the heap budget has one. A guest that has already done the diet has nothing for it to reclaim, and the honest table says so. What would reverse the decision is written down there: an allocation regression in the compile path (the 54× table is what one looks like), or a guest that gains a real `on_tick` and therefore paces as designed.

**And the collector's own valve was never touched by a real guest.** No `fkgc:` line was logged in any run, `Stats().Deadlines` was 0 everywhere including the 54× churn, and `markDeadline`'s slack and floor — unvalidated against a real guest before that pass — held. *(That last clause did not survive: the same guest's globals later crossed one step's budget and every collection ran to its deadline. See "The root-scan floor" below — the counter that was 0 here is the counter that found it, once somebody looked at it.)*

### The same guest, re-measured — and it flipped to collected

**2026-08-02, on the sharded pin, and it is what ships.** The decision above was re-taken because two of the four things it rested on had moved underneath it — and the useful part is that **neither of the two was the collector**.

| the reason leaking won on 2026-08-01 | what it was then | what it is on the sharded pin |
|---|---|---|
| **152 ticks of collector script after every load** | measured, and structural to a guest that builds 200 networks inside ONE `on_init` dispatch where no paced step can run | **71 ticks / 65–71 ms.** Sharding stage C and the grow pacing more than halved it. Still structural, still a mass-builder's shape rather than play's |
| **163 KiB of permanent linear memory** | a `.bss` reservation | **73,112 B** — stage C deleted the reservation, and `MetaBytes` is now `32,116 + 40,960 × ceil(heap / 4 MiB)` |
| **the heap it would bound is 1.9 MiB the diet already made invisible** | true of a fresh save, and it was the load-bearing half | **false of a 300-hour one.** ~26 MiB of permanent heap on a busy four-player server, and the doubling into it is a single-tick stall nothing downstream can bound |
| **+25.9% of `fk_module.lua`, +11.9% of the zip** | measured | **worse: +32.4% and +13.7%.** This is the one cost that did not move in leaking's favour, and it is now the whole of leaking's case |

**And the stall it was avoiding by discipline was measured rather than projected.** The 2026-08-02 projection put a 16→32 MiB `memory.grow` at ~450 ms from 107 ns/word. Driven up the ladder for real — the same guest, one leg of its marathon suite, 3,400 net-zero operations:

| rung | words filled | leaking worst tick | ns/word |
|---|--:|--:|--:|
| 2 → 4 MiB | 524,288 | 48.7 ms | 92.9 |
| 4 → 8 MiB | 1,048,576 | 120.3 ms | 114.8 |
| 8 → 16 MiB | 2,097,152 | 226.1 ms | 107.8 |
| **16 → 32 MiB** | **4,194,304** | **782.4 ms** | **186.5** |

The two middle rungs land on the 107 ns/word model within 5%, which is what says the instrument measures what it claims. The last one is **1.74× the prediction**, and the excess is the shape `agents/sharding.md` §15 names — a 16 MiB grow creates eight new 2¹⁹-word shards and pays each one's last array-part reallocation on top of the fill. **So the projection's arithmetic was sound and its answer was optimistic, in the direction that matters.** Same 3,400 operations, the other arm: 0.52 MiB of linear memory at the end against 31.9, worst tick **71.4 ms against 782.4** — and the collected arm's next four worst ticks are lower too, so the bounded heap is not being bought with a worse ordinary tick.

Everything the steady state could see came out a wash or pointed the wrong way for leaking: `scriptUpdate` was 1.5–2.9 µs in both arms **and in a no-mod control**, and collected was faster on saturated `avg_ms`, on saturated `scriptUpdate` and on that mod's own recompile hitch, all inside a session whose control drifted more than the difference.

> **The transferable lesson is not "collect by default" — this document already said that — it is that ALLOCATION DISCIPLINE IS NOT A SUBSTITUTE FOR A GROWTH LAW.** The 2026-08-01 decision was correctly reasoned from a correct premise whose scope nobody had stated: *there is nothing here for a collector to reclaim* was a measurement of a fresh save, and a guest that allocates a bounded amount per player edit still climbs the doubling ladder over three hundred hours. A diet bounds what is LIVE. Nothing but a collector bounds what has ever been allocated, and `memory.grow` prices the second one.

**What would reverse THIS decision**, from the same mod's own record: an `fk_module.lua` a portal or a load time cannot carry (that is the whole cost, and it is the only one left); a guest whose live set stops being ~9 KB, since every argument above rests on the collector having almost nothing to retain; or a single-player-only audience, whose quiet column never reaches a doubling worth the name.

### The flaky gate, and the discipline it did not have

`TestAHostCallIsUnderM0sTwoMicrosecondGate` failed at 2005–2156 ns against a 2000 ns wall-clock threshold whenever tinygo builds ran beside it, on clean `master`, with nothing about the ABI changed. **And the assertion that fired was never M0's.** The no-op round trip M0 wrote the number about is ~530 ns and has 3.7× of headroom; the measurement sitting 5% under the wall was the string return, which the test's own comment already admitted "is not covered by M0's wording". A gate that fails for a reason its own comment disclaims trains people to re-run.

It now follows the discipline every measured number in this repo already has — nothing asserted against a bare wall-clock figure without something measured in the SAME RUN to say what the machine was doing:

- **A floor**: a plain Lua method call on the same object through the same instrument, no ABI at all. 30 ns quiet.
- **An A/A**: the floor measured again, bracketing the ABI measurements rather than sitting beside them. 0.4% spread quiet, 2.0% under load.
- **M0's 2000 ns, scaled by the measured dilation** and by nothing else. On a quiet machine it is M0's threshold verbatim and a 4× ABI regression still fires it.
- **The string return moved to a RATIO** against the no-op measured beside it — 3.56× quiet, 4.53× under 22 spinning threads on 11 cores, bar 7×.

**The floor has to run for the same WALL TIME as what it qualifies, not the same iteration count**, and getting that wrong is how the first version of this fix was useless: at 33 ns the floor finished 400,000 iterations in 13 ms and — taken as best-of-three, which finds a quiet slice — reported a 1.13× dilation for a machine on which the ABI bodies had dilated 2.5×. At 16× the iterations both legs run ~200 ms and the floor reads 2.00× against the ABI's 2.55×. Verified by running the test under 22 spinning threads: the old assertions fail there, the new ones pass, and the A/A stays at 2%.

#### The SECOND instance, and it wore a different costume

`internal/guest.TestWhatAHostCallCostsThroughARealGuest` failed mid-session with per-call numbers ~100× high under machine load and then passed on a re-run — the same trained-to-re-run shape, on a test that had **no wall-clock threshold at all**. What fired was its ordering check, "a step that does strictly more cannot cost less". Three things were wrong and the first is the interesting one:

- **That sentence is true about WORK and false about MEASUREMENTS of work**, and the difference is the resolution of the run. Two of its legs genuinely cost the same: under `--persist=packed`, `dispatch, no host call` is 1195 ns and `call, no blocks` is 1113 ns, because `ReloadScript` takes no argument block and returns no return block, so it dirties no page — and packed's flush, which is the whole cost of a packed dispatch, has nothing extra to do. Demanding a strict ordering between them was demanding resolution the harness never had. It got it by luck most of the time.
- **The legs were sized in ITERATIONS and span three orders of magnitude.** At 2,000 reps the cheapest leg is 0.9 ms of signal differenced out of a ~10 ms process while the most expensive is 300 ms, so the cheap legs were nearly all process startup and the expensive ones nearly none — and then they were compared. Every leg **and the floor** is now sized by TIME (100 ms each), which is the same rule stated the other way round.
- **There was no floor**, so "the string return measured cheaper than a dispatch" and "the machine got slower between two runs" were indistinguishable.

The fix is the discipline above: a floor (a plain Lua call in the loop where the other legs dispatch, in the same chunk and the same stub environment), an A/A bracketing the five measurements, ratios rather than wall-clock, and a NOISY-RUN banner that reports loudly and keeps asserting. The ordering check survives with the word it was missing — *measurably* cheaper, where measurably is the A/A spread with a 10% floor — and one real ratio gate replaces it as the thing that would catch a regression: a tier-2 map argument against a bare dispatch, measured 23.9× under table and 124–295× under packed, barred at 3×.

**Two things this cost that are worth carrying.**

**The rep ceiling has to clear the CHEAPEST leg at the target, and getting it wrong reproduced the first instance's exact failure.** A 400,000 cap ran the ~19 ns floor for 7.6 ms instead of 100 ms; under 22 spinning threads it reported a floor A/A of *"15 and −5 ns, spread 412%"* — a **negative floor** — while the legs it was supposed to qualify were dilating 2–3×. The sizing now converges on measured wall time over at most three passes rather than trusting one pilot, because a short run on a loaded machine is precisely the measurement most likely to be wrong in the direction that matters.

**AND `go test` CACHED THE RESULT, which nearly produced a fabricated verification.** The first "under load" run printed numbers byte-identical to the quiet run, and `ok … (cached)`. That is the same stale-input trap as `run-gcbench.sh`'s `if [ ! -f "$wasm" ]`, which cost four wrong conclusions in a row: **identical numbers across genuinely different conditions is a stale input, not a result.** Timing verification runs with `-count=1`.

Verified the way the first instance was: under 22 spinning threads on 11 cores at load average 28–33, the **old form fails 3/3** (`call, no blocks measured cheaper than a dispatch that does nothing`) and the **new form passes 4/4**, with the NOISY-RUN banner firing on the runs that deserve it.

### The silent SKIP, and the thing Go does not give you

`/bin/` is gitignored — correctly; `lua52f` is a build artefact — so **every fresh `git worktree add` starts without the host-side oracle**, and about thirty tests across `internal/luagen`, `internal/spectest`, `internal/factorio`, `internal/guest` and `internal/luahost` respond by skipping. `go test` prints nothing for a skip without `-v`, so the transcript reads `ok …/internal/guest 0.4s` for a package whose entire collector suite declined to run. Stage D started in that state and it was indistinguishable from a pass.

The fix is not to make thirty tests fail — each is right to skip individually. The absence is reported ONCE, by something that fails:

- `internal/luahost.TestTheOracleIsBuilt` fails when `bin/lua52f` is missing, and `ErrNotBuilt` now names the worktree remedy first, because `make lua52f` in a worktree used to re-fetch and rebuild Lua from source to reproduce a binary the main checkout already has.
- **`make lua52f` copies from the main checkout when it is in a worktree** — `git rev-parse --git-common-dir` says whether it is one, and `lua52f` is a pure function of the tarball and the committed patches, which every worktree shares.
- `make test` now depends on `$(LUA52F)`, so the repo's own entry point cannot reach the silent state at all.
- `internal/guest.TestTheGuestToolchainIsAvailable` does the same for tinygo and wasm-opt, with `-short` as the opt-out — the idiom that package already uses — because an external toolchain's absence IS a decision where the oracle's is an accident of checkout mechanics.

**There is no loud channel below a failure, and this was established by building one.** A passing test that writes a banner to `os.Stderr` was written and discarded: `go test` runs the test binary with its output captured and prints it only when the package fails, so the banner never reaches anyone. That is why both guards above are failures and not log lines.

### Gates

| gate | result |
|---|---|
| `go test ./...` | green, all 11 packages |
| spectest, default opt, **both gc modes** | 15,675/15,675 in each. `PASSRATE` unmoved |
| the fixed `run-gcbench.sh` in real Factorio | 1,200 ticks × 3 runs × 2 arms, both arms completing collections, table above |
| the de-flaked gate under load | passes under 22 spinning threads on 11 cores, where its previous form fails |
| the silent-SKIP guards | verified by removing `bin/lua52f`: `internal/luahost` goes red and names the remedy; `make lua52f` then copies it back from the main checkout |
| `gofmt` / `go vet` | clean |
| codegen/runtime | untouched, which is why the full 16-combination sweep is not here |

### What a later stage needs to know

1. ~~**The in-game/host-side gap is the open item.**~~ **ANSWERED, and the recorded lead was wrong.** The prescribed test — re-measure a collection either side of 4 MiB — was run (`ARMS="stw stwbig" ./scripts/run-gcbench.sh`, and `examples/gcbench` now carries `nlive` as the `gcbig` build tag for it). **The arm that reproduces 88 ms/MiB (84.5) never crossed the wall**; its heap is 711,680 words, which stage D's own table already said. Above the wall the same collection costs 15.9× more again, so the wall is a second and larger effect, not this one. The 2.7×–6.4× is the INTERPRETER: Factorio's Lua reads a table 4–6× slower than `bin/lua52f` does, on the same below-wall table, with the loop machinery at 1.04–1.10×. **So a host-side ms/MiB figure carries into the game at ~2.5× below the wall** — stage C's 555× ratio was always the part that travelled, and the milliseconds now travel too, with a constant. [`agents/sharding.md`](sharding.md) §2.
2. ~~**`mem_grow` appends one word at a time and pays Lua's rehash for it.**~~ **ANSWERED, and the answer is that there is nothing to fix in `mem_grow`.** The 2.8 s is real and reproduces in a bare Lua mod with no guest in it; the rehash attribution is wrong; six append shapes land within 4% of each other; Lua 5.2 cannot presize an array part past 2²⁰ at all. The real finding is ~20× on every access above 4 MiB, permanently, plus ~2.9 s on every load. Full record and the shard numbers that DO work: "The 4 MiB wall" above. **And they now DO work in the shipped runtime**: sharding stage B landed and the same `stwbig` arm went from a 5,299.70 ms worst tick to 446.72, whose 69.7 ms/MiB is the below-wall band. There is no wall left for `mem_grow` or anything else to cross.
3. **A budget must clear the guest's dirty rate, not just the pause target.** Two guests have now fallen over this. `Phase()` stuck at 1 is the symptom and it is silent.
4. **A collection deferred out of one dispatch lands on the ticks after the next LOAD.** Confirmed on two guests at two scales. It is a placement, and a guest author reading only "mass allocation is not covered" will look in the wrong place for the cost.
5. ~~**The terminating step is not budgeted the way the others are** — 65× at the default budget in game, 1.17× host-side.~~ **ANSWERED at sharding stage C, and it was not the terminating step.** It was FOUR defects, all of which put collector work somewhere the budget could not see it, and all of which land in the same tick as mark termination — which is why one step got the blame. The record is in [`agents/sharding.md`](sharding.md) §14; in one line each:

   - **The instrument could not see the work.** `step()` zeroed its own accumulator on entry and left it dirty on exit, so everything the mutator's own calls charged between two steps was either discarded or attributed to the wrong side. `Stats().MaxUnpaced` is the missing half, and 1.17× host-side against 65× in game was never one number measured twice.
   - **The sweep-ahead in `allocSpans` was UNBOUNDED**, and at the instant marking terminated `findSpanRun`'s window was EMPTY — stage C forbade the mutator from claiming a span above the sweep cursor — so the next allocation swept until a span fell free, inside an event handler. `clsFresh` answers the cursor's question per span and the sweep-ahead is one bounded bite.
   - **A re-scan of a dirtied page re-read the WHOLE object.** A guest writing one slot of a 44,000-entry pointer array per tick cost 176 KiB of re-scan per tick, which is eleven steps of the default budget, forever: marking could not terminate. In a real Factorio that is 1,100 ticks in phase 1 with `cycles=0`. A re-scan is one SPAN now.
   - **The root re-scan was free by omission.** `markStep` charged what the scan DISCOVERED and never the range it walked, which is the guest's whole globals section and a number nothing here bounds. It is charged.

### Recorded future work, carried forward

> **CLOSED — see "The Rust collector, as built" below.**

**Rust has no `fkgc`, and `--gc=collected` is Go-only until it does.** Unchanged from stage C, and the go/no-go is still the root set: `-gc=custom`'s value here is that TinyGo *hands over* `markStack()` and `findGlobals()`, rustc has no equivalent seam, and Rust keeps live references in wasm locals that a conservative scan of `[__global_base, __heap_base)` plus the shadow stack cannot see. Answer that before writing the allocator, which is the easy half.

**And do not repeat the leaking-is-fine-in-Rust folk claim.** `guest/rust/fk`'s `#[global_allocator]` is a bump allocator whose `dealloc` is a no-op, so a Rust guest's linear memory only grows and `agents/guests.md`'s heap budget is its entire answer.

---

## The root-scan floor — the OTHER way a mark never terminates

**A guest whose root set is bigger than one step's budget could never finish a mark, in either language, at any allocation rate including zero.** Filed by BetterBeltBalancer through fklua-ports as item 21, fixed 2026-08-03, and it is the second defect in this collector whose entire symptom was a number pointing at the wrong cause.

### The mechanism

A termination attempt does two things: it re-scans `[__global_base, __heap_base)` wholesale, and it charges what it walked — `rootWords>>2` granules. Then it asked whether it had budget left, and if it did not it deferred to the next step, which did exactly the same thing.

**But the scan had already happened.** `charge()` is post-hoc accounting for a walk that is complete, so "out of budget" said nothing whatever about whether marking was finished. When the roots cost more than the whole allowance the charge saturated to zero on *every* attempt, so the answer was always "defer", and the only thing that ever ended the phase was `markDeadline` — hundreds of steps later, each one having re-walked the whole root range and banked nothing.

Measured on `examples/gctorture` at 390 root words (97 granules of charge), before the fix:

| budget | steps | termination attempts | deadlines |
|--:|--:|--:|--:|
| 1024 | 3 | 1 | 0 |
| 64 | 915 | **903** | 1 |
| 32 | 1,222 | **1,205** | 1 |
| 8 | 3,051 | **3,014** | 1 |

**And it gets worse as the budget falls**, because `markDeadline` scales as `heap/budget` — so the one escape recedes exactly as the condition worsens. The Rust arm is a line-for-line mirror and reproduced at **3,398 attempts over 3,436 steps**.

### Why it was expensive to find, and the doc that was wrong

The only symptom a guest can see is `Stats().Deadlines` rising with `Phase()` stuck at 1 — which is **identical** to the dirty-rate livelock `markDeadline` was built for. `SetBudget`'s own comment and this file's "what a later stage needs to know" item 2 both said, without qualification, that this symptom means the allocation rate is over the budget. For this cause that advice is not merely unhelpful, it points at the one knob that **cannot** work: lowering the budget makes it worse, and raising it fixes it only by accident, at whatever threshold nobody can compute from outside. The downstream mod mis-filed its deadlines for a day on the strength of that sentence. Both comments now name both causes and the one-line test that separates them.

### One counter, two diagnoses — the escape is split by cause

**`Deadlines` counted two different escapes, and every document that read it read the wrong one at least once.** The escape fires on `steps > markLimit || stalls >= markStallLimit`, which is two conditions with two remedies:

| | fires when | what it says on its own |
|---|---|---|
| **`StallEscapes`** | `markStallLimit` consecutive windows of `markStallWindow` steps in which the pending dirty list never emptied AND scan work did not shrink | the mark has stopped CONVERGING. A diagnosis, and it fires within a few dozen steps of that becoming true |
| **`StepEscapes`** | the mark ran past `markDeadline` = `4 × (heap granules / budget) + 600` steps | the mark is affordable but SLOW for the heap it is on. A backstop, deliberately far enough out that a short run finishes first |

`Deadlines` is their sum, exactly and unchanged, so every existing reader — `examples/gcbench`, `examples/gcsave`, `scripts/run-gcbench.sh`, and the downstream mods that grep `deadlines=` — reads the number it always read. `MemStats` gains two fields and nothing is renamed. Mirrored in Rust (`step_escapes` / `stall_escapes`), and both `gctorture` corpora expose them at stat indices 27 and 28.

**Two misdiagnoses are what asked for it, and both are recorded above.** The root-scan defect presented as `Deadlines` rising and was filed against the allocation rate for a day, on the strength of a sentence in this file — that is the section this one sits under. Then the same mod's marathon suite reported six of these and its own notes attributed them, in writing, to the *write rate of the two legs they were counted in* — including a leg that allocates **16 bytes per operation** and could not outrun anything. That attribution was retired as unsupported, and the reason it could not be checked is that the counter carried no cause: a collection started under one leg is still marking when the schedule has moved to the next, so the leg column locates a mark termination and not a mutator.

**What the split does and does not buy.** It does not identify the root-scan case — `EffectiveBudget() > Budget()` does that, and the collector logs a line the first time the floor binds. It does not say WHICH allocation caused a stall; that still wants a per-collection trace no guest emits. What it buys is that a bare escape count can no longer be read as evidence for a cause it never carried, which is the failure mode both misdiagnoses actually had. *Gated inside `TestAnAllocationStormGrowsInsteadOfCollectingSynchronously`, which asserts the sum invariant and — because that leg is a dirty-rate storm by construction — that the escape it provokes is the STALL and not the backstop.*

### The fix, and why it is a floor rather than a resumable scan

**The scan cannot be made resumable, and the argument is soundness rather than measurement.** The roots live BELOW the heap and `ingestDirty` drops every dirty page below `heapBase`, so there is no write barrier over the globals at all. The terminate-time barrier is sufficient — instead of a tricolour one — precisely because the root range is read in ONE uninterrupted pass at ONE safe point (the precondition at the top of `collect.go`). A scan resumed across two safe points would read the first half at one and the second at the next, and a reference moved from the second half to the first in between is a live object swept, with no error anywhere. So the budget yields to the scan.

Three changes, all in `markStep`/`mark_step`:

1. **`EffectiveBudget()` floors `Budget()` at `rootScanCost() + rootScanMargin`** (64 granules, 1 KiB of heap). A guest whose statics are big enough for this to bind cannot have the pause it asked for, and this is the number that says so.
2. **The scan's cost is RESERVED, not merely afforded.** `markStep` holds `rootScanCost()` back and gives the queues what is left, then adds it again at the attempt — so a termination attempt is affordable on every step that reaches one, and no step spends more than its budget, because the reserve is part of it rather than added to it.
3. **The post-scan check dropped its `budget == 0` term.** The four predicates that remain — empty gray stack, no overflow, no half-scanned object, no re-scan owed — characterise a finished mark exactly, at any budget.

**Point 2 was a second cut, and the first one reintroduced the livelock from the other side.** The first version only floored the allowance and then guarded the attempt with `budget <= rootScanCost()`. That is fine on an idle guest and wrong on a busy one: a guest with a high dirty rate AND large roots spends the whole allowance on the dirty queue every step, arrives with less than the scan costs, and defers — every step, forever. `examples/gcsave` under Rust showed it as **`terms=0` with `phase` stuck at 1 across 300 ticks of a real Factorio and `cycles=0`**, which is a worse failure than the one being fixed. It was caught by `scripts/run-roundtrip.sh` and by nothing else: every host-side gate passed, in both languages, because none of them drives a guest that writes most of its heap every tick. **That is the gate earning its place** — and the per-tick `cycles=` column is what separates the two failures, since a schedule that merely MOVED still shows collections completing.

After the reservation, the same leg is **6 collections with 32/32 blocks intact, resuming mid-mark and mid-sweep in both persistence modes**. Its save ticks moved (`GC_SAVE_TICKS_RS` 120 → 180) because a Rust guest's ~420 granules of roots held back changes how many steps a mark takes; the script's header now carries the re-derivation recipe and the way to tell a shift from a regression.

Measured after, same guest, same legs:

| budget | steps before | **steps after** | attempts before | **after** | deadlines |
|--:|--:|--:|--:|--:|--:|
| 1024 | 3 | 3 | 1 | 1 | 0 → 0 |
| 64 | 915 | **12** | 903 | **1** | 1 → **0** |
| 8 | 3,051 | **17** | 3,014 | **1** | 1 → **0** |

**Nothing moves where the floor does not bind**, which is every guest measured here at the default budget: `TestNoPacedStepOverrunsItsBudget` is 1.11× as before, `TestTheBudgetTradesStepsAgainstWorstTick` is unchanged at 256/1024/4096, and `TestAllocatingThroughAPacedSweepKeepsLiveObjects` still shows the **dirty-rate** livelock firing 12 deadlines at a 128-granule budget — so the two causes remain distinguishable by measurement and not only by prose.

### The collector says so, because nothing else can

`rootWords` is measured inside a scan the host never sees and `Budget()` is a number the guest chose; the collector is the only thing that holds both. So it logs one `fkgc:` line, once per guest, the first time the floor binds — naming the cause, naming `EffectiveBudget()`, and saying explicitly that `SetBudget` is not the fix. That is the general lesson this defect is worth keeping for: **a condition only one component can observe is that component's obligation to report**, and every silent failure in this collector's history has been one of those.

*Enforced by `TestAMarkTerminatesWhenTheRootsCostMoreThanTheBudget` and `TestARustMarkTerminatesWhenTheRootsCostMoreThanTheBudget`, each of which asserts the termination, the absence of a deadline, that the floor is what did it, that the line was logged, and that the retained structure is unchanged — plus a control at the default budget asserting the floor does NOT bind and the line is NOT logged.*

---

## The grow pacing — what changed under the collector, and what it costs it

**Stage C's open question was answered and it was not the collector.** Sharding stage C left `paced` at a 22.9–30.0 ms worst tick with the collector's own worst step at 1.20–1.22× of a 0.5 ms budget, and attributed the gap to `mem_grow` by arithmetic. That attribution held up: the whole of it was `memory.grow`'s zero-fill, and it is now paced the way collection is. The design, the probe and the before/after tables are in [`agents/sharding.md`](sharding.md) §15; what belongs here is the part that touches this package.

### `growHeap`'s quarter is capped

`growCapSpans = 16` — one wasm page — bounds the SPECULATIVE part of the growth law. `needSpans` always wins, so a large single allocation still forces a large grow; the runtime's paced pre-build is what covers that case, and this bound is what a guest gets when it outruns the pre-build.

**The quarter was buying nothing measurable.** `mem_grow` has no fixed cost to amortise a big increment against: fitted over four increments at three heap sizes in a real Factorio, the intercept is negative at every size, and reaching 40 MiB in 640 one-page grows costs 0.984× what reaching it in 10 four-megabyte grows costs. The reason the quarter existed — "doubling is the ladder this package exists to keep a guest off" — is an argument against a growth law that scales with the heap, and a cap is more of that argument, not less.

The cap must clear `metaChunkSpans`, or `growHeap`'s coverage-crossing round-up asks for more than the cap allows on every grow that reaches a 4 MiB slice boundary. That is a compile-time assertion beside the constant. *Enforced by `TestTheGrowIncrementIsBounded`, which also asserts the chunk count keeps up — a bounded increment reaches a slice boundary far more often than a quarter-grow does.*

### What it costs this collector, stated rather than buried

Measured on `run-gcbench.sh`'s `paced` arm, three runs each side:

| | master | with the cap |
|---|--:|--:|
| worst `scriptUpdate` tick | 29.9 – 35.9 ms | **16.6 – 17.2 ms** |
| worst paced step | 1232 (1.20×) | 1230 (1.20×) |
| worst IN-CALL burst | 141 (0.14×) | **1024 (1.00×)** |
| mark escapes | 0 | 0 |
| grows per run | 12 | 44 |
| guest heap | 3.352 MiB | **2.988 MiB** |

**The in-call burst rose from 0.14× of budget to 1.00×, and that is the bound being REACHED rather than the bound moving.** `allocSpans` takes at most one sweep-ahead bite per tick, gated on `callWork`; ten times as many grows reach that path, so the bite that used to find a span early now runs to its full `sweepAheadUnits`. Stage C's 0.14× was luck. What the design promises is one bite, and one bite is what this is.

The storm shape is unchanged and slightly better: `TestAnAllocationStormGrowsInsteadOfCollectingSynchronously` ends the burst at **13.10 MiB against master's 13.88** with the same single latched mark escape and the same recovery — a bounded increment overshoots less. It takes 209 grows where master takes 22, which is the trade stated and not a regression: the assertion that matters is that the heap stops growing when the burst does, and it does.

### A note for a future stage: the residual grow tick is a SHARD, not a heap

What is left on a growing guest's worst tick is 16.2–19.1 ms, flat in heap size, at every odd megabyte — 2¹⁸ entries into a 2¹⁹-word shard, i.e. the last array-part doubling a shard ever does. It is Lua's, it is one indivisible reallocation per 2 MiB of guest memory ever taken, and no pacing in this package or the runtime can split it. The knob is the shard size, and the trade it sits inside is `agents/sharding.md`'s.


---

## The Rust collector, as built

**`guest/rust/fkgc` is BUILT, and `--gc=collected` is no longer Go-only.** The gate never named a toolchain — it asks for `fk_gc_step`/`fk_gc_dirty_base`/ `fk_gc_dirty_cap`, and a collected Rust guest exports all three — so `checkGC` accepted the new language without a line changing. The only edit it forced was to the diagnostic, which used to tell an author no Rust collector existed.

Turn it on with one flag and no source change:

```sh
cargo build --release --target wasm32-unknown-unknown -p <guest> --features fk/fkgc
```

`guest/rust/fk` owns the single `#[global_allocator]` site and the feature chooses what backs it, so a module can never link two allocators. `fk::gc` is the collector under the feature and a no-op shim without it — the Cargo equivalent of `guest/go/fkgc/off.go` — so the same guest source builds both arms.

### The go/no-go was the root set, and the answer is YES for a reason the question missed

The recorded objection was exact and its premise is TRUE:

> rustc has no equivalent seam, and Rust keeps live references in wasm locals that a conservative scan of `[__global_base, __heap_base)` plus the shadow stack cannot see.

The conclusion does not follow, because of WHEN a collection looks at anything. The safe-point precondition in section 1 says a step runs only at an outermost dispatch boundary — and at such a point **there is no guest frame at all**, so there are no live wasm locals to miss. Every reference the guest still holds is in a `static`, which is in `[__global_base, __heap_base)`, or in the heap, which is reached by tracing. Rust has no third place to keep something across a return.

What the argument depends on is rustc's LAYOUT, which is a fact about a compiler rather than about this repo, so it is measured and asserted rather than assumed (`TestARustGuestsRootRangeIsWhereTheCollectorLooks`):

```
__stack_low  = 0          __stack_high = 1048576    <- the shadow stack
__global_base = 1048576   __heap_base  = 1048576+   <- the statics
Global[1]: global[0] i32 mutable=1 <__stack_pointer> - init i32=1048576
```

rustc links wasm32 **stack-first**, so the statics are one contiguous range above the stack and the module has exactly one mutable global, holding a stack pointer and never a heap pointer. That is the same shape TinyGo's `wasm-unknown.json` produces with `--stack-first`, which is why the range test is the same two compares in both languages. The census agrees: a collected `gctorture` ends its statics at **1,050,620** — the collector's ~36 KiB of `.bss` moves `__heap_base` and nothing comes near `agents/sharding.md`'s 2 MiB shard line.

### What it costs, which is where the two collectors genuinely differ

**Allocation never collects, with NO exception.** `guest/go/fkgc` keeps one synchronous `Collect()` inside `allocSpans`, reached when `memory.grow` itself is refused, and it is sound there *only* because `markStack()` scans the live shadow stack of whatever event handler it landed in. That path is **deleted** here rather than ported: a refused `memory.grow` traps with a diagnostic. The alternative is not a pause, it is a mark that cannot see the frame it is standing in — which frees live objects and reports nothing.

`fkgc::collect()` survives with a stated precondition: the calling frame must hold no heap reference, i.e. it is sound as the ENTIRE body of an exported function invoked by the host at an outermost dispatch. That is how the corpus uses it and it is the only use the crate blesses. `collect_if_needed()` has no such precondition — it only OPENS a mark phase, and marking cannot TERMINATE except inside `step()`, which only `fk_gc_step` calls.

**The useful budget floor is higher, and it scales with the guest's statics.** The root re-scan is charged against the step budget (stage C's open item 5), and a Rust guest's statics are far larger than a TinyGo guest's — `RootWords()` on `examples/gctorture` is about 1,200 words, i.e. ~300 granules per termination attempt, against ~36 for the Go churn guest. Under the default 1,024 budget that is comfortable. Under 256 it is not: the termination attempt spends the whole allowance on roots, `drainGray` is entered with zero, and the mark can only end through `markDeadline`. **Below about 512, a Rust guest's mark terminates on the deadline rather than on the budget**, which is correct and is not what the pacing is for. `Stats().Deadlines` rising with `Phase()` stuck at 1 is that condition, and the knob is the budget.

### The mirror table

`guest/rust/examples/gctorture` mirrors `guest/go/examples/gctorture`, and the bar is stated as a property: every export whose value is pure arithmetic over the workload returns the same `u32` in both languages. `TestTheTwoCollectorsAgreeOnTheTortureCorpus` diffs them.

| field | agrees | why |
|---|---|---|
| `built` `before` `after` | **yes** | wrapping-`u32` folds of the graph |
| `interior` / `interior_want` | **yes** | an interior pointer retains its block in both |
| `large` / `large_want` | **yes** | a multi-span object survives in both |
| `one_past` | **yes** (both 0) | neither retains through a one-past-the-end pointer |
| `repoint` `repoint_seen` `repoint_want` | **yes** | the write barrier keeps a store into a marked object |
| `held` `held_after` `held_bytes` | **yes** | a 512 KiB live set survives a cycle intact |
| `kept` | no, by design | folds `roots.capacity()`; Go's `append` and Rust's `Vec` do not grow on the same curve |
| `believed` / `liveobj` | close, not equal | 422,048 B in 5,036 objects (Rust) against 430,192 B in 5,036 (Go) — the same object COUNT, different size classes |
| `backed` | no, by design | linear memory starts at 1 MiB in rustc and 64 KiB in TinyGo |

Retention on the Rust side is **1.013×** against the ~2× kill bar, which is stage B's figure for the Go collector to three decimal places.

### The defect this port found, and the shape of it

The first thing that failed was a checksum that differed between the LEAKING and COLLECTED arms of the same Rust guest **before any collection had run**. That ruled out the collector and pointed at the allocator, and the instrument that localised it was `torture_root_at`, which reads one node field at a time: every value agreed, so the graph was right and the memory under it was not.

`allocate` read its size-class table before lazy initialisation had filled it in. Rust has no `initHeap` the runtime calls, so this crate initialises on first use, and the first draft funnelled that through `allocSpans` — which is BELOW the table lookup. The first allocation therefore got class 0, and class 0 means UNASSIGNED: `refill` claimed a span, wrote class 0 back into the span table, computed zero slots of zero bytes, and pushed an empty run. The block came back at the span base and the span stayed marked free, so the next class to want a span was handed the same one and zeroed it with a live object in it.

Nothing trapped and nothing logged. It is the failure shape `gc_test.go`'s header describes — "the only symptom anywhere is a number that moved" — arriving one stage earlier than that header expects it.

**And it was nearly missed twice, by the same cache lesson in a new dress.** `go test` caches a result keyed on Go inputs, and `guest/rust/**` is not one: editing the collector and re-running the same `-run` filter returned the PREVIOUS run's output verbatim. Stage C's rule — verify the rebuilt wasm is actually rebuilt — applies to the Go test cache as much as to cargo's target directory.


### The roundtrip leg's save ticks — measured, and the leg is now DEFAULT

`scripts/run-roundtrip.sh` has a Rust leg -- `gcsave-rs`, from `guest/rust/examples/gcsave`, which logs the Go guest's lines character for character so every assertion in that script is reused unchanged. It is **in the default `GUESTS` list** since its cadence was captured. Both persist modes, both phases:

```
gcsave-rs --persist=table:  save at  30 -> seen=301, 32/32 intact across 2 collections, resumed MID-MARK
gcsave-rs --persist=table:  save at 239 -> seen=301, 32/32 intact across 2 collections, resumed MID-SWEEP
gcsave-rs --persist=packed: save at  30 -> seen=301, 32/32 intact across 2 collections, resumed MID-MARK
gcsave-rs --persist=packed: save at 239 -> seen=301, 32/32 intact across 2 collections, resumed MID-SWEEP
```

A Rust guest's heap **and its collector's own bookkeeping** -- span table, mark bitmap, free runs, class cursors, sweep cursor -- survive a real Factorio save and reload, and a save taken in EITHER phase is resumed rather than restarted. `GC_SAVE_TICKS_RS` is `30 239` and `CHECK_TICK_RS` is `300`.

**Both reasons this stayed open were wrong, and the second is the useful mistake.** The note said the phase cadence differs between `scripts/run-guest.sh` and this harness -- the standalone run putting the sweep window at 63-64 while "a run here shows phase changes at 237 and 242". One instrumented `--start-server` run to tick 460 logs every phase line, and they are one trace:

```
phase 0   -> 1 cycles=0      cycle 0: mark 0-62,   sweep 63-64
phase 63  -> 2 cycles=0
phase 65  -> 1 cycles=1      cycle 1: mark 65-236, sweep 237-241
phase 237 -> 2 cycles=1
phase 242 -> 1 cycles=2      cycle 2: still marking at tick 460
```

63 is the FIRST collection's sweep and 237 is the SECOND's. Nothing about `--start-server` moves a tick, and there was never a per-harness cadence to measure. **Two windows of one trace were read as two traces** -- and both readings were of a partial log, the standalone run being too short to reach the second collection and the roundtrip log that produced 237/242 having lost its opening lines.

**What actually kept the leg opt-in was `CHECK_TICK_RS=200`, and the symptom named a different constant.** `cycles` does not reach 2 until tick 242, so at 200 both arms tripped the "only 1 collection ran" gate -- which `continue`s before the phase is looked at, so nothing was ever added to `phases_seen` and the run ended on "no save landed mid-sweep", pointing at `GC_SAVE_TICKS_RS`. *A gate whose failure message names the last assertion rather than the first will point at the wrong constant*, and a save tick is exactly the kind of constant somebody then tunes until it passes.

**And a Rust collection gets LONGER every cycle, so one cycle's numbers do not generalise to the next.** 65 ticks, then 177, then over 218 -- the live set grows (`live=0`, then 19,008, then 47,984) and the mark is charged for it. The "~65 ticks against the Go guest's ~47" figure above is the FIRST cycle only, and the old `CHECK_TICK_RS` comment reasoned from it that 200 ticks was 1.8 cycles when it is 1.0. The cause of the length is measured and is not this guest's: a Rust guest's statics are larger, the root re-scan is charged against the step budget, and at this guest's deliberate 512 a step spends a few hundred granules on roots before it does anything else -- see "What it costs" above.
