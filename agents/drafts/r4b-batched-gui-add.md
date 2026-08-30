# 4b — the batched GUI add

**Round 4, design-and-measure. Verdict: THE PROPOSED FEATURE DOES NOT WORK AND
A DIFFERENT ONE DOES. Batching an array of element specs into one crossing buys
13%. Not re-marshalling strings buys 12×. Implement the second; the first is
worth taking only because it is where the second has to live.**

The survey's shape is *"a host-side batched add — an array of element specs
applied in one crossing, returning the handles"*, cost `M-L`, and it names it
as *"the only lever against the refresh cost"*. Measured, it is not a lever at
all. **86% of what a GUI `add` costs is `read_dyn` decoding the spec**, and a
batch decodes the same specs the same number of times.

That is not a reason to drop the item. It is a reason to point it at the
encoding, where the same measurement says there is a 12× win — and the encoding
question is the one round 2 is already opening for `add` under a different
name, which is the coupling this document exists to flag.

---

## The measurements

`scratchpad/r4/bench.py`, `scratchpad/r4/harness.lua`, output in
`scratchpad/r4/RESULTS.txt`.

```sh
make lua52f && go build -o bin/fklua ./cmd/fklua
python3 scratchpad/r4/bench.py 2>&1 | tee scratchpad/r4/RESULTS.txt
```

The corpus is 50 elements shaped like one row of a table — a flow, a label and
a button, each carrying `type`, `name`, `caption`, `style` and `tooltip`, with
`name` and `caption` distinct per row and the rest repeated. That is the
audited GUI applications' dominant shape (`agents/lua-temptations.md`: whole
window rebuild, 500 to 1,200 element-creation sites). Both encodings are
checked to decode to the SAME spec before anything is timed, and the stand-in
`add` reads `spec.type` and raises if it is missing, so no decode can be
optimised away.

### Where a GUI element's cost is

50 elements per crossing. **A/A 7.6% on the run quoted.**

| leg | ns/element | × per-call |
|---|--:|--:|
| **`add` per call, hoisted tier-2 map** (the status quo) | **20,359** | 1.000 |
| **batched: an array of tier-2 maps** (the survey's shape) | **17,673** | **0.868** |
| batched: an array of FLAT specs plus a string pool | 6,175 | 0.303 |
| `read_dyn` of one spec, no engine call under it | 17,699 | 0.869 |
| the flat decode of one spec, same | 6,013 | 0.295 |
| ...its five `read_string` calls alone | 5,768 | 0.283 |
| ...its table construction alone, strings already in hand | 169 | 0.008 |
| flat + a PER-BATCH string pool (each distinct string decoded once) | 2,964 | 0.146 |
| flat + strings ALREADY INTERNED, none decoded (the ceiling) | 465 | 0.023 |
| flat + interned, with the CAPTION re-decoded per element (realistic) | 1,671 | 0.082 |
| hand-written Lua building the same 50 elements | 201 | 1.000 (its own column) |

Read down that table and the whole design is in it:

- **`read_dyn` is 17,699 of the 20,359.** The dispatch a batch removes is
  ~475 ns/element (measured in 4a's table, same harness, same run), which is
  2.3% of the total. That is the 13%-with-noise the batched-dyn row shows.
- **The flat encoding is 3.3×** — and it is 3.3× because `read_dyn` walks a
  tagged key/value pair list with a string key per entry, where a flat block
  reads a `(ptr, len)` at a known offset. Ten strings become five.
- **Of the flat decode, 97% is still the five `read_string` calls.** Building
  the Lua table from strings already in hand is 169 ns — the same order as
  hand-written Lua's whole 201 ns.
- **So the lever is the strings, and the lever on the strings is not decoding
  the ones that did not change.** A per-batch pool (decode each DISTINCT string
  once) is 2.1× on top of flat. Strings interned across refreshes is 12.9× on
  top of flat, and the realistic mixed case — four fields stable, the caption
  fresh every refresh — is 3.6× on top of flat and **12.2× on the status quo**.

### What the ratios mean in a real game, honestly

Three separate cautions, all pointing the same way, so the "× hand Lua" column
in `RESULTS.txt` must not be quoted as a game figure:

1. **The stand-in `add` is a Lua function; the engine's is a C++ GUI-element
   construction.** The survey's own numbers price it: a 50-row table is 1-2 ms
   in Lua for ~750 elements, i.e. **~1.3-2.7 µs per element of engine work**
   that both sides pay. Adding a constant to both sides compresses every ratio.
   With 2 µs of engine added to each row: the status quo is ~22 µs/element, the
   realistic interned form is ~3.7 µs/element, and hand-written Lua is ~2.2 —
   so the feature takes the genre from **10× Lua to 1.7× Lua**.
2. **`bin/lua52f` reads a Lua table 4-6× faster than Factorio.** Every row here
   is table-dominated, so all of them scale up together and the ratios hold.
3. **`read_string`'s cost is length-dependent** and this corpus's strings are
   4-24 bytes. `agents/abi.md` measures the fixed reader at 14 ns/byte at 148
   bytes with an explicit `n < 8` fast path, and notes that short strings gain
   nothing — which is exactly this corpus. A GUI carrying localised strings
   (arrays, not scalars) is worse than this, not better.

**And the 4b-relevant half of the real-guest anchor** (`go test
./internal/guest/ -run TestWhatAHostCallCostsThroughARealGuest`, this machine,
today, `--persist=table`): a tier-2 map argument is **+11,981 ns over the
dispatch**, against +680 for a call with no blocks. The guest-side `writeDyn`
is in that number and it is not in this harness's — so the true per-call `add`
is larger than 20 µs and the batched form removes the guest-side encode too.
The host-side ratios are conservative in the same direction 4a's are.

---

## The design

### It is one import and one ENCODING, not a batch of tier-2 values

```
fk.batch_add(parent, specp, count, handlep) -> status
```

`specp` points at `count` copies of a **flat element-spec block**; `handlep` at
`count` `u32` slots the host writes transient handles into. The parent is an
ordinary handle in the first operand.

The block's layout is the one `internal/factorio/layout.go` already emits for a
struct with optional fields — presence bytes where `Placed.HasOffset` puts
them, `(ptr, len)` for a string, natural alignment — with two columns of its
own:

| column | why |
|---|---|
| `parent` — a `u32` index into this batch, or `0xFFFFFFFF` for the receiver | **one crossing builds a TREE.** `add` takes no children, which is the survey's own "element count equals call count"; a parent column is what turns a batch from a list into the window. Element *i* may name only *j < i*, which makes the order a topological one by construction and needs no sort. |
| every string field is a `u32` INDEX into a per-call string table | the measured lever. |

The string table crosses beside the specs: a count, then `(ptr, len)` pairs,
then the bytes. The host decodes it once, into `count` Lua strings, and every
spec's string field is an index into that array. **Fifty rows carrying fifty
distinct names, three types, one style and one tooltip is 55 decodes instead of
250.**

### The interning question, which is where the 12× is and where the risk is

The per-batch pool is 2.1× and needs nothing but the encoding. The remaining
5.9× needs the strings to survive BETWEEN crossings, and that is a second
mechanism with its own lifetime rule. Three shapes, and the design takes the
first:

| shape | verdict |
|---|---|
| **per-batch pool only** | **the first cut.** No new state, no lifetime question, 6.9× on the status quo (0.146). It is the whole of what the encoding buys and it is entirely inside one dispatch. |
| a per-SESSION intern table the guest fills with an explicit `fk.intern(ptr, len) -> id` | 12.2× realistic. It is guest-visible state that must live where the handle table lives, it must be rebuilt or invalidated across a save, and an id the guest kept over a reload would be the F1 defect (a retained thing that did not survive) one table over. **Designed, not proposed** — see below. |
| the host caching by `(ptr, len)` | REFUSED. The guest reuses its buffers, so the same address means a different string on the next dispatch. This is the buffered-`(ptr, len)` use-after-free that `CLAUDE.md` already names as an invariant, met from the caching side. |

**Why the per-batch pool is the honest first cut**: it is 6.9× of a possible
12.2×, it is the part with no new lifetime, and the measurement says where the
rest is if anyone wants it. An intern table is a feature of its own — it would
serve `create_entity`'s prototype names and every other repeated string in the
API, not only GUI — and it should be scoped on that basis rather than smuggled
in under a GUI item.

### What the batch does NOT do

- **It does not become a general batched method call.** `add`'s value is that
  it is called 500-1,200 times with a spec whose shape is describable. A
  general "call member m with these N argument blocks" is a bigger surface with
  no measured consumer, and `agents/lua-temptations.md` names exactly four
  members whose option table defeats the typed generator. Two of those four
  (`set_gui_arrow`, `create_segmented_unit`) have 8 keys and nobody calls them
  in a loop.
- **It does not cover `create_entity`.** That is the other mass-construction
  member and it has 84 variant groups and 126 distinct keys at the 2.1.17 pin
  (against `add`'s 22 and 87). The same machinery would serve it and the same
  measurement would apply — but a mass-builder's cost is already recorded in
  game (`agents/abi.md`: a 4×4 network recompile at ~21 µs/call) and nobody has
  measured what a batched form does to it. **Named as a follow-on, deliberately
  not designed here**, because a design that claimed two consumers and measured
  one would be exactly the over-generalisation this file's 4a sibling refuses
  for the same reason.

### Partial failure, and what has already been built when element *i* is refused

The engine raises for a bad spec (a `type` that is not a `GuiElementType`, a
`name` that collides with a sibling). By the time element *i* raises, elements
`0..i-1` are real GUI elements in the player's window.

The rule, and it is the opposite of 4a's:

> **The batch STOPS at the first refusal, reports how many were built, and does
> not unwind.**

Three reasons, in the order they decide it:

1. **Unwinding is not free of side effects.** Destroying elements `0..i-1`
   raises `on_gui_*` events at other mods, so a failed batch would be
   observable by a third party as a window that appeared and vanished. A
   partial window is a visible defect the author will fix; a phantom event
   storm is a bug in somebody else's mod.
2. **The parent column makes continuing meaningless.** Element `i+1` may name
   `i` as its parent. Skipping `i` and continuing would attach `i+1` to
   nothing, which is a second failure the caller cannot diagnose. 4a can skip
   because its elements are independent; this one's are not.
3. **The count is enough to diagnose.** `handlep[0..k-1]` are valid handles,
   `k` comes back in the return block, and `fk.last_error` carries the engine's
   own sentence about element `k`. A guest that wants to clean up has the
   handles; a guest that does not gets a visibly wrong window and a log line
   naming the element.

`lastError` is cleared on entry, per `M.call`'s contract, and set from the
raise.

### Determinism

The walk is index order over an array the guest supplied, the string table is
index-addressed, and nothing iterates a hash table — so the sequence of engine
calls is a function of guest state alone and is identical on every peer. The
handles minted are transient and come out in index order. **This is the same
argument 4a makes and it is stronger here**, because the parent column forbids
forward references and therefore forbids any reordering at all.

---

## Alternatives considered and rejected

| alternative | why not |
|---|---|
| **The survey's shape: an array of tier-2 maps** | **Measured at 0.868×.** It removes the dispatch, which is 2.3% of the cost, and keeps `read_dyn`, which is 86%. Shipping it would be shipping the 13% and closing the item. |
| **Speeding up `read_dyn` instead** | Already done once — `agents/abi.md`'s `fk_str` pass took a 148-byte string from 94 to 14 ns/byte and explicitly records that "the create_entity-shaped map shows no reliable change, because all ten of its strings are 1-14 bytes". The GUI corpus is the same shape. The remaining cost is the tag walk and the per-string call overhead, not the byte loop. |
| **A `LocalisedString`-aware fast path** | A caption is a `LocalisedString`, which is a string OR an array. The flat block carries it as a string index and falls back to a `K_DYN` slot when it is an array — one presence bit. Not rejected; folded in as a field-level detail rather than a design of its own. |
| **Building the whole window guest-side and handing over one blob** | This IS the design; "one blob" is the flat spec array plus the string table. |
| **Interning by `(ptr, len)` host-side** | Unsound: the guest reuses its buffers. Named above. |
| **Letting the batch continue past a refusal** | The parent column forbids it. Named above. |

---

## Implementation plan

**Estimated size: ~600 lines of hand-written code. The spec-block generator is
the bulk of it and it is shared with round 2 (see the coupling note).**

### Files touched

| file | what | ~lines |
|---|---|--:|
| `runtime/lua/fk_abi.lua` | `M.batch_add`: the string-table decode, the spec walk, the parent resolution, the handle writes, the stop-at-first-refusal rule | 130 |
| `runtime/lua/fk_mod.lua` | one entry in the `fk` imports table | 4 |
| `internal/factorio/used.go` | `BatchAddName`; the member id unions into `UsedMembers` exactly as 4a's does | 15 |
| `internal/factorio/layout.go` | the spec block's `parent` and string-index columns — **or nothing at all**, if round 2's typed-args work has already produced a spec block for `add` (see the coupling) | 0-90 |
| `internal/factorio/gogen.go` | `AddBatch(specs []GuiSpec) ([]Object, error)`, the spec struct, its `encodeAt`, and the string-table builder | 140 |
| `internal/factorio/rustgen.go` | the mirror | 140 |
| `guest/go/fkapi/fkapi.go`, `guest/rust/fkapi/src/api.rs` | GENERATED: one struct and one method per language | ~250 |
| `docs/factorio-api.md` | the author-facing paragraph and a worked 50-row table | 60 |
| `agents/abi.md` | the shape, the measured table, the failure rule | 50 |
| `CLAUDE.md` | the deliverables row | 10 |

### Test plan

| test | what it pins |
|---|---|
| `TestABatchedAddBuildsTheSameWindowAsNSeparateCalls` | the corpus built both ways, then every element's `type`/`name`/`caption`/`style`/`tooltip` compared on the stub. **The equality against the per-call path is the correctness argument**, exactly as in 4a. |
| `TestOneCrossingBuildsATree` | a flow whose two children name it by index; the stub records the parent it was called on |
| `TestAForwardParentReferenceIsRefused` | element 2 naming element 3 is `ERR_BAD_ARGS` before anything is built |
| `TestABatchStopsAtTheFirstRefusalAndSaysHowMany` | the stub raises on element 7; handles 0-6 valid, count = 7, `last_error` naming it, elements 8+ never attempted |
| `TestARefusedBatchDoesNotUnwind` | the six elements built before the refusal are still there — the DECISION pinned as behaviour, so a later "tidy-up" moves a test |
| `TestTheStringTableIsDecodedOncePerDistinctString` | a counting `read_string` stand-in: 50 rows of 5 fields with 55 distinct strings makes 55 calls, not 250. **This is the assertion the whole feature is about** and it is what a future refactor would break silently. |
| `TestAnAbsentOptionalFieldIsNotSetAtAll` | the presence byte, per field, distinguishing "absent" from "present and false" — the F2 shape |
| `TestALocalisedStringCaptionCrossesAsAnArray` | the one field that is not a plain string |
| `TestTheBatchMemberIDSurvivesTheGeneratedWrapper` | the pruning assertion, the R6 shape, red-proven |
| `TestBothBackendsBuildTheSameSpecBlock` | Go and Rust encoding one corpus to identical bytes — the AD5 shape, one test over both backends |

### Red proofs

1. **Decode each string per element instead of once per batch**: the counting
   test goes 55 → 250 and the timing leg goes 2,964 → 6,013 ns/element. This is
   the proof that the pool is what the feature is.
2. **Accept a forward parent reference**: element 2 attaches to nothing and the
   window is silently wrong — which is why the refusal is checked before
   anything is built rather than at the element.
3. **Unwind on refusal**: the stub's destroy counter rises and
   `TestARefusedBatchDoesNotUnwind` fails, which is what makes the decision a
   decision rather than an accident.

### Gates

`make test`, `gen-bindings --check`, `make check-lua52f`. **No emitter change,
so no spectest movement and no golden diff outside the generated bindings.**
Member ids do not move (this adds no member), so no downstream re-pin.

---

## Recommendation

**NEEDS AN ORCHESTRATOR DECISION, and it is a sequencing one rather than a
technical one.**

The design is sound and the win is measured at **6.9× for the first cut**
(0.146 against the per-call form) and 12.2× if an intern table follows. What
makes it a decision rather than a go-ahead is this:

> **Round 2 is already generating a typed args struct for `add`.** The survey's
> round-2 item is *"typed args structs for the four variant-defeated members,
> generated from the shared parameters plus an `Extra` pair-list escape hatch
> for the variant tail"*. That struct and this batch's spec block are **the same
> artifact**: 15 shared parameters, optional presence bytes, `(ptr, len)`
> strings, and a tail. If round 2 lands first and its struct crosses as a tier-1
> block rather than as a tier-2 map, then **round 2 has already bought most of
> the 3.3×** and 4b is the string pool and the parent column on top of it —
> about 250 lines rather than 600.
>
> If round 2's `Extra` escape hatch is a `K_DYN` field, the two are still
> compatible: the batch carries the typed block plus one dyn slot per element,
> and only the variant tail pays `read_dyn`.
>
> **The two must not produce two spec shapes for one member.** That is this
> repo's most-repeated failure shape (two things spelling one fact) pointed at a
> generated struct, where it would be silent.

So the recommendation, precisely:

- **If round 2 has not started**: hand it this measurement, because it changes
  what round 2's typed `add` is FOR. Round 2 filed it as ergonomics ("every
  element is a hand-built nested pair list"); it is a **3.3× performance
  change** as well, and a typed struct that still crosses as tier 2 would get
  the ergonomics and none of the speed.
- **If round 2 has landed**: 4b is a small follow-on and should be built —
  ~250 lines, one import, and the string pool is the whole of the new idea.
- **Either way, do not build the survey's shape.** An array of tier-2 maps is
  0.868× and would close the item having moved nothing.

**Coupling notes.** `internal/factorio/used.go` (with 4a and possibly round 1),
`gogen.go`/`rustgen.go`'s member loop (with 4a and round 2), and the spec block
itself (round 2, load-bearing — see above). Nothing touches the emitter, the
persistence modes or `layout.go`'s existing shapes.
