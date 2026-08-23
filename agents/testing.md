# Testing

The spec suite is the primary correctness gate. Coverage is a weak metric for a compiler; pass rate is the real one, and it may rise but never fall.

## Running it

```sh
make test          # go test ./...
make spectest      # the conformance suite under lua52f
./bin/fklua spectest --nan=exact          # the boxing mode
./bin/fklua spectest --opt=0              # and 1, 2, 3 -- all must be green
./bin/fklua spectest --filter i64 -v      # one file, with skip reasons
./bin/fklua spectest --update-passrate    # record a new baseline, deliberately
```

`testdata/spec/PASSRATE` holds the recorded baseline. A run that passes fewer assertions than it records fails the build.

### Green at the default level says nothing about the others

From M5 the emitter has four optimization levels, and they do not share the code that decides where a value lives or whether a step is emitted at all. **Run the suite at every level**, or an optimization that trades correctness for speed ships:

```sh
for L in 0 1 2 3; do ./bin/fklua spectest --opt=$L; done
```

Writing the M5 peephole, the suite caught five separate defects that unit tests did not: a dropped `i32.div_u` losing its trap, a `select` arm evaluated only on one branch, a forwarded i64 expression whose high half was overwritten, a loop's `end` mistaken for the function's, and `-0.0` substituted into a unary minus to produce `--0.0` — a Lua comment. **Every one was a hazard about what happens between a value's definition and its use**, and none of them looked like an optimizer bug from the failure message.

`./scripts/run-guest.sh` takes an `OPT` variable, because whether a mod LOADS at a given level is also not something the oracle can answer:

```sh
for L in 0 1 2 3; do OPT=$L ./scripts/run-guest.sh; done
```

---

### Audit the skip list, not just the failures

A pass rate of 100% says nothing about what is being skipped, and skips hide real bugs. Two audits have now each turned up defects that had survived several milestones:

- The global initialiser switch failed the **whole module** on an f32 global, which is why `unreachable.wast` — a file with nothing to do with globals — scored 0 of 63. **A module-level refusal turns every assertion in a file into a skip.** When a whole file sits at zero, suspect the module, not the feature the file is named after.
- Auditing every skip and every *unlisted* testsuite file (43 of 257 were being exercised) found four more: the **start section was ignored entirely** so a guest initialising there came up unset; `trunc_sat` was missing although TinyGo emits it unconditionally; `call_indirect` compared declared rather than canonical type indices, breaking structural type equality; and export names went out with Go's `%q`, whose `\u` escapes Lua cannot parse.

Three of those four were found by *adding files that test things already claimed to work*, not by reasoning about the code. Before declaring a milestone done, run `./bin/fklua spectest -v | grep skip:` and group the reasons.

**`internal/guest` guards the assumption underneath all of it.** Which wasm proposals we must support is not a property of the spec — it is whatever TinyGo and Rust enable by default, and that moves between releases. The package reads TinyGo's target JSON and fails if a guest emits anything outside `guest.Supported`, distinguishing *scheduled* gaps (in `guest.Planned`, e.g. bulk-memory at M10) from a toolchain that has simply started emitting something new. Add a feature to `Supported` when you implement it, or to `Planned` with a milestone — never silence the test.

This is the guard the `trunc_sat` bug should have tripped: "TinyGo emits nontrapping-fptoint unconditionally" sat in prose for three milestones and nothing checked it. **Rust is guarded too now** — `TestRustFeatureSetIsCoveredOrMitigated` asks rustc for its own default feature set and checks every entry against `guest.Supported` or `guest.RustMitigated`, and it found a live problem on its first run. `TestRustIsNotYetGuarded` is gone.

**But a guard that skips is not a guard**, and that half took four more milestones. Every Rust test in `internal/guest` opens with `if ok, why := RustAvailable(); !ok { t.Skipf(...) }`, so a machine without `wasm32-unknown-unknown` skipped all of them and the package still reported `ok` — the feature guard included, which is the one that exists to notice a toolchain changing under us. `TestTheRustToolchainIsAvailable` mirrors `TestTheGuestToolchainIsAvailable`: it hard-fails on a missing Rust toolchain, with the same `-short` opt-out rather than a second flag. It probes by DOING, because `RustAvailable` already learned that lesson — nothing rustc will *print* distinguishes "knows the target" from "has the target's rlibs", so it compiles a two-line `no_std` crate.

The decoder marks individual *functions* unsupported rather than failing the module, so a spec file with one unsupported helper still exercises everything else. Calling such a function raises `fk_unsupported`, which the harness counts as SKIP — never PASS.

---

### Spec files are stateful — do not trust a failure that follows a skip

Once an assertion is legitimately skipped (an i64 store, say), later assertions that depend on the state it would have written say nothing about the compiler. The runner counts those separately as **tainted** and reports them without failing the build. Chasing a tainted failure as if it were a miscompile wastes a lot of time; two of the first four M2 "bugs" were exactly this.

---

### A module that will not instantiate is a skip, not a dead run

The harness supplies the testsuite's own `spectest` host module, whose print functions several files import. Instantiation runs under `pcall`, and when it fails, the file's assertions are recorded as skips naming the reason.

Without that, one unsatisfied import raised out of an unprotected chunk load, killed the driver process and took the whole run's results with it — a corpus addition could turn a 15,675-assertion report into no report at all.

**The audit habit found this one too.** Function imports were refused outright until M4, so `func_ptrs.wast` was scoring 0 while looking like a supported file. Supporting them was worth 5 assertions the suite had never run.

---

## The corpus

`scripts/fetch-spec.sh` pins a testsuite commit and converts with `wast2json`. The output is committed, so CI needs neither network nor WABT. Adding a file is a deliberate act: it raises the bar the pass gate holds us to.

Upstream is now Wasm 3.0, where multi-value and reference types are core, so files depending on them convert only partially or not at all. `testdata/spec/SOURCE` records exactly which, rather than letting the corpus shrink silently.

### A property no value can observe needs the GUEST corpus, not the spec one

An optimization that silently does nothing computes the right answer, so the spec suite, the checksum comparisons and the differential run are all blind to it by construction. The instrument for that class is an assertion over the **emitted text**, taken over every guest this repo ships rather than over a sample — `TestEveryGuardAGuestReadsIsAlsoSeeded` in `internal/luagen` is the worked example, and it found five dead loop guards that every other gate was green on. Two habits make it worth the toolchain builds it costs:

- **Count what you audited and fail on zero.** A corpus test that skipped its builds, or matched nothing, passes forever. That one logs a per-guest count and fails if either toolchain contributed none.
- **Assert per FUNCTION, not per chunk.** Guard flags are function-scoped, so a chunk-wide scan for "is this name ever assigned" can be answered by another function's guard entirely. A guard flag was also `gN` off a step index while a wasm global is `gN` off a global index, so the two families could collide, and the test refuses that too.

### A tripwire over a corpus is not a fix

That last clause is the cautionary half of the worked example. A guard sharing a name with a module global does not merely confuse the scan — it **shadows that global for the whole function**, so a `global.set` writes the flag and a `global.get` reads a boolean. Written as a clause in a corpus audit, it turned a silent miscompile into a test failure for *this repo's* guests, whose step indices happened never to collide, and into nothing at all for anyone else's.

The right instrument for "these two things must never be equal" is a property over the **construction**, not over a sample. `TestNoNameFamilyCanCollideWithAnother` enumerates every identifier family the emitter can emit over a generous range of every index and demands the sets be pairwise disjoint — an assertion about every module that could ever be compiled, which no corpus can be. The guard's names moved to their own namespace and the corpus clause is kept as belt-and-braces, now an impossibility proof rather than a tripwire.

**When a corpus test refuses something, ask whether the thing it refuses can be made unrepresentable instead.** A refusal says a bad state is reachable and you are watching for it; a disjointness proof says it is not reachable.

## Guarding the guest feature surface

`internal/guest` reads TinyGo's target JSON and fails when a guest emits anything outside `guest.Supported`, separating scheduled gaps (`guest.Planned`, e.g. bulk-memory at M10) from a toolchain that has started emitting something new. Add a feature to `Supported` when you implement it, or to `Planned` with a milestone — never silence the test.

**Rust is guarded the same way**, by `TestRustFeatureSetIsCoveredOrMitigated` against `guest.RustMitigated` — and rustc needs the second entry that TinyGo does not, because a feature the CRATE disables can still arrive in a precompiled rlib. `TestTheRustToolchainIsAvailable` is what stops the whole surface from skipping silently on a machine with no `wasm32-unknown-unknown`; see the availability section above.

## The end-to-end test

`TestGoProgramBecomesARunningMod` is M4 stated as an assertion, and it is the only test where **nothing in the middle is written by this project**: TinyGo compiles `guest/go/examples/hello`, the decoder reads the wasm TinyGo actually emitted, the emitter lowers it, the packager writes the mod, and lua52f runs that mod's own `control.lua` against stand-ins for the four Factorio globals it touches.

Every earlier milestone was measured against modules the project wrote for itself. This one is not, which is why it is worth the ~0.5 s a warm TinyGo build costs. It skips when TinyGo or `wasm-opt` is missing — `guest.Available()` reports which, since "could not find wasm-opt" from inside a TinyGo build does not tell you to `brew install binaryen`.

The guest deliberately uses a map, a growing slice, `strconv`, a u64 FNV hash and f64 division, so the assertions check arithmetic rather than merely that the pipeline connects.

**It is still not Factorio.** The mod format, `require` resolution and the log plumbing are outside what lua52f can speak to, so [`./scripts/run-guest.sh`](../scripts/run-guest.sh) runs the real thing. Expect one difference: Factorio's first tick is 0, the harness's is 1, so counts from a real run sit one ahead.

`TestGuestErrorsReachTheHost` is the other half, and it caught a real defect: a guest trap left the guest as a bare table, which Lua reports as "(error object is not a string)" — inside Factorio, a crash with the only useful fact removed. Trap payloads now carry a `__tostring`. **Asserting on what a failure looks like found a bug that asserting on success could not.**

---

## Determinism

Factorio is lockstep multiplayer: every client runs the same Lua and any divergence desyncs the game. Three checks, at three different depths.

| Check | Where | What it would catch |
|---|---|---|
| `TestSaveLoadSaveIsIdentical` | `internal/factorio`, under `bin/lua52f` | A load that PERTURBS the state it loaded — a data segment landing on top of live state, a global reset to its initialiser. That difference is a desync on the first tick after a join. |
| `TestTwoRunsFromTheSameStateAgree` | same | Two clients reaching different states from identical input. |
| `./scripts/run-guest.sh` | **real Factorio**, `--benchmark-runs 2` | The same, inside the actual lockstep simulation. Every distinct guest log line must appear exactly `RUNS` times; a line seen a different number of times means the runs disagreed. |

The in-game one is the only check that runs generated Lua inside the simulation that actually enforces determinism, and it costs one extra benchmark run.

**Only `--persist=table` is checkable this way.** Under `--persist=none` the guest's state lives in Lua upvalues nothing outside the chunk can reach, so comparing `storage` would compare two empty tables and pass whatever the guest did. Both determinism tests assert the state they compare actually MOVED, so they cannot pass by comparing nothing — that guard exists because the first version of the second test did exactly that.

### A harness that always supplies an OPTIONAL engine callback tests one branch and hides the other

Every save/load harness in this repo drove a load as `on_load()` **and then** `on_configuration_changed()`, unconditionally — `persist_test.go`'s `session()`, `bufpin_test.go`'s `boot()`, `buildstamp_test.go`'s `crossPinScript`, three independent authors converging on the same shape because it is the shape that makes the rebuild tests pass. Factorio raises that hook only when the mod SET changes, which for one mod means its **version** moving; a build stamp moves for a dev rebuild, a `--gc` change or a repackage against another `--api` pin. So the harnesses covered the rarer of the two loads and **nothing in this repo had ever executed the commoner one**, through two milestones, until it desynced a real multiplayer game on 2026-08-07 (CLAUDE.md, "a declined heap").

The general shape is worth more than the instance: **an engine callback the harness always calls is a branch the harness cannot see the absence of.** It is the same family as "a skipped gate reads exactly like a pass" and "a gate that cannot fail" — the assertion is green, the code under it is real, and the input that reaches it in the field never occurs in the test. Ask, for every callback a harness synthesises: *is the engine obliged to make this call, or merely allowed to?* If it is allowed to, the harness owes both arms. `cfg` is a parameter of `crossPinScript`'s `session()` now, and `rebuild_test.go` exists entirely to drive the arm where it is false.

### Not covered

`--run-replay` is the check this does not have: it replays a recorded input log and is the real multiplayer determinism regression test. It needs a save with an input log, which means an interactive session. Same gap as the save→load→save byte comparison — see the carried-forward table in `CLAUDE.md`.


## A build cache keyed on nothing is not a cache

`scripts/run-roundtrip.sh` built each guest with `if [ ! -f "$wasm" ]`, so the wasm was reused across runs whatever had changed underneath it. During sharding stage C that cost **four wrong conclusions in a row**: each collector fix was re-run against the binary the FIRST invocation had built, produced a byte-identical failure, and was read as "that was not the cause". The tell was there and was missed — identical numbers across three genuinely different code paths is not a result, it is a stale input.

**AND THE CODE WAS NOT CHANGED FOR A WHOLE STAGE, which is the second half of the lesson.** The paragraph above was written, the fix was a manual `rm testdata/tmp/*.wasm`, and the `if [ ! -f "$wasm" ]` stayed in the script — i.e. a cache whose key is whether somebody remembered reading this file. It was deleted outright at the grow-pacing work, and `scripts/run-growbench.sh` was written without one. A warm TinyGo build is under a second and either script already spends minutes inside Factorio, so there was never a trade.

Two habits follow, and the second is the general one:

- **A cache needs a key.** Content, mtime, a flag — anything. A cache keyed on existence is a cache that can only be wrong in one direction, silently.
- **Identical output from a changed input is a bug in the HARNESS until proven otherwise.** Before concluding that a change had no effect, prove the change reached the thing being measured. In a compiled-guest harness that means proving the guest was rebuilt.

The same shape has now bitten this repo three ways: a spectest job that skipped because a dependency was red, a package that reported `ok` because thirty tests skipped without `bin/lua52f`, and this. All three are an absence reading exactly like a pass.

### `go test` DOES NOT KEY ON `guest/**` — the third occurrence, and the fix

`go test` caches a package's result against the Go inputs it can see: the package's sources, its dependencies, the command line, the environment, and **the files the test binary itself opened**. A guest-dependent test depends on none of those. It shells out — `tinygo build`, `cargo build` — and a subprocess's file opens are invisible to the cache, so editing `guest/rust/fkgc/src/heap.rs` and re-running the same `-run` filter **replays the previous run's verdict, exit status and all**.

It has already cost real time: the Rust collector's first defect was nearly missed twice because each attempted fix re-ran green against the binary built before it (`agents/gc.md`, "the same cache lesson in a new dress"). That is the shell script's stale-wasm bug wearing Go's clothes, and it is the same two habits above — the guest was not rebuilt, and identical output was read as a result.

**The mechanical guard is a KEY, not `-count=1`.** `internal/guest.SourceKey()` opens every file under `guest/go` and `guest/rust` and hashes it; the three guest-dependent packages — `internal/guest`, `internal/luagen`, `cmd/fklua` — each call it from a one-assertion `TestTheGoTestCacheKeysOnTheGuestSources`. Opening the files is the whole mechanism: the cache hashes the content of everything a test opened, which is exactly why a `testdata/` fixture invalidates a cached result, and this puts `guest/**` on the same side of that line. Verified in both directions — a repeated run reports `(cached)`, and appending a comment to either a Rust or a Go guest source makes the next run real.

Three notes on the shape, because each is a way to get it wrong:

- **It has to be called per package.** The cache tracks the opens of each test binary, so a call in one package says nothing about another's. A new package that builds a guest needs its own copy of the test; nothing can notice that for it, which is the honest limit of this guard.
- **The count is asserted and zero fails.** A walk that matched nothing would key the cache on the empty set and pass forever — the same habit the guest corpus audits follow.
- **`guest/rust/target` is skipped.** It is gitignored build output; hashing it would key the cache on the artifacts instead of the sources, which is the exact inversion this exists to prevent.

`-count=1` was the alternative and it is worse twice over: it turns the cache *off* rather than making it correct, so an unrelated edit re-runs minutes of TinyGo and cargo for nothing — and it only helps somebody who went through the entry point carrying the flag, while the collector work that got bitten was running `go test ./internal/guest -run Rust` by hand. **Reach for `-count=1` anyway when you are chasing a stale-input suspicion, and when you are testing this guard itself** — verifying a cache guard through the cache proves nothing.
