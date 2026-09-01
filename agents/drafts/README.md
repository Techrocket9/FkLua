# Design drafts

Working notes, exempt from `agents/docs-style.md`. Most of this directory is
the round 4 set below; the standalone drafts at the end are the exceptions,
indexed by their own section rather than by round 4's table.

## Round 4 — the long levers

One document per item of
[`agents/lua-temptations.md`](../lua-temptations.md)'s round 4, each carrying
its design, its measurements with the exact commands, the alternatives it
rejected and why, an implementation plan with a file surface and a test plan,
and a recommendation.

These are the design-and-measure half; the charter's rule is that an honest
"designed, not shipped, here is why" is an acceptable terminal outcome. The
verdict column below says where each item's design landed; the SHIPPED markers
are appended as the implementation rounds close them and the drafts themselves
stay as written, because they are the record of what was measured and decided,
not of what the tree now holds.

| | item | verdict |
|---|---|---|
| [`r4a`](r4a-bulk-attribute-read.md) | the bulk attribute read | **IMPLEMENT.** 979 → 43 ns per element on the host side, favourable from N = 2, no new member ids and therefore no downstream re-pin. The shape is a new IMPORT rather than a new member kind, because that is what keeps the pruning scan exact. One scoping question for the orchestrator. **SHIPPED**: `fk.bulk_get`, both languages, optionals INCLUDED (1,533 bindings at the GA pin, one per eligible member). Three deviations, each measured: the fast arm hoists the READ and not the WRITE, because a direct shard store bypasses the dirty-page set that is packed's save set and the collector's write barrier — so the shipped fast arm is **0.220× at N = 256 and 0.598× at N = 1** rather than 0.044×, and the ~87 ns pcall plus the ~64 ns store funnel is the whole of the gap; a skipped element is written as ZERO rather than left alone, because a reused destination would otherwise hand back the previous crossing's value; and the object list is a `[]Object` rather than a `[]<Class>`, which is what every array-of-handles return in the API actually is and which removed the inherited re-rendering the first cut needed. End to end through a real guest: **0.239× under `--persist=table` and 0.0083× under `packed`** at N = 256. |
| [`r4b`](r4b-batched-gui-add.md) | the batched GUI add | **THE PROPOSED FEATURE DOES NOT WORK.** Batching an array of tier-2 specs buys 13%; 86% of the cost is `read_dyn`, which a batch decodes just as often. Not re-marshalling strings buys 12×. Sequencing decision needed, because the artifact it wants is round 2's typed `add`. **CLOSED BY MEASUREMENT**: round 2 landed the artifact and took the encoding half (0.735× on this corpus, not the predicted 3.3×, because two of `add`'s five audited fields are `LocalisedString` unions and a union stays a tier-2 slot inside the block). The RESIDUAL — a batch of typed spec blocks plus a per-batch string pool — was then prototyped against the shipped typed call over a whole 50-element window and measured at **0.655× (six runs, 0.639-0.680)** against a pre-registered rule of ≤ 0.6×. Not one run reached it, the figure is an UPPER bound (the prototype carries no parent column, writes no handles back and skips everything `M.call_typed` pays), and both corrections that can be applied move it toward 1.0: crediting the batch with the whole guest-side cost of a blockless host call gives 0.604, and adding the engine's own ~2 µs per element to both sides gives 0.668. The pool itself is worth 1.335× rather than 12×, because only `type` and `style` repeat — `name` is distinct per element by construction. Recorded, not built; `scratchpad/r2/RESULTS.txt` is the measurement. |
| [`r4c`](r4c-data-stage-function-splitter.md) | the data-stage function splitter | **THE RECORDED DESIGN CANNOT WORK**, measured: a function that reaches the jump limit has four zero-depth split points and all four are outside the jump's span, for a structural reason. A trampoline relay does work, needs no IR pass and no size estimate, and is ~180 lines. Correct the recorded row either way. **SHIPPED**: the relay, the chained-goto fix and the corrected row are on master; the guarded placement is what ships and the unreachable-placement refinement is measured-working and deliberately not built. |
| [`r4d`](r4d-q5-build-time-configuration.md) | Q5, the build-time configuration channel | **LARGELY CLOSED ALREADY.** Two of its three forms are fixed — one by the data stage becoming a guest, demonstrated end to end here, one by an engine prototype both halves of which bind. What is left is doctrine at S. The queue amendment's phrasing does not match the finding. **SHIPPED**: the doctrine is `docs/data-stage.md`'s "Sharing one config between the two stages", the queue amendment is withdrawn, and the ONE open question — the `--dump-data` probe of the `mod-data` half — was run and answers MORE than it was asked: the prototype is accepted and lands in the dump, AND a control guest reads the blob back at runtime through `ModDataRaw` in the same run, so that half is proven end to end rather than inferred. The Rust shared-crate residual stands as written. |

### The measurements

Harnesses and their recorded output live in [`scratchpad/r4/`](../../scratchpad/r4/):

| file | what it measures |
|---|---|
| `bench.py` + `harness.lua` | 4a and 4b: the real `fk_abi.lua` and the real emitted `memio`, with a prototype of each shape beside the per-call path. `RESULTS.txt` is its output. Since 4a shipped it also drives the REAL `M.bulk_get`, both arms and three attribute shapes, and `RESULTS-shipped.txt` is that output. |
| [`scratchpad/r2/`](../../scratchpad/r2/)'s `bench.py` + `harness.lua` | round 2's typed block on the SHIPPED path, and since the re-judgment a whole 50-element window per call against a batch-and-pool prototype over the same block. `RESULTS.txt` there is its output and carries the decision arithmetic. |
| `jumpladder.py` | 4c: what Lua 5.2 actually refuses, and whether a trampoline can relay a long jump. `RESULTS-jumplimit.txt` is its output. |
| `livesets.go.txt`, `livesets-repro.go.txt` | 4c: where a split point can be and how wide the live set is there, over the repo's own guests and over the funclimit reproduction. A throwaway `main` over `internal/ir`, kept as text rather than as a package so it does not join the build. |

Every one of them follows `agents/benchmarks.md`: legs sized by wall time, a
floor measured twice bracketing what it qualifies, conclusions carried by
ratios, and the A/A spread printed whether or not it is comfortable. The oracle
caveat is restated in each: `bin/lua52f` reads a Lua table 4-6× faster than
Factorio, so a host-side ratio between forms differing in table work understates
the in-game difference — and every "vs hand-written Lua" column here is an
upper bound, because the stand-in objects are cheaper than the engine's.

## Standalone drafts

Not round-4 items; nothing above indexes them and they answer to no survey. Same
voice, same rules, same acceptable terminal outcome — a design recorded honestly
is a finished draft whether or not anything is built from it.

| | draft | verdict |
|---|---|---|
| [`recipes`](recipes-library-design.md) | the recipes-and-research guest library — `fkrecipes`, data-stage-only and pin-transparent, answering the 2026-08-30 library research round's open questions | **DECISIONS RESOLVED 2026-08-31**, implementation assigned to a standalone repo. |
| [`record-replay`](record-replay-debugging.md) | Tier D of the toolkit's debugging tiers: record every host→guest byte, replay the guest against the recording under a real debugger, with tick-boundary memory checksums proving the capture surface was complete | **DESIGN RECORDED, NOT SCHEDULED.** Nothing implemented, no cost measured, nothing queued. Follows from the determinism invariant rather than from a measurement, which is why it could be written at all and why every number in it is an open question. Carries the Tier B "maybe someday" note, whose hook is the Tier A map's `fklua_map` version field. |
