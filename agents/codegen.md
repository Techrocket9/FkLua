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

`internal/luagen/funclimit.go` refuses such a module at package time, beside `checkChunkLocals` and for the same reason one limit over: without it the chunk compiles here and the *player's* game start reports `control structure too long` against a generated file and line, naming nothing in the guest. It is enforced inside `EmitModuleWith`, so `fklua compile` and `fklua mod` both carry it, and `mod` carries it for the data module as well as the control one.

**The metric is emitted BYTES between a `goto L<n>` and its `::L<n>::`, per function**, and every part of that is a decision:

| decision | why |
|---|---|
| per JUMP, not per function | a jumpless function is unbounded — measured at 140,998 instructions, loading |
| BYTES, not lines | a line is 1 to 49 instructions in this repo's own output; bytes per instruction is 5.6 to 8.0 over spans big enough to matter |
| read out of the emitted TEXT | a lowering that emits a goto is covered without knowing that file exists |
| both directions | Lua's test is `abs(offset)` |
| EVERY function scanned, no early-out on `len(src)` | the scan used to skip a function shorter than the threshold, on the sound argument that a span cannot exceed the text it sits in. That rule is why nothing could report a distance: a number that only exists once it has been crossed is one nobody can steer by, and the headroom figure below cannot be computed under it. The cost of dropping it is measured, not asserted: `fklua mod --data-module` over the two FkRecipes example data guests, seven runs each, median wall clock, 0.47 s to 0.48 s on the Go one (147 functions) and 0.36 s to 0.36 s on the Rust one (254) |

**The floor the threshold converts through is measured, not assumed.** Every guest here was compiled at `-opt=0`, 2 and 3 in both languages, each chunk dumped under `bin/lua52f` and walked as Lua 5.2 undump output (`lundump.c`) for its per-function instruction counts and the true `sBx` offset of every jump in it. 2,713 emitted functions, 1,931 carrying a real jump:

| | |
|---|--:|
| bytes per instruction of span, over spans ≥ 10,000 instructions | 5.606 – 8.046 |
| bytes per instruction of span, over spans ≥ 1,000 instructions | min **5.606** |
| largest `sBx` span the goto/label scan cannot pair | **42 instructions** |
| widest span in this repo's own guests (`guest/rust ./examples/array`, `-opt=3`) | **248,744 B** |

**Five bytes per instruction ships**, so the threshold is 655,355 bytes. At the measured 5.606 floor that is 116,900 instructions, **10.8% inside Lua's limit**; the repo's own widest span is **38%** of the threshold. The margin leans towards refusing a module that would just have loaded rather than letting the cryptic in-game refusal through, and what it does refuse is the top ~11% of what Lua can represent — a guest in that band is one prototype away from not loading at all.

**What the scan does not pair, and what that is worth.** Three constructs carry an `sBx` jump the emitter never writes as a `goto`: a counted loop's `FORPREP`/`FORLOOP` over the `for` body, the implicit jump over a multi-line `if ... then ... end` (a `br_if` that copies a value, and a loop guard's seed), and a branch-table chain. All three are bounded by construction — a guard seed and a value copy are a handful of statements, and a counted loop is a wasm loop, which is wrapped in a block whose exit branch the scan *does* pair. Measured across those 2,713 functions, the largest `sBx` span in any function whose goto-to-label distance was under 200 bytes is **42 instructions**, three orders of magnitude below the limit.

**AND WHAT IT DID NOT PAIR UNTIL 2026-08-30 IS A CHAIN, WHICH IS A DIFFERENT KIND OF MISS.** `::A::` immediately followed by a BARE `goto B` — nothing between the label and the jump — does not produce two independent jumps. `luaK_patchtohere` puts the jumps pending at A into `fs->jpc`; the very next instruction emitted is the relaying jump, and `luaK_jump` begins by taking `fs->jpc` and `luaK_concat`ing it onto the new jump, so the two lists MERGE and every jump in them is patched to B's eventual target. A ladder `A → B → C → L` is therefore ONE jump from every entry point straight to `L`, and Lua measures it that way. Measured under `bin/lua52f` (`scratchpad/r4/RESULTS-jumplimit.txt`), every hop 50,000 instructions against a limit of 131,071:

| | |
|---|---|
| bare ladder, 2 hops × 50,000 | loads, returns 100,000 |
| bare ladder, 4 hops × 50,000 | **refused** — control structure too long |
| bare ladder, 10 hops × 50,000 | **refused** |
| **separated** ladder, 10 hops × 50,000 | **loads**, returns 500,000 |

Every goto-to-label distance in all four programs is 50,003 bytes, so the pre-fix scan measured one hop and passed the three refused ones. `maxJumpSpan` resolves the chain now — `labelIndex` records what each label relays to, looking past blank lines, comments and further labels because none of them emits an instruction, and `resolveChain` follows it with a visited set so `::A:: goto A` terminates. **The blind spot was latent and not live**: three real guests at `-opt=0`, 2 and 3 emit zero instances of that shape. It stopped being optional the moment the relay below started emitting it deliberately.

### The relay — a too-long jump becomes a ladder of short ones

**Since 2026-08-30 an over-long jump is RELAYED before anything is refused**, and only what cannot be relayed is refused. `relayOrRefuse` in the same file is the shipping path: check, and on failure relay, re-check and accept, and on failure again return the original error.

The transform inserts, at function-body level, a station of five lines and points the original `goto` at it:

```lua
  goto LTs3            -- the guard: the straight-line path steps over the station
  ::LT3::              -- the trampoline the previous hop lands on
  if 0 ~= 0 then end   -- the separating statement
  goto LT4             -- the next hop, or the original label
  ::LTs3::
```

Both extra lines are load-bearing and neither is decoration. **The separator is what makes the ladder legal rather than a workaround** — without it the hops chain and the whole ladder is patched as one jump, which is the finding above; `0 ~= 0` is chosen because it names nothing (a guest function may have no locals, no memory, no globals and no scratch registers) and because Lua 5.2 does not fold it: `luaK_posfix` sends `OPR_NE` to `codecomp` and `constfolding` is reached only from `codearith`, so it really does emit an `OP_EQ` and `luaK_code` discharges the pending list on the way past. **The guard is what stops control falling into the station**, which on the dominant shape would take the jump to the trap; `TestATrampolineIsNeverFallenInto` asserts it as a TEXT property, because a missing guard costs an answer on one fixture and is invisible on every fixture whose straight-line path happens not to reach that far.

| decision | why |
|---|---|
| runs on the EMITTED TEXT, where the check already runs | renumbers nothing, so `analysis.Plan`, `Wrap`, `Align`, the counted-loop recogniser, the loop guards and every `lg`/`lw`/`fk` name spelled from a step index are untouched by construction |
| relay, not outline | the recorded outlining design split at a point where the operand AND control stacks are empty, and **that point is by construction outside every jump's span** — a wasm branch targets an ENCLOSING construct, so control depth is ≥ 1 at every step strictly inside a span. Measured on the reproduction: 4 such points, all four the prologue and the tail |
| hops at HALF the threshold | stations are planned against the ORIGINAL offsets and each insertion pushes what follows along by ~80 bytes, so the emitted hops are a little wider than the planned ones. The slack is four orders of magnitude more than that drift can use, and `relayOrRefuse` re-measures regardless |
| stations only at DEPTH 1, never before a block closer, never inside the prologue | a `goto` may leave a block but never enter one, so a body-level label is the only one every jump can reach; inserting before `end`/`else`/`elseif`/`until` would put the station inside the block being closed; and a station between two `local`s would relay a jump INTO a local's scope, which is Invariant B seen from the other side and which Lua rejects outright |
| both directions | Lua's test is `abs(offset)`, so a loop back edge over an enormous body gets the same ladder with the sign flipped |
| plan in order of first appearance, greedy over line offsets | the transform is a pure function of the emitted text, so two builds of one module produce the same bytes |

**Measured.** One fixture emitted twice: at a raised threshold it is 2,300,141 bytes and Lua refuses it in its own words; at the shipped threshold it is relayed to a widest span of 327,706 bytes, loads, and **returns the same answer the direct one does** (`TestARelayedJumpLoadsWhereTheDirectOneDoesNot`, `TestARelayedFunctionComputesTheSameAnswer`). **It is a byte-for-byte no-op on everything this repo emits** — 15 guests at `-opt=3`, 1,979 function texts, nothing changed — which is what says the feature costs the ordinary path nothing (`TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay`).

**And a no-op audit is a statement about the threshold comparison, not about the transform**, so the threshold is also driven DOWN until the relay fires on the whole corpus and every result is handed to the real parser: **19 emissions, 15 relayed, 1,227 stations, every one parses** (`TestARelayedRealGuestStillParses`). That is the only place the three insertion-point rules meet the shapes they were written for — nested `if`, `do return end`, branch tables, loop guards, spilled frames — because every fixture that fires the relay directly is a four-line hand-written WAT. Red-proven: remove the block-closer rule and eleven guests emit Lua that does not parse, while nothing else in the suite notices. It asks whether the chunk PARSES and not whether it runs, since a guest chunk calls `fk_import` for host functions no host-side harness supplies; reaching that call is proof the whole chunk compiled, and the first draft of the test reported all twelve guests broken for missing it.

**What it does NOT solve, said plainly, AND IT IS THE REAL CEILING RATHER THAN A FOOTNOTE.** A run longer than ONE HOP with no body-level statement in it to hang a station on. The ladder relays a jump; it cannot shorten a distance the intervening code genuinely has to cover. This used to read "a single basic block longer than the limit", as a shape that had never been observed, and both halves were understated. The condition is against the HOP (327,677 bytes) and not against the limit, and the shape is not exotic: a counted loop with an early exit whose body the optimizer has made large is exactly it, because the emitter writes a `for` body at depth 2 and the label that exit reaches at depth 1.

**Measured on that shape, built from real wasm rather than hand-written Lua.** A loop with a `br_if` out of it and 30,000 straight-line statements in its body emits a 990,047-byte span whose widest block is **990,038 bytes**: the only body-level insertion point strictly inside the span is the `::L1::` between the loop's `end` and the label the jump reaches, nine bytes from the far end. Room = 327,677 - 990,038 = **-662,361**, and the function is refused. `TestASpanInsideOneBlockIsRefusedWithTheDistanceThatRefusedIt` builds it, asserts the goto is deeper than body level and its label is at body level so the fixture is the nested shape rather than merely a long one, and requires the refusal to carry both numbers.

**AND THE SHAPE REACHES THE ENGINE'S OWN REFUSAL, measured rather than argued.** The same fixture packaged with the threshold raised (the `TestWhatTheCheckRefusesIsWhatLuaRefuses` trick) and run under a real Factorio 2.0.77 (build 84539, mac-arm64, steam) with `--dump-data` and a private `FACTORIO_USERDIR`:

| filler statements | span | engine |
|--:|--:|---|
| 30,000 | 990,047 B | loads, `Checksum of fk-blockjump: 416670568` |
| 60,000 | 1,980,047 B | loads |
| 90,000 | 2,970,047 B | **refused**, exit 1 |

The run so it can be repeated. The binary is re-asked its version first, because a number attributed to an engine that was not the engine is worse than no number:

```sh
"$FACTORIO" --version                       # Version: 2.0.77 (build 84539, mac-arm64, steam)
bin/fklua mod --data-module blockjump.wasm --name fk-blockjump --version 0.1.0 \
  --author Techrocket9 --factorio-version 2.0 -o MODS   # with maxJumpSpanBytes raised
FACTORIO_USERDIR=/tmp/fk-r5-relay "$FACTORIO" -c /tmp/fk-r5-relay/config/config.ini \
  --mod-directory MODS --dump-data
```

one packaging and one dump per filler count. The private user dir is the convention every `scripts/run-*.sh` here follows and it is load-bearing rather than tidy: the default one is locked by a running game, which kills the run rather than failing it.

The refusal is verbatim `Failed to load mod "fk-blockjump": __fk-blockjump__/fk_data.lua:93: __fk-blockjump__/fk_data_module.lua:92594: control structure too long near 'end'`, which is the same failure the whole check exists to move to package time, now shown for the BLOCK shape rather than only for the flat one. Two things follow. The bracket puts this shape at **15.1 to 22.7 bytes per instruction of span** (1,980,047 / 131,071 and 2,970,047 / 131,071), well above the 5.606 floor the threshold converts through, so the 990 KB fixture the unit tests use is genuinely in the band the threshold refuses and Lua accepts: it tests the CHECK, and the engine run is what says the shape reaches Lua at all. And the token is `near 'end'` here rather than `near 'trap_unreachable'`, for the same reason: it is whatever follows the label the pending gotos were patched to, and for this shape that is the `end` of a `for`.

**THE BLOCK IS THE RELAY'S PRECONDITION EXACTLY, which is a theorem rather than a heuristic.** `relayPlan` cuts the widest span at every insertion point strictly inside it; the widest of the resulting pieces (goto to first point, point to point, last point to label) is the WIDEST BLOCK. The greedy walk succeeds if and only if that block is at most one hop: if every piece fits in a hop then every step finds a point, and if a piece `p_i..p_{i+1}` does not, the walk cannot pass `p_i` and stalls with the label still more than a hop away. The plan and the measurement are ONE function so the two cannot disagree, and `TestTheBlockMeasureIsTheRelaysOwnCeiling` requires `ok` and `block <= hop` to agree over 252 span/point/hop combinations, two of whose point sets put a point exactly ON a span end, which is the boundary the `inside` filter decides. Sortedness of the point set is a PRECONDITION of `relayPlan` rather than something it checks, met by construction because `collectInsertionPoints` appends offsets while walking the lines in order; it is stated in the doc comment rather than asserted with an unsorted case no caller can produce. Red-proven by measuring every piece from the start of the span instead of from the previous point, which reports the whole span as the block and fails at the first evenly-spaced case.

### What the relay cost, reported rather than only enforced

**Every emitted function is measured now, not only the ones over the threshold.** `fklua mod --report` carries the result per module as a `jumps` object (`limit_bytes`, `hop_bytes`, `widest_span_bytes` and `widest_span_function`, `stations`, `widest_span_after_relay_bytes`, `widest_block_bytes`, `block_room_bytes`, `relayed`, `relayed_functions`), and `fklua mod` and `fklua compile` print the same facts on stdout under the line naming the module. **The same facts on BOTH arms**: a module the relay did not have to carry gets the block and the room too, and a second advisory line when that room is negative, because "no relay was needed today" and "the relay could have carried this" are independent statements about a module. The blocky fixture at 15,000 filler statements is the case that separates them: 495,047 bytes of span, 76% of the limit, no relay, and **-167,361 bytes of block room** already. `TestAModuleUnderTheSpanLimitStillSaysWhatTheRelayCannotCarry` is that fixture and `TestCompilePrintsTheJumpNoteUnderItsWroteLine` is `compile`'s arm, which pins the `wrote` line above the note as a whole string rather than by `Contains`.

**A REFUSAL NAMES THE SPAN THE RELAY FAILED ON, WHICH IS NOT ALWAYS THE WIDEST ONE.** `relayJumps` plans every label independently, so a function whose widest jump is carried can still be refused for a narrower one, and the error used to be built from `spanOver`'s widest regardless: on a fixture with a 1,204,010-byte body-level span beside a 720,018-byte one inside a `while` body, the message named the carried label, quoted a block of 28 bytes and offered **+327,649 bytes of room** as the reason a function would not fit. Every number in it was about a jump that worked. `worstUnrelayableSpan` re-derives the refusal from the spans `relayPlan` actually failed on, through the same `gotosByLabel` grouping the relay plans with, so the room is negative on every refusal; `widest_block_bytes` follows it, while `widest_span_bytes` stays the widest span because that is a true fact about the function and the number an author steers by. A label below body level needs no separate arm: Lua forbids a goto that ENTERS a block, so such a span contains no body-level insertion point at all and the walk fails on it like any other. `TestTheRefusalNamesTheSpanTheRelayCouldNotCarry` is the two-span fixture.

**The same choice one level up.** A module's `jumps` row is its WIDEST function while it packages, and the REFUSED function on a refusal, whatever that function's span. The two differ for the same reason: a module with an 1,120,046-byte body-level span in one function and a 990,047-byte nested one in another is refused for the second, and the row used to be the first's, reporting **+327,647 bytes of block room** for a build that had just stopped. `TestARefusedModulesJumpRowIsTheRefusedFunctions` packages both shapes in one module with the relayable one first and wider, which is the only arrangement in which the two are different functions. `EmitModuleJumps` is the entry point; `EmitModule`, `EmitModuleWith` and `EmitModuleSpans` are unchanged for their ~seventy call sites. The cost of dropping the `len(src) <= threshold` early-out is MEASURED rather than assumed: `fklua mod --data-module` over the two FkRecipes example data guests, seven runs each, median wall clock **0.47 s to 0.48 s** on the Go one (min 0.46 to 0.48) and **0.36 s to 0.36 s** on the Rust one (min 0.34 to 0.35). An earlier round of the same shape on a busier machine read 0.53 to 0.54 and 0.35 to 0.37, so the DELTA is one to two hundredths of a second in both rounds where the absolute figures are not stable.

**Why a number nobody could read was a defect.** The limit was enforced and never reported, so an author learned the distance only by crossing it. FkRecipes transcribed this file's scan into a script of its own to find out where it stood, and the number that WAS visible was the wrong one: after a relay the widest span is about one hop whatever the plan's size, so it reads 50% of the limit for a module 161% over it, and the pilot mod quoted "83 percent of the limit" from what was really the hop. That is the reading the stdout line is worded to make impossible.

**The measured instance, both languages, on FkRecipes master c7a806e**, packaged with `fklua mod --data-module W --name fkrecipes-example --version 0.1.0 --author Techrocket9 --factorio-version 2.0 -o DIR --report R.json`:

| | Go | Rust |
|---|--:|--:|
| emitted data module | 3,535,966 B | 3,106,966 B |
| functions measured | 147 | 254 |
| widest span BEFORE the relay | **958,365 B** (146% of the limit) | **562,908 B** (86%) |
| the function carrying it | `(*github.com/Techrocket9/fkrecipes/go.Lib).PlanData` | `_RNvMNtCsg99gZ7im5TJ_9fkrecipes4dataNtNtB4_4plan3Lib9plan_data` |
| stations, functions relayed | 11, 1 | **0, 0** (no relay needed) |
| widest span AFTER the relay | 328,167 B (50% of the limit) | 562,908 B (86%, untouched) |
| **widest block** | **802 B** | **461 B** |
| **block room** (hop 327,677) | **326,875 B** (100%) | **327,216 B** (100%) |

Those are the numbers AFTER the dirty-page barrier gate (`luagen: the dirty-page barrier is emitted only in a mode that can arm it`, same round), which took 9.7% off the Go module and 15.0% off the Rust one and took the Rust module out of the relay altogether. Measured the same way BEFORE it, on the same wasms: Go 3,915,886 B of Lua, widest span **1,051,960 B** (161%), 13 stations, 328,256 B after, block 802 B, room 326,875; Rust 3,655,976 B, **668,851 B** (102%), 7 stations, 327,957 B after, block 607 B, room 327,070. Both are miles from the ceiling that matters: the widest nested run in either is under a kilobyte against a hop of 327 KB, so what the Go guest is over and the Rust guest is near is the SPAN, which the relay handles, and not the block, which it cannot. The pre-gate span figures confirm FkRecipes' finding and correct it in two places. Go's 1,052,862 is the same span measured in the POST-relay text, where the 13 stations have already added 851 bytes plus the rewritten gotos; the pre-relay distance is 1,051,960. Rust's 664,632 does not reconcile with 668,851 by any station arithmetic and is presumably a different build of the same source; the station count agrees at 7. **The emitted `fk_data_module.lua` is byte-identical before and after this round on both guests** (3,916,145 B with 26 `LT` labels in Go, 3,656,235 B with 14 in Rust, `cmp` clean), which is what says the measurement changed nothing about the output.

**The remedy the message gives is `//go:noinline` in Go and `#[inline(never)]` in Rust**, on the boundaries the author already thinks of as sections, and it is now what is left when the relay could not help rather than the first answer. **The message LEADS with the block against the hop** rather than with the span, because since the relay the span is no longer the reason anything is refused, and the remedy has a second half it could not have had before: nesting is what costs the author, not length. A long straight-line run at function body level is fine at any size, since it has a statement boundary every few bytes; the same run inside one `if` or one loop body is what stops packaging. Proven on the reproduction: twenty section functions of sixteen prototypes each are inlined into one whose jump crosses 1,556,741 bytes and is refused, and the same source with the pragmas packages. **The size win reported downstream does not generalise, and the message says so**: that guest came out 27% smaller once its six sections stopped being inlined, and this reproduction goes 0.2% the *other* way. Both are real; it is a property of a particular guest's shape rather than a rule, and an error message that promised it would be wrong most of the time.

`JumpSpanError` is a TYPE rather than an `fmt.Errorf` string, because `relayOrRefuse` has to tell "this function would have been refused, try the relay" apart from any other failure, and a caller further out may want the same distinction without matching on prose.

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
| **relay trampoline label** | `::LT3::` | **per-function TRAMPOLINE index** | Lua's label namespace |
| **relay skip label** | `::LTs3::` | **per-function TRAMPOLINE index** | Lua's label namespace |

`MEMMAX` was in that fixed row and is gone. It held a compile-time constant read at exactly ONE site, the `memory.grow` lowering, which prints it as a numeral now — a chunk local spent for the life of every guest to save nothing at runtime. Its slot is what `SHBOUND` spends; see the budget section below.

**The last seven are the dangerous ones**, because a step index — and a trampoline index, and a label index — is a dense small number counted per function and so collides with any other dense small number. `LT3` against the branch label `L3` is one character away and `LTs3` against the guard's `ls3_0` is one case change away, which is why the relay's two families were added to `nameFamilies` before the first one was emitted rather than after the first collision. The guard's names were `g41`/`w41_0` until 2026-08-01, and `g%d` is also a module global: a guarded loop whose header step index fell below the module's global count declared a *function-scoped* local named `g0`, which shadowed the global for the whole function — `global.set` wrote the flag, `global.get` read a boolean. In TinyGo output `g0` is the **shadow-stack pointer**. See [`agents/optimizer.md`](optimizer.md), "The guard's names were in the globals' namespace".

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
