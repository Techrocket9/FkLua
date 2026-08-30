# 4c — the data-stage function splitter

**Round 4, design-and-measure. Verdict: THE RECORDED DESIGN CANNOT WORK, FOR A
REASON NOTHING HAD CHECKED — and a much cheaper mechanism can. Recommend
building the cheaper one; recommend correcting the recorded row either way,
because it is currently an invitation to spend a milestone on a pass that
cannot fix the thing it is for.**

`CLAUDE.md`'s `function split` row is the starting point and it is a careful
piece of design: split as an IR pass before analysis, at a point where the
operand stack and the control stack are both empty, outline the span into a
function taking the live wasm locals as parameters and handing the written ones
back through multiple return, and the hard part is the size estimate.

Three of its claims are now measured and two of them are wrong.

| the row says | measured |
|---|---|
| "The split point is where the operand stack AND the control stack are both empty" | **Sound, and VACUOUS.** A function that reaches the jump limit has **4** such points and **all four are outside the jump's span**. There is a theorem behind it: every wasm branch targets an *enclosing* construct, so the control stack is ≥ 1 at every step strictly inside any jump's span. **A zero-control-depth split can never shorten the widest jump in a function.** |
| "A function with 40 locals crosses ~40 arguments and ~40 returns per split … against Lua's 255 registers, which is headroom but not much" | **The ceiling is 124, not 255** — measured — and the live set is nowhere near it. Across 118 functions in five real guests: median **6** slots, p99 **18**, max **30**. On the LLVM-inlined data-stage shape the row exists for: **2**. The arity cost the row names as one of its two fall-outs does not arise. |
| "leaves every existing golden file untouched" | True, and **stronger than it needs to be: there are no codegen goldens at all.** No emitted Lua is committed anywhere. |

And a fourth thing, which is not in the row and is the finding this pass turns
on: **`funclimit.go`'s model of what Lua refuses is incomplete.**

---

## The measurements

Two harnesses, both committed under `scratchpad/r4/`.

### Where a split point can be, and how wide the live set is

`scratchpad/r4/livesets.go.txt` and `livesets-repro.go.txt` — a throwaway
`main` package over `internal/ir`, run against the repo's own guests. It
recomputes control depth (a 15-line forward scan; `ir.Step.StackDepth` already
carries the operand depth, which is half the predicate for free) and counts the
points where both are zero.

**The repo's real guests** — `examples/api`, `callcost`, `gcbench`, `retain`
and `bench/guests/go`, 118 functions, biggest 3,942 steps:

| | |
|---|--:|
| live-set slots (params + locals, an i64 counting two): median | 6 |
| p90 / p99 / max | 14 / 18 / **30** |
| functions whose live set exceeds Lua's 124-slot call ceiling | **0** |
| functions with fewer than two zero-depth split points | 5 of 118 (4.2%) |

**And the shape the feature is for.** A synthetic overhaul-scale data stage —
twenty sections of sixteen prototypes each, all called from one export,
compiled with TinyGo at `-opt=2` so LLVM inlines them, which is exactly the
concentration `agents/codegen.md` records as what reaches the limit:

| `main.onData` | |
|---|--:|
| IR steps | **143,643** |
| live set at a split point | **2 slots** |
| zero-depth split points | **46,175** |

So on the shape that matters the live set is two slots and the pass would have
forty-six thousand places to cut. The arity worry evaporates.

**But that function is JUMPLESS.** Emitted at `-opt=3` it is 18.9 MB of Lua in
one function with a widest goto-to-label span of **zero**, and it compiles
happily — which is `TestAJumplessFunctionIsNotRefusedHoweverBig` met from the
other side. `fkdata.Extend` crosses an import, so there are no bounds checks for
LLVM to merge into one trap block, and it is the merged trap block that makes
the long jump.

**So the honest corpus is the funclimit reproduction**, which is that shape: a
`br_if` near the top of one enormous block, jumping to the trap at the bottom.
Measured on it, 33,767 ops, 135,076 steps:

| | |
|---|--:|
| steps with an empty OPERAND stack | 33,774 |
| steps with an empty CONTROL stack | **4** |
| steps with **both** empty — the split points | **4** |

The four are the function's prologue and its tail. **None is inside the span
the jump crosses**, so splitting at any of them leaves the widest jump exactly
as wide as it was.

**This is structural, not an artifact of one fixture.** In wasm a branch names
an enclosing construct by depth. The construct is open across the whole
distance from the branch to its label. Therefore the control stack is ≥ 1 at
every step strictly between a `goto` and its `::label::` — always, in every
module. A zero-control-depth point is by construction outside every jump's
span, and moving code out from outside a span does not shorten the span.

### The arity ceiling, which the row also gets wrong

Measured directly against `bin/lua52f`, since Factorio's Lua is what decides
it (`scratchpad/r4/`, reproduced inline below):

| shape | ceiling |
|---|--:|
| `local function f(a1..an) return end` — params only | **200** (`LUAI_MAXVARS`) |
| `local function f(a1..an) return a1..an end` — the OUTLINED shape | **124** |
| a caller declaring L locals making a call passing and receiving n of them | `L + n ≈ 247` |

So a caller at the emitter's own `ir.MaxSlots = 180` has room for **67**
values in a call, and the outlined function itself caps at **124** whatever the
caller does. Both are well above the measured live sets (max 30, and 2 on the
target shape) — so this is a *check the pass would owe*, not a constraint that
binds. The row's "255 registers, headroom but not much" is the wrong number in
the pessimistic direction, and the real answer is that there is plenty.

### What Lua actually refuses — `funclimit.go`'s model is incomplete

`scratchpad/r4/jumpladder.py`, piped into `bin/lua52f`. Output committed as
`scratchpad/r4/RESULTS-jumplimit.txt`:

```
one jump over 131071 instructions (Lua's stated limit)     LOADS, returns 0
one jump over 150000 instructions                          REFUSED -- control structure too long
BARE ladder,  2 hops x 50000 = 100000 total                LOADS, returns 100000
BARE ladder,  4 hops x 50000 = 200000 total                REFUSED -- control structure too long
BARE ladder, 10 hops x 50000 = 500000 total                REFUSED -- control structure too long
SEPARATED ladder,  4 hops x 50000 = 200000 total           LOADS, returns 200000
SEPARATED ladder, 10 hops x 50000 = 500000 total           LOADS, returns 500000
UNREACHABLE-placed ladder, 10 hops x 50000 = 500000 total  LOADS, returns 500000
```

Read the two middle blocks together. A ladder of trampolines —
`goto T1`, `::T1:: goto T2`, … `::Tm:: goto L0` — whose **every hop is 50,000
instructions, far under the 131,071 limit**, is REFUSED once the TOTAL crosses
the limit. Every goto-to-label distance in that program is small;
`funclimit.go`'s scan would measure 50,003 bytes and pass it.

**The mechanism, and it is the whole of the fix.** `::T:: goto X` with nothing
between the label and the goto puts the pending jumps into one list —
`luaK_patchtohere` concatenates into `fs->jpc`, and the very next instruction
emitted is the relaying jump, so the two lists merge and the ladder is patched
as ONE jump to the final target. **One ordinary statement between the label and
the goto discharges the pending list**, the hops become independent, and a
ten-hop ladder spanning 500,000 instructions — 3.8× Lua's single-jump limit —
loads and computes the right answer.

The last line is the design: with each trampoline placed where nothing falls
through, the ladder costs the straight-line path nothing at all, and the
program still returns 500,000, which is the body having run and no trampoline
having been entered.

---

## The design: relay the jump, do not move the code

> **Insert a ladder of trampolines to break a too-long jump, each of the form
> `::Tk:: <one statement> goto Tk+1`, with hops sized under the threshold and
> each trampoline placed at a point control cannot fall into.**

Everything the recorded design finds hard, this does not have:

| the outlining design's cost | the ladder |
|---|---|
| must be an IR pass BEFORE analysis, because a split renumbers steps and invalidates `analysis.Plan`, `Wrap`, `Align`, `CountedLoops`, `LoopGuards` and `Frames` — all keyed by step index — and the emitter's own `lg%d`/`lw%d`/`fk%d` names, which are spelled from step indices | **inserts statements; renumbers nothing.** It runs at the very end, on the emitted text, exactly where `checkJumpSpan` already runs, and every analysis result is untouched by construction |
| "the one genuinely unpleasant part is the SIZE ESTIMATE" — the pass runs on IR and the budget is in emitted bytes, so it needs a second proxy stacked on the first | **there is no estimate.** The transform runs after emission, so it measures the real bytes, which is the same measurement `funclimit.go` already makes |
| ~40-60 values crossing per split, against a Lua ceiling | **nothing crosses** |
| an early `return` inside the outlined span returns from the wrong function, needing a sentinel and an `if` at the call site, interacting with `b.unwind()`'s `FP = fb` | **there is no second function**, so no frame, no unwind, no sentinel |
| outlined functions change the call-count census `analysis.HotCallees` sees, so `-opt=3` upvalue promotion can shift | **no function is added** |
| new functions need indices consistent with `F[…]`, `m.Exports`, `TBL`/`TSIG` and `Start` | **none of it** |

What it does cost:

- **One new name family**, the trampoline labels. `agents/codegen.md`'s
  exhaustive table is where that is decided and `TestNoNameFamilyCanCollideWith
  Another` is what enforces it: the family needs a prefix nothing else uses,
  and label indices are dense small numbers so `::T3::` would be the guard's
  mistake all over again. `::LTk::` off a per-function trampoline counter.
- **One statement per trampoline**, executed only when the trampoline is
  entered — which for the dominant shape is a trap, terminal and never hot.
- **A placement rule.** Placed at a reachable point, a trampoline needs a
  `goto Sk` / `::Sk::` guard to jump over it, which is one unconditional jump
  on the straight-line path per hop. Placed at an unreachable point — right
  after any `do return … end` or unconditional `goto`, of which a big emitted
  function has thousands — it costs the straight-line path **nothing**, which
  the last measured line above demonstrates. The first cut should take the
  guarded form (simpler, and 3 extra jumps in a 1.5 MB function is nothing) and
  the unreachable placement is a refinement with a measurement already in hand.

### What it does NOT solve, said plainly

**A single basic block longer than the limit with no relayable jump in it.**
The ladder relays a jump; it cannot shorten the distance a `br_table` arm or a
loop back-edge must travel if the intervening code genuinely has to be there.
For the recorded failure — a merged trap block reached by `br_if` from
everywhere — the ladder is exact, because every one of those branches goes to
one label and the ladder relays all of them. For a hypothetical function whose
single `loop` body exceeds the limit, only outlining helps, and that shape has
never been observed. **The check stays** as the backstop for it, and its
message should gain a line saying the ladder was tried.

### And `funclimit.go`'s scan has to be fixed regardless

This is the part that is worth taking even if nothing else here is built. The
scan measures one goto-to-label distance. Measured above, **a chained ladder
whose every such distance is small is still refused**, so a guest that happened
to emit `::A:: goto B` (a label immediately followed by a jump) could be
refused by Lua after passing the check — the exact failure the check exists to
prevent, with the player seeing `control structure too long` against generated
Lua.

**Does the emitter emit that shape today? Measured: no.** Three real guests
(`examples/api`, `bench/guests/go`, `examples/gcbench`) compiled at `-opt=0`,
`2` and `3` — **zero** instances of a label immediately followed by a `goto`,
in all nine outputs:

```sh
for w in api kernels gcbench; do for o in 0 2 3; do
  ./bin/fklua compile /tmp/g-$w.wasm --opt=$o -o /tmp/chk.lua
  # count lines where a ::label:: is followed immediately by a goto
done; done
```

So the blind spot is **latent, not live**, which is the right thing to know
before deciding urgency: it is worth a comment and a test rather than a
correction to a shipped guest. **And it constrains the relay directly** — the
relay's own trampolines are exactly that shape, which is why the separating
statement is not a workaround but the thing that makes them legal. A relay
built without it would have created the blind spot in order to fall into it.

---

## Alternatives considered and rejected

| alternative | why not |
|---|---|
| **The recorded outlining design, at zero-depth points** | Measured: the shape that reaches the limit has four such points and all four are outside the span. It cannot shorten the jump it exists to shorten. |
| **Outlining at NON-zero control depth**, with the outlined function re-opening the nesting and returning a branch-target code the caller switches on | This is what a real outliner does and it works. It is also catastrophic here: for the dominant shape every bounds check inside the outlined region branches OUT of it, so every bounds check becomes a return plus a dispatch — on the hottest operation in any guest. Rejected on the shape, not on the difficulty. |
| **Refusing at a lower threshold and telling the author to split** | This is what ships, and it is the standing mitigation. It is right for a hand-written guest and wrong for a GENERATED one, which is the survey's own reason for the item: *"a generated data guest, say, where the author does not control the section boundaries."* |
| **Emitting `//go:noinline` advice per section automatically** | The compiler cannot write the author's Go. |
| **Splitting the CHUNK instead of the function** | Chunk size is free (`load()` parses 4 MB in 106 ms) and is not the constraint. |

---

## Implementation plan

**Estimated size: ~180 lines of hand-written code. One to two days including
the corpus test. This is a SMALL feature, where the recorded design is a
milestone.**

### Files touched

| file | what | ~lines |
|---|---|--:|
| `internal/luagen/funclimit.go` | `relayJumps(src) (string, bool)`: find the over-long pairs, choose hop points, insert `::LTk:: <sep> goto LTk+1`, re-measure. Plus the chained-goto fix to `maxJumpSpan` (below), plus a typed error so a caller can tell "would have been refused" from any other failure | 110 |
| `internal/luagen/luagen.go` | the span loop at `:355` gains "relay, then re-check, then refuse" — the transform runs where the check already runs, over `src[e.start:e.end]`, and the spans re-derive | 25 |
| `internal/luagen/loopguard_test.go` | the trampoline label family in `nameFamilies` | 5 |
| `agents/codegen.md` | the name-families table gains a row; "A function that is too big for one jump" gains the relay and the chained-goto correction | 40 |
| `CLAUDE.md` | the `function split` row is REPLACED by what was measured, and the jump-limit section gains the incompleteness finding | 30 |

**Nothing under `internal/analysis/` or `internal/ir/` is touched at all**,
which is the whole difference from the recorded design.

### Test plan

| test | what it pins |
|---|---|
| `TestARelayedJumpLoadsWhereTheDirectOneDoesNot` | the ladder over the funclimit reproduction, handed to `bin/lua52f` — the same instrument `TestWhatTheCheckRefusesIsWhatLuaRefuses` already uses, and it needs `maxJumpSpanBytes` to be a `var` exactly as that one does |
| `TestARelayedFunctionComputesTheSameAnswer` | the reproduction run before and after the relay, results compared. **The correctness argument**, and the reason the harness above prints the return value rather than only "LOADS". |
| `TestATrampolineIsNeverFallenInto` | the guard, or the unreachable placement, asserted as a TEXT property — this is a `lg`/`lw` shaped claim and it is a text property or it is nothing |
| `TestALabelImmediatelyFollowedByAGotoIsMeasuredAsAChain` | the incompleteness fix: the bare ladder must be REFUSED by the check, and the separated one accepted. Red-proven by reverting `maxJumpSpan`. |
| `TestATrampolineLabelCannotCollideWithAnother` | via `nameFamilies`, over every index |
| `TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay` | the corpus half: nothing this repo emits is over the threshold, so the relay must be a byte-for-byte no-op on all of it. **This is the assertion that says the feature is free**, and it reuses `guardCorpus`/`guardCorpusRust`. |
| the whole spectest matrix | 4 opt × 2 NaN × 2 GC. Measured at **1.9 s a run, ~30 s for the matrix** — much cheaper than the recorded row's tone implies. |

### Red proofs

1. **Omit the separating statement** in the emitted trampoline: the ladder is
   refused by Lua, which is the measurement above turned into a gate.
2. **Place a trampoline at a reachable point without its guard**: the
   reproduction returns the wrong answer, which is what
   `TestARelayedFunctionComputesTheSameAnswer` is for.
3. **Revert `maxJumpSpan`'s chain handling**: a bare ladder passes the check and
   Lua refuses it — the blind spot, demonstrated.

### Gates

`make test`, the full spectest matrix, `make check-lua52f`. **No golden file
moves anywhere in the repo, because there are none for emitted Lua** — which
this pass established rather than assumed. `PASSRATE` must not move.

---

## Recommendation

**NEEDS AN ORCHESTRATOR DECISION, and the decision is easy in one direction and
worth taking in the other.**

**Take now, unconditionally: correct the recorded row.** It currently sends the
next agent to build an IR outlining pass at zero-depth split points, and that
pass cannot shorten the jump it is for. That is a milestone's work aimed at
nothing, and the row is exactly where `CLAUDE.md` says it sends an agent for
open work. Whatever else happens, the row should carry the theorem and the four
measured split points.

**Take now if the round has room: the chained-goto fix to `maxJumpSpan`.** It is
~20 lines and it closes a blind spot in a shipped check. Measured, the blind
spot is **latent rather than live** — nothing this repo emits produces
`::A:: goto B` at any level — so it is not urgent on its own account. It stops
being optional the moment the relay is built, because the relay's trampolines
are that shape by construction.

**The relay itself is IMPLEMENTABLE AT BOUNDED COST — ~180 lines, no analysis
touched, no golden moved — and the question is whether it is worth building
before a real guest is refused.** The standing mitigation (`//go:noinline`) is
adequate for a hand-written guest and inadequate for a generated one, and the
generated-data-guest case is real: BetterBeltBalancer hit it, and its remedy
was `//go:noinline` marks the author had to place by hand and must not tidy
away. The design's own preference is **build it**, because it is now a small
feature rather than a milestone and because the alternative is a check that
refuses a mod with advice its author may not be able to follow.

**Coupling notes.** `internal/luagen/funclimit.go` and one loop in
`luagen.go` — nothing else, in this round or any other. It touches no
generator, no binding, no member id, no runtime Lua and no persistence mode, so
it is independent of 4a, 4b, 4d and of rounds 1-3 entirely. The one shared
surface is `agents/codegen.md`'s name-families table, which any pass that names
something has to edit.
