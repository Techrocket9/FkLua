# Benchmarks — what each one measures, and what they say

There are now **seven** harnesses, and they answer different questions. Quoting a number from the wrong one is the main hazard this file exists to prevent.

| command | what it runs | what it can say |
|---|---|---|
| `fklua bench` | `bench/kernels/` — hand-written Lua modelling emitter output, against idiomatic Lua | a **ceiling**. Nothing here moves when the compiler does |
| `fklua bench --opt` | `bench/wasm/` — hand-written `.wat` compiled by the real compiler at each `-opt` level | whether a **pass** paid for itself |
| `scripts/bench-guests.sh` | `bench/guests/` — real TinyGo and real rustc output, against idiomatic Lua | what a **mod author actually gets** |
| `scripts/run-gcbench.sh` | `guest/go/examples/gcbench` in **real Factorio**, both collector arms | what a COLLECTION costs a tick, **per tick** |
| `scripts/run-growbench.sh` | `guest/go/examples/growbench` in **real Factorio**, three heap targets x both growth laws | what a **`memory.grow`** costs a tick, on the ticks the guest says it grew |
| `scripts/run-growprobe.sh` | a **bare Lua mod**, no wasm, holding `mem_grow`'s fill loop | what it costs Factorio's Lua to CREATE a table slot |
| `scripts/run-gctail.sh` | a **bare Lua mod**, no wasm, holding a configurable vector of live tables | what **Lua's OWN** collector costs a tick, over a LONG run, against heap size and table count separately |

**`run-growbench.sh` reports the GROW ticks separately from every other tick**, because a worst-tick number cannot say what it was the worst at. The guest logs the game tick of every `memory.grow` and the script recovers the offset between that and Factorio's `t<N>` rows from the guest's own first line, refusing to report a correlation rather than guessing one.

**`run-gctail.sh` exists because `run-gcbench.sh` cannot hold anything still.** Through a real collected guest the heap size, the number of shard tables and the guest's own allocation rate all move together, so "does the tail track BYTES or TABLE COUNT" is not a question that harness can be asked. In a bare Lua mod they are three numbers in a config file — which is how `agents/guests.md`'s two unexplained 24,000-tick rows got a mechanism. It also defaults to **24,000 ticks**, because the 1,200-tick runs never saw the tail and a rare event is not disproved by a short run.

**`run-growprobe.sh` is a bare Lua mod for the same reason `run-shardprobe.sh` is**: the quantity is a property of Factorio's table internals, `bin/lua52f` cannot see it in either direction, and a mod with no wasm, no emitter, no `--persist` and no collector in it is what makes the answer attributable to the representation rather than to FkLua.

The in-game ones are the only instruments here that measure inside the game, and the first of them was rebuilt at GC stage D. Two things about it generalise to any future in-game measurement and are the reason it is in this table:

- **`--benchmark`'s `avg/min/max` is PER RUN, and every run begins by loading the save.** For a guest holding a 2 MiB heap that first tick is 211 ms, which swamps anything a guest does. `max` is not a worst tick; it is the load. `--benchmark-verbose <counters>` reports one CSV row per TICK instead, so the load becomes a row to drop and what is left is a distribution.
- **Read the header, never count the columns.** Factorio emits the counters in ITS OWN canonical order and not the order the command line asked for. A positional parser silently relabels them the moment a counter is added — asking for `wholeUpdate,scriptUpdate,luaGarbageIncremental` returns `tick,wholeUpdate,luaGarbageIncremental,scriptUpdate`.

Both were established downstream first, in the first real mod's `bench/run.sh`, and ported back. See [`gc.md`](gc.md), "Stage D, as built".

---

## A wall-clock assertion needs a floor measured in the same run

This applies to Go tests, not only to the harnesses above, and it is here because the repo learned it the expensive way: `TestAHostCallIsUnderM0sTwoMicrosecondGate` carried a bare 2000 ns threshold and failed at 2005–2156 ns whenever tinygo builds ran beside it, on clean `master`, with nothing about the ABI changed.

The rules are the ones every table in this file already follows, written down for the Go side:

- **Measure a FLOOR in the same run** — something that does none of the work under test but runs on the same interpreter, so it prices the machine right now.
- **Make the floor run for the same WALL TIME, not the same iteration count.** A floor that finishes in 13 ms while the measurement it qualifies spends 200 ms fighting for a core reports a 1.13× dilation for a machine that dilated 2.5×. Measured, and it is why the first version of that fix did nothing.
- **Take an A/A** — the floor twice, bracketing the measurements rather than sitting beside them — and say the spread out loud.
- **Prefer a RATIO to a wall-clock number wherever the question allows one.** Both terms dilate together, so the answer is the same on a busy machine.
- **A noisy run must not SKIP.** Report it loudly and keep the load-invariant assertions running; a timing test that quietly declines to assert is the silent-skip shape in a different costume.

Three more, added when the same bug turned up a SECOND time — in `internal/guest.TestWhatAHostCallCostsThroughARealGuest`, which had no wall-clock threshold at all and was flaky anyway. Full record: [`gc.md`](gc.md), "The SECOND instance".

- **Size a leg by TIME, not by iterations, and size the floor the same way.** This is the same-wall-time rule stated positively, and it is what makes it hold for a suite whose legs differ by orders of magnitude. That test's legs span 400 ns to 300 µs; at a shared 2,000 iterations the cheap ones were almost entirely process startup and the expensive ones almost none — and then they were compared to each other. Watch the ceiling on any rep count: a cap that stops the cheapest leg reaching the target reproduces the exact bug the same-wall-time rule exists to prevent, and did.
- **An ORDERING assertion is a wall-clock assertion in disguise.** "A step that does more cannot cost less" is true about work and false about MEASUREMENTS of work, and it needs the same floor everything else does. Two legs that legitimately do the same work invert about half the time, forever, and the test reads as flaky rather than as wrong. The word it needs is *measurably*, and the A/A is what defines it.
- **Verify a timing fix with `-count=1`.** `go test` caches by default, so a "verified under load" run can print the quiet run's numbers verbatim and say `ok … (cached)`. Identical numbers across genuinely different conditions is a stale input, not a result — the same trap as a cached `.wasm`, which cost this repo four wrong conclusions in a row.

---

## The headline: what a mod author gets

`scripts/bench-guests.sh`, `-opt=3`, best of 5, process startup and setup subtracted. Ratios are against hand-written Lua, so **below 1.00× means FkLua wins**.

| kernel | Lua | TinyGo | Rust | Go/Lua | Rust/Lua |
|---|---|---|---|---|---|
| **pure** | | | | | |
| `pure_sum` — u32 array reduction | 22.3 ms | 41.9 | 38.5 | **1.88×** | **1.73×** |
| `pure_prng` — xorshift32, no memory | 237.1 | 160.1 | 160.1 | **0.68×** | **0.67×** |
| `pure_dot` — f64 dot product | 19.3 | 164.1 | 163.8 | **8.49×** | **8.48×** |
| **realistic** | | | | | |
| `real_entities` — struct scan + filter | 29.7 | 162.6 | 159.7 | **5.47×** | **5.38×** |
| `real_grid` — flood fill over a 2D grid | 95.5 | 818.8 | 780.7 | **8.57×** | **8.17×** |
| `real_names` — build and hash strings | 268.7 | 1215.4 | 1479.7 | **4.52×** | **5.51×** |

`pure_dot` moved last, 11.44× → **8.50×** (TinyGo) and **8.48×** (Rust), when the loop guard learned multi-base spans, affine bases and 8-byte accesses. Rust's did not move at first, and the cause was the same shape of mistake as the countdown exit test: rustc indexes both arrays off the LOOP COUNTER, and the guard refused any base derived from the counter on the theory that it meant two strides. It does not — the base's stride simply is the counter's. **Twice now a toolchain looked immune to a pass and was not.**

**The whole realistic half moved at M12**, and it was the inlined byte LOADS that did it — `real_entities` 7.23× → 5.47×, `real_grid` 9.73× → 8.57×, `real_names` 5.17× → 4.52×, with `pure_sum` and `pure_prng` unchanged as the control since neither contains a byte load. See [`agents/optimizer.md`](optimizer.md); the short version is that the reason sub-word accesses stayed function calls was always a fact about STORES, and nobody had separately checked whether it applied to loads. It does not.

`pure_dot` moved last at M12 — 12.57× → **11.51×** — when `ldexp` became a table read (`PE`, see [`agents/codegen.md`](codegen.md)). The remaining 0.777× that a loop guard over 8-byte accesses would buy is measured and unbuilt; see [`agents/optimizer.md`](optimizer.md).

**One row moved at M12 and the rest are the control.** `pure_sum` went 95.5 → 41.6 ms for TinyGo (4.41× → **1.88×**) and 90.0 → 38.3 ms for Rust (4.15× → **1.73×**), which is the loop guard hoisting the bounds check and the alignment test out of the hot loop and then replacing the address arithmetic with a word index ([`agents/optimizer.md`](optimizer.md)). Independent A/Bs of the pass alone measured 0.424× and 0.417× on the same kernel. Every other cell is inside run-to-run noise, which is what says the win is where it claims to be.

**A compiled guest is now within 2× of hand-written Lua on an array reduction.** That does not overturn the conclusion below — Lua still wins everything but `prng` — but it moves `pure_sum` from "an order of magnitude of headroom" to "nearly parity", and it does so on the kernel most like a mod scanning an array.

**Rust now beats TinyGo on `pure_sum`, and the reason is worth knowing.** It did not move at first: the guard recognised the `i32.ne` and `i32.lt_u` exit tests TinyGo emits, and rustc closes an unrolled loop with a bare `local.tee` used directly as the branch condition — no comparison step anywhere — counting DOWN to zero. Once that third shape is recognised, Rust benefits *more*, because its loop is 8×-unrolled where TinyGo's is 4×, so one entry test covers eight accesses instead of four. The pass is not language-specific; which loops it reaches is very much toolchain-specific, and a shape that looks like an edge case in one toolchain's output can be the only shape another produces.

Every one of these moved at the M11 perf pass, and the **previous** figures were 5.80/0.67/17.46/7.58/13.36/9.49 for Go. Two independent instruments agree on the new ones: this harness and `scratch_tmp`'s paired A/A harness, which measured 4.53, 0.68, 12.78, 7.15, 9.76, 5.19 for the same builds. Where the win came from is split roughly evenly between the `-opt=2` guest flag and the emitter's inlined memory paths — see [`agents/optimizer.md`](optimizer.md) and [`agents/guests.md`](guests.md).

**Read this honestly: hand-written Lua is still faster than a compiled guest at everything except `prng`.** The gap narrowed a lot — `real_names` went from 9.49× to 5.15× and `pure_dot` from 17.46× to 12.58× — but narrowing an order of magnitude to a half-order does not change the conclusion. The one clean win remains `prng`, where FkLua beats idiomatic Lua by 1.5× because `bit32.bxor` is a function call per operation and the emitter lowers a shift to a multiply and a floor.

That is not an argument against the project, but it does mean **speed is not the argument for it**. What FkLua buys is writing a mod in Go or Rust — types, tests, a package ecosystem, and a compiler that catches what Lua finds at runtime — at a cost that ranges from a win to roughly an order of magnitude. For a mod whose hot path is the Factorio API rather than its own arithmetic, that cost lands on a small fraction of the frame.

### Rust no longer beats TinyGo, and the old claim was measuring a flag

This file used to say Rust won everywhere by 1.05×–1.46× and attribute it to "generated-code quality, not allocator strategy". **That was comparing rustc `-O3` against TinyGo `-opt=z`.** `scripts/bench-guests.sh` passed no `-opt` to TinyGo, so it got the size-optimising default, while the Rust guest got `opt-level=3` + LTO + `wasm-opt -O3` in the same script.

With both at a comparable optimisation level the two are within noise of each other on four kernels, Rust still wins `real_grid` (9.25× vs 9.77×), and **TinyGo now wins `real_names` outright** (5.15× vs 6.13×) — an inversion of the previous headline. Both guests still run without a garbage collector, so allocator strategy was never the explanation and neither, it turns out, was codegen quality.

---

## The M0 kernels are an idealisation, and the gap is 2×

This is the finding most likely to mislead someone, because `fklua bench` reports the friendlier number and it has been quoted since M0.

For `sum`, on the same machine:

| | ns/op | ratio to hand Lua |
|---|---|---|
| `bench/kernels/` `gen` — Lua modelling the emitter | 25.37 | 2.89× |
| **real TinyGo through FkLua** | **~49** | **5.80×** |

The baselines agree — M0's `nat` is 8.79 ns/op and the guest benchmark's Lua is 8.5 — so the entire gap is on the generated side. `bench/kernels/sum.lua`'s `gen` variant is what the emitter would produce for an *ideal* input:

```lua
if l1 >= n then goto L1 end
s0 = MEM[p * 0.25 + 1]          -- no bounds check, no alignment test
l0 = (l0 + s0) % 4294967296.0
p  = (p + 4) % 4294967296.0
l1 = (l1 + 1) % 4294967296.0
```

Real TinyGo output for the same Go loop carries four things that model does not:

```lua
::L4::
if v0 == 0 then goto L6 end                      -- the index counter
if v5 == 0 then goto L0 end                      -- AND the slice length, separately
v0 = v0 - 1
v5 = v5 - 1
t0 = v4
if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end -- the bounds check
if t0 % 4 == 0 then v10 = MEM[t0 / 4 + 1] else v10 = ld32(MEM, MEMSIZE, t0) end
v3 = (v10 + v3) % 4294967296.0
v4 = (v4 + 4) % 4294967296.0                     -- a wrap on a pointer bump
goto L4
```

Two of those are TinyGo's (the duplicate counter, from `range` over a slice keeping both an index and a remaining-length) and two are ours (the bounds check, the alignment test). **The M0 ceiling is real as a ceiling and should not be quoted as a prediction.**

### Two optimisations this makes visible

Both are unbuilt; recorded here because the loop above is where they were spotted.

- **Alignment analysis.** `v4` starts at a `[]uint32`'s data pointer and advances by 4, so `t0 % 4 == 0` is provably true and the branch is dead. A pass proving 4-alignment could emit `MEM[t0 / 4 + 1]` directly — removing a modulo, a compare and a branch from the hottest operation in any program. This is a bigger prize than the bounds check, which [`agents/optimizer.md`](optimizer.md) measures at only 20% of a load.
- **Wrap elimination on pointer bumps.** `v4 = (v4 + 4) % 4294967296.0` cannot wrap: `v4` is bounded by `MEMSIZE`. M5a's fixpoint proves this for loop *counters* but not for pointers, which is why it removed only 27 of 467 wraps on the M4 guest.

---

## Where the time goes, by kernel

- **`pure_sum` 2.84×** is the one kernel where the compiler now does most of what the ceiling model predicted. What closed it was not loop overhead but the per-access bounds check and alignment test, hoisted behind a single entry guard — on this loop that is worth 1.57×, against 1.05× for replacing the loop construct itself. **Per-access memory overhead outweighs loop overhead by about seven to one here**, which the M0 kernels do not show because they have no bounds check to begin with.
- **`pure_dot` 17.46×** is the f64-in-linear-memory problem, unchanged in kind since M0. Every element reassembles a double from two words. The M10 fix to `ld_f64` took 1.48× off it; the rest needs an f64-typed shadow of linear memory, still an open question in `CLAUDE.md`.
- **`real_grid` 13.36×** is byte stores. `seen[n] = 1` in a word-table memory is a read-modify-write of a whole word plus a page mark, against a Lua table store that is one hash write.

  This used to add "its `cur % side` and `cur / side` on a non-power-of-two are helper calls too", which was **two claims and both are wrong** — read from the Go source rather than from the guest's actual wasm. Disassembling `bench/guests/go` says: (1) there is only **one** division in that loop, not two, because LLVM computes the remainder as `cur - (cur/side)*side`, so the multiply and the subtract are already native; and (2) `side` is a **parameter of an exported function**, so it can never be constant-folded — the constant-divisor lowering does not apply and `real_grid` is not one of its targets. The division that remains is `i32.div_s` with a runtime divisor, wrapped in the two `select`s LLVM emits to dodge the −1 and INT_MIN cases, and it stays a helper call.

  What the same disassembly *did* find is worth more than the claim it replaced: **LLVM does not strength-reduce constant integer division on wasm at all.** Six of the eight division sites in that guest have a literal `i32.const` divisor, the hot one being `i32.div_u` by 10 in `real_names`' digit loop. If a measurement of the constant-divisor lowering is wanted against a real guest, `real_names` is the kernel to look at — not `real_grid`.
- **`real_names` 9.49×** is the kernel a compiled guest is *supposed* to lose, and it is in the suite for that reason. Lua strings are C-implemented and interned; `"iron-plate-" .. i` is a memcpy the interpreter does for free, where a guest assembles bytes in linear memory through its own allocator. A suite containing only the kernels FkLua wins would not be telling anyone the truth about what to write in what.
- **`real_entities` 7.58×** is the closest thing to a typical mod inner loop, and the number to quote if only one is quoted.

---

## Three bugs the checksum comparison caught

The harness refuses to report a timing unless all three languages return the same checksum. That is not ceremony — it caught every one of these:

- **A 32-bit hash written naively in Lua is silently wrong.** `h = (h *
  16777619) % 4294967296` overflows: `h` reaches 2³², and 2³² × 16777619 is 7.2e16 against a double's 9.0e15 exact-integer ceiling. It produces a plausible number and the wrong one. `bench/guests/lua/kernels.lua` splits the multiply into halves. **A guest never has this problem** — wasm says the multiply is 32-bit and the emitter lowers it accordingly, so the same source in Go or Rust is correct without anyone thinking about it. That asymmetry is a real part of the answer to "should I write this in Lua", and it is not a speed number.
- **The flood fill filled nothing.** It started at the grid centre, which is a wall on this maze, so all three languages agreed on a checksum of zero — agreement about doing no work. It now scans forward to the first open cell.
- **The Rust guest was handicapped by its own heap.** Bump-allocating out of a 24 MiB static array made the module declare 26 MB of linear memory, and a guest's memory is a Lua **table** — 6,569,984 entries against TinyGo's 32,768. The allocator now grows memory on demand the way TinyGo's does, taking it to 1,114,112. Worth knowing: fixing it moved the timings by under 2%, so table size was not the driver it looked like.

---

## Running it

```sh
./scripts/bench-guests.sh          # needs tinygo, cargo, wasm-opt, bin/lua52f
OPT=2 ./scripts/bench-guests.sh    # compare against another level
```

The kernels are three mirrors of each other — `bench/guests/go/main.go`, `bench/guests/rust/src/lib.rs`, `bench/guests/lua/kernels.lua`. Adding one means adding it to all three and to `KERNELS` in the script. **The Lua version is deliberately idiomatic**, not shaped to resemble emitter output: plain tables, numeric `for`, string concatenation, and wrapping arithmetic only where the checksum depends on it. Making it model a guest would flatter FkLua by slowing down the thing it is measured against.
