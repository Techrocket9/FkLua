# Tier D — record/replay debugging, built on determinism

**Status: DESIGN RECORDED 2026-08-31. NOT SHIPPED, NOT SCHEDULED, AND NOT ASSIGNED.** Nothing here exists. No line of it is implemented, no cost in it is measured, and no part of it is queued behind anything. This is the record of a shape that follows from a property FkLua already has, written down while the reasoning was in front of somebody, so that the next person to want it starts from a design rather than from an intuition.

It is written against the debugging-tiers conversation with the fklua-mod-toolkit fork, which settled on four tiers and built one:

| tier | what it is | where it stands |
|---|---|---|
| **A** | `fk_module.map.json` beside `fk_module.lua`: each emitted guest function's Lua line range, wasm function index, name-section name (Rust demangled best effort), optional `src`/`line` from DWARF. Version field `"fklua_map": 1`. The toolkit's DAP proxy annotates Factorio debug-adapter stack frames with guest function names. | **Being implemented on this branch.** Function-level attribution only: no breakpoints, no stepping, no variables. |
| **B** | Full source-level breakpoints and stepping, via a line map composed from DWARF and an emitter-recorded wasm-to-Lua table. | **Maybe someday.** Sketched at the end of this file. The `fklua_map` version field is the hook. |
| **C** | — | Not a thing; the conversation numbered A, B and D. |
| **D** | This file. Record every host-to-guest byte, replay the guest against the recording under a real debugger. | **Designed here, nothing else.** |

Tier A is the baseline this builds on and the two are complementary rather than sequential: Tier A tells you *which guest function* a Lua stack frame is in, and Tier D gives you a guest you can put a breakpoint in at all. Neither needs the other to be useful, and the pair is worth more than the sum, because a checksum divergence at tick T plus a map lookup at line N is a two-coordinate localisation of a bug that neither coordinate localises alone.

---

## Why this is possible at all — the determinism invariant, stated as a capture rule

CLAUDE.md's critical rule says determinism is a correctness property, and `agents/ipc.md` states the half that a joining client measured. Restated as the precondition for recording:

> **Guest state advances only from (1) its own state, (2) the replicated tick, and (3) the bytes that arrived through the replicated inbound path.** Nothing else may reach a write. Under the default `--persist=table`, guest memory **is** `storage.fk_mem` and Factorio CRCs it across every peer, so anything else that reached a write would already be a desync.

That rule exists to stop desyncs, but it is also the strongest statement a debugger could ask for. It says the guest is a pure function of its initial memory image and a byte stream, and it says that stream is *finite and enumerable*, because the only way a byte reaches the guest is across the host ABI. There is no clock, no entropy (the WASI `random_get` shim is a counter-based LCG whose state lives in `storage`), no thread, no file read, no socket the guest owns, and no iteration order the guest may depend on.

So: **record every host-to-guest byte and you can re-run the guest exactly.** Not approximately, not modulo timing — exactly, to the byte, for as many ticks as the recording covers. That is the whole of the idea and everything below is the consequences.

### The direction rule is what makes the surface finite

`agents/ipc.md`'s cost model decides everything by DIRECTION, and it decides this too:

- **Outbound is free, and therefore records nothing.** `send_udp`, `write_file`, `rcon.print`, `fk_log`, `fk_print` are local side effects. Their *outcome* is per-peer — `--enable-lua-udp` binds the socket and a joining graphical client has no such flag — which is why the fkipc transport seam now returns **nothing at all** (`Send(frame []byte)`, `fn send(&mut self, frame: &[u8])`) rather than a status a guest could store. A value that cannot cross back is a value the recording never has to carry.
- **Inbound is expensive, and therefore records everything.** A received datagram becomes an InputAction: replicated to every peer, quantised to a tick, present in the replay. That is what makes it legal to branch on, and it is also what makes it recordable once and replayable anywhere.

The asymmetry is the design's foundation. A recorder that had to capture outbound outcomes would be capturing per-peer facts and would produce a recording that only replays on the peer that took it. It does not, because the ABI has already been shaped so that no such fact can reach guest state.

---

## The capture surface, enumerated

The host ABI call boundary **is** the seam. `runtime/lua/fk_mod.lua`'s `imports` table plus the `memio` closures the generated chunk exposes are, between them, every route a byte has into the guest. Four groups.

### 1. The entry points the host calls, in order, with their scalar arguments

Every one of these is a guest export the host invokes; the recording needs the call, its arguments, and its position in the sequence.

| export | recorded |
|---|---|
| `_initialize` | that it ran (it runs on **every** load, not only on a new map) |
| `fk_on_init` | that it ran |
| `fk_on_tick(tick)` | `tick` |
| `fk_on_nth_tick(n)` | `n` |
| `fk_on_event(id, ptr)` | `id`, and the payload bytes at `ptr` — see group 2 |
| `fk_on_deferred()` | that it ran |
| `fk_after_load()` | that it ran — **and this one is peer-local**, see the multiplayer open question |
| `fk_migrate(old_version)` | `old_version` |
| `fk_on_configuration_changed()` | that it ran, plus its payload (the only hook that carries one) |
| `fk_on_call(...)` | the callback seam's id-dispatched entry: command and remote-interface invocations |
| `fk_state_version()` | nothing inbound; the guest answers |
| `fk_gc_step`, `fk_arena_mark`, `fk_arena_release`, `fk_alloc`, `fk_free`, `fk_alloc_static`, `fk_migrate_adopt` | the host chooses **when** to call these and with what, so the sequence and the arguments are inbound even though the values often came from the guest |

The last row is easy to miss and load-bearing. `fk_free(ptr)` takes a pointer the guest handed out, but *when* the host frees is a host decision, and the arena bracket (`fk_arena_mark` at an outermost dispatch, `fk_arena_release` before `dispatch_done`) is entirely host-driven. A replay that reconstructed those calls from its own logic rather than from the recording would be reimplementing `fk_mod.lua`, which is the second-implementation failure the `fklua meta --json` round already paid for once.

### 2. Everything the host writes into guest linear memory

This is the group that decides whether the idea works, because it is where the interesting bytes are and where an incomplete list is silent.

- **Event payload structs.** `H.write_struct(fields, buf, e)` writes the payload into `event_buffer(depth)` and then calls `fk_on_event(id, buf)`. The encode happens *inside* the dispatch, so the payload's strings are live for the whole handler. Record the buffer's bytes as delivered, at the size the field list produced (which the mask may have shrunk — `SubscribeMasked` refuses fields the guest never reads).
- **Return blocks.** `fk.call(handle, member, argp, retp)` and `fk.call_typed` write the member's return block at `retp`: status-adjacent out-params, handles, numbers, and `(ptr, len)` pairs for strings. The block layout is placed **once, at generate time** by `internal/factorio/layout.go`, so a recorder can size a frame without walking it.
- **Strings the host writes through `fk_wstr`.** Every string the ABI marshals back takes this path: the host allocates through the guest's `fk_alloc`, writes the bytes with `fk_wstr`, and the guest frees. `fk_wstr` is *the only write path into guest memory that the host, not the guest, drives*, which is exactly why it needed its own `MEMPACK.mark` call and why omitting it lost every marshalled string from every packed-mode save. That history is the reason to trust the list here only as far as it has been re-derived: a shorter version of this list was wrong for two milestones.
- **`fk.bulk_get` results.** `dstp` receives `count` copies of the ordinary getter's own return block; `retp` receives four bytes saying how many elements were read successfully. A skipped element is written as **zero** rather than left alone, so the destination is fully determined by the call and there is no "unchanged" case to reason about.
- **`fk.last_error(ptr, cap)`.** The engine's refusal text, written through `instance.memio.wstr`, truncated to `cap`, with the **full** length returned. Both halves are inbound.
- **`fk.remote_call(callp, retp)`.** One tier-2 value written into the result slot.
- **The WASI shim.** `fd_write` writes `nwritten` through `st32`. `random_get` writes `len` bytes through `st8`, from a PRNG whose state is in `storage` — replicated, and therefore reproducible in principle, but the recording should carry the bytes anyway rather than reimplement the LCG.
- **The collector's dirty-page handoff.** `MEMPACK.gc_drain` writes the pages written since the collector's last step into guest memory at a base the guest supplies, as i32 words, returning a count (or `4294967295` meaning "assume everything is dirty"). This is host bookkeeping crossing into the guest and it changes what the guest's collector does. A collected guest that omitted it from the recording would replay a different marking schedule.

### 3. The values host imports return

`fk.call` / `fk.call_typed` / `fk.bulk_get` return an i32 status; `fk.retain` returns a promoted handle out of the persistent handle space; `fk.release`, `fk.subscribe`, `fk.define`, `fk.defer`, `fk.on_nth_tick`, `fk.gc`, `fk.register` and `fk.remote_call` each return a number the guest may branch on. `fk.last_error` returns a length. All of them are inbound bytes in the sense that matters, and all of them are cheap to record.

### 4. fkipc inbound frames

Mechanically these are group 2 — an inbound datagram raises `on_udp_packet_received`, which arrives as an ordinary event payload — so a recorder at the ABI seam captures them without knowing what fkipc is. They are called out separately because they are the **only channel that brings bytes from outside the game into a guest**, and because they are the one place where "the recording is complete" has a second meaning: an fkipc session is a stateful conversation with a peer that has a real clock, and a replay is not talking to that peer. The recording is the peer, from the guest's side, and the guest cannot tell.

### The initial condition

A byte stream replays against an initial state, and that state is `--persist`-mode-shaped:

- **`--persist=table`** — guest memory *is* `storage.fk_mem`. The initial image is whatever the save carried, or the data segments on a fresh map.
- **`--persist=packed`** — the live word table is outside `storage`, mirrored into one `string.pack` page per 4 KiB. `MEMPACK.restore` installs the saved image wholesale and clears the dirty set.
- **`--persist=none`** — memory is rebuilt from the module's data segments every load, which makes the initial condition free but makes the recording's usefulness end at the load.

Plus the guest's mutable globals, which cannot alias (a wasm global is a Lua local) and are copied back after every guest call. A TinyGo guest has exactly one, the shadow-stack pointer; a Rust guest's count is its own business. They are part of the initial condition and part of every call boundary.

The honest summary: **the recording is (initial memory image, initial globals, ordered byte stream), and the guest is a function of the three.**

---

## Detecting an incomplete capture — tick-boundary checksums

The list above is a claim about a codebase that has twice found the same list to be short. So the design does not rest on it. **Checksum guest memory at tick boundaries during BOTH record and replay, and compare.** An incomplete capture surface, a miscompile, or a replay harness bug then shows up as a divergence at the **first** tick where it matters, rather than as a replay that drifts quietly and produces a plausible wrong answer three hundred ticks later.

This is the same instrument the project already trusts elsewhere: `fklua bench` and `bench --opt` compare checksums across variants and levels and **fail the run** on a mismatch rather than reporting a flattering number. A record/replay checksum is that rule applied across time instead of across variants.

Two cost shapes, and the cheap one has a condition on it:

- **Bound the re-hash with the dirty-page set.** `MEMPACK`'s page set already records exactly which 4 KiB pages were written, with a two-compare fast path (`DPLO`/`DPHI` bound the one most recently marked page) and a division only when a store leaves that page. Hashing only the dirty pages and folding into a running digest is the obvious cheap form. **But the set is not armed in every mode** — this corrects a plausible assumption. `MEMDIRTY` is `false` by default; `MEMPACK.arm` sets it under `--persist=packed` (`PKARM`), and `MEMPACK.gc_arm` sets it while a `--gc=collected` collector is **marking** (`GCARM`), with `MEMDIRTY` being their OR. Under the default `--persist=table` with `--gc=leaking` there is no set to read: the store leaves pay one upvalue read and one test and nothing else. So a recorder wanting incremental hashing either runs only in a mode that already arms the set, or arms it for the recording — which changes the store path's cost, and the only number nearby is the **3.5%** measured for the flag test alone on a kernel that does nothing but store in a loop. That figure is about the *test*, not about the marking, and it is not a measurement of this.
- **A full-memory hash at a coarser cadence, as the backstop.** Every N ticks, hash the whole word table. This catches anything the page set could miss (a writer that marks nothing, which is the exact failure `fk_wstr` was) at the price of a periodic stall. Cost unknown and it is a function of heap size, cadence and the hash — a 52 MiB heap is 26 shards of 2¹⁹ words and nothing here has ever timed a walk of one for this purpose.

**Both cost lines are OPEN, deliberately.** This repo's culture is measured-or-marked-open and there is no harness for either yet. What the design does commit to is the *shape*: an incremental digest whose correctness does not depend on the page set being armed, with a full hash as the periodic proof that the incremental one is still telling the truth. If the two ever disagree, the incremental one is wrong and something writes memory outside the mark path — which is a real bug independent of debugging, and worth finding.

---

## Replay flavour 1 — a native rebuild, under delve or lldb

Rebuild the guest for the host architecture (`go build`, `cargo build`), link it against a shim that implements the host ABI by reading the recording instead of by calling Factorio, and run it under an ordinary debugger.

**What it buys, and it is a lot.** Real breakpoints, real single-stepping, real variable inspection, real conditional breakpoints, real watchpoints — in the author's own `main.go` or `lib.rs`, in the debugger they already use, with their editor's existing integration. This is by a wide margin the best developer experience of anything in this document, and for the class of bug people actually file (my logic is wrong; my state machine took the branch I did not expect; my entity handle was stale) it is the correct answer.

**What it cannot see, and this is the whole caveat: it debugs a RECOMPILATION.** The binary under the debugger is not the artifact that ran in the game, and the differences are not cosmetic:

- different backend — native LLVM (or `gc` for standard Go) against TinyGo's `wasm-unknown` at `-opt=2`;
- different pointer width — 64-bit host against wasm32, so every layout, every truncation, every pointer-sized assumption differs;
- different allocator and collector — the host runtime's against `fkgc` or `-gc=leaking`, so anything about the guest heap budget, the write barrier, the span metadata or the last-slot arithmetic is simply not present;
- different wasm — there is none.

A miscompile in TinyGo, in `wasm-opt`'s lowering pass, in FkLua's wasm-to-Lua emitter, or in the shard/page machinery is **invisible** here. The native build will happily compute the right answer while the shipped artifact computes the wrong one, which is the failure mode that makes this flavour a tool and not a proof.

**And building it is not free.** A TinyGo guest built `-target=wasm-unknown -gc=custom` does not build for the host as-is: `fkgc` is wasm-only, the `//go:wasmexport` entry points need host-side stubs, and the `fkapi` bindings are generated against the wasm ABI. A Rust guest needs the same treatment against `wasm32-unknown-unknown`. The shim is therefore a real piece of engineering per language, not a thin adapter, and its size is not estimated here.

## Replay flavour 2 — the actual wasm, under wasmtime

Run the **exact bytes** `fklua mod` consumed, under wasmtime, with its lldb/gdb DWARF support, driving the same recording through wasmtime's host-function imports.

**What it buys.** The translation *input* is exact. Every toolchain-shaped bug flavour 1 hides is present here: TinyGo's codegen, `wasm-opt`'s lowered `memory.copy`/`memory.fill`, wasm32 layout, `fkgc`'s actual behaviour against the actual heap, the shadow-stack pointer as a real global. If the wasm is wrong, this catches it and flavour 1 does not.

**What it costs.** Worse DX, on two axes. Wasm DWARF support is thinner than native — stepping generally works, variable inspection is patchy and optimisation-dependent, and `-opt=2` is mandatory for this project's guests rather than optional. And it needs the guest to **carry** DWARF, which is the same open question Tier A already has about its optional `src`/`line` enrichment: whether each toolchain's shipped profile emits it (and whether the Rust scaffold's release profile needs `debug = true`) is documented as needing verification on the FkLua side, not assumed here.

**What it still does not touch: the Lua.** Wasmtime runs wasm. The shipped artifact is `fk_module.lua`. Everything between those two is FkLua's emitter, and this flavour is blind to all of it.

## What NEITHER flavour catches, and what localises it anyway

**A miscompile in FkLua's wasm-to-Lua emitter is invisible to both debuggers, by construction** — neither one runs the emitted Lua. That is not a gap in the design so much as a restatement of what the design is: record/replay debugs the *guest program*, and the emitter is a different suspect.

The consolation is real and worth stating precisely, because it is the argument for building the checksum half at all:

1. **The checksum divergence names the tick.** Record in game, replay under either flavour, and the first tick where the digests disagree is the first tick where the shipped artifact and the reference implementation computed different bytes. That is a bisection down to one dispatch, for free, from an instrument that has to exist anyway.
2. **The Tier A map names the function.** A Factorio DAP stack frame at `fk_module.lua:N` binary-searches `functions` and comes back `main.onTick (main.go:87)`. Tier A never lies about where execution is — the frame's source and line stay the real Lua location — it enriches the name.

Tick plus function is the coordinate pair this repo's own miscompile history says is expensive to get. Both confirmed miscompiles (negated float branches, a deferral into an identity lowering) were reachable at the default `-opt` level and were found by differential opt-level tests, not by anybody reading a stack. A field bug that arrives with "tick 41,207, in `main.onTick`" is a `sameAtEveryLevel` case somebody can write the same afternoon.

### A third arm, which this repo is closest to already

Worth recording because it is the one that *does* catch an emitter miscompile, and because most of it exists: **replay the recorded stream against the emitted `fk_module.lua` under `bin/lua52f`.** The oracle is already built from PUC source and patched to Factorio's shape, `make check-lua52f` already guards it in CI, and `internal/guest` already drives a real `control.lua` through it (`TestWhatAHostCallCostsThroughARealGuest` is the shape). Feeding a recording in instead of a synthetic script is a smaller step than either flavour above.

It has the worst DX of the three by a distance — you are stepping generated Lua whose only names are the Tier A map's — and it does **not** reproduce Factorio's table internals, so nothing about the cost of a large table transfers. But as a *correctness* arm it dominates: same emitted Lua, same recording, same answer, or the emitter is wrong. If any single arm gets built first, the argument for this one is that it is cheapest and catches the class the other two cannot.

---

## The re-record loop, and why it is the accepted cost

**You cannot poke state mid-replay and continue.** The moment a debugger writes a variable, the guest's state stops being a function of the recording, and every subsequent host-call return in the stream was computed by a game that saw different guest behaviour. Continuing past that point replays a fiction: the returns are stale, the checksums diverge immediately, and everything after is noise dressed as evidence.

**A changed guest invalidates the recording past the point of behavioural divergence.** Not immediately — a recording is still valid for an edit that cannot change what the guest asks the host, and it stays valid up to the first call whose arguments differ. But there is no way to know where that point is except by hitting it, and the checksum is what tells you.

So the workflow is:

> **record → replay → read → edit → re-record.**

Which is a compile-and-rerun loop, not an interactive-debugging loop, and it is slower than what a Go developer is used to.

**It is accepted, for two reasons.** First, *deterministic reproduction of a field bug is the win*, and it is a large one: today, a bug that manifests on a player's map at tick 41,207 after a particular sequence of events is reproduced by asking the player for a save and guessing. A recording reproduces it exactly, on a different machine, every time, as many times as you like, with the failing tick already named. That is the thing that does not currently exist and it does not need live poking to be worth having. Second, *live poking was never on the table*. It cannot be: a Factorio mod runs inside a lockstep simulation whose memory is CRC'd across peers, and there is no version of "let the developer write a variable and keep playing" that is not a desync. The constraint that makes recording possible is the same one that forbids poking, and trading one for the other is not an option that exists.

---

## Alternatives considered and rejected

**Instrument the emitted Lua to trace.** Have the emitter add a per-function or per-line trace call to `fk_module.lua` and read the log. Rejected: it is a second emitter mode to maintain and test (this repo already carries `-opt` × `--persist` × `--gc` as a variant matrix and each axis has cost a defect); the trace is per-execution rather than per-tick and a busy guest emits an unusable volume; it changes the artifact's performance profile enough that timing-shaped bugs move; and it gives you a log, not a debugger — no variables, no state, no ability to ask a question you did not think of before the run. Tier A gets the *attribution* half of this for zero runtime cost, from a sidecar file, which is most of what the trace was for.

**A wasm interpreter in-process.** Ship a wasm interpreter written in Lua inside the mod, run the guest under it, and expose a debug protocol. Rejected on its face: FkLua exists precisely because the interpreter route is too slow for a tick budget, and an interpreter that reproduces the shipped artifact's behaviour is not the shipped artifact anyway — it is a third implementation of wasm semantics, alongside the emitter and the toolchain, and this repo has already learned what a second implementation costs. It would also have to live inside Factorio's sandbox, where `coroutine` is absent and `debug` is `getinfo`/`traceback` and nothing else.

**Snapshot-based time travel.** Checkpoint guest memory every N ticks and let the developer scrub backwards. Rejected as the *primary* mechanism: a snapshot of a multi-megabyte heap every N ticks is a storage cost that scales with heap size rather than with event rate, where a byte stream scales with what actually crossed the boundary; and scrubbing gives you states without giving you the *reason* for a transition, which is the thing a debugger is for. Worth keeping as a possible *optimisation* of the replay harness — a snapshot every N ticks lets a replay seek to tick T without re-running from zero, which for a bug at tick 41,207 is the difference between a usable tool and an unusable one. Recorded as a candidate, not a design.

**`debug.sethook` inside the mod.** Not available: `agents/sandbox.md` records all of `debug` beyond `getinfo` and `traceback` as **verified absent** from Factorio's Lua 5.2 sandbox. Whether `--instrument-mod` lifts that is not something this file has verified, and the toolkit's DAP proxy sits in front of Factorio's own debug adapter, which already does the Lua-level breakpoint and stepping machinery — so there is nothing for a guest-side hook to add even if one existed.

---

## Open questions

None of these has an answer here, and several are the kind that decide whether the thing gets built at all.

1. **Capture format and versioning.** A version field first, on this repo's own precedent (`fklua_map: 1`, `api/<version>/census.json`, `fklua.lock`) — call it `"fklua_rec": 1` and require consumers to ignore unknown fields. Open beyond that: JSON-per-frame (readable, greppable, enormous) against a length-prefixed binary frame stream (compact, and fkipc's wire format plus its committed `testdata/ipc/wire-vectors.txt` is an existing candidate to reuse rather than invent). If a recording is ever a gate artifact it has to be byte-deterministic, the way `fk_module.map.json` must be for `--zip` reproducibility.
2. **Where the recorder lives.** Four candidates, none costed. (a) *In the runtime Lua* — `fk_mod.lua`'s `imports` table and the `memio` closures **are** the seam, so this is the only place that sees the whole surface by construction; but it is in the shipped runtime, on the hot path, in every mod, and the only way out is `helpers.write_file` (outbound, therefore free by the cost model, which is the one piece of luck here). (b) *A packaging flag* — `fklua mod --record` emitting a recording variant of the runtime, which matches how `--persist`, `--gc` and `-opt` already vary the emitted artifact and keeps the cost out of an ordinary build; this is the shape the author of this file would start from. (c) *An fkipc tap* — a peer outside the game; sees fkipc traffic and nothing else, so it cannot capture the ABI, but it might be the right transport for shipping a recording off a player's machine. (d) *A Factorio instrument-mode hook* — out of the shipped artifact entirely, which is the right property, but `--instrument-mod` is a launch flag and cannot record a player's field session, which is the use case that motivates the whole tier.
3. **Replay harness ownership: FkLua or the toolkit?** FkLua owns the ABI, the emitter and `bin/lua52f`, so the third arm (emitted Lua under the oracle) is nearly free there and belongs there. The toolkit owns the DAP proxy and the developer-facing driver, so the delve and wasmtime arms and all the DX belong there. That split is a guess; the format is the contract between them either way, exactly as the Tier A map is.
4. **Storage cost, per tick and per session.** Unmeasured. The *shape* is known: a quiet guest with only `fk_on_tick(tick)` records an entry-point tag and a tick number; an event-heavy guest records a payload struct per event plus a return block per host call, and both of those have sizes the ABI already knows at generate time. Whether a one-hour session on a real mod is megabytes or gigabytes is not known, and it decides whether a player can be asked to send one.
5. **Save/load mid-recording.** A load replaces the initial condition, differently per mode (`--persist=table`: `storage.fk_mem` is restored wholesale; `packed`: `MEMPACK.restore` installs the image and clears the dirty set; `none`: memory is rebuilt from the data segments). Either the recorder checkpoints a full memory image at every load, or a recording refuses to span one. And `fk_migrate` is the harder case: the build id changed, so the guest on the other side of it is a *different program*, and no recording spans that.
6. **Multiplayer capture — which peer records?** The replicated inbound bytes are identical on every peer at the same tick, and `agents/ipc.md` says so explicitly, so **for group 4 any peer suffices**. It does **not** follow that any peer suffices for the whole surface, and the counterexample is in the same file: `fk_after_load` fires on the joining client and on **no other peer**, so a recording taken on a joiner contains an entry point the server's timeline does not have. A joining client's guest memory is also downloaded from the server, so its initial condition is a different object from the server's. Open: whether a recording should be peer-tagged and refuse to replay against a different peer's role, or whether `fk_after_load` should simply be recorded like everything else and the difference allowed to be visible.
7. **Interaction with `--persist` modes.** `table` gives the initial condition for free (it is the save). `packed` needs the unpacked image or the pages. `none` makes the initial condition trivial and the recording's usefulness end at the load. And the checksum question above is `--persist`- and `--gc`-shaped too, because that is what decides whether the dirty-page set is armed.
8. **The data stage.** A data guest is a **second wasm module** with its own eight-import ABI, where errors RAISE rather than returning a status, and whose determinism story is the sort-everything rule rather than the tick. It runs at load, is never ticked, and is a much smaller capture surface. Whether Tier D covers it at all, or whether `--dump-data` plus the existing acceptance gate is already the right instrument there, is not decided.
9. **What a recording contains that a player might not want to send.** Event payloads carry player names, positions, chat-adjacent strings and whatever `fk.last_error` quoted from the engine. Anything shipped off a player's machine needs an answer to that, and this file does not have one.

---

## Tier B — full source-level stepping, maybe someday

**Not designed here, and not scheduled.** Recorded so the hook is not lost.

The shape: compose two maps. DWARF's line table gives *guest source line → wasm instruction offset*; an emitter-recorded table would give *wasm instruction → emitted Lua line*. Composed, that is *guest source line → `fk_module.lua` line*, which is everything a DAP adapter needs to set a real breakpoint in `main.go` and have it fire.

**The hook is the map's version field.** `fk_module.map.json` carries `"fklua_map": 1` and the v1 contract already requires consumers to ignore unknown fields, so a **v2** that adds per-line tables beside the existing per-function entries breaks no v1 consumer. That is the entire reason the version field is there and the reason to keep it exactly where it is.

**What it would take, none of it costed:**

- **A per-line emitter map.** Today's map is one entry per emitted function. A line map is one entry per wasm instruction or per basic block, which is three to four orders of magnitude more entries, in a sidecar that ships with the mod. Nobody has sized it.
- **DWARF at line granularity, actually present in the shipped wasm.** Tier A's `src`/`line` needs only `DW_TAG_subprogram`'s `decl_file`/`decl_line` and is explicitly best-effort; a line table is a stronger requirement of the same unverified thing, per toolchain and per profile.
- **A DAP adapter that lies about location, coherently and in both directions.** Tier A's contract is explicit that it never lies: the frame's source and line stay the real Lua location and only the *name* is enriched. Tier B is the opposite — it must report `main.go:87` where execution is at `fk_module.lua:4283`, translate the user's breakpoint requests back the other way, and keep the two views consistent through stepping, stack unwinding and variable scope. "Coherently" is doing a great deal of work in that sentence.
- **Survival through the optimizer, which is the part most likely to kill it.** The line correspondence this needs is destroyed by passes the project depends on: at `fklua`'s `-opt` ≥ 1, range analysis eliminates compares outright, a wrap is deferred to a consumer that re-reduces it, and the counted-loop lowering *replaces* a loop header. Worse, one rewrite is not level-gated at all: the trampoline relay **rewrites one function's text in place, so every span after it shifts by whatever was inserted**, which a per-line map has to track wherever it happens. So the map is either `fklua -opt=0`-only — not a build anybody ships, and a level whose whole point is that the shipped ones are faster — or every present and future pass owes it a maintenance obligation forever. That is the cost nobody has estimated, and it is the reason this is a paragraph rather than a plan.

**Verdict: maybe someday, and the version field is the only commitment made.**
