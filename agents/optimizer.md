# The optimizer

What `-opt` does, what each pass assumes, and what each one measured. Read `CLAUDE.md` first — the two invariants constrain every pass here — and [`agents/codegen.md`](codegen.md) for the lowerings the passes rearrange.

The analyses live in `internal/analysis` and answer questions; they never rewrite IR. `internal/luagen` acts on the answers. That split is what makes `-opt=0` a genuine reference rather than an approximation of one: at level 0 the emitter simply never asks.

---

## The levels

| Level | Adds | Assumes |
|---|---|---|
| `0` | nothing | — |
| `1` | the peephole — expression forwarding, wrap deferral, comparison folding, biased signed compares, constant-divisor division — over a whole-function range fixpoint, plus the counted-loop `for` | nothing beyond the wasm spec |
| `2` **(default)** | typed-slot promotion | the module respects its own shadow-stack convention |
| `3` | upvalue promotion | nothing, but spends scarce chunk-level locals |

**`-opt=0` must stay byte-for-byte the M4 emitter.** `TestLevelZeroKeepsTheM4Lowerings` and `TestSignedCompareKeepsItsScratchFormAtLevelZero` pin it. It is what a miscompile gets bisected against.

**The conformance suite has to be green at every level**, not just the default. It is the only thing that will tell you an optimization traded correctness for speed, and every level exercises different code:

```sh
for L in 0 1 2 3; do ./bin/fklua spectest --opt=$L; done
./bin/fklua spectest --opt=3 --nan=exact
```

---

## Measuring

`./bin/fklua bench` **cannot see any of this.** The M0 kernels are hand-written Lua standing in for generated code: they establish the ceiling, and they do not move when the emitter improves. A pass that halves the work in a generated loop shows up in them as exactly nothing.

`./bin/fklua bench --opt` compiles `bench/wasm/*.wat` with the real compiler at every level and times the result. **That is the number to quote.** Kernels mirror the M0 set so the two are talking about the same programs.

Measured on this machine, ratios against `-opt=0`, lower is faster:

| kernel | opt0 ns/op | opt1 | opt2 | opt3 | what moved |
|---|---|---|---|---|---|
| `sum` | 65.88 | **0.75×** | 0.75× | 0.75× | address arithmetic collapses into the load; the loop test folds into its branch; the counter's increment loses its wrap |
| `chase` | 107.50 | 0.96× | 0.90× | 0.91× | dominated by `ld32` — the CALL, not the check |
| `prng` | 79.90 | 0.88× | 0.88× | 0.89× | shift/xor chains become one expression |
| `dot` | 343.85 | 0.93× | 0.92× | 0.92× | dominated by `ld_f64`, which nothing here touches |
| `fib` | 23.27 | 0.77× | 0.75× | **0.63×** | upvalue promotion is the last 12% |
| `frame` | 759.22 | 0.98× | **0.01×** | 0.01× | typed-slot promotion, where it applies |
| `count` | 31.04 | **0.31×** | 0.31× | 0.31× | the loop-header fixpoint: a direct signed compare and no wrap on the increment |
| `constdiv` | — | — | — | — | **never measured.** Added with the constant-divisor lowering; the number has to come from a quiet machine |

`opt1` and `opt2` are byte-identical for every kernel but `frame`, so any difference between those two columns is measurement noise, not a pass.

Generated Lua for the M4 guest also shrank from 4,403 lines to 3,703 (16%).

---

## Pass 1 — the peephole (`-opt=1`)

The rewriting is block-local: nothing is forwarded across a label, because a label can be entered from anywhere. The **range analysis underneath it is not**, since M5a — it solves wasm-local ranges over the whole function, which is what lets a loop guard reach the code it dominates. Both halves are below.

### Expression forwarding

M4 forwarded a `local.get` or an `i32.const` into its consumer. Level 1 forwards **any step that lowers to a single expression**, which turns a straight-line run of stack operations into one Lua expression:

```lua
v4 = v2 >= v1 and 1 or 0                 if v2 >= v1 then goto L0 end
if v4 ~= 0 then goto L0 end              v3 = (v3 + ld32(MEM, MEMSIZE,
v6 = (v2 * 4) % 4294967296.0      -->          (v0 + v2 * 4) % 4294967296.0
v5 = (v0 + v6) % 4294967296.0                )) % 4294967296.0
v5 = ld32(MEM, MEMSIZE, v5)              v2 = (v2 + 1) % 4294967296.0
v4 = (v3 + v5) % 4294967296.0
v3 = v4
v4 = (v2 + 1) % 4294967296.0
v2 = v4
```

Ten statements become four. It is sound because a wasm operand-stack slot is written once and read once — but **every hazard is about what happens between the two**, and each one below was a real failure the conformance suite caught:

| Hazard | Rule |
|---|---|
| The expression reads something a later step writes | Each pending carries its slot, local, global and memory dependencies, and dies when any is written |
| Two trapping operations in one expression | At most one. Lua does not fix operand evaluation order, so *which* trap fires would depend on something the language does not promise |
| An operand a lowering names more than once | `duplicatesOperand` refuses a composite there. `f64.abs` names its operand four times |
| An operand a lowering may not evaluate at all | `mayNotEvaluate`: `drop`, `select`'s value arms, `and 0`, `or 0xFFFFFFFF`. **`(drop (i32.div_u 1 0))` must still trap** — the suite's `no_dce` tests exist for exactly this |
| A basic-block boundary | Everything is flushed. A label can be entered from anywhere |

Nesting is capped at 8 (`maxFwdDepth`): Lua's parser is recursive with `LUAI_MAXCCALLS = 200`, and every level also costs a VM register on top of the up-to-180 locals a function declares against a 255-register ceiling.

### A constant-folded operand must still be evaluated

**Fixed at M5a. Present from M5 through `v0.6.0`, at the default level.** Kept here because the shape of the mistake is going to recur: the fold and the forward are two passes that each behave correctly and are wrong together.

`mayNotEvaluate` is what stops the forwarder deleting a step whose expression the consumer will not print. It listed `drop`, `select`'s value arms, and `and`/`or`'s position 0 — and missed the class those last two belong to. **Every constant-specialised lowering discards operand 1's expression**, because it prints `u32(k)` in its place: `i32.mul` by a small constant, all four shifts and rotates, and each of `and`/`or`/`xor`'s identity cases.

The trap comes from where the constant comes from. At `-opt=0` it is an `i32.const` that was forwarded, so a trapping operand can never be one. At `-opt>=1` `constOf` reads **the range analysis** instead, and a trapping operand can have an exact range:

```wat
(i32.mul (i32.const 7) (i32.div_u (i32.const 0) (local.get $z)))
```

`div_u`'s range is `[0,0]` because its dividend is. So the multiply took its constant path, never named the divide, and the forwarder — seeing the only use gone — deleted it. `f(0)` returned `0` where the spec requires a trap. `(i32.and (i32.const 5) …)` was `return 0`, same cause.

The suite's `no_dce` tests miss it because they go through `drop`, which `mayNotEvaluate` already covered. `TestAConstantFoldedOperandStillTraps` covers all nine ops.

The fix refuses the forward, and refuses it only for an operand that can trap. That costs one statement on a path that is already rare, and **nothing on the common one** — the constant is still folded, because the fold reads the range and never read the expression:

```lua
v2 = div_u(0, v0)     -- its own statement again, and it traps
return 7 * 0          -- still specialised
```

**The general rule, worth stating because it outlives this bug: an operand position that a lowering may replace with a range-derived constant belongs in `mayNotEvaluate`.** Adding a constant specialisation to `expr.go` without adding its operand position there reintroduces exactly this.

**It recurred, exactly as predicted, when the constant-divisor lowering was added** — see below. That one is the sharpest instance of the class, because the operand being discarded is a *divisor*, and the expression most likely to be sitting in a divisor position is another division: `div_u`'s range is `[0,0]` whenever its dividend is, whatever its own divisor does. The guard was in place from the start there, and `TestAConstantFoldedDivisorStillTraps` was confirmed to report `25` instead of a trap with it removed.

**`--` starts a Lua comment.** Substituting the literal `-0.0` into the `f64.neg` lowering produced `--0.0`, which swallowed the rest of the line and failed to parse a long way from anything to do with floats. A negative numeric literal is a unary minus applied to a numeral, not a primary expression, and `numLit` marks it so.

### Comparison folding

A comparison whose consumer is a branch never becomes 0 or 1 at all: `v4 = v2 >= v1 and 1 or 0; if v4 ~= 0 then` becomes `if v2 >= v1 then`. `if` needs the *inverted* comparison, because it jumps to its else-label when the condition is false — a negated operator rather than a `not`.

In exact NaN mode a float comparison routes through a helper returning 0 or 1, and the negation has to be of the **result**, not of the operator: a float comparison is false when either operand is NaN, so `lt` and `ge` are not complements.

### Wrap deferral and range analysis

Each i32 value carries an interval over **the Lua number actually stored**, which is not the same as the wasm value's range — that difference is the point. A wrap is dropped for either of two independent reasons:

- **The result provably fits [0, 2³²)**, so the wrap never did anything.
- **The single consumer reduces modulo 2³² anyway**, so a value merely *congruent* to the wasm value is enough. `(p + i*4) % 2³²` goes from two wraps to one.

The membership rule for absorbing consumers is arithmetic, not a hunch: `(x + y) % M` depends on `x` only through `x mod M`, and `x % 2ⁿ` for n ≤ 32 depends on `x` only through `x mod 2³²` because 2ⁿ divides 2³². Anything that reads a bit pattern — a shift right, a memory address, a comparison — does not qualify and never will.

Deferred values are capped at 2⁴⁸ so the arithmetic stays exact in a double, and the interval is **signed**, because a deferred `i32.sub` can leave a negative number in the slot. Lua's floored `%` recovers the right answer from one.

It runs in **two passes**: deciding whether to defer step *i* needs to know whether step *j*, which comes later, masks. The first pass runs with deferral off and exists only to populate the operand ranges the second reads ahead into.

**Operand-stack slots are block-local; wasm locals are not.** See the fixpoint below. The split is the important part: a slot's range is entangled with whether its wrap was deferred, which is a deal struck with one consumer inside one block, so letting one cross a boundary would hand a consumer elsewhere a value only *congruent* to the one it expects. A local has a Lua name that outlives the block and carries no such deal.

### Signed comparison

M4 lowered `i32.lt_s` to two conditional sign fixups through scratch registers — three statements. Level 1 biases both sides by 2³¹ instead: signed order is the unsigned order rotated by half the range, so `(x + 2³¹) % 2³²` turns one into the other. Branch-free, one expression, and therefore foldable into a branch. When a side is constant the bias is folded at compile time; when the range analysis proves both sides below 2³¹ the compare is direct.

### The loop-header fixpoint

`ir.BuildCFG` builds a graph over the **flat step list**, not over the wasm block nesting, because that is what the emitter prints: everything goes out at function-body level with function-scoped labels and gotos. The one subtlety is where a label lands — a `loop` defines its label at its own step, while a `block`, an `if` and an `else` define theirs at the step *after* the one that writes them. Get that wrong and an edge points one instruction off.

`analysis.Ranges` then solves wasm-local ranges to a fixpoint over it. Three parts, and **all three are needed**; drop any one and the result is the full i32 range everywhere, which is what the pass replaced:

- **Join at merges.** A block entered from several places gets the union. An absent key is the top, never the bottom, so a predecessor that knows nothing poisons the merge — the correct direction.
- **Narrowing on conditional edges.** A guard is a fact about everything it dominates. `i <ᵤ n` on the not-taken side of `br_if $exit (i32.ge_u i n)` is what bounds the counter at all; without it the widening has nothing to converge onto. Three condition shapes are recognised, and the third is the one that matters most in practice: a comparison, an `eqz` of a comparison, and a **bare value used as a condition** — because LLVM strength-reduces most counted loops into a countdown, whose test is `while (n--)` and not a compare.
- **Widening with thresholds at loop headers.** A bound that grew jumps to the next rung of a ladder rather than climbing one iteration at a time. The structural rungs are 2³¹ and 2³², **and the rung one below each**. That second rung is not padding: a guard leaves `i ≤ bound-1` and the increment puts the 1 straight back, so the interval actually stable at the head of a rotated bottom-tested loop is `[0, bound-2]`. Land on `bound-1` and the next sweep steps past it and the whole ladder is climbed for nothing.

Widening points are the targets of **retreating** edges in reverse postorder, not of back edges found by dominance. For a reducible graph they are the same set; for an irreducible one the retreating set is a superset, which costs precision and never costs soundness — and the property that matters holds either way: reverse postorder is a topological order of everything else, so every cycle contains at least one.

**Two things about the entry state pay for themselves.** A declared local starts at zero, which the spec guarantees and the emitter's prologue writes, so a counter's lower bound needs no proof. A parameter starts unknown, which is why the `count` kernel's pre-header guard is load-bearing: it is the only thing that says the trip count is a non-negative *signed* number, and without it the loop test keeps its bias no matter how well the counter is known.

Measured, `-opt=1`, against the block-local pass on the same machine:

| kernel | block-local | fixpoint | |
|---|---|---|---|
| `count` | 18.00 ns/op | **9.50** | **1.89×** — the compare goes direct and the increment loses its wrap |
| `fib` | 18.88 | 17.57 | 1.07× — `n ≥ 2` survives the branch, so `n-1` and `n-2` lose theirs |
| `sum` | 54.11 | 52.51 | 1.03× |
| `chase` | 108.12 | 105.31 | 1.03× |
| `prng` | 73.21 | 71.38 | 1.03× |
| `dot` | 332.50 | 330.22 | 1.01× |
| `frame` | 789.77 | 782.40 | 1.01× |

**On the M4 guest it removes 27 of 467 wraps and none of the 34 biased compares.** That is the honest number and it is worth understanding rather than explaining away: TinyGo's signed compares are mostly not loop guards but comparisons on values built by arbitrary arithmetic — a byte index counted down from a computed base, an error code — and no interval analysis is going to prove those non-negative. The kernel is where the pass shows up, because the kernel is the shape a front end writes and LLVM at `-O0`/`-O1` preserves.

**Not done, and the reason.** TinyGo emits its bounds checks as `if (a | b) trap`, and the *false* edge of that `or` proves both disjuncts false at once. Refining through it would need `guardFacts` to carry a list rather than a pair. Counted: six such conditions in the whole M4 guest. Not worth the machinery until something measures otherwise.

**It costs compile time**: the M4 guest at `-opt=2` goes from 36 ms to 46 ms (+28%). It was 59 ms before the sweep learned to skip blocks whose entry state had not moved — most of a large module stabilises on its first visit, and re-running it once per round was most of the cost. The conformance suite's wall time is unchanged at 1.2 s.

Two safety valves exist and neither is a policy: `maxRounds` caps the sweeps and `maxWidenings` caps one block's climb, and either firing drops that function to knowing nothing — which is exactly the block-local behaviour, so a valve costs speed and never correctness. `maxWidenings` counts only a climb that actually loosened a bound; charging it per merge instead spends the budget on the header's own pre-header edge once per sweep, and a nested loop then runs out while it is still making progress.

---

## Pass 2 — typed-slot promotion (`-opt=2`)

LLVM gives a wasm function a shadow stack because wasm locals cannot have addresses. The prologue is always the same five instructions:

```wat
global.get $__stack_pointer
i32.const  <frame size>
i32.sub
local.tee  $fp
global.set $__stack_pointer
```

When no other use of `$fp` exists, each (offset, width) pair becomes a Lua local, and the store and matching load both vanish. For an f64 that removes an IEEE-754 disassembly and reassembly per access — **842 ns/op to 16 ns/op** on the `frame` kernel — 51×.

**It assumes the module respects the stack-pointer convention it set up itself.** Nothing in the wasm spec stops a module computing the same address another way and reading it, so the pass is sound for compiler output and not for arbitrary wasm. That is why it starts at `-opt=2`: level 1 makes no whole-program assumption at all and is the level for anyone who will not accept this one.

### It finds nothing in TinyGo output, and that is not a bug

**Measured: the M4 guest promotes zero slots. `-opt=1` and `-opt=2` output for it are byte-identical.** Five of its 33 functions use the shadow stack, and in every one the frame pointer is passed to a callee.

That is structural, not incidental: LLVM's own mem2reg has already promoted everything that does *not* escape, so what remains on the shadow stack is precisely what does. The M5 plan expected this pass to be the biggest available win on the strength of the `dot` kernel's 11.18×; **`dot`'s cost is heap-resident f64 arrays whose addresses are function parameters, which no escape analysis can promote.** Closing that gap needs a different change — an f64-typed shadow of linear memory — and that is not what this pass is.

The shape it does target is real: LLVM at `-O0`/`-O1` spills non-escaping locals to the shadow stack, and `bench/wasm/frame.wat` is written by hand to have it.

Refused, each for a reason: a dynamic offset, a frame pointer in a call argument or a store's value operand, one offset written at two widths, overlapping offsets, a reassigned `$fp`, a re-read stack pointer, and promotion that would push the function past the local budget.

---

## Pass 3 — upvalue promotion (`-opt=3`)

`F[idx](…)` measured 21.32 ns in Factorio against 16.82 for a call through an upvalue — 27%. Callees are ranked by call sites weighted by loop depth (×10 per level, saturating at three) and the hottest get a chunk-level name.

**The functions still live in `F`.** `call_indirect` dispatches through the table, exports are taken from it, and a chunk caps at 200 locals. Promotion adds a second name, nothing more.

Order is the whole trick: the `local` must be declared **before** any function body that references it, or the name resolves to a global read — slower than the table lookup it replaced, and wrong, since a mod shares its global table. The binding `fu7 = F[7]` comes **after** the last definition, so a forward or recursive call sees it set.

### The budget is ~25, not the ~120 the plan assumed

The prelude alone declares 167 of Lua's 200 chunk-level locals, and the emitter adds `F`, `MEM`, `MEMSIZE`, `MEMMAX`, `BT`, `TBL`, `TSIG`, `IMPORTS`, `FS`, `FP` plus one local per global. On the M4 guest that leaves room for **21 upvalues** out of 33 functions. `maxUpvalues` is 120 so the cap is never the surprising part of an overflow, but headroom is what actually binds.

### It became the default at M7, and here is what had to be true first

For two milestones the reason to hold back was that chunk-level locals are the scarcest resource a chunk has, and M7 was about to add a dispatch table, a handle table and defines competing for the same 200.

**That premise was wrong.** Those live in `fk_abi.lua` and `fk_api_gen.lua`, which a packaged mod `require`s — separate Lua chunks, each with its own 200. A packaged M7 guest measures:

| file | chunk locals |
|---|---|
| `control.lua` | 23 |
| `fk_abi.lua` | 35 |
| `fk_api_gen.lua` | 0 (it is data) |
| `fk_module.lua` | 167 at `-opt=2`, **188** at `-opt=3` |

So the API cost the guest chunk nothing, and the question reduced to two things that were then measured rather than argued:

**It cannot break a build that worked.** `upvalueBudget` surrenders one name for every name the chunk already spends. Sweeping a 40-callee module across global counts, promotion backs off exactly one-for-one and the chunk lands on a constant:

| globals | `-opt=2` | `-opt=3` |
|---|---|---|
| 20 | 182 | 196 (14 promoted) |
| 26 | 188 | 196 (8 promoted) |
| 32 | 194 | 196 (2 promoted) |

`TestPromotionLeavesTheMarginItPromises` pins that sweep. Writing it found a real off-by-one: `local exports` is emitted **after** promotion has already chosen, so it was invisible to the budget and the chunk landed at 197 when `upvalueMargin` promised 196. `trailingChunkLocals` accounts for it, and the test fails at every fill level if another trailing local appears without updating the constant.

**It is never slower.** Five runs of `bench --opt`, because one run said otherwise:

| kernel | `-opt=2` | `-opt=3` |
|---|---|---|
| fib | 17.95–18.36 | **15.48–16.48** |
| sum | 53.96–55.36 | 53.98–56.31 |
| count | 10.01–10.53 | 9.95–10.43 |
| chase, prng, dot, frame | — | flat |

A single earlier run showed `sum` 10% *worse* at `-opt=3`, which would have been a reason not to ship. It did not reproduce in any of the five; in the same run `dot`'s `-opt=1` swung 0.96× → 1.14×, which is the harness's noise floor rather than a pass. **One run of this harness cannot tell a 10% regression from nothing.**

`-opt=0` remains the bisect reference and reproduces the M4 emitter byte for byte.

---

## Frame-stack spilling (every level)

Not an optimization — a capability, so it is on at `-opt=0` too. A function needing more than `ir.MaxSlots` (180) Lua locals used to be **refused**. It now keeps its hot slots as locals and puts the coldest in `FS`, a chunk-level array indexed off a per-call base:

```lua
local FS, FP = {}, 0            -- chunk scope
...
F[0] = function(v0)
  local fb = FP FP = FP + 24    -- prologue, Invariant B intact
  ...
  FS[fb+3] = 0                  -- a spilled wasm local still starts at zero
  ...
  FP = fb return v201           -- give the frame back
end
```

Cold is measured the same way as hot callees: slot references weighted by loop depth. **A `local.get` names its local in the immediate, not in `Args`** — Args holds the stack slot it pushes — so scoring only `Args` gives every declared local a weight of zero and spills the loop counter first, which is the exact opposite of the intent.

**A trap unwinds past the epilogue and leaves `FP` stale.** Every export is wrapped to reset it:

```lua
exports["f"] = function(...) FP = 0 return F[3](...) end
```

Without that the frame stack creeps upward by one frame per trap for the life of the session — a slow leak whose symptom appears nowhere near its cause. The wrapper is only emitted when the module actually spills, so nothing pays for it otherwise.

A spilled **operand-stack** slot needs no initialiser (it is written before it is read, exactly as an undeclared Lua local is), but a spilled **wasm local** does: the frame entry holds whatever the last call left there, and the spec says a local starts at zero.

**Parameters cannot spill.** A parameter is a Lua local by virtue of being in the parameter list and there is nowhere else for the caller to put it, so a function whose parameters alone exceed the budget is still refused — and `ir.TooManySlotsError` now names that case specifically.

## Where a load's time actually goes — **measured, and it contradicts what was written here**

This file said `chase` was "already dominated by `ld32`'s bounds check", and CLAUDE.md said the same. **That was an inference, never a measurement, and it is wrong.** Breaking one `ld32` into its parts under lua52f, 3M loads each:

| variant | ns/load | what it removes |
|---|---|---|
| as emitted — `ld32(MEM, MEMSIZE, a)` | 40.9 | — |
| the same body, inlined at the call site | 27.0 | the CALL |
| inlined, bounds check removed | 18.7 | the check too |

So of a load's 40.9 ns:

- **34% is the function call itself** — the largest single component
- **46% is the modulo, the divide and the table index**, the work that has to happen either way
- **20% is the bounds check**

The check is the *smallest* of the three. Any plan that starts with `--bounds=fast` is aiming at a fifth of the cost and giving up a safety property to get it.

**On the real kernel** — `chase`'s hot loop is exactly two loads, hand-expanded in the generated Lua and checksum-compared against the original:

| | ms | vs emitted |
|---|---|---|
| as emitted | 201.2 | — |
| loads inlined | 145.4 | **1.38×** |
| inlined, no bounds check | 102.8 | 1.96× |

Inlining alone is 1.38× and costs no safety at all. It would take `chase` from 0.90× to about 0.65×.

**What inlining costs, and why it is not free to adopt.** A load stops being an expression and becomes three statements, so it can no longer be forwarded into a larger one — inlining partially undoes `-opt=2`'s expression forwarding, and every load grows from one line to three or four. On `chase` that trade still won 1.38×; on a guest with thousands of loads the chunk grows accordingly, and Factorio has to parse it. Neither the code-size cost nor the interaction with forwarding has been measured on a real guest.

**The sound alternative to `--bounds=fast` is hoisting**, not removal: for an address of the form `base + i*stride` with `i` proven in range, the M5a CFG fixpoint already has what is needed to prove one check before the loop covers every iteration. That gets the 20% without giving anything up, and only for loops where it can be proven — which is the honest scope.

## The 8-byte access stopped nesting two 4-byte ones — **1.48× on `dot`**

The load-cost breakdown above pointed straight at this. `ld_f64` was:

```lua
local lo = ld32(mem, size, a)
local hi = ld32(mem, size, a + 4)
```

Three calls deep for one f64, and the bounds check run twice for one 8-byte range. `st64` was worse: it carried a comment explaining why an 8-byte store must be bounds-checked **as one access** — and then delegated to two `st32` calls that each checked again, re-tested alignment and re-marked the dirty page. For a pair of adjacent words at `i` and `i+1`.

Both now read or write the pair directly on the aligned path, with one check:

| kernel | before | after | |
|---|---|---|---|
| `dot` | 447.6 ms | 302.0 ms | **1.48×** |
| `chase` | 215.0 | 215.8 | 1.00× |
| `sum` | 566.6 | 567.9 | 1.00× |

`chase` and `sum` are flat because neither touches an f64, which is the control that says the win came from where it claims. `dot` goes from 0.92× to about 0.62× against `-opt=0`.

**This is not the f64-typed shadow of linear memory**, which remains the open question it was. That would keep f64s unpacked and never reassemble at all; this only stops the reassembly from being three function calls. The shadow is still the only thing that addresses the remaining gap, and it still needs a coherence design for aliasing i32 access before it can be costed — see **The f64-typed shadow** below, which now says where the invalidation point would have to be and why this file's own passes moved it.

**What the fast path put at risk, and what checks it.** The ragged path is now a genuinely different branch rather than the same code run twice, so `internal/luagen/f64mem_test.go` walks every alignment for both f64 and i64, compares BITS rather than values (`-0.0 == 0.0` in Lua, so a lost sign bit would compare equal and pass), and asserts an 8-byte store writes exactly eight bytes and leaves its neighbours alone. Separately it pins the property the single leading check exists for: an out-of-range 8-byte store four bytes from the end must write **nothing**, where writing the low word before discovering the high word does not fit would leave a half-written value after a trap the spec says changed nothing.

Confirmed to fail before being trusted: dropping `mem[i + 1]` from the store reports `wrong=24`, and swapping the pair on the load reports the same.

## Division by a constant stopped being a call — **`-opt=1`, 0.684× on `constdiv`**

Same reasoning as the inlined load below: a helper call is the expensive part, measured at 34% of an `ld32`. `i32.div_u` and `i32.rem_u` were an *unconditional* `div_u(a, c)` / `rem_u(a, c)` at every level, and a division helper is the same shape as a load helper — a check and two arithmetic operations behind a call.

From `-opt=1`, a known non-zero constant divisor lowers to arithmetic: `rem_u` to `a % c`, `div_u` to `(a - a % c) / c`, `div_u` by 1 to the identity. The exactness argument and its verification sweep live in [`agents/codegen.md`](codegen.md); the emitter's copy is `constDivIsExact` in `expr.go`, which is a doc comment on a name nobody calls, deliberately.

The signed pair specialises only under a range proof that the dividend is below 2³¹ — the reason is in codegen.md, and the honest consequence is that it will not fire on most TinyGo output.

**Measured on a quiet machine: 0.684× against `-opt=0`** on `bench/wasm/constdiv.wat`, which mirrors the two shapes a real guest contains — 95% CI [0.668, 0.696] against an A/A noise floor of ±0.45%, so a 1.46× speedup on the shape it targets, and identical at `-opt=1` and `-opt=3` exactly as the gating predicts. The structural half agrees with the timing: the kernel's loop body goes from 20 statements and three helper calls at `-opt=0` to six statements and none at `-opt=1`, and the harness's checksum agrees at all four levels (35408).

**Three hazards, each with a test that was seen to fail with the guard removed.**

- **The discarded divisor** — `mayNotEvaluate`. This is the recurrence of the constant-fold bug above and it is fully live here, not theoretical: with the four ops removed from `mayNotEvaluate`, `(i32.rem_u 100 (i32.add 4 (i32.div_u 0 (local.get $z))))` returns `0` instead of trapping, at every level from 1 up. `TestAConstantFoldedDivisorStillTraps`.
- **The duplicated dividend** — `duplicatesOperand`. `(a - a % c) / c` names operand 0 **twice**, so a composite substituted there runs twice. Removing the entry emits `(ld32(MEM, MEMSIZE, v0) - ld32(MEM, MEMSIZE, v0) % 3) / 3` — two bounds checks and two table reads from one wasm operation, and two chances to trap where wasm had one. `rem` is **not** on that list: `a % c` names its operand once.
- **The trap budget.** A specialised division has no zero check left, so it no longer spends the one-trap-per-expression budget. That refinement is at the call site in `forwardPeephole` rather than inside `stepTraps`, because the answer depends on the level and on the range analysis exactly as the choice of lowering does — and the two must agree, which they do by both asking `constDivisor`/`constDivisorS`. It is what lets `(i32.add (i32.load $p) (i32.div_u $i 7))` become one expression.

  Worth knowing that this refinement is also what makes the first hazard *reachable*: before it, `stepTraps` marked every division as trapping, so the one-trap rule already refused the forward and `mayNotEvaluate` was shadowed. A test written against the shadowed version passes with the guard deleted. If the refinement is ever reverted, the guard must stay anyway.

**Where it has a target, checked in the real thing rather than assumed.** Disassembling `bench/guests/go` (TinyGo 0.41.1, `wasm-unknown`): **LLVM does not strength-reduce constant integer division on wasm.** It emits `i32.div_u`/ `i32.rem_u`/`i32.div_s` against a literal `i32.const` — six of the eight division sites in that guest have a constant divisor, including `i32.div_u` by 10 in `real_names`' hot digit loop. The constant survives to the emitter, so the experiment has real targets. **`real_grid` is not one of them** — see [`agents/benchmarks.md`](benchmarks.md).

## The inlined i32 load — **`-opt=3`, 1.36× on `chase`**

Follows directly from the cost breakdown above: 34% of a load is the call, so `-opt=3` expands it at the use site.

```lua
t0 = v0 + 4
if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
if t0 % 4 == 0 then v5 = MEM[t0 / 4 + 1] else v5 = ld32(MEM, MEMSIZE, t0) end
```

The bounds check stays — it is 20% of the cost and buying it back is a safety trade nobody asked for. The **unaligned case still calls `ld32`** rather than being expanded as well: LLVM aligns what it can, so almost nothing takes that path, and expanding it would triple every load's size for nothing.

| kernel | `-opt=2` | `-opt=3` | |
|---|---|---|---|
| `chase` | 104.56 | 77.00 ns/op | **1.36×** |
| `sum` | 52.96 | 44.82 | **1.18×** |
| `dot` | 228.41 | 228.87 | 1.00× — f64, not i32 |
| `prng`, `count`, `frame`, `fib` | | | unchanged |

**What it costs.** A load stops being an expression, so `-opt=2`'s forwarding can no longer fold it into a larger one — the pass gives up ground to buy the call, and on `chase` buying the call wins. On the real API guest the chunk grows **+9.5% in bytes and +8.8% in lines**, with 118 loads inlined. Both guests still load and run in Factorio 2.0.77, deterministic across runs.

**`t0` must be DECLARED, and it nearly wasn't.** The scratch register is only emitted by `needsScratch`, which lists the ops that reach for one — and the inlined load was not on it. A bare `t0 = ...` in a function whose prologue has no `local t0, t1` is a write to a **global**: it parses, it runs, it computes the right answer, and the entire spec suite passes at every level in both NaN modes. What it actually does is turn every scratch access into an `_ENV` table lookup and scribble a name into the mod's global namespace.

It surfaced only as a performance number going the **wrong way** — `chase` and `sum` came back 1.28× *slower* from a change meant to speed them up. Hence `TestAFunctionUsingAScratchRegisterDeclaresIt`, which walks every scratch-using shape at every level and asserts the declaration is there; it reports the bug when the fix is reverted. A correctness gate that cannot see an error this large was worth adding rather than trusting that someone benchmarks the right kernel.

The level check is at the call site rather than inside `needsScratch`, because adding `OpI32Load` to that list would declare `t0` at **every** level, and `-opt=0` has to keep reproducing the M4 emitter byte for byte.

## One entry test replaces every bounds check in a loop — **`-opt=3`, 1.57× on a real guest**

This is the largest win in the optimizer measured against a REAL guest rather than a kernel, and it is where a hot loop's time actually goes.

A guarded i32 access keeps its address computation and loses everything else:

```lua
t0 = ((v1 + 12) % 4294967296.0)
if lg41 then v11 = MEM[t0 / 4 + 1] else
  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
  if t0 % 4 == 0 then v11 = MEM[t0 / 4 + 1] else v11 = ld32(MEM, MEMSIZE, t0) end
end
```

A guarded access does not compute an address at all. The loop keeps a **word index** — the base divided by four, plus one — stepped alongside the base, so the access is one table read:

```lua
if lg0 then v11 = MEM[lw0_0 + 3] else
  t0 = ((v1 + 12) % 4294967296.0)          -- the address SINKS into this arm
  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
  …
end
```

**Measured, real compiler, checksum-compared, interleaved under `bin/lua52f`: `pure_sum` goes 0.424× through the TinyGo guest and 0.417× through the Rust one** — 2.36× and 2.40× — against A/A floors under 1%. It arrived in two steps that are worth keeping separate, because the second was nearly all of the remaining headroom a decomposition had already predicted: the guard alone is 0.639× / 0.596×, and the word index and hoisted page mark below take it the rest of the way at 0.663× / 0.699×.

End to end that is **`pure_sum` 4.41× → 1.88× for TinyGo and 4.15× → 1.73× for Rust** against hand-written Lua. A compiled guest is now within 2× of hand-written Lua on an array reduction, where it started this milestone at more than 4×. The two halves separately are 0.773× (bounds) and 0.934× (alignment), so both are worth having and **the bounds check is the larger one** — which does not contradict this file's "20% of a load" figure, it reframes it: 20% is the check's share of one load, and hoisting it out of a loop removes it from every iteration rather than making it cheaper.

For scale, on the same loop the counted-loop pass — replacing label, increment, compare and goto with one `FORLOOP` — is worth 0.950×. **Per-access memory overhead outweighs loop overhead by about seven to one**, which is the opposite of what the M0 kernels suggest and is the single most useful thing this pass established.

### The word index, and why the address computation could just move

The guard's first form still paid, on every access, an add and a modulo to build the address and then a divide and an add to turn it into a table index. All four are gone:

- The **address computation sinks into the unguarded arm.** It is arithmetic on a local with no side effect and nothing else reads `t0`, so moving it is sound — and on the guarded path it was pure waste, since that path never looks at the address.
- The **word index replaces the divide.** `(base + off) / 4 + 1` is `w + off/4` when `w = base/4 + 1`, and both the stride and every offset are already required to be multiples of four, so neither division is inexact. `w` is seeded in the guard, where the base still holds its entry value, and stepped by `stride/4` wherever the base is stepped by `stride`.

That trades **one add per iteration for one add-modulo-divide-add per access** — on Rust's 8×-unrolled loop, one against eight. It needs no body duplication, which is what an earlier estimate assumed and what made this look expensive.

`w` is declared initialised rather than merely declared: on a path where the guard is false it is never read but IS still stepped, and `nil + 8` is an error where a wasted add is not.

Cost is +27 lines on the TinyGo bench guest and +4 on the Rust one.

### The dirty-page marking is hoisted too, and it was free

Under `--persist=packed` every store carries `if MEMDIRTY and <span left the cached page> then MEMPACK.mark(…) end`. A guarded loop already computes the exact span it will write — that is what the bounds test proved — so the guard marks the whole range once, unconditionally (the cached-page test is a saving for a store run millions of times, not for one call per loop entry), and no guarded store carries the marking at all. The unguarded arm keeps its own, because on that path the guard marked nothing.

Marking up front is sound for the same reason the bounds check is: an early exit writes FEWER bytes than the span, never more. Over-marking costs a repacked page, which is the safe direction; under-marking costs a save that silently omits the write, which is the bug class this repo has been bitten by three times now. `TestAGuardedStoreReachesTheSave` reports `dirty 0 / word 0` when the hoisted block is removed.

### Three exit-test shapes, and the one that decides which toolchains benefit

```wat
br_if $top (i32.ne  (local.tee $i ...) (local.get $n))   TinyGo, counting up
br_if $top (i32.lt_u (local.tee $i ...) (local.get $n))
br_if $top (local.tee $i (i32.sub (local.get $i) 8))     rustc, counting down
```

The third has **no comparison step at all**: a `br_if` continues while its operand is non-zero, which *is* `!= 0`, and the counter reaches the branch straight out of its own `local.tee`. Recognising only the first two is the entire reason the pass reached TinyGo's `pure_sum` and left Rust's alone — the loops are otherwise the same shape, and Rust's is *better* suited to it, being 8×-unrolled where TinyGo's is 4×, so one guard covers eight accesses instead of four. That is why Rust ends up faster than TinyGo on this kernel once both are covered.

A countdown also takes its trip-count difference in the **opposite direction**: `counter - bound` rather than `bound - counter`. Getting that backwards is not a miscompile — the difference goes negative, `t0 > 0` fails, and the guard is simply false for every countdown — which makes it a silent loss of the whole win and invisible to any test that only compares values. `TestACountdownLoopIsGuarded` pins the emitted text for exactly that reason, and it is the one property here that cannot be observed behaviourally.

### The guard is a RUNTIME test, and that is the whole design

The plan this replaced needed a static congruence proof that the counter hits its bound exactly, because the hot loop is 4×-unrolled and closed with `i32.ne`. That proof is genuinely hard — it wants a residue `align.go` does not publish, and a direction fact the range analysis cannot carry from the guarded local to the bound, which is derived from a different one.

None of it is needed. The guard runs **once, at loop entry**, where every quantity is a live Lua value, so divisibility, direction, alignment and the whole address span are just arithmetic:

```lua
t0 = v8 - v3                              -- bound minus counter
if t0 > 0 and t0 % 4 == 0 then            -- reachable in whole steps
  t0 = t0 / 4 - 1                         -- iterations, less one
  lg41 = v1 >= 0 and v1 % 4 == 0 and v1 + t0 * 16 + 16 <= MEMSIZE
else lg41 = false end
```

What must be proved STATICALLY is only the loop's **shape** — that the body runs a predictable number of times and each address advances by a fixed amount — and that is structural, with no dataflow in it. The general lesson is worth keeping: when a check is loop-invariant, hoisting it is often cheaper to *justify* than to *prove*, because the hoisted position has values the compiler does not.

The divisibility test is not a precision nicety. An `i32.ne` counter whose bound is not a whole number of steps away walks past it and wraps, so the trip count the span was computed from would be wrong by about four billion.

### The seed belongs to the loop HEADER, not to the label — **and that was a bug**

The guard has four emitted parts and they were not written in one place: the flag and word indices are **declared** in the prologue, the **seed** is written at the loop header, the **arms** are written at each access, and the word-index **step** is written wherever a base advances. Three of those are keyed off things the emitter cannot skip. The seed was keyed off the `OpLoop` *step*, and a step is exactly the thing another lowering can take over.

The counted-loop pass takes it over. It replaces a loop's header, increment and test with a numeric `for`, and the emitter's step loop `continue`s past `emitStep` when it does — so `case wasm.OpLoop`, where `emitLoopGuard` used to live, was never reached for a loop both passes claimed. The other three parts were emitted as normal. The result is a guard flag that is **declared, read, and never assigned**:

```lua
local g95, w95_0 = false, 0            -- declared
                                       -- seed: MISSING
for v4 = v4, 1, -1 do
  if g95 then MEM[w95_0] = ... else ... end   -- read, always false
  w95_0 = w95_0 + 1                    -- stepped for nothing
end
```

**Nothing behavioural can see this.** The flag is false, every access takes the unguarded arm, and the unguarded arm is the code that ran before the pass existed — so the checksums agree, the conformance suite is green, the differential run agrees across toolchains, and the loop is simply slower than an unguarded one by a dead branch and a wasted add per iteration. Under `--persist=packed` it is worse than a performance loss in kind: the hoisted `if gNN and MEMDIRTY then MEMPACK.mark(...)` lives **inside** the seed block, so a guarded store loop marked nothing — sound only because the guard was also false, which is a correctness argument nobody was making on purpose.

The fix is one line moved: the guard is written by the step loop, in front of whichever form the header takes, and `case wasm.OpLoop` now says so in a comment rather than doing it. The rule generalises past this pass — **a lowering that replaces a loop HEADER inherits everything else that was written there** — and the reason it needs writing down is that the emitter has no way to complain: the guard's other three parts do not ask whether the seed happened.

Measured, real compiler, checksum-compared, interleaved under `bin/lua52f` on the shape (a 4×-unrolled countdown over 16,384 iterations, both lowerings claiming it): **0.374×**, an A/A floor of 0.15% and per-variant spread under 2.4%. That is the whole guard, recovered.

Which loops it hit is not where `bench-guests.sh` looks, and that is worth being plain about. Across the whole guest corpus it was **three distinct source loops**: the remainder loop of the TinyGo bench guest's `pure_setup`, and the array and map loops of `fkapi.writeDyn`, which appear in `examples/array`, `heap` and `callcost`. Neither is in a timed kernel — `pure_setup`'s time is explicitly subtracted by that harness and its remainder runs **zero** times at the benchmark's own argument of 65,536, and `writeDyn` is on the host-call marshalling path, which the bench guest never touches. The whole cross-language table is therefore the control, and it behaved like one: the emitted chunk for `bench/guests/go` differs from the previous one by exactly the ten lines of that one seed block and nothing else, in **both** persistence modes, which is a stronger statement about attribution than any timing run.

Where it does land is the host call. `callcost`'s **tier-2 map argument** — `writeDyn` over a two-entry map, which is the loop that was dead — measures **0.91×** at `-opt=3 --persist=table`, 13.3 µs → 12.1 µs per call, against a ±3.3% A/A floor on the same harness, with every A sample below every B sample over four interleaved rounds. That is ~9% off the end-to-end cost of a map argument for a two-iteration loop, which is about what 21 hoisted bounds checks an iteration should be worth. The packed mode's own figure is ~139 µs and is all flush: the guard is real there too and invisible underneath it.

`TestACountedLoopStillSeedsItsGuard` pins the shape. `TestEveryGuardAGuestReadsIsAlsoSeeded` closes the class: it emits every bench guest and every example at `-opt=3` in **both** persistence modes and asserts, over the emitted text, that every guard flag a function declares is one that function assigns — and also reads, and also does not share a name with a module global. It counts what it audited and fails if either toolchain contributed nothing, because a corpus test that quietly checks nothing passes forever.

That last clause was a **tripwire** when it was written, because the guard was spelled `gN` and so is a global. It is now an impossibility proof, and the next section is why.

### The guard's names were in the globals' NAMESPACE — **a silent miscompile, fixed 2026-08-01**

The flag was `guardName(header) = "g%d"` off a loop's header **step** index. A module global is `globalName(i) = "g%d"` off the **module's global** index. Two unrelated counters, one spelling: a guarded loop whose header step index fell below the module's global count declared a *function-scoped* local with a module global's name, and Lua's scoping did the rest without a word.

**Everything inside that function then talked to the wrong variable.** A `global.set` wrote the guard flag, so the write was discarded when the function returned. A `global.get` read a boolean. `g0` is the shadow-stack pointer in TinyGo output, and header step 0 is nothing exotic — it is a function whose first instruction is the loop, which is most of the hand-written `.wat` in `loopguard_test.go`.

Reproduced as a value, not argued (`TestAGuardLocalDoesNotShadowAModuleGlobal`): a module with globals `$sp` and `$mark`, and a function whose first step is a guarded walk that writes both at the end.

| | `walk` returns | `mark` (`g1`) | `sp` (`g0`) |
|---|--:|--:|--:|
| `-opt=0`, `1`, `2` | 22 | 22 | 22 |
| **`-opt=3`, before the fix** | 22 | 22 | **66560** — its initialiser |

`$sp` keeps the value it was born with; its neighbour one global index along is fine, which is what says this is shadowing rather than a broken harness. The read side fails louder and is pinned too: `(i32.add acc (global.get $sp))` becomes `attempt to perform arithmetic on local 'g0' (a boolean value)`, a trap with nothing on its face to do with the loop that caused it.

**No corpus guest hit it**, because header step indices in real guest functions happen to exceed those guests' global counts — a TinyGo guest emits 0 or 1 global. So the only thing standing between a *user's* guest and this was the corpus audit's collision clause, which fires on this repo's guests and not on theirs. A tripwire over a corpus is not a fix; it converts a miscompile into a test failure for the people who already have the test.

The fix is the namespace: `lg%d` and `lw%d_%d`. The **rule** it comes from is worth more than the letters — *a family indexed by a step index owns a prefix nothing else uses*, since a step index is a dense small number counted per function and so is every other index in the emitter. `TestNoNameFamilyCanCollideWithAnother` enumerates every family the emitter can emit (globals and their i64 high halves, slots, guard flags, word indices, the counted loop's `fk%d` control variable, promoted upvalues, labels, scratch, the fixed chunk names, and the prelude's own column-zero locals) over a generous range of every index, and demands the sets be pairwise disjoint. That is a proof over every module rather than over the guests that happen to be checked in, and it is the assertion this pass should have had from the start. The full table is in [`agents/codegen.md`](codegen.md).

The word index `w%d_%d` and the counted loop's `fk%d` were audited against every other family in the same pass and neither ever collided; `w` moved to `lw` for consistency, `fk%d` stayed. Emitted output for the corpus differs by **exactly the renamed identifiers**: `examples/array` and the TinyGo bench guest, both persistence modes, are byte-identical to master's after the inverse rename, at identical line counts, and the guard census is unchanged guest for guest.

### What it refuses, and why each would be an out-of-bounds access

This removes a **bounds check**, so a loop admitted on a wrong premise does not compute a wrong number — it reads or writes outside the guest's memory, which in a word table means a `nil` surfacing somewhere far away or a silent write past the end.

- **One branch to the header, closing the loop.** A `continue` would skip an increment and neither the trip count nor the stride would hold.
- **A straight-line body** — no block, `if`, `else` or any other branch. That is what makes "each increment happens once per iteration" true by inspection rather than by a dominance argument.
- **The counter and the base are each written exactly once**, by their own increment.
- **Every guarded access hangs off the same base.** A second base is expressible — the guard is a conjunction — but nothing measured needs it.
- **Stride and offsets are multiples of 4**, checked statically, so the only alignment fact left for runtime is the base's own.
- **`memory.grow` in the body is refused**, though growth alone would be safe: `MEMSIZE` only ever increases within a call, so a hoisted span stays valid.

Three mutations were confirmed to fail, each against the exact trap MESSAGE rather than merely "something went wrong" — a read past the end of the word table yields nil and raises a Lua arithmetic error, which the harness also reports as a trap, so asserting only failure would pass on exactly the bug being hunted:

| removing | reported |
|---|---|
| the stride from the span | `nil` written past the end of the table |
| the divisibility test | a wrapping loop guarded on a four-billion-iteration trip count |
| the access WIDTH from the span | `attempt to perform arithmetic on local 'v5' (a nil value)` |
| the per-access offset from the word index | every access in an unrolled loop reads the same word |
| the word index's per-iteration step | the loop reads its first element forever |
| the guard's hoisted page mark | `dirty 0 / word 0` — the save does not carry the write |

The offset case needs an **unrolled** kernel, with several accesses at distinct offsets off one base. With a single access at offset zero the index and the base coincide and the bug is invisible — which is what the first version of these tests missed.

The width case needs its own kernel — one whose last access *starts* exactly at `MEMSIZE` — because the other two refuse under both the correct and the broken span, and the omission would otherwise go unnoticed.

A guarded **store** owes the dirty-page mark on both arms, same as the inlined store and the substituted `memcpy`. `TestAGuardedStoreReachesTheSave` replays the control.lua protocol through a stand-in `storage`.

### Coverage, and the census

When the body still had to be straight-line this reached **5 loops in the TinyGo bench guest and 2 in the Rust one**, covering the hot loop and its remainder in each, and **0** in `examples/hello`, `grow`, `array` and `api`. The binding constraint was the straight-line body: real Go code carries its own bounds checks, which are branches, so a loop only qualified once LLVM had unrolled it into a straight run — which is exactly what happened to `pure_sum` and exactly why it was the kernel that moved. The latch rule below removed that constraint.

**Re-taken on this build, at `-opt=3`,** by `TestEveryGuardAGuestReadsIsAlsoSeeded`, which counts what it audits so this census stops being something nobody re-took — **and re-taken again 2026-08-03**, which found the table had drifted anyway on four rows nobody had changed deliberately. That is worth more than the numbers: a census that is only re-derived when somebody remembers is a census, and one the test PRINTS on every run is a measurement. The rows below are copied from that log.

| guest | TinyGo | Rust |
|---|---|---|
| bench guest | 8 | 5 |
| `examples/hello` | 6 | 6 |
| `examples/array` | 16 | 12 |
| `examples/api` | 9 | 8 |
| `examples/dict` | 4 | — |
| `examples/heap` | 8 | — |
| `examples/callcost` | 5 | — |
| `examples/churn` | 2 | — |
| `examples/retain` | 1 | — |
| `examples/gcconfig` | 0 | 2 |
| `examples/grow` | 0 | — |
| `examples/gctorture` | — | 5 |
| `examples/gctorture`, collected | — | 7 |

`api`'s TinyGo row moved 6 → 9 and its Rust row 7 → 8 in the ports round, and that one IS a deliberate change: both examples gained four filtered subscriptions, and `NameFilter`/`name_filter` builds them in a loop. The `gctorture` rows were always audited and were never in this table.

Size cost is +1.4% on the bench guest, the smallest of any pass here.

**Seven of those were counted and did not work**, until the seed defect above was fixed: 1 in the TinyGo bench guest and 2 each in `array`, `heap` and `callcost` (three distinct source loops, two of them in shared `fkapi` code). A census of guards *planned* is not a census of guards *seeded*, and until that test existed nothing distinguished the two.

## Multi-base guards, affine bases and 8-byte accesses — **`-opt=3`**

The guard's span calculation was rewritten around a LIST of bases rather than one, which is what `pure_dot` needs: it reads two arrays in step, so one span cannot describe it.

**Measured: `pure_dot` 11.44× → 8.50× for TinyGo and → 8.48× for Rust** against hand-written Lua, with every other cell inside noise. An A/B of the prototype alone measured 0.761× on the same kernel.

Three things had to change together, and none is a widened predicate:

- **Multiple bases.** The guard becomes a conjunction, one span and one word index per base, capped at three. Each base's span is its OWN reach — `MaxEnd` is `max(offset + width)` over that base's accesses — because sharing one under-covers whichever base reaches further.
- **Affine bases.** A base is often not advanced in place but rebuilt each iteration as `index + arrayStart`. Its stride is the induction variable's, and its entry value is the SUM, which the guard reconstructs from the two locals because the base local itself holds nothing meaningful before the first iteration.
- **8-byte widths.** `f64.load` and `i64.load` join, and the alignment requirement stays mod 4 — that is what the inlined 8-byte path gates on, since it reads its two words separately. What the guard does not prove is that the bits are a normal double, so the exponent test and its fallback stay, which also keeps exact-NaN mode correct for free.

### Branchy bodies, and the three things that had to change together

The body no longer has to be straight-line. **Measured: `real_entities` 5.53× → **4.83×** (TinyGo) and 5.43× → **4.81×** (Rust)** against hand-written Lua, which matches a hand-edited ceiling of 0.899× taken first.

**The number was re-measured rather than reused.** The old estimate was 0.950×, taken when ~60% of that loop was `ld8`/`ld16` CALLS. Those are inlined now, so the same absolute saving is a larger fraction of a smaller loop — the estimate was stale in the useful direction, and re-taking it is what made the work worth doing.

**1. The latch rule replaces the straight-line rule.** What has to be true is that every increment runs exactly once per completed iteration, and that is a property of the LATCH — the block the back edge leaves from. The latch runs exactly once per completed iteration by construction: the only way back to the header is the back edge, there is exactly one, and it is in the latch. Accesses in earlier blocks run at MOST once, which is fine — a span covers an access that happens, not one that must. No dominator tree is needed and the repo has none.

Two things still go: a nested `loop` would let the latch run many times per outer iteration while `writesOf` still reported one write, and `memory.grow` would move `MEMSIZE` under a span already proved.

**2. `defOf` becomes block-aware, and this is the sharp part.** It is a backward LINEAR SCAN over step indices. In a straight-line body textual order IS execution order, so the nearest preceding writer of a slot is the definition; in a branchy one it can sit in a sibling arm that never executed on the path to the use. Every guard fact goes through it — the exit test, every increment, every access ADDRESS. `defOfB` requires the definition to be in the use's own block, where textual order is execution order again.

Operand-stack values normally die at a block boundary, so producing the bad case takes a block that yields one: `TestADefinitionFromAnotherBlockIsRefused` uses an `if (result i32)` whose arms offer different bases, and without the restriction the loop is guarded against whichever arm is textually last.

**3. An undescribable access is SKIPPED, not fatal — and so is an undescribable BASE.** This is what actually admits `real_entities`. Its hot loop reads and writes `totals[kind]` through an address computed from a loaded byte, on a base reassigned twice, alongside two ordinary reads off the entity pointer. Refusing the loop for the former threw away the latter, which is where the time is. A skipped access simply stays out of `Steps`, so the emitter gives it its own full bounds check and its own page mark, exactly as if no guard existed — the guard only ever claims something about what it specialises.

Coverage moved a long way as a result: `examples/hello` 0 → 6 guarded loops, `examples/array` 2 → 11, `examples/api` 0 → 5. Those were the numbers the extension bought on the build that landed it; the census above is the one re-taken on the current build and is what to quote.

One check was DELETED rather than kept: the counter's own write needed no latch test, because the exit test is resolved with `defOfB` from the back edge and the tee from the test, so both are in the latch by construction. It was dead code that read as load-bearing, which is worse than none — a later change making it reachable would find it already "tested".

### The same base wears two spellings, and missing that covers half a loop

The first access off a base reads the SUM straight out of the peephole; every later one reads the local the sum was stored into — and that store is a `local.tee`, so the value arrives a third way:

```lua
v16 = (v1 + v9) % 2^32   v2 = v16   t0 = v16      the first access
t0 = ((v2 + 8) % 2^32)                            every later one
```

Canonicalising all three to the pair (induction variable, invariant) is what makes them one base. Before that, the guard landed on `pure_dot`'s REMAINDER loop with 2 accesses and refused the 4×-unrolled hot loop entirely — a failure that looks exactly like success in the emitted output, since a guard was present and the code was correct. `resolveBase` is where the three spellings meet.

### Rust's `pure_dot` did not move at first, and the cause rhymes

It sat at 11.44× while TinyGo's went to 8.50×. The loops are structurally identical — affine bases through a `local.tee`, accesses at +0/+8/+16/+24 — and the difference was one predicate: **rustc indexes both arrays off the LOOP COUNTER** (`base = counter + arrayStart`), and this pass refused any base whose walker was the counter, on the theory that a counter-shaped base meant two strides at once.

It means no such thing. A base *derived* from the counter has exactly one stride, the counter's; what the rule was written for is the counter used AS a base, which is still refused because nothing needs it. Relaxing it to `walker == g.Ctr && !key.affine` took Rust to 8.48×.

**That is the second time a toolchain looked immune to a pass and was not** — the first was the countdown exit test. Both times the loops were the same shape and one over-broad predicate hid it. When one toolchain benefits and another does not, the prior should be that they are the same loop wearing different clothes.

### What is pinned

Five mutations were confirmed to fail: a span that forgets the access width, a base given another base's span, a word index that ignores which base it belongs to, an affine stride taken from the invariant instead of the induction variable, and the earlier single-base set. Two runtime tests carry the multi-base cases — two arrays whose values differ, so a shared word index is visible, and a second base placed so only its OWN span refuses it — because the first version of these tests had single-base loops only and caught none of it.

## The inlined byte LOADS — **`-opt=3`, 1.14×–1.30× on every realistic kernel**

`ld8` is two nested calls deep — it bounds-checks, then calls `ld8raw` to do the extraction — and this file's own breakdown puts a single call at 34% of an access. Expanded at the use site:

```lua
t0 = ((v2 + 1) % 4294967296.0)
if t0 < 0 or t0 + 1 > MEMSIZE then trap_oob() end
t1 = t0 % 4                       -- the byte's position in its word
t2 = MEM[(t0 - t1) / 4 + 1]       -- the containing word
t1 = P2[8 * t1]                   -- 2^(8*position)
v17 = ((t2 - t2 % t1) / t1) % 256
```

**Measured against a binary built from the commit before it, checksums compared:**

| kernel | | end to end, vs hand-written Lua |
|---|---|---|
| `real_entities` | **0.768×** | 7.23× → **5.47×** |
| `real_grid` | **0.876×** | 9.73× → **8.57×** |
| `real_names` | **0.875×** | 5.17× → **4.52×** |
| `pure_sum`, `pure_prng` | — | unchanged — **the control**, neither has a byte load |

This is the broadest win in the optimizer: it moves every kernel in the *realistic* half of the table, which is the half that resembles a mod. Size cost is +5%.

### Why this was missed, and it is worth knowing

This file said sub-word accesses stay calls because `st8b`'s body is a read-modify-write needing "the byte's position within the word, the word index, a power-of-two divisor out of `P2` and the old word all live at once — five values against the two scratch registers a function declares."

**Every word of that is about a STORE.** A load has no read-modify-write, no old word to preserve and nothing to write back: three values, against a 4-scratch tier the inlined 8-byte access had already introduced. Loads and stores were reasoned about together, only the store's constraint got written down, and the load's much weaker one was never separately checked. The stores genuinely do stay calls — that part was right.

It is also the safest expansion in the emitter, for a reason worth stating because the opposite case has bitten three times: **a load records nothing.** The dirty-page mark that every inlined STORE must reproduce simply does not apply, and the bounds check is kept, so no memory-safety property moves at all.

Six mutations were confirmed to fail: dropping the byte position, an off-by-one word index, the wrong divisor scale, swapped 16-bit halves, a 16-bit bounds check covering only one byte, and a dropped sign extension. Alignment is the whole risk — the expansion picks a byte out of a word *by position* — so the tests sweep every alignment against `-opt=0` rather than checking one.

The two bytes of a 16-bit load are fetched independently rather than through a same-word fast path. A 2-byte access inside one word re-reads that word, which is one wasted table read; adding a branch to avoid it costs a test on every 16-bit load to save a read on most of them. Measure before assuming that trade.

## Ideas that were measured and NOT taken

Eight further ideas were scouted, prototyped by hand-editing real emitted Lua, and timed by the same interleaved A/A harness as everything else here. Five are dead; of the three that were not, two have since shipped — the `PE` table (in [`agents/codegen.md`](codegen.md)) and the 8-byte guard extension above — and one is still unbuilt. They are recorded with their numbers so nobody re-derives them.

**Prototyping by hand-editing emitted Lua is what makes this cheap.** Every entry below cost one prototype and one timing slot, against days of building. Two of them looked obviously good and were not.

| idea | measured | verdict |
|---|---|---|
| 8-byte accesses under the loop guard | **0.777× on `pure_dot`** | **BUILT** — multi-base, affine bases, width 8 |
| guard for loops with branchy bodies | 0.899× on `real_entities` | **BUILT** — latch-block rule, block-aware `defOf` |
| byte-per-entry linear memory | 0.730× on `real_grid`, **2.93× WORSE on `pure_sum`** | dead — trade is lopsided |
| f64-typed shadow of linear memory | ceiling **0.465×** on `pure_dot` | open question, now with a number |
| per-signature `call_indirect` views | — | dead — no `call_indirect` exists |
| host-backed hash tables for guests | — | dead — premise false |
| compressed packed pages | 0.978×–1.091× save size | dead |
| registered-metatable lazy zero pages | — | dead |

### Built since, and still unbuilt

**~~8-byte accesses under the guard~~ — BUILT.** See the multi-base section above; `pure_dot` went 11.44× → **8.58×** for TinyGo. What follows is the original entry, kept because its coverage measurement is still the reason the cheap version was not taken.

**8-byte accesses under the guard — 0.777× on `pure_dot`.** The prototype hoists the bounds check and the 4-alignment test out of `pure_dot`'s 4×-unrolled loop, exactly as the i32 guard does.

**There is no cheap version, and this was checked rather than assumed.** Adding `f64.load`/`i64.load` to `guardableAccess` and nothing else — the one-line change — was measured for COVERAGE against the real guests: it covers **one** access, in `examples/array`, and **zero** in either bench guest. The whole win is in loops the single-base machinery cannot describe, so the minimal change buys nothing and is not a step toward the full one.

What it actually needs: `pure_dot` reads TWO arrays, so the guard needs **multiple bases** (a conjunction, one word index each), and each base is `v1 + v9` — an induction variable plus a loop-invariant — rather than a local incremented in place, so `incrementOf` does not describe it. Three extensions together: width 8, multi-base, and an affine base whose stride is the induction variable's. The alignment requirement stays mod 4, not mod 8, because that is what the inlined 8-byte path actually gates on.

**Branchy loop bodies — 0.950× on `real_entities`, and nothing else in the corpus.** The straight-line-body requirement is why the guard reaches 5 loops in the TinyGo bench guest and 0 in every `examples/*`. Admitting branchy bodies would be a block-identity test rather than a dominator tree — require the counter's and base's writes to be in the LATCH block, which by construction runs exactly once per completed iteration — and `ir.BuildCFG` already provides it.

Two things make it more than 120 lines, and both were found by scouting rather than by reasoning about the idea itself:

- **`defOf` is a backward LINEAR SCAN over step indices with no control-flow awareness** (`internal/analysis/loops.go`). In a straight-line body the nearest textually-preceding writer of a slot IS the definition; in a branchy one it can sit in a sibling arm that never executed. Every guard fact — the exit test, both increments and every access ADDRESS — goes through `defOf`, so admitting branchy bodies makes address computation unsound before dominance is even reached. The fix is to require `cfg.BlockOf[def] == cfg.BlockOf[use]`.
- `real_entities`' `totals[kind]` load and store hang off a second, DYNAMIC base, and `finishGuard` currently lets an undescribable address poison the whole loop. They would have to become skips rather than refusals.

At 0.950× for that, it is deferred rather than rejected. Worth knowing: the guard is worth **more** after byte-load inlining, because `real_entities` spends ~60% of its loop in `ld8`/`ld16` calls that the guard does not touch.

### Dead, with the numbers that killed them

**Byte-per-entry linear memory — the trade is real and lopsided.** One table entry per byte makes `i32.store8` a single table write instead of a read-modify-write, which is what `agents/benchmarks.md` blames for `real_grid`. Prototyped faithfully — the variant builds a 131072-entry table against the word table's 32768, and `mem_fill`/`mem_copy` do 4× the writes, so it pays byte mode's real costs. **`real_grid` 0.730×. `pure_sum` 2.927× SLOWER.** A word load becomes four table reads, three shifts and three adds, and most guest code is word-oriented because that is what `i32.load` is. The compiler cannot know which side of that trade a guest sits on, and a flag that is a 1.4× win or a 2.9× loss depending on something the author cannot easily see is worse than no flag. Recorded because both the scout and the adversarial critic predicted "close to a wash" and BOTH were wrong — the upside and the downside are each much larger than predicted.

**The f64-typed shadow — the ceiling is now measured, at 0.465× on `pure_dot`.** That is with the reassembly deleted outright and no coherence cost paid at all, so it is an upper bound on an upper bound. A **loop-scoped** shadow — built at guard entry, discarded at exit — was the new angle the open question predates, and it is provably negative for `pure_dot`: building the shadow is itself a pass over the data, and `pure_dot` reads each element once. A shadow only pays where a loop reads elements more than once. Separately, the `ldexp` slice of that 0.465× was **0.902×** and has now shipped on its own as the `PE` table, so the shadow's remaining prize is smaller than 2.15× by that much.

**Per-signature `call_indirect` views — there is no `call_indirect`.** Counted with `wasm2wat` on the actual binaries: `k-go.wasm` and `k-rs.wasm` have **zero** `call_indirect` and no table section at all, and the same holds across `examples/*`. The lowering being optimised does not execute anywhere in the corpus. The arithmetic was sound (one table read instead of three, ~4.5 ns by this file's own upvalue measurement); the opportunity does not exist.

**Host-backed hash tables — the premise was false.** The pitch was to give `real_names` Lua's C hash tables instead of a compiled map. **`real_names` contains no map.** It is `make([]byte, 0, 24)`, an append of `"iron-plate-"`, a base-10 digit loop and FNV-1a over the bytes. And for a guest that did have one, the string-key path is probably a LOSS rather than a win: indexing a Lua table by a key living in guest memory means materialising a Lua string first, so crossing the ABI does not avoid hashing — it adds a copy to it.

**Compressed packed pages — 0.978× on an init heap and 1.091×, i.e. WORSE, on a realistic post-tick one.** `helpers.encode_string` is in the census and is deflate+base64; base64 gives back most of what deflate wins, and a partly-written heap does not compress. Worse, resident `storage` cost rises 36–52% permanently: there is **no `on_save` event**, so the encoded blob has to be serialisable at all times, which means holding it alongside what it encodes. A save-size idea that costs resident memory and CPU to break even is not one.

**Registered-metatable lazy zero pages** were bundled with the above and are dead for a simpler reason: `MEMPACK.restore`'s "an absent page IS zeros" invariant already gives sparse heaps most of what lazy materialisation would, and adding an `__index` metatable to `MEM` puts a metamethod dispatch on the hottest table in the system.

## The counted loop becomes a numeric `for` — **`-opt=1`**

A loop whose trip count is fixed when it is entered stops being a label, an increment, a compare and a goto, and becomes one `FORLOOP` opcode with the counter in a register the loop owns:

```lua
::L1::                                for v1 = v1, v0 - 1 do
v2 = (v2 + v1) % 4294967296.0    -->     v2 = (v2 + v1) % 4294967296.0
v1 = v1 + 1                           end
if v1 < v0 then goto L1 end
```

Measured twice, by two instruments that agree. First by hand — editing emitted Lua into the shape and timing it under `bin/lua52f`, interleaved, with an A/A pair in every batch: `count` 0.858×, `sum` 0.893×, against a 1.3% floor. Then through the real emitter, `bench --opt` against a binary built from the commit before it:

| kernel | `-opt=1` | `-opt=3` | |
|---|---|---|---|
| `constdiv` | **0.78×** | **0.76×** | the driver loop is most of the kernel |
| `count` | **0.85×** | 0.84× | matches the 0.858× hand estimate |
| `sum` | 0.92× | 0.90× | matches the 0.893× hand estimate |
| `chase` | 0.93× | 0.93× | |
| `prng` | 0.94× | 0.97× | |
| `frame` | 1.01× | **0.56×** | at `-opt=3` the body is promoted to nothing, so the loop IS the kernel |
| `fib` | 1.01× | 1.01× | **the control** — recursion, no loop, no change |

`fib` not moving is what says the win is where it claims to be.

### It finds almost nothing in a real guest, and that is the honest headline

**Measured: 2 of 30 loops in the TinyGo bench guest, 1 in `examples/hello`, 3 in `examples/array`, 0 in `grow` and `api`. `pure_prng` through the real guest measures 0.998× — flat against a 0.5% floor.** Neither lowered loop in the bench guest is in a timed hot path.

So this is a kernel win and, today, a no-op for a mod author. It is kept on the same footing as the alignment analysis: sound, gated, and aimed at a shape that is real even where this corpus does not have it — LLVM at `-O0`/`-O1` preserves the counted shape, and `bench/wasm` is written to have it. **Do not quote the kernel ratios as what a guest gets.**

Where the rest went, counted rather than guessed. The first two columns have since been addressed; the census is kept because it is what a later pass should aim at:

| refused because | loops | now |
|---|---|---|
| a second exit — the two-counter `range` shape TinyGo emits | ~10 | **lowered**, with a per-iteration copy |
| the bound is computed inside the loop, not a constant or an invariant local | 7 | **partly lowered** — a bound hoisted into the preheader is invariant by construction |
| the step is not ±1 — the 4× and 16× strides of an unrolled loop | 5 | still refused; needs congruence |
| the back edge is not the last step | 3 | still refused |
| the counter is live after the loop and the exit value is not provable | 3 | still refused |

**Coverage went 2 → 5 of 30, and the benchmark did not move.** `pure_sum` 1.002×, `real_entities` 1.003× against a 0.5–0.7% A/A floor — no gain and no regression. The loops the two extensions unlocked are simply not the hot ones.

That is the finding worth carrying: **the hot loop in a real guest is the 4×-unrolled one, and it is refused for its stride.** A stride of 4 with an `i32.ne` test is a `for` only when the counter hits the bound exactly, which is a **congruence** fact rather than a range fact — and it is provable here, because the bound really is `band(x, 2147483644)`, i.e. `x & ~3`. `internal/analysis/align.go` already solves residues mod 4 and 8 over the same CFG fixpoint, but it publishes only `Addr[i]`, the congruence of a memory access's effective address. Proving `(bound - init) mod stride == 0` needs the congruence of an arbitrary VALUE, so that pass has to record per-step results the way `Ranges` does before this can be attempted.

### The multi-exit form, and why the copy is free

A loop with more than one way out cannot use the wasm local as the `for` variable directly: Lua scopes that name to the loop, so the outer one still holds whatever it held before the loop on any edge leaving from the middle. The `for` therefore gets a control variable of its own and copies it into the local at the top of the body, which makes the local current at every point in the body — so every exit path is right without the pass having to know where the exits are.

**Measured on `count`, whose body is small enough for one extra `OP_MOVE` to show if it were going to: 0.844× with the copy against 0.847× without, A/A floor 1.9%. No detected cost.** The direct form is kept for the single-exit case only because that is what already measured and shipped, not because the copy is worth avoiding.

`TestAMultiExitLoopIsLoweredWithAPerIterationCopy` reports `got "0", want "3"` at every level above zero when the copy is removed — the counter read after an early exit is exactly the observable.

### What the recogniser refuses, and why each would be a miscompile

`analysis.CountedLoops` is deliberately blunt, because this is the one part of the emitter where a wrong answer does not look like one: the loop still runs, still terminates, still returns a number.

- **One branch to the header.** A `continue` in a bottom-tested loop skips the increment, so the header stops predicting the trip count.
- **One write to the counter, and it is the increment.**
- **One exit edge.** Lua's `for` variable does not outlive its loop, so the outer name is stale on any other way out.
- **The bound is loop-invariant** — a constant or a local nothing in the loop writes.
- **Step ±1 only**, so divisibility is free and `ne` needs no congruence proof.
- **A bottom-tested loop must be PROVED to run at least once.** wasm runs its body before testing; Lua's `for` tests first. `TestABottomTestedLoopEnteredPastItsBoundStillRunsOnce` is that case written out — a loop entered with the counter already past its bound — and it reports `got "0", want "1"` at every level above zero when the proof is removed.

Two things about the emitted form are load-bearing and neither is obvious:

- **The control variable reuses the wasm local's name.** Lua scopes it to the loop, so the body sees a fresh local shadowing the outer one — which is what the body wants, since no lowered loop writes its counter. And `for v1 = v1, …` reads the OUTER `v1` for its initial value, because Lua evaluates the header expressions in the enclosing scope before creating the loop variable. That is what lets the lowering work without knowing what the counter started at.
- **`i32.sub n 1` is not a stylistic variant of `i32.add n -1`.** Under Invariant A the slot is unsigned, so the add's interval is `n + 4294967295`, which leaves u32 and is clamped to the full range — taking the `n != 0` the guard proved with it. The sub keeps it, and the sub is what LLVM emits.

Getting it to fire on real output needed two shapes the first version missed, and without them it found **zero** loops in every guest: a `local.tee` as the increment, and a **bare value used as the branch condition** with no comparison step at all (`br_if $top (local.tee $i (i32.sub (local.get $i) 1))`), whose bound is an implicit zero no step holds.

It also needed a fix in the range analysis: `isCompare` was missing `OpI32Ne`, which made `refine`'s own `OpI32Ne` case unreachable dead code. A comparison never recorded can never be resolved back to a guard, so every `!=` test was discarded before it could refine anything — losing `n >= 1` on exactly the back edge a countdown closes with. That fix is codegen-neutral on its own: it left the bench guest's generated Lua byte-identical.

### Invariant B is bent here, and only here

The `for` control variable is the one local the emitter declares after the prologue. The invariant exists because Lua rejects a goto that jumps INTO a local's scope, and wasm's structured control flow cannot name a label inside a loop body from outside it — so the rejected case is unreachable by construction. `third_party/lua-5.2.1/sandbox_check.lua` asserts it rather than arguing it: a goto out of a `for` body to a function-level label, a backward goto across a `for`, a label inside a body jumped to from inside it, that jumping IN is rejected, and both halves of the scoping rule above.

`--fuel` still charges per iteration: the charge moves INSIDE the `for`, where the iterations are. `TestFuelIsStillChargedPerIterationInsideAFor` pins it, because in front of the loop it would charge once for the whole trip.

## A guest's memcpy becomes the runtime's — **`-opt=1`, 3.94× on a copy-heavy guest**

A guest toolchain that cannot emit bulk memory ships compiler-rt's `memcpy` as a hand-rolled byte-and-word loop, and that loop compiles to Lua and runs one interpreted iteration per word. `mem_copy` is a Lua loop over the word table with one bounds check and one dirty-page update for the whole span — 3.5 ns/byte against 173 for a byte loop. So the whole body is replaced:

```lua
-- body replaced by the runtime's own mem_copy
F[2] = function(v0, v1, v2) mem_copy(MEM, MEMSIZE, v0, v1, v2) return v0 end
```

**Measured, real compiler, checksum-compared: 0.254× on a copy-heavy TinyGo guest** — a 64 KiB `copy()` repeated 400 times, 415 ms → 106 ms. **And flat on every kernel in `bench/guests`**: `real_grid` 1.008×, `real_names` 1.001×, both inside a ~1% A/A floor, because those copy tens of bytes at a time and pay call overhead rather than per-byte cost.

That split is not new — it is exactly what [`agents/guests.md`](guests.md) records for the TinyGo **bulk-memory custom target**, which reaches the same place by making the guest emit `memory.copy` and measured 5.78× on a copy-heavy guest and flat on these kernels. The difference is that this needs nothing installed and no `$TINYGOROOT/targets` packaging, which is the reason that target was judged not worth adopting.

It is also the only pass here that makes output **smaller**: the bench guest goes from 3,522 to 3,021 lines, −14.2%, because two byte loops stop being compiled at all. Every real guest tried gets one or two substitutions.

### Why a NAME is allowed to select it, and what stops that being a miscompile

The name section is a **custom** section. It carries no semantics, a producer may omit it, and nothing stops a module calling any function anything — so a name alone must never change what a program computes. Here it only nominates a *candidate*, which then has to survive `isMemoryShuffle`: no call of any kind, nothing touching a global, no float arithmetic, no `memory.grow`/`size`, no existing bulk-memory op, and at least one store. It cannot prove the function IS memcpy — that would need to execute it, and this project deliberately has no wasm interpreter to execute it with — but a same-named function that is doing something else has to also be shaped like a self-contained byte mover before its body is discarded. `TestASameNamedFunctionThatIsNotAMemoryShuffleIsCompiled` walks five such functions at every level.

What the substitution changes, stated exactly:

- **Overlap.** C `memcpy` is undefined on overlapping ranges; `mem_copy` has `memory.copy`'s memmove semantics and is defined. Strictly more defined, never less — which is also why `memmove` is accepted under the same name set. `TestAnOverlappingCopyIsMemmoveSemantics` pins which behaviour the emitted code now has, rather than leaving it to be discovered.
- **Trap timing.** The byte loop writes bytes and then traps on the one that is out of range; `mem_copy` checks the whole span first and writes nothing. Only reachable from a guest that was already going to trap, and the spec's own `memory.copy` takes the check-first side.
- **The dirty-page mark.** `mem_copy` and `mem_fill` mark their whole span in one update, which is the same guarantee the byte loop gave through `st8b`. This is the hazard class the audit found twice, most recently in `fk_wstr`, so it is asserted rather than assumed: `TestASubstitutedCopyReachesTheSave` runs the control.lua protocol through a stand-in `storage` and reports `dirty 0 / word 0` — the exact desync signature — when `mem_copy`'s marking update is removed.
- **Nothing else.** Both return their first argument, as C does.

`-opt=0` compiles the guest's own body, so it remains the bisect reference.

## The inlined i32 store — **`-opt=3`, 0.988× on `chase`**

The mirror of the inlined load, and it follows from the same breakdown: 34% of a memory access is the call. Every store in the project was a helper call at every level until now.

```lua
t0 = v0 + 4
t1 = (v1 + 1) % 4294967296.0            -- only when the value is composite
if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end
if t0 % 4 == 0 then MEM[t0 / 4 + 1] = t1 % 4294967296.0 else st32(MEM, MEMSIZE, t0, t1) end
```

**Measured on a quiet machine, and it is the weakest of the three inlinings:** `chase` 0.988× (95% CI [0.980, 0.995] against an A/A noise floor of ±0.51%) — real, but 1.2%, and `sum` showed **no detected change at all**. The estimate written here before the measurement said as much, for a structural reason: `chase` and `sum` are the only bench kernels with an `i32.store` in their hot loop, and neither is store-*dominated*, so the load's 1.36×/1.18× was never on offer. Take it as the going rate for inlining a lowering that is already a statement.

**It does not move `real_grid`.** `agents/benchmarks.md` attributes that gap to *byte* stores, and `i32.store8` is untouched here — see below.

### Three things it has to get right, and each has a test

**The dirty-page mark.** In `--persist=packed` the save carries only the pages the set says were written, and the whole reason that marking lives inside `st8b`/`st16`/`st32` is that every store funnels into them. Inlining `st32` walks around the funnel, so the emitted code marks the page itself — the cached-page test copied from the prelude verbatim rather than re-derived, and gated on `MEMDIRTY` exactly as `st32` gates it, **unconditionally rather than only in packed mode**, so the emitter never has to know which mode is in force and nothing breaks the day `arm` becomes reachable from somewhere new. A missed update is not an error anywhere: the store lands in the live table, the session runs perfectly, and the value is simply absent from the save — a desync, days later, with nothing pointing at the store. `TestTheInlinedStoreStillDirtiesItsPage` asserts both halves of the consequence (`flush` reports the page rewritten, and a second instance restoring from the pages sees the word) and reports `dirty 0 / word 0` at `-opt=3` when the line is removed.

**`t0`/`t1` must be DECLARED.** Same trap as the load, one section up, and the same fix: the level check is at the call site, not inside `needsScratch`, so `-opt=0` still declares nothing it did not declare in M4. `TestAFunctionUsingAScratchRegisterDeclaresIt` gained `i32.store` and `i32.store composite` and fails on both when the call site is reverted.

**Two operands, evaluated once and in order.** A store is the first inlined lowering with two of them. `t0` takes the address and `t1` the value, so wasm's order — address, value, access — survives, and a composite value is materialised rather than printed into both arms of the alignment branch; a bare name or numeral is left in place, because naming those twice is free. Neither operand can trap, and that is a property of the peephole rather than luck: `stepTraps` is true for every store, so `forwardPeephole` has already refused to forward a trapping expression into either position. That is what makes it safe for the bounds check to sit *after* both operands without changing which trap a guest sees. `TestTheInlinedStoreEvaluatesACompositeValueOnce` pins the materialisation.

### Two deliberate conservatisms

**The `% 2^32` on the aligned path is kept**, even though nothing today can hand a store an out-of-range value: `absorbs` in `internal/analysis/rng.go` does not list any store, so a wrap is never deferred into one. Keeping it makes the inlined form behaviourally *identical* to the call for every possible input, which is the strongest property to hand a reviewer given that level divergence is the bug class that has bitten twice — and `MEM` is separately required to hold genuine u32 words, because packed mode feeds them to `string.pack("<I4")`, which raises on anything else. It could be dropped, guarded on `b.w.ArgRange(i, 1).FitsU32()` so it comes back automatically if a store is ever made an absorbing consumer. Worth perhaps 12% of the win; not taken blind.

**`i32.store8` and `i32.store16` stay calls.** `st32`'s aligned fast path is one table write, which is why inlining it is three lines. `st8b`'s body is a read-modify-write of the containing word: it needs the byte's position within the word, the word index, a power-of-two divisor out of `P2` and the old word all live at once — five values against the two scratch registers a function declares. Expanding it means widening the scratch file, which is its own experiment with its own measurement, not a rider on this one.

### What it costs in size

Generated Lua at `-opt=3`, function bodies only (the prelude is constant), real TinyGo guests built with `fk.BuildFlags`:

| guest | bytes | lines |
|---|---|---|
| `examples/hello` | 129,964 → 149,188 (**+14.8%**) | 3,019 → 3,306 (**+9.5%**) |
| `examples/grow` | 31,184 → 38,991 (**+25.0%**) | 1,018 → 1,140 (**+12.0%**) |
| `examples/array` | 233,406 → 283,444 (**+21.4%**) | 6,216 → 6,929 (**+11.5%**) |
| `examples/api` | 108,143 → 122,957 (**+13.7%**) | 2,351 → 2,572 (**+9.4%**) |
| bench kernels, all seven | 10,446 → 11,158 (+6.8%) | 366 → 380 (+3.8%) |

**Bytes grow roughly twice as fast as lines**, which is the shape to expect: one short call becomes four long statements. It is a larger relative cost than the inlined load's +9.5%/+8.8%, and it stacks with it. Only `chase` and `sum` move among the kernels; the other five have no `i32.store` at all, which is the control saying the growth is where it claims to be.

Chunk size is still not the binding constraint — `load()` parses ~40 MB/s, so 149 KB is under 4 ms — but the inlined load and store together have roughly doubled a guest's generated Lua against `-opt=0`, the 8-byte expansion below adds to that again, and the next one should be costed against the running total rather than against zero.

## The inlined 8-byte access — **`-opt=3`, 0.832× on `dot`**

The same trade one width up. After the pair-access fix `ld_f64` and `st_f64` were still function **calls**, so `-opt=3` expands the aligned path at the use site for all four 8-byte accesses too: `f64.load`, `f64.store`, `i64.load`, `i64.store`.

```lua
t0 = v0
if t0 < 0 or t0 + 8 > MEMSIZE then trap_oob() end
if t0 % 4 == 0 then
  t1 = t0 / 4 + 1 t2 = MEM[t1 + 1] t1 = MEM[t1]
  t3 = t2 % 2147483648.0
  t3 = (t3 - t3 % 1048576.0) / 1048576.0
  if t3 > 0 and t3 < 2047 then
    v6 = (t2 >= 2147483648.0 and -1.0 or 1.0) * ldexp(…, t3 - 1075)
  else v6 = ld_f64(MEM, MEMSIZE, t0) end
else v6 = ld_f64(MEM, MEMSIZE, t0) end
```

**Measured on a quiet machine: `dot` 0.832×** (95% CI [0.831, 0.837] against an A/A noise floor of ±1.52%), and on the real TinyGo guest `pure_dot` went 18.03× → 12.78× against hand-written Lua. **That is far more than the estimate**, which reasoned that the f64 load goes from two calls (`ld_f64`, then `ldexp` inside it) to one and so could be worth **at most** half of an f64 access's call cost. Being well past that ceiling says the missing time was never the call: the **reassembly** — the exponent extraction and `ldexp` round trip the inlined form does with three arithmetic lines — was costing more than the call it sat inside. That matters beyond this change, because it is the same reassembly the f64-typed shadow below is trying to delete outright.

Four things about the shape are load-bearing:

- **The fast path serves NORMAL doubles only** — `e` in (0, 2047). Zero, subnormals, infinities and NaNs fall back to the helper. That keeps the inlined arithmetic one straight line, and it makes exact-NaN mode correct for free: every value that could be a **boxed** NaN takes the fallback to `xld_f64`, and the inlined path has no box to return.
- **The store puts its VALUE in a scratch before the bounds check.** wasm evaluates address, then value, then performs the access, so a trapping value expression must trap before the store's own out-of-bounds check — and the two carry different trap codes. Checking first would swap them.
- **Four scratches, all declared.** `scratchCount` returns 0, 2 or 4; `TestTheInlinedEightByteAccessDeclaresEveryScratchItUses` walks every function at every level and fails if a `t2` or `t3` is used without being in the `local` run. This is the bug that already shipped once, one width down.
- **`--persist=packed` does not get the inlined STORE.** See below; it is the crux, not a footnote.

Code size, measured (this is structural, not a timing): the `array` TinyGo guest grows **+9.25% in bytes and +7.0% in lines** at `-opt=3` — 18 f64 loads, 8 f64 stores, 31 i64 loads and 80 i64 stores expanded. The `api` guest, which has no f64 in memory at all, grows +2.7% on its 18 i64 loads alone. Comparable to the i32 load's +9.5%, and paid by every guest whether or not it is f64-heavy.

### The dirty-page set is why packed keeps its WIDE stores out of line

`--persist=packed` tracks a set of dirty pages (`MEMPACK.mark`, with `DPLO`/`DPHI` caching the last page marked) and flushes only the pages in it. What makes that sound is a funnel: a guest cannot write memory except through `st8b`, `st16`, `st32`, `st64`'s aligned path, `mem_copy` or `mem_fill`, and each marks its own pages.

**An inlined store writes `MEM` directly and walks past all six.** The bytes land in the live table, the flush never learns the page changed, and the value is silently missing from the save — surfacing one load cycle after the code that caused it. So `inlineWideStores` is gated on the mode, not just on the level: packed keeps calling `st64`/`st_f64`, and every other mode gets the expansion. `TestAnEightByteStoreInPackedModeReachesTheSave` fails (`f false, i false`) when the gate is removed, which is how the hazard was confirmed rather than assumed; `TestPackedModeKeepsTheEightByteStoreOutOfLine` pins the gate itself.

Inlined **loads** are unaffected in every mode: nothing records a read.

**The 4-byte store took the other route, and the asymmetry is deliberate but not principled.** Two sections up, `emitInlineStore32` emits the page mark *into the generated code* and is therefore available in every mode; `inlineWideStores` refuses the expansion under packed instead. Both routes are correct and both are tested, but they are two answers to one question, and they are that way because two people wrote them — `fk_rt.lua` says so at the `MEMDIRTY` declaration rather than leaving the next reader to infer a rationale that was never there. **The page set made the mark one line instead of four and did NOT settle this**: the objection to a second copy was never its length. Whoever settles it is choosing between duplicating an invariant into generated Lua, which is the cost the 8-byte route refused to pay, and giving up an expansion in one mode, which is the win the 4-byte route refused to give up. This file's own repeated lesson argues for the second: an invariant maintained in two places is an invariant that drifts.

## Sharded linear memory — **`-opt=3`, and every access form changed**

Linear memory is a vector of 2¹⁹-word shards, always. The design, its refusals and the measurements are [`agents/sharding.md`](sharding.md); what belongs HERE is what each pass above now emits, because every one of them touches memory.

**The one sentence that matters: THE BOUNDS CHECK IS THE SHARD TEST.** `SHBOUND` is `min(MEMSIZE, 2097152)`, so below 2 MiB it **is** `MEMSIZE` and the opening test of every access is the bounds check it already carried rather than an addition to it. An implementation that emits the shard select *in addition to* the bounds check has thrown the result away — measured 1.46–1.59× below the wall, and the e2e harness still carries it as the `slow` control arm precisely so that a future change which quietly un-merges the two says so.

### The three forms

| form | when | emitted |
|---|---|---|
| **static fold** | the address operand is a compile-time constant and 4-aligned | `v9 = S1[513]`, or `MEM[3][513]` above shard 0 |
| **guard-hoisted** | inside a loop guard whose span is proven within one shard | `v9 = ls41_0[lw41_0 + 3]` |
| **shard-0 fast path** | everything else | `if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then v9 = S1[t0 / 4 + 1] else … end` |

**A constant address folds to a constant SHARD, not to shard 0.** That is 15.5% of this repo's corpus and 26.2% with the downstream mod — the biggest single class, and 952 of its 962 sites are literal addresses rather than a range-analysis result. The address is still evaluated into `t0` and the bounds check is still emitted: the range analysis proves the VALUE, not that the expression is free of effects, and `MEMSIZE` is a runtime quantity a host `adopt` can move. What the fold buys is the index arithmetic and the shard select.

### The two SHAPES, and why there are two

`if fast then A else B end` compiles in Lua 5.2 to test, jump-to-else, A, **JUMP-TO-END**, else, B — so the fast path pays one unconditional jump the flat form's `if bad then trap_oob() end` did not. Where the slow arm can end in a CALL there is nothing to be done about it, and the jump is paid on a path that was going to be a call anyway. Where the tail is ONE shared expression — a byte load, or a 4-byte load whose alignment the congruence analysis proved — the emitter uses the **no-else** form instead:

```lua
t2 = S1
if t0 < 0 or t0 + 1 > SHBOUND then
  if t0 < 0 or t0 + 1 > MEMSIZE then trap_oob() end
  t1 = t0 % 2097152
  t2, t0 = MEM[(t0 - t1) / 2097152 + 1], t1        -- t0 becomes a WITHIN-shard offset
end
t1 = t0 % 4
t2 = t2[(t0 - t1) / 4 + 1]
```

The slow arm rewrites `t0` into a within-shard offset and `t2` into that shard, so the tail is shared and there is nothing to jump over. **A proof of alignment now buys two things rather than one**: it drops the `% 4` branch as it always did, and by leaving the slow arm call-free it collapses the whole access into this form.

### What the guard gained, and what it gives up

`emitLoopGuard` adds **one conjunct per base** and **one hoisted local per base**. The conjunct is not "shard of the first byte equals shard of the last" — that is two floors of two compound expressions. It is the same predicate with the algebra done:

```lua
<base> % 2097152 + t0 * <stride> + <MaxEnd> <= 2097152
```

one modulo and one multiply-add, reusing the `t0 * stride` shape the bounds conjunct beside it already prints. **Sound only because a stride is non-negative**, which `analysis.LoopGuards` enforces outright (`c < 0 || c%4 != 0` refuses the base), so the far end of the walk is always the high end and there is no downward case to fold in.

**A span that CROSSES a boundary fails the guard and every access in that loop takes the shard-0 fast path.** That is the stage-B answer and it costs little — a guarded loop's span is almost always far smaller than 2 MiB, so it crosses with probability about span/2 MiB. Strip-mining the loop into one guarded run per shard piece is the general fix and is stage C's.

`ls41_0` initialises to `false` rather than to a plausible table, and the asymmetry with `lw41_0`'s `0` is deliberate: a word index is STEPPED on a path where the guard is false, so `nil + 8` has to be impossible, while a shard table is only ever INDEXED under the flag. Seeding it with a real table would turn a missing seed into silently wrong data — the failure class the guard audit exists for. `auditGuardSeeds` checks every `ls` a chunk reads is also seeded from `MEM`.

### The persistence gating is unchanged, and the reason is arithmetic

A page is 4 KiB and a shard is 2 MiB, both powers of two with the page smaller, so **a page can never straddle a shard boundary** — 512 aligned pages per shard. The dirty-page set therefore survives untouched: it still indexes byte addresses, `MEMPACK.mark` is the same call over the same span, and the inlined 4-byte store emits the same mark line. `--persist=packed` and `--gc=collected` still keep the 8-byte store out of line, for exactly the reasons they did before.

**The mark is emitted in BOTH arms of the inlined store rather than hoisted in front of the merged test**, and that is not tidiness: hoisting it would mark a page for a store about to trap, and for a NEGATIVE address, which floors to a negative page number that reaches `DPQ`, the flush and `storage`.

### What it costs, measured

In game, paired, through the real emitter against a pre-sharding compiler: **1.007× on the median over 1,596 ticks at 2 MiB**, and **17.8×** at 6 MiB. Host-side the corpus is 0.99–1.03× except `real_names`, the allocation kernel, at 1.10×.

The floor is **+1 VM instruction per inlined access**, and it is worth knowing why: `MEM[k]` where `MEM` is an upvalue compiles to a single **`GETTABUP`**, so the flat form was already optimal and a shard decision cannot be free. That instruction is arithmetic or a branch, never a table read — which is why it is 1.10× host-side and invisible in game: `agents/sharding.md` §2 measured Factorio's table read at 4–6× the oracle's with the loop machinery at 1.04–1.10×, so non-table work is proportionally much cheaper there. **Do not accept a host-side ratio as the below-wall verdict.**

---

## The f64-typed shadow — where the invalidation point would have to be

The open question is whether a shadow array holding each 8-byte-aligned slot as a native Lua double pays for itself. Two halves: what it would save, and what it would cost to keep coherent.

**What it would save is now measurable rather than arguable.** `scratchpad/harness/f64-breakdown.lua` builds one dot-product iteration six ways — `call` (pre-change), `inline` (this change), `noldexp` (inline with a power-of-two table read in place of the `ldexp` call), `nocheck`, `shadow` (one table read, no reassembly), `nat` (the M0 ceiling). All six agree on the checksum to 17 significant figures, which is the part a busy machine can establish; **none of them has been timed.** `shadow` minus `inline` is the prize, and `inline` minus `noldexp` says how much of what is left is the second call rather than arithmetic only the shadow can remove.

**The coherence problem, stated precisely.** A shadow entry is stale the moment anything writes the underlying words by any other route: an `i32.store` to either half, an unaligned store straddling the slot, `memory.copy`/`fill`, a `memory.grow`, a host `memio` write, a packed `restore`. The natural design is the same one the page marking uses — a single funnel, invalidating `SHADOW[a/8+1]` (and the neighbour an unaligned write also touches) on every store — and the funnel was already there, which is what made this look cheap.

**It is not there any more, and that is the crux.** All three `-opt=3` expansions bypass the runtime helpers by construction:

| access | -opt≤2 | -opt=3 |
|---|---|---|
| `i32.load` | `ld32` | inline `MEM[t0/4+1]` |
| `i64`/`f64` load | `ld_f64` / two `ld32` | inline `MEM[t1]`, `MEM[t1+1]` |
| `i32.store` | `st32` | inline `MEM[t0/4+1] = …` — **every mode** |
| `i64`/`f64` store | `st64` / `st_f64` | inline `MEM[t1] = …` (except packed) |

Loads bypassing the funnel is harmless for a shadow — a read invalidates nothing. **Both stores break it**, and the 4-byte one breaks it worse. The 8-byte store at least stays in the funnel under packed, and its shadow update would be free information anyway: it has the double in hand before it takes the double apart. `i32.store` is the aliasing case that actually *needs* invalidating — an i32 write into one half of an f64 slot leaves a stale double behind — and it is inlined at `-opt=3` in **every** persistence mode, with no existing gate to hang a "no shadow" condition on. Anyone costing the shadow should start there: the funnel it was going to be built on no longer exists, and the option chosen below has to be applied to both stores, not just the wide one.

So a shadow has three options, and they should be priced against each other rather than one being assumed:

1. **Put every store back in the funnel.** Revert both inlined stores, or gate each on "no shadow" the way the 8-byte one is already gated on "not packed". Cheapest to reason about, and the price is now known rather than guessed: the 4-byte store is worth 1.2% on `chase` and nothing on `sum`, so surrendering it costs almost nothing, while the 8-byte store's share of `dot`'s 0.832× is the part that would actually hurt.
2. **Emit the invalidation inline.** Every inlined store also writes `SHADOW[…] = nil` (or the new value, for the 8-byte case, which is a *write through* rather than an invalidation). This is what the packed mode gate deliberately refused to do for the page mark, and the reason applies here too: two copies of an invariant in two languages, drifting independently. The difference is that a shadow's version is one table write with no MEMDIRTY-style flag to read, and an 8-byte store's shadow update is free information — it already has the double in hand before it disassembles it.
3. **Make the shadow lazy and self-validating**, e.g. a version word per page. Pays a compare per load, which is most of what the shadow was trying to avoid.

Option 2 is the only one that keeps both wins, and its real cost is not performance but the thing this file keeps re-learning: an invariant maintained in two places is an invariant that will drift. It wants the same treatment the scratch declaration got — a test that walks every store shape at every level and asserts the invalidation is present, rather than a benchmark that would notice.

None of this is scheduled. What has changed is that the question now has a harness and a named crux instead of a sentence.
