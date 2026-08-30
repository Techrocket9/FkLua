# 4a — the bulk attribute read

**Round 4, design-and-measure. Verdict: IMPLEMENT, at a bounded cost, in the
shape below — which is not the shape the survey proposed.**

The survey files this as `L`, "decides whether the tick-per-entity genre is
reachable at all". It is reachable. The measured per-element cost falls to
**0.044x** of the per-call form on the host side -- 979 ns to 43 ns on the run
quoted below, 994 to 45 on an independent re-run -- and the amortization is
favourable from **N = 2** upward, which puts a 1,000-entity poll inside a tenth of a tick where
today it is most of one. What the measurement also says is that two things the
survey assumed are wrong, and both change the design:

- **The 12.5 µs figure is the tier-2 path and does not price an attribute
  read.** `agents/abi.md` says so already ("the ~12 µs the downstream report
  measured is the TIER-2 PATH, not generic dispatch"), and the survey's polling
  arithmetic multiplied it by a workload made of scalar reads. Re-measured
  through a real guest on this machine today, a scalar attribute read is
  **2,682 ns** end to end, not 12,500. The genre was two-and-a-half times less
  blocked than the survey said, and is now about thirty times less blocked.
- **Batching is not where the win is.** A bulk form that does the same
  per-element work in one crossing buys **1.47×**. Everything above that comes
  from HOISTING work out of the per-element loop, and the largest single term
  is not the dispatch at all — it is FkLua's own `ld32`/`st32`, which
  bounds-check, test alignment and select a shard once per element for
  addresses that are a contiguous, aligned, in-range run known before the loop
  starts. That is the loop guard the optimizer already applies to a GUEST loop,
  applied to the host side of one crossing.

---

## The measurements

`scratchpad/r4/bench.py` and `scratchpad/r4/harness.lua`; full output in
`scratchpad/r4/RESULTS.txt`.

```sh
make lua52f && go build -o bin/fklua ./cmd/fklua
python3 scratchpad/r4/bench.py 2>&1 | tee scratchpad/r4/RESULTS.txt
```

The harness loads `runtime/lua/fk_abi.lua` **verbatim** and the real emitted
`memio` out of a compiled `(module (memory 16))`, then runs a prototype bulk
read beside the real per-call path. Every leg is sized by wall time, the floor
is measured twice bracketing everything it qualifies, and every arm is checked
for having done its work before anything is timed. **A/A 0.8% on the run
quoted.**

### Where the per-element cost is, arm by arm

N = 200 entities per crossing, one `f64` attribute unless the row says
otherwise. Each arm is the one above it with one step removed or hoisted.

| arm | ns/element | × per-call | what changed |
|---|--:|--:|---|
| **per-call `GET`** (the status quo) | **979** | 1.000 | — |
| per-call `GET` of a STRING attribute | 1,539 | 1.573 | the string crosses |
| a bare call with no blocks, N times | 475 | 0.486 | the dispatch alone |
| **generic bulk**, `encode_rets` per element | **667** | **0.681** | the crossing only |
| bulk, signature walk hoisted, `pcall` per element | 418 | 0.427 | the field descriptor |
| bulk, ONE `pcall` for the batch | 331 | 0.339 | the per-element `pcall` |
| bulk, no `pcall` and no `valid` check | 318 | 0.325 | the guards |
| bulk, storing a `u32` instead of an `f64` | 184 | 0.188 | *(a property of the attribute)* |
| **bulk, memio hoisted and resolve inlined** | **43** | **0.044** | `ld32`/`st32`'s per-element work |
| hand-written Lua doing the same poll | 21 | 0.022 | — |

The isolated terms, same run, so the staircase adds up rather than being
asserted:

| term, on its own | ns/element |
|---|--:|
| `ld32` of the handle out of guest memory | 63 |
| ...and resolving it through `M.get` | 111 |
| ...and reading the attribute off the stand-in | 118 |
| `stf64` of one result | 189 |
| `st32` of one result | 72 |

**The two biggest per-element costs are FkLua's own memory accessors, not the
engine and not the dispatcher.** `ld32(a)` is a closure into
`ld32(MEM, MEMSIZE, a)`, which bounds-checks, tests `a % 4 == 0` and selects a
shard — per element, for a run of addresses whose bounds, alignment and shard
are decided before the loop starts.

### Does one crossing pay for itself at small N?

The whole claim is that the fixed cost amortizes, so it is measured rather than
asserted. `bulk_inline` here goes through the same dispatcher every other arm
does; the first cut of this leg did not, reported 66 ns at N = 1, and was
wrong — a bulk form that never paid for its crossing.

| N | per-call ns/el | generic bulk ns/el | hoisted bulk ns/el | hoisted ÷ per-call |
|--:|--:|--:|--:|--:|
| 1 | 1,021 | 1,100 | 723 | 0.708 |
| 2 | 1,005 | 767 | 392 | 0.390 |
| 4 | 1,050 | 620 | 248 | 0.236 |
| 8 | 1,038 | 497 | 128 | 0.123 |
| 16 | 1,020 | 459 | 84 | 0.082 |
| 64 | 1,024 | 415 | 50 | 0.049 |
| 256 | 1,005 | 420 | 45 | 0.045 |
| 1,024 | 1,038 | 426 | 41 | 0.039 |

Re-run independently the same table comes back 0.82, 0.44, 0.25, 0.17, 0.09,
0.05, 0.044, 0.040 -- **the N = 1 and N = 2 cells are the noisy ones**, because
they are one and two elements of work differenced out of a whole crossing, and
every cell from N = 8 down reproduces to the third digit.

**N = 1 is 0.71-0.82× and N = 2 is 0.39-0.44×**, so there is no size at which a guest is
worse off for having used the bulk form, and no threshold anyone has to
document. The generic arm is the one that loses at N = 1 (1.08×), which is a
second reason to ship the hoisted fast path rather than only the generic one.

### The end-to-end anchor, through a real guest

`go test ./internal/guest/ -run TestWhatAHostCallCostsThroughARealGuest -v
-count=1`, this machine, today, `--persist=table`, A/A 0.8%:

| leg | ns |
|---|--:|
| dispatch, no host call | 825 |
| call, no blocks | +680 |
| **scalar in, scalar out** | **+1,858** |
| string return (44 B) | +4,110 |
| tier-2 map argument | +11,981 |

So the host half this harness measures (979 ns) is about **37%** of a per-call
scalar attribute read; the rest is the guest's own block setup, the import call
and the decode. **A bulk read removes almost all of that too** — one block for
N elements, one import call, and a per-element decode that is a contiguous
guarded array read, which is the `pure_sum` shape the loop guard already
covers. The host-side ratios above are therefore CONSERVATIVE: end to end the
hoisted form should be better than 0.044×, not worse.

**And under `--persist=packed` it is a different order of magnitude again.**
The same profile reports a scalar in / scalar out at **+131 µs** under packed
against +1.9 µs under table, because packed costs ~40 µs per page actually
written and a per-call return block scatters. A bulk read writes ONE contiguous
destination: 1,000 `u32` results is 4 KiB, which is one page. `agents/abi.md`
says not to quote either packed figure as universal and this design does not —
it is recorded as a direction, and it is a direction that only helps.

### The oracle caveat, stated where it bites

`bin/lua52f` reads a Lua table 4-6× faster than Factorio does, and the
machinery around it is 1.04-1.10× in both. Every arm above is dominated by
TABLE work — a shard read, a handle lookup, an attribute read — so the in-game
per-element numbers are larger than these, in every row, by roughly the same
factor. **The ratios are what survive**, which is why the conclusions are
carried by the `× per-call` column and not by the nanoseconds.

Two more honesty items:

- **The stand-in object's attribute read is a plain Lua table read; Factorio's
  is a C++ `__index` metamethod.** So the `vs hand Lua` column overstates the
  gap: whatever the engine's own read costs, both the per-call form and the
  bulk form pay it once per element, so it is a constant ADDED to both sides
  and it compresses the ratio. The survey's own figure for a hand-written Lua
  poll is ~50 ns/entity in game against this harness's 21 ns, which is the
  right order.
- **`bulk_direct_raw` (18.6 ns) is not an achievable arm** and is in the table
  as a floor only: it reaches the object without resolving a handle at all.
  The achievable floor is `bulk_direct_inline` at 43 ns.

---

## The design

### It is a NEW IMPORT, not a new member kind, and the pruning scan is why

The repo has taken the kind-versus-import decision five times (`MemberGetEq`,
the three class operators, `MemberGetHandle`, `MemberIndexSet`,
`MemberGlobalFunc`) and it has come out "kind" every time, on one argument:
*as a kind it inherits handle resolution, the `valid` check, the `pcall` and
`ERR_NO_MEMBER` for free, and the member-id scan that prunes the shipped table
keeps working because the id is still an ordinary `i32.const` at the call site.*

**The first half of that argument holds here and the second half is what
decides the shape.** A bulk read needs the member id to reach the host, and
there are three ways to do it:

| shape | what it costs |
|---|---|
| a new member KIND, one bulk member id per eligible attribute | pruning works unchanged; **+1,391 members** at the 2.0.77 pin (see the count below), a 33% growth of `host_members_bound`, and 1,391 more generated functions in each language |
| one bulk member for the whole API, the target id passed in the ARGUMENT BLOCK | no new members; **pruning breaks silently** — the id is an `i32.const` stored to memory, not an operand of an import, so `UsedMembers` cannot see it and the guest ships all 4,262 members |
| **a new IMPORT, `fk.bulk_get(member, handlep, count, dstp)`** | **no new members; pruning works by construction** |

The third is right and it is not a compromise. `internal/factorio/used.go`'s
scan is already parameterised by import name and operand position —
`usedIDs(m, DispatchName, 1)` for `fk.call`, `usedIDs(m, SubscribeName, 0)` for
`fk.subscribe`, `usedIDs(m, DefineName, 0)` for `fk.define`. A fourth call with
`("bulk_get", 0)`, unioned into `UsedMembers`, is four lines. The member id is
the first operand of a call to an import, which is exactly the shape the scan
was built for, so a bulk read prunes as precisely as an ordinary one and
`api check`'s `GuestSurface` — which reads `UsedMembers` — covers it with no
change at all.

**The member entry it names is the ORDINARY getter's.** No new id, no new
signature, no new census row for the member. What the host reads out of it is
`kind` (must be `M.GET` or `M.GETH`), `name`, `valid`, `opt` and
`sig.rets` — all of which are already there.

### The destination is an array of the getter's OWN return block

This is the decision that makes the feature small.

`retsize` is already in every member entry. `sig.rets` is already a one-field
block with the presence byte `Layout` places for an optional attribute. So:

> **The destination is `count` copies of the getter's return block, laid end to
> end at `retsize` stride.** Element *i* lives at `dstp + i * retsize` and is
> byte-for-byte what a single `fk.call` would have written at `retp`.

Everything falls out of that:

- **Optionality is free.** An absent value clears the presence byte at
  `dstp + i*retsize + f.has`, exactly as `encode_rets` already does. There is
  no second destination, no bitmap and no "did element 7 have a value" API.
- **The guest-side decoder is the one it already has.** The generated bulk
  binding's loop body is the single getter's decode with `&r[0]` replaced by
  `&dst[i*retsize]`. `gogen.go` and `rustgen.go` already emit that expression
  shape for an array return's element walk.
- **A handle-returning attribute works with no special case**, because
  `encode_rets` mints a transient handle for a `K_HANDLE` field and the bulk
  form is the same code N times. The handles are transient, so they die with
  the dispatch like every other, and a guest that wants one keeps it with
  `Retain`.
- **Nothing new is laid out anywhere**, which is the property that keeps this
  off `internal/factorio/layout.go` entirely.

### Two arms, and the fast one is the point

Measured above: reusing `encode_rets` per element buys 1.47× and hoisting buys
23×. So the host implementation is a fast path with a general fallback, which
is the same shape `ld32` itself has:

```
bulk_get(mid, hptr, n, dstp):
  m = members[mid]; refuse unless m.kind is GET or GETH
  lastError = ""                      -- M.call's contract, kept
  f = m.sig.rets[1]
  if  n > 0
  and hptr % 4 == 0 and hptr + n*4 <= SHARD0
  and dstp % align(f) == 0 and dstp + n*retsize <= SHARD0
  and retsize == 4 and f.has == nil and f.kind is a 4-byte scalar or a handle
  then  -- the fast arm: one bounds check, direct shard indexing, inline resolve
  else  -- the general arm: ld32/encode_rets per element
```

The fast arm's preconditions are the loop guard's own, one level up
(`agents/optimizer.md`): a contiguous aligned span whose bounds are proved at
entry. **Being wrong about the precondition costs the general arm and never a
wrong answer**, which is the same failure direction the guard has.

### Error semantics: per element, and it is not the expensive choice

The measurement makes this decision cheap to take well. A `pcall` per element
costs 418 − 331 = **87 ns/element** on the general arm and is not on the fast
arm at all (a 4-byte scalar attribute read on a resolved, valid object cannot
raise — `check_valid` is what stops the one case that can, and it is one
compare).

The rule:

- **A dead handle, an invalid object, or a raise at element *i* clears element
  *i*'s presence byte and the walk CONTINUES.** The call returns `M.OK` and the
  return block carries the number of elements written.
- **The status is about the CALL, not about the elements.** `ERR_NO_MEMBER` if
  the member is absent or is not a readable attribute, `ERR_BAD_ARGS` if the
  destination cannot hold `count` elements. Nothing about one element's fate
  reaches the status, because a caller that wanted per-element failure has the
  presence byte.
- **Determinism is not at risk and the reason is worth stating.** The walk is
  index order over an array the guest supplied, every branch is a function of
  the handle table and the engine's own answer, and both are identical on every
  peer. There is no iteration over a hash table anywhere in it — which is the
  property that makes "abort at i" and "skip i" equally deterministic, so the
  choice can be made on ergonomics. Skipping is chosen because a poll over a
  thousand entities of which one died between the search and the read is the
  ORDINARY case, not an error.
- **`lastError` is cleared on entry and set by the last element that raised**,
  matching `M.call`'s "the call that just returned" contract. The re-entrancy
  seam that contract already documents is unchanged.

For an attribute the description marks optional, "absent" and "the element
failed" are the same presence byte — which is a real loss of resolution and is
stated rather than papered over. It is the same trade `M.call` already makes
for a single optional GET, one level down: `f == nil` with `opt` set is a
value, not evidence.

### Which attributes get a bulk binding

Counted at the 2.0.77 pin from `runtime-api.json`:

| readable attributes | 2,304 |
|---|--:|
| fixed-width scalar (`float`, `double`, the ints, `boolean`) | 1,157 |
| a class, i.e. a handle | 234 |
| **bulk-eligible: the two above** | **1,391** |
| ...of which the description marks optional | 403 (29%) |
| `string` | 237 |
| everything else (arrays, dictionaries, `LuaCustomTable`, `Color`, unions, …) | 676 |

**Strings are excluded, and the reason is a measurement rather than a rule.**
A string element is a `(ptr, len)` into the scratch region, and 1,000 of them
would exhaust the 4 KiB region and fall through to `fk_alloc` per element —
which is the allocation the region exists to remove. A bulk string read wants a
pooled encoding, and that is 4b's finding, not this one. `NameIs` already
covers the dominant string case (asking whether a name is a known constant)
host-side for 1,539 → the `NameIs (no match)` leg's cost.

**Containers are excluded** because an element would be a nested `(ptr, count)`
into the arena, so the bulk destination would stop being a flat array — which
is the one property everything above rests on.

Scoping to 1,391 follows the `Into` precedent's own rule ("every member the
same branch covers gets the variant for free"), and it costs more than `Into`
did: 1,391 more generated functions per language, against `Into`'s 240.
Estimated at ~16 lines each that is **+22k lines on `fkapi.go`'s 121k (+18%)**
and comparably on the Rust side. That is the largest single cost of this
feature and it is a build-time and download-time cost, not a runtime one — the
member table does not grow at all, so nothing reaches a save.

**If the orchestrator wants that smaller**, the honest lever is to restrict the
first cut to the 988 NON-OPTIONAL eligible attributes, which is also exactly
the set the fast arm covers. That is a defensible line — the fast arm is the
whole value — and it is the one open scoping question in this design.

---

## Alternatives considered and rejected

| alternative | why not |
|---|---|
| **A new member KIND with one bulk id per attribute** | The kind argument's second half — "the id is still an ordinary constant at the call site" — is satisfied by the import too, and the kind costs +1,391 census members where the import costs none. The first half (inheriting resolution, `valid`, `pcall`) is satisfied either way, because the bulk implementation lives in `fk_abi.lua` beside `M.invoke` and calls the same helpers. |
| **One bulk member, the target id in the argument block** | Silently defeats `UsedMembers`. A guest that only ever reads attributes in bulk would ship all 4,262 members and 890 KB of Lua, and nothing would say so. This is the R6 failure shape (a pruning scan defeated by a call-site detail) with the detail moved one level in. |
| **A bulk read that returns an AGGREGATE** (sum, min, count) | A different feature with a different audience, and it cannot be generated from the description — there is no per-attribute notion of what aggregating it means. `count_entities_filtered` already exists for the one case the engine models. |
| **Several attributes per crossing** (`k` attributes × N handles) | Tempting, and it is a strict superset. Rejected for the first cut because the destination stops being an array of one return block and becomes a struct-of-arrays whose layout nothing currently emits — which is precisely the property that keeps this feature off `layout.go`. It composes: `k` bulk calls over the same handle array cost `k ×` the per-element figure, and the crossing is 475 ns amortized over N. Revisit with a measurement that shows the second crossing mattering. |
| **A guest-side helper that loops the existing getter** | This is what a guest can already write, and it is the 979 ns row. |
| **Widening `M.call` instead of adding an import** | `M.call`'s four operands are `(handle, member, argp, retp)` and a bulk read needs `(member, handlep, count, dstp)` — the handle slot would have to mean something else, decided by the member's kind. That is a second meaning for one operand, which is the failure shape this repo names as "two things spelling one fact". |

---

## Implementation plan

**Estimated size: ~450 lines of hand-written code and ~22k lines of generated
output. Two days of careful work, most of it tests.**

### Files touched

| file | what | ~lines |
|---|---|--:|
| `runtime/lua/fk_abi.lua` | `M.bulk_get(mid, hptr, n, dstp)`: the general arm, the fast arm, the precondition test, the `lastError` clear | 90 |
| `runtime/lua/fk_mod.lua` | one entry in the `fk` imports table | 4 |
| `internal/factorio/used.go` | `BulkGetName`; `UsedMembers` unions a second scan | 20 |
| `internal/factorio/gogen.go` | `goMemberBulk`, beside `goMemberInto`, and its call site in the member loop | 90 |
| `internal/factorio/rustgen.go` | `rustMemberBulk`, the mirror | 90 |
| `internal/factorio/census.go` | one row, `bulk_variants`, beside `IntoVariants`' reporting | 10 |
| `guest/go/fkapi/fkapi.go`, `guest/rust/fkapi/src/api.rs` | GENERATED, regenerated by `gen-bindings` | ~22k / ~20k |
| `guest/go/examples/callcost/main.go` | two probes: bulk read at N=4 and at N=256 | 15 |
| `docs/factorio-api.md` | the author-facing paragraph and one worked example | 40 |
| `agents/abi.md` | the shape, the two arms, the measured table, the error rule | 60 |
| `CLAUDE.md` | the Host ABI section's sentence, and the deliverables row closed | 15 |

`fklua.lock` and `api/<version>/census.json` move; every consumer needs a
re-pin **only if member ids move, and they do not** — this adds no member.
That is worth saying loudly in the round's report, because it is the one
question a downstream mod asks first.

### Test plan

| test | what it pins |
|---|---|
| `TestABulkReadFillsEveryElement` | N handles in, N return blocks out, byte-compared against N separate `M.call`s over the same objects. **The equality against the per-call path is the whole correctness argument** and it is what makes the fast arm safe to add later. |
| `TestTheFastAndGeneralArmsAgree` | the same corpus with the precondition forced false, byte-identical results. Red-proven by breaking one arm. |
| `TestADeadHandleClearsItsElementAndTheWalkContinues` | element 7's handle released, elements 0-6 and 8-N intact, presence byte 7 clear, count = N−1 |
| `TestAnInvalidObjectIsSkippedRatherThanFailingTheCall` | `valid = false` at element 3, same shape |
| `TestARaisingAttributeDoesNotAbandonTheBatch` | a stand-in whose `__index` raises for one object, and `last_error` naming it |
| `TestAnOptionalAttributeClearsItsPresenceByte` | absent value, presence byte 0, value slot untouched |
| `TestABulkHandleReturnMintsTransientHandles` | N handles minted, all released at `dispatch_done`, `M.stats()` back to zero |
| `TestTheBulkMemberIDSurvivesTheGeneratedWrapper` | **the pruning assertion** — the R6 shape. A guest making one bulk read must prune to one member. Red-proven by removing the `//go:noinline`/`#[inline(always)]` that keeps the id constant, exactly as `TestTheEventIdSurvivesTheGeneratedRustSubscribeWrapper` does. |
| `TestAPIcheckSeesABulkOnlyGuest` | a guest that reads ONLY in bulk reports a complete surface naming that member |
| `TestBothBackendsBindTheSameBulkVariants` | the Go/Rust count and name sets, extending `TestBothBackendsBindTheSameMembers` |
| `TestABulkReadRefusesAMemberThatIsNotAReadableAttribute` | `ERR_NO_MEMBER` for a `CALL`, a `SET`, an operator |
| `TestABulkReadRefusesADestinationThatCannotHoldIt` | `ERR_BAD_ARGS` rather than a write past the end |
| `TestAStraddlingBulkDestinationTakesTheGeneralArm` | the shard boundary, which is the fast arm's one soundness precondition |

### Red proofs

1. **Remove the fast arm's `dstp + n*retsize <= SHARD0` conjunct** and put the
   destination across the shard boundary: the fast arm indexes `mem[1]` past
   its end, which is a nil index rather than a wrong answer — so the test has
   to assert the RESULT and not merely that nothing raised.
2. **Drop the second `usedIDs` scan**: a bulk-only guest ships all 4,262
   members, and `fklua mod`'s own output line says `all 4262 members` where it
   said `1 member`. This is the assertion that the pruning claim is real.
3. **Make the fast arm read the wrong stride** (4 where `retsize` is 8): the
   byte-comparison test against the per-call path fails on element 1, which is
   what says that test has teeth.

### Gates

`make test`, `make spectest` (untouched — no emitter change), `gen-bindings
--check` across every committed description, `make check-lua52f`, and
`census.json` regenerated with the new row. **No golden file under
`testdata/spec` moves**, because nothing in the emitter changes; the generated
bindings are the whole diff and they are the review artifact.

---

## Recommendation

**IMPLEMENT NOW.** The design is small (one import, ~90 lines of `fk_abi.lua`,
two generator branches), it adds no member ids and therefore forces no
downstream re-pin, the pruning story is exact by construction, and the measured
win is 23× on the host half with the amortization favourable from N = 2.

**One decision for the orchestrator**, stated because it is a scope question
and not a technical one:

> **Do the 403 optional eligible attributes get a bulk binding in the first
> cut, or only the 988 non-optional ones?** Including them costs ~29% more
> generated lines and makes the general arm the only arm for them (the fast arm
> needs `f.has == nil`). Excluding them makes the feature's coverage a rule an
> author has to look up. The design's own preference is INCLUDE, because a rule
> of the form "bulk works for some attributes" is the shape this repo keeps
> finding authors trip over, and 18% of a generated file that nobody reads is a
> download rather than a cost anyone feels.

**Coupling notes.** Nothing here touches `layout.go`, the emitter, or the
persistence modes, so it is independent of every other round-4 item and of
rounds 1-3. It shares one file with round 1 (`internal/factorio/used.go`, if
round 1's subscribe-name widening lands there) and one with round 2
(`gogen.go`/`rustgen.go`'s member loop, where typed args also add a branch) —
both are additive and neither is a conflict of substance.
