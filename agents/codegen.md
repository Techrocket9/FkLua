# Codegen reference

The lowering rules, the two numeric representations, and the NaN modes. Read `CLAUDE.md` first for the invariants these all follow from.

## Measured lowerings

Every number below was **measured inside Factorio 2.0.77** by the day-0 probe. Raw data: `bench/baselines/probe-2.0.77.json`; rerun with `./scripts/run-probe.sh && python3 scripts/analyze-probe.py`. Costs are marginal, with the empty loop (3.56 ns/iter) subtracted. Use them for **relative** comparison between lowerings, not as absolute targets.

⚠️ **Do not "optimize" these back to what intuition suggests.** Three of them are the opposite of what benchmarking on Lua 5.5 indicated, because 5.5 has an integer subtype and Factorio's 5.2.1 does not. If you want to change a lowering, re-run the probe.

**`%` is the cheapest way to wrap. Not a conditional fixup, not `bit32`.**

| i32.add wrap | ns | |
|---|---|---|
| `(a+b) % 4294967296.0` | **2.81** | ← emit this. Branch-free, so cost does not depend on whether it actually overflowed. |
| `s=a+b; if s>=2^32 then s=s-2^32 end` | 3.66 (not taken) / 5.34 (taken) | 1.3–1.9× worse |
| `bit32.band(a+b, 0xFFFFFFFF)` | 19.15 | 6.8× worse |

**Shift right uses `%`, not `math.floor`.**

| i32.shr_u | ns | |
|---|---|---|
| `(a - a % 2^n) / 2^n` | **5.04** | ← emit this |
| `math.floor(a / 2^n)` | 12.99 | 2.6× worse — the C call costs more than `fmod` |
| `bit32.rshift(a, n)` | 17.46 | 3.5× worse |

**Multiply: specialize constants, and use the magic-number floor otherwise.**

| i32.mul | ns | |
|---|---|---|
| by constant `c` where `a·c < 2⁵³` | **2.88** | **18.75× cheaper than the general path.** Struct offsets, array strides and hash multipliers are nearly all small constants, so this is not an optional optimization. |
| 16-bit split, magic-number floor | **53.99** | ← the general path. `q + 6755399441055744.0 - 6755399441055744.0`, with a correction branch. **Verified exact** in Factorio. |
| 16-bit split, `math.floor` | 62.18 | |
| 16-bit split, `bit32` | 111.91 | |

**Division by a constant is arithmetic, not a helper call — from `-opt=1`.**

`i32.div_u` and `i32.rem_u` were an unconditional `div_u(a, c)` / `rem_u(a, c)` at every level. When `c` is a known non-zero constant the call goes:

| i32 op | constant-divisor lowering |
|---|---|
| `i32.rem_u a c` | `a % c` — which for `c = 2ⁿ` is already the and-with-low-mask form |
| `i32.div_u a c` | `(a - a % c) / c` — the same expression as a constant `shr_u`, so a power of two needs no separate case |
| `i32.div_u a 1` | the identity |
| `i32.div_s` / `i32.rem_s` | the same two forms, **only** when the range analysis has also proved the dividend below 2³¹ |

**Exact, not approximately right, and verified rather than argued.** Under Invariant A `a` is an integral double in [0, 2³²) and `c` an integer in [1, 2³²), so `a % c` is exact (Lua's `%` is `fmod` plus a sign fixup, and `fmod` is exact), `a - a % c` is the exact multiple `q·c < 2³²`, and IEEE division of an exactly-representable `q·c` by `c` returns `q` exactly because `q` is representable and division is correctly rounded. Checked under `bin/lua52f` against a Go `uint32`/`int32` oracle over 200,812 `(a, c)` pairs — the corners plus 200,000 xorshift32 pairs — and 10,000,000 more covering every divisor in [1, 2·10⁶] against the five largest dividends. Zero mismatches. `internal/luagen/constdiv_test.go` keeps a distilled version of that sweep in CI.

**The signed pair is deliberately narrow.** Invariant A puts an *unsigned* value in the slot, so a general `div_s` must first recover the signed one — a conditional subtract, which is a statement and a scratch register rather than an expression, and the win over one helper call then stops being obvious. What is clearly safe is a dividend the range analysis has bounded below 2³¹ against a constant divisor in [1, 2³¹): both are non-negative as signed values, truncating and flooring division agree, and *both* traps are unreachable — `c ≠ 0`, and the INT_MIN/−1 overflow needs a divisor of −1, which is `0xFFFFFFFF` and not below 2³¹. A dividend nothing bounds keeps the helper call, which on TinyGo output is the common case.

Two hazards, both in [`agents/optimizer.md`](optimizer.md) and both with a test that was seen to fail: the discarded divisor operand belongs in `mayNotEvaluate`, and `div`'s `(a - a % c) / c` names its dividend **twice**, so it belongs in `duplicatesOperand`.

**Other bit ops that have a cheaper arithmetic form:**

| Instead of | Emit |
|---|---|
| `bit32.band(a, 2^k-1)` | `a % 2^k` |
| `bit32.band(a, ~(2^k-1))` | `a - a % 2^k` (align down) |
| `bit32.bxor(a, 0xFFFFFFFF)` | `4294967295.0 - a` |
| `bit32.bor(a, b)`, bits provably disjoint | `a + b` — catches `(hi<<16) \| lo` |

Genuinely need `bit32`: general `and`/`or`/`xor`. Everything else has an arithmetic form, and `bit32` was the *slowest* option in every case measured.

**Operand forwarding is load-bearing, not an optimization.** A `local.get` or `i32.const` is substituted directly into the instruction that consumes it. *(From M5, `-opt=1` generalises this to any single-expression step, with five hazards that each cost a conformance failure to find — [`agents/optimizer.md`](optimizer.md).)* Without it every operand is copied through a stack slot first, roughly doubling instruction count:

```lua
v2 = v0                          v2 = (v0 + v1) % 4294967296.0
v3 = v1                  -->
v2 = (v2 + v3) % ...
```

It is sound because a wasm operand-stack slot is written once and read once. **The one hazard is a `local.set`/`local.tee` landing between a `local.get` and its consumer** — that invalidates the pending forward, and `TestForwardingRespectsLocalWriteHazard` pins it. When control flow arrives at M2, forwarding must not cross a basic-block boundary.

This matters beyond code size: the M0 kernels were hand-written in the forwarded style, so un-forwarded output would invalidate the ratios that justified the project.

**Other rules:**
- **`MEM` is 1-based** — but for `#MEM` semantics and tidiness, **not** for speed. The previously claimed 3–5× penalty for 0-based indexing **does not reproduce**: 1-based and 0-based array reads differ by ~2% (18.20 vs 17.81 ns), and a pure hash-part table was actually 21% *faster* (14.74 ns). Do not spend effort defending this.
- **Float bit-punning uses `frexp`/`ldexp`, not `string.pack`** — 15.43 vs 85.88 ns, and allocation-free where `string.pack` allocates a string per operation. *Caveat: the frexp kernel extracts only the exponent, so the true ratio is smaller than 5.6×.*
- **…and `ldexp` is now a TABLE READ.** Every f64 and f32 reassembly ended in `ldexp(mant, e - bias)`, which is a C call. `PE[e]` is `2^(e-1075)` indexed by the **biased** exponent, so the reassembly ends in one table read and one multiply instead. Worth **0.910× on `pure_dot`** through the real compiler (`dot` at `-opt=3` goes 0.76× → 0.71× against `-opt=0`), and it is the same reasoning `P2` already carried one width down.
  - **Exact, not close.** Scaling by a power of two only changes an exponent, so `mant * PE[e]` and `ldexp(mant, e-1075)` are one operation either way. Verified under `bin/lua52f` over every biased exponent a caller can reach crossed with corner and random mantissas: 65,472 f64 pairs and 1,778 f32 pairs, zero mismatches.
  - **Built by iterated halving and doubling, never by `^`.** `2 ^ k` is `OP_POW`, i.e. libm's `pow()`, whose exactness on a power of two is not promised — and a table differing by one ulp across platforms is a desync in a lockstep game. Repeated scaling from 1.0 is exact by construction, subnormals included.
  - **Indexed by the BIASED exponent** so the table is a dense array part. Indexing from −1074 would put every entry in the hash part.
  - **It costs no chunk local.** `PE` replaced the last use of `ldexp`, so dropping `ldexp` from the localised set pays for it exactly. `TestPromotionLeavesTheMarginItPromises` caught the version that did not — landing a guest at 197 chunk locals against the 196 the margin promises, which is the "every local added to the prelude costs a guest one global" trade being enforced rather than described.
- **i64 is two Lua locals `(lo, hi)`, never a boxed table.** i64-returning functions use Lua's native multiple return. Boxing costs an allocation and a metamethod dispatch per operation, putting GC pressure into a lockstep game loop.
- **Emit float constants as hex float literals** (`0x1.91eb851eb851fp+1`). **Verified** to parse and to be exact. This matters more than it looks: Factorio's `tostring` gives only 14 significant digits (`tostring(3.14159265358979)` → `3.1415926535898`), so decimal emission silently loses precision.
- Prefer `x * 0.25` over `x / 4` — `OP_MUL` beats `OP_DIV`, and the reciprocal is exact for powers of two.
- **Shift left masks first**: `(a % 2^(32-n)) * 2^n`. `a * 2^n` alone can reach 2⁶³ and lose precision.
- Localize `floor, frexp, ldexp, band, bor, bxor, sqrt, abs` in the chunk prologue. Never `math.floor(x)` inside a function — that is `OP_GETTABUP` + `OP_GETTABLE` per call.
- **Promote hot callees to upvalues.** `F[idx](…)` costs 21.32 ns against 16.82 ns for an upvalue call — 27%, over the 25% threshold. **Landed in M5 as `-opt=3`**; functions still *live* in `F` (the chunk caps at 200 locals). The budget is not the ~120 the plan assumed: chunk-local headroom is what binds, and on the M4 guest that is **21 upvalues across 33 functions**. See [`agents/optimizer.md`](optimizer.md).
- Prefix everything user-visible with `fk_`.

**Non-issues, measured:** `load()` parses **4 MB / 45,220 functions in 106 ms** (~40 MB/s), so generated-chunk size is not a constraint worth designing around. Building a 262,144-entry table (a 1 MiB guest heap) takes 22 ms, and packing it into 16 × 64 KiB strings takes 21 ms — packing is not more expensive than building.

**…but one FUNCTION's size is, and the constraint is a jump.** See the next section: chunk size is free and a single function carrying a jump past ~131k VM instructions is not.

### A function that is too big for one jump

**Lua encodes a jump offset in 18 bits, so no jump may span more than `MAXARG_sBx` = 131,071 VM instructions.** The mechanic, the exact boundary and what the engine says when it fires are in [`agents/sandbox.md`](sandbox.md), "A jump is 18 bits". This section is what the emitter does about it.

`internal/luagen/funclimit.go` refuses such a module at package time, beside `checkChunkLocals` and for the same reason one limit over: without it the chunk compiles here and the *player's* game start reports `control structure too long`, naming neither the file nor the function. It is enforced inside `EmitModuleWith`, so `fklua compile` and `fklua mod` both carry it, and `mod` carries it for the data module as well as the control one.

**The metric is emitted BYTES between a `goto L<n>` and its `::L<n>::`, per function**, and every part of that is a decision:

| decision | why |
|---|---|
| per JUMP, not per function | a jumpless function is unbounded — measured at 140,998 instructions, loading |
| BYTES, not lines | a line is 1 to 49 instructions in this repo's own output; bytes per instruction is 5.6 to 8.0 over spans big enough to matter |
| read out of the emitted TEXT | a lowering that emits a goto is covered without knowing that file exists |
| both directions | Lua's test is `abs(offset)` |
| early-out on `len(src)` | a span cannot exceed the text it sits in, so nothing this repo emits is ever scanned |

**The floor the threshold converts through is measured, not assumed.** Every guest here was compiled at `-opt=0`, 2 and 3 in both languages, each chunk dumped under `bin/lua52f` and walked as Lua 5.2 undump output (`lundump.c`) for its per-function instruction counts and the true `sBx` offset of every jump in it. 2,713 emitted functions, 1,931 carrying a real jump:

| | |
|---|--:|
| bytes per instruction of span, over spans ≥ 10,000 instructions | 5.606 – 8.046 |
| bytes per instruction of span, over spans ≥ 1,000 instructions | min **5.606** |
| largest `sBx` span the goto/label scan cannot pair | **42 instructions** |
| widest span in this repo's own guests (`guest/rust ./examples/array`, `-opt=3`) | **248,744 B** |

**Five bytes per instruction ships**, so the threshold is 655,355 bytes. At the measured 5.606 floor that is 116,900 instructions, **10.8% inside Lua's limit**; the repo's own widest span is **38%** of the threshold. The margin leans towards refusing a module that would just have loaded rather than letting the cryptic in-game refusal through, and what it does refuse is the top ~11% of what Lua can represent — a guest in that band is one prototype away from not loading at all.

**What the scan does not pair, and what that is worth.** Three constructs carry an `sBx` jump the emitter never writes as a `goto`: a counted loop's `FORPREP`/`FORLOOP` over the `for` body, the implicit jump over a multi-line `if ... then ... end` (a `br_if` that copies a value, and a loop guard's seed), and a branch-table chain. All three are bounded by construction — a guard seed and a value copy are a handful of statements, and a counted loop is a wasm loop, which is wrapped in a block whose exit branch the scan *does* pair. Measured across those 2,713 functions, the largest `sBx` span in any function whose goto-to-label distance was under 200 bytes is **42 instructions**, three orders of magnitude below the limit.

**The remedy the message gives is `//go:noinline` in Go and `#[inline(never)]` in Rust**, on the boundaries the author already thinks of as sections. Proven on the reproduction: twenty section functions of sixteen prototypes each are inlined into one whose jump crosses 1,556,741 bytes and is refused, and the same source with the pragmas packages. **The size win reported downstream does not generalise, and the message says so**: that guest came out 27% smaller once its six sections stopped being inlined, and this reproduction goes 0.2% the *other* way. Both are real; it is a property of a particular guest's shape rather than a rule, and an error message that promised it would be wrong most of the time.

**Splitting the function in the emitter instead is designed and not built.** See the `function split` row in `CLAUDE.md`'s deliverables table for the shape, the costs and why it is a milestone rather than a follow-up.

### Every identifier the emitter emits, and why the table is exhaustive

Generated Lua has no symbol table and no shadowing diagnostic. **Two name families sharing a spelling is a miscompile, not an eyesore**: whichever is declared in the narrower scope silently wins every reference in it, and nothing — not the emitter, not Lua, not the conformance suite — says a word.

Each family is indexed by a counter of its own, and **no two of those counters are related**, so a shared prefix collides exactly when two unrelated numbers happen to meet. That is not a thing to leave to chance in a corpus.

| Family | Spelling | Indexed by | Scope |
|---|---|---|---|
| module global | `g0`, `g0h` (i64 high half) | module global index | chunk |
| promoted hot callee | `fu7` | function index | chunk |
| fixed emitter names | `F`, `MEM`, `MEMSIZE`, `SHBOUND`, `S1`, `BT`, `TBL`, `TSIG`, `IMPORTS`, `FS`, `FP`, `FUEL`, `exports` | — | chunk |
| runtime prelude | `ld32`, `st8b`, `MEMPACK`, `PE`, … | — | chunk |
| slot: param, local, operand stack | `v11` | slot number | function |
| spilled slot | `FS[fb+3]` | frame offset | (a table index, not a name) |
| frame base | `fb` | — | function |
| scratch register | `t0`–`t3` | — | function |
| **loop guard flag** | `lg41` | **loop header STEP index** | function |
| **loop guard word index** | `lw41_0` | **STEP index**, base | function |
| **loop guard shard table** | `ls41_0` | **STEP index**, base | function |
| counted-loop control variable | `fk41` | **STEP index** | the `for` |
| branch label | `::L3::` | label index | Lua's label namespace |

`MEMMAX` was in that fixed row and is gone. It held a compile-time constant read at exactly ONE site, the `memory.grow` lowering, which prints it as a numeral now — a chunk local spent for the life of every guest to save nothing at runtime. Its slot is what `SHBOUND` spends; see the budget section below.

**The last five are the dangerous ones**, because a step index is a dense small number counted per function and so collides with any other dense small number. The guard's names were `g41`/`w41_0` until 2026-08-01, and `g%d` is also a module global: a guarded loop whose header step index fell below the module's global count declared a *function-scoped* local named `g0`, which shadowed the global for the whole function — `global.set` wrote the flag, `global.get` read a boolean. In TinyGo output `g0` is the **shadow-stack pointer**. See [`agents/optimizer.md`](optimizer.md), "The guard's names were in the globals' namespace".

**The rule: a family indexed by a step index owns a prefix nothing else uses.** `lg`/`lw`/`ls` and `fk%d` are those prefixes. Adding a lowering that names something new means adding it to `nameFamilies` in `internal/luagen/loopguard_test.go`, which enumerates every family above over a generous range of every index and demands the sets be pairwise disjoint — a proof over every module rather than over the guests that happen to be checked in. Prefix-by-eye is not enough: `g` against `gh` would pass an eyeball and collide on an i64 global.

### The chunk-local budget is nearly spent

A chunk is a function, so Lua's **200-local cap applies to it**. The runtime prelude declares its own at chunk scope, and the emitter adds `F`, `MEM`, `MEMSIZE`, `SHBOUND`, `S1`, `BT`, `TBL`, `TSIG`, `IMPORTS`, `FS`, `FP` — **plus one local per global, two for an i64 global, and one per promoted upvalue at `-opt=3`**.

**SHARDING SPENT ONE, AND HERE IS THE WHOLE LEDGER**, because this was the risk `agents/sharding.md` listed first — the only thing in that milestone that could make a guest stop compiling, and it fails at `-opt=3` while still compiling at `-opt=2`.

| | |
|---|--:|
| `S1` — shard 0, bound directly; exactly the status `MEM` itself had | **+1** |
| `SHBOUND` — `min(MEMSIZE, 2097152)`, what every access opens on | **+1** |
| `MEMMAX` — a compile-time constant with ONE reader, now a numeral | **−1** |
| runtime prelude | **±0** |
| **net** | **+1** |

The prelude paid nothing, and that was a design constraint rather than luck. Every helper the sharded runtime wanted — the byte leaves, the piece splitters, `shof`, `rdw` — is either an existing name or lives inside a `do ... end` block, because `countChunkLocals` counts only column-zero `local` statements and an indented one is free. The four bulk functions moved into ONE shared block for exactly this: they declare the same four names they always did and get their shard helpers for nothing. Un-indenting any of it is not a tidy-up; it is a guest with many globals being refused at `-opt=3` for reasons nothing to do with the guest.

`TestAMemoryCostsTheChunkTheNamesShardingSaysItDoes` pins the +4 a memory costs the chunk (`MEM`, `MEMSIZE`, `SHBOUND`, `S1`) so that a prelude or an emitter which grows by a name moves a number in a test rather than moving the cliff for someone else.

**Measured: 26 mutable i32 globals fit; 27 do not.** `checkChunkLocals` refuses the module rather than letting Lua reject the chunk at the user's game start with "too many local variables" and nothing about which module caused it. `TestChunkLocalBudgetIsEnforcedWhereLuaEnforcesIt` pins that boundary against lua52f, so a prelude that grows moves the test rather than quietly moving the cliff.

**Nothing hits this today** — real TinyGo guests emit 0 or 1 global. Spilling the overflow into a table is the fix when something does, and it is not free: a global read becomes `OP_GETUPVAL` + `OP_GETTABLE` instead of `OP_GETUPVAL`, on values as hot as a guest's stack pointer. Measure before reaching for it, and spill only the overflow.

**Upvalue promotion is what makes the headroom scarce rather than theoretical.** It takes whatever is left after everything above, minus a margin of 4, so a guest with many globals simply gets fewer promoted callees rather than a compile error. `checkChunkLocals` is still the backstop.

**A function's 180-local budget is no longer a refusal.** Since M5 the coldest slots spill to the chunk-level `FS`/`FP` frame stack. That is a per-function limit, distinct from the chunk-level one above, and the two share nothing except that both come from Lua's 200.

**Every local added to the prelude costs a guest one global.** That is the trade to weigh before putting a helper there.

---

---

### Floating point

A Lua number **is** an IEEE-754 double, so f64 arithmetic is native and free. f32 is the work: every operation must round its result to single precision via `f32()`, or results drift from the spec. That rounding is arithmetic (Dekker split, `C = 2^29+1`) rather than `string.pack`-based, because `string.pack` allocates per operation and that GC pressure lands in a lockstep game loop.

`f32()` is validated differentially against Go's own `float32` conversion in `TestF32RoundingMatchesGo` — 3087 values, zero mismatches. **The random sweep is what earned its keep**: boundary cases alone missed that negative values rounding to zero were losing their sign.

**Negative zero is the recurring trap.** `x < 0.0` is *false* for `-0.0`, so any sign test must go through `negative(x)`, which checks `1/x < 0`. This bit `copysign`, `fmin`, `fmax`, `fceil`, `ftrunc` and `fnearest`.

---

### i64 — the width-aware slot model

An i64 is a `(lo, hi)` pair of unsigned doubles occupying **two** Lua slots, never a boxed table: boxing would cost an allocation and a metamethod dispatch per operation, which is GC pressure inside a lockstep game loop. Pairs cross function boundaries via Lua's native multiple return, so nothing needs packing.

**Every emitter path that touches a value must be width-aware.** Getting this wrong is the single most productive source of bugs in the project so far: one non-width-aware `local.get`/`set`/`tee` caused 389 spec failures in three unrelated-looking disguises — a nil half reaching a runtime helper, a `return` handing back nil, and `i64_eqz` never reporting zero, which turned a recursive factorial into unbounded recursion. Use `slotNames(base, typ)`, never `slotName(base)`, for anything that can be an i64.

Two subtler ones worth remembering:

- **A full 32×32 product overflows a double.** `alo * bhi` reaches 2⁶⁴ against a 53-bit mantissa, so `i64_mul`'s cross terms must go through the 16-bit split.
- **A multi-word store must bounds-check the whole width up front.** Two independent `st32` calls leave memory half-written when the second traps, and the spec says a trapping store changes nothing. `st64` checks 8 bytes once.

`f32.convert_i64_*` uses round-to-odd on the way through f64, so only one rounding remains; the naive `f32(i64_to_f64(x))` rounds twice.

---

### Two platform limits, distinct from unimplemented features

A Lua number cannot carry a NaN **sign bit** or a NaN **payload** — Lua canonicalizes NaN, and preserving either would mean boxing every float. So:

- `copysign` with a negative NaN is *unrepresentable*, not wrong. It is the only op where a NaN's sign reaches a non-NaN result.
- An i32 expectation whose bits are a non-canonical NaN (reinterpret, float loads) is likewise unrepresentable.

Both are reported as **skips with a reason**, never as passes. Keep that distinction: a limit we cannot fix and a feature we have not built must not look alike.

**They are also not silent.** `fklua compile` reports every NaN-sensitive operation, naming the function, the op, what is lost, and the remedy. Only *bit-observing* ops are reported — `copysign`, `reinterpret`, float load/store. Arithmetic and comparisons never observe a NaN's bits, so flagging them would be noise that trains people to ignore the output.

### `--nan=exact`

An opt-in mode that boxes a NaN carrying non-canonical bits into a table, so its sign and payload survive. **It is genuinely slow**: an operand may now be a table, so no float operation can use a plain Lua operator — every one routes through an `x`-prefixed helper with a type check. Do not make it the default.

Arithmetic deliberately does **not** propagate boxes. The spec permits any payload from any operation, so an arithmetic op with a boxed operand returns a plain canonical NaN. Boxes only need to survive the paths where bits are read or written, which keeps the slow path narrower than it looks.

Validated by running the whole suite in both modes: canonical 13031 passing, exact 13202. The 171 difference is exactly the assertions canonical mode must skip. **Re-run `fklua spectest --nan=exact` after touching any float lowering** — it is the only evidence the boxing is still correct.

---

## Diagnostics are filtered by reachability

`Diagnose` only reports NaN-sensitive operations in functions reachable from an export or the start function, following direct calls and — conservatively — every element-segment entry once a module contains a `call_indirect`.

Each diagnostic **names the entry point that reaches it** — "reached from export `fk_on_tick`" rather than naming a function the author never wrote.

**TinyGo exports its libm.** The M4 hello-world's export list is `fminimum`, `fminimumf`, `fmaximum`, `fmaximumf`, `_initialize`, `fk_on_init`, `fk_on_tick`. So `fmaximumf` was a reachability *root*, not dead code and not dragged in by the `call_indirect` rule — which is why filtering by reachability alone changed nothing.

`Options.Roots` fixes it properly. A mod's `control.lua` wires only `factorio.Hooks`, so `fklua mod` passes exactly those and TinyGo's libm stops being an entry point. A bare `fklua compile` keeps every export, because an arbitrary host genuinely could call `fmaximumf` — and its message now says so.

Do not silence this by suppressing the diagnostic or by name-matching runtime functions. If a guest's own code reaches a NaN-sensitive op, the author needs to know.

---

## `--fuel` — the one limit with no upper bound

wasm has no instruction budget and a conforming module may loop forever. Factorio enforces none either, and a mod cannot be interrupted, so an infinite guest loop hangs every player's client until they kill the process. `--fuel=N` charges one unit per loop iteration and traps when the budget runs out.

Charged at the **loop header**, not at each back edge: every iteration passes through the header exactly once, and a loop with four `continue`s would otherwise carry four copies of the check. Entering a loop also charges one, which is off by one per loop and not worth a branch to avoid.

```lua
::L0::
FUEL = FUEL - 1 if FUEL < 0 then trap_fuel() end
```

The budget is refilled at **every guest entry point**, not once per session. A mod given one budget for the whole game would run fine for an hour and then start trapping in a handler that had not changed, which is worse than not having the check.

### It is off by default, and the plan said otherwise

The plan estimated 5–10% and recommended defaulting it on. **Measured, it is up to 98%** — `bench/wasm` kernels at `-opt=2`, best of 5 under `bin/lua52f`, the same module emitted with `Fuel: 0` and `Fuel: 1<<30`:

| kernel | off | on | |
|---|---|---|---|
| `count` — a bare counted loop | 39.7 ms | 78.7 ms | **1.98×** |
| `sum` | 530.5 ms | 643.2 ms | 1.21× |
| `prng` | 276.5 ms | 319.8 ms | 1.16× |
| `chase` | 198.0 ms | 216.0 ms | 1.09× |
| `fib` — recursion, no loop | 75.9 ms | 75.3 ms | 0.99× |

The cost is intrinsic to checking per iteration: `count`'s loop body is two VM instructions and the check adds four, so tripling the op count roughly doubles the time. `fib` is free because recursion has no loop header — runaway recursion is caught by Lua's own stack limit instead.

Doubling loop-heavy code contradicts the project's central claim of landing within a small constant of hand-written Lua, and the failure it prevents is a guest bug the author can find in testing. So it is opt-in, and prominently documented for anyone shipping to other people.

**How it could become the default.** Charge a bounded loop's whole trip count *once*, before it runs, instead of per iteration — the M5a range fixpoint already proves the counter's bounds for exactly the counted-loop shape that costs the most here. Loops it cannot bound keep the per-iteration check. Not built, and it is not free to get right: a bounded counter is not the same as a bounded trip count, since the body may reassign it.

---

## The chunk-local budget

Lua caps a function at **200 locals**, and a chunk is a function. The prelude is inlined into every generated chunk, so what it declares at column zero is taken out of every guest's budget before the guest gets any. On top of it the emitter adds `F`, `MEM`, `MEMSIZE`, `MEMMAX`, `BT`, `TBL`, `TSIG`, `IMPORTS`, `FS`, `FP`, `FUEL`, one name per global (two per i64 global), and one per promoted upvalue at `-opt=3`.

`countChunkLocals` counts only **column-zero** `local` statements, because anything declared inside an indented `do ... end` block is not live at chunk level and costs the chunk nothing. That is the lever, and M6a used it.

**Measured on the M4 guest**, which is the only thing in the repo with enough functions for the budget to bind:

| | before M6a | after |
|---|---|---|
| `-opt=2` (no promotion) | 182 / 200 | **167 / 200** |
| `-opt=3` | 197 / 200 | **192 / 200** |
| functions promoted | 15 of 33 | **25 of 33** |

What was reclaimed, and why each is free or better:

- **Nine trap sentinels → one `TRAPS` table.** Saves 8 names for one extra `OP_GETTABLE`, paid only on the trap path, which is terminal and never hot.
- **`MAGIC`, `INV65536`, four `F32_*` → `do ... end` blocks.** Saves 6. A block-scoped local is not a chunk local; the functions using them are forward-declared at chunk level and assigned inside. Nothing about the emitted code changes — the emitter never referenced any of these names.
- **`F32_MAX` deleted.** It was defined and never read.

**Do not "simplify" a prelude `do ... end` block away.** The indentation is load-bearing: un-indenting its contents puts those names back on the chunk's budget, and the symptom is a guest with many globals being refused at `-opt=3` for reasons that have nothing to do with the guest.

`-opt=3` still is not the default, and reclamation is not what is blocking it any more — see the carried-forward table in `CLAUDE.md`.
