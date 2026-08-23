# The host-call ABI

How a guest reaches Factorio's 3283 API members. **Read before touching `runtime/lua/fk_abi.lua` or the binding generator.**

Read `CLAUDE.md` first — Invariant A is the contract every value crossing this boundary obeys.

**Status: reachable end to end, and verified in Factorio 2.0.77.** The handle table, dispatch, all three marshalling tiers, event dispatch, the generator, the Go bindings and the `control.lua` wiring are built and tested.

Not built: the guest-side **C** bindings. At the default 2.0.77 pin the host carries **4257 of 4261** member entries; the 4 it does not are three callback parameters and one variadic — **and three of those four are reachable now, through a seam rather than a binding: see "The callback seam" below.** **2 are deferred on each of the Go and Rust sides**, both a name colliding with another member of the same class, listed under `go_deferrals_by_reason` and `rust_deferrals_by_reason` in the census and printed IN FULL by `fklua gen-bindings` since the campaign found two gaps hiding in the fourteen groups the headline never named — and every one of them is reachable from Lua.

**Every count in this file is PER PIN, and the pin is a build-time axis.** These are 2.0.77's, which is the general-availability release and therefore the default; 2.1.14 is committed too and binds 4,840 of 4,842. Take a number from `api/<version>/census.json` rather than from here, and if you move the default pin, `agents/versioning.md`'s "Moving the default pin" lists every row below that has to move with it.

The count includes **282 `MemberGetEq` entries**, the host-side string predicate described below: a third member KIND, one per plain-string attribute. And **11 class-operator entries** under three more kinds — `M.IDX`, `M.LEN`, `M.SELF` — which are Lua's `obj[k]`, `#obj` and `obj(...)`. See "Class operators" below.

**Every section says which it is in its first line** — a design doc that reads as a description of working code is worse than no doc.

---

## What the API actually looks like

Counted from `api/2.0.77/runtime-api.json`, and pinned by `api/2.0.77/census.json` so these do not quietly stop being true:

| | |
|---|---|
| classes | 148 |
| members | **3283** — 960 methods + 2323 attributes |
| calling convention | 852 positional, 108 `takes_table` |
| events | 219 |
| concepts | 420 — 283 table-shaped, 41 pure string enums |
| defines | 60, of which 4 nest |
| global objects | 9 |
| operators | 11, and **9 are attribute-shaped** (`__index`, `__len`) |
| methods with variant groups | 4 — but **55 CONCEPTS** have them too; see below |
| **subclass-restricted methods** | **184** |

Two of those need saying out loud because a generator that misses them produces bindings that fail only at runtime:

- **`subclasses` is not decoration.** 184 methods are restricted to certain concrete classes. A member listed on an abstract base that carries a `subclasses` list does *not* exist on every child.
- **Classes inherit.** `parent` is set on many of them, and a member reached through a parent appears in neither the child's method list nor its attribute list.

`storage` is **not** a global object and has no handle: it is a plain table the engine serializes, not a LuaObject. The nine that do exist are `commands`, `game`, `helpers`, `prototypes`, `rcon`, `remote`, `rendering`, `script`, `settings`.

### The census is data, not test literals

Every number above lives in `api/<version>/census.json`, written by `fklua gen-bindings` alongside the bindings and checked by `--check`. An API upgrade is then a data diff and one command:

```sh
fklua gen-bindings     # rewrites bindings AND census
git diff               # exactly what moved, one line per number
```

**This was measured, not assumed.** Running the pipeline against 2.1.12 — an API version it had never seen, 482 members larger — the generators handled it with **no code change at all**, and then **seven tests failed**, every one of them a Go literal that had moved rather than a logic error. Counts pinned in test source turn a mechanical regeneration into a source edit across three files, which is precisely the manual step automatic regeneration exists to remove. With the counts in committed data the same experiment fails **two** tests, and both say `run fklua gen-bindings`.

What that leaves is a diff a reviewer reads top-down, raw counts first and what the generators made of them second:

```
application_version              2.0.77 -> 2.1.12
classes                          148 -> 156
members                          3283 -> 3765
...
operators                        11 -> 9
host members skipped             72 -> 96
skip: nil                        NEW, 1 members
skip: variant parameter groups (hand-written) 68 -> 91
```

Three deliberate choices in that output:

- **Pins are equalities, not floors.** A shrinking API is news too — 2.1.12 *removed* two operators, which `!=` catches and `>= 9` would wave through.
- **Skip reasons are diffed by name**, and a reason appearing for the first time is called out as `NEW`. That is the actionable half: it means the generator met a shape it had never met, which is worth reading even when the totals barely move. `skip: nil` above is exactly that — a return type 2.0.77 never produced.
- **The census is written by `gen-bindings`, not its own command.** They ride together on purpose; splitting them is how one of them gets forgotten.

Tests that need a *specific* member rather than a count now skip when this API lacks it, so a version bump cannot fail on a member Wube renamed.

### `gen-bindings` writes into the WORKING DIRECTORY, and reads the manifest

Two things this command got wrong until 2026-08-01, both found by the first downstream mod rather than by anything here:

- **One command, one root.** The bindings resolved against the working directory and the census resolved against the **executable**, so `fklua gen-bindings` run inside a mod project rewrote `api/<version>/census.json` in whichever FkLua checkout built the binary. Building a mod must never write into the compiler. The census is now written only where its input lives — beside a `api/<version>/runtime-api.json` in *this* directory — and otherwise the command says why it is not writing one. `--check` still reads wherever the description came from, because reading is not the problem. *Enforced by `TestGenBindingsDoesNotWriteIntoTheCompilersCheckout`, which asserts on the mtime rather than the bytes: identical content only means the census happened to be current, which is exactly what made this invisible.*
- **`fklua.toml` is the default and the flag is the override**, for `lang` *and* for `api`. `fklua lock` has always read both; this command read neither, so `fklua init --lang go` printed "Next: `fklua gen-bindings`" and following that advice dropped an unwanted `guest/rust/` into a Go-only project — which `lock` then refused to hash. Two commands disagreeing about one key is worse than either behaviour on its own. *Enforced by `TestGenBindingsHonoursTheProjectsLangList` and `TestAnExplicitLangFlagOverridesTheManifest`.*
- **…and `--into DIR` writes into a DIFFERENT root on purpose**, which is the one case the first bullet's rule does not serve. A consumer that vendors a FkLua checkout has to regenerate *that checkout's* committed `fkapi` at its own pin, because the library packages inside its guest module — `fkipc` above all — import that copy rather than the consumer's. `--into` does it for every language the manifest declares, writes neither the static Rust crate scaffolding nor a census into the target, and refuses to be combined with `-o`. It is what the packager's pin-stamp refusal tells the reader to run; the whole account is in [`agents/versioning.md`](agents/versioning.md), "The pin stamp, and repinning a vendored checkout". *Enforced by `TestGenBindingsIntoRepinsAVendoredCheckout` and `TestGenBindingsRefusesBothOutputFlags`.*

**And the generated bindings now carry a PIN STAMP** — one exported function named `fk_api_pin_<version>`, emitted by both generators from `factorio.PinExport` — which is how `fklua mod` proves the packaged table and the guest's ids came from one description. It is deliberately **not** in `factorio.Hooks`: `control.lua` never calls it, and the both-directions guard above is about what the runtime wires, not about every export a module has. *Enforced by `TestBothGeneratorsStampTheSameName`.*

---

## The handle table — **built**

`runtime/lua/fk_abi.lua`, `require`d by a packaged mod's control.lua.

wasm has four numeric types and nothing else, so a `LuaEntity` cannot cross into guest memory. The guest holds an **i32 index** into a table the host keeps.

### Two spaces, split at `0x40000000`

| range | space | lifetime |
|---|---|---|
| `1..9` | globals | fixed at load, never allocated or freed |
| `10 .. 0x3FFFFFFF` | **persistent** | until `fk_release`; lives in `storage`, survives a save |
| `>= 0x40000000` | **transient** | released wholesale when the dispatch returns |

**The transient space exists to kill a leak class, not to be fast.** The dominant shape in real mod code is: a handler reads `event.entity`, uses it, and returns. Under one space every one of those pins an entry forever and the author has no way to notice. Here the whole space is discarded at the end of the dispatch that created it, so the default leaks nothing and `fk_retain` is the only thing an author must remember — for state they meant to keep.

Splitting on a bit rather than a per-entry flag makes "is this persistent" a single compare, and lets the guest hold an opaque number that still tells the host which table to look in.

### Rules that are load-bearing

- **`get` checks `.valid`.** Factorio invalidates a LuaObject when the thing behind it is destroyed, and touching an invalid one raises a Lua error — which from inside a guest call would unwind through wasm frames that *cannot* be unwound, because there are no coroutines. It comes back as `ERR_INVALID`.
- **`retain` is idempotent** for a handle that is already persistent or global, so a guest never has to ask which space it is in.
- **`release` of a transient handle is a no-op, not an error**, for the same reason.
- **A global cannot be released.** `ERR_BAD_HANDLE`.
- **`adopt` rebuilds the free list rather than restoring it.** A stale free list read back from a save would hand out a slot that is still in use — two guest handles aliasing one object, which is corruption, not a leak.
- **The persistent table is the live one.** A retain during play lands straight in what Factorio serializes, the same aliasing `--persist=table` uses for guest memory, and the reason there is no sync step.

### What a retained handle means after a load

**The persistent space really is in `storage` as of 2026-08-03, and until then the paragraph above was a promise with nothing behind it.** `M.persistent_table()` and `M.adopt(saved)` both existed and the shipped `fk_mod.lua` called **neither**, so a retained handle was valid for the SESSION and `ERR_BAD_HANDLE` on the next load. The only caller of either in the whole tree was a test. Found by the first mod that ever called `fk_retain` (fklua-ports' inventory-sensor, F1) — nothing in this repo had, because every guest here re-reads the world instead.

The fix is two lines in `fk_mod.lua`: `state_init` publishes `storage.fk_handles = H.persistent_table()`, and `state_load` adopts it. Both sit beside the guest heap's own publish and adopt, and the placement is the whole semantics:

| | |
|---|---|
| **published unconditionally**, not inside the packed/table branch | what differs between those modes is how MEMORY is mirrored, and this is not memory. It is the LIVE table, aliased into `storage`, so a retain during play lands in what Factorio serializes with no sync step |
| **adopted under the same `same_build() or fk_migrate_adopt` gate as the heap** | a handle is a NUMBER the guest wrote into its own memory. Adopting the table without the heap that remembers those numbers pins the previous build's LuaObjects in `storage` forever, reachable by nobody and freeable by nobody, because freeing needs the number |
| **the discard path needs no code** | the rebuilt-guest path calls `state_init`, which publishes the live table this session started with — which is empty. It used to say `on_configuration_changed` calls it, and that was the whole of the 2026-08-07 defect: Factorio raises that hook for a mod-set change and a rebuild that keeps the version is not one, so the discard path was **never reached** on a dev rebuild and the previous build's handle table sat in `storage` for the rest of the save's life. `finish_rebuild` is what calls `state_init` now, from the hook when it fires and from the first outermost dispatch when it does not — see `fk_mod.lua`, and CLAUDE.md's "a declined heap" |
| **`--persist=none` stores none of it** | that memory is rebuilt from the data segments on every load, so a carried handle table would be entries no guest could ever name again |

**What a retained handle MEANS after a load is a separate question from whether it resolves, and both answers are useful.** Factorio serializes the reference, so the handle **resolves**; if the thing behind it was destroyed meanwhile, the object comes back with `valid == false` and the call is **`ERR_INVALID`**. That is exactly the distinction `ERR_BAD_HANDLE` exists to make, and a guest was being denied it: a dead handle reads as "I never had this", an invalid one as "the entity is gone", and before the fix everything read as the first.

**Handle numbering across a load is deterministic**, which it has to be in a lockstep game. `adopt` rebuilds the free list rather than restoring one — a stale free list read back from a save would hand out a slot that is still in use, which is corruption rather than a leak — and it rebuilds it ASCENDING from the saved table alone, so every client derives the same list from the same bytes. The order does change across the boundary: `free` is a LIFO stack during play and an ascending list after a load. That is a property worth knowing and not a defect — what matters is that every client agrees, not that a session agrees with its own past.

Verified in two places, because the defect was in neither of the places a test was looking. `internal/factorio/retain_test.go` replays the real control.lua protocol through a stand-in `storage` — fresh Lua state, fresh `require`, only `storage` crossing — in **both persist modes**, plus the rebuilt-guest discard and the on_load-writes-nothing property. `scripts/run-roundtrip.sh`'s **`retain` leg** is the one that says the engine agrees: two handles retained before a real save still answer `surface.name_is("nauvis")` after it, and the slot a RELEASED handle freed is the one the next retain gets.

### Handle order is a compatibility surface

`M.GLOBAL_NAMES` fixes 1..9. A guest compiled against it uses the numbers, so **appending is safe and reordering is not**. `TestGlobalHandleOrderIsFixed` pins it.

### Status codes

A host call returns an i32; it **never raises into wasm**. The guest turns a non-zero status into whatever its language calls an error — a Go `error`, a Rust `Result`.

| | |
|---|---|
| `OK` 0 | |
| `ERR_BAD_HANDLE` 1 | not a live handle |
| `ERR_INVALID` 2 | the LuaObject's `.valid` went false |
| `ERR_NO_MEMBER` 3 | resolved to nothing on this Factorio version — see below |
| `ERR_BAD_ARGS` 4 | |
| `ERR_CALL_FAILED` 5 | the API itself raised |
| `ERR_NO_SPACE` 6 | |

**An ABSENT OPTIONAL is not `ERR_NO_MEMBER`, and it was until 2026-08-03.** `runtime-api.json` declares **666 readable attributes** `optional: true` — `LuaEntity.temperature` is present on a reactor and absent on a chest. The generator honoured `optional` on METHOD returns and dropped it on attributes, so all 666 were typed as always-present on both sides and an absent one came back as `ERR_NO_MEMBER`, *"no such member on this Factorio version"*. That is the same status a member genuinely REMOVED in a point release produces, so a guest could not tell "this chest has no temperature" from "this Factorio has no such attribute" — which is the one distinction the status exists to make. (fklua-ports' inventory-sensor, F2.)

**Two things were missing and either alone leaves the defect.** The member table had no `has=` for the return, so `encode_rets` had no presence byte to write; and `M.invoke` turns a nil value into `ERR_NO_MEMBER` *before* `encode_rets` ever runs, so the layout on its own changes nothing. The member entry carries **`opt=true`** now — the generator's statement that nil is a legal value here — and that is what keeps the distinction rather than erasing it: **nil still means `ERR_NO_MEMBER` everywhere the description did not say optional**, which is every method and every attribute that is not one of the 666.

Three consequences worth knowing:

- **the bindings change type.** `func (o LuaEntity) Temperature() (*float64, error)` and `pub fn temperature(&self) -> Result<Option<f64>, Status>`. The return block grows by a presence byte and its padding;
- **a SET is unaffected.** Assigning a field that reads as nil is how you create it, which is why `M.invoke` always exempted SET, and `opt` is not emitted for one. An optional WRITE — the guest clearing an attribute by sending nothing — is a separate change and not this one: it needs an absent argument to mean "assign nil" rather than "leave the argument out", and `M.call`'s trailing- argument trim already means the second thing;
- **the 30 optional STRING attributes keep their `EQ` predicate.** The guard that offered it read `&& !f.Optional` and was DEAD, because `f.Optional` was never set on that path — so honouring it would have deleted thirty working members as a side effect of fixing something else. `call_eq`'s `type(f) == "string"` already answers false for an absent attribute, which is the honest answer; a caller who needs absent-versus-different asks the GET, which now says so.

Nothing else moved when this landed: **4160 Go and 4140 Rust members bound of 4187**, because optionality is a type and not a member. (Those were the counts of the day; the current ones are in `census.json`.)

---

## Dispatch — **built**

`M.invoke(handle, mid, ...) -> status, ...` is the value layer, and `M.call` adds `fk.call`'s argp/retp encoding on top of it. They are separate so the codec is testable without a wasm module and dispatch is testable without a codec.

One generic import rather than 3283 of them:

```
(import "fk" "call" (func (param i32 i32 i32 i32) (result i32)))
;;                         handle member argp retp -> status
```

`MEMBERS[mid]` is a **generated, statically-specialised closure** — not a reflective interpreter — so this is one array index away from the cost of a per-member import while keeping a generator's code quality.

**Why not 3283 separate imports.** With per-member imports, a single method removed in a Factorio point release becomes an unresolved import and *the whole module fails to instantiate*. Generic dispatch plus load-time name→id resolution degrades to `ERR_NO_MEMBER` on the one call instead. Given that `latest` is already ahead of a typical install, that is the difference between "one call fails with a diagnostic" and "your mod breaks for everyone, at load, silently."

### A member entry is smaller than it looks

Reading `obj[name]` and calling `obj[name](...)` are generic over every class, so dispatch needs **no per-class code**. An entry is a kind (`CALL`, `GET`, `SET`) and a name. Specialisation belongs to marshalling, which is the part that knows argument types.

Member ids are **per-build and dense**, assigned by the generator from the manifest of members that guest actually references. Adding a member to Factorio therefore cannot renumber anything.

### Where the plan was wrong: resolution cannot happen at load

The plan says an unresolvable name is *logged at load*. It cannot be. Lua has no way to ask whether a class has a member without an instance of it, and a packaged mod does not ship the API description it was built against — so there is nothing to check a name against until a call supplies an object.

What happens instead: a missing member is discovered on **first call**, reported **once**, and returns `ERR_NO_MEMBER` every time after. The deduplication is not cosmetic — a guest calling a removed member from `on_tick` would otherwise write sixty log lines a second and bury the one that mattered.

### The member read is itself inside the `pcall`

`obj[name]` is not a plain table lookup: a LuaObject has an `__index` metamethod, and Factorio raises from it for some accesses. Reading it outside the guard lets that error unwind straight through the wasm frame the call came from, which is the one thing this layer exists to prevent. A test with a raising `__index` found this the first time it ran.

Four result slots, against a **measured maximum of three** return values across all 960 methods (`TestMethodReturnArity`), so nothing truncates. Named slots rather than `table.pack`, because packing allocates on every host call inside a lockstep game loop.

### A method is a BOUND closure, and the arity is exact

**A Factorio `LuaObject` is not a table with functions in it.** Its `__index` returns a closure *already bound to the object* — which is why every line of real mod code reads `surface.create_entity{...}`, dot-called and never colon-called — and the engine's argument checker counts what arrives exactly. Two rules follow, and until 2026-08-01 dispatch broke both:

- **`pcall(f, ...)`, never `pcall(f, obj, ...)`.** Passing the object a second time is one argument too many on *every* method in the API.
- **`M.call` forwards exactly `#m.sig.args` slots**, by a four-way dispatch chain rather than by handing `invoke` all four decoded values. Lua counts trailing nils, so four slots into a one-argument method is `Expected 1 argument but 4 were given`.

Together they made `game.get_surface("nauvis")` arrive as **five** arguments and come back `ERR_CALL_FAILED` — i.e. *every method call in the API was malformed*, for as long as the ABI has existed. Attribute reads (`GET`/`SET`) return before either line, which is why the first downstream consumer only found it on reaching its first method call.

**Why the suite was blind, which is the durable half.** Every other dispatch test drives `M.invoke` directly with the right number of arguments, so the padding in `M.call` was never on the path; and every stand-in object declared its methods as `function(self, x)` left in a plain table — the exact shape that makes a spurious `obj` look correct. *A stub for this layer must be built the way the engine builds one*: methods handed back from `__index` as closures over the object, with a strict argument count. *Enforced by `TestAMethodIsCalledTheWayFactorioBindsIt`*, which reports the engine's own `Arguments count error` text at arity 0, 1 and 2 when either line is reverted.

### …and the arity is the last argument PRESENT, not the declared count

The two rules above and the absent-optional rule below are each correct alone and were together wrong — the kind of defect a suite only meets when something real calls it. `M.call` forwarded `#m.sig.args`; `decode_args` started honouring presence bytes, so an absent optional became a real `nil`. A member with a trailing optional therefore reached the engine as N arguments whose last was an explicit nil, and the engine counts an argument that was *given* and then type-checks it:

```
bad argument #2 of 3 to '?' (table expected, got nil)
```

`game.create_surface(name)` is exactly that shape, and it is the call the first downstream mod's whole architecture rests on. Worse than one member: **a member whose trailing optional is a number or a boolean could not be called at all** — the binding omits the parameter, absent decodes to nil, and nil is rejected. That is a large fraction of the API, and it shipped for a day.

`M.call` now walks back from the declared count while the last argument's presence byte is clear. The declared count stays the **upper bound**; a hole in the **middle** still crosses as nil, because Lua has no other way to say it, so the trim stops at the last present argument rather than removing every absent one.

**Why nothing here caught it, which is the durable half.** The arity tests use members whose arguments are all mandatory, so the trailing case never arose; and `TestAnAbsentOptionalArgumentCrossesAsNil` asserted that the nil *arrived*, against a stub that only counted — the wrong assertion, made confidently, under a comment arguing that a trailing nil was inside the engine's accepted range. A stub for this layer has to type-check as well as count. *Enforced by `TestATrailingAbsentOptionalIsNotForwardedAtAll`, which covers present, absent, and middle-absent-with-a-trailing-present.*

`--specialize-imports=<list>` is the opt-in escape hatch for hot members, and is not built.

---

## Marshalling — **built, tiers 1 through 3**

Every scalar kind, strings, LuaObjects as handles, nested structs with optional fields, arrays, dictionaries, and tier 2's tagged dynamic value — all composing arbitrarily.

### Tier 2: one codec, not 93 union types

A structural union has no fixed layout, and `LocalisedString` is defined in terms of itself. Neither survives a struct. A **tag saying what is actually there** carries both, and tolerates version skew for free: it describes the value rather than what the schema said the value would be.

16 bytes — tag at 0, payload at 8: an f64, a `(ptr, len)` string, a handle, or a `(ptr, count)` over more of these. **This took host coverage from 89.5% to 98.2%**, and what remains is the four methods the plan always said to hand-write, three callbacks and one variadic — no type-system gap at all.

Telling a LuaObject from a plain table cannot be done by reading a key: a key a LuaObject lacks **raises**, the same trap that broke the `valid` probe. The `object_name` probe is therefore guarded by a `pcall` — affordable in tier 2, which is the general slower road by construction, and not on the tier-1 path.

### The three global functions, and the hand-binding escape hatch

Two round-B2 adjudications, both of which came out "already answered" once the generator work of B1a/B1b had landed. Recorded so neither is re-opened on the strength of a stale ledger entry.

**`table_size`, `log` and `localised_print` stay unbound, and `table_size`'s case specifically has been retired by the slice-return world.** `fk.call` takes a handle and a member id; a global function is on no class, so binding one needs either a reserved pseudo-handle or a new import — cheap either way. The question was whether `table_size` is worth it, and the argument for it was **sizing a dictionary without materialising it**. That argument is gone twice over. A dictionary RETURN already crosses as a `(ptr, count)` pair slice, so the guest is handed the count with the data and there is nothing left to ask; and a `LuaCustomTable` now has a handle route, whose `Length()` operator answers the same question with no materialisation at all — which is strictly better than `table_size`, since it does not need the table to have crossed in the first place. `log` is `fk.Log`, which every guest already has. `localised_print` is a real gap and a small one: a guest that wants it calls `LuaGameScript::print(LocalisedString)`, which IS bound. **Left unbound; revisit only with a use that survives the two answers above.**

**G2 — "Go's `Object` handle is unexported, so there is no escape hatch at all" — is CLOSED, and hand-bindings are supported.** `ObjectAt(h uint32) Object` and `(o Object) Handle() uint32` are both exported and are inverses; the Rust `Object` is a public tuple struct. So a guest that needs to reach a member the generator did not emit can construct a handle and call `hostCall` — and the generated package is one file in the guest's own module, so nothing stops it. **What has actually changed is that the residual need is gone**: the report's own evidence was the class operators (RM1, Q2, F-IDX — one gap filed three times), and those are bound. Across the six ports, the members left that a hand binding would reach are the three callback/variadic skips, and those now have a seam. **The position is: the hatch exists, it is documented, and a mod that finds itself using it has found a generator gap worth reporting rather than working around.** `Handle()`'s own comment says a guest almost never wants it and names the one thing it IS for — logging which persistent slot a retain landed in — and that remains the honest recommendation.

### What a host call costs, and the walls a headless run hits

Two things six ports agreed on, kept because they are what a person planning a mod actually needs.

**A host call is ~12.5 µs, and that is the design constraint.** Measured by fklua-ports' resource-marker over **2,487 real calls of a mixed workload** (`RM3`), cross-confirming BetterBeltBalancer's independently measured 12.6 µs across a completely different mix. So the cost model a guest should plan with is **calls, not bytes**: a 4×4 balancer recompile is ~350 calls and 4.4 ms, and the thing that makes it 4.4 ms is the 350, not the network. There is no bulk form for per-entity attribute reads — an `n`-entity scan reading three attributes each is `3n` calls — so the shape that wins is one `find_entities_filtered` returning a slice and as few follow-up reads as the logic can be rewritten to need. The `<Name>Into` variants exist for exactly this reason on the array side, and the `<Name>Raw` handle route above exists for it on the dictionary side.

**Some things cannot be verified headlessly at all, and the list is worth knowing before designing a test around one.** `--create` has no player, so anything resolving `game.get_player` is out; `script.raise_event` refuses a documented subset outright (`on_player_mined_entity`, `on_undo_applied` and friends answer *"can't be raised through script"*); and **charting is unreachable** — `chart`, `chart_all`, `rechart`, radar coverage, a character and a server all leave `is_chunk_charted` false, and `on_chunk_charted` is not raiseable either (fklua-ports' RM4). A path behind one of these walls is not untested by choice, and the discipline that pays is the one BetterBeltBalancer arrived at the hard way: **ask which HALF of it is actually behind the wall.** Twice, a feature declared unverifiable turned out to have a trigger that needed a player and arithmetic that did not — and the arithmetic is where the bug was both times.

### A LuaCustomTable has a handle route, and until B2 nothing returned one

**B1b bound `LuaCustomTable`'s `index` and `length` operators and recorded one thing it had not closed: `force.technologies` had no handle to call them on. The gap was universal rather than particular** — a grep across all 4,191 generated members finds that **nothing in the API returns a `LuaCustomTable at all**, so `Get` and `Length` were bound-and-unreachable from everywhere.

The cause is one line. The description models the type STRUCTURALLY — `{"complex_type": "LuaCustomTable", "key": …, "value": …}` — and `mapType` aliases it onto `dictionary`, so it never reaches the `m.classes[name]` branch that would have made it a handle. **59 attributes across 7 classes** carry one: all 46 of `LuaPrototypes`, five of `LuaGameScript` (`surfaces`, `players`, `forces`, `planets`, `backer_names`), both of `LuaForce`, and one each on `LuaPlayer`, `LuaRailPath`, `LuaStyle` and `LuaSettings`. Every one generated as a materialising dictionary read.

What that costs, measured by fklua-ports rather than estimated: **one `force.technologies` read is 14,544 bytes of guest heap**, for a guest that wanted one entry of 319 (its Q2).

**A second member over the same attribute, not a change to the first.** The materialising read is the right answer for iterating the whole table and is what every existing guest calls; the handle is the right answer for a point lookup; which one a guest wants is a question about the guest. The precedent is the `<Name>Into` variant one level up, with **one difference that decides the implementation**: `Into` shares its member id because the host does identical work and only the guest's use of the returned `(ptr, count)` differs. This does not — the host writes a HANDLE where it wrote a `(ptr, count)` — so it is a real member with its own id, and `MemberGetHandle` (kind 7) is how the ABI is told which. It needs no new branch in `M.invoke`: like `MemberGet` it resolves `obj[m.name]`, and everything that differs is in the declared return kind, which `write_value` has always dispatched on. So `M.GET` and `M.GETH` share a line.

    force.TechnologiesRaw()   -> Object          one host call
    LuaCustomTable{o}.Get(k)  -> Value           one more
                                                 vs 14,544 B and 319 entries

**The gate is on the DESCRIBED type, not on the field kind**, and that is load-bearing: a plain `dictionary` maps to `KindDict` too and is a Lua table with no handle behind it, so gating on `f.Kind == KindDict` would grow a handle accessor on every dictionary attribute in the API pointing at nothing. `isCustomTable` asks the description again for exactly that reason, and `TestALuaCustomTableAttributeHasAHandleRoute` asserts the negative.

**And the doc comment next to it was a measured trap.** The pair-slice's advice was *"build a map from it if you want lookup"*. fklua-ports measured that: the map adds 12,512 B on top of the read's 14,544, i.e. **27,056 B — worse than the 24,576 B Go map the ordered slice was introduced to replace.** The comment now says not to, and names the two right answers: a zero-allocation linear scan for a scan, and `<Name>Raw` + `Get(key)` for a point lookup. The union-key half of the same comment (B1b's RM2 — `pairs()` yields the NAME, so filtering on `TagNumber` matches nothing, silently) is a different statement and survives intact; `TestThePairSliceDoesNotAdviseBuildingAMap` pins both halves.

Members bound: **4,191 → 4,250**.

### Subclass restrictions — counted honestly, and deliberately NOT suppressed

**854 members of the API are declared on a class but restricted to certain subclasses, and the generator emits every one of them everywhere.** A guest can call `entity.add_autopilot_destination()` on a chest and get `ERR_NO_MEMBER` at runtime. The obvious fix is to honour the restriction in emission. **It is the wrong fix, and the measurement says so rather than an argument.**

**None of the 202 distinct subclass tokens is a class name.** The overlap with the 148-class graph is **0 of 202**. They are runtime TYPE discriminants — `Character`, `Inserter`, `SpiderVehicle`, `textfield`, `BlueprintItem` — the thing `entity.type` returns, not a node anything inherits from. Correspondingly **15 of the 16 declaring classes have no descendants at all**: `LuaEntity` is one class covering every entity type, and there is no `LuaInserter` for a generator to withhold a member from.

The one exception proves it. `LuaItemCommon` has two children, `LuaItem` and `LuaItemStack`, and 58 restricted members that become 85 generated functions forwarded onto each — **170 of the 1,329 forwarders, 12.8%**. Suppressing those would be **wrong**: the tokens are `BlueprintItem`, `DeconstructionItem`, `UpgradeItem`, which are item KINDS, and both children can hold a blueprint. There is no correct answer to "is `LuaItemStack` in `["BlueprintItem"]`?" short of a hand-written token-to-class table that would have to say "yes, sometimes". And it would address **6.8%** of the restricted members either way; the other 796 are emitted directly on leaf classes, where no forwarder rule reaches them.

**So the answer is to count it and say it.** The census was doing neither honestly: `subclasses` is declared on both `api.Method` and `api.Attribute`, and only the method loop asked — **184 counted against 854 present, a 78% undercount** of the one number this file exists to keep honest, with attributes the larger half four to one (`LuaGuiElement` alone declares 70). `subclass_restricted_attributes` is a census field now and the diff reports it.

| declaring class | restricted | descendants |
|---|--:|--:|
| LuaEntityPrototype | 281 | 0 |
| LuaEntity | 237 | 0 |
| LuaGuiElement | 70 | 0 |
| **LuaItemCommon** | **58** | **2** |
| LuaItemPrototype | 53 | 0 |
| LuaRecord | 43 | 0 |
| LuaRenderObject | 41 | 0 |
| LuaStyle | 36 | 0 |
| ...nine more | 35 | 0 |

**Revisit only with a token-to-class table in hand**, and note that building one is a statement about Factorio's type system that this repo would then own and have to re-derive at every pin — against a payoff of 170 forwarders on two classes. `ERR_NO_MEMBER` on a member the docs say is Character-only is the honest failure.

### The callback seam — commands and remote interfaces

**Three of the four members the generator skips are the same problem twice, and the answer is not a binding.** `LuaCommandProcessor::add_command` and `LuaRemote::add_interface` take a Lua FUNCTION; `LuaRemote::call` is the API's one variadic method. Between them they put an entire genre of mod out of reach — anything with a console command, and anything that talks to another mod in either direction — and fklua-ports reported it four times over (AD7, G6, FTS4), from three different ports, in both languages.

`fk_abi.lua`'s `write_dyn` is blunt about the first half and it is right: *"a function crossing into a guest has nowhere to live"*. A wasm guest has no callable Lua value, no way to make one, and no way to be one.

**So the function does not cross.** The host synthesises a Lua closure, gives THAT to Factorio, and dispatches back into the guest by an id the guest chose — which is exactly what `fk.subscribe` has always done for events. The closure it installs is a trampoline in every sense, so this is one mechanism generalised rather than a second one invented, and it is deliberately built out of the same four parts:

| | events | callbacks |
|---|---|---|
| registration | `fk.subscribe(id, filterp, mask)` | `fk.register(kind, descp)` |
| what crosses at registration | a tier-2 FILTER, read once | a tier-2 DESCRIPTOR, read once |
| the closure | installed by `subscribe`, captures the event id | installed by `register_callback`, captures the callback id |
| the entry | `fk_on_event(id, ptr)` | `fk_on_call(id, argp, retp)` |
| the buffer | `event_buffer(level)`, `fk_alloc_static` | `call_buffer(level)`, `fk_alloc_static` |

Two imports and one export, and the wire is:

    fk.register(kind, descp) -> status
        kind 1  a command:   {name = "...", help = <LocalisedString>, id = <u32>}
        kind 2  an interface: {name = "...", methods = {["m"] = <u32>, ...}}

    fk.remote_call(callp, retp) -> status
        callp -> ["interface", "method", [args...]]
        retp  -> one tier-2 slot for the result

    fk_on_call(id, argp, retp) -> status
        argp -> one tier-2 ARRAY of the arguments as they arrived
        retp -> one tier-2 slot; a command's trampoline ignores what is in it

#### The four decisions, and why each is the one it is

1. **The GUEST declares these, and `fklua.toml` does not.** The manifest was the obvious home — it already carries identity, dependencies and the data stage, and a static list would let the host register without asking the guest anything. It is the wrong home for two reasons and the second settles it. First, the names would then live in two places, the manifest and the guest's own id switch, which is the "configs written twice" complaint fklua-ports already recorded against the data stage (its Q5). Second: **a command registration is not saved.** Factorio re-executes `control.lua` on every load, so registration must happen on every load — and a guest that registers from its own `_initialize` gets that by construction, with no `storage` flag, no `on_load` re-arm, and no way for the two to disagree. `fk.defer` needs `storage.fk_deferred` precisely because it is arming something that must SURVIVE a save; this is the opposite case, and treating it the same way would have been the bug.

2. **One export, not two.** A command handler and a remote method differ in what Factorio does with the result and in nothing else the ABI can see. So they share `fk_on_call`, and the guest's `switch id` does the rest — the same shape `fk_on_event` already asks for.

3. **The arguments are TIER 2 rather than a generated struct.** A command handler is handed `CustomCommandData`, which IS a described concept and could have had an event-payload-style struct. A remote method is handed whatever the calling mod passed, which cannot: there is no description of it anywhere, because the other end is another mod. One shape that serves both is worth more than a typed shape that serves one.

4. **`remote.call`'s arguments are packed rather than given an arity ceiling.** The bounded-arity alternative — `call4`, `call8` — puts an arbitrary number in the ABI forever. `unpack` on the host side does not, and `remote.call` is not a hot path: `M.call` avoids `unpack` because a 4×4 balancer recompile is ~350 of them in one tick, and this happens at the rate another mod's script decides to ask us something.

#### Two things this pass found that were not the feature

**`write_dyn`'s container payloads came from the ALLOCATOR, and had to come from the scratch region.** A tier-2 value written as a RETURN is bracketed by the guest binding that made the call — mark before, release after — so `fk_alloc` there is call-scoped. A tier-2 value written as an ARGUMENT to a host-initiated dispatch has no such bracket: nothing on the guest side made the call, so nothing on the guest side releases it, and **every command invocation would have leaked its own arguments for the life of the mod.** `dyn_alloc` applies the policy `write_field`'s `K_STR` path has always had — region first, allocator second — which is also strictly cheaper on the path that already existed (one `fk_alloc` is 1,535 ns against 53 for the Lua-side alternative).

**And `dispatch` resets that region at depth 0, AFTER a trampoline has written into it.** `dispatch` encodes an event payload *after* raising the depth, so the reset is harmless there. A trampoline cannot work that way — it has to encode the arguments before it knows which export to call them with — so the first cut wrote the arguments and then had `dispatch` zero the region underneath them. The symptom was not an error and not even a wrong first read: the outer invocation's arguments were correct until the first NESTED callback, and then it was reading somebody else's. That is the shape `TestANestedDispatchLeavesTheOuterOneIntact` was written for, met from a new direction. `invoke_callback` does its own depth bookkeeping now: reset once at an outermost invocation, raise the depth across the encode, and take a `scratch_mark`/`scratch_release` pair so a callback invoked from inside another takes its arguments from above whatever is still being read.

#### What is verified

`TestACommandAndARemoteInterfaceReachTheGuest` drives a real TinyGo guest through the **verbatim** `fk_mod.lua` and `fk_abi.lua`, against `commands` and `remote` stubs shaped like the engine's. It is end to end on purpose: every part of this path is a seam between two separately-correct things, and a unit test of any one of them passes with the chain broken. What it asserts:

| | |
|---|---|
| a command registered from `init()` reaches the guest, with its `CustomCommandData` decoded | `cmd fk-echo param=hello world tick=77` |
| remote methods, in and back out with a result | `add 42`, `greet hello, world` |
| **arity is preserved through a hole** — `f(1, nil, 3)` is three arguments | `arity 3`, and `arity0 0` for none |
| a method that writes nothing reads back nil, TWICE, so the second is not the first's leftovers | `noret nil`, `noret2 nil` |
| `remote.call` made BY the guest, **from inside its own command dispatch** — the re-entrant case | `outbound 9` |
| a missing interface is a STATUS, not a trap | `missing 5` (`ERR_CALL_FAILED`) |
| the outer invocation's arguments survive both nested calls encoding their own into the same region | `still hello world` |
| a guest with no `fk_on_call` export registers nothing | `TestRegisteringWithoutTheExportIsRefused` |

`guest/go/examples/callback` is the guest, and `H.write_varargs`'s header is why `{...}` is not good enough: `#` stops at the first nil, so a caller passing a hole would have had its later arguments vanish — silently, and only for that caller.

**What is still not reachable**: `LuaBootstrap::get_event_handler`, which RETURNS a function. There is nothing for a guest to do with one, so it stays a skip.

### The wire is a C struct

`argp` and `retp` point at blocks in the **guest's** memory, laid out as a C struct would be: each field aligned to its own alignment, in declaration order, padded to the widest. `internal/factorio/layout.go` places them **once, at generate time** — nothing recalculates an offset per call — so the generator can emit a matching `#[repr(C)]` or Go struct and the two ends agree by construction.

Two placements shift everything after them if wrong, so both are explicit: a **string is `(ptr, len)`** and a **u64 is `(lo, hi)`**. Each is 8 bytes wide but aligns to **4**, not 8.

The kind numbers, widths and alignments are a contract between Go and Lua that **no compiler checks**. `TestKindNumbersMatchTheLuaABI` and `TestGoAndLuaAgreeOnWidthAndAlignment` check it instead, by reading the constants out of `fk_abi.lua`. Both were verified to fail when one side moves.

### Invariant A reaches this layer

An i32 crosses as an **unsigned** double, so every signed width needs an explicit fold at the boundary. Getting it wrong is a wrong *number* rather than a crash — the kind of bug that ships — so the fold lives in one place per width.

### Strings out need the guest's allocator

A string the host returns needs somewhere in **guest** memory to live, and only the guest owns that address space. So the guest exports `fk_alloc(n) -> ptr` and `fk_free(ptr)`, and **the host allocates while the guest frees**: the generated binding copies the bytes into its own language's string and calls `fk_free`, which is the only point at which anything knows the value is finished with.

With no allocator bound, a string return is **refused** rather than given an invented pointer — a made-up address lands in the middle of whatever the guest had there, and the corruption surfaces nowhere near the call.

An empty string and a `nil` both cross as `(0, 0)` and allocate nothing. The API returns `nil` for an absent optional constantly, so allocating for it would be a per-call cost paid for nothing.

`encode_rets` tracks what it allocated and **frees it if a later field fails**. Otherwise a two-string return whose second allocation fails leaves the first block owned by nobody — the host has forgotten it, the guest never saw the pointer — and it leaks for the session.

`fk_wstr` bounds-checks the **whole span once**. Per-byte checking would leave a half-written string behind when it tripped, and the spec's rule for an out-of-range store is that memory is not modified at all — the same rule `st64` already exists to honour for eight bytes.

It marks the whole span's PAGES once too, and that took a bug to establish. It writes head and tail through `st8raw` and the body straight into the word table, so it passes none of the store leaves the `--persist=packed` marking lives in — and it is the only write path into guest memory that the host, not the guest, drives. Marshalled strings were therefore live in memory and absent from every page flush, surfacing a save/load cycle later as zeros. Anything else added here that writes `mem` directly owes `MEMPACK.mark` the same call; see the packed-mode notes in [`guests.md`](guests.md). `TestAHostWrittenStringIsMarkedDirty` asserts the page COUNT as well as the bytes, which is only checkable against a page set: three pages for a string straddling two plus a control in a fourth, where a byte range could only ever have reported the five it spanned.

### Aggregates

**Optionality is not an edge case**: 619 of the 1203 fields across table-shaped concepts are optional, and 144 fields are themselves table concepts. An optional field expands into a **presence bool ahead of its value** — what a Rust `Option<T>` or Go `*T` lowers to under `repr(C)`. Matching what the guest language emits anyway beats the byte a bitmask would save, because the generator has to produce that struct too.

**An absent optional is omitted, not defaulted.** Factorio distinguishes "absent" from "present and false" throughout — absent means leave it alone, present-false means turn it off — so a default changes what the call does.

**And that was true of returns and of struct fields but NOT of arguments.** `read_struct` consulted a field's presence byte; `decode_args`, which reads the top-level argument list, never did — so every optional *argument* of every method arrived present-and-zero: an absent boolean as `false`, an absent number as `0`, an absent string as `""`. `entity.teleport(position, surface, raise_teleported)` is the shape that bites: a guest passing nothing for `raise_teleported` was telling the game **no** rather than saying nothing, and the difference is whether other mods see the event. Handles happened to survive it (handle 0 resolves to nil anyway), which is part of why it went unnoticed. *Enforced by `TestAnAbsentOptionalArgumentCrossesAsNil`, which also checks that present-and-zero still arrives as zero.*

An **array** is `(ptr, count)` with elements out of line, for the same reason a string is. A **dictionary is the same layout** over key/value pairs; only the table built at the end differs. Sharing the walk is not just less code — a dict of structs, or an array of dicts, works without anyone having written that case.

**An array of structs strides by the element's padded size**, not by the sum of its field widths. Get that wrong and the first element still reads correctly, which is the worst way for it to be wrong.

Nothing here promises an iteration order for a dictionary. `pairs` is insertion -ordered in this Lua and stable, but a guest that depends on it is depending on something the ABI does not offer.

A failed element frees the block it was writing into, the same rule `encode_rets` follows for strings and for the same reason: the guest never saw the pointer, so nobody else can free it.

---

## The guest bindings — **Go and Rust built (4255 of 4257 each at the 2.0.77 pin), C not**

`fklua gen-bindings` writes `guest/go/fkapi/fkapi.go` and `guest/rust/fkapi/src/api.rs`, committed as golden files so a regeneration is a reviewable diff. `--check` is the CI gate: a stale checkout is a build failure, not a method a mod author finds missing.

**4255 of 4257 members bound (99.95%), 2 deferred** at the default 2.0.77 pin — plus **240 `Into` variants**, which are second bindings over members already counted rather than members of their own. Scalars, strings, handles, optionals, structs, arrays, dictionaries, tier-2 dynamic values, **an array field inside a struct**, and containers nested to any depth. (The 4160/27 this line read until the nested-container round is the number the ports round left behind, and it is the shape of the history below rather than the state now.)

**Both backends, to the member id, since 2026-08-03.** Rust was at 4140/47 for four milestones while Go moved to 4160/27, and every one of the twenty was a branch the Rust generator had not grown rather than a shape Rust could not express. What closed it is the ports round, and what keeps it closed is the census: `rust_members_bound`, `rust_members_deferred`, `rust_deferrals_by_reason`, `rust_members_inherited`, `rust_event_payload_structs` and `rust_define_accessors` sit beside their Go twins in one committed file, so a feature added to one backend and not the other is a diff rather than something four mod authors report independently. *Enforced by `TestBothBackendsBindTheSameMembers`, which compares the counts AND the member id sets — a missing member and an extra one cancel in a total.*

Optionals are **pointers**, so `nil` means absent rather than zero — Factorio distinguishes the two throughout, and a Go author needs to be able to say which. Structs get real named types, so a binding reads `Teleport(position MapPosition, surface *Object, raise_teleported *bool)` rather than taking an anonymous struct nobody can declare.

### Arrays

An array field is `(ptr, count)` and the elements live out of line at `ptr + i*stride`. Both halves of the layout are needed to generate one: `Placed` carries the stride, which is the element's **padded** size and cannot be recomputed from the Go type, and `FieldSpec` carries the element's TypeName, which is what a struct element is named after. Neither alone is enough.

Direction decides who allocates, and the ownership follows:

- **Coming back**, the host wrote the elements by calling the guest's own `fk_alloc`. The binding copies them into a Go slice and frees immediately, so what the caller holds is guest memory with no lifetime tied to the call.
- **Going out**, the binding allocates, fills, passes `(ptr, count)`, and frees on return. The host reads during the call and never retains.

An optional array stays `[]T` rather than becoming `*[]T`: nil already says absent, and `[]T{}` says present-and-empty, which is the distinction the pointer existed to preserve.

`fk_free` now drops the pin rather than doing nothing. Under `-gc=leaking` the bytes are never reclaimed either way, but arrays made allocation routine instead of rare — a mod calling `find_entities_filtered` every tick would otherwise append sixty entries a second to a list nothing ever shortens.

**Only running it proves any of this.** TinyGo removes every member a guest does not call, so the type-check gate sees the array encoders and nothing more, and a wrong stride or a transposed pointer/count read is past the type checker by definition. `TestArraysCrossInBothDirections` builds a guest that calls one of each element shape and checks the values that come back; transposing the pointer and count reads turns its first line into `handles: 80112`.

### `<Name>Into(dst, …)` — a caller-supplied destination — **built, both toolchains**

`out := make([]T, n)` on every call, under a mandatory `-gc=leaking`, is a permanent allocation per call. Downstream measured **~1.3 KB of permanent guest heap per network compile**, most of it here. The elements were already free — the host writes them into the marshalling arena and the binding's bracket reclaims it — so the slice's backing array is the whole of what was left.

So the generator emits a **second binding** for every member whose single return is an array. **240 in each language** at the 2.0.77 pin — they were 240 and 238 while the Rust backend was behind, and the ports round levelled them — across 62 classes, 110 of them arrays of objects:

```go
ents, err := surface.FindEntitiesFiltered(f)          // allocates, per call
ents, err = surface.FindEntitiesFilteredInto(ents, f) // reuses the capacity
```

```rust
surface.find_entities_filtered_into(&mut buf, f)?;    // buf is cleared and refilled
```

**The host side is untouched.** Both bindings pass the same member id and the same blocks; only what the guest does with the returned `(ptr, count)` differs. No new member entries, no coverage change, nothing to prune differently — which is what makes it cheap enough to apply to every member the branch covers rather than to the one that was asked for.

**The two languages' signatures deliberately differ, and this is the only place in this generator where they do.** Go has no out-parameter, so its variant takes `dst []T` and *returns* the slice — a grown slice is a different header, so the caller must use the return value, and the doc comment says so. Rust has `&mut Vec<T>`, which reallocates in place, so the value comes back through the parameter and the result is `Result<(), Status>`. Forcing one shape onto the other would mean handing a `Vec` in and out by value for no reason.

**Dictionaries are excluded in both**, for mirror-image reasons: `BTreeMap` has no `reserve`, so "reuse the allocation" has no expression in `alloc`, and `make(map[K]V, n)` into an existing Go map means clearing it key by key.

*Measured — `examples/heap` reads the allocator's own bump pointer; `examples/callcost` times it through the real guest under `lua52f`, four elements:*

| find_entities_filtered, 4 entities | allocating | `Into` |
|---|--:|--:|
| permanent guest heap | **16 B/call** | **0** |
| `--persist=table` | 10,919 ns | 10,649 ns *(1.03×, at the noise floor)* |
| `--persist=packed` | 172,625 ns | **132,071 ns** *(1.31×)* |

**Under `table` this is an allocation fix and not a speed fix, and the table says so.** Packed gains because a guest allocation dirties pages the flush then has to repack; `table` has no such term, and quoting the packed ratio as the feature's value would be quoting a persistence mode.

`TestArraysCrossInBothDirections` gates it in both languages and asserts *two* things, because either alone passes a variant that is wrong in the other way: the contents match the allocating form, **and** a destination with room is not reallocated (compared by the address of the first element). The equality half is asserted over an array of **strings**, not handles — every array of objects comes back as fresh transient handles, so two calls return different numbers for the same three players and comparing them would fail on correct code.

### `<Name>Is(want)` — a host-side string predicate — **built, both toolchains**

`entity.name == "transport-belt"` with the name never existing in guest memory. **282 members**, one for every attribute the API declares as a plain non-optional string, in both languages (`NameIs` / `name_is`).

The cost it removes is the one downstream named as the last no mod can write its way out of: a guest subscribed with a **category** filter is entered for every entity anyone builds anywhere on the map, and has to read the name to discover it does not care. `Name()` returns `string(b)` — a copy, necessarily, because the arena underneath is released when the call returns — so the string is bought before the decision that would have said not to buy it.

**It is a third member KIND (`MemberGetEq`, `M.EQ = 3`), not a new import.** The member table is already one entry per (class, member, kind) with its layout computed at generate time, and a comparison is a third thing you can do to an attribute exactly as `GET` and `SET` are the first two. As a kind it inherits handle resolution, the `valid` check, the `pcall` around the member read and the `ERR_NO_MEMBER` path with no code of its own; `fk.call` needs no new shape; and the member-id scan that prunes the shipped table keeps working, because the id is still an ordinary `i32` constant at the call site. A `fk.streq(handle, member, ptr)` import would have needed a seventh host import, its own wiring in `fk_mod.lua` and `factorio.Hooks`, and would have read as ABI plumbing in mod code.

**The constant costs nothing to send.** A Go string literal's bytes live in the data section, so `putStr` writes a `(ptr, len)` out of the string header and marshals nothing — the same way a filter constant travels.

**The length check is not an optimisation, it is what makes the feature pay.** Measured without it, the predicate cost *the same* as reading the name and comparing it in the guest — 4,484 ns against 4,269 — and won only on heap. The direction is why: the host **writing** a string into the guest is 6.44 ns/byte and **reading** one back out is 14, so a predicate that always decodes trades a cheap write for an expensive read. So `call_eq` reads the guest's `(ptr, len)` *before* `decode_args` would have run, compares `#f` against the length, and only decodes on a match — and the answer a category-filtered handler usually gets is "no".

*Measured, 44-character name:*

| `entity.name` | read + compare in the guest | `NameIs`, match | `NameIs`, no match |
|---|--:|--:|--:|
| permanent guest heap | **48 B/call** | **0** | **0** |
| `--persist=table` | 4,205 ns | 3,504 *(1.20×)* | **2,308 *(1.82×)*** |
| `--persist=packed` | 161,592 ns | 123,315 *(1.31×)* | 120,819 *(1.34×)* |

**EVERY plain-string attribute, optional ones included — 282 of them — and that is an adjudication rather than the original design.** This paragraph read "non-optional attributes only" for two milestones, and the guard behind it (`&& !f.Optional`) was **dead**: `f.Optional` is set by `mapFields`, one level down, and never on the attribute path, so all 30 optional string attributes always got a predicate. fklua-ports' nixie-tubes found it as **G4**, from the outside and the hard way — it had to reproduce the generator's numbering to derive one member id, and honouring the comment produced 4,143 members where the census recorded 4,187.

**Adjudicated in favour of the code, on both halves.** An absent optional compares **false**, which is the honest answer and the one `call_eq`'s `type(f) == "string"` already produces; a caller who needs absent-versus-different asks the GET, which since the optional-attribute fix says so. Deleting thirty working members to honour a sentence would have moved every member id after the first one, for nothing. What is fixed is the sentence — here and in `GenerateMembers`, which carries the same reasoning at the call site.

A value that is not a string at all answers **false** rather than coercing: the generator emits this kind only where the API promised a string, so anything else means the running Factorio disagrees with the description the mod was built against, and `17 == "17"` being true would hide that.

`TestAStringPredicateComparesHostSide` runs it under `lua52f` over the case a length check cannot answer — **a different string of the same length** — plus longer, shorter, empty-both and not-a-string.

### What both of those are worth under `--gc=collected`

**Less, and still positive.** The numbers above were taken under `-gc=leaking`, where every byte is forever, and they are the upper bound. A collected guest reclaims the copy and the slice, so neither is permanent heap any more — they are garbage. Garbage still has to be allocated, marked around and swept, and the pacer's step budget is spent on exactly that, so **fewer allocations is fewer collections**. The first downstream mod ships `--gc=collected` as of its round-8 re-measure, which weakens the urgency of both of these and not their value.

### An unexpressible OPTIONAL argument is omitted, not fatal

`LuaGameScript::create_surface(name, settings?)` was deferred whole because its optional `MapGenSettings` carries dictionaries — so a mod could not create a surface at all, and a genre (hidden-surface mods, Factorissimo-alikes) sat behind an argument almost nobody passes. It binds now as `CreateSurface(name string) (Object, error)`.

**This is a different act from dropping a struct FIELD**, which this package refuses on the grounds that a struct missing a field is a wrong value the guest cannot detect. Here nothing is wrong and nothing is hidden: an absent optional is omitted rather than defaulted at every layer, so the call the host makes is exactly the call a Lua author writes when they leave the argument out; the presence byte stays 0 because the block arrives zeroed; and the caller can *see* the parameter is not there, at compile time, with the reason in the doc comment above it. A MANDATORY argument that cannot be expressed still defers the member.

`MapGenSettings` remains unreachable, and the way to reach it is **dictionary fields inside a struct** — the same capability the 5 deferred event payloads want. Two of its three dictionaries are ordinary `string`-keyed maps; `autoplace_settings` is keyed by a union and would need the tier-2 key case too.

### Dictionaries, and the one that is refused on purpose

A dictionary is the same walk over key/value **pairs**. The pair is a two-field block, so the value sits at the key's *padded* size rather than its width, and both offsets come from the layout rather than being computed.

**Dictionary arguments used to be refused, and the refusal's own words are what retired it.** It read *"would need a deterministic iteration order"*, and it was right about a Go MAP: Go randomizes map iteration per process, Factorio is lockstep, and a per-run ordering reaching the game is a per-*client* difference. What changed is that a dictionary is **not a map here any more** — every one of them is the ordered pair SLICE, whose order is the guest's own, chosen once, identical on every client. So the seven that were counted are emitted, and among them are the `tags` setters on `LuaEntity`, `LuaGuiElement` and `LuaItemCommon`: `read_write` attributes whose getter generated alone, which fklua-ports' fluid-memory-storage reported as **F-TAGS**.

**And every dictionary RETURN is the pair slice too, which is qol-research's Q3.** `game.forces` (union-keyed) came back as an ordered slice and `force.technologies` (string-keyed) as a Go map, one generator line apart — and resource-marker widened the hazard past the version anyone could argue with: a loop that only READS still has to sort first, because walk order decides the order the guest ALLOCATES in and the guest heap is in the save. A guest that wants lookup builds a map from the slice and owns the decision.

The Rust side needed neither change: a `BTreeMap` iterates in key order, so its returns were always deterministic and its arguments only ever lacked an emitter.

What still defers is **2 members in each language**, and both are genuine name collisions: `LuaControl::set_driving` against the `driving` attribute, and `LuaPlayer::set_zoom_limits`. Everything the container shapes were blocking is bound — see "A nested container" below. One string LITERAL also has no identifier name (the empty string in `LinkedGameControl`) and is counted in a row of its own, because a constant is not a member; see "The census arithmetic". `fklua gen-bindings` prints all of them, in full, every run.

### A nested container — a dictionary of a dictionary, and of an array

**AD4 one level over, and the host never had the gap.** A dictionary nested in a STRUCT was AD4: a top-level dictionary rendered fine, one nesting level refused, and unblocking it took 17 deferrals with it. A dictionary whose VALUE is a dictionary or an array is the same fact one level further in, and it was **16 of the 18 remaining member deferrals** in each language — 8 "a dictionary of a dictionary", 7 "a dictionary of an array", 1 "an array of an array".

Nothing about the wire changed, because the wire was never the problem. `LayoutStruct` lays a container's element out as a one-field block and recurses; `placedList` renders a nested `stride/key/elem` descriptor at any depth; and `fk_abi.lua`'s `read_value` routes `K_ARRAY` and `K_DICT` to one `read_array` walk that calls `read_value` again on each element — the shared-decoder comment there has said "a dict of structs, or an array of dicts, works without anyone having written that case" since the ABI existed. Every one of these members has been in the host table, correctly laid out and correctly marshalled, all along. What was missing was a guest TYPE and a guest-side codec, in both backends.

**What recurses**: a dictionary's VALUE and an array's ELEMENT, to any depth. `LuaPlayer::get_alerts` is three levels — `dictionary[uint32 -> dictionary[uint32 -> array[Alert]]]` — and comes out whole.

**What does not**: a dictionary's KEY. It must still be a scalar, a handle or a tier-2 `Value`, because a Lua table key is not a table and no member in any pinned version keys one that way. That refusal keeps its own census reason ("a dictionary keyed by …") so a description which grows one arrives as a NEW number in the version diff rather than as a binding somebody guessed at. *Enforced by `TestADictionaryKeyedByAContainerIsStillRefused`, in both backends.*

**Determinism is applied PER LEVEL**, which is the constraint that decides the shape. Q3's rule — a dictionary is an ordered pair slice in Go, never a map, because Go randomizes map iteration and a lockstep game turns a per-client walk order into a desync — is a property of each level independently, so an inner dictionary is `[]Entry<K><V>` exactly as an outer one is. Rust reaches the same place from the other side, asking `rustDictType` once per level: a `BTreeMap` where that level's own key is `Ord`, an ordered pair `Vec` where it is not. So `LuaRemote::interfaces` is `[]EntryStringSliceEntryStringBool` in Go and `BTreeMap<LuaStr, BTreeMap<LuaStr, bool>>` in Rust, and neither can produce a per-client order at either depth.

**A CODEC FUNCTION RATHER THAN A DEEPER INLINE LOOP.** The depth-one walks are inline at four sites per backend (a member's argument encode, its return decode, a struct field's encode and its decode); inlining a second level would multiply that by the depth. Each distinct nested container gets one generated triple instead — `decCtn<T>` / `encCtn<T>` / `valCtn<T>` in Go, `dec_ctn_<t>` / `enc_ctn_<t>` / `val_ctn_<t>` in Rust — and the four sites gained one branch each: where they called `goLoad` on a scalar or `decode<T>` on a struct, they call the container's decoder. **The depth-one output is therefore byte for byte what it was**, which is what makes the golden diff readable: new members, new helpers, and nothing that already worked moved.

The third function is not decoration. `ToValue`/`to_value` renders a generated struct as the tier-2 table a union-typed field wants, and it walks a container field's elements — so a nested one there would have fallen through to `OfNumber(float64(...))` on a slice. That is the silent-wrong-value shape this package refuses everywhere else.

**`LuaPrototypes::utility_constants` is what this was owed to.** The nil-field fix moved it from an attribute-shaped host SKIP to a named deferral, on the promise that it would arrive free the day this shape was built; its blocker was one field, `default_trigger_target_mask_by_type`, typed `dictionary[string -> dictionary[string -> boolean]]`. *Enforced by `TestUtilityConstantsBindsAndReachesItsNestedLeaf`, which asserts the member is in BOTH backends' name maps and that the field is typed all the way down to the boolean.*

**The stride is what no compiler checks, so it is pinned three ways.** `TestANestedDictionaryCrossesInsideAStruct` builds that exact shape, asserts the literal stride and both offsets at each level, reads the SAME numbers back out of each backend's emitted decoder, and then round-trips a two-group nested table through the real `fk_abi.lua` with `write_struct`/`read_struct`. The inner pair is the one worth knowing: a string key is `(ptr, len)` and aligns to **4**, not 8, so a `bool` value sits at offset 8 and the pair pads to **12** — not 9, not 16. That is the `(dyn, handle)` stride-24 lesson one level down, and a decoder using the value's own width as the stride reads the next pair's key as a boolean and is wrong from the second entry onward. One test over both backends rather than two, because AD5 is what happens otherwise.

### The census arithmetic — `host = bound + deferred`

`host_members_bound` is what `GenerateMembers` put in the table; `<lang>_members_bound` and `<lang>_members_deferred` are what each guest backend made of exactly that table, in one loop that ends every iteration in an emit or a deferral. So the three rows reconcile by construction — and they read **4842 against 4843, in both languages, for five milestones.**

The extra one was not a member. The string-enum constant loop runs after the member loop, emits `pub const` / untyped constants rather than bindings, and called the same `defer1` the member loop does; one literal in the description (`LinkedGameControl`'s empty string) has no identifier name. `LiteralsDeferred` is its own counter now, with `<lang>_literals_deferred` and `<lang>_literal_deferrals_by_reason` census rows and its own heading in the deferral report. What a nameless literal costs a guest is one spelling of a string it can still write out; what a deferred member costs it is a call.

**The accounting line had the same defect pointing the other way.** `fklua gen-bindings` reconciles the description's methods, both halves of every attribute and the class operators against the member KINDS they became — and `MemberGetHandle`, kind 7, is in none of those four buckets, because it is a SECOND member over an attribute the readable half already counted. So the printed decomposition summed to **4784 against 4842** and said nothing about the 58 missing. A member kind that reaches no line of the report is exactly the F-IDX shape, met from inside the generator instead of outside it: `custom_table_handle_members` is a census row now, the line prints it, and the line prints its own total.

*Enforced by `TestTheCensusMemberArithmeticCloses`, which checks the identity in both languages against the live generation AND against the committed `census.json`, checks the bound member ids are ids the host table actually has (a member counted twice and a member counted in neither cancel in a total), and checks the seven accounted kinds sum to the table.*

### Class operators — `obj[k]`, `#obj`, `obj(...)` — **built, both toolchains**

Eleven operators across seven classes, and until 2026-08-03 **no generator read `Class.Operators` at all**: they were not bound, not deferred, and not counted. `LuaChunkIterator` bound `object_name`, `object_name_is` and `valid` — three members, none of which was the iterator — and a guest that wanted `surface.get_chunks()` had to sweep `is_chunk_generated` over a bounded square, which is 289 host calls where upstream makes one and cannot see a chunk outside the radius at all. Reported by fklua-ports' resource-marker as **RM1**, and independently as qol-research's **Q2** (reading one entry of `force.technologies` materialised all 319, ~24 KB of guest heap per call) and fluid-memory-storage's **F-IDX** (`inventory[1]`, the way every Lua mod reads a stack, unreachable). One gap, filed three times from three sides.

| class | operators | bound as |
|---|---|---|
| `LuaChunkIterator` | `call` | `Call()` / `call()` |
| `LuaCustomTable` | `index`, `length` | `Get(Value)`, `Length()` |
| `LuaFluidBox` | `index`, `length` | `Get(uint32)`, `Length()` |
| `LuaGuiElement` | `index` | `Get(Value)` |
| `LuaInventory` | `index`, `length` | `Get(uint32)`, `Length()` |
| `LuaRandomGenerator` | `call` | `Call(lower, upper)` |
| `LuaTransportLine` | `index`, `length` | `Get(uint32)`, `Length()` |

**THREE MORE KINDS — `M.IDX = 4`, `M.LEN = 5`, `M.SELF = 6` — and that is where the report needed correcting.** RM1 predicted a fifth kind for `call` and *no ABI change at all* for the other nine, reading `obj[key]` as something the `GET` kind could already carry. It cannot: **every existing kind begins by resolving `obj[m.name]`**, and an index operator's key is an ARGUMENT rather than the member's name; `#obj` is not a member read either. So each gets its own branch in `M.invoke`, placed BEFORE the member read — falling through would resolve a key called `"index"` on a LuaObject, which raises on some classes and returns nil on the rest, i.e. `ERR_NO_MEMBER` for a member that is right there.

Each branch is two lines and is `pcall`-ed for the reason the member read is: a Factorio metamethod raises (an inventory index out of range, an iterator past its end) and an error crossing a wasm frame takes down the mod rather than the call. A **nil result is not an error** — `LuaFluidBox`'s index is declared optional and an empty fluid box really is nil — so it goes back through `encode_rets`, which clears the presence byte the signature carries. `M.SELF` is `pcall(obj, ...)` rather than `pcall(f, ...)`: the object IS the callable, and there is no member to look up.

**`Get`, not `Index`, and the rename is forced rather than chosen.** `seen` in both binding generators is first-come, and `LuaInventory` and `LuaGuiElement` each declare an ordinary attribute called `index` — so an operator named after itself would lose the name to it on the very class F-IDX was about. `TestOperatorsBindOnEveryClassThatHasOne` fails loudly if a future pin puts something else in the way.

**WHERE THE INDEX KEY'S TYPE COMES FROM, since the description does not carry one.** An operator declares only what indexing *yields*, so the key is derived, by two clauses over facts the description does state:

- a class that also declares `length` answers Lua's `#`, the SEQUENCE-length operator, so it is indexed by position: `uint32`. `LuaFluidBox`'s own index description says so out loud — *"the index must always be in bounds (see length_operator)"* — and `LuaInventory`'s example is `get_main_inventory()[1]`;
- ...unless what it yields is itself tier 2, which is the description saying the class is heterogeneous. `LuaCustomTable` yields `Any`, and it really is keyed by `uint32 | string` at `game.players` and by string at `force.technologies` — so its key is a tier-2 `Value`, which is the same answer `goDictKV` gives a union-keyed dictionary one file away.

That leaves `LuaGuiElement`, which declares no `length` and indexes by child NAME, on the tier-2 arm as well. `TestOperatorKeyKinds` enumerates all five so a pin that adds a sixth fails rather than being classified by a rule nobody re-read.

**There is no write half, and that is the description's doing.** An operator carries a `read_type` and never a `write_type`, so `fluidbox[1] = f` — which the prose says is legal — is not a shape the API description models.

**`Any` had to stop being a handle first.** `canonicalUnion` picks the one option a fixed layout can carry, and its shape B is *one class plus scalar identifiers* — `ForceID` is `string | uint8 | LuaForce`. `Any` is `string | boolean | number | table | LuaObject`, which matches shape B **on a count** and is not shape B at all: the `table` arm makes it a genuine any-value union. Left alone it would have typed `LuaCustomTable`'s index as returning an OBJECT, and it was already mistyping `remote.call` and `LuaLazyLoadedValue::get`. An option that maps to tier 2 now disqualifies the union.

**Several return values are emitted too**, which is fklua-ports' nixie-tubes **G1**: 13 members, one of them `LuaBootstrap::register_on_object_destroyed`, the ONLY way to arm `on_object_destroyed` — so that port hand-wrote a binding into the generated package to ship. The deferral's own comment said *"naming rules, not marshalling"* and was right: the host has carried four result slots since M6 and the layout has laid out multi-field return blocks all along. Go returns several values, Rust returns a tuple. Two of the thirteen return an ARRAY first and two handles after it, so the decode is per-field with every local suffixed by the field's index — which is what the single-return version was quietly relying on not needing.

### Inherited members are FORWARDED, not embedded

**79 of the 148 classes have a parent, and an inherited member appears in neither the child's method list nor its attribute list** — so `LuaEntity` had no `Position()` and no `SurfaceIndex()`; they are `LuaControl`'s. Dispatch never cared, because it is name-based and the handle decides the object, and that is exactly what made the workaround legal *and* undiscoverable: `fkapi.LuaControl{Object: fkapi.ObjectAt(h)}.Position()`.

**1094 one-line forwarders**, not an embedded parent field, and that is the whole decision. Embedding promotes the parent's methods in one generator line — and breaks every `fkapi.LuaEntity{Object: h}` in existence, because a composite literal cannot name a promoted field and the idiom would become `LuaEntity{LuaControl: LuaControl{Object: h}}`. The forwarders cost ~30% more generated source, which TinyGo's dead-code elimination removes again, and cost nobody a rewrite.

A name the class declares **itself** always wins, and among ancestors the nearest wins: an override is a real thing in this API and a forwarder must never shadow the member it exists to complete. *Enforced by `TestASubclassReachesItsParentsMembers` and `TestAForwarderNeverShadowsTheClassesOwnMember`.*

**And the same 1325 forwarders in Rust since 2026-08-03**, sharing the pass and the two rules — four of the seven mod ports found this independently, and one of them called the workaround `LuaControl(e.0).position()` "legal because dispatch is by member id, and the compiler error points nowhere near it", which is the same sentence the Go comment above already contained.

**A forwarder rather than a `Deref` impl, and the alternative is worth stating because it looks free.** `impl Deref for LuaEntity { type Target = LuaControl }` is 79 impls instead of ~1300 methods and gets the override rule for nothing — an inherent method always beats one reached through a deref. Three reasons against, in order of weight. It is the *Deref polymorphism* anti-pattern, and a `*entity` that yields a `LuaControl` is a semantic claim a handle newtype should not make. It needs `#[repr(transparent)]` on all 148 handle types plus an unsafe reference cast per class, because `Deref` must return a **reference** and there is no parent value anywhere to borrow. And the inherited set stops being countable, where a forwarder is one census row beside the Go one — which is the mechanism this round added specifically to stop the two backends drifting again. The generated source is bigger and rustc's dead-code elimination removes it, which is the trade gogen already took.

### Event payloads get a generated struct

`gen-bindings` emitted an `Event*` id constant per event and **nothing for the fields**, so a guest read them by casting the pointer `fk_on_event` was handed and adding hand-derived byte offsets — FkLua's own `examples/api` did it, with the layout in a comment. Fields are placed by the API's `order`, so one new optional field upstream shifts everything after it, and being wrong is **silent**: the guest reads a neighbouring handle and quietly does nothing.

**213 of the 218 bound events now have a Go struct and a `Read<Event>(ptr)` reader.** An event's field list *is* a struct's field list, so this is the same machinery — which is also why the remaining **5 are deferred with a reason** rather than emitted wrong: their payload carries a **dictionary** field (`tags`), and `goStructs` accepts scalars, structs and arrays but not yet dictionaries. That list includes `on_built_entity`, `on_robot_built_entity` and `on_space_platform_built_entity`, which makes **dictionary fields inside a struct** the single highest-value next step here. Counts live in the census under `go_event_payload_structs` and `go_event_deferrals_by_reason`, apart from the member counts — an event is not a member, and one number moving for two unrelated causes is a diff nobody can read.

**A deferred struct used to be emitted as an EMPTY one, and this found it.** `add()` reserves the name in the emission order before recursing (so a type reachable from itself does not spin) and its failure path deleted the entry from `byName` and left it in `order`; `emit()` then read a zero `StructBlock` and wrote `type X struct{}` under the concept's real name. **Ten types shipped that way** — `MapGenSettings` and `TileBuildabilityRule` among them — which is exactly the failure this package has a rule against. It stayed invisible because every member that would have used one was deferred for the same reason, so nothing referenced the empty type; event payloads are the first *top-level* registrations that can fail, which is what made it visible. *Enforced by `TestADeferredStructIsNotEmittedAsAnEmptyType`.*

**The Rust generator had the identical bug and kept it two milestones longer, because the test was written against the other backend.** `rustStructs.add`'s failure path deleted the entry from `byName` and left it in `order` exactly as gogen's had, so `pub struct CollisionMask {}` and `pub struct MapGenSettings {}` shipped with codecs that read and write **zero bytes** — `decode_at` compiles, runs, and returns a default while sixteen bytes of wire sit unread. A mod port found it by grepping the committed bindings (fklua-ports, AD5).

Both are fixed and both are now covered by a test that does not depend on something being deferred: `TestARefusedStructLeavesTheEmissionOrder` asks each collector directly, with a shape that really fails, and asserts the name leaves the emission order. The source-scan test above can only catch this while some struct is deferred, so on the day the last deferral closes it starts passing vacuously — which is exactly the state the Rust backend is in now.

**218 of 218 in Rust too, with `read_<event>(p)` readers**, since 2026-08-03. Three ports carried 45 hand-derived offsets between them plus two scripts that re-derived each one from the **Go** bindings, because nothing else could check them; one of them supplied the sharpest example of why they are not guessable — `on_built_entity` puts `entity` at 0 and `on_robot_built_entity` puts it at 4, because `robot` sorts before `entity` by `order` while the JSON lists the fields alphabetically in both. Reading the description top to bottom gives the right answer for one and the wrong answer for the other.

### Struct type names

155 concepts are named and keep their name. 126 uses are inline tables in a method signature — nearly all `takes_table` argument tables — and get one synthesised from where they appear. The nested-type mapping is **recorded when the name is decided**, not recovered afterwards by matching layouts: two distinct concepts can share a shape, and guessing between them names the wrong type.

### Variant parameter groups are tier 2, not hand-written

The plan said "the 4 with `variant_parameter_groups` get hand-written", and that sentence was carried as a deliverable for three milestones. It rested on a miscount that the skip census made visible: **4 METHODS have variant groups, and so do 55 CONCEPTS** — 31 of them the `Lua*EventFilter` family, plus every prototype filter. Both refusals reported the same reason, so one number read as 68 skipped members when it was two different problems, and hand-writing was never going to reach the larger one.

A variant-parameter table is a **discriminated union**: base fields, plus a group selected by a discriminant. That is what tier 2 already carries. Mapping it to `KindDyn` instead of refusing it took the host from 3836 to **3905 of 3909**, and left exactly four skips — three callback parameters and one variadic.

The cost is real and worth stating plainly: a caller gets a `Value` rather than a named struct, so `create_entity` takes a tagged table instead of a typed one. Available-but-untyped beats unavailable, and a hand-written typed wrapper over the handful of high-traffic methods can still land on top. Generating a Go type per variant is the trap tier 2 exists to avoid — and at 91 variant-group methods in 2.1.12, so is hand-writing.

**This is also why the "few enough to hand-write" premise deserved re-checking before being acted on.** It was version-dependent, and it was miscounted.

### A dictionary field inside a struct — and it is a SLICE of pairs

`goStructs.add` took scalars, structs and arrays, and one refusal took three things down with it: the **5 deferred event payloads** (`on_built_entity`, `on_robot_built_entity`, `on_space_platform_built_entity`, `script_raised_revive`, `on_research_cancelled` — all blocked on `tags`, which is `dictionary[string → any]`), `MapGenSettings` and therefore `create_surface`'s optional argument, and 12 members blocked by a struct blocked by one of those.

| | before | after |
|---|---:|---:|
| Go members bound | 3859 | **3875** |
| Go members deferred | 46 | **30** |
| event payload structs | 213 | **218** |
| event payloads deferred | 5 | **0** |

The Lua side never had this gap — `read_value` routes `K_DICT` into the same `read_array` walk an array uses, so a dict inside a struct already crossed. Only the Go generator refused.

**The field is `[]EntryStringValue`, not `map[string]Value`, and that is the whole design question.** A struct crosses in **both directions** — the same type is an argument and a return — and writing a Go map to the wire means choosing an order for its pairs, which Go deliberately randomizes. Factorio is a lockstep simulation where `pairs` follows insertion order, so a per-run ordering reaches the engine as a per-*client* difference: a desync, found by players. A slice of pairs is deterministic by construction, which is the same reasoning that made tier 2's own `Value.Map` a slice and the same reasoning that refuses a dictionary *argument* at member level rather than sorting it. A guest that wants lookup builds a map itself, from an order it chose.

Pair types are deduplicated by (key type, value type), so `Tags` appearing in five payloads emits one `EntryStringValue` rather than five identical types — **6 entry types** for the whole API.

**A member-level dictionary RETURN is still a `map[K]V`, and that is not an inconsistency to fix by symmetry.** It is decode-only: the host built it, the guest reads it, and there is no order for the guest to get wrong on the way back, so the map buys lookup for nothing. **The exception is a key a Go map cannot hold**, and that is now built.

### A dictionary keyed by a tier-2 value — **built**

`game.surfaces`, `game.players` and `game.forces` are dictionaries keyed by "an index or a name", which is a union and therefore `KindDyn`. All three deferred, so a guest could not enumerate surfaces and the first downstream mod probed indices until it found a gap.

The refusal was arithmetic, not design: `Value` holds slices, so it is not comparable and cannot be a Go map key, and "three members do not earn a second container shape in the bindings" was true *when there was no second container shape*. The dictionary-field work introduced one — and a pair slice has no comparability requirement at all — so the fix is that `goDictKV` stops refusing and the **caller picks the container**: a slice when the key is dyn, the map otherwise. **3875 → 3878 members bound, 30 → 27 deferred**, and the reason `a dictionary keyed by a dynamic (tier 2) value` leaves the census entirely.

```go
func (o LuaGameScript) Surfaces() ([]EntryValueObject, error)
```

The asymmetry with the map is deliberate and now has two different reasons behind it rather than one, which is why the generated doc comment on an entry type states both: a struct FIELD is a slice because it crosses in both directions and a Go map's order is randomized (a desync); a dyn-keyed RETURN is a slice because the key could not be a map key at all. A guest wanting lookup builds the map itself, from an order it chose.

**Rust reaches it from the other side and lands in the same place.** A `BTreeMap` key must be `Ord`, and tier 2's `Value` holds an `f64` and a `Vec`, so it is neither `Ord` nor `Hash` — a more honest refusal than Go's "not comparable" and the same arithmetic. The container is a `Vec<(Value, Object)>`, a **tuple** rather than a generated `Entry` type: Rust tuples are structural, so there is no name to invent or deduplicate, `for (k, v) in ...` beats `e.Key` / `e.Val`, and the hand-written runtime had already made the same choice for tier 2's own `Value::Map`. Where the key IS `Ord` the return stays a `BTreeMap`, for Go's reason plus one Go does not have: a BTreeMap iterates in key order, so a dictionary that crosses in both directions is deterministic by construction rather than by refusing to be a map. *Enforced by `TestARustDictionaryKeyedByADynamicValueBinds`, which pins the stride and the value offset as well.*

*Enforced by `TestADictionaryKeyedByADynamicValueBinds`, which asserts the census reason is gone and that `Surfaces` returns a pair slice, and by `TestADictionaryKeyedByADynamicValueCrosses`, which pins the thing no compiler checks — Go places the pair's key at 0 and its value at the key's PADDED size, so a (dyn, handle) pair strides by **24, not 20**, and the generated decoder reads `readDyn(&d[0])` and `&d[16]` at exactly those numbers.* The Lua side never had this gap; `read_value` already routed a K_DYN key through `write_value` like any other.

**Only running it proves any of this.** `TestADictionaryFieldCrossesInsideAnEventPayload` (`internal/guest`) builds `examples/dict` with TinyGo, has `fk_abi.lua` encode a real `on_built_entity` payload with three tags of three different tier-2 tags, and checks the values the generated decoder produces — plus the scalars placed *after* the dictionary and a handle placed *before* it, since a wrong header width moves both. Reading the value from the key's offset turns its first line into `colour=MISSING count=MISSING live=MISSING`. It ran **both languages** since 2026-08-03, against one stub and one set of expectations — `guest/rust/examples/dict` is the mirror, and it could not have been written before, because the Rust generator carried neither dictionary fields inside structs nor event payload structs at all. Two guests agreeing line for line is what says one analysis has two renderings.

### A Lua string is BYTES, and Rust's `String` is not — the reader that rewrote what it read

The section above is the Rust rendering being **better** than the Go one. This is the one where it was worse, and it is the same lesson from the other side: two renderings of one wire, and only one of them was ever asked what it did with a byte the other language takes for granted.

**A Lua string is an arbitrary byte sequence.** `string.char(0xFF)` is a Lua string; so is a UDP datagram, a `helpers.write_file` payload, and anything a mod put in a `tags` entry. Go's `string` is exactly that, so `getStr` is `string(b)` and is byte-exact by construction. Rust's `String` is **not** — it carries a UTF-8 validity invariant that safe code relies on — and the generated reader reconciled that with:

```rust
String::from_utf8_lossy(b).into_owned()   // until 2026-08-06
```

which replaces every byte outside UTF-8 with U+FFFD, **silently, and changes the length while it does it**. So `read_on_udp_packet_received` handed a Rust guest a frame header whose epoch `0x51C0FFEE` had become replacement characters and whose length was no longer the length the host wrote, while the Go binding read the same wire correctly. Every generated event reader and every string-returning attribute shared it — **738 call sites**.

**Why it shipped for four-plus milestones is the reusable part, and it is a recorded shape**: *a corpus written by the compiler's authors tests the compiler's authors' habits.* Every string in this repo's own guests is a prototype name, a locale key or a surface name — ASCII by engine constraint — so lossy and exact are the same function on all of them. It took a guest doing something no example did (`guest/rust/fkipc`, carrying binary frames) to reach it, which is exactly how "the Rust generator was four milestones behind" and AD5 were found. fkipc's workaround is worth naming because it was written the *right* way and was still a workaround: it located the `(pointer, length)` pair by asking the generated ENCODER where it puts one — never a hand-derived offset — and read the bytes itself.

**The fix is a type.** A string-shaped generated position is a `LuaStr`, which owns a `Vec<u8>`:

```rust
pub struct LuaStr(Vec<u8>);          // Deref<[u8]>, Ord, Hash, Default
ls.as_bytes()          -> &[u8]      // the bytes, always
ls.as_str()            -> Option<&str>   // the CHECKED conversion
ls.to_string_lossy()   -> Cow<str>       // the old behaviour, named
ls.set(&[u8])                            // refill, reusing the allocation
```

Four decisions inside it are load-bearing:

- **`from_utf8_unchecked` was rejected, and the argument is soundness rather than taste.** Handing safe Rust a `String` whose bytes are not UTF-8 is library UB the moment anything slices, iterates or formats it — and the bytes here come from the engine, so no guest could establish the precondition. It is not hypothetical: `fkipc` was doing exactly that to send a binary frame (`String::from_utf8_unchecked(data.to_vec())`, and `as_mut_vec` on the send path), which the byte type retires along with the reader.
- **Arguments stay `&str`.** `put_str` writes a `&str`'s bytes verbatim, so an argument is byte-exact for anything a `&str` can hold; what it cannot hold is a non-UTF-8 sequence, and for that the tier-2 path (`Value::Str(LuaStr::from(bytes))`) is the escape hatch — which is where the binary-carrying members already live, since `helpers.write_file`'s `data` is a LocalisedString and not a string. Widening the **1,254** string argument positions to `&[u8]` would cost `e.set_name(b"chest")` at every one; `impl AsRef<[u8]>` would monomorphise each of those bodies once per caller type, into a target whose code size is Lua the game parses. **This is the one residual asymmetry with Go's `string` argument, and it is stated rather than closed.**
- **`Borrow<[u8]>`, not `Borrow<str>`.** A `BTreeMap<LuaStr, V>` is looked up with `m.get("colour".as_bytes())`. `Borrow` is infallible and Ord-consistent by contract, and a `LuaStr` holding non-UTF-8 has no `&str` to hand back.
- **No dictionary's order moved.** `LuaStr`'s `Ord` is byte-lexicographic over the same bytes `String`'s was, so `BTreeMap<LuaStr, _>` iterates exactly where `BTreeMap<String, _>` did — which matters because that order is the wire order a struct field crosses in, i.e. a desync if it moved. **Allocation count did not move either**: `from_utf8_lossy(..).into_owned()` copied too, so it is one `Vec` per string then and now.

**And every tier-2 ARGUMENT is now taken by reference**, which is the outbound half. `Value::Str` owning a `String` plus `send_udp(data: Value)` meant a guest sending a byte payload allocated an owned copy per send — garbage under `--features fk/fkgc`, a permanent leak under the default bump arena, and Go paid neither because it hands `putStr` an `unsafe.String` over its own buffer. Taking `&Value` at all **235** tier-2 argument positions lets the caller keep the tree and refill the `LuaStr` in place, so `fkipc`'s transport holds one `{"", frame}` value and allocates **nothing** per send. It also makes the generated surface agree with the hand-written one, which had taken `&Value` all along (`add_command`, `write_dyn_at`, `remote_call`). What is still allocated per send is the tier-2 WIRE — two 16-byte blocks from `fk_alloc` — and that is the missing Rust marshalling arena recorded in `guest/rust/fk`, not this.

**No member count moves**: a byte type and a reference are renderings, not members, so `census.json` is unchanged and `TestBothBackendsBindTheSameMembers` never sees it. That is also the honest limit of the census as an instrument — it diffs *what* is bound, never *how*, so a rendering defect in one backend is exactly the class it cannot report. What pins this one is **`TestABinaryStringCrossesAGeneratedEventReaderByteExact`** (`internal/guest`): all 256 byte values through `examples/dict` in **both** languages, one host stub, one set of expectations, as `<len>:<hex>` — through a tier-2 `tags` value and through `on_console_chat.message`, a plain mandatory string field, because those are two different decode paths over one wire — and then straight back OUT through `helpers.write_file`, printed by the HOST, so the send direction is pinned outside `fkipc` as well. Both the length and the bytes are asserted, because the lossy reader got both wrong and a test checking one of them could have passed. Confirmed to fail against the old reader: 256 → 512 bytes, half of them `efbfbd`. `TestBothGuestLibrariesSpeakTheSameWire` is the second pin and the proof that fkipc's workaround could go, since its 256-byte echo now runs through the generated reader in both languages.

### An array field inside a struct

`goStructs` accepted only scalar fields until this landed, so a struct holding an array was refused and took every struct containing *it* down with it — 43 members plus 28 structs, the largest single item left after tier 2.

Two things the member-level array path did not have to deal with:

- **An encoder cannot be an expression.** `storeField`/`loadField` return one expression each, which an alloc-and-loop is not, so array fields get their own branch in `emit` rather than going through them.
- **The element's offset and the field's offset are different numbers.** The field's `(ptr, count)` sits at the field's offset in the struct; the element sits at its own offset inside the one-field pair block. `LuaEntityBeltNeighboursResult` has two arrays at struct offsets 0 and 8, and a decoder that used the first field's header for both would report the same length twice — `TestArraysCrossInBothDirections` gives it 2 and 1 for exactly that reason.

`encodeAt` allocates without a bracket of its own: it is only ever reached from a member binding, and that binding's `allocMark` already covers it.

### Tier 2 on the guest side

One Go type carries every union, LocalisedString and open-ended field the API has — the same bet the Lua codec makes, and for the same reason: generating 93 tagged union types is where a generator drowns.

```go
type Value struct {
    Tag    ValueTag   // TagNil TagBool TagNumber TagString TagObject TagArray TagMap
    Bool   bool
    Number float64
    Str    string
    Object Object
    Array  []Value
    Map    []KeyValue      // NOT a Go map
}
```

`Map` is a **slice of pairs**, for two reasons that happen to agree: a `Value` holds slices and so cannot be a Go map key, and a slice keeps the order the host sent — which matters where table order is insertion order. The same reasoning refuses a dictionary *keyed* by a dynamic value (3 members): it would need this second container shape at tier 1 too.

**Allocation is bracketed, not walked.** A call can allocate on both sides — the guest for anything going out, the host for anything coming back — and a dynamic value nests arbitrarily, so a recursive free would have to mirror `writeDyn` exactly and would rot the first time either changed. Instead every binding with a block or an allocating value opens with

```go
mark := allocMark()
defer allocRelease(mark)
```

which restores the **marshalling arena**'s bump pointer to where it was. O(1), and it covers both sides however deep. **The invariant that makes it safe: an allocation meant to outlive the call must be made outside one** — or through `fk_alloc_static`, see below.

### The marshalling arena — what a host call keeps forever

**Measured, through `examples/heap`, which reads the allocator's own bump pointer rather than reasoning about the code.** `-gc=leaking` is mandatory, so every byte the ABI allocates per call is a byte in every save and every multiplayer join; the first downstream mod hit ~2.4 MB of guest heap on a test map from a ~350-call network compile.

| probe (200 calls, TinyGo, 2.0.77) | before | after |
|---|---:|---:|
| hoisted tier-2 map argument (3 entries) | 128 B/call | **0** |
| scalar argument, 4-byte block | 16 B/call | **0** |
| scalar return, 4-byte block | 16 B/call | **0** |
| string return, 44-char name, result kept | 90 B/call | **48** |
| no argument and no return block (control) | 0 | 0 |

Three findings, and only one of them was the reported one:

- **A block costs 16 bytes whatever its size.** `var a [4]byte` whose address is taken does **not** stay on the guest's stack under TinyGo — the `ptrtoint` that makes an address crossable defeats LLVM's promotion — so every argument and every return block was a permanent heap allocation. The control line is what says so: a member with neither block allocates nothing. Blocks now come from the arena as `(*[N]byte)(block(N))`, which keeps every use site (`a[0]`, `&a[0]`, `&r[4]`) reading exactly as it did, because Go indexes through an array pointer without a deref. **4160 blocks, mean 10.9 bytes, max 584.**
- **`fk_alloc` was `make([]byte, n)`**, which is the reported half: 96 bytes for a three-entry tier-2 map, and the shape a mass-builder calls in a loop.
- **The 48 bytes that remain are the caller's, not the ABI's**, and the report is wrong to group them in. A returned Go `string` (or `[]T`) is a value the guest owns and reads after the call returns; putting it in an arena would be a use-after-free. Only `-gc=leaking` itself addresses that, and the flag is load-bearing. Discarding the result makes the copy dead and the probe reads 0 — which is how this line first measured wrong, and why the probe keeps it.

The arena is **chunked, never reallocated**: a pointer handed out earlier in the same call must stay valid, so running out of room moves to the next chunk rather than growing the current one, and a release undoes that too. Steady state after the first call is zero allocation. Blocks come back **zeroed**, because a plain `[N]byte` local was and an absent optional writes its presence byte while leaving the value slot alone.

It removed one half of the `--persist=packed` pathology the report describes and **not the other**. The dirty record was a min/max **byte range**; an allocator walking upward pushes the high end further on every call, and an arena reuses the same addresses so the range stops growing. What an arena could not fix is that the range was a *span*: the string scratch region is a static and a returned Go string is on the heap, so one call touches both ends of the address space and the flush repacked everything between them. **That half is fixed separately, by the dirty-page SET** — see the persistence section of `CLAUDE.md`. The table below is kept as first measured, with the after-column beside it, because the two together are what say the mechanism was correctly identified rather than merely plausible.

### What `--persist=packed` costs per host call — measured twice, 7× apart

`TestWhatAHostCallCostsThroughARealGuest` (`internal/guest`, guest compiled at the default `-opt`, driven through the real `control.lua` under `lua52f`, 2000 dispatches differenced against the same script at zero):

| per dispatch | `table` | `packed`, byte range | `packed`, page set |
|---|---:|---:|---:|
| dispatch, no host call (the baseline) | 529 ns | 549 ns | 180 ns |
| call with no argument or return block | +427 ns | +557 ns | +956 ns |
| scalar in, scalar out | +1.5 µs | **+134 µs** | +127 µs |
| string return, 44 B | +3.8 µs | **+678 µs** | **+172 µs** |
| tier-2 map argument, hoisted | **+14.3 µs** | +150 µs | +141 µs |

**Only the string-return row moves, and that is the point.** It is the only leg whose call touches a static (the scratch region) and the heap (the Go string sink) — the exact span shape — so it is the only one the page set can help, and it drops 3.9×. The others were never span-bound: they scatter over pages they genuinely write. Read the sub-microsecond rows as noise between columns measured in different sessions; the µs rows are the signal.

Two things fall out, and neither was visible before:

- **The ~12 µs the downstream report measured is the TIER-2 PATH**, not generic dispatch. A bare call is 427 ns — within noise of the 513 ns the host-side `abicost_test` measures for the same thing, so the guest adds essentially nothing to dispatch itself. `writeDyn` on the guest side plus `read_dyn` on the host side is the whole of it, and `create_entity` is exactly that shape, which is why a mass-builder feels it and an event-time-only guest does not.
- **`packed`'s cost was a SPAN, not a byte count**, which is why one call that touched the scratch region (a static, low) and a Go string (the heap, high) repacked everything between them. The dirty **page set** that replaced the min/max range is what closed it; `packed` now costs ~40 µs per page ACTUALLY WRITTEN, so a call that scatters genuinely still pays per page. The `auto` threshold's heap-size proxy still does not see any of this.

**And "100×" is this harness's guest, not a law.** The first downstream mod measured the same question **in real Factorio**, after the arena landed: a 4×4 network recompile went 41 ms → **7.4 ms** and its save delta 128 KB → **18.7 KB**, which is about **21 µs per host call** on a `create_entity` shape against the 150 µs this table reports for the same shape. The mechanism explains the disagreement rather than resolving it: the harness guest assigns a returned name into a package-level Go string and reads a static scratch region in the same probe, so its span is nearly the whole address space, while a mass-builder's is not. **Do not quote either figure as universal, and prefer the in-game one when telling a mod author what a mode costs.** Downstream picked `table` (4.4 ms against 7.4 ms on the same recompile) and then found the bill for it elsewhere: the word table gave Lua's collector a 27.8 ms worst tick on an idle map. The page set is what makes the mode choice reopenable, and the in-game numbers on the other side of it are downstream's to take.

### Where the tier-2 DECODE's time goes — it is the STRINGS

Rounds 1 through 3 named `read_dyn` as the actionable target and left it there. Ablated (`read_dyn` over one value, 20k iterations, differenced against a zero run):

| what is decoded | before | after |
|---|---:|---:|
| one number | 86 ns | 86 ns *(control)* |
| one 14-char string | 1.84–1.91 µs | **1.03 µs** |
| one 148-char string | **13.9 µs** | **2.08 µs** |
| a create_entity-shaped map (6 keys, nested map, 2 string values) | 14.1–14.6 µs | 11.0–14.2 µs |

**A bare number costs 86 ns and a 14-byte string costs twenty times that**, so the tag dispatch was never the story — string decoding is essentially the whole of tier 2, keys included.

`fk_str` was **one `string.char` call and one table slot per BYTE**, while its mirror `fk_wstr` had been batched to four words per `string.unpack` a milestone earlier. That is the same load/store asymmetry this project already recorded once, when the reason sub-word accesses stay function calls turned out to have been measured for **stores** and inherited by loads — and it is worth noticing that writing down the lesson did not prevent the next instance of it. `fk_str` now reads four words per `string.pack`, the exact inverse: **94 ns/byte → 14 ns/byte** at 148 bytes, against `fk_wstr`'s 6.44 ns/byte in the other direction.

Two honest caveats, both measured:

- **A short string gains nothing, and below 8 bytes it pays.** The head and tail eat the whole string and two loop preambles are added for free. Tier 2 is *full* of short strings — they are the keys — so there is an explicit `n < 8` fast path, without which a map of six one-character keys went 4.15 → 4.54 µs.
- **The create_entity-shaped map shows no reliable change**, because all ten of its strings are 1–14 bytes and most are under the fast path's threshold. Two of three paired runs came back at 11.0 µs against 14.1 and one at 14.2; on this machine that is not a number to quote. **The win is real and it is for long strings** — blueprint strings, localised strings, descriptions — not for the mass-builder shape the downstream report measured.

`read_dyn` itself also lost its `M.DYN_*` hash lookups (module locals, with the `M.*` aliases kept beside them as the cross-language contract) and its branches are now in measured frequency order rather than tag order. **Paired A/B: 0.974×, at the noise floor.** It is kept as a simplification, not quoted as a win.

*The change is gated for correctness rather than for speed: `TestReadStringIsExactAtEveryAlignmentAndLength` round-trips every length 0–40 at every alignment 0–7 through `fk_wstr` and back, because a head/body/tail split is wrong at boundaries or not at all. Zeroing the head makes it fail with `bad argument #2 to 'pack_'`.*

**The residual is the DECODE, and hoisting the guest-side encode buys nothing.** Round 1's advice was to hoist a guest's tier-2 argument construction; downstream did it and the recompile did not move, because its buffers were already reused. Its ~12.6 µs/call matches the +14.3 µs above, so what is left of the tier-2 path is `read_dyn` on the host side. That is the same eager-decode machinery event marshalling goes through, and it is where a measurable win is.

### `fk_alloc_static`, and an invariant that used to hold by accident

`fk_mod.lua` caches an event scratch buffer **per nesting level**, and every level past the first is allocated from inside a *nested* dispatch — that is, while an outer binding's bracket is open. An arena buffer taken there is reclaimed the moment that binding returns and then handed to something else, while `control.lua` goes on writing event data into it.

Nothing was wrong with the old code. It was correct only because `-gc=leaking` never reclaims, so dropping a pin was harmless — **making the reclaim real turns that accident into a requirement**, so the requirement gets its own export: `fk_alloc_static(n)`, which never comes out of the arena. Go keeps those buffers in a `kept` list; Rust's `fk_alloc` was already a permanent leak, so there the two are the same body and the export exists to give `control.lua` one name.

Adding it means editing `runtime/lua/fk_mod.lua` *and* `factorio.Hooks`, in both directions — the usual guard.

**AND THE CACHE THAT HOLDS THE RESULT LIVES IN `storage`, NOT IN A LUA LOCAL.** This is the half that was missing until P12, and it is a general statement about this export rather than a note about one caller: **`fk_alloc_static` hands back a GUEST HEAP ADDRESS, and a guest heap survives a load — so anything caching one has to survive the load too, or the cache and the thing it describes come apart.** A load re-executes `control.lua`, so a Lua local is rebuilt empty while the heap comes back from the save with the buffer already in it; the next dispatch then allocated a **second** buffer beside the first. Per level, per load: `n` bytes of guest heap into the save, one more entry pinned in `kept` — so the pin list's own bound, "the deepest nesting the mod ever performs and then it stops", held only *within a session* — and every later allocation landing that much further up than on an instance that never reloaded, which under `--persist=table` is two peers disagreeing about `storage.fk_mem`.

Both caches (`scratch` for events, `callbuf` for the callback trampolines) are aliased into **`storage.fk_bufs`**, published by `state_init` where the heap is fresh and adopted back by `state_load` under the `same_build()` gate — the same gate, and for the same reason, as the heap and the persistent handle space. The mirror is *also* written at the allocation itself, which is the only replicated place such a write can go (`on_load` is read-only with respect to `storage`) and what lets a save written by an older runtime heal on its first load.

**And it records the SIZE beside each address** (`.evn`, `.calln`), because an address does not carry its own length and the length is not a constant of the guest: `API.event_scratch` is the largest subscribed event's payload, computed from the **packaged** event table, so two packages of one wasm against two `--api` pins can disagree about it; `fk_migrate_adopt` hands over another build's heap outright. A mirror recorded at a size this build does not ask for is refused and the allocation happens again. Reusing one allocated smaller than what `write_struct` is about to put in it would be a silent overwrite of whatever the guest allocated next, which is a worse failure than the leak this fixes.

**The two-pins half of that has since been closed one level up, and the size guard stays anyway.** The build stamp used to hash the module and nothing else, so those two packages really did share an identity and `same_build()` adopted across them; it folds the resolved `--api` pin in as of 2026-08-07, so a cross-pin repackage now takes the rebuild path — which is what shifted member, event and define ids demand, `event_scratch` being only the symptom that could be pointed at. See `agents/guests.md`, "Recompiling, and `fk_migrate`". What the stamp cannot cover is `fk_migrate_adopt`, whose entire purpose is to hand over a DIFFERENT build's heap on purpose; the size test is what covers that, and it is also the cheaper of the two answers for a save written by an older runtime.

**A future caller of `fk_alloc_static` inherits all of this**: reaching for it means the address outlives the call, and an address that outlives the call outlives the save. *Enforced by `TestTheEventBufferIsAllocatedOncePerHeapAndNotOncePerLoad`.*

### The outermost dispatch brackets the arena — **built**

**Everything above this heading is about a GUEST-initiated call, and the whole of it stopped at the boundary.** `allocMark`/`allocRelease` are called by the generated binding, so `TestAHostCallKeepsNoHeap` reads 0. A **host-initiated** dispatch — an event Factorio raised, a console command somebody typed, a remote method another mod called — has no binding to take that bracket, because nothing on the guest side made the call. The host still allocates: `write_field`'s `K_STR` path and `dyn_alloc` write into the 4 KiB string scratch region when the value fits and **fall back to `fk_alloc` when it does not**, and `fk_free` is a documented no-op, so every one of those advanced the arena's bump pointer permanently.

Measured through `guest/go/examples/eventheap`, which reads the allocator's own bump pointer the way `examples/heap` does, over 50 dispatches each carrying a 5,000-byte string:

| host-initiated dispatch, 5 KB payload | before | after |
|---|---:|---:|
| `on_console_chat`, one string field | **16,442 B/dispatch** | **0** |
| a remote method, one string argument | **16,444 B/dispatch** | **0** |

**Read the before-column as "the growth of one driver iteration", not as one leg's own cost.** The two legs interleave — the driver raises the event and then calls the remote method — and each measures the whole loop, so both report the same ~16 KB and the per-dispatch share is about **8 KiB**. That is still larger than the 5,000-byte payload, because a chunk that cannot hold the next request is **abandoned rather than split**: what keeps a pointer handed out earlier in the same call valid is that a chunk never moves, so an oversize request takes a fresh one and the remainder of the old one is gone. The after-column is 0 either way, which is what the gate is on.

**It is worse under the collector, not better.** The arena's chunks hang off a package-level `arena [][]byte`, which is a root, so a chunk it has moved past can never be reclaimed by anything — where under `-gc=leaking` it is at least the same kind of leak as everything else in that arm.

**Nothing in this repo's corpus carries a large string in an event**, which is the whole of why it went four milestones unseen — the same shape as F1, F2 and R6 in the ports round, and the same lesson: *a corpus written by the compiler's authors tests the compiler's authors' habits.* It graduates from latent to blocking the moment a feature exists whose purpose is carrying payloads.

**The fix is the rule the string scratch region already follows.** `enter_outermost` takes a mark at depth 0 and `leave_outermost` releases it when that dispatch returns, and depth 0 is the only sound place for the same reason it is the only place the region may go back to zero: nothing the host wrote is still being read there. What makes it sound rather than merely convenient is that **everything crossing inbound is copied out of arena memory before the handler returns** — `getStr` makes a Go `string`, `readDyn` makes Go slices, Rust's `get_str` makes a `String` — which is `CLAUDE.md`'s safe-point precondition read in the other direction. A guest holding a `(ptr, len)` past its dispatch was already illegal; this makes it fail rather than merely being undefined.

Four details are load-bearing:

- **`fk_arena_mark`/`fk_arena_release` are OPTIONAL, and `fk_mod.lua` feature-detects the pair.** A guest built against an older substrate exports neither and gets precisely the behaviour it had, leak included. Requiring them would turn every mod already packaged into one that stops loading, over a leak that only bites a payload larger than 4 KiB. They are in `Hooks` — the usual both-directions guard — as non-`Event` entries beside `fk_scratch_base`, so they are wired and reported and never make a guest look inert.
- **One slot, and a token that says whether it was filled.** `allocState` is two ints and a wasm export returns one value, so the state stays guest-side; there is exactly one slot because both entry paths test `depth == 0` before marking, so a second mark before the first release cannot happen. Token 0 is "no mark", which is what a host that never called `fk_arena_mark` hands back, and release ignores it — rewinding to a slot nobody filled is a rewind to zero, which is somebody's live bytes. What would break the single slot is a mark at depth > 0, and the token is what makes that a change to both sides rather than a silent overwrite.
- **Neither export writes when it has nothing to say, and that IS the cost story.** A package-level var is *linear memory*, so a store dirties a page — and under `--persist=packed` a page nothing else touched is ~40 µs of repack at the end of the dispatch. Measured on a do-nothing dispatch, the case with no other write to hide behind: an unguarded save cost **614 ns → 51.4 µs**, while every leg that allocates a block was flat, because `arenaAlloc` writes those same two words anyway. So the save is skipped when the state is already the saved one and the restore when nothing moved — the `DPLO`/`DPHI` trick from `MEMPACK.mark`, against the same cost. It is caching rather than semantics: the depth-0 arena state is invariant *because* everything that allocates from the arena gives it back.
- **The arena goes back BEFORE `dispatch_done`**, which is the `pcall`'s argument applied to a second piece of state: nothing in `dispatch_done` reads guest memory the host wrote, and going first means a raise in there cannot strand the arena at a mark nothing will ever release.

**What it costs, paired, `TestWhatAHostCallCostsThroughARealGuest` on a quiet machine:**

| per OUTERMOST dispatch | no bracket | bracket | delta |
|---|---:|---:|---:|
| `--persist=table`, dispatch making no host call | 454 ns | 822 ns | **+366 ns** |
| `--persist=packed`, same | 499 ns | 862 ns | **+357 ns** |
| every leg that allocates a block, either mode | — | — | **flat** |

**The whole of it is the two guest calls, and the two modes agreeing is what says so** — `call, no blocks` sits +671 ns over the dispatch with the bracket and +668 ns without it, in both columns, so nothing moved except the fixed entry cost. It is 1.8× on a `fk_on_tick` that does nothing, which is a real ratio over a small number: 0.37 µs against a 16.67 ms frame. Quote it as a per-dispatch constant, not as a percentage of anything.

**And this file's one real ABI performance gate would NOT have caught the unguarded form**, which is worth knowing before trusting it with the next one. `TestWhatAHostCallCostsThroughARealGuest` gates the tier-2 leg at **3× the bare dispatch**, on the reasoning that the measured margin is 124× under packed and a 3× bar therefore cannot fire on a busy machine. A 51 µs baseline takes that ratio to **3.25×** — still green, by 8%. The bar is right about noise and blind to a baseline that grew two orders of magnitude, because everything it measures is *relative to* the baseline. What found this was reading the number, not a gate.

*Enforced by `TestAHostInitiatedDispatchKeepsNoHeap`, which drives a real TinyGo guest through the verbatim `fk_mod.lua` and `fk_abi.lua` and gates both legs at **zero** — a ratio would pass a variant that kept a little, and "a little, forever" is the complaint. Its companion `TestAGuestWithoutTheArenaBracketStillDispatches` is the optional half.*

**And the encode had to move inside the dispatch first, which was a second defect.** `subscribe`'s closure ran `H.write_struct` *before* entering `dispatch`, so the payload's string fields were written at the bottom of the scratch region and `dispatch` then reset that region out from under them — the handler's own first host call wrote its returned string over an event field the handler still held a pointer to. `run_callback`'s header has said for a milestone that the reset is *"correct for an event, whose payload it encodes AFTER raising the depth"*; that sentence described the shape it has now and described nothing that existed. It was invisible because every generated decoder copies **eagerly** — `ReadOnConsoleChat` turns the field into a Go string on the first line of the handler — so the clobber landed on bytes nobody read again, and a guest reading lazily from the pointer it was handed, which is what this file's own re-entrancy note says a handler does, got somebody else's data with no error anywhere. It is also what makes the bracket placeable at all: a mark taken inside `dispatch` is taken *after* the one allocation it exists to reclaim. *Enforced by `TestAnEventsStringFieldSurvivesTheHandlersOwnHostCall`, which asserts at the BYTE through a wat guest that reads the pointer rather than the value, and pins the message's address so the two spans provably overlap.*

**Rust exports neither, deliberately, and it is not the same defect there.** `guest/rust/fk`'s `fk_alloc` is the **global** allocator, not an arena: under `--features fkgc` the block is ordinary garbage — nothing the collector can see refers to it once the dispatch returns — so the next collection reclaims it and growth is bounded; without the feature it is the same never-reclaimed allocation as every other allocation in an arm whose entire contract is that nothing is reclaimed. Exporting a mark/release pair a bump allocator cannot honour would be a claim the host would believe and the guest could not keep, and rewinding the *global* bump would free what the handler itself allocated. Closing the asymmetry means giving that crate a marshalling arena of its own — which is also the thing that would finally make its generated `AllocMark` mean something — and that is a change to what every Rust host call costs rather than a bug fix. **The residual, stated: a Rust guest at `--gc=leaking` still keeps its inbound payloads, exactly as it already keeps every string a host call returns.**

**A nil inside a Lua sequence is the end of that sequence.** A guest sending `[true, nil]` gets `[true]` on the host, because Lua cannot hold the hole. Tier 2 does not pretend otherwise; a guest that needs an absent element to survive encodes it as something Lua can hold. `TestArraysCrossInBothDirections` asserts the truncation rather than working around it.

### The per-class gate

M7's exit criterion is "the generated dispatcher exercised against at least one real method per class", and the other dispatch tests do not meet it: they pick two members by name, which says the chain works for those two. A class whose members all failed to resolve would look exactly like a class nobody tested.

`TestEveryClassDispatchesAtLeastOneMember` picks one member per class **by shape rather than by name** — a read whose single return is a bool, number or string — so a version bump cannot break it on a rename. It covers **147 of 148**; `LuaCombinatorControlBehavior` has no simply-shaped read and the test names it rather than quietly skipping it.

It found a hole in the harness on its first run: 72 of the probes came back `ERR_BAD_ARGS`, because a string RETURN is written through the guest's allocator and none of the marshalling tests had ever bound one. The host refusing rather than guessing is correct — but it meant no test had exercised a string coming back until this one did.

### Parsing is not compiling

`TestGeneratedGoParses` accepted `*p.h`, which parses fine and means `*(p.h)` — dereferencing a `uint32` field instead of the pointer. Seventy members were broken and the parse test was green. `TestGeneratedBindingsCompile` in `internal/guest` builds `examples/api` with TinyGo, which is the gate that actually holds for generated Go. It was verified to fail against a deliberately broken binding.

### The package is complete on purpose

Emitting every member makes the file 555 KB, and **TinyGo builds it in 1.2 seconds** because dead-code elimination drops what a guest never calls. Better still, the surviving `fk.call` sites are exactly what the member-id scan then finds — so `examples/api`, which calls three members, compiles to three call sites and ships a **714-byte** member table pruned from 3456. Generating a subset up front would mean guessing what the author will write.

### `valid` is not universal, and reading it raises

**15 of 148 classes have no `valid` attribute, including six of the nine globals.** Reading a key a LuaObject does not have does *not* return nil — Factorio's `__index` raises: `LuaGameScript doesn't contain key valid.` A blanket probe in `get()` therefore crashed every call on `game`, and every host-side test passed because a stub is a plain table where a missing key *is* nil. Only running in the game found it.

The fact now travels **per member**, set by the generator from the API description, so the check is free where it does not apply and correct where it does — with no `pcall` on the hot path.

---

## The generator — **Lua side built, plus the Go guest bindings**

`internal/factorio/gen.go` walks the API model, produces one member entry per (class, member, kind) with laid-out blocks, and emits `fk_api_gen.lua`. Not built: the guest-side C/Go bindings, and the `fklua gen-bindings` command that would write the file out.

`TestGeneratedTableLoadsAndDispatches` runs the whole chain — real `runtime-api.json`, generator, Lua source, `lua52f`, `bind_members`, a dispatched call with a u32 in and a boolean out. Everything before it was tested against hand-written descriptors.

Each entry carries `argsize`/`retsize` because the guest needs them to reserve a block before calling, and recomputing that at runtime would repeat work already done at generate time.

### A mod ships the members it calls, not the API

The full table is **~890 KB for 4257 members** at the 2.0.77 pin. Putting that in every mod would make a guest that calls five members carry more than four thousand it never touches — in every save, in every download, and in Factorio's parse time at load. Measured: **913,566 bytes for the whole API and its event table, 859,892 for the member table alone, and 1,359 for five members.** The same three numbers at the 2.1.14 pin are **1,100,428 / 1,044,482 / 1,902** — the table scales with the description, so quote the pin with the number.

**The five-member figure said 1,288 until the fkipc closeout and the origin of that number is unknown**, which is worth one sentence rather than a quiet edit. It is not any committed pin measured the way `TestAPrunedTableIsTiny` measures it — the same five member ids render as 1,359 B at 2.0.77, 1,624 at 2.1.12 and 1,902 at 2.1.14, and the first of those is the current default rather than a coincidence — and it is not a units or an events-table difference either. The two halves of the old sentence were also measured through different functions, which is the likelier shape of the mistake: the whole-API number is `LuaSourceWith` (members plus events) and the five-member one was `LuaSource`. Both numbers above now come from that one test's own log line, which is where a reader should take them from.

`UsedMembers` finds them. The compiler already parses the guest, so it can *see* which ids reach `fk.call` rather than being told: the binding generator emits each one as a constant, and the scan collects them.

**IDs are preserved, never renumbered.** The guest baked them in when its bindings were generated, so a pruned table is *sparse* — `members[1729]` stays 1729. Closing the gaps would point every call at the wrong member, which is the worst imaginable way to save a few kilobytes.

The scan is deliberately shallow — an `i32.const` feeding the member operand and nothing cleverer — and **anything it cannot prove makes it give up on pruning entirely**. The alternative to "provably constant" is not "probably fine", it is a member missing at runtime on whichever path computed one. A guest that computes an id, or writes one on two sides of a branch, gets the whole table: bigger, never broken.

**Coverage is 89.5%** — 3456 of 3862 entries, as first measured; the shape of the tail below is what that number is kept for. (More entries than the 3283 documented members, because a read/write attribute yields a GET and a SET — and since the string predicate landed, a plain-string attribute yields an EQ too.)

| skipped | cause |
|---:|---|
| 337 | union or recursive type — **tier 2** |
| 65 | variant parameter groups — hand-written |
| 2 | untyped `table` |
| 1 | callback parameter |
| 1 | variadic parameter |

**Tier 2 is the whole remaining story and nothing else is close.** That is the number to watch: it says which missing piece buys coverage.

### A skipped member is skipped, never faked

A member whose signature cannot be expressed is omitted with a reason. A guest author who finds a binding missing can see why in the report; one who finds a binding that exists and returns nonsense cannot. For the same reason, **one unexpressible field skips the whole struct** rather than being quietly dropped — a struct missing a field is a wrong value the guest cannot detect.

### Canonical unions: what took coverage from 78% to 89%

52 concepts are structural unions, and the most-referenced all fall into two shapes the API measurably repeats:

```
MapPosition = table{x,y}      | tuple[double, double]      152 references
Color       = table{r,g,b,a}  | tuple[float x4]             95
ForceID     = string | uint8  | LuaForce                    69
```

- **One table plus array shorthands → the table.** It is what a read returns and a write accepts either, so carrying only the table costs a guest nothing.
- **One class plus scalar identifiers → the handle.** "A force, or its name, or its index." A read returns the object, so the handle is the form that must work.

**What that costs, because it is not free:** under the second rule a guest can pass a force only as a handle, never as a name — reaching one by name means finding the `LuaForce` first. An ergonomic loss the generated bindings will have to paper over, not a correctness one, and tier 2 removes it.

`LocalisedString` is genuinely recursive and stays refused. No fixed layout holds it.

### Two things about member ids

They are **dense indices over the generated set, and need no stability across Factorio versions**: the member table is generated from the same API version the guest was compiled against and ships in the same mod, so the pair always matches. What must degrade gracefully is a member missing from the *running* game, which is the `ERR_NO_MEMBER` path.

**Classes inherit, and an inherited member appears in neither the child's method list nor its attribute list.** `LuaEntity` gets `position` from `LuaControl` and has no entry of its own. Dispatch does not care — it is name-based and the handle decides the object — so one entry per *declaring* class is enough, and a subclass's bindings reference the parent's ids.

### The three tiers

Sized by the census above:

| tier | covers | mechanism |
|---|---|---|
| 1 — static structs | 219 events (mean 4.8 fields), 283 table-shaped concepts | generated fixed layout + encoder/decoder pair. ~90% of traffic. |
| 2 — dynamic codec | 93 unions, `LocalisedString`, dictionaries, `LuaCustomTable` | one tagged encoder/decoder, version-skew tolerant |
| 3 — trivial | 41 string enums, 60 defines | plain i32 |

**Defines are resolved through a generated table at load, never hardcoded.** Their numeric values are Factorio's and are not stable across versions. See `fk.define` below: since 2026-08-01 that is a generated table on both sides rather than a rule guests were asked to follow by hand.

Strings cross as ptr+len UTF-8. Guests export `fk_alloc(n)`/`fk_free(ptr)` for out-parameters — the only guest-side requirement beyond the memory export.

Tier 2 exists because generating 93 nested-optional tagged unions is where a generator drowns; one hand-written codec is smaller and survives version skew better than generated code that has to be regenerated to tolerate it.

---

## Event dispatch — **built**

`fk.subscribe(id)` registers; the host encodes the event data eagerly into a scratch buffer and calls `fk_on_event(id, ptr)`. **210 of 219 events** are tier-1 expressible — they are flat, mean 4.8 fields — which is exactly why eager encoding is right here: writing the whole struct costs less than a host call per field for anything but a handler that reads one field and returns.

The scratch buffer is **128 bytes for all 210 events** and is allocated once, so a dispatch allocates nothing.

Event ids are per-build like member ids, and pruned by the same constant scan over `fk.subscribe`. The example subscribes to two and ships two, not 210.

**`defines.events` values are Factorio's and are not stable**, so the generated table carries the *name* and `control.lua` resolves `defines.events[name]` at load. An event this version does not have is logged and skipped; the mod runs on.

### One dispatcher per event, and why

**Factorio allows one handler per event per mod, and `script.on_event` REPLACES rather than adds.** Two things here want `on_tick` — the legacy `fk_on_tick` hook and a guest that subscribes to it — and whichever registered last silently won. The symptom was a subscription that reported success and never fired.

Registration therefore goes through one dispatcher per event holding a list — and since `fk.defer`, a **removable** one: `off_event` drops an entry and tears the dispatcher down entirely when the list empties, so a one-shot handler costs nothing once it has run. The walk is by identity (`if list[i] == fn then i = i + 1`) rather than `for i = 1, #list`, because that form evaluates `#list` once and a handler that removes itself would make the final index a nil and call it.

### Event filters — a tier-2 value, decoded once at subscribe time

`fk.subscribe(id, filterp)` takes a pointer to a **tier-2 dynamic value** in guest memory, or 0. Factorio's filter list is an array of string-keyed tables, which is `DYN_ARR` of `DYN_MAP` — so nothing new crosses the boundary, the guest builds it with the same `writeDyn` every tier-2 argument goes through, and `control.lua` hands `read_dyn`'s output straight to `script.on_event`.

**Why it is a value and not a generated constant.** The round-1 sketch said filters could travel in the generated table beside the event name, the way member ids do. They cannot: the prototype names a mod filters on are *the mod's own*, and no amount of scanning `runtime-api.json` would ever find them. What is constant in the guest is the *event id*, which the scan already proves; the filter is data.

It is decoded **once, at subscribe time** — which happens during `_initialize` — so this is a load-time cost and never a per-event one. That is the whole point: the engine applies the list in C++ before the handler runs, so a guest caring about one prototype stops paying a dispatch plus a host call plus a string crossing for every build event on the map.

Three rules, each of which is a way this could have been quietly wrong:

- **Two subscriptions to one event share a registration, so their filters are UNION-ed, and an unfiltered one widens the pair to unfiltered.** `script.on_event` takes one list per registration and this runtime keeps one dispatcher per event holding a list of handlers. The union is the merge that cannot *lose* an event — a filter list is OR-ed term by term, so appending is exactly the union. The reverse (letting the filtered subscription win) would silently stop delivering to the unfiltered handler. *Enforced by `TestAnUnfilteredSubscriptionWidensAFilteredOne`.*
- **A filter for an event that takes none is logged and dropped, not fatal.** `script.on_event` raises for those, and raising during `_initialize` takes the whole mod down at load. Running unfiltered with a line in the log is the widening direction again.
- **Zero stays unfiltered, and a guest compiled before this existed declares the import with one parameter.** Lua hands the second one a nil, which reads the same. Nothing about the wire changed for an existing mod.

Guest side: `fkapi.Subscribe(id)` and `fkapi.SubscribeFiltered(id, filters...)`, with `fkapi.NameFilter("a", "b")` for the commonest list. **The Go bindings had no `Subscribe` at all** before this — every Go guest hand-declared `//go:wasmimport fk subscribe`, which the Rust bindings never made anyone do. Both wrappers were measured to inline under TinyGo `-opt=2`, so the constant scan still proves the id and a mod still ships 2 event descriptors rather than 219; *enforced by `TestTheEventIdSurvivesTheGeneratedSubscribeWrapper`*, because if that ever stopped being true nothing else would say so.

### The event FIELD MASK — declared once at subscribe time

`fk.subscribe(id, filterp, mask)` takes a third argument: a bitmask over the event's field indices naming the fields this guest never reads. Zero is the whole payload, which is what a guest compiled before this existed sends by not sending anything.

**The Rust binding declared the import with TWO parameters until 2026-08-03**, so no Rust guest could decline a field however expensive it was — and nothing said so, because a wasm import is a Lua function here and the missing argument simply arrived as `nil`, which `subscribe` reads as mask 0. It is the shape of gap this whole round is about: a feature landed on one backend, and the other one kept working while silently offering less. `subscribe_masked` and `subscribe_filtered_masked` exist there now, with the `SKIP_*` constants beside the `EVENT_*` ones.

**It exists because the encode is eager and complete.** That is right for a flat payload — the mean event has 4.8 fields and a host call per field would cost more — and wrong for the few that carry a container. `on_undo_applied`'s `actions` is an array of tier-2 values, so `write_dyn` deep-copies an undo step's whole `BlueprintEntity` list before a handler that wants one `uint32` is entered. Measured through the real dispatch protocol, 2 entities per action:

| `actions` | unmasked | masked |
|---:|---:|---:|
| 20 | 725 µs/dispatch | **1.9 µs** |
| 200 | 7.49 ms/dispatch | **2.7 µs** |

The cost is linear in the array and the masked leg is flat, which is the honest way to read it: **the mask removes exactly what the field cost**, so what it is worth is a property of the payload rather than of the mask. Do not quote a ratio. *(`TestWhatAnEventFieldMaskIsWorth`.)*

**Three designs, and the layout is what rules two of them out.** A guest reads fields at offsets compiled into it, so nothing here may move a field.

- *Prune by what the generated readers touch* — cannot work alone. `ReadOnUndoApplied` reads every field, and a guest that skips one does so at a hand offset no scan can see. It needs guest opt-in either way.
- *A mask on `fk.subscribe`* — the natural fit, because filters already travel this way and are already resolved once at subscribe time. **Built.**
- *Lazy fields behind an on-demand host call* — the cleanest semantics and the largest change: a new import, generated accessors, and the same re-entrancy rule that has now bitten the scratch buffer and the transient handle space.

**A masked field is WRITTEN AS EMPTY, not skipped, and that is the whole correctness argument.** The scratch buffer is allocated once and reused for every dispatch at that level, so leaving the bytes alone shows the guest whatever the *previous* event put there — a presence byte still reading 1 over a pointer since reclaimed. `write_struct` zeroes the header instead: two stores against a deep copy. *Enforced by `TestAMaskedFieldReadsAsEmptyNotStale`, which writes the block twice into one address and reports `masked 9 3 hi` — the previous dispatch's data — when the zeroing is removed.*

**Only OPTIONAL and CONTAINER fields are maskable, and the restriction is what makes a wrong mask safe.** A masked optional reads as **absent** (presence byte 0) and a masked container as **empty** (`ptr`/`count` = 0, 0); both are readings every generated decoder already produces. A mandatory scalar has no such reading, so masking one would hand the guest a zero indistinguishable from a real value — the silent-wrong-value class this ABI refuses everywhere. Lying about the mask therefore yields *empty*, never garbage.

The rule is enforced twice on purpose. `mask_fields` refuses the bit at subscribe time, logs which field it refused, and encodes it anyway — the widening direction, the same one an unreadable filter takes. And the generator emits a `Skip<Event><Field>` constant **only for a maskable field**, so the rule is discoverable at compile time rather than as a line in the log. Both, because a guest can compute a mask and no constant can stop it. *Enforced by `TestAMandatoryScalarIsRefusedByTheFieldMask` and `TestAMaskOverAMandatoryFieldIsLoggedAndIgnored`.*

Two smaller decisions:

- **Masks are not merged across subscriptions, unlike filters.** Filters share one `script.on_event` registration and therefore have to be union-ed; a mask belongs to one handler's closure and says only what *that* handler will not look at. Nothing is shared, so nothing needs merging.
- **The bit index is the LAID-OUT field order**, which is why the constants are generated beside the layout rather than written by hand — the same reason the event ids are. A hand-written `1 << 1` drifts the moment the API pin adds a field, and drifts toward masking the wrong one.

Guest side: `fkapi.SubscribeMasked(id, skip)` and `fkapi.SubscribeFilteredMasked(id, skip, filters...)`. The mask is a plain `uint32` rather than a tier-2 value: the widest event payload in 2.0.77 has **13 fields**, so nothing needs allocating, and unlike a filter list there is nothing here the guest computes from its own data. *`TestTheEventIdSurvivesTheGeneratedSubscribeWrapper` still holds, so the constant scan still prunes 219 event descriptors to 2.*

### `fk.define` — defines, resolved by name at load

`fk.define(id) -> i32` is the fifth `fk` import. It answers the last thing a guest still had to hardcode: `defines.direction.east` was a hand-written `4`, in a project whose own ABI doc says defines are "resolved through a generated table at load, never hardcoded."

**The downstream report's premise for how to fix it was wrong, and the reason is the whole design.** It asked for constants baked from the API pin. `runtime-api.json` carries define **names** and an `order` and **not their values** — so there is nothing to bake *from*, and there should not be: a define's number is Factorio's own and moves between versions. The shape that works is the one `defines.events` has always used, generalised: the generated table carries the dotted **path**, `control.lua` resolves it against the running game at load, and the guest holds a per-build **id**.

| | |
|---|---:|
| top-level groups | 60 |
| groups including nested | 147 |
| generated values | **1137** |
| `defines.events`, excluded | 219 |

**Scope: every group except `defines.events`.** That one already has a resolved table of its own, and its numbers are not what `fk.subscribe` takes — offering a guest both spellings of "on_tick" would be a trap dressed as a convenience. Both counts are in the census (`define_values`, `go_define_accessors`).

**Why a read is an import call rather than a table in guest memory.** The memory-resident form is faster per read and was the round-2 sketch. It is also **unprunable**: the whole set is ~45 KB of paths, a guest naming four directions has no business shipping the other 1133, and the only pruning machinery this compiler has is `usedIDs` — a scan for a constant reaching an **import**. So `UsedDefines` is `UsedMembers` with a different import name and the same "cannot prove it, ship it all" rule, and the per-read cost is bought back on the guest side instead:

```go
func DefinesDirectionEast() uint32 {
	if !dok133 {
		d133, dok133 = hostDefine(133), true
	}
	return d133
}
```

**The laziness is what makes the two halves work together.** Caching in a package-level initialiser would run the host call whether or not the guest ever reads the define — so every mod would name every id and the scan would prune nothing. Caching *inside* the accessor keeps the call site inside a function TinyGo deletes when nobody calls it. One host call per define for the life of the mod, and a table the size of what the guest actually names.

Two failure readings, and they are deliberately different:

- **A path the table has and this Factorio does not** is logged once, at load, and reads 0. Same direction as a missing event: say so, keep running.
- **An id outside this build's table** reads 0 with nothing to say, which is why ids are **1-based**. A guest built against a different table gets a diagnosable zero rather than a plausible value from a neighbouring group.

**And 1137 Rust accessors since 2026-08-03**, the same ids, the same laziness, the same literal reaching the import. Three ports declared `fk.define` by hand and re-derived the ids by reading `GenerateDefines` in the **Go** source and cross-checking against a different backend's committed output — for `defines.direction.north`. One of them supplied the argument that makes this more than ergonomics: `defines.train_state` was **renumbered between 1.1 and 2.0**, so a transcribed number is not a number that might be wrong, it is a number that WAS wrong and said nothing.

The Rust accessor caches in **two** function-scoped statics rather than one with a zero sentinel, and that is not tidiness: `defines.direction.north` **is** 0, so a cache treating 0 as "not resolved yet" would make a host call on every read for exactly the defines a mod reads most.

```rust
pub fn defines_direction_east() -> u32 {
    static V: AtomicU32 = AtomicU32::new(0);
    static OK: AtomicBool = AtomicBool::new(false);
    if !OK.load(Ordering::Relaxed) {
        V.store(unsafe { fk_define(133) }, Ordering::Relaxed);
        OK.store(true, Ordering::Relaxed);
    }
    V.load(Ordering::Relaxed)
}
```

*Enforced by `TestDefinesAreGeneratedAsNamesNotValues` (the table carries the name; `events.*` is not in it), `TestDefinesGenerateGuestAccessors` and `TestDefinesGenerateRustAccessors` (no value is baked into the bindings, the id reaches the import as a literal, and the resolved flag exists), and `TestADefineIsResolvedAgainstTheRunningGame`, which packages a real guest, resolves against a stand-in game, and checks all three readings plus the pruning.*

### `fk.defer` — the third import, and why it is not an end-of-dispatch hook

The `fk` module's imports are `call`, `subscribe`, `retain`, `release`, `define` and **`defer`**, which asks for the guest's `fk_on_deferred` export to be called once on the next tick.

**It exists because a blueprint paste is P SEPARATE dispatches in one tick.** Factorio raises one `on_built_entity` per entity from the engine's own loop, so each is an *outermost* dispatch — `depth` goes 0→1→0 P times — and the hook the downstream report asked for, at `dispatch_done`, would have fired P times and batched nothing. Re-entrancy (`create_entity{raise_built=true}`) is a different phenomenon and is not what a paste is.

The flush point is therefore the only per-tick point the API offers, `on_tick`, registered **only while something is pending**: `arm_deferred` is idempotent within a tick, and `flush_deferred` calls `off_event` on itself *before* dispatching — so a guest that defers again from inside its own flush arms a fresh one-shot rather than having it removed by a teardown that has not happened yet. Steady state is no registration at all. The cost is a one-tick latency, which is honest rather than avoidable: there is no end-of-tick hook in this API.

`storage.fk_deferred` carries the armed flag across a save, because Factorio does not serialize event registrations; `fk_mod.lua`'s single `script.on_load` re-arms from it. Guest-side ergonomics: [`guests.md`](guests.md).

### The scratch buffer is allocated lazily

Not at load: `fk_alloc` is a *guest export*, and TinyGo raises `//go:wasmexport function called before runtime initialization` for any export called before `_initialize`. Not in `subscribe` either, because a guest subscribes *from* `_initialize` — calling back into another export while the runtime is still starting is not something to rely on. By the first event everything is up.

---

---

## What a packaged mod contains

| file | |
|---|---|
| `control.lua` | verbatim `fk_mod.lua`: binds the ABI, wires events |
| `fk_abi.lua` | verbatim: handle table, dispatch, codec (~26 KB, fixed) |
| `fk_api_gen.lua` | generated, **pruned to the members this guest calls** |
| `fk_module.lua` | the compiled guest |
| `info.json` | generated from the flags and `fklua.toml` |
| *anything under `--include DIR`* | the mod's **data stage**, copied verbatim |

The hello guest calls no API and ships a **123-byte** member table.

### A mod has a DATA STAGE, and the packager has to carry it

`Files()` used to return exactly the five generated entries, `WriteDir` `os.RemoveAll`s its target first, and `--zip` archived the same five — so `data.lua`, `prototypes/`, `graphics/` and `locale/` had nowhere to go. The only workaround was to copy files over the output *after* packaging, which is something `--zip` cannot have done to it at all. Every non-trivial mod has a data stage; this was the top gap the first downstream consumer reported.

`fklua mod --include DIR` (repeatable) walks the tree and merges it into `Files()` **before either writer runs**, so a directory and a zip carry the same bytes and neither can be the path that was forgotten. Three decisions:

- **`--include DIR` is the mechanism; `fklua.toml`'s `[mod] data` is the default.** The same shape `gen-bindings` settled on for `lang` — one code path with the manifest feeding it, rather than two that can disagree. A manifest key *instead* of a flag would have left `fklua mod`, which takes everything else as a flag, with one setting it could only get from a file.

### `fklua mod` reads the manifest, and `dependencies` reach `info.json`

Identity used to live in two places: `init` wrote name/version/title/author into `fklua.toml` and `mod` took every one of them as a flag and never read the file, so the first downstream mod's Makefile `sed`'d the manifest back into flags to keep them in step. Same rule as everywhere else now — **the manifest is the default and a flag is the override**, filled field by field so a flag never has to fight the file. `fklua mod guest.wasm` with a manifest present needs no flags at all.

`[mod] dependencies` reaches `info.json` **verbatim, unparsed**, in Factorio's own syntax (`"base >= 2.0.0"`, `"? optional-mod"`, `"! conflicting-mod"`). The game is the authority on its own grammar and a half-understanding of it would reject strings the game accepts. `init` writes `["base >= 2.0.0"]`, because a key nobody knows exists is the same as one that does not.

*Enforced by `TestModReadsTheManifest` and `TestAModFlagOverridesTheManifest`.*
- **A collision with a generated name is an error**, refused in `Files()`, and neither precedence is defensible: an included file winning produces a mod whose guest never runs, a generated file winning produces a mod whose data stage is silently not the one the author wrote. Both are discovered in game; this is discovered at package time. Two included directories contributing the same path is the same error one level out, and it names both directories.
- **Nothing is filtered.** Factorio ignores files it does not recognise, and a packager that quietly drops something the author put in the directory is a worse surprise than an extra file in the archive.

Values are **bytes, not text** — most of a real data stage is PNGs — and keys are slash-separated whatever the host separator is, because a zip entry name is defined that way and Factorio's `require` resolves the same way.

### Wiring order is not arbitrary

Memory has to be reachable before any host call can marshal through it; the allocator is a *guest export* so it cannot be bound before the module exists; and all of it has to be in place before `_initialize`, because a guest's package initialisers can call the API.

**Globals are resolved lazily, not captured.** `game` does not exist while `control.lua` is loading, nor inside `on_load`. Binding a snapshot at load time would store nine nils and every handle 1..9 would be dead for the session. The handle table reads `_G` on access instead — the same single table read a snapshot would have done.

**`dispatch_done` releases the transient handles**, flushes packed memory and syncs globals, at the end of every guest entry point. The transient release is the half that matters: it is what makes the dominant leak shape cost nothing.

`env.fk_log` and `env.fk_print` remain alongside `fk.call`, because a guest wants them whether or not it touches the game.

## What a call costs — **measured**

M0 wrote down a red flag while sizing the project: *"no-op host round trip
> 2 µs → the ABI is too heavy."* The ABI was then built across M7 and the
threshold was never checked against it. `TestAHostCallIsUnderM0sTwoMicrosecondGate` checks it now, and asserts on it, so the number cannot go stale:

| what crosses | ns | vs the 2 µs gate |
|---|---|---|
| no-op call — handle, member table, a Lua method, nothing marshalled | **433** | 4.6× under |
| one i32 in, one i32 out | 962 | 2.1× under |
| a 37-byte string return | **1680** | 1.2× under |

**M0's worry was misplaced, and the measurement says where the cost actually is.** Generic dispatch — resolve the handle, index the member table, invoke — is 433 ns and nowhere near the flag. What approaches it is a **string return**. A guest reading `entity.name` every tick is the shape to worry about, not one calling a method.

### Where a string return's time actually goes — measured by ablation

Each component removed from `write_value`'s string branch in turn, the ABI cost test re-run, against a 1809 ns baseline:

| component | ns | share |
|---|---|---|
| `fk_wstr` | ~900 | **50%** |
| generic dispatch | ~440 | 24% |
| the allocator call | ~275 | 15% |
| the two return-slot `st32`s | ~165 | 9% |

**Read that table with the caveat below — it is measured with a STUBBED allocator and understates production by about 70%.** Within the harness the allocator is 15%, which is what made `fk_wstr` look like the whole story.

### A string return leaked its pin for as long as the ABI has existed

`write_field`'s `K_STRING` branch calls `alloc_(n)` to find somewhere in guest memory for the bytes, exactly as an array does — the only difference is that a string has a fixed layout, so it goes through `write_field` rather than `write_value`. The generator's `allocs` predicate was written from the *list of kinds* that reach `write_value` (array, dict, dyn) rather than from the question it was actually asking, and `KindString` was never on it. **290 of 291 string-returning Go members, and 95 of 144 in Rust, emitted no `allocMark`/`allocRelease` bracket.**

Nothing was corrupted and no test failed. The guest binding copies the bytes into its own string type immediately and never looks at the pointer again — the pin list simply grew by one entry per call, forever. A mod reading `entity.name` in `on_tick` appends sixty entries a second to a slice nothing shortens, on every client. `fkFree`'s own comment describes exactly this pathology as the reason it exists; the bracket that would have prevented it was just never emitted.

`HostAllocatesFor` is now a **whitelist of the kinds that cannot allocate**, and that inversion is the actual fix. A blacklist fails open — a kind nobody thought about leaks. A whitelist fails closed — a kind nobody thought about gets a bracket it may not need, which costs two integer operations on a path that already crosses the host boundary. `KindStruct` is on the allocating side for the same reason: its fields go through `write_field`, so a struct with a string in it allocates, and the generator cannot see that without resolving the concept. *Enforced by `TestAStringReturnReleasesWhatTheHostAllocatedForIt`, which reports the exact counts above when the predicate is reverted.*

### The allocator is the real cost, and this test cannot see it

`abicost_test.go` binds a bump allocator written as a **Lua closure**. Production binds `E.fk_alloc` — a `//go:wasmexport` whose body is `make([]byte, n)` plus a pin-list append, compiled to Lua. Measured, 200k `alloc(37)`/`free` pairs through a real compiled `examples/array` guest against that same closure:

| | ns per alloc+free |
|---|---|
| the test's Lua closure | **53** |
| a real compiled guest | **1535** |

**29×.** Splitting it: `alloc` is ~1333 ns and `free` only ~202, so `fkFree`'s backwards linear scan is not the problem — TinyGo's allocator is. Composing the measured parts, a real 37-byte string return costs roughly:

| component | ns | share |
|---|---|---|
| **`fk_alloc` + `fk_free`** | **~1535** | **~53%** |
| `fk_wstr` | ~770 | ~27% |
| generic dispatch | ~440 | ~15% |
| the two return-slot `st32`s | ~165 | ~6% |
| | **~2900** | |

So a real `entity.name` costs about **2900 ns, not the 1680 this test reports — already 45% past the 2000 ns gate it claims to pass.** The test is not wrong about what it measures; it is measuring dispatch and marshalling with the allocator held constant, which is a fair thing to want. But nothing currently gates the number a mod author actually pays, and the gate's reassurance is therefore narrower than it reads. Bind a real guest allocator before trusting it as a budget.

### The string scratch region — **built, and worth 2.26×**

The host allocates a buffer the guest binding immediately copies out of, so the lifetime is call-scoped and a reusable region serves it. `fk_scratch_base` and `fk_scratch_size` are two more guest exports (two, not one returning a pair, because multivalue is not in the feature set FkLua compiles); the guest still owns the address for the same reason it owns `fk_alloc`'s. A string that fits is a bump; anything longer falls back to `fk_alloc`, which is what lets the region be 4 KiB rather than sized for a blueprint string.

Measured end to end with a **real compiled guest allocator** bound — 200k 37-byte string returns, paired A/A harness:

| | ns per string return |
|---|---|
| `fk_alloc` | 2429 |
| the scratch region | **1072** |
| | **0.442×, a 2.26× speedup** |

**The re-entrancy rule is the whole of the difficulty, and it is not obvious.** Factorio raises some events synchronously from inside the API call that caused them, so an event's string fields are written into the region, the guest handler starts reading them *lazily from the pointer*, and the handler makes its own host calls before it is finished. A reset-to-zero at the top of `encode_rets` would write the handler's return values straight over the event fields it is still reading — structurally valid bytes belonging to something else, which is a desync and not an error. So:

- a host call reclaims only back to **its own mark**, never to zero: whatever is below belongs to something further out that is still live (`scratch_mark` / `scratch_release`, bracketed in `M.call`);
- **only the outermost dispatch** resets the region, in `fk_mod.lua`'s `dispatch`, before the depth increment so a re-entrant dispatch does not.

*Enforced by `TestANestedCallDoesNotClobberAStringTheOuterOneIsStillReading`, which reports `outer inner2entity` — a partial overwrite — when `scratch_release` is changed to reset to zero.*

**`bind_scratch` runs AFTER `_initialize`, not with the other binds.** These are `//go:wasmexport` functions like any other and TinyGo's runtime raises `unreachable` from every one of them until `_initialize` has run, with no name attached to the trap. The bind block higher up only *stores* functions; this one *calls* two.

Regenerating the 3,858 bindings was never the obstacle — `gen-bindings` does it in one command and `--check` gates it. In the event, the per-member bindings did not change at all: the string decode goes through one substrate helper.

**Still missing:** nothing gates the real number. `abicost_test.go` binds a Lua closure, so its 2000 ns threshold is measured against a path that no longer resembles production in either direction. `TestAStringThatFitsNeverReachesTheAllocator` gates that the region is *engaged*, which is the durable half; a timing gate would need a compiled guest in that package.

For the marshalling half: a floor measurement says only ~18% of `fk_wstr`'s word body is the table stores it cannot avoid.

`fk_wstr` therefore writes **four words per `string.unpack` call** rather than one word per `string.byte`. **The batching is the whole win, not `unpack`**: one word at a time through `unpack` measured identical to the byte form, because either way it is one C call per word. Measured through the real `memio` closure:

| | fixed | per byte | 37 B | 148 B |
|---|---|---|---|---|
| before | ~246 ns | 12.2 ns | 697 ns | 2049 ns |
| after | ~294 ns | **6.44 ns** | 532 ns | 1247 ns |
| | | | 1.32× | **1.65×** |

The per-byte rate nearly halves and lands within 1.6× of `mem_copy`'s 4.11 ns aligned rate. Fixed overhead rises slightly — one more loop to set up — so the win grows with string length and a very short string gains little. On the whole ABI call at 37 bytes it is ~7%, because `fk_wstr` is half the call and 37 bytes is only nine words.

`string.unpack` is localised **inside a `do ... end` block**, not at column zero. Putting it at chunk level costs every guest one of Lua's 200 chunk locals and `TestPromotionLeavesTheMarginItPromises` fails — which is exactly what happened on the first attempt.

That is worth knowing before optimizing the dispatcher, which is where the plan's wording would have pointed.

**Three things about the measurement, each of which caught a real error:**

- **Timing comes from outside the process.** `os.clock` is nil — lua52f is patched to Factorio's sandbox and Factorio removes the `os` library. The oracle is right and a guest cannot read a clock either. So the same script runs at N iterations and at zero, and the two are differenced.
- **400,000 iterations, not 20,000.** The first version's delta was 11 ms buried in ~200 ms of process startup and Go-side compilation, and it reported a **string return as cheaper than a no-op**. Building the harness once and timing only the lua52f run fixed the variance; raising the count fixed the ratio.
- **Every member is called once and its status checked BEFORE anything is timed.** The first version bound no allocator, so a string return was *refused* rather than performed — the benchmark was timing an error path and reporting it as a fast one. The test now refuses to time a call that does not return status 0, and asserts the three costs are **monotone in the work done**, since a step that does strictly more cannot cost less.
