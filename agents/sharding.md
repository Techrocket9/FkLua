# Sharded linear memory — stage A: design and measurement

**Read this before proposing anything about the 4 MiB wall, and before quoting any number about the cost of a large Lua table.**

FkLua's linear memory is one flat Lua table of 32-bit words. Above 2²⁰ = 1,048,576 keys a Factorio Lua table stops behaving like an array **for all of its keys**, so 4 MiB of guest memory is a cliff: every load and store gets ~20–40× slower, permanently, and every LOAD pays seconds rebuilding the table. `agents/gc.md`, "The 4 MiB wall", established that and shipped a notice.

This stage answers whether the representation change is worth making, what it should be, and what it costs. **The verdict is GO**, the design is not any of the three candidates the milestone opened with, and the attribution question it was also meant to settle came back **negative** — which is the more important of the two results.

> **STAGE B HAS SHIPPED. §13, "Stage B as built", is the measurement; sections 1–12 are the design and the forecast that preceded it.** Where the two disagree, §13 wins. The headline: **1.007× below the wall and 17.8× above it, in game, through the real emitter**, and the `gcbench` collection that cost 5,299 ms above 4 MiB now costs 447 — the same 69.7 ms/MiB the below-wall arm always paid. There is no 4 MiB wall any more, and the notice that reported it is deleted.

Everything here is Factorio 2.0.77 on an M3 Pro. Nothing here was measured under `bin/lua52f`, except where the oracle is deliberately one arm of a comparison.

---

## The rule this document exists under

> **`bin/lua52f` CANNOT SEE THE WALL. Every wall-related number must come from real Factorio, and the oracle is only valid below 4 MiB.**

The oracle is stock Lua 5.2.1, whose array part grows to 2³⁰. It prices the crossing grow at 3.0 ms against 1.3 — a 2.3× slope where the game has a 27× cliff. `CLAUDE.md`'s heap-budget row rejected sharding on a **host-side** measurement that could see the cost and could not see the benefit, and that mistake is the reason this milestone exists. Do not repeat it.

Two instruments, both new here, both in the game:

- **`scripts/run-shardprobe.sh`** — a bare Lua mod (`testdata/shardprobe/`), no wasm, no emitter, no `--persist`, no collector. Each arm is the **actual emitted `-opt=3` access shape**, copied out of `emitInlineStore32` / `emitInlineLoad32` / `loopguard.go`, with the bounds check, the alignment test and the `MEMDIRTY` test all present — because the shard arithmetic's cost only means something against a baseline carrying everything else the real access carries. Timing goes through `helpers.create_profiler()` and the log, because that is the only clock in the sandbox.
- **`scripts/run-shard-e2e.sh`** — one packaged guest, three representations, through the real emitter, the real `fk_rt.lua` and the real `fk_mod.lua`. `scripts/shard-e2e-edit.py` produces the sharded arms by exact string replacement and **fails if a replacement does not match exactly once**, so an emitter change breaks it loudly rather than quietly measuring three copies of the same file. Prototyping by hand before building the pass is how the counted loop, the loop guard and the dirty-page set were each predicted first.

**Run-to-run spread is large and it is stated everywhere it matters.** The same flat load at the same size in the same game came out at 56.1, 60.9, 62.0 and 73.4 ns across four runs — up to 31% apart. That does not matter against a 33× cliff and it matters entirely against a 1.4× regression, so the one number the whole decision hangs on is measured **paired**: flat, sharded, flat, sharded, seven times in one run at one size, reporting the median ratio and its range.

---

## 1. Where the wall is, measured to 4,096 words

`agents/gc.md` states the law as "more than 2²⁰ keys" from a 1,000,000 vs 1,100,000 pair. The probe brackets it far tighter — the **same** 400,000 sequential accesses at every size, so only the table's size changes:

| words | MiB | load, flat | store, flat | load, sharded | load, sharded + guard |
|---|---|--:|--:|--:|--:|
| 262,144 | 1.0 | 50.0 ns | 53.2 | 84.6 | 24.3 |
| 524,288 | 2.0 | 60.9 | 65.8 | 91.0 | 38.5 |
| 786,432 | 3.0 | 105.4 | 99.9 | 99.2 | 43.0 |
| 1,022,976 | 3.9 | 138.7 | 142.5 | 92.4 | 33.4 |
| **1,048,576** | **4.000** | **108.2** | **116.5** | 88.5 | 31.4 |
| **1,052,672** | **4.016** | **3,819.6** | **4,252.4** | 91.6 | 35.9 |
| 1,310,720 | 5.0 | 3,333.7 | 3,752.1 | 95.4 | 38.4 |

**35× on a load and 36× on a store, for 4,096 more words.** 2²⁰ exactly is still an array; 2²⁰ + 4,096 is not. The floor — the identical loop with the table read deleted — is 23.7–25.1 ns at every size, so the loop overhead is not moving and what the table costs is the whole difference.

Two things follow that the existing record does not say.

**The flat form also degrades continuously BELOW the wall.** 50.0 → 138.7 ns from 1 MiB to 3.9 MiB, a 2.8× slope with no cliff in it, on the same 400,000 low indices. The sharded form is **flat at 85–99 ns across the entire range, 1 MiB to 8 MiB**, and the guard-hoisted form is flat at 24–43 ns. The crossover where always-sharding stops costing and starts paying is at about **3 MiB, a megabyte below the wall.**

**The degraded cost depends on the access pattern in a way this probe does not explain.** An 8 KiB-stride walk of the whole memory at 8 MiB costs 1,259–1,512 ns flat, against 3,565–4,541 ns for a sequential walk of the low 1.6 MB of the same table — strided is reproducibly *cheaper*. Both are far above anything sharded (90–333 ns for every pattern measured at every size). Recorded as observed; the mechanism is not established here and no conclusion rests on it.

---

## 2. Attribution — stage D's open item 1, and the recorded lead is WRONG

Stage D left this open: *a stop-the-world collection is **88 ms per MiB** in game against the **13.9–32.8** stage B derived under `bin/lua52f` — 2.7×–6.4×, unexplained*, with the recorded lead being "`gcbench`'s heap was past 2²⁰ words, where the table stops being an array". The prescribed test was to re-measure a collection **either side of the wall**.

`examples/gcbench` now carries `nlive` as a build tag (`size_small.go` / `size_big.go`) and `run-gcbench.sh` takes `ARMS="stw stwbig"`, so that test is a command. `fk_mod.lua`'s own wall notice is the discriminator and `run_arm` reports it. 1,000 ticks × 2 runs, `-opt=3`, `--persist=table`, one guest, one allocator, one trigger, stop-the-world in both:

| arm | `nlive` | fkgc heap | linear memory | wall notice | **worst tick** | ms/MiB | load tick |
|---|--:|--:|---|---|--:|--:|--:|
| `stw` | 44,000 | 2,846,720 B (2.715 MiB) | **under 4 MiB** | silent, at create and at run | **229.47 ms** | **84.5** | 212.16 ms |
| `stwbig` | 110,000 | 6,717,440 B (6.406 MiB) | **6.7 MiB** | fires | **8,580.67 ms** | **1,339.5** | 7,032.00 ms |

**The wall taxes a stop-the-world collection 15.9×.** That half of the hypothesis is confirmed and it is large: a collector above the wall is not usable at any budget.

**And the gap does NOT collapse below the wall, so the lead is falsified.** The `stw` arm reproduces stage D's figure — 84.5 ms/MiB against the 88 it recorded — with a word table that is demonstrably still an array. The wall was never the explanation, and the arithmetic that pointed at it was available all along: stage D's own table records that arm's heap as 2,846,720 B, which is 711,680 words, 0.68 of 2²⁰.

**What the gap actually is: Factorio's Lua interpreter is slower than the oracle's on the identical loop.** The probe carries `ld_ctl` — `ld_flat` with the table read deleted and nothing else changed — precisely so the pair can be run under `bin/lua52f` too. On a 524,288-word table, below any wall, the same two loops:

| | Factorio 2.0.77 | `bin/lua52f` | ratio |
|---|--:|--:|--:|
| the loop with the table read deleted | 24.0–25.2 ns | **23.0 ns** | **1.04–1.10×** |
| the whole emitted access | 56.1–73.4 ns | **31.0 ns** | **1.8–2.4×** |
| therefore the table read itself | 32–48 ns | **8.0 ns** | **4.0–6.0×** |

(`testdata/shardprobe/oracle-access.lua`, best of five, 10,000,000 iterations per leg; `run-shardprobe.sh` runs it as its last step. The oracle leg is extremely stable — 0.230 s and 0.310–0.320 s across five runs each.)

**The loop machinery is the same speed in both interpreters and the TABLE READ is 4–6× slower in Factorio.** That covers the 2.7×–6.4× band on its own, it is measured on a table with no wall anywhere near it, and it is the first time this repo has measured the constant at all.

**The collection-cost model is therefore: an in-game collection costs the host-side band × ~2.5 for the interpreter, below the wall, and × ~16 more above it.** Sharding fixes the second factor completely and the first factor not at all — which is the honest statement and the one that should replace the open item.

> **Consequence for every published host-side number.** Stage C's 555× and stage B's ms/MiB band are not contradicted; they are host-side, and the constant that carries them into the game is ~2.5 and is now measured rather than open. `agents/gc.md`'s "nothing derived from stage B's band should be quoted as an in-game number" can be replaced with a conversion.

---

## 3. The candidates, measured

Below the wall, **paired**, seven repetitions in one run, median ratio against the flat form measured beside it (`1.00×` = today's cost):

| | 1 MiB | 2 MiB |
|---|--:|--:|
| load, runtime shard select | 1.59× (1.54–1.71) | 1.49× (1.45–1.54) |
| store, runtime shard select | 1.53× (1.46–1.57) | 1.46× (1.11–1.51) |
| **load, shard-0 fast path** | **0.93× (0.89–0.99)** | **0.97× (0.93–1.26)** |
| **store, shard-0 fast path** | **1.00× (0.93–1.04)** | **1.01× (0.82–1.06)** |
| **load, guard-hoisted** | **0.99× (0.93–1.05)** | **1.02× (0.92–1.06)** |

Single-shot, at four sizes, ratio against flat at the same size:

| shape | 2 MiB | 3.5 MiB | 5 MiB | 8 MiB |
|---|--:|--:|--:|--:|
| shard select, arithmetic | 1.49–1.74× | 0.87–0.89× | 0.03× | 0.02–0.03× |
| shard select, `bit32` | 2.42–2.90× | 1.29–1.33× | 0.04× | 0.03–0.04× |
| shard by base-address hash | 1.52–1.65× | 0.79–0.83× | 0.02–0.03× | 0.02× |
| guard-hoisted, shard in a local | 0.64–0.67× | 0.34–0.35× | 0.01× | 0.01× |
| mode BRANCH, flat arm | 1.12–1.16× | 1.03× | — | — |
| upvalue FUNCTION swap, flat arm | 1.16–1.20× | 1.11–1.34× | — | — |

Bulk, total ms:

| | 2 MiB | 3.5 MiB | 5 MiB | 8 MiB |
|---|--:|--:|--:|--:|
| build from empty, flat — **this is the per-LOAD cost** | 53.7–67.4 | 113.4–163.3 | **2,630–4,069** | **4,882–6,003** |
| build from empty, 2 MiB shards | 54.7–67.0 | 100.6–121.0 | **134–176** | **243–287** |
| `mem_fill` 1 MiB, flat | 9.5–13.6 | 4.8–5.5 | **699–1,184** | **1,630–2,096** |
| `mem_fill` 1 MiB, split at shard boundaries | 9.4–12.5 | 6.9–8.7 | **8.7–11.5** | **8.7–13.4** |

Four things decide the design.

1. **`bit32` loses to arithmetic**, by 1.5×. `CLAUDE.md`'s "prefer arithmetic" rule holds here; a `bit32.rshift`/`band` pair is two C calls where `t0 % 2097152` and a subtraction are two opcodes.
2. **A base-address hash lookup is marginally cheaper than the divmod** (0.79× vs 0.87× at 3.5 MiB) and is refused anyway: it needs a second table keyed by shard base, kept in step with the shard vector, in `storage`, forever, to buy ~5% on a path the design is about to stop taking.
3. **The guard-hoisted form is free below the wall and size-independent above it** — 0.99×/1.02× paired, and flat at 24–43 ns from 1 MiB to 8 MiB where today's guarded form goes from 26.8 ns to 3,886 ns.
4. **The runtime shard select costs 1.46–1.59× below the wall**, and the corpus census (§4) says it would be the form **68–77%** of accesses take. That is what kills always-sharding as the milestone specified it.

---

## 4. The corpus census — the design's central hope is FALSE

The milestone's candidate (a) assumed *"the existing range analysis proves the address under the first shard's bound"* for a useful fraction of accesses. Measured over 9 TinyGo guests, 4 Rust guests and the downstream mod's `bbb.wasm` (read-only), at `-opt=3`, by a harness that reconstructs `emitFunc`'s exact setup and reproduces `agents/optimizer.md`'s published guard census guest for guest:

| class | this repo's 13 guests | +`bbb.wasm` | weighted (repo) | weighted (+bbb) |
|---|--:|--:|--:|--:|
| **A** proven under one shard | 962 (**15.5%**) | 2,743 (**26.2%**) | 11.7% | 17.8% |
| **B** in a guard whose span can be proven within one shard | 461 (**7.4%**) | 608 (**5.8%**) | 15.4% | 4.3% |
| **C** neither — runtime shard selection | 4,765 (**77.0%**) | 7,133 (**68.0%**) | **72.9%** | **77.8%** |
| total | 6,188 | 10,484 | 84,803 | 591,038 |

The 13-guest column is what `go test ./internal/luagen -run TestShardCensus -v` prints here; the second adds the downstream mod via `FKLUA_SHARD_EXTRA=…/bbb.wasm`, which is not in this repo. TinyGo and Rust split sharply — class C is 66.6% for TinyGo and **88.1% for Rust** — and `agents/optimizer.md` twice records a pass that looked toolchain-specific and was not, so that gap deserves a second look before any coverage number is quoted as "the corpus".

**Class A is constants, not range analysis.** Of 962 sites, **952 are literal constant addresses and 10 come from the range analysis** — 0.16% of the corpus (19 of 2,743 with `bbb`). And only 1,295 of 6,188 addresses carry any bound narrower than the full u32 at all, so 79% of the corpus has *nothing* known about its address rather than a bound that wants tightening. A and B do not overlap (measured: 0 sites), so the two mechanisms are additive.

Two consequences fold straight into the design:

- **A constant address does not need shard 0; it needs no runtime selection for ANY shard.** The emitter folds the shard index statically and emits `S[3][513]`. Tying class A to "under 2 MiB" gives up coverage for nothing. (Measured: 0 constant addresses exceed shard 0 in any corpus module today, but rustc's static image is already 1,053,496 bytes — half the line.)
- **Weighting toward loops makes A and B *worse*, not better.** Whatever the default form is, it is what the hot code takes.

The census also found the one extension that would move the needle and why it is fragile: **1,704 class-C accesses are `$fp + const` off LLVM's shadow-stack prologue** (1,860 such accesses in all; 12.2% of C's loop weight), and a module-scoped range for the stack-pointer global would flatten every one. Measured SP init is 65,536 for every TinyGo module including `bbb` and 1,048,576 for every Rust one — all under 2 MiB. But it rests on the same whole-program stack-pointer convention `analysis.Frames` already documents, so it is an `-opt=2`-and-up fact; and **it dies exactly for the guest sharding exists to serve**, because a guest whose static image passes 2 MiB puts SP init above the line and takes all 1,704 back at once. rustc's static image is already 1,053,496 bytes — half the line.

A second class-C route was tried and abandoned, recorded so nobody repeats it: a forward taint attributing class-C addresses to their roots collapses, because 99.5% of them are reachable from a load somewhere and every bucket ends up containing `load`. The `$fp + const` scan replaced it and is precise.

---

## 5. The design — the bounds check IS the shard test

> **Linear memory is ALWAYS a vector of 2¹⁹-word shards. There is no mode flag, no runtime transition and no compile-time gate. Every emitted access opens with a test it already had, and below 2 MiB that test is the bounds check unchanged.**

Every `-opt=3` access today is two `if`s:

```lua
t0 = <address>
if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
if t0 % 4 == 0 then v = MEM[t0 / 4 + 1] else v = ld32(MEM, MEMSIZE, t0) end
```

Under sharding it is **one**:

```lua
t0 = <address>
if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then v = S1[t0 / 4 + 1] else
  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
  t2 = t0 % 2097152
  v = MEM[(t0 - t2) / 2097152 + 1][t2 / 4 + 1]
end
```

`MEM` is the shard vector, `S1` is shard 0 bound to a chunk-local — exactly the status `MEM` has today — and `SHBOUND` is `min(MEMSIZE, 2097152)`.

The whole argument is one line: **below 2 MiB, `SHBOUND` IS `MEMSIZE`, so `t0 + 4 <= SHBOUND` is the bounds check rather than an addition to it, the fast arm is today's access unchanged, and the else arm is unreachable for any address that is not a trap.** Measured paired: **0.93–0.97× on loads and 1.00–1.01× on stores.** Merging the two `if`s pays for the shard vector; a guest below 2 MiB comes out at or slightly under today's cost.

Above 2 MiB the same test decides shard 0 versus everything else, and the else arm is the divmod — which at 90–100 ns is 35–45× better than the flat form it replaces.

### The three emitted forms

| form | when | cost, below the wall | cost, above |
|---|---|--:|--:|
| **static** `S[k][c]` | the address is a compile-time constant (15.5% of this repo's corpus, 26.2% with the downstream mod) | free | free |
| **guard-hoisted** `ls[lw + k]` | inside a loop guard whose span is proven within one shard (7.4% / 5.8%) | 0.99–1.02× paired | 0.01× |
| **shard-0 fast path** (above) | everything else (77.0% / 68.0%) | 0.93–1.01× paired | 0.02–0.20× |

**No form regresses below the wall.** That is the bar — the barrier-candidate bar of ~1% — and it is met by every form rather than on average.

### The guard extension, and what crossing spans do

`analysis.LoopGuard` already publishes `Bases[]` with `Stride` and `MaxEnd`, and `emitLoopGuard` already evaluates, per base, `e >= 0 and e % 4 == 0 and
<e + t0*stride> + MaxEnd <= MEMSIZE` — so both ends of the span are live Lua
values at loop entry. The extension is **one extra conjunct per base**: the two ends land in the same shard, i.e.

```lua
(e - e % 2097152) == ((<e + t0*stride> + MaxEnd) - (<...> % 2097152))
```

plus one hoisted local per base holding that shard's table, and `wordIndex` becomes a **within-shard** index. No analysis change.

**A span that crosses a boundary falls to the fast path, and that is stage B's answer.** Strip-mining the loop — an outer loop over shard pieces, the guarded body per piece — is the general fix and is what `mem_fill` already does below (measured free), but it requires the loop body to be re-enterable at an arbitrary counter value and it is a stage-C refinement, not a stage-B requirement. The conservative version costs little: a guarded loop's span is almost always far smaller than 2 MiB, so it crosses with probability ≈ span / 2 MiB.

### Shard size: 2¹⁹ words, and the one thing that argues against it

2¹⁹ words = 2 MiB is exactly half the 2²⁰ wall, so a shard can never stop being an array however the memory grows, and 2²¹ bytes makes the shard select two opcodes. It is also what `agents/gc.md` measured.

**But it is not the size that minimises the GC tail.** Lua traverses a table in one indivisible `propagatemark`, so the worst tick is the LARGEST shard: `agents/guests.md` measured 4,096-word shards at **0.095 ms flat against 25.85 at 128 MiB**, and extrapolating the 0.202 ms/MiB slope puts a 2 MiB shard at ~0.4–0.5 ms — flat in heap size, which is the win, but not zero. Smaller shards would cut that and would destroy the guard lever, because a guarded loop over anything larger than a shard crosses. **2 MiB is chosen for the access cost and the guard, and the tail it leaves is a stage-C measurement, not an assumption.**

### What this design refuses

- **Candidate (b), the runtime transition.** Measured, it is *more* expensive than the winner on the fast path it exists to protect — a mode branch is 1.12–1.16× and an upvalue function swap is 1.16–1.20×, against the shard-0 fast path's 0.93–1.01×. On top of that it needs two `storage` shapes and a migration between them, a mid-session conversion tick, two emitted arms at every access site (against an inlined store that already costs +13.7–25.0% in bytes), and a threshold — and the crossover is at ~3 MiB, not at 4, so the threshold would be wrong wherever it was put. It buys nothing and costs all of that.
- **Candidate (c), the compile-time gate on declared `Memory.Max`.** Moot once the below-wall cost is zero, and `Memory.Max` is usually absent anyway.
- **`bit32` for the shard select.** 1.5× worse than arithmetic, measured.
- **A shard table keyed by base address.** ~5% better on a path the design does not take, at the price of a second structure in `storage` that must never drift from the first.
- **Making the shard vector's directory anything but a plain array table.** 4 GiB of wasm address space at 2 MiB per shard is 2,048 entries — three orders of magnitude under 2²⁰, so the directory itself can never hit the wall it exists to avoid.

---

## 6. End to end, on a real mod

`scripts/run-shard-e2e.sh`: one `.wat` guest packaged three times at `-opt=3`, `--persist=none`, through the real emitter and the shipped runtime. Two kernels per tick — 32,768 sequential i32 loads at base 0 (inside one shard), and 4,096 i32 stores at an 8 KiB stride over the **whole** memory (crossing every shard, unhelped by anything). `--benchmark-verbose`, 119 measured ticks after the load tick is dropped.

**6 MiB — above the wall:**

| arm | median tick | mean | worst | load tick | vs flat |
|---|--:|--:|--:|--:|--:|
| flat, as emitted today | 43.833 ms | 44.275 | 50.211 | 45.263 | — |
| every access shard-selected | 4.125 | 4.146 | 6.144 | 5.763 | **10.6×** |
| **shard-0 fast path** | **2.527** | **2.596** | **4.329** | **5.165** | **17.3×** |

`fk_sum` is **6,235,200 in all three arms.** A variant that computes a different answer is not a faster variant, and this one does not.

**The fast path is worth 1.63× over the plain shard select even above the wall**, because loop A's 32,768 loads all live in shard 0 — which is where a guest's statics, its globals and the bottom of its heap live too.

**2 MiB — below the wall**, which is where the design has to cost nothing. 1,596 measured ticks per arm (400 ticks × 4 passes), same guest, same kernels, same checksum:

| arm | median tick | mean | vs flat |
|---|--:|--:|--:|
| flat, as emitted today | 2.498 ms | 2.697 | — |
| every access shard-selected | 3.450 | 3.619 | **1.38×** |
| **shard-0 fast path** | **2.441** | **2.638** | **0.98×** |

`fk_sum` is 19,185,600 in all three arms.

**0.98× end to end, on the medians and on the means**, which agrees with the paired microbenchmark's 0.93–1.01× and with nothing else needing to be true. A first pass of this same comparison at one pass per arm read 1.10×, and the disagreement is the instrument rather than the code: separate Factorio processes, one shot each, no A/A. **Do not quote a single-pass e2e number**; the below-wall effect is smaller than the spread between two runs of the same arm.

The `slow` arm is the control that says the merge is doing the work: identical in every respect except that its shard select is unconditional, and it lands at 1.38× — the same regression the paired microbenchmark measures for that form.

---

## 7. The obligations, each with its answer

**`mem_copy` / `mem_fill` / `fk_wstr` spans that cross a shard boundary.** Split the span at every boundary and run one plain loop per piece — the same shape the flat version already has, once per piece. Measured on a 1 MiB fill deliberately straddling a boundary: **9.4–12.5 ms below the wall against the flat form's 9.5–13.6** (free), and **8.7–13.4 ms above it against 699–2,096** (89–159×). Verified byte-for-byte against the flat form under `bin/lua52f` before it was ever timed, because a fast fill of the wrong words is not a result.

**`MEMPACK`'s page set survives unchanged, and the arithmetic says why.** A page is 4 KiB and a shard is 2 MiB; both are powers of two and the page is smaller, so **a page can never straddle a shard boundary** — there are exactly 512 pages per shard. The set still indexes by byte address, `DPLO`/`DPHI` are still byte bounds, every writer's `mark` call is unchanged, and the two-compare fast path is untouched. Only `pack_page`/`unpack_page` translate: page `p` is shard `p >> 9`, offset `(p & 511) * 1024` words. The dirty-page set is also the collector's write barrier, and that consumer is unaffected for the same reason.

**`--persist=table`: `storage.fk_mem` becomes the shard VECTOR, and the aliasing invariant holds one level down.** `storage.fk_mem[s+1]` **is** shard `s`, so a store lands in the saved structure with no sync step exactly as today. This is strictly simpler than what exists: `sync_memory` currently reassigns `storage.fk_mem = mem` on every grow because `mem_grow` replaces the whole table, and under sharding a grow **appends a shard to a table `storage` already holds**, which needs no reassignment at all. `storage.fk_memsize` still needs its refresh, and the invariant that every mode round-trips a *grown* memory still needs its test.

**`--persist=packed` is unaffected** beyond the page translation above; pages are strings and never aliased the live table.

**`fk_migrate_adopt` hands over a vector instead of a table.** The rodata hazard is unchanged and still documented rather than fixed. The shape change is a build-id concern: a save written by a flat build and loaded by a sharded one must be converted on load, which is the same 26 ms/MiB the rebuild costs anyway.

**`fk.memory.adopt` and `MEMPACK.restore` reassign the whole table** (`MEM = t`, `MEM = t MEMSIZE = n`). Under sharding they replace the **vector**, and **no hoisted per-shard local may survive them.** This is the same failure class as the dead loop-guard seed and the missed page mark: nothing behavioural sees a stale table reference until it does. It wants a text property, not a behavioural test.

---

## 8. Collector × sharding

**The conservative scan gets cheaper, and the shape is already the right one.** The scan walks the heap sequentially, which is exactly the pattern that binds one shard to a local and steps a within-shard index — the guard-hoisted form, measured at 24–43 ns flat across every size against today's 26.8 ns at 1 MiB and 3,886 ns at 8 MiB. Below the wall it is a wash; above it, `stwbig`'s 8,580 ms collection should fall toward the below-wall rate of ~84.5 ms/MiB, i.e. ~540 ms at 6.4 MiB — **a predicted ~16×, and stage B must measure it rather than quote this sentence.**

**The dirty-page set is unchanged.** See §7: pages nest inside shards.

**`gcbench` above the wall is the acceptance vehicle**, and the arm exists now: `ARMS="stw stwbig"` builds `-tags gcstw,gcbig` and `run_arm` reports which side of the wall each arm measured, from `fk_mod.lua`'s own notice.

---

## 9. The fkgc cap — a defect to remove, not a knob to document

`guest/go/fkgc/cap.go` declares `HeapCap = 16 << 20` as a **hard cap**: a guest that grows past it traps, via a bare `unreachable` because wasm-unknown has no stderr. `-tags fkgcheap4` / `fkgcheap64` move it. FkLua must impose no memory cap beyond what Factorio imposes, so this goes.

**The metadata, measured** (read out of three real TinyGo builds as the compile-time constant `MetaBytes()` returns, and cross-checked against the `memory[0] initial=` page count):

| build | metadata |
|---|--:|
| `-tags fkgcheap4` | **58.32 KiB** (doc says ~42 — **wrong by 39%**) |
| default | **163.32 KiB** (doc says ~162 — right) |
| `-tags fkgcheap64` | **583.32 KiB** (doc says ~645 — **wrong by 10.6%**) |

Exactly: `MetaBytes = 23,880 + 8,960 × (HeapCap in MiB)`. The heap-proportional part is `markBits` + `spanClass` + `spanAux` = **0.854% of the heap**; the fixed part is 23.32 KiB, dominated by `gray` (16 KiB), `slotTab` (5.5 KiB) and `dirtyQ` (1 KiB). The 42/645 pair fits an older `gcMeta` and nothing tests it.

**The stated hazard is real and static placement is not what neutralises it.** `agents/gc.md` §6 puts metadata outside the heap so that marking does not dirty the cards the next step re-scans. But `collect.go`'s `drainDirtySpans` already opens with `if p < heapBase>>spanLog { continue }` — the collector's `.bss` writes **already** go through the store funnel, **already** land in the page set and **already** get dropped by one compare each, and `collect.go` says so in a comment. The `.bss` placement buys a comparison against a bound, nothing more. Give metadata spans a dedicated class and the `spanClass` load `rescanSpan` **already performs** answers the same question at the same price.

**The design: split `gcMeta` by growth law.** Keep the 23,880-byte fixed part in `.bss` exactly as it is. Chunk the three heap-proportional arrays — one chunk per 4 MiB slice of heap, 35,840 B rounded to 9 spans — behind a static `metaDir [1024]uint32` directory, and allocate the chunks from the guest's own span allocator under a new class `clsMeta`: heap **memory**, never a heap **object**, never moved, never copied. Addressing is pure shifts (chunk `(addr−heapBase) >> 22`, then the existing arithmetic within), and one directory load is amortised across the span-class lookup and the bitmap update that `markObject` already does together.

**4 KiB of static directory covers the entire 4 GiB wasm32 address space.** A flat table sized for 4 GiB would be 35.02 MiB of `.bss` — ~7 ms of Factorio worst tick before the guest allocates a byte. The static floor drops from 163 KiB to 27.3 KiB plus one chunk; at a *completely full* heap the chunked scheme costs 1–5% more than the static one, and at any partially-used heap far less.

The two rejected alternatives, with the reason each fails: **heap objects** — `markBits` is a contiguous growing array, which is the fragmentation ladder a non-moving collector exists to avoid; **a movable metadata arena** — a move is a pure memcpy with no fixups and is safe anywhere inside `growHeap`, but a movable base means a base load on `allocate`'s four constant-address indexed loads, and `heap.go` records that this allocator only gets under `-gc=leaking`'s allocation cost by carrying *nothing* per allocation.

**The new limit, stated rather than hidden**: `clsMeta` spans are permanent blockers every 4 MiB, so the largest single object drops from 16 MiB to just under 4 MiB (place each chunk at the top of the slice it describes so a run can stretch between chunks). Relocating a blocking chunk — a 36 KiB memcpy and one directory store — is the escape hatch if it ever matters, and should be offered as a fallback rather than as the default.

### The cost statement that replaces the cap

Per MiB of guest linear memory = 262,144 words:

| cost | per MiB | grade |
|---|--:|---|
| host RAM, `--persist=table` | 4.00 MiB (16 B/word) | derived |
| host RAM, `--persist=packed` | 5.00 MiB (live table + pages) | derived |
| save size, `--persist=table` | 586 KiB (2.29 B/word) | measured |
| save size, `--persist=packed` | 113 KiB (0.44 B/word) | measured |
| worst-tick contribution | 0.202 ms | measured, 8→128 MiB, flat |
| load time, sharded | **~26 ms**, flat and no cliff | measured here |
| fkgc metadata | 8,960 B (0.854% of heap) + 27.3 KiB floor | measured |

**Sharding keeps host RAM at 16 B/slot, and that is a saving rather than a neutrality.** A Lua array part is a power-of-two `TValue[]`, and 2¹⁹ is a power of two — so N words in N/2¹⁹ shards is the same array bytes as N words flat, plus ~80 B per 2 MiB for the `Table` struct and its directory entry. Above 2²⁰ keys an unsharded table abandons its array part and slots become 40 B `Node`s in a power-of-two hash part, so **sharding is also a 2.5–5× host-RAM saving above 4 MiB.** (Derived from Lua's structure sizes, not measured in the game.)

> **fkgc imposes no heap cap.** A guest grows until Factorio's bill is one it does not want to pay, and the bill is the table above. The only hard bounds left are wasm32's 4 GiB — in practice a little under it, where `uint32` span arithmetic wraps, and the code must say so rather than wrap — and whatever `memory.grow` refuses.

---

## 10. The staged plan

### Stage B — the emitter and the runtime

1. `fk_rt.lua`: `MEM` becomes a shard vector; `S1`/`SHBOUND` chunk-locals; `ld8raw`/`st8raw`/`ld*`/`st*`/`st64` take the vector; `mem_grow` appends shards; `mem_copy`/`mem_fill`/`fk_wstr` split at boundaries; `MEMPACK.pack_page`/`unpack_page` translate; `restore` and `adopt` replace the vector.
2. The emitter: the three forms of §5, the static fold, and the guard's same-shard conjunct plus its hoisted shard local.
3. `fk_mod.lua`: `sync_memory` under the vector; the 4 MiB notice **retires**, because the wall stops being reachable.
4. **Chunk-local budget** is the live constraint: `S1` and `SHBOUND` are two new column-zero names against Lua's 200, and `TestPromotionLeavesTheMarginItPromises` already fails at one more. Something has to come out — `DPLO`/`DPHI` moving onto `MEMPACK` is the precedent.

**Gates.** Spec suite green at `-opt=0..3` in both NaN modes and both GC modes, `PASSRATE` unmoved. `run-roundtrip.sh` green on all three guests in both persist modes including the grow leg. Golden files regenerated **in the same commit**. `bench --opt` and `bench-guests.sh` within noise below the wall. The e2e harness reproduces §6 against the emitter instead of against the hand edit.

**New spectest coverage, because there is none above 4 MiB today.** A module declaring more than 2²⁰ words, exercising: a load and a store in every shard including the last partial one; an access at each side of a shard boundary; an 8-byte access **straddling** a boundary (the one shape whose two halves land in different tables); `memory.copy`/`memory.fill` spanning two and three shards; `memory.grow` that adds a shard and one that does not; and an out-of-bounds access one word past the last shard, which must still trap and must leave memory untouched. These are host-side and legitimate — they assert **answers**, not timings, and the oracle is correct about answers at any size.

### Stage C — the fkgc cap, and collected as the recommended default

1. Delete `cap.go`/`cap4.go`/`cap64.go` and the build tags; chunked metadata per §9; `oom()` rewritten to say `memory.grow` was refused.
2. `TestAGrowPastAChunkBoundaryInOneAllocation`, a determinism test on chunk placement (a lockstep game CRCs the save), and a large-object test at the new ~4 MiB ceiling.
3. Fix the stale 42/645 KiB figures wherever they appear (`cap*.go`, `agents/guests.md`, `cmd/fklua/main.go`).
4. Docs: `--gc=collected` as the recommended default, and the heap budget rewritten as a **cost** table with no functional limit in it.

**Gate:** `ARMS="stw stwbig"` — the above-wall arm's collection must fall from 8,580 ms toward the below-wall 84.5 ms/MiB, and `gcbench` must complete collections in both.

### Out of stage A's scope, recorded for triage

- **The 4 MiB wall notice does not fire on LOAD.** `note_wall` is reached from the chunk-level `P.memory()` (which sees the *declared* size, not the adopted one) and from `sync_memory` (which only fires when the size *changes*). A guest that crossed the wall in a previous session comes back past it, pays 7.0 s on every load, and says nothing — confirmed on the `stwbig` arm, whose notice appears in the map-creation log and in none of the benchmark runs.
- **The paced terminating step** (stage D item 5) should be re-derived now that the host-to-game constant is ~2.5 rather than unknown.

---

## 11. Risks, in the order they should worry someone

1. **The chunk-local budget** (stage B item 4). It is the only thing here that can make a guest stop compiling, and it fails at `-opt=3` while still compiling at `-opt=2` — a shape this repo has already been bitten by.
2. **A hoisted shard local outliving a vector swap.** `adopt` and `MEMPACK.restore` reassign the whole memory; a guard that hoisted `MEM[k]` before one of them keeps a table nobody else can reach. Silent, and only a text property catches it.
3. **The 8-byte access straddling a shard boundary.** `st64`'s aligned path writes `mem[i]` and `mem[i+1]`; across a boundary those are two different tables, and the spec requires an out-of-range store to leave memory untouched. This is the one correctness shape with no analogue in the flat representation.
4. **The above-wall win is pattern-dependent.** 17.3× end to end, 35–45× on the sequential microshapes, but only 5.1× on an 8 KiB-stride walk of the whole memory. Quote the range, not the maximum.
5. **The guard extension raises local pressure by one per base** — up to 9 new locals per function at 3 guards × 3 bases, on top of the flag and the word index.
6. **Nothing here is measured on a Rust guest above the wall.** The census says Rust's class-C share is 88.1% against TinyGo's 66.6%, which the design's default form covers — but `optimizer.md` twice records a pass that looked toolchain-specific and was not, and the reverse deserves the same suspicion.

---

## 12. What stage B needs to know

1. **The bounds check is the shard test.** That single merge is what makes the below-wall cost zero, and any implementation that emits the shard select *in addition to* the bounds check has thrown the result away — it measures 1.46–1.59×.
2. **A constant address folds to a constant shard**, not to shard 0. 15.5% of the corpus, and it is the biggest single class.
3. **`S1` and `SHBOUND` must be chunk-locals**, not table fields. The whole effect is one table index off a local.
4. **Verify the answers under `bin/lua52f` before timing anything in the game.** The oracle is wrong about the cost of a big table and right about every word in it; both shard-indexing forms and the boundary-crossing fill were checked that way here before a single tick was measured.
5. **The paired instrument is the one that matters.** Single-shot in-game access numbers carry up to 31% run-to-run spread, which is larger than the entire below-wall effect.

---

## 13. Stage B as built

**Everything in sections 1–12 was a design and a prediction. This section is what landed, what it measured, and the four places the design was wrong or incomplete.** Where a number here disagrees with one above, this one is the measurement and the one above was the forecast.

### The three forms, as emitted

`-opt=3`, from `internal/luagen/shard.go`, `loopguard.go` and `byteload.go`:

```lua
-- STATIC FOLD: the address is a compile-time constant and 4-aligned.
t0 = 1048836
if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
v10 = S1[262210]                       -- MEM[3][513] above shard 0

-- GUARD-HOISTED: the entry test gained one conjunct and one hoisted local.
lg41 = v1 >= 0 and v1 % 4 == 0 and v1 + t0 * 32 + 32 <= MEMSIZE
       and v1 % 2097152 + t0 * 32 + 32 <= 2097152
if lg41 then t1 = v1 % 2097152 lw41_0 = t1 / 4 + 1
             ls41_0 = MEM[(v1 - t1) / 2097152 + 1] end
...
if lg41 then v10 = ls41_0[lw41_0 + 7] else ... end

-- SHARD-0 FAST PATH: everything else.
t0 = ((v1 + 28) % 4294967296.0)
if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then v10 = S1[t0 / 4 + 1] else
  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
  if t0 % 4 == 0 then v10 = MEM[(t0 - t0 % 2097152) / 2097152 + 1][t0 % 2097152 / 4 + 1]
  else v10 = ld32(MEM, MEMSIZE, t0) end
end
```

**The static fold is not tied to shard 0** — §4 said so and it is worth repeating, because it is the biggest single class and the obvious implementation gives up half of it. It also keeps its bounds check: the range analysis proves the VALUE, not that the address expression is free of effects, and `MEMSIZE` is a runtime quantity a host `adopt` can move.

**A FOURTH shape exists that the design did not anticipate, and it is worth more than it sounds.** `if fast then A else B end` compiles in Lua 5.2 to test, jump-to-else, A, **JUMP-TO-END**, else, B — so the fast path pays one unconditional jump that the flat form's `if bad then trap_oob() end` did not. Where the tail is ONE shared expression the emitter avoids it entirely by having the slow arm rewrite `t0` into a within-shard offset and a scratch into that shard's table:

```lua
t2 = S1
if t0 < 0 or t0 + 1 > SHBOUND then
  if t0 < 0 or t0 + 1 > MEMSIZE then trap_oob() end
  t1 = t0 % 2097152
  t2, t0 = MEM[(t0 - t1) / 2097152 + 1], t1
end
t1 = t0 % 4
t2 = t2[(t0 - t1) / 4 + 1]
```

It applies to the inlined byte load — which can never straddle anything — and to a 4-byte load whose alignment the congruence analysis proved, because that is what leaves the slow arm call-free. **A proof of alignment now buys two things**: it drops the `% 4` branch as it always did, and it collapses the whole access into this form.

### The guard's conjunct is one modulo, not two floors

The obvious spelling of "both ends land in the same shard" is §5's `(e - e % 2097152) == ((…) - (…) % 2097152)`, which is two floors of two compound expressions. The same predicate with the algebra done is one modulo and one multiply-add, reusing the `t0 * stride` shape the bounds conjunct beside it already prints:

```
<base> % 2097152 + t0 * <stride> + <MaxEnd> <= 2097152
```

**Sound only because a stride is non-negative**, which `analysis.LoopGuards` enforces outright — `c < 0 || c%4 != 0` refuses the base — so the far end of the walk is always the high end and there is no downward case to fold in. Nothing about the analysis changed, and no loop stopped being guarded: the guard counts over the corpus are identical to the flat build's.

### The chunk-local budget: net +1, and the prelude paid nothing

§10 called this the live constraint and §11 listed it as risk 1. Resolved:

| | |
|---|--:|
| `S1` — shard 0 bound directly, exactly the status `MEM` had | **+1** |
| `SHBOUND` — `min(MEMSIZE, 2097152)` | **+1** |
| `MEMMAX` — a compile-time constant with ONE reader, now a numeral | **−1** |
| runtime prelude | **±0** |
| **net** | **+1** |

`MEMMAX` was read at exactly one site, the `memory.grow` lowering, which prints the number instead. The prelude's zero is the part that took design rather than luck: every helper the sharded runtime wanted lives inside a `do ... end` block, and the four bulk operations moved into ONE shared block so they declare the same four names they always did and get their shard helpers for free. `TestAMemoryCostsTheChunk…` pins the +4 a memory costs so a later prelude moves a test rather than a cliff.

### The straddle, and the order that makes it safe

Risk 3, and the one correctness shape with no analogue in the flat representation. A 4-byte *aligned* access can never straddle — a shard is 524,288 whole words — so only the 8-byte forms can, at exactly one offset per shard, 2097148.

**The emitter never inlines it.** The merged test `t0 + 8 <= SHBOUND` proves both words inside shard 0, and every else arm delegates to `st64` / `ld_f64` / `xld_f64` / two `ld32`s. That keeps ONE copy of the rule, in the runtime, where it is tested — the same trade the dirty-page mark already makes for wide stores.

In `st64` the bounds check covers all eight bytes and runs before either word is written, so a trapping straddle leaves both shards untouched. Pinned two ways: `TestAStraddlingEightByteAccessCrossesTheShardBoundary` reads the halves back as separate 4-byte loads on either side of the boundary, and `TestATrappingStraddleTrapsAndLeavesMemoryUntouched` **grows a guest to a memory that ENDS on a shard boundary**, so the last straddle point is also out of range — the case where writing the low word first would half-write memory and then raise a Lua nil-index error instead of a wasm trap.

### New coverage, because the suite had none above 4 MiB

`internal/luagen/shardmem_test.go`, all at four opt levels because the four levels emit genuinely different access forms: every shard including the last partial one; both sides of every boundary at every sub-word width; a straddling 8-byte load and store, i64 and f64; the trapping straddle above; fills across two and three shards; a copy whose two streams split at different points, in all four of `mem_copy`'s loops; a grow that adds a shard and one that only finishes filling the partial last one; and OOB one word past the end, which under a partial last shard is inside a table that exists at an index that has no value.

`internal/luagen/shardtext_test.go` carries the properties no behavioural test can see: `adopt` and `restore` move `S1` and `SHBOUND`, `S1` is the only chunk-level shard binding, pages nest inside shards, and the budget above.

In game, `run-roundtrip.sh` gained a **`growbig`** leg — `examples/grow` behind a build tag, 5 MiB of heap in ten half-megabyte blocks, which TinyGo's doubling takes to **8 MiB in four shards**. Every byte is written and checked after a real save and load, in both persist modes.

### The numbers

**In game, paired, through the REAL emitter against a pre-sharding compiler.** §6's tables were the hand edit; these are the shipped one.

| | flat | sharded | |
|---|--:|--:|--:|
| **2 MiB e2e**, median of 1,596 ticks | 2.210 ms | 2.225 ms | **1.007×** |
| 2 MiB e2e, mean | 2.244 ms | 2.248 ms | **1.002×** |
| 2 MiB e2e, the un-merged CONTROL arm | — | 3.155 ms | 1.43× |
| **6 MiB e2e**, median of 119 ticks | 41.534 ms | 2.328 ms | **17.8×** |
| 6 MiB e2e, load tick | 42.022 ms | 5.110 ms | 8.2× |
| **`gcbench stwbig`** worst tick, 6.4 MiB heap | 5,299.70 ms | 446.72 ms | **11.9×** |
| `gcbench stwbig` load tick | 5,252.17 ms | 434.06 ms | 12.1× |
| `gcbench stw` worst tick, 2.7 MiB heap | 190.03 ms | 182.68 ms | 0.96× |
| Lua's own GC worst tick, 2.7 → 6.7 MiB | 0.843 → 1.537 ms | 0.426 → **0.417** ms | flat |

`fk_sum` is identical across all three e2e arms at both sizes.

**17.8× against the 17.3× §6 predicted from hand-edited output**, and the control arm lands at 1.43× against the 1.38× it measured — so the merge is doing exactly the work it was measured to do. **The `gcbench` result is the one that changes a model rather than confirming one**: `stwbig`'s 827 ms/MiB became 69.7, which is the below-wall band's 67.3. §8 predicted "~540 ms at 6.4 MiB, a predicted ~16×"; the measurement is better than the prediction because the below-wall rate itself improved — the conservative scan walks the heap sequentially, which is the shape that binds one shard to a local and steps a within-shard index.

**Host-side, below the wall, paired and interleaved** (`bench-guests.sh`'s corpus, driven A/B/A/B): `pure_sum` 1.01–1.02×, `pure_prng` 1.00×, `pure_dot` 1.01×, `real_entities` 1.01–1.03×, `real_grid` 0.95–1.02×, and **`real_names` 1.10×**. `bench --opt` at `-opt=3`: `sum` 0.97×, `chase` 0.96×, `dot` 0.98×, everything else 1.00×.

### What it costs, stated rather than buried

1. **An inlined access is +1 VM instruction**, and it is the floor rather than a defect. `MEM[k]` where `MEM` is an upvalue compiles to a single **`GETTABUP`**, so the flat form was already optimal and a shard decision cannot be free — it is either a jump (the if/else shape) or a `GETUPVAL` (the no-else shape). Both were built and measured; they are the same cost.
2. **`real_names` is 1.10× host-side**, and it is the only kernel outside noise. Its iteration is almost entirely TinyGo's allocator, so its access-per-unit-of-work density is far above anything else in the corpus. **This does not transfer to the game**, and §2 is why: Factorio's table read is 4–6× the oracle's while the loop machinery is 1.04–1.10×, so an extra arithmetic or branch instruction is proportionally much cheaper there. The 2 MiB e2e arm — a packaged mod, 1,596 ticks — is 1.007×. **Do not accept a host-side ratio as the below-wall verdict.**
3. **`-opt=0..2` memory access is 1.14–1.22× host-side**, because below `-opt=3` every access is a helper call and the helper now selects a shard. `-opt=3` is the shipping default and is unaffected.
4. **A 16-bit load above 2 MiB falls back to `ld16`** instead of inlining. Two bytes can land in two shards and the lowering fetches each through its own address arithmetic, so inlining the straddle would double the size of a lowering whose whole purpose was to remove a call, for the arm taken only above 2 MiB. It is the one access shape sharding makes no faster above the line.
5. **A guarded loop whose SPAN crosses a shard boundary loses its guard** and every access in it takes the shard-0 fast path. §5 chose this and the probability is about span/2 MiB; strip-mining is stage C.

### Three things the runtime needed that the design did not mention

**The bulk operations' FIXED cost is what matters, not their per-word cost.** A guest's allocator copies tens of bytes at a time — TinyGo's `append` is the shape that dominates — so the piece machinery (two moduli, two shard lookups, two minima) was about as much work as a 24-byte copy itself: **1.24×**. The fix is a single compare: the bounds check already proved `d + n <= size`, so **`size <= 2097152` proves the whole operation inside shard 0 without touching either address**, and the flat loops run with `mem[1]` bound once. That is the same merge the emitted access makes against `SHBOUND`, one level up. A second and separate 1.14× on a 4 KiB copy was a doubled ADD per word in the piece loop, where the flat form had one.

**`st8b` is expanded rather than delegating to `st8raw`.** A byte store is not inlined at any opt level — its read-modify-write needs five values against a function's two scratch registers — so every byte a guest writes goes through that call *and* `st8raw`'s. Instrumented on `real_names`: **2,177,780 `st8b` calls in one rep and no `mem_copy` or `mem_fill` at all**, which is not where the guess would have gone. Expanding it removes a call the flat version also paid, which more than covers the shard test: **1.06× → 0.91×**, faster than the flat form.

**`mem_grow` returns THREE values.** `SHBOUND` moves with `MEMSIZE` and comes back from the same call, so a grow stays one statement in generated code. Deriving it at the call site would be a second comparison emitted at every `memory.grow`, and forgetting it would leave the fast path bounded by the OLD size — correct, silently slower, and invisible to every checksum.

### The wall notice is deleted

§10 listed it for triage: `note_wall` never fired on LOAD, so a guest that crossed in a previous session came back past the wall and said nothing. **Confirmed once more in the flat baseline run above** — `run-gcbench.sh` used the notice as its discriminator and printed "silent" for `stwbig`, whose linear memory is 6.7 MiB.

It is deleted rather than repointed, because every sentence it printed is now false: there is no crossing tick, no 20×, and no rebuild. The residual cost that DOES still scale with memory size is Lua's own collector walking it, and `note_memory` already reports exactly that. `run-gcbench.sh` reports the guest's own `heap=` line instead, which is a better instrument anyway — both arms report a number rather than one reporting silence.

### What stage C inherits

1. ~~**The fkgc cap** (§9), unchanged and now unblocked.~~ **DONE — see §14.**
2. **Strip-mining a guarded loop whose span crosses a shard**, which is the one coverage class stage B deliberately left on the fast path. **STILL OPEN**; stage C did not touch the emitter.
3. **`real_names`' 1.10× host-side**, if anyone wants it: it is the +1 instruction × a very high access density, and the only route to it is a form where the fast path is the fall-through and the slow path exits and returns — a `goto` shape this emitter has no precedent for. **STILL OPEN.**
4. ~~**`agents/guests.md`'s memory table is now a TOTAL cost curve.**~~ **RE-TAKEN at stage C, in game, out to a 37 MiB heap.** See §14.

---

## 14. Stage C as built — the cap is gone

**§9 was a design. This is what landed, what it measured, and the five places the design or the collector underneath it was wrong.** Where a number here disagrees with one above, this one is the measurement.

### The scaling metadata, as built

`guest/go/fkgc/meta.go`. The metadata splits by GROWTH LAW, which is §9's design with three corrections:

| part | where | size |
|---|---|--:|
| class tables, mark stack, free-run heads, pending dirty list, counters | `.bss`, one `gcMeta` struct | **32,116 B** |
| directory `metaDir[1024]`, covering all 4 GiB of wasm32 | `.bss`, inside `gcMeta` | 4,096 B of the above |
| mark bitmap + span table + span-aux, **one chunk per 4 MiB slice** | **the heap**, class `clsMeta` | **40,960 B each** |

```
MetaBytes = 32,116 + 40,960 x ceil(heap / 4 MiB)
```

which is a **0.977% tax on the heap** on a **31.4 KiB floor**, against the old static scheme's 163 KiB floor and 16 MiB ceiling. `TestTheMetadataSizeModelHolds` drives a real guest to four heap sizes and asserts the identity at each, which is what §9 asked for: the 42/645 KiB figures in `cap4.go`/`cap64.go` were wrong by 39% and 10.6% and nothing tested them.

**Three corrections to §9's design.**

1. **The chunk goes at the BOTTOM of the slice it describes, not the top.** §9 said the top, "so a run can stretch between chunks". That is not a reason — consecutive chunks leave the same 1,014-span gap either way — and the top does not work: a chunk at the top of slice *k* cannot be written until the heap has grown through the whole slice, so a guest wanting 64 KiB of heap would have had to take 4 MiB to describe it. Bottom placement is what makes coverage incremental.
2. **The chunk is placed in the lowest free run of the ALREADY-COVERED heap when there is one**, and only falls back to the slice's own first ten spans under pressure. §9's fixed position made every chunk a permanent blocker at a 4 MiB boundary and capped a single object at just under 4 MiB forever. With low-first placement the top of the heap stays contiguous and the ceiling is only reached when the heap is full — which is when `oom()` says so, by name.
3. **All three tables are `uint32`, not `uint32`/`uint8`/`uint16`.** A chunk is LINEAR MEMORY, and a byte or halfword store there is a read-modify-write in emitted Lua where a word store is one table assignment (stage B's `st8b` finding, one level up). It costs 14% more metadata — 10,240 B/MiB against §9's predicted 8,960 — and takes a read-modify-write out of the sweep's inner loop. It also makes the chunk exactly ten spans with nothing left over.

**The hazard §9 named is answered by one compare, as predicted.** Marking writes mark bits, mark bits are heap words now, so the collector dirties its own cards — and `rescanSpan` drops a `clsMeta` span with the span-class load it already performs. `markCandidate` rejects `clsMeta` too, so no mark bit is ever set for a metadata byte. The directory itself is a field of `gcMeta`, because it holds HEAP ADDRESSES and a directory outside `[metaLo, metaHi)` would be scanned as roots — marking every chunk live through the collector's own bookkeeping.


### The five defects, and four of them were older than this stage

**None of them was what agents/gc.md's open item 5 said it was.** The item read "the terminating step is not budgeted the way the others are — 65× at the default budget in game, 1.17× host-side". There is no such step. There were four separate places where collector work escaped the budget, and every one of them lands in the same tick as mark termination, which is why one step got the blame.

**1. The instrument could not see the work, and that is why the two numbers never reconciled.** `step()` zeroed its accumulator on entry and left it dirty on exit. Everything the mutator's own calls charged between two steps was therefore either discarded (if a step ran next) or attributed to the wrong side (if an allocation ran next). The host-side gate read `MaxStepWork` and saw 1.17×; the game measured a TICK and saw 65×; they were never the same quantity. `Stats().MaxUnpaced` is the missing half — collector work charged inside a guest call — and `MaxUnpacedFolds` is how many bursts made it up, which is what turned the next finding from a guess into a measurement.

**2. The sweep-ahead was unbounded, twice over.** Stage C forbade `findSpanRun` from handing out a span above the sweep cursor, which is sound — a span claimed there is walked by the sweep afterwards, found to hold unmarked slots, and freed with live objects in it — and which makes the mutator's search space EMPTY at the instant marking terminates. The next allocation then swept until a span fell free, inside an event handler. `clsFresh` answers the cursor's question per span instead, so the search space is whole. Bounding the bite at one step's worth was still not enough: a dispatch makes as many allocations as it likes, and in a real Factorio one dispatch took **131 bounded bites** — 131× the budget between two steps. The bite is gated on `callWork` now, so it is one bite per TICK.

**3. A re-scan of a dirtied page re-read the whole object.** `examples/gcbench` writes one slot of a 44,000-entry pointer array per tick; that array is 176 KiB, which is eleven steps of the default budget, and the mutator dirtied it again every tick. Marking could not terminate: **1,100 ticks in phase 1 with `cycles=0`** in a real Factorio, which agents/gc.md calls the worst failure available to a guest that opted in to a collector. A re-scan is one SPAN now — the store is in the page, so the page is what has to be re-read — which also deletes stage C's "skip a continuation span" patch and the O(size²) it patched.

**4. A full re-scan pass could complete without covering the heap.** `rescanOwed` is resumable through `rescanCursor`, and `drainGray`'s overflow path always reset the cursor — but `ingestDirty` and `Collect` set the flag and left the cursor where it stood. A pass already halfway up then declared itself COMPLETE without revisiting the spans below it, so a store into a marked object down there, made after the cursor had gone past, was never re-scanned: marking terminated, the sweep freed a live object, and the only symptom was a checksum. **It survived three stages because the heap cap was hiding it** — a guest in that state hit 16 MiB within a few hundred steps and the valve's synchronous `Collect()` forced an unbudgeted pass from zero. Removing the cap removed the accident.

**5. The dirty page record was perishable, and the recovery was on the normal path.** `dirtyQ` is the landing pad the host overwrites at every step, so a batch not drained in the step it arrived in was lost and the only recovery was a full re-scan. Any step whose budget went on one large object reached the drain with nothing left: measured on gctorture at the default budget, **876 of 1,752 steps** owed a pass — eight passes over a 4.5 MiB heap for one collection, and a mark that only ended at the deadline. The batch is copied into a pending list of the collector's own now, four times the size, drained over as many steps as it takes. Same guest afterwards: **264 steps, one pass, worst step 1.18× the budget.**

**And the deadline did not end the phase it was supposed to end.** agents/gc.md says "after the deadline the mark phase finishes in one unbudgeted step". It did not: `markStep`'s last act before the termination attempt is a `drainGray`, and a `drainGray` that overflows the gray stack owes a fresh pass — so the step ended with the phase still marking and the collector dropped back to a budget already shown not to converge. It is a loop over `markStep` now, which terminates because marks are monotone and the mutator is not running.

### In game, before and after

Factorio 2.0.77 on an M3 Pro, `scripts/run-gcbench.sh`, `-opt=3`, `--persist=table`, 1,200 ticks × 3 runs, the load tick dropped. BEFORE is master at `1087d74`; AFTER is this branch. Same guest, same map, same machine.

| | before | after |
|---|--:|--:|
| `paced` worst scriptUpdate tick | 26.435 ms | 22.9 – 30.0 ms |
| `paced` p90 | 0.906 ms | **1.25 – 1.50 ms** |
| `paced` worst PACED STEP | not measurable | **1,232 granules, 1.20× budget** |
| `paced` worst IN-CALL burst | not measurable | **141 granules, 0.14× budget** |
| `paced` mark escapes | not measurable | **0** |

(Ranges are run to run over four 3,597-tick runs; the granule figures did not move between them, which is the point — the collector's own accounting is stable where the wall clock is not.)

**The acceptance number is the p90 and the step, not the worst tick.** A budget of 1,024 granules is ~0.5 ms host-side and the host-to-game constant is ~2.5 (§2), so a step should cost about **1.25 ms in game** — and p90 lands at 1.25–1.50 ms with the worst step at 1.20× the budget in every run. That is budget × interpreter factor, which is what agents/gc.md's open item 5 asked for, against the 65× it recorded.

**The worst TICK did not come down to 1.25 ms and it is not the collector.** It is `memory.grow`: see the grow table in agents/guests.md, where 107 ns a word predicts both the 22.7 ms tick at a 3.5 MiB heap and the 288.6 ms tick at 40 MiB to within 5%. The collector's own worst step is 1.2× its budget in both.

### Past the cap, in game

`ARMS=pacedhuge TICKS=24000 RUNS=1`. The guest takes 36 MiB of patterned one-megabyte blocks in `on_init`, drops all but six on the first tick, and re-derives the surviving blocks' checksum every hundred ticks:

| | |
|---|--:|
| guest heap | **52 MiB** |
| collector metadata | 564,600 B (**1.06%** of heap) |
| bulk checksum across the run | **unmoved** |
| worst paced step | **1,248 granules, 1.22× budget** |
| worst in-call burst | 140 granules, 0.14× budget |
| outruns | 0 |
| scriptUpdate median / p90 | 1.025 / 1.895 ms |

**A guest grew through 16 MiB and past 32 MiB, paced, in a real Factorio, with its answer intact.** Under the old cap that guest trapped with a bare `unreachable` at 16 MiB, and `-tags fkgcheap64` would have cost 583 KiB of .bss before it allocated anything to reach 64.

**What it costs, and it is a real cost.** A 40 MiB heap at the default 0.5 ms budget takes tens of thousands of ticks to turn over once — the reclaim-rate table in agents/gc.md says ~190 KB/s and 40 MiB is 210 seconds of it. The same arm at 1,200 and 6,000 ticks completes no collection at all, which the script correctly refuses to report. A guest with a heap that large and a collection it wants finished should raise `fkgc.SetBudget`; a guest that just wants the memory does not have to.

### Gates

| gate | result |
|---|---|
| `go test ./...` | green, 10 packages |
| `go test ./... -race` | green |
| spectest, 4 opt levels × 2 NaN modes × 2 GC modes | 15,675/15,675 in each, `PASSRATE` unmoved |
| `scripts/run-roundtrip.sh`, four guests × two persist modes | green, after the sixth defect below |
| `scripts/run-gcbench.sh` in a real Factorio | tables above |
| golden files | regenerated in the same commit |
| `gofmt` / `go vet` | clean |


### The sixth defect: the escape could not see the thing it existed to escape

`scripts/run-roundtrip.sh`'s `gcsave` leg went RED at the first attempt and stayed red through four wrong fixes, which is the most expensive thing in this stage and the most reusable. The guest sat in `phase=1` with `cycles=0` for 140 ticks in a real Factorio, in both persist modes, so no save landed inside a collection and the leg's whole assertion tested nothing. Master completes 16-17 collections on the same guest.

**The first thing to fix was the harness, and it invalidated four measurements.** `build_guest` is `if [ ! -f "$wasm" ]`, so the wasm is CACHED across runs — every re-run after a collector change was measuring the binary built by the FIRST run. Four "the fix did not help" conclusions were drawn from a stale guest before `rm testdata/tmp/*.wasm` produced a different number. A build cache keyed on nothing is not a cache, and this one is silent.

**The root cause, once the instrument was honest, was two things.**

**(a) The pending dirty list had no deduplication.** A page the mutator writes every tick took a new slot every tick, so the owed work climbed to **103,092 granules on a THIRTY-TWO span heap** — 402 pending spans on a heap that has 32. The list was not a set. `clsPending`, a second flag bit in the span-class word, makes it one: a page whose span is already pending is dropped, and so is one below the heap, above the coverage line, unassigned, or holding metadata, none of which can hold a marked object. Owed fell from 103,092 to a bounded 673-1,061. Dedup does not weaken the barrier — re-scanning a span once covers every store into it, because what a re-scan reads is the span as it stands now.

**(b) The escape was keyed on the wrong signal, three times over.** With the list bounded, the mark still never terminated, and the reason is the one the milestone brief predicted: the forward-progress metric was counting the wrong thing.

| keyed on | what it read on gcsave | why it is wrong |
|---|---|---|
| "the record of what changed was lost" (`rescanRestarts`) | **0**, for the whole livelock | nothing is ever lost; the collector is merely too slow. An escape keyed on losses cannot see a guest that is only too fast. |
| "did this step mark anything new" | rose every step (33 → 436) | a mutator that allocates every tick has new objects marked every step. That is the target moving, not the chase closing. |
| "did the owed work fall, step to step" | fell about half the time | it oscillates with the per-tick influx. gcsave's owed reads 673, 721, 757, 1061, 841, 877 over fifty ticks — no trend, and a consecutive-step test resets constantly. |

**What works is to ask the two OWNERS separately**, over a window:

> **SCAN work** — the gray stack, the resumable object scan, the remainder of a full re-scan pass. Only the COLLECTOR adds to it, and it consumes it monotonically.
>
> **DIRTY work** — spans the mutator wrote that the collector owes a re-scan. Only the MUTATOR adds to it.
>
> A window of `markStallWindow` steps is STALLED when the pending dirty list did not reach empty at any step in it AND the scan work did not shrink across it. After `markStallLimit` consecutive stalled windows the mark phase stops yielding. Neither condition alone is a livelock; together they are exactly "no net shrinkage of (unmarked + dirty)".

That is the split the two cases demand, and each one needs the other's clause:

- **gcsave is dirty-dominated.** Its live set is tiny and marked in a handful of steps, so scan work is nil and flat; `pempty` reads **0** over the whole run — the pending list never once empties. Every window stalls; the escape fires at step ~40.
- **`gcbench pacedhuge` is scan-dominated.** A 40 MiB live set spends sixty consecutive steps inside one 1 MiB object, so its pending list is non-empty that whole time — an escape keyed on "the dirty list did not empty" would force it. Its scan work falls by a full budget every step, so those windows show progress. Measured at a **54.5 MiB heap over 24,000 ticks**: `stalls=1`, `deadlines=0`, `pempty=4956`, worst paced step 1.21× budget, one collection completed. The escape stays out of the way.

**And the escape finishes the MARK PHASE AND NOTHING ELSE.** The unbudgeted allowance was falling through into the sweep, which swept the whole heap in the same step — so a collection finished inside one tick and the leg's "save landed mid-sweep" case became unreachable, because there was no sweep left to land in. The sweep is the expensive half and needs no barrier; keeping it paced is the whole of stage C's design.

**The leg's tick constants moved with the cadence, and the guest now prints the cadence.** A collection on this deliberately-over-budget guest was ~4 ticks and is ~47: what used to end its mark was a full re-scan pass completing WITHOUT COVERING THE HEAP — defect 4, the use-after-free — and what ends it now is an escape that must watch for a bounded number of windows before it may conclude anything. `examples/gcsave` logs a line at every phase change, so the save ticks are read off a trace instead of guessed:

```
phase 0  -> 1 cycles=0      mark
phase 41 -> 2 cycles=0      sweep
phase 48 -> 1 cycles=1
phase 89 -> 2 cycles=1
phase 95 -> 1 cycles=2
```

`GC_SAVE_TICKS` is `60 45` (mid-mark, mid-sweep) and `CHECK_TICK` is 120. The loaded run's length is derived from those rather than fixed at 30 ticks, which was a second silent cap: a `CHECK_TICK` more than 30 ticks past the save produced no report line at all and every leg failed as "state did NOT survive" — a benchmark that stopped early, reading exactly like a persistence bug.

**And `run-gcbench.sh` no longer calls a slow mark a livelock.** `cycles=0` wore one message for two failures. `phase=2`, or `stalls=0` with `deadlines=0`, is a mark that terminated or is converging — a run too short for the heap at that budget, whose remedy is more ticks or a bigger budget. A rising stall count with `phase=1` is the livelock. `pacedbig` is the first case and was being told it was the second.

### Gates, re-run after the fix

| gate | result |
|---|---|
| `go test ./...` | green, 10 packages |
| `go test ./... -race` | green |
| spectest, 4 opt × 2 NaN × 2 GC | 15,675/15,675 each, `PASSRATE` unmoved |
| `run-roundtrip.sh`, four guests × two persist modes | **green**, `gcsave` resuming MID-MARK and MID-SWEEP with 32/32 intact across 2 collections |
| `run-gcbench.sh` `paced` / `pacedbig` / `pacedhuge` | green; worst paced step 1.20-1.21× budget, 0 mark escapes. **Tick counts are load-bearing**: `paced` at the default `TICKS`, `pacedbig` needs enough ticks to complete a cycle at the default budget (`TICKS=1200` is TOO SHORT — the harness now says so rather than misdiagnosing a livelock), `pacedhuge` was run at `TICKS=24000 RUNS=1`. A rerun at smaller counts failing with "RUN TOO SHORT" is the harness working, not a regression |
| bindings / goldens `--check` | up to date |
| `gofmt` / `go vet` | clean |

---

## 15. The grow pacing — the last unbounded stall, and it was never the collector

**Read this before touching `mem_grow`, `fkgc.growHeap`, or the shard size.**

Stage C ended with the collector paced to 1.20–1.22× its budget and a worst TICK of 22.9–30.0 ms it could not explain with the collector. §14 attributed that to `memory.grow` at 107 ns a word, by arithmetic. This stage measured it, and the arithmetic was right about the mechanism and wrong about what follows from it.

### What the probe found, and it changed the design

`scripts/run-growprobe.sh` is a bare Lua mod — no wasm, no emitter, no `--persist`, no collector — holding `mem_grow`'s fill loop verbatim, timed with `helpers.create_profiler()`. Same instrument shape as `run-shardprobe.sh`, and for the same reason: the quantity is what it costs Factorio's Lua to CREATE a table slot, and the oracle is wrong about that in both directions.

| what | measured |
|---|---|
| ns per word, fitted over four increments × three heap sizes | **109.7 – 127.7** |
| the FIXED term of that fit | **negative at every size** |
| 0 → 40 MiB in 640 grows of 64 KiB vs 10 grows of 4 MiB | **0.984×** |
| a grow whose words are already materialised | **1.2 – 2.7 µs** |
| one 2 MiB shard filled in 8,192-word pieces vs in one go | **1.02 – 1.03×** |
| `{table.unpack(z, 1, 524288)}` cloning a zero shard vs filling it | 0.59 – 0.68× |

**There is no fixed cost per grow.** That is the finding the whole design turns on, and it falsifies the premise the milestone opened with — that smaller increments buy a lower worst tick at the price of more total overhead. They do not: the total is flat to within 2% across a 64× range of increment, and it is the LARGER increments that are marginally worse.

**And a presize IS possible for a shard**, which `fk_rt.lua`'s own comment said was impossible. It is right about the FLAT table — `{table.unpack}` refuses at 2²⁰ — and a shard is 2¹⁹, under the limit. It is refused anyway and the comment now says why rather than saying it cannot be done: it is 40% off a cost that pacing takes to zero, and it is ONE INDIVISIBLE C CALL, so it cannot be cut into pieces. Trading a paced 0.9 ms for an unpaceable 35 ms is the wrong direction.

### The design — a fill cursor, and a cap on the speculation

**`FILL` is the word index up to which the shard vector is materialised, and it may run ahead of `MEMSIZE`.** The words between them are zero BY CONSTRUCTION and not by convention: every path that can write a word — every emitted access, `ld*`/`st*`/`st8raw`, `mem_copy`, `mem_fill`, `fk_wstr`, the host's own writes — opens with a bounds check against `MEMSIZE`, so neither the guest nor the host can reach them. A grow into them moves the bound and does nothing else.

`MEMPACK.prebuild(mem, budget)` advances the cursor by one bounded piece and reports whether more is owed. `control.lua` drives it from a one-shot `on_tick` that `mem_grow` arms through `MEMPACK.grow_hook` — **the `fk.defer` / `fk_gc_step` shape for the third time**, and for the third time because the rule is the same: a guest that is not growing must carry no per-tick handler. The hook fires only from the arming path of a REAL grow, and the chunk's own initial construction goes through `mem_grow` with `size = 0`, so **a guest whose declared memory is enough pays nothing at all** — no callback, no registration, and not one word beyond what it declared.

Two numbers are policy and both are bounded rather than proportional:

- **`PREAHEAD` = 1 MiB.** A materialised word above `MEMSIZE` is a real Lua slot, and "one grow ahead" is unbounded for a guest whose growth law is a doubling.
- **`growCapSpans` = 16 spans = 64 KiB, one wasm page**, capping the SPECULATIVE part of fkgc's quarter. `needSpans` always wins — a megabyte-sized object still forces a megabyte-sized grow, and the pre-build is what covers that. The cap must clear `metaChunkSpans` or the coverage-crossing round-up fights it on every grow that reaches a 4 MiB slice boundary; that is a compile-time assertion in `heap.go` rather than a comment.

A cap of 64 spans (256 KiB) was built and measured and is not here: on the `gcbench paced` arm it was worse on BOTH axes, 31.6 ms worst against 17.1 and 0.815 ms mean against 0.769.

### In game, before and after

`scripts/run-growbench.sh`, Factorio 2.0.77 on an M3 Pro, 6,000 ticks, `-opt=3`, one guest allocating 8 KiB a tick to each target, six arms. The guest logs the tick of every `memory.grow` and the script pulls exactly those rows out of `--benchmark-verbose`, so these are GROW ticks rather than a maximum that cannot say what it was.

| law | target | worst grow tick | p90 | median | grows |
|---|---|--:|--:|--:|--:|
| collected | 4 MiB | 43.2 → **17.2** | 26.4 → **2.6** | 7.0 → **1.1** | 17 → 66 |
| collected | 16 MiB | 108.5 → **22.8** | 78.7 → **3.3** | 11.4 → **1.6** | 23 → 262 |
| collected | 40 MiB | 253.9 → **24.6** | 168.5 → **3.7** | 20.0 → **2.2** | 27 → 654 |
| leaking | 4 MiB | 127.4 → **98.0** | — | 37.5 → **21.1** | 6 → 6 |
| leaking | 16 MiB | 495.0 → **491.3** | — | 60.7 → **38.5** | 8 → 8 |
| leaking | 40 MiB | 998.0 → **974.5** | — | 59.3 → **37.5** | 9 → 9 |

**The collected column stopped SCALING, which is worth more than the ratio.** 43 / 109 / 254 was proportional to the heap; 17 / 23 / 25 is flat in it.

`gcbench paced`, three runs each, everything else identical:

| | master | this branch |
|---|--:|--:|
| worst `scriptUpdate` tick | 29.9 – 35.9 ms | **16.6 – 17.2 ms** |
| p90 | 1.64 – 1.67 | 1.74 – 1.76 |
| p99 | 3.81 – 4.23 | **7.06 – 7.14** |
| mean | 0.67 – 0.69 | **0.77 – 0.78** |
| worst paced collector step | 1232 (1.20×) | 1230 (1.20×) |
| worst IN-CALL burst | 141 (0.14×) | **1024 (1.00×)** |
| guest heap | 3.352 MiB | **2.988 MiB** |

**Three of those moved the wrong way and all three are the same trade**: work that used to land on one tick lands on many. p99 and the mean rise because a growing guest now spends ~0.9 ms of most ticks pre-building; the in-call burst rises from 0.14× to 1.00× of budget because ten times as many grows reach `allocSpans`'s sweep-ahead, which takes exactly one bite — the bound is unchanged and it is now simply reached. The heap ends SMALLER, because a bounded increment overshoots less.

### The residual, attributed — it is Lua reallocating a shard

The worst grow ticks left are 16.2–19.1 ms for a **16,384-word** grow, which is 1,090–1,503 ns a word against a 107 ns/word model. The model does not explain them and must not be stretched to. What they are is exact:

> **Every odd megabyte, 1.00 through 41.00, at 16.2–19.1 ms, FLAT in heap size** — deterministic across `--benchmark-runs`, as a minimum over passes.

An odd megabyte is 2¹⁸ entries into a 2¹⁹-word shard: the last array-part doubling a shard ever does, one indivisible reallocation per 2 MiB of guest memory ever taken. It was always there — the pre-pacing 43.2 ms outlier at a 4 MiB heap is a 163,840-word grow crossing 3.00 MiB, which is 17.5 ms of fill plus 25.7 ms of this — and the fill was simply bigger.

**So the grow tail is bounded by the SHARD SIZE, which is the second time that sentence has been true.** §"Shard size: 2¹⁹ words" chose it for the access cost and the guard and said the tail it leaves is a measurement rather than an assumption; Lua's own collector's worst tick is one shard's `propagatemark` (~0.5 ms) and the grow tail is one shard's last array-part doubling (~17 ms). Halving the shard would roughly halve both and would cost the guard lever, which is the trade that section already frames. **Recorded, not taken.**

**AND THE PRESIZE WAS RE-MEASURED AGAINST THE SHAPE THAT SHIPS — see §16. It loses, and the numbers are there rather than the argument.**

### Gates

| gate | result |
|---|---|
| `go test ./...` and `-race` | green, 10 packages |
| spectest, 4 opt × 2 NaN × 2 GC | 15,675/15,675 (15,777 exact) in each, `PASSRATE` unmoved |
| `run-roundtrip.sh`, four guests × two persist modes | green, incl. `grow` and `growbig`; `gcsave` MID-MARK and MID-SWEEP |
| `run-gcbench.sh` `paced` | green ×3, worst paced step 1.20× budget, 0 mark escapes |
| `TestAnAllocationStormGrows…` | green; heap 13.10 MiB against master's 13.88, one latched escape |
| bindings / goldens `--check` | up to date |
| coverage gate, `gofmt`, `go vet` | clean |

---

## 16. The shard-doubling residual — measured against the shape that ships, and ACCEPTED

**Read this before proposing a presize, a template clone, or a smaller shard.**

§15 left one thing open: the last piece of a grow tick is Lua reallocating a shard's array part at its final doubling, 2¹⁸ → 2¹⁹ entries, **16.2–19.1 ms, once per 2 MiB of guest memory ever taken**. It also recorded the presize as REFUSED, on the grounds that `{table.unpack(z, 1, 524288)}` is one indivisible C call.

**That refusal was taken against the wrong comparison, and the comparison is the whole finding here.** §15 scored the presize at "0.59–0.68× of the fill loop" against a fill loop that was then a **57 ms stall**. Pacing landed in the same milestone, and afterwards the thing to beat is not 57 ms of total — it is the LARGEST PIECE, because the rest of the total now lands on ticks nobody is waiting on. A shape that is cheaper in aggregate and larger in its largest piece is a regression, and that is the opposite of how the presize was originally scored.

### The three shapes, measured

`scripts/run-growprobe.sh` §6, Factorio 2.0.77 on an M3 Pro, `RUNS=2`, minimum over passes. One 2 MiB shard, built three ways at three heap sizes, on a freshly built memory per arm. Pieces are `PREBUILD_BUDGET` = 8,192 words, which is `fk_mod.lua`'s real number rather than a chosen one. **Every piece is timed separately** — `helpers.create_profiler()` will not hand Lua a raw number, so a maximum cannot be computed in the mod and the analysis takes it over the logged pieces.

| heap | shape | worst piece | total | pieces | indivisible |
|---|---|--:|--:|--:|--:|
| 4 MiB | **A — paced fill (ships)** | **16.47** | 58.15 | 64 | — |
| | B — clone 2¹⁹ at creation | 39.54 | 53.65 | 65 | 39.5 |
| | C — clone 2¹⁸, pace the rest | 19.50 | 55.05 | 33 | 19.5 |
| 16 MiB | **A** | **17.96** | 63.70 | 64 | — |
| | B | 39.60 | 53.81 | 65 | 39.6 |
| | C | 19.56 | 56.76 | 33 | 19.6 |
| 40 MiB | **A** | **15.71** | 57.34 | 64 | — |
| | B | 37.03 | 52.01 | 65 | 37.0 |
| | C | 18.76 | 56.36 | 33 | 18.8 |

**A wins on the only axis that matters, and it is not close.** B is 0.84–0.92× on the total and **2.25–2.36× on the worst tick**, all of it in one C call that nothing can cut. C is 0.89–0.96× on the total and 1.09–1.19× on the worst.

### Where Lua's doublings actually land, which is what kills the hybrid

C existed because of a plausible-sounding idea: presize to 2¹⁸ and the last doubling is *skipped* rather than merely relocated. The probe says which:

| heap | arm | worst piece is # | at word offset | worst ms | median piece |
|---|---|--:|--:|--:|--:|
| 4 MiB | A | 32 | 262,144 | 16.47 | 0.555 |
| | B | 0 | 0 | 0.95 | 0.146 |
| | C | 32 | 262,144 | 16.52 | 0.603 |
| 16 MiB | A | 32 | 262,144 | 17.96 | 0.562 |
| | B | 1 | 8,192 | 0.67 | 0.148 |
| | C | 32 | 262,144 | 17.23 | 0.657 |
| 40 MiB | A | 32 | 262,144 | 15.71 | 0.541 |
| | B | 0 | 0 | 1.10 | 0.148 |
| | C | 32 | 262,144 | 17.43 | 0.662 |

> **The doubling lands on piece 32 — word offset 262,144 — in A and in C alike, and 262,144 IS 2¹⁸.** §15's attribution is confirmed to the piece.

**So the hybrid pays the doubling TWICE.** Presizing to exactly 2¹⁸ gives the table an array part of exactly 2¹⁸; the first store past it goes to an empty hash part, forces a rehash, and `computesizes` picks 2¹⁹ — the identical reallocation, now immediately after a 19.5 ms indivisible presize instead of after 32 cheap pieces. There is no arrangement of a presize that skips the last doubling, because a presize to N followed by a write to N+1 IS the doubling.

**What B genuinely buys, and it is real:** its pieces run at a **0.146–0.148 ms median against A's 0.541–0.662**, i.e. the clone really does remove the fill work rather than moving it — writing a slot that exists is ~4× cheaper than creating one. It buys that for a 37–40 ms tick, which is more than twice the one it is trying to remove.

### Can the pre-build cursor absorb it on its own tick?

**It already does, and that is why 16 ms is the number to beat.** The splice — a grow whose words the cursor already materialised — measures **0.0010–0.0025 ms** across every increment and heap size in the same run. When the pre-build keeps up, the doubling is already on a pre-build tick and the growing tick pays nothing; the 16–19 ms `run-growbench.sh` attributes to GROW ticks is the case where the guest **outran** the cursor and `mem_grow` did the fill itself.

Moving the presize to that same tick therefore changes nothing about its size. The pre-build's tick is not a cheaper place to spend 37 ms than the grow tick is — it is the same 16.7 ms frame budget either way, and a one-shot `on_tick` is not a background thread.

### The template is a standing cost that no table above charges for

B and C both need a live 2¹⁹-word zero shard to clone from: **8 MiB of host RAM, 2.29 B/word of save under `--persist=table`, and one more table for Lua's own collector to walk on every cycle — a permanent ~0.5 ms `propagatemark` and its share of the 0.202 ms/MiB total, paid by every guest whether or not it ever grows.** Building it on demand instead makes the first shard cost the fill it was trying to avoid. Neither table above includes any of this, and including it widens a loss.

### Verdict

**ACCEPTED WITH NUMBERS. The current shape is the best known one and the item is closed as bounded, attributed and best known** — not as solved. What stands:

- the residual is **one indivisible 15.7–19.6 ms piece per 2 MiB of guest memory ever taken**, at 2¹⁸ words into each shard, flat in heap size and deterministic across passes;
- **no presize shape beats it**, and the two candidates lose on the axis that matters by 2.3× and 1.2×;
- **the only lever left is the shard size**, exactly as §"Shard size: 2¹⁹ words" framed it — halving the shard roughly halves both this and Lua's own worst `propagatemark`, and costs the guard lever. Still recorded, still not taken.

**Do not re-try the presize without a new argument about the LARGEST PIECE.** "It is 40% cheaper in total" is true, was true before, and is not the question.
