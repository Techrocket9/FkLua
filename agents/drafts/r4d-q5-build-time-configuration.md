# 4d — Q5, the build-time configuration channel

**Round 4, design-and-measure. Verdict: THE ITEM HAS LARGELY CLOSED ITSELF AND
NOBODY WROTE IT DOWN. Q5's original form is fixed by the data stage becoming a
guest — demonstrated end to end below — and its second form is fixed by an
engine prototype both halves of which already bind. What is left is DOCTRINE,
at S, plus one small optional helper. The queue amendment's own phrasing
("a config written once reaching both a build tag and a startup setting") does
not match the finding and should be corrected.**

---

## What Q5 actually says

Q5 is `qol_research`'s fifth finding, not a global index (the `Q` prefix is that
port's; `AD` is autodeconstruct's, `G` nixie-tubes', `FTS` FuelTrainStop's,
`RM` resource-marker's). Verbatim, the observed half:

> A FkLua guest cannot: its control stage is compiled Go and `fklua mod` writes
> every byte of `control.lua`, so there is no `require`. This port carries the
> table twice, in `mod-data/prototypes/categories.lua` and `guest/go/config.go`,
> and a disagreement between them is a mod that toggles technologies that do not
> exist or grants half the bonus it says it does.

and what it asked for:

> What would close it is a documented convention, or a helper, for the data
> stage to publish a blob the guest can read at load.

**Read that carefully and there are three different questions inside it**, and
the queue amendment collapses them into a fourth that is in none of them.

| | the question | status |
|---|---|---|
| **Q5-a** | a config known at AUTHORING time, needed by both the data stage and the control stage | **CLOSED.** Both stages are Go now, in one module. Demonstrated below. |
| **Q5-b** | a config the DATA STAGE COMPUTES (from a startup setting, from what other mods defined) and the control stage needs at load | **CLOSED by the engine**, via the `mod-data` prototype, whose read side is fully bound. Not written down anywhere. |
| **Q5-c** | a config that must select COMPILED CODE — a Go build tag, a cargo feature | **not a thing anyone asked for.** See below. |

**Q5-c is the queue amendment's phrasing and it has no evidence behind it.**
The whole ports campaign — five findings ledgers, the campaign roll-up, and the
Factorio-facing findings file — contains **no mention of build tags at all**.
The amendment reads *"a config written once reaching both a build tag (guest
compile-time) and a startup setting"*; Q5's own text is about a config reaching
two STAGES, both at load, neither of them a compile. The amendment appears to be
a synthesis rather than a quotation, and acting on it would build a mechanism
for a requirement nobody has.

There is also a reason to refuse Q5-c on its own terms, stated so it does not
come back: **a build tag is fixed when the author compiles and a startup
setting is chosen by the player, so the two can only ever agree about the
DEFAULT.** The moment a player moves the setting, the compiled half is stale.
A config that genuinely has to be compile-time is not a setting, and a config
that is a setting cannot be compile-time. The only coherent overlap is "the
manifest names a value, the build bakes it in, and the data stage emits a
setting whose default is it" — which is Q5-a with an extra artifact, and Q5-a
already has an answer that needs no manifest key.

---

## Q5-a is closed, demonstrated rather than argued

When Q5 was filed the data stage was hand-written Lua and the control stage was
compiled Go, so there was genuinely no channel: `fklua mod` writes every byte of
`control.lua` and there is no `require` into it. **Since the data stage became a
second wasm module compiled from the same Go module, the channel is an ordinary
Go import.** The rule that makes it work is one sentence: a data module may not
import `fkapi` (`fklua mod` refuses it) and a control guest may not import
`fkdata` — so **the shared package must import neither.**

I built it and ran it:

```sh
# guest/go/examples/q5cfg/cfg.go -- imports NOTHING
package q5cfg
type Category struct { Name string; Rungs int; Scale float64 }
var Categories = []Category{{"mining-speed", 8, 0.05}, {"inserter-capacity", 4, 1.0}, {"lab-speed", 6, 0.10}}
const SettingPrefix = "q5-"

# two mains, both importing it
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 -o /tmp/q5ctl.wasm  ./examples/q5ctl)
(cd guest/go && tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 -o /tmp/q5data.wasm ./examples/q5data)
./bin/fklua mod /tmp/q5ctl.wasm --data-module /tmp/q5data.wasm --name q5-probe --api=2.0.77 -o /tmp/q5out
```

`fklua mod` packages it without complaint, wiring `fk_settings -> settings.lua`,
`fk_data -> data.lua` and `fk_on_init -> script.on_init`. Driven through the
**real generated stage files** under `bin/lua52f` with an engine-shaped
`data:extend` stub and an engine-shaped `script` stub:

```
--- the settings stage emitted ---
  int-setting q5-mining-speed default=8
  int-setting q5-inserter-capacity default=4
  int-setting q5-lab-speed default=6
--- the control stage logged ---
  q5 control: q5-mining-speed
  q5 control: q5-inserter-capacity
  q5 control: q5-lab-speed
```

**One Go file, two compiled guests, one packaged mod, and both stages produce
the same three names, the same count and the same defaults from one source.**
That is Q5's requirement, met, with no new mechanism at all.

**It is not a hypothetical, either — it is what the first real consumer already
does, three times.** BetterBeltBalancer's `guest/go/engine` (the Factorio-version
branch, called by both stage hooks), `guest/go/skin` (the sprite sheet's cell
count, which the prototype and the runtime had each written down separately),
and `guest/go/obs/protos` — whose own note is the clearest statement of the
pattern anyone has written:

> the loader NAME a suite's data stage defines and its observer places was
> written down twice per suite, forced, because a control guest may not import
> fkdata and a data guest may not import fkapi — so protos imports NOTHING AT
> ALL, which is what lets both halves have it

That mod found the pattern independently and recorded it as a local note. **The
gap is that FkLua never wrote it down.** `docs/data-stage.md` explains the two
modules and why they are two; it does not say that they are one Go module and
that a shared package is therefore the channel. An author reading it would
conclude what Q5 concluded.

**The Rust side is the same shape and is unverified.** `guest/rust` is a cargo
workspace and a shared crate would work identically, but nothing has built it
and the campaign's own hazard applies: cargo's v2 resolver unifies features
across a workspace build, which is why `RustCollectorFeature` is passed on the
command line rather than declared. A shared crate declares no features and is
therefore outside that hazard — but "therefore" is not a measurement, and the
doc should either say it is unverified or somebody should build it.

---

## Q5-b is closed by the engine, and that is not written down either

Q5's own text describes upstream's workaround: the config is a base-N string in
a startup setting, decoded at the data stage, and the decoded result is smuggled
across the stage boundary in the `order` fields of hidden `virtual-signal`
prototypes. The port's own comment calls it an ugly hack. The campaign escalated
it to Factorio as feature request 17:

> **The ask.** A documented, first-class way for the data stage to publish a blob
> the control stage can read at load. The mechanism exists in every practical
> sense; what is missing is a sanctioned one.

**It exists, and it binds.** `LuaPrototypes.mod_data` is a
`LuaCustomTable<string, LuaModData>`, and `LuaModData` carries `data`
(a `dictionary<string, AnyBasic>`), `data_type`, `valid` and `object_name`.
Present in **both** committed descriptions, 2.0.77 and 2.1.17. And the bindings
are complete on both halves:

| generated binding | what it is |
|---|---|
| `LuaPrototypes.ModDataRaw() (Object, error)` | the point query — a handle, not a materialised table |
| `LuaPrototypes.ModData() ([]EntryStringObject, error)` | the whole table, if you want it |
| `LuaModData.Data() ([]EntryStringValue, error)` | the blob |
| `LuaModData.DataType()`, `.DataTypeIs(want string)` | the discriminator, with the host-side compare |

The write side is a data-stage prototype, which `fkdata.Extend` emits like any
other. So the whole channel is:

```go
// the data guest, after it has computed whatever it computed
fkdata.Extend(fkdata.Obj(
    fkdata.KVs("type", fkdata.Str("mod-data")),
    fkdata.KVs("name", fkdata.Str("my-mod-config")),
    fkdata.KVs("data_type", fkdata.Str("config")),
    fkdata.KVs("data", /* the blob */),
))

// the control guest, at load
raw, _ := fkapi.LuaPrototypes{Object: fkapi.PROTOTYPES}.ModDataRaw()
md, _  := fkapi.LuaCustomTable{Object: raw}.Get(fkapi.OfString("my-mod-config"))
blob, _ := fkapi.LuaModData{Object: md}.Data()
```

**Two caveats and one open item, all of them honest.**

- **The `mod-data` PROTOTYPE's existence is inferred, not probed.**
  `LuaModData.data_type` and a `prototypes.mod_data` table keyed by string are
  strong evidence that a `mod-data` prototype type exists and that this is what
  fills it, and one of the campaign's own audited mods (`nixie-tubes`) is
  recorded as shipping Lua source in `mod_data`. But this pass did not run
  `--dump-data` over a mod emitting one, and **the recommendation below should
  not be published until somebody has.** `scripts/run-datastage.sh` is the
  instrument and it is about three seconds per arm.
- **`AnyBasic` is scalars, strings, booleans and nested tables of them.** A blob
  is data, not a Go value; the guest still writes a decoder. That is the
  residual and it is much smaller than a base-N string codec.
- **The determinism story is the data stage's and it is already stated.** The
  data stage runs per client and a divergent prototype set is a **join refusal**
  rather than a desync — Factorio checksums the prototype list. So a blob
  computed from startup settings is identical on every peer that could join at
  all, which is exactly the property a control guest branching on it needs.

---

## What is actually missing

Everything above is mechanism that exists. What is missing is that **an author
cannot find any of it**, and that is Q5's own request word for word: *"a
documented convention, or a helper"*.

### The doctrine, which is the deliverable

A page — `docs/sharing-config.md`, or a section of `docs/data-stage.md` — that
says the three things in order:

1. **A config known when you write the mod goes in a Go package (or a Rust
   crate) that imports neither `fkapi` nor `fkdata`, and both guests import it.**
   With the constraint stated as the reason rather than as a rule, because the
   reason is what makes it memorable: `fklua mod` refuses a data module that
   imports `fkapi`, a control guest cannot import `fkdata`, and a package that
   imports neither is what both halves can have. With the worked example above.
2. **A config the data stage COMPUTES goes in a `mod-data` prototype**, read at
   load through `ModDataRaw` and `LuaModData.Data`. With the caveat that it is
   data rather than a value, and with the determinism sentence.
3. **A startup setting is readable from BOTH stages and is not a channel.** The
   data guest reads it through `fkdata.StartupSetting` (and gets nothing at the
   settings stage, by design, because a mod's settings are not readable while
   they are being declared); the control guest reads `settings.startup` at
   runtime. What a setting cannot do is carry a DERIVED value, which is what
   sends people to the virtual-signal hack.

**Cost: S. It is one page and it invents nothing.**

### The optional helper, and why it is optional

If the orchestrator wants a mechanism as well as a doctrine, the one worth
having is a **typed accessor pair over `mod-data`** — `fkdata.PublishConfig(name,
value)` on the data side and `fkapi.ModDataBlob(name) (Value, error)` on the
control side — so the two ends are named the same thing and the shape cannot
drift. That is ~40 lines per language and it buys naming rather than capability.

**It should NOT be a `fklua.toml` key.** The manifest is where identity, the
pin, the GC arm and the data module live, and the repo's most-repeated failure
shape is *two commands disagreeing about one manifest key* — recorded four
times, for `api`, `lang`, `gc` and `--dependency`. `agents/abi.md` already
refused the manifest for the callback seam on exactly this ground, **citing Q5
by name**:

> the names would then live in two places, the manifest and the guest's own id
> switch, which is the "configs written twice" complaint fklua-ports already
> recorded against the data stage (its Q5)

A config in `fklua.toml` that both guests then have to agree with is Q5's own
defect with the second copy moved into a TOML file. **The Go package IS the
single source; adding a manifest key would create the second one.**

---

## Alternatives considered and rejected

| alternative | why not |
|---|---|
| **`[config]` in `fklua.toml`, generated into a constants file in each guest tree** | Precedent exists (`gen-bindings` writes 1,137 define accessors to a hard-coded path, `--check`ed and hashed by `fklua lock`), so it is buildable. It is the wrong shape: it makes the manifest a second place the config lives, which is the failure this item is ABOUT, and it buys nothing over a Go package that both guests already import. The one thing it would buy is reaching a NON-guest consumer — `info.json`, a locale file, a test script — and no port has asked for that. |
| **Build tags / cargo features driven from the manifest** | Q5-c, above: no evidence in the campaign, and a compile-time value and a player-chosen setting can only agree about the default. Also carries the recorded cargo hazard (v2 feature unification across a workspace build) and the recorded lesson from the deleted `fkgcheap4`/`fkgcheap64` tags, whose per-arm `.bss` cost the docs got wrong by 39%. |
| **A `fk_data`-to-`fk_on_init` in-memory channel inside FkLua** | The two modules are two Lua states in two stages of one game load; there is no memory between them that FkLua owns. `mod-data` is the engine's answer and it is better than anything this project could invent. |
| **Blessing the virtual-signal `order`-field smuggle** | It is upstream's workaround for a mechanism that now exists. Recording it as history is right; recommending it is not. |
| **Doing nothing, on the grounds that BBB found the pattern unaided** | Three mods each found the `guest/go` scaffold deviation unaided too, and the repo's own conclusion was that three mods converging on one deviation IS the report. The same reasoning applies: one mod deriving the pattern three times is evidence the pattern should be written down, not evidence that it need not be. |

---

## Implementation plan

**Estimated size: one documentation page (~150 lines), one probe, and
optionally ~80 lines of helper. This is an S item.**

### Files touched

| file | what | ~lines |
|---|---|--:|
| `docs/data-stage.md` | the three-way section above, with the worked shared-package example and the `mod-data` recipe | 110 |
| `docs/factorio-api.md` | one paragraph pointing at it from the runtime side, beside the existing `settings.global` note | 15 |
| `agents/datastage.md` | the working note: why the shared package must import neither, and the three downstream instances | 30 |
| `agents/lua-temptations.md` | Q5's row corrected — closed in two of its three forms, with the amendment's phrasing withdrawn | 10 |
| `guest/go/examples/` | **optionally** the two-main shared-config example, which is what makes the doc checkable rather than quotable | 60 |
| `guest/go/fkdata`, `guest/go/fkapi` shim | **optionally** the typed `mod-data` pair | 80 |

### The probe that has to happen first

**One `--dump-data` run over a mod whose data guest emits a `mod-data`
prototype**, to establish that the engine accepts it and that
`prototypes.mod_data` is filled from it. `scripts/run-datastage.sh` is the
instrument, ~3 s, and it needs a real Factorio install. Until that runs, the
`mod-data` half of the doctrine is inference from the bindings and must be
marked as such — which is this repo's own rule about probing an entry point
before publishing a recipe for it, stated in the survey for the simulation
bridge and applying here word for word.

### Test plan

| test | what it pins |
|---|---|
| `TestASharedPackageReachesBothStages` | the two-guest example built, packaged, and both stage files driven under `lua52f` — the exact run reproduced above, as a gate. **This is the whole doctrine as a test**, and it fails the day `fklua mod` starts refusing a data module for a reason the doc does not mention. |
| `TestADataModuleStillRefusesFkapi` | the constraint the pattern rests on, which today is asserted nowhere near the doc that would depend on it |
| the `--dump-data` arm | `mod-data` emitted and present, once the probe has run |

### Red proofs

1. **Have the shared package import `fkapi`**: `fklua mod` must refuse the data
   module, naming it — which is the sentence the doctrine turns on, so it should
   fail loudly rather than by inference.
2. **Change one field in the shared package**: both stage outputs move together.
   That is the property Q5 asked for and it is what a two-copy config cannot do.

---

## Recommendation

**DESIGN STANDS; SHIP THE DOCTRINE, NOT A MECHANISM — and correct the queue
amendment.** Three things for the orchestrator, in order:

1. **Correct the round-4 entry.** It reads *"Q5, the build-time configuration
   channel (a config written once reaching both a build tag and a startup
   setting)"*. Q5 is neither about build tags nor about a startup setting
   reaching a compile; it is about one config reaching two STAGES, and the
   campaign contains no mention of build tags anywhere. Left as it stands it
   asks the next agent to build a mechanism for a requirement nobody filed.
2. **Ship the doctrine page.** S, invents nothing, and it is Q5's own stated
   remedy. The shared-package half is demonstrated end to end above and should
   land with the two-guest example so the doc is checkable.
3. **Run the `--dump-data` probe before publishing the `mod-data` half.** It is
   three seconds and it is the difference between a recipe and a guess. This is
   the ONE open question in this item.

**What this does NOT close**, said plainly so the item is not marked done on a
half-truth: the Rust side of the shared-package pattern is unbuilt and
unverified here, and `AnyBasic` means a `mod-data` blob is still decoded by
hand at the far end. Neither blocks anything; both belong in the doc as
residuals.

**Coupling notes.** None, in either direction. This touches no generator, no
binding, no runtime Lua, no member id and no emitter, so it is independent of
4a, 4b, 4c and of rounds 1-3. The only shared surface is `docs/` and
`agents/lua-temptations.md`'s own verdict table, which every round edits.
