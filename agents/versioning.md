# API versioning

`fklua api pull | list | diff`, and how a Factorio upgrade is meant to go. **Read before touching `internal/factorio/apidiff.go` or the `api` command.**

**Status: M9 is complete.** `api pull/list/diff/check`, `docs`, `fklua.toml`/ `.lock` and the weekly regeneration bot are all built, and both gates are met.

---

## Why this is a feature and not a chore

Factorio ships breaking API changes on its own cadence, and `latest` is routinely ahead of a typical install — 2.1.12 against a 2.0.77 install while this was written. "Will my mod survive the upgrade" is a real question with a mechanical answer, and answering it without building anything is the point.

Two version axes, and only one of them moves:

- **`api_version`** — the JSON schema. **6**, and stable across every version seen. One generator serves N data sets because of this, which is why a change to it is the single most dangerous thing the diff can report.
- **`application_version`** — the game. This is what actually changes.

**And there is a THIRD axis this file used to leave implicit, which is the one that gets confused: the PIN is not the ENGINE.** The pin — `DefaultAPIVersion`, `fklua.toml`'s `api`, `--api=` — is a BUILD-TIME choice of which `runtime-api.json` the bindings and the packaged member table come from. The engine is whichever Factorio a player launches, and nothing in the repo knows it; a guest that needs to know asks `helpers.game_version` at RUN TIME. They touch in exactly one place, `info.json`'s `factorio_version`, which is a claim about the ENGINE and merely DEFAULTS to the pin's major.minor.

Two consequences, and both have already cost something:

- **The default pin is the general-availability release** (2.0.77), not the newest description committed. A default is what an author who pinned nothing ships to players. This machine's in-game gates run on a 2.1.16 install and package with `--factorio-version 2.1` for that reason — see `scripts/lib-engine.sh`, which is the one place that derivation lives.
- **A run-time floor is not a pin floor.** fkipc requires an engine of 2.1.14 or newer and gates on `helpers.game_version`, so a GA-pinned mod gets the whole library on a newer engine with no rebuild and no repin. Every member it calls is in the 2.0.77 description.

**And moving the pin is a BUILD-IDENTITY change, not only a bindings change.** `fklua mod` folds the resolved `--api` version into the build stamp a save records (`cmd/fklua`'s `buildID`; the construction is in [`agents/guests.md`](agents/guests.md), "Recompiling, and `fk_migrate`"), so repackaging the same wasm against a new pin fails `same_build()` and the load takes the rebuilt-guest path. That is the point rather than a cost: member, event and define ids are dense sorted indices over one version's set, so a pin bump shifts them wholesale, and the packaged tables reach into the guest heap — a cached buffer's size comes out of the packaged event table, a cached define id is a per-build number. Before 2026-08-07 the stamp hashed the wasm alone and those two packages shared one identity. **So a pin bump costs a guest its heap, exactly as a recompile does** — which is what `api diff`'s classification is for, and one more reason to bump deliberately rather than incidentally.

**And a pin bump does NOT move the mod's version, which is what made this the worst case of a defect fixed on 2026-08-07.** The rebuilt-guest path was reached from `on_configuration_changed` alone, and Factorio raises that for a mod-set change; a repackage against a new pin leaves `info.json` untouched. So "takes the rebuilt-guest path" above was, for two milestones, only half true: `state_load` declined the heap and nothing finished the job — no migrate dispatch, no warning, and no republished stamp, which left the save self-inconsistent and every later load declining again. The decline is now acted on at the first replicated dispatch as well; see CLAUDE.md, "a declined heap".

## `api pull` — **built**

```sh
fklua api pull 2.1.12          # from lua-api.factorio.com
fklua api pull --from-install  # from the local game's doc-html
```

Writes `api/<version>/runtime-api.json`, which is **committed**. A build must never reach the network and CI must never depend on lua-api.factorio.com being up, so pulling is a deliberate, separate act.

**The version comes from the file, not from the argument.** `latest` is a real endpoint that resolves to whatever shipped most recently; trusting the argument would file it under a directory named `latest` that silently means something else next month. The command says so when they differ.

## `api diff` — **built, and this is M9's gate**

```sh
fklua api diff 2.0.77 2.1.12              # markdown, breaking first
fklua api diff 2.0.77 2.1.12 --breaking   # just the breaking list; exits 1 if any
fklua api diff 2.0.77 2.1.12 --json d.json
```

`census.json` answers *how much* moved; this answers *what* moved and whether it breaks you. Neither substitutes for the other — a release that removes one member and adds one leaves every census count identical.

### The classification, drawn from the guest's side

| | |
|---|---|
| **BREAKING** | a guest that compiled before may now fail or behave differently |
| **additive** | new surface; nothing that existed changed |
| **cosmetic** | descriptions and examples; regenerating differs, behaviour does not |

Two calls that are not obvious, and both follow from FkLua's wire format rather than from Lua's semantics:

- **Adding a parameter is BREAKING**, though Lua would not care. Arguments are laid out positionally in a fixed block, so a new one moves every offset after it and a guest built against the old layout writes into the wrong slots. The same applies to a new field in a table concept or an event.
- **A union gaining an alternative is additive**, losing one is breaking. Tier 2 carries a union dynamically, so a reader tolerates a new arm; a writer of a removed arm does not.

An attribute's readability and writability **are** the presence of `read_type` and `write_type`, so one comparison covers both the access and the shape.

### Field-level, because the list is meant to be read

A table concept's whole signature is unreadable at API scale — a `BlueprintControlBehavior` renders as ~900 characters of which four matter. So two tables diff **field by field**, and two unions by alternative:

```
BREAKING  AccumulatorBlueprintControlBehavior — field "output_networks" added, which moves the fields after it
BREAKING  LuaAssemblingMachineControlBehavior::include_fuel — attribute removed
BREAKING  FluidBoxFilter — concept removed
```

The whole-signature form survives as the fallback for shapes that are not comparable field-wise: "it changed, and here is both" still beats silence.

### 2.0.77 → 2.1.12, measured

**200 breaking, 578 additive, 52 cosmetic.** The breaking set is mostly one shape — 2.1 added circuit-network selection to nearly every blueprint control behaviour, and each new field moves the offsets after it.

`TestAPIDiffClassifiesAgainstAHandCheckedExpectation` is the gate. It names specific changes verified against the raw JSON rather than asserting a total: a test pinning "200 breaking" would pass whatever the classifier did, as long as it kept doing it. It also asserts that additive **outnumbers** breaking, which a classifier that called everything breaking would fail while passing everything else.

Two properties worth their own tests: a version against itself produces the **empty** diff, and a schema bump is always reported.

## `api check` — **built, and this is M9's other gate**

```sh
fklua api check my-mod.wasm --to 2.1.12
```

The plan calls this the feature that matters most to a mod author, and the reason is arithmetic: 2.0.77 → 2.1.12 has **200** breaking changes and a typical mod touches a few dozen members, so the honest answer for most mods is "none of them are yours" — but only a cross-reference can say that, and without one an author reads 200 lines hunting for the four that matter.

```
# api check: 2.0.77 -> 2.1.12

This guest touches 1 member(s), 0 event(s) and 0 named type(s).

**1 breaking change(s) affect this guest**, out of 200 in the release.

- `LuaAssemblingMachineControlBehavior::include_fuel` — attribute removed
```

The manifest is not new work. `UsedMembers` already recovers exactly which members a compiled guest references, because the table pruner needs the same answer to ship 1 KB instead of 1,032 KB.

**The surface is more than the member list.** A guest calling a member whose argument is a `MapGenSettings` breaks when `MapGenSettings` gains a field, even though the member itself did not change — so the check also collects every named type reachable from the signatures it uses, recursively through structs, arrays and dictionaries. Without that it would miss the *dominant* shape of the 2.0.77 → 2.1.12 breakage, which is table concepts gaining fields.

Exits non-zero when something breaks, so CI can gate without parsing. **It also exits non-zero when the scan was incomplete** — if a member id was not a compile-time constant the check cannot see everything, and unproven is not a pass.

### The gate is a deliberately broken fixture, plus a control

A check that reports "clean" for every guest passes trivially and is worth nothing — and that is the live failure mode, because most guests really are unaffected by most releases, so a broken implementation looks identical to a working one on real input.

`TestAPICheckCatchesABrokenGuest` builds a guest that calls `LuaAssemblingMachineControlBehavior::include_fuel`, removed in 2.1.12, and asserts it is reported: **1 hit, 199 ignored**. The control is `examples/api`, which must come back clean — without it, a check that reported every breaking change would pass the first half.

## `fklua init` and `fklua lock` — **built**

`fklua.toml` is what an author writes; `fklua.lock` is what the toolchain derives. The split earns its keep for a specific reason here: the bindings a project builds against are **generated**, so "which API did this come from" is not answerable from the source tree unless something records it.

```toml
[mod]
name = "my-mod"
version = "0.1.0"
factorio_version = "2.0"

[fklua]
api = "2.0.77"
lang = ["go", "rust"]
```

`fklua lock --check` distinguishes three failures, because each means something different and each has a different fix:

| | |
|---|---|
| the pin moved | `fklua.lock` pins 2.0.77 but `fklua.toml` says 2.1.12 — run `fklua lock` |
| the description moved | the `runtime-api.json` for a **pinned** version changed underneath the lock, which should be impossible; something edited `api/<v>/` |
| the bindings moved | generated code was edited by hand, or `gen-bindings` was not re-run |

The tree hash covers **paths as well as contents**, over a sorted list. A hash that depended on readdir order would differ between machines and the lock would be worthless; one that ignored paths would not notice a generated file being renamed.

**The TOML reader is hand-rolled**, and rejects an unknown key rather than ignoring it. `go.mod` requires exactly `watgo` and a config file with six keys does not justify breaking that — but the strictness is the load-bearing part: `apy = "2.0.77"` silently doing nothing would leave the project unpinned while `--check` compared against the default, and nothing would ever say so.

## The pin stamp, and repinning a vendored checkout — **built**

Everything above is about *which* pin a project chooses. This is about what happens when a single build ends up with **two**, which it can do without anybody choosing anything.

### The arrangement, which is structural rather than a mistake

`fklua mod` packages the member, event and define tables at the project's pin. The guest's ids come from generated bindings. Those are produced by two different commands at two different times, and **each succeeds on its own** — so until 2026-08-08 nothing checked the pair, and a mismatch had no symptom except the guest calling different members.

The way a consumer reaches it without doing anything unusual:

| set | generated from | who imports it |
|---|---|---|
| the project's own `guest/go/fkapi` | its `fklua.toml` pin | its own guest code |
| the **vendored FkLua checkout's** `guest/go/fkapi` | whatever upstream committed — `DefaultAPIVersion` | **`fkipc`**, and every other library package living in that guest module |

`fkipc` is hand-written and lives *inside* `guest/go` (and `guest/rust`), so it imports **that module's** `fkapi` — not the consumer's. A consumer that vendors a checkout and pins anything other than the default therefore links bindings at the default and packages a table at its own pin, by construction.

**Measured downstream at pin 2.1.14 against committed bindings at 2.0.77:** `fkipc` subscribed to event **207** believing it was `on_udp_packet_received` and got `on_train_changed_state`; it read `helpers.game_version` and got `LuaForce.object_name`, so the engine-floor gate parsed `"0.0.0"`, concluded it was below its own floor, and correctly went **inert**. The mod loaded, ran, logged and ticked. **One log line about a version was the entire symptom of a mod calling the wrong half of the API.**

Note what this is *not*: it is not `fkipc` being wrong, and its floor is still on the ENGINE (see the run-time-floor bullet above). It is two binding generations meeting in one wasm.

### The stamp

`gen-bindings` emits, into every generated binding set, **one exported function whose name carries the version**:

```go
//go:wasmexport fk_api_pin_2_0_77          // guest/go/fkapi/fkapi.go
func fkAPIPin() {}
```
```rust
#[no_mangle]                                // guest/rust/fkapi/src/api.rs
pub extern "C" fn fk_api_pin_2_0_77() {}
```

`attachAPI` reads it out of the module's export section and refuses the package when it is not the pin being packaged, naming both versions, where the packaging one came from, and the two ways out. `internal/factorio.PinExport` builds the name for **both generators and the checker** — one function, because a checker that mangled differently from a generator would find no stamp and say nothing, which is the behaviour it exists to replace.

Four choices, each of which had a plausible alternative:

- **An export, not a call the `used.go` scan proves.** A call only exists in the module if something reaches it, and nothing in a binding set reaches a stamp: TinyGo runs `-opt=2` then `wasm-opt`, and the Rust release profile is `lto = true`, so a call-shaped stamp is deleted. An export is a **root** — the module's ABI surface, which no optimizer may drop. Measured in both toolchains, including out of a Rust `rlib` nothing references.
- **The name, not a returned constant.** A version is a string and a wasm result is a number, so a body would have to encode one. A name also needs no code analysis and survives any rewriting of the body.
- **Absent stamp ⇒ silence.** Bindings older than the stamp carry none, and so does a guest linking no generated bindings. Refusing those would break correct builds — including GA-pinned ones — to catch what cannot be proven. This is the opposite call from `api check`, which exits non-zero on an incomplete scan, and for the same reason: there the alternative is reporting a guest clean that it could not read, here it is refusing a build that is right.
- **Only when a table is actually attached.** A guest that calls no member, subscribes to no event and reads no define gets no table, and a table that does not exist cannot disagree with anything — the same line `compile` sits on.

It costs an existing save nothing: the stamp lives in the *bindings*, so a guest only acquires one by being rebuilt, and a rebuild moves the module hash — and therefore the build stamp — already.

**What it does not cover**, deliberately: a description **edited underneath a committed version directory** moves the ids while the version string stands still. That is `fklua lock --check`'s `api_sha256`, which is the right place for it — the lock is about the source tree, the stamp about the compiled artifact.

### `fklua gen-bindings --into DIR`

The supported way to repin a vendored checkout, and the remedy the refusal names:

```sh
fklua gen-bindings --into vendor/fklua            # at THIS project's pin
fklua gen-bindings --into vendor/fklua --check    # a standing gate
```

The pin and the language list come from **this** project's `fklua.toml`; the files land at `DIR/guest/go/fkapi/fkapi.go` and `DIR/guest/rust/fkapi/src/api.rs`. It writes **neither** the Rust crate scaffolding (`Cargo.toml`, `lib.rs` are static, say nothing about the pin, and belong to the vendored snapshot) **nor** a census (that is a fact about the checkout owning `api/`, which is not this project). `-o` and `--into` are mutually exclusive: `-o` names one file for one language, `--into` names a checkout for every language the manifest declares.

**A consumer's whole recipe**, which is one line per idea:

```sh
fklua gen-bindings                       # this project's own bindings
fklua gen-bindings --into vendor/fklua   # what fkipc actually imports
fklua lock
```

…and `--check` on the first two in CI. A resync restores upstream's committed file, so the second line is what every re-extract has to be followed by — and if it is forgotten, `fklua mod` now refuses instead of shipping.

## Moving the default pin

**Distilled from the two migrations this repo has actually performed** — 2.0.77 → 2.1.14 on 2026-08-06, and back to 2.0.77 on 2026-08-07 — rather than from what the change looks like it ought to involve. Both times the constant took about a minute and everything below it took the rest of the day, and both times something in step 4 was found by a reader rather than by a gate.

It is a **deliberate** change. A pin bump costs every existing save its guest heap (see the build-identity note above), so it is not something to do because a newer description happens to be committed.

**1. The constant.** `internal/factorio/api.go`'s `DefaultAPIVersion`. That is the only line: `DefaultFactorioVersion` is `majorMinor` of it, `internal/factorio`'s test `apiPath` follows it, and `cmd/fklua`'s no-manifest path reads it. Anything that spells a version a second time is a bug — the whole reason those are derivations is that two constants which must agree about one fact is this repo's most-repeated failure shape.

The description must already be committed under `api/<version>/runtime-api.json`; `fklua api pull <version>` first if it is not.

**2. Regenerate, with `--lang=all`.**

```sh
go build -o bin/fklua ./cmd/fklua      # the constant is compiled in
./bin/fklua gen-bindings --lang=all
./bin/fklua gen-bindings --check
```

One command writes all three things that have to move together: `guest/go/fkapi`, the whole `guest/rust/fkapi` crate, and `api/<version>/census.json` — the last of those **for every description this checkout owns, not only the one being pinned**. A member id is a dense sorted index per version, so bindings from one description against a table from another call the wrong member silently, in a lockstep game — half a regeneration is worse than none.

**Half a regeneration is now REFUSED rather than silent**, because the pin stamp travels with the bindings: a guest built against whichever half did not move is turned away by `fklua mod` naming both versions. That is a backstop for the language you forgot, not a substitute for `--lang=all` — nothing catches it until somebody packages a guest.

**Say `--lang=all` rather than relying on the default.** With no `fklua.toml` in the working directory the default already IS `all`, so the bare command happens to be right here; run it one directory over in a Go-only mod project and it picks up that project's `lang` and leaves the Rust crate a version behind, which `--check` then reports as a codegen failure. Being explicit costs eight characters. **The census pass ignores `--lang` entirely** and always counts both backends, which is what makes `rust_members_bound` meaningful in a run that emitted no Rust.

**Read the `census moved:` block gen-bindings prints.** It is the review artifact for the whole change, and it is the only place a shape the generators newly cannot express shows up as a number rather than as nothing. It is printed per version now, headed `census moved (<version>):`.

**3. The census is no longer the file that goes stale invisibly, and it was for two milestones.** A census is written by whatever generation last ran against its description, and until 2026-08-24 the only generation that ever ran was the DEFAULT PIN's — so every other committed version's census was a snapshot of whichever day somebody last pinned it, and the generators gaining a row left all of them behind at once with nothing saying so. The 2026-08-07 revert found 2.0.77's census eight fields behind and treated that as a step of the recipe; that was the same defect seen from inside, and writing it down as a thing to remember is what let it recur.

**What it cost is downstream and it cost a whole pin move.** `gen-bindings --check` compares the census it would take against the committed one, and that check runs in a MOD PROJECT too. BetterBeltBalancer moved its pin to the committed 2.1.14 description the day the index-assign member kind added `index_setter_members`, and failed the check on a file it could not write — the write half refuses to write into the compiler, by design and correctly — while the suggested command, run in this checkout, would have regenerated 2.0.77's bindings and left 2.1.14's census exactly where it was. **No invocation of any command in any directory could make that check pass.** The pin move was reverted (BetterBeltBalancer, gap 24).

Two changes, and neither is a step to remember:

- **`gen-bindings` takes a census of every description this checkout owns**, whatever pin it was invoked at, and `--check` gates every one of them and names all the stale versions in one refusal. So a generator row that moves three versions' numbers moves three files in the round that added it. There is deliberately **no `fklua census` subcommand**: a second command writing the same file is the split `internal/factorio/census.go`'s own header argues against, and the repair for a stale sibling is the command a maintainer was going to run anyway.
- **A mod project's `--check` does not fail on it.** Nothing downstream READS a census — not `mod`, not `lock`, not either generator — so it is an FkLua-internal consistency artifact and a mod's gate has no business failing on one. It says so at notice level instead, naming the checkout and the command, which is the one place a downstream author learns the toolchain is behind its own numbers.

`TestEveryCommittedDescriptionHasACurrentCensus` is the standing gate in `go test`; it reported 2.1.14 four counts behind and 2.1.12 with **no census at all** — a pulled description used to arrive without one, and 2.1.12 had sat committed and censusless since the day it was pulled. `TestTheCensusMemberArithmeticCloses` and `TestBothBackendsBindTheSameMembers` still gate the shape of the result, and the CLI tests in `cmd/fklua` gate the three behaviours above.

**One consequence for the regeneration bot**, which is where the cost of this lands: `gen-bindings` now reads the description the bot has just pulled, so a release the generators cannot absorb turns that step red where before it was untouched. It is `continue-on-error` with its outcome folded into the draft condition, for the same reason the test step always was — that failure is the interesting one and the PR should carry it rather than be blocked by it. The compensation is that a pulled version now arrives WITH its census, which is what 2.1.12 did not.

**4. The count-bearing prose, file by file.** These are the numbers that are written down somewhere a person reads, and a build cannot check any of them. Take each from the regenerated `census.json` — never re-derive one by hand, and never carry one across from the other pin:

| where | what |
|---|---|
| `README.md`, "What a guest can actually do" | members bound/total, event payload structs, inherited forwarders, defines accessors, class operators, `Into` variants |
| `README.md`, the sample build output | `API <version>: N members, pruned from M` |
| `README.md`, the pruning bullet | the full member table's size |
| `README.md`, the fkipc wiring | how many event descriptors the scan prunes |
| `CLAUDE.md`, "Host ABI and bindings" | the same six numbers as the README's first row |
| `CLAUDE.md`, the paragraph that says which pin those counts are | rewrite it to describe the pin as it now IS; the previous migration's delta list is history and stays |
| `CLAUDE.md`, deliverables table, `bindings` row | bound and deferred |
| `agents/abi.md`, the status header and the census table | bound/skipped/deferred, and everything derived |
| `agents/abi.md`, the packaged-table sizes | full and pruned bytes, both per-pin |
| `agents/versioning.md`, the `docs` example | member count and markdown size |
| `agents/guests.md` | the bindings-compile test's member count |
| `agents/ipc.md` | the event-descriptor count the pruning scan works over |

**HISTORY STAYS HISTORY.** A section describing what a previous migration measured is correct about the past and must not be rewritten to the new numbers; what has to change is any sentence claiming a number is what the repo is like NOW. The two are easy to tell apart by tense and hard to tell apart by grep.

**5. `factorio_version`, which is the axis the pin is not.** `info.json`'s series defaults to `majorMinor(DefaultAPIVersion)`, and moving the pin across a major.minor boundary moves it. A 2.1 engine REFUSES a mod declaring `2.0` outright — *"Incompatible Factorio version (current: 2.1, required: 2.0)"*, at game start, before a line of the mod runs — so if the new default and the installed engine disagree, **every in-game gate breaks in a way that reads like a broken gate**. That is what `--factorio-version` and `[mod] factorio_version` are for, and `scripts/lib-engine.sh` is where the scripts get the installed series; nothing under `scripts/` should ever hard-code it. The hand-written probe mods under `testdata/` are stamped on copy by the same helper.

**6. The gates, in this order**, because each is cheap relative to the next:

```sh
gofmt -l . && go vet ./...
make test                                    # sdk/go and cargo legs included
./bin/fklua gen-bindings --check
./bin/fklua spectest --opt=3                 # bindings moved; run the wide net
./bin/fklua spectest --opt=3 --gc=collected
./bin/fklua spectest --nan=exact --opt=3
(cd guest/rust && cargo test -p fkipc &&
   cargo build --target wasm32-unknown-unknown --workspace)
```

...and then **in game**, which is the half nothing above reaches: `scripts/run-guest.sh`, `GUEST=gcsave ./scripts/run-guest.sh`, `scripts/run-roundtrip.sh` (whose **migrate leg packages one wasm at two pins**, so a pin move changes what that leg is comparing), and the IPC gates if the installed engine clears fkipc's floor.

**Expect host-side tests to fail for a reason that is not a defect.** A handful assert properties of a SHAPE that exists in one description — the `nil`-typed concept, a three-level nested container, the class-operator table. They pass at whichever pin happens to have the shape and fail at the other, which is a coupling rather than a property. The fix is for a shape test to name its own description: `internal/factorio`'s `loadShapeAPI` and `shapeAPIVersion` exist for that, and `operatorsByVersion` is the per-pin table for the one set that really did move between descriptions.

**7. And when GA itself moves to 2.1.x**, this whole recipe applies and **fkipc needs nothing**. Its floor is on the ENGINE and is read from `helpers.game_version` at run time; it has never been a statement about the pin, and `MinEngineVersion` does not move because a description did.

## The regeneration bot — **built**

`.github/workflows/api-regen.yml`, weekly. Polls for a release newer than anything committed and, when it finds one, opens a PR carrying the pulled JSON, regenerated bindings for every language, a regenerated census, and the classified diff as the PR body.

Three choices worth knowing:

- **It stops early when nothing is new**, so a quiet week costs one HTTP request and no PR noise.
- **A failing test suite makes the PR a DRAFT rather than blocking it.** The suite runs against the old pin, so red means the generators could not absorb the new description — which is the interesting failure and the one a human most needs to see, not a reason to hide the PR.
- **It is the only job allowed near the network**, and it reaches it exactly once. Nothing is ever built against the network: the JSON lands in the tree first, which is the same reason `api pull` is a separate command.

**It had never run to completion, and both reasons were in the shell rather than in anything Go builds.** Every scheduled run from the day `api list` grew its legend line died in the second step, and the failure arrived as an email rather than as a red mark on anything a person passes on the way to work.

- **A human-facing table is not a data interface, and it looks exactly like one.** The bot read the pin with `api list | awk '/^\*/ {print $2}'`, which matches the starred row and also the legend printed under the table — so it wrote two lines into `$GITHUB_OUTPUT` and GitHub answered `Invalid format 'is'`. The legend was a presentation change and could not have known it was editing an interface. `api list --current` is the machine-readable half now: one line, no decoration, and the table is free to grow a column or a footnote.
- **`git diff` cannot see a new version, because a new version arrives as an UNTRACKED directory.** The gate that stops the bot when nothing is new asked `git diff --quiet -- api/`, and git diff does not report untracked files — so the one gate whose whole job is to notice a new release was structurally unable to see one, and would have reported "nothing to do" and exited GREEN on exactly the week the bot exists for. That is this repo's *a skipped gate reads exactly like a pass*, one step further out than the two instances already recorded. It reads `git status --porcelain` now — which is also where the pulled version's number comes from, replacing "the highest directory on disk", so the two cannot disagree and a hand-dispatched older version is no longer diffed under somebody else's number.

Both are TEXT properties and neither is reachable by building anything, so `TestTheRegenBotDoesNotReadTheHumanTableOrAskGitDiffForANewDirectory` reads the workflow file itself and was confirmed to fail against both pre-fix lines.

## `fklua docs` — **built**

```sh
fklua docs --lang go   -o docs/     # 4257 members, ~770 KB of markdown at the 2.0.77 pin
fklua docs --lang rust -o docs/
```

Rendered from the **same `Report` and the same `Names` map** the generator produced, which is the entire design. Documentation built by a second walk of the JSON can disagree with the bindings and eventually would; here a member appears in the docs exactly when it was emitted, and under the name it was emitted as. `TestDocsNameExactlyWhatTheBindingsBind` asserts that rather than trusting the arrangement.

Two things it does that a straight dump would not:

- **A deferred member is absent from the docs.** Documenting something a guest cannot call is worse than omitting it — an author would write the call and find out later. The skip census is reported instead, under *Not bound*, so someone hunting a missing member learns it was skipped rather than never in the API.
- **Factorio's own `[label](runtime:Foo)` markup is stripped to the label.** It is not markdown any renderer here understands, and a dead link in every other description would be noise at 700 KB.

## Not built

Nothing in M9. What the plan lists beyond this — `--api` selection for `gen-bindings`, and C in `--lang` — belongs to M11 with the C guest.
