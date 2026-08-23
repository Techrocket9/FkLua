# `agents/ipc.md` — the fkipc protocol and guest API

**FkIPC** — *Factorio, kommunikativ (per IPC)* — the sibling of the project's own *Factorio, kein Lua*.

**Read before touching `guest/go/fkipc`, `guest/rust/fkipc`, `sdk/go/fkipc` or anything that puts bytes on the UDP socket.** Phase 1 is UDP-only and touches **nothing** host-side: no import, no export, no line of `fk_mod.lua` or `fk_abi.lua`, no golden file, no `Hooks` mirror, no census row. Everything below is a guest library and an external library over surfaces that are already bound, generated and committed. If a change here needs a host-side edit, that is the signal to stop and re-read this sentence, because the whole reason phase 1 is UDP-only is that it does not need one.

**This file was a draft and is now the as-built record.** Where it still says what a thing is *for* rather than what it *does*, that is deliberate — the reasoning is the part that does not become obsolete. Everything the probe left as `TBD-PROBE` has a measurement beside it now; the sections marked **As built** record where the implementations departed from the design and why. The gate that proves the whole of it against a running game is [`scripts/run-ipc.sh`](../scripts/run-ipc.sh).

**The deepest defect this milestone found was in the design and not in the code**, and it is the thing to read before changing anything about session lifetime: the session-reset-on-load rule desynced a joining multiplayer client, because `fk_after_load` is a peer-local signal and guest memory is CRC'd game state. See [the rule the cost model implies](#the-rule-the-cost-model-implies-which-this-design-got-wrong-once), [what each side does on a session boundary](#what-each-side-does-on-a-session-boundary), and [rollback detection](#rollback-detection-the-clock-owning-side-owns-the-clock-anomaly).

---

## Why this exists, and the cost model that shapes it

Factorio's sandbox has no sockets and no file *reads*. Everything a mod can say to the outside world goes out through three channels, and everything the outside world can say back goes in through two — RCON, and (since 2.0.59) UDP receive. The raw surface is already reachable from a FkLua guest today, in both languages, at member-id parity across both API pins:

| surface | bound as, Go | bound as, Rust |
|---|---|---|
| `helpers.send_udp(port, data, for_player)` | `fkapi.Helpers.SendUdp` | `fkapi::HELPERS.send_udp` |
| `helpers.recv_udp(for_player)` | `fkapi.Helpers.RecvUdp` | `fkapi::HELPERS.recv_udp` |
| `on_udp_packet_received` | `fkapi.EventOnUdpPacketReceived`, `fkapi.ReadOnUdpPacketReceived` | `fkapi::EVENT_ON_UDP_PACKET_RECEIVED`, `fkapi::read_on_udp_packet_received` |
| `helpers.write_file(name, data, append, for_player)` | `fkapi.Helpers.WriteFile` | `fkapi::HELPERS.write_file` |
| `rcon.print(message)` | `fkapi.Rcon.Print` | `fkapi::RCON.print` |

**The table names SYMBOLS, and that is deliberate — the numbers behind them are not quotable.** A member id and an event id are dense sorted indices *per API version*: a member added or removed anywhere shifts every later one, so the number for `send_udp` is a fact about one `api/<version>/` and nothing may hardcode it (CLAUDE.md's api-pin rule, which is there because a guest built against one description and packaged against another calls the *wrong* member, silently, in a lockstep game). The generated symbols above are the stable surface: they survive a pin bump, and the constant behind each one moves under them. All five are present in both languages, member id for member id, at both committed pins — re-checked after the 2.1.14 regen and again after the revert to the 2.0.77 default, which is what makes the engine-gated floor legal: the members exist in the GA description, only the ENGINE has to be new. Nor should file:line refs into the generated bindings be trusted — `fkapi.go` is four million bytes and every offset in it moves with the description.

So the work is **policy, ergonomics and a test story**, not plumbing. What follows is the policy.

### What the probe established (2026-08-06) — authoritative over every TBD-PROBE below

`scripts/run-ipcprobe.sh` ran the 13-arm study against the installed 2.0.77. Where a TBD-PROBE marker below disagrees with this table, this table wins; full evidence is `scratch_tmp/ipc-research/probe-findings.md`.

| question | answer |
|---|---|
| headless inbound | **BROKEN — the 2.1.9 crash exists on 2.0.77.** `recv_udp(0)` or `recv_udp()` with a packet queued kills a headless server at `TickClosure.cpp:91`, a C++ abort no pcall can catch, deterministically. The crash needs BOTH the pump call and a queued packet. `recv_udp(1)` — a player who does not exist — is a safe no-op. `--benchmark` delivers nothing at all, no crash: `players=0 connected=0 multiplayer=false`, so there is nobody to read FOR. **On 2.0.77 there is NO headless environment with working inbound UDP.** |
| outbound | **fully healthy.** All 256 byte values exact, in every LocalisedString form, both directions of the byte-value test; ten sends in one tick all delivered, in order; no coalescing; source port = the `--enable-lua-udp` port; `send_udp` never crashed anything. |
| outbound ceiling | **9,188 B on macOS (`net.inet.udp.maxdgram` − 28), and over it the send FAILS SILENTLY** — no error, no raise, nothing on the wire. The library must enforce its own cap; the engine will not say anything. 3,900 is comfortably under every OS's floor. |
| LocalisedString shape | a bare string crossed byte-perfect on a peerless server, but a bare string IS a locale key wherever someone can localise it — **the library sends `{"", frame}`**, the literal-concat form, which never localises. |
| `for_player` polarity | `0` works on a headless server and is a **silent no-op under `--benchmark`** (no server exists there); omitted works under benchmark. `for_player=1` on a server with no players: silent no-op, no error. The profile config owns this. |
| event shape / inbound sizes / latency / drain | **UNVERIFIED — no `on_udp_packet_received` fired in any arm**, because no environment the harness can drive delivers inbound. The dump machinery is in place and answers these the moment one does. |
| event id | **208 at runtime, 207 in the bindings** — two namespaces, both correct (the recorded 110-vs-74 shape). Nothing to fix; the bindings' `Subscribe` handles it. |

**The consequence the library MUST carry: pumping is fatal where it is not useless, on 2.0.x.** So `Pump` must not call `recv_udp` unless the engine is one where that is known safe: `Open` reads the base-game version (deterministic — identical on every peer) and refuses below the floor. **`MinEngineVersion = 2.1.14`, and it is a measured floor**: the crash is confirmed present at 2.0.77, reported upstream at 2.1.9, and verified FIXED AND DELIVERING here at 2.1.14 — versions between are unverified, so the floor is what was measured. The graphical-client path is unprobed here but carries strong ecosystem evidence (blueprint-share and ~12 others receive on clients in production).

**AS BUILT THE REFUSAL IS TOTAL, AND THE SEND-ONLY DESIGN THIS RECORDS WAS WRONG.** Until 2026-08-07 the library ran the session send-only below the floor — `HELLO` still went out, on the reasoning that outbound is healthy on every version and free by the cost model. That is true of the DATAGRAMS and false of the PROTOCOL: a session is established by a `HELLO_ACK`, an ACK arrives through the INBOUND path, and inbound is exactly what is shut off. So a send-only link searched at `SearchTicks` forever, never came up, refused every `Send` it was handed for want of a session, and produced a steady trickle of frames no peer could answer — while telling its author nothing beyond a counter nobody reads. Below the floor the link is now **inert**:

| | |
|---|---|
| `Open` | returns `StatusDisabled` / `Status::Disabled`, having logged **one** line: `fkipc: disabled -- requires Factorio >= 2.1.14; this engine is X`. Byte-identical in both languages |
| `Pump` | does nothing. No poll, no HELLO, no heartbeat, no flush, not one datagram |
| `OnEvent` | returns false, claiming nothing — another IPC mod's datagram still reaches this mod's dispatcher through the one shared socket, and a disabled link must not swallow it |
| `Send` / `Snapshot` / `Request` / `WriteBulk` / `NotifyFile` | `StatusDisabled`, counted in `Stats().Refusals`. `WriteBulk` refuses BEFORE the write, so the file/notify pair stays together |
| `Stats()` | `Enabled` is the verdict, `BaseVersion` what was read, `Refusals` the count. `RecvEnabled`/`RecvRefused` are gone with the mode they described |

**`StatusDisabled` rather than `StatusNoSession`**, which is the one design choice in it worth arguing. `NoSession` is the quiesce shape and means "the peer is down, it may be back this second" — transient, and a guest is told to keep playing and not branch on it. Below the floor the session can NEVER come up, because an engine cannot change under a running game, so reporting a permanent condition with a transient one's name invites a guest to spin on it. `StatusNotOpen` was the other candidate and means "you did not call `Open`", a programming mistake fixed in source; here the author did everything right. The counted-no-op property is kept either way: a `Status` is not an error a mod must handle at every call site.

**The gate is re-read while it is SHUT and never once it is open**, at `SearchTicks`, from the replicated tick — so a save made on an old engine and loaded on a new one comes up by itself. It is monotone within a session (Factorio refuses a save written by a newer build, so a restored "the link may run" can only have come from an engine at or below this one), and the condition is guest state plus the tick, which is what makes it join-safe where an `fk_after_load` one-shot would not be.

**AND THE FLOOR IS ON THE ENGINE, NOT ON THE API PIN.** Those are separate axes and the distinction is load-bearing now that the default pin is the general-availability release: the pin decides which `runtime-api.json` the bindings and the packaged member table came from, at BUILD time, while this reads `helpers.game_version` at RUN time. Every member this library calls — `send_udp`, `recv_udp`, `write_file`, `game_version`, `on_udp_packet_received` — is in the 2.0.77 description, which shipped with 2.0.59. So a mod built at the default GA pin gets the whole library on a 2.1.14 engine, with no rebuild, no repin and no second build of the guest, which is exactly what `scripts/run-ipc.sh` packages and runs.

**`scripts/run-ipc.sh` and `scripts/run-ipcdemo.sh` REFUSE TO START below the floor**, reading the constant out of `guest/go/fkipc/version.go` through `scripts/lib-engine.sh` rather than spelling it a second time. With the hard-disable there is nothing for those gates to observe down there, and every leg would sit at its deadline and then report a protocol failure for an engine that is merely too old — which is the reported-red twin of the skip-that-reads-as-a-pass this repo has been bitten by twice.

### The 2.1.14 re-run (same day) — inbound is now measured too

The install moved to 2.1.14 and the critical arms re-ran. Everything the 2.0.77 table above marks UNVERIFIED is now answered:

| question | answer on 2.1.14 |
|---|---|
| headless inbound | **WORKS.** The 2.0.77 killer arm survived 25 s with **467 events delivered**; full handshake; sustained inbound through the InputAction path. `--benchmark` still delivers nothing, ever — the live gate stays on `--start-server`. |
| event shape | exactly the five documented fields — `name`, `payload` (string, byte-exact), `player_index` (0 on server), `source_port`, `tick`. No metatable, no extras. Runtime `name` is **212 on 2.1.14** against 208 on 2.0.77 — and the bindings' own constant was 207 under the 2.0.77 pin and 212 under the 2.1.14 one. Several numbers, one event, which is why nothing may ever hardcode any of them: the generated `Subscribe` constant is the only spelling a guest may use. |
| inbound size ceiling | **between 4,000 and 8,192 B** — 4,000 arrives byte-exact, 8,192/16,384/65,000 silently never arrive. `MaxFrameCeiling = 3900` clears the inbound wall, the scratch region and every OS's outbound cap, and is CONFIRMED as the protocol maximum. |
| drain | 20 packets blasted in 0.34 ms all arrived **in one tick, in order, complete** — a per-tick pump drains the backlog as a batch of events. One `recv_udp` per tick is the shape; `DrainMax` survives only as a safety valve. |
| binary inbound | **byte-exact**, all 256 values including NUL. Both guest libraries get that through the GENERATED reader as of 2026-08-06: the Rust mirror used to read the (pointer, length) pair itself because `get_str` was `from_utf8_lossy`; string fields are `LuaStr` (bytes) now and the scan is deleted. See `agents/abi.md`, "A Lua string is BYTES". |
| latency | median **31.5 ms ≈ 1.9 ticks**, min 8.4, p90 94.8, headless server, InputAction path. |

### The graphical single-player re-run (2026-08-07) — and the two things that were blamed on focus

Every measurement above was taken on a headless server, and `scripts/run-ipcdemo.sh --play` was reshaped around a claim about the other environment: *single player stops ticking when the game window loses focus*. **That claim is false**, and both real causes were hiding behind it. The evidence for it was confounded twice over — the dead runs were background-script launches of a `--create`d freeplay map running fkipc mods, and the live control was a bare-Lua mod in a window that took focus, so *three* variables moved together.

**Focus is not a variable.** A bare-Lua mod logging its own tick, 45 s a phase, window state read from `lsappinfo front` (2.1.14, graphical single player, `--load-game`):

| audio | phase 1 | phase 2 | phase 3 |
|---|--:|--:|--:|
| on | focused **59.87** | defocused **60.00** | refocused **59.87** |
| off (`--disable-audio`) | focused **59.87** | defocused **59.87** | refocused **59.87** |
| on | never focused from birth **59.87** | activated **61.30** | defocused **59.87** |
| off | never focused from birth **60.00** | activated **59.87** | defocused **59.87** |

Sixty ticks a second in every cell, including a window that was never key from the moment it existed. Audio makes no difference, so App Nap is not the mechanism and `NSAppSleepDisabled` is not a lever — there is nothing for it to disable. **`--benchmark`-style throttling, occlusion and minimisation were not what was happening either**: the stalled process sat at 10–15% CPU with 39 threads, i.e. rendering normally while the map stood still, which is what a *paused* game looks like and not what a suspended one does.

**Cause 1 — a fresh freeplay map opens a MODAL at tick 750, and a modal pauses single player.** `base/script/freeplay/freeplay.lua`'s `show_intro_message` is:

```lua
if game.is_multiplayer() then player.print(...)
else game.show_message_dialog{text = ...} end
```

It is called from `on_cutscene_waypoint_reached` when the crash-site intro ends. Measured: **60 ticks/s to tick 750, then 0.00 ticks/s indefinitely, with the window focused and frontmost** — and the last event logged before the stall is `on_cutscene_cancelled tick=750`. `remote.call("freeplay", "set_skip_intro", true)` before that tick removes it and the identical map then runs forever in every focus state, which is the table above. **This is also why the headless topology looked like a cure**: a server takes the `player.print` branch and has no modal to open. It binds any harness that `--create`s a freeplay map and then loads it in single player with nobody to click.

**Cause 2 — fkipc could not hold a session in single player at all, and it was ours.** `Open` built the transport from the raw `Config`:

```go
func Open(cfg Config) Status {
	tr, st := newTransport(cfg)   // reads cfg.ForPlayer -- still 0
	...
	return pkg.configure(cfg, tr) // normalises ForPlayer 0 -> -1, on its own copy
}
```

`newTransport` sets `sendFP` from `cfg.ForPlayer >= 0`, so a `ProfileClient` guest that never set `ForPlayer` sent **every frame with `for_player = 0`** — "the server if present", which on a headless server is the server and in single player is a **silent no-op**. ProfileClient's entire reason for existing never reached the send path. The receive half was keyed on `Profile` directly and was correct, which is why the symptom is a guest that pumps happily and is never heard. **Both languages had it, line for line** (`guest/rust/fkipc`'s `open` calls `new_transport(cfg)` then `configure(cfg, tr)` the same way). Fixed in both by normalising before the transport is built, with the rule in one function per language.

Why nothing caught it: **every host-side test goes through `Attach`, which takes a transport the test built**, so `newTransport` — the only place the defect lives — is not on any tested path; and the only in-game gates are `run-ipc.sh` and `run-ipcdemo.sh --smoke`, both headless servers, which is exactly the one environment where `for_player = 0` is right.

**The engine surface is entirely healthy in graphical single player**, measured with `testdata/ipcprobe` in a `--load-game` client (`multiplayer=false`, `players=1 connected=1`):

| | result in graphical SP |
|---|---|
| outbound | **31 datagrams delivered**, ASCII and all 256 byte values, in order, ten in one tick |
| outbound ceiling | 9,188 B arrives, 9,216 and above silently does not — the same macOS `maxdgram − 28` wall as headless |
| `for_player = 0` | **silently dropped** — there is no server. This is cause 2's mechanism, measured directly |
| `for_player` omitted | delivered |
| `for_player = 1` | delivered — the local player exists here, unlike on a peerless server |
| inbound | `on_udp_packet_received` fires with `player_index = 1`, correct `source_port`, byte-exact payload; round trip ~30–90 ms |
| pump form | bare `recv_udp()` delivers; no crash in 75 s |

And with the fix in, the full demo holds in graphical single player: both mods `up`, `sessions=1` each, telemetry at **57 frames/s of guest tick**, and slider RPCs acked **while the window was not focused**.

#### And it is a GATE now, not a re-run — `run-ipcdemo.sh --smoke-single`

The paragraph above was a one-off measurement, which is exactly the state the defect it describes had already survived. `--smoke-single` runs the full 20-leg demo conversation against **one graphical single-player process** — no server, no client, no join — with no human input anywhere in it, and `--play --single` is its interactive twin. Four things about it are worth carrying:

- **It waits for the game to pass tick 750 before it says a word.** The scripted conversation is over in about **seven seconds of game time** (measured: `guest_tick=392` at the final STATS line), so a gate that starts talking when the socket opens finishes four hundred ticks short of the dialog and reports green having never met the thing it exists for. The first build did exactly that. Waiting first also means every fkipc leg is a measurement taken on the FAR SIDE of the dialog, and a regression fails ONE leg naming the dialog rather than nine legs blaming fkipc.
- **The clock witness is bare Lua.** `fk-demo-nointro` is scaffolding the script writes into the single-player mod directory (the `fk-savetrigger` precedent), with no wasm in it, so `[fk-nointro] tick=N` is evidence about the ENGINE's clock rather than about fkipc's reading of it. Its `on_init` — which runs at `--create`, before any player exists — calls `set_disable_crashsite(true)` and `set_skip_intro(true)`, and **where those calls happen is the whole design**: freeplay creates the crash site and starts the cutscene inside `on_player_created`, so a first-TICK call is already too late for the cutscene and only removes the dialog at the end of it. A first-tick retry plus a `player.exit_cutscene()` fallback covers a map created without the mod. The single-player map is therefore **recreated on every run**: this mod's behaviour is saved INTO it.
- **The negative control was run.** With both remote calls disabled and the heartbeat left in, the game's last logged tick is **750, indefinitely** (`ctrl=6`, cutscene controller, `screen=1`), which is the 2026-08-07 measurement reproduced as a property of the harness rather than a memory. Note for anyone repeating it: pressing Tab dismisses the dialog and the game resumes, so an 18-second stall in a log is a **human**, not a finding.
- **Teardown is a leg.** This is the first in-game gate here whose game is started through a subshell, and `exec` is what makes `$!` the game's own pid; without it every kill reaches a shell that has already forked and an orphaned graphical Factorio keeps holding the user directory's lock. The leg asserts the process is gone, that no `factorio` still names this run's mod directory, and that `$USERDIR/.lock` can be `flock`ed. The same `exec` fix went into `--play --launch`'s client, which had the same hazard.

What it does **not** cover, stated so nobody reads more into it: there is no join in single player, so the peer-local-write rule is still gated only by `--play --launch` and by `TestAJoiningPeerStaysByteIdenticalToTheServer`; and its `rpc-binary` leg proves an all-256-byte-value payload is DELIVERED and answered, not that every byte arrived unchanged — `run-ipc.sh`'s `bytes` leg is the byte-exact one, because its guest echoes.

### The determinism cost model, which decides everything by DIRECTION

This is the one fact a reader should carry out of this file if they carry nothing else, because every asymmetry below follows from it.

**Outbound is free.** `send_udp` and `write_file` are local side effects. Every peer in a lockstep game executes the same guest code and would perform the same send; `for_player` is the knob that says which peer's copy actually goes out. Nothing about an outbound frame enters game state, so nothing about it is replicated, saved into a replay, or assigned to a tick.

**Inbound is expensive.** A received datagram becomes an InputAction. It is replicated to every peer through the multiplayer server, it lands in the replay, and it is quantized to a tick — the API's own description says so:

> …in case of multiplayer game with many players, all this data will have to go through the multiplayer server and be distributed to all clients. — `api/2.0.77/runtime-api.json`, `LuaHelpers::recv_udp`

That is the whole design brief: **talk a lot, listen a little, and make the listening idempotent**, because the listening is the part that costs a populated server real bandwidth and costs every profile ≥1 tick of latency.

It also buys something. Inbound data arrives at every peer identically, at the same tick, through the engine's own replication — so a guest may branch on it without desyncing. That is what makes [the epoch handshake](#sessions-and-the-epoch) legal, and it is the one place in this design where the expensive direction pays for itself.

### The rule the cost model implies, which this design got wrong once

**NO PEER-LOCAL SIGNAL MAY MUTATE GUEST STATE.** Under the default `--persist=table`, guest memory **is** `storage.fk_mem`, and Factorio CRCs it across every peer. So the only things a guest may branch on *when it writes* are its own state, the replicated tick, and what arrived through the replicated inbound path. Everything else is a desync with a delay fuse.

`fk_after_load` is none of those. `fk_mod.lua` arms it as a one-shot `on_tick` from `script.on_load`, and Factorio runs `script.on_load` **on every peer that loads the state** — which includes a client joining a game already in progress. The server ran it when it started; the joiner runs it on its first simulated tick and nobody else does. `fk_mod.lua` says the rule one level up, for the hook this one hangs off:

> on_load is READ-ONLY with respect to storage, and has to be: Factorio runs it on every client when joining a multiplayer game, and a write here is a desync waiting to happen. — `runtime/lua/fk_mod.lua`, above `state_load`

A one-shot armed from `on_load` is a write from `on_load` with one tick of delay, and the whole of the original session-reset-on-load design was written on the other side of that line. **Measured on 2.1.14** with `scripts/run-ipcdemo.sh --play`: sessions up on the server, a graphical client joins over `--mp-connect`, the client logs `fkipc session reset` and the game logs `Multiplayer desynchronisation: crc test failed` from the very next tick, repeating, with a desync report generated. The fix is [below](#what-each-side-does-on-a-session-boundary): a load does nothing at all, and every session boundary is driven by a replicated signal.

**This is not a rule about IPC.** It binds any mod that writes guest state from `fk_after_load`, and `agents/guests.md` carries it where a guest author will find it.

#### The second instance, which is not a HOOK but a STATUS

The load-reset was the obvious peer-local write. The other one was in the hottest path in the library and had been there since wave 2a:

```go
if l.tr.Send(f) == StatusOK { l.stats.TxFrames++; ... } else { l.stats.QueueDrops++ }
```

**Whether `send_udp` succeeds is a fact about the peer's command line.** `--enable-lua-udp` is what binds the socket; a headless server in this project has it and a graphical client joining that server does not. So the two peers took different branches on every frame and wrote different words into `storage.fk_mem`. `WriteBulk` was worse than a miscount: it *returned early* on a failed `write_file` and skipped the `FILE_NOTIFY`, which consumes the channel's seq — so one peer would advance the counter and the other would not.

Both now attempt and ignore. `TxFrames`/`TxBytes` count what the link **attempted**, which is a deterministic function of guest state; what is lost is seeing a failed send in `Stats`, and that is the right trade in a direction the cost model already calls FREE — an outbound frame never enters game state, so its fate is not something guest state may have an opinion about.

**The general form, and it is the one to carry:** *outbound is free* is not only a statement about bandwidth. It says an outbound call's OUTCOME is per-peer, so a guest may perform one from anywhere and must never store what happened. Any host call whose success depends on how this peer was launched belongs in the same class. *Enforced by `TestAFailedSendIsInvisibleToGuestState` and its Rust mirror, which drive two links that differ in nothing but whether their transport works and require every counter to match — both confirmed to fail before the fix, with `TxFrames:13/TxBytes:448` against `QueueDrops:13`.*

**And the seam now has NO VALUE TO BRANCH ON, which is what turns that from a discipline into a property.** `Transport.Send` and `Transport.WriteFile` return **nothing** — `fn send(&mut self, frame: &[u8])` in Rust, `Send(frame []byte)` in Go — on the trait/interface and in the wasm implementation behind it. A comment saying "these count what the link attempted" is exactly the kind of thing this repo has watched lose to a plausible-looking edit (the dead loop-guard seed, the missed page mark, and this defect itself); a void return cannot lose, because `if l.tr.Send(f) == StatusOK` no longer compiles.

`Status` stays everywhere it describes a **deterministic refusal** — a full queue, an oversized message, a link that is not open, a build with no transport, a transport out of the link during a poll — because each of those is a function of guest state and therefore the same answer on every peer. That is the whole classification: **transport outcome is void, guest-state refusal is a Status.**

*Enforced by `TestTheOutboundTransportSeamHasNoReturnValue` (Go) and `tests/seam.rs`'s `the_outbound_transport_seam_has_no_return_value` (Rust), each a TEXT property over the declarations. Their two halves have different teeth and that is deliberate: putting a result back on the interface/trait does not reach the assertion at all — it fails to compile, because every test double implements it — while the wasm arm is behind `//go:build tinygo.wasm` / `#[cfg(target_family = "wasm")]`, is never type-checked against the seam by `go test` or `cargo test -p fkipc`, and is caught by nothing else. Confirmed by mutation in both languages.*

### The join-safety contract — the authoritative list

Everything above in the form a mod author needs it, and it is mirrored verbatim in both package docs (`guest/go/fkipc/doc.go`, `guest/rust/fkipc/src/lib.rs`) so that an author who never opens this file still gets it. A joining multiplayer client downloads guest memory and then simulates alongside every other peer; Factorio CRCs that memory every tick.

**A guest MAY branch on, and store what it decided:**

| | why it is safe |
|---|---|
| a `MSG` / `REQ` / `RESP` payload | inbound is an InputAction: replicated to every peer, at the same tick |
| a session event | every one of them is driven by a replicated signal — a `BYE`, the liveness test, or the guest's own clock |
| the tick handed to `Pump` | replicated by construction |
| `Stats` / `stats()` | every field is a function of the three above, of build-time configuration, or of this link's own decisions. **That is a constraint on what may be counted, not an observation** |
| its own guest state, and the world read through `fkapi` | the world is game state and is identical on every peer |

**A guest MUST NEVER store:**

| | why it is a desync |
|---|---|
| whether an outbound host call SUCCEEDED | `send_udp`, `write_file`, `rcon.print`. `--enable-lua-udp` binds the socket; a joining graphical client has no such flag, so two peers take two branches on every frame. **Attempt it and drop the answer** |
| anything computed in `fk_after_load` | it fires on the joining peer and on no other. A one-shot armed from `on_load` is a write from `on_load` with one tick of delay |

**The sanctioned sink for a per-peer fact is `fk.Log` / `fk::log`.** The game log is not CRC'd, is not in the save, and is per-peer by nature — which is exactly where "did my write land" belongs. There is no other legal home for it.

**`WriteBulk`/`write_bulk` is the worked example of the pattern**, and it is worth copying rather than only reading: it attempts the write, ignores the outcome by construction, and sends the `FILE_NOTIFY` **unconditionally**, because the notify consumes the channel's seq and a peer that skipped it would advance that counter differently from its neighbours — guest state diverging *and* a permanent gap at the far end, from one early return. `NotifyFile` is the same shape over a file the engine wrote.

#### What the in-game bisection actually said, in order

Worth recording because three of the four steps were about *ruling something out*, and the last one was a confound in the harness rather than in the code:

| run | result |
|---|---|
| demo mods + companion, client joins | **desync** at the first tick it simulated — and **no `fkipc session reset` line**, which is the load-reset fix working and something else still wrong |
| demo mods, **no companion at all** | **desync** — so it is not inbound, not the event encode, not anything a datagram touches |
| a **non-IPC** guest (`examples/hello`, mutating guest memory every tick) | **clean, 75 s** — so the runtime and the ABI are join-safe for an ordinary guest |
| `examples/ipc` alone, after the send-status fix | **clean, 75 s** — the library is join-safe |
| both demo mods, no companion, **stale map** | desync — the map had been created with the pre-fix wasm, so the build id had moved and the load took the rebuilt-guest path |
| both demo mods, **fresh map** | **clean** |
| the full `--play --launch`: server + companion + graphical client | **clean, 120 s joined, 15 slider RPCs, telemetry reading back, `sessions=1` on both mods** |

The stale-map row is the one to be careful with next time: **a map created with an older build of the guest — or against another `--api` pin — is not a valid control**, because `same_build()` then fails and the load discards the heap instead of adopting it, which is a different code path from the one under test. The pin counts since 2026-08-07: the build stamp folds the resolved API version in beside the module's digest, so repackaging against a different pin moves the stamp exactly as recompiling the guest does.

**And that is no longer something anybody has to remember.** The trap cost two runs when it was first recorded and then cost a third the same day the stamp fold landed — every cached map in existence became stale at once, and the run that met it desynced from the first joined tick with **no warning on either peer**, because `on_configuration_changed` fires on a mod's VERSION changing and a dev rebuild keeps the version. So `run-ipcdemo.sh` now CHECKS rather than trusts: after packaging it reads each module's `build = "…"` stamp out of `fk_module.lua` and looks for it in the cached map's `script.dat`, and on a mismatch recreates the map and says `the map was built by another build of these mods; recreating it`. A substring search is enough — the stamp is a 16-hex literal and Factorio writes `storage` strings verbatim. Same-build maps are still reused, `FRESH=1` still forces a rebuild, and a bare-Lua mod with no module to stamp is simply not something the check can speak about. Verified positively (a fresh map is judged current), negatively (a stamp rewritten inside `script.dat` makes the guard fire and the run go green afterwards) and end to end in both the `--smoke` and `--play --launch` arms.

The failure mode it removes is worth stating once, because it is asymmetric and that is what made it read as a guest defect: the SERVER declines to adopt and runs on from tick 0 quite happily, and the joining client declines too and rebuilds a **tick-0 heap against a world already at tick 1250**. Only the join can see it, and what it looks like is a desync.

#### ...and the RUNTIME defect underneath it, which the map guard was papering over

The guard above is right and is kept, because a stale map is still not a control for anything. But it was written as though the decline were the whole story, and it was not: **the decline was never FINISHED**, and that is a defect in `fk_mod.lua` rather than a property of the experiment. Root-caused from the same 2026-08-07 desync and fixed the same day.

`state_load` declines when `same_build()` is false and — correctly — cannot write `storage`, because it runs from `on_load` and that is peer-local. Everything that finished the job lived in `on_configuration_changed`: the `fk_migrate`/`fk_migrate_adopt` dispatch, the warning, and the `state_init` that republishes `storage.fk_build`. **Factorio raises that hook when the mod SET changes — for one mod, when its VERSION moves — and a build stamp moves for a dev rebuild, a `--gc` change, or a repackage against another pin.** So on every same-version rebuild the hook never fired and three things were left behind:

1. **the save stayed permanently self-inconsistent.** Nothing republished `storage.fk_mem`, so `storage` held the previous build's heap while the guest ran on the fresh one `_initialize` built. Two unrelated tables — the guest's writes reached neither the save nor the CRC. Under `--persist=packed` it is worse: `pages` is a local `state_load` never set, so `sync_memory` flushed *nothing* for the life of the session.
2. **every later load declined again**, off the stamp nobody republished.
3. **`fk_migrate` and `fk_migrate_adopt` never fired.** The guest was never told.

That is why the stale-map runs desynced with **no warning on either peer** — the warning was in the hook. It is also why "recreate the map" was the only remedy anybody found: nothing a running game did could ever heal the save.

**As built**, the decision is still taken at load and the ACT is deferred to the first **replicated** execution point: `state_load` sets an upvalue (a write `on_load` may make), and the first OUTERMOST DISPATCH calls `finish_rebuild` — the same function `on_configuration_changed` now calls, so the two paths cannot drift. The flag is set **before** the adopt gate, because the `fk_migrate_adopt` arm falls through that gate and is owed its notification too; `state_init` clears it, so `on_init` and the hook cannot double-fire.

The determinism argument is the one this whole document is built on, applied one level down: **the trigger is a function of the loaded state alone** (`storage.fk_build` against `P.build`), so every peer that loaded the same bytes computes it identically, with no clock and no peer-local signal — unlike `fk_after_load`, which is armed on whichever peers happen to load. A peer joining LATER downloads post-republish state and declines nothing.

**The residual window is real rather than theoretical and it is worth naming here, because this document's own gates walk into it.** A peer can join a server that has declined and not yet dispatched anything — indeed `auto_pause` defaults to **true**, so a headless server sits at its load tick until somebody connects, which is that window every time. It is safe by the same argument: both peers then hold the same flag over the same state and settle it at *the same dispatch*, because every remaining source of one is replicated and the one peer-local source (`fk_after_load`) is itself a dispatch. What must NOT be leaned on is the earlier opportunity — `on_configuration_changed` is exactly what a same-version rebuild does not get, which is the whole defect. (`run-ipc.sh` sets `auto_pause: false` and is single-peer, so it never sees this; `run-ipcdemo.sh --play` is the arm that joins a client, and it recreates a stale map rather than loading one, which is the guard above still doing its job.)

*Enforced by `TestAJoiningPeerStaysByteIdenticalToTheServer`'s **stale** arm, which boots the server from a save carrying another build's stamp with no `on_configuration_changed`, in all four language/mod arms. It asserts on the SERVER first and deliberately: when neither peer republishes, both hold a deep copy of the same frozen heap that neither is running on, so the tick-by-tick comparison reports IDENTICAL over two guests with nothing in common — the arm's own vacuity trap, met from a new direction. Confirmed to fail on `stamp another-build-of-this-guest`, `memsum` unmoved across 135 ticks of traffic, no rebuild line in the log, and `app B nil` (the joiner's guest never reached a telemetry frame). Plus, host-side and toolchain-free, `TestARebuiltGuestIsToldWithoutOnConfigurationChanged`, `TestARebuiltPackedGuestRepublishesItsPages` and `TestASameVersionRebuildIsStillHandled`.*

### The two driving profiles

They are held side by side deliberately, because they invert each other's defaults and a design tuned to either one alone would be wrong for the other.

**The server profile (Clusterio-shaped).** A headless server, `for_player = 0`, one authoritative peer injecting. Telemetry-dominated: entity and production state streaming out continuously, control coming in rarely. Its binding constraint is the **~6 kB/s inbound wall** once even one player is connected (~100 B/tick of InputAction segment rate; 1.5 kB/s at ≥20 players) — so on this profile inbound is a *control channel*, never a data channel, and anything bulk goes outbound or does not go.

**The interactive-agent profile (Vibetorio-shaped).** A graphical client, single player, a model in a side process, push-to-talk. Here the walls move: RCON becomes the *worst* inbound channel (hidden local-rcon-socket configuration is consumer-unacceptable friction) while `--enable-lua-udp <port>` is one Steam launch-option line; UDP receive is nearly free because there is no replication fan-out and the latency floor is ~1 tick rather than ~6; screenshots exist (headless no-ops them); and **pause is the hazard** — the pump stops, the OS buffer fills silently, and the guest must drain on resume. **Pause is a much wider hazard than the menu key**, which the single-player re-run above measured: `game.show_message_dialog` pauses single player, freeplay opens one at tick 750 of a fresh map, and the game sits there at 60 fps with the map stopped for as long as nobody clicks it. A modal any mod or scenario opens has the same effect, and on this profile there is no second peer to keep the world moving. What is NOT a hazard, measured: window focus, minimisation from birth, and audio — graphical single player ticks at 60/s alt-tabbed.

Their traffic classes, which is what the frame types are for:

| flow | dir | size | latency | profile |
|---|---|---|---|---|
| state stream (deltas, snapshots) | out | 1–50 KB | sub-second | both |
| RPC (tool calls, control) | both | < 4 KB | few ticks | both |
| push-to-talk / hover context | out | < 200 B | < 100 ms | agent |
| plan / bulk command | **in** | 1–20 KB | seconds | agent |
| screenshots, full dumps | out | 100 KB–MB | seconds | agent |

The last two rows are why this is not a telemetry protocol. Inbound bulk has no file path — there is no file-read API — so it must fragment over UDP; outbound bulk has one and should use it.

---

## The wire format

### Framing

**One datagram is one frame. Frames are never split across datagrams, and a datagram never carries two frames.** UDP delivers a datagram whole or not at all; anything else in this design is either the message layer above (which fragments *messages* into frames) or a stream transport we do not have.

A frame is a **24-byte header** followed by `length` payload bytes.

| off | size | field | |
|--:|--:|---|---|
| 0 | u16 | `magic` | bytes `'F','K'` — LE `0x4B46` |
| 2 | u8 | `version` | protocol major; **1** |
| 3 | u8 | `type` | frame type, below |
| 4 | u16 | `flags` | bitfield, below |
| 6 | u16 | `channel` | 0 is the protocol's own; 1–65535 are the app's |
| 8 | u32 | `epoch` | session id — see [Sessions](#sessions-and-the-epoch) |
| 12 | u32 | `seq` | per-channel, per-direction frame counter |
| 16 | u32 | `corr` | correlation / message id; 0 = none |
| 20 | u16 | `length` | payload bytes following this header |
| 22 | u8 | `frag` | fragment index, 0-based |
| 23 | u8 | `nfrag` | fragment count; 1 = not fragmented |

**DECIDED — little-endian.** wasm linear memory is little-endian by specification, so a guest reads a header field with a plain load and no swap; the tier-2 ABI wire is already a LE C struct in guest memory (`agents/abi.md`, "The wire is a C struct"); the runtime's own packing is `string.pack("<I4", …)`; and every peer that will ever speak this is x86-64 or arm64. Network byte order would cost both ends a swap to please neither.

**DECIDED — every field is naturally aligned within the frame, and the guest still parses it byte-wise.** The offsets above are free to choose, so they are chosen aligned. They do not *buy* an aligned load, because the frame's base address is not under the guest's control: an inbound payload lands wherever `scratchTop` happens to stand, and `fk_abi.lua:1100-1102` bumps that pointer by the exact string length with no rounding. A misaligned 4-byte load in generated Lua takes the checked slow arm (`agents/optimizer.md`, the inlined load's `t0 % 4 == 0` test), so the guest reads header fields through an explicit little-endian byte reader — six shifts against a 12.5 µs host call, which is not a cost worth designing around. Rounding the scratch bump to 8 would make the base aligned and is a one-line host change; phase 1 declines it and the layout above is what makes it a *free* change later. **OPEN** — worth taking in a phase that is already editing `fk_abi.lua`.

**DECIDED — `length` is carried even though UDP already knows it.** It is redundant on a datagram transport and that redundancy is the point: `length != len(datagram) - 24` is the cheapest possible detector for truncation, coalescing, and a peer that has desynchronised its idea of the format. The rule is absolute — **a mismatch drops the frame and increments a counter; it never produces a partial parse.** It also lets the same framing ride a stream transport later without a format change, which is the transport-neutrality hedge proposals.md kept.

**DECIDED — 2-byte magic, not 4.** The socket is a shared local port and anything on the machine can send to it, so the magic's job is rejecting junk. Two bytes give 1/65536 alone, but the acceptance test is compound — magic, a version we speak, a type in range, `length` agreeing with the datagram, and an epoch we recognise — which is far stronger than four bytes of magic and two bytes cheaper. ASCII `FK` also means a hexdump identifies the protocol.

### Frame types

| | name | payload | |
|--:|---|---|---|
| 0x01 | `HELLO` | protocol-defined | guest → peer, opens a session |
| 0x02 | `HELLO_ACK` | protocol-defined | peer → guest, mints the epoch |
| 0x03 | `HEARTBEAT` | protocol-defined | both, liveness + flow control |
| 0x04 | `MSG` | **opaque** | fire-and-forget, seq'd, gap-detectable |
| 0x05 | `REQ` | **opaque** | correlated request |
| 0x06 | `RESP` | **opaque** (or an error record) | correlated response |
| 0x07 | `FILE_NOTIFY` | protocol-defined | "there is a file at X" |
| 0x08 | `RESYNC` | none | "channel N is stale, send me a snapshot" |
| 0x09 | `BYE` | none | advisory clean shutdown |

**An unknown type is dropped and counted, never guessed at.** Same for an unknown `version`, which additionally logs once per session rather than per frame.

**DECIDED — `MSG`/`REQ`/`RESP` payloads are opaque bytes and the protocol says nothing about their encoding.** JSON is available on the guest side (`helpers.table_to_json`, member 2155) and is what most apps will reach for; requiring it would put a JSON encoder in the guest's hot path for apps that want a packed struct, and would make the protocol's own conformance tests depend on a JSON library's escaping rules. Control frames (`HELLO`, `HELLO_ACK`, `HEARTBEAT`, `FILE_NOTIFY`) have protocol-defined payloads because both ends must agree on them and there is nothing app-shaped about them.

**Determinism note, and it is a real trap rather than a formality.** Anything the *guest* assembles must be deterministic. A JSON object built by iterating a Go `map` produces a different byte string on different peers, and while `for_player = 0` means only one peer's bytes reach the socket, the *guest state* that produced them — a buffer, a length, an allocation — differs on every peer, and that is a desync. This is the same rule that made every dictionary in the generated bindings an ordered pair slice rather than a map (`agents/abi.md`, "A dictionary field inside a struct"); it applies unchanged here. **Encode from ordered structures only.**

### Flags

| bit | name | on | meaning |
|--:|---|---|---|
| 0 | `RETRY` | `REQ`, `RESP` | a retransmission — lets the peer count dedup hits and a log tell "slow" from "lost" |
| 1 | `ERROR` | `RESP` | payload is an error record, not a result |
| 2 | `SNAPSHOT` | `MSG` | payload is a complete state, not a delta — clears the receiver's gap condition |
| 3 | `HAS_DIGEST` | `FILE_NOTIFY` | the notify carries a length and checksum the peer can verify |
| 4–15 | — | | reserved; a sender sets 0, a receiver ignores unknown bits |

Unknown flag bits are **ignored**, unlike unknown types and versions. A flag is by construction an optional refinement of a frame the receiver already understands; a type is not.

### Control payloads

`HELLO` / `HELLO_ACK`, in header order:

```
u8  proto_min, u8 proto_max     versions this side speaks
u16 max_frame                   the largest frame I will ACCEPT
u16 max_fragments               the most fragments I will reassemble
u32 boot                        guest: the load counter. peer: 0
u32 tick                        guest: current tick. peer: last tick it saw
u8  profile                     0 = server, 1 = client
u8  reserved
u16 name_len, name[name_len]    the mod name, for logs and multiplexing
```

`HEARTBEAT`: `u32 tick, u32 rx, u32 drops, u32 gaps` — frames accepted, frames dropped and gaps observed since the last heartbeat. **This is flow control, not telemetry**: see [The pump](#the-pump) for what the peer does with it.

`FILE_NOTIFY`: `u32 bytes, u32 fnv1a32, u16 name_len, name[name_len]`, with `bytes` and `fnv1a32` meaningful only when `HAS_DIGEST` is set.

`RESP` with `ERROR`: `u16 code, message[]` (UTF-8, to the end of the payload). Codes: 1 `NO_HANDLER`, 2 `BAD_FRAME`, 3 `DUPLICATE` (executed, result no longer cached), 4 `BUSY`, 5 `APP` — after which the rest of the payload is the app's own error, opaque again.

### The frame-size cap, and why it is negotiated rather than constant

**All four are measured and shipped** (`guest/go/fkipc/wire`, mirrored in `guest/rust/fkipc::wire`, asserted equal by both suites' `the_wire_constants_are_the_agreed_ones`):

| constant | value | why |
|---|--:|---|
| `MaxFrameCeiling` | **3900 B** | absolute protocol maximum. It clears the inbound wall the 2.1.14 re-run bracketed (4,000 B arrives, 8,192 silently does not), every OS's outbound cap, and the guest's 4 KiB string scratch |
| `DefaultMaxFrame` | **2048 B** | what a session negotiates unless told otherwise |
| `MaxFragments` | **16** | message ceiling 16 × (MaxFrame − 24) ≈ 62 KB at the ceiling |
| `HeaderBytes` | **24** | fixed |

The ceiling is set by the **4 KiB string scratch region**, `var fkScratch [4096]byte` (`fkScratch` in `guest/go/fkapi/fkapi.go` — a generated file, so line numbers move with every regen; grep the symbol). An inbound payload is written into that region by the host's `K_STR` path, which falls back to `fk_alloc` when it does not fit (`runtime/lua/fk_abi.lua:1100-1115`). That fallback is the hazard proposals.md promoted to a phase-1 prerequisite: **an event encode has no bracket.** `fk_alloc` hands out arena memory that the *calling binding* releases at `allocRelease` (`guest/go/fkapi/fkapi.go:450-452`), and for a host-initiated dispatch nothing on the guest side made the call, so nothing releases it. It is the exact shape `agents/abi.md` records for `write_dyn`'s container payloads, met from a third direction.

**The permanent half of that is fixed, in this worktree, by work that is not fkipc's** — `fk_arena_mark`/`fk_arena_release` bracket the outermost dispatch (`runtime/lua/fk_mod.lua:474-520`, "THE MARSHALLING ARENA'S OUTERMOST BRACKET"; `guest/go/fkapi/fkapi.go`, `fkArenaMark`; `internal/factorio/mod.go:375-382`). That changes the ceiling's argument from **correctness to cost**, and it does not retire it:

- **the exports are optional and feature-detected**, deliberately, so a guest built against an older substrate exports neither and leaks exactly as before — which means the cap is still what protects a mod nobody has rebuilt;
- an over-scratch frame is still a per-packet arena allocation and a memcpy, and the arena's *chunks* are retained as capacity once taken, so the peak frame size sets a permanent floor on guest memory — which is in the save;
- **a frame that fits the scratch touches none of it.**

Staying under 4096 also keeps a frame inside one loopback datagram with room to spare (`lo0` MTU is 16384 on this machine), so nothing is IP-fragmented under us.

**The cap is negotiated because it is a budget shared with the handler.** The scratch region is reset once per *outermost* dispatch (`runtime/lua/fk_mod.lua:474-475`, reached at `:515`), so an inbound payload holds its own length for the whole handler. Every host call the handler makes takes its string returns from above that point and releases them at its own bracket — so a 3900 B frame leaves ~196 B for any single call's string returns before *they* start falling back to the arena instead. A guest that reads entity names from inside its message handler wants a smaller frame than one that only decodes. `DefaultMaxFrame = 2048` leaves ~2 KiB of headroom; a guest that does no API calls from its handler may raise it to the ceiling. **Each side states what it will ACCEPT in `HELLO`; the sender respects the peer's number.**

**Contingency, and the reason the header does not need to change for it.** If the probe finds that a payload cannot carry arbitrary bytes — see [Binary safety](#binary-safety) — v1 respins as a byte-safe transfer encoding over the identical 24-byte header, and the only thing that moves is `MaxFrameCeiling` (3900 → ~2925 under base64). Negotiation absorbs it without an app noticing.

### Binary safety

**ANSWERED, in both directions, and it was the highest-risk unknown in this document.** All 256 byte values cross exactly, including NUL and every high byte, in the `{"", frame}` form the library sends — measured by the probe on the real transport, re-measured through four layers of marshalling by `internal/guest`'s end-to-end and parity tests, and measured a third time against a live 2.1.14 by `scripts/run-ipc.sh`'s `rpc-binary` leg. **The Rust half needed a generator fix to inherit it**: string fields were `String::from_utf8_lossy`, which rewrites what it reads. See `agents/abi.md`, "A Lua string is BYTES". Neither contingency below was needed; they are kept because the reasoning is what a future transport change would need.

`send_udp`'s `data` parameter is typed `LocalisedString`, not `string` (`api/2.0.77/runtime-api.json`, `LuaHelpers::send_udp`). The probe's own comment is the clearest statement of the problem:

> The data parameter is a LocalisedString, so a bare string is a LOCALE KEY and a payload that is not a known key may come out as "Unknown key:" text — or not, which is exactly what has never been measured. — `testdata/ipcprobe/fklua-ipcprobe/control.lua:228-232`

So there are two questions and `scripts/run-ipcprobe.sh`'s `forms` and `echo` legs answer both:

1. **Which LocalisedString shape puts bytes on the wire verbatim** — bare `s`, `{"", s}`, or `{"", head, body}`. The working assumption is **`{"", s}`**, the documented literal form, which crosses as a tier-2 `DYN_ARR` of two `DYN_STR` and costs one extra `dyn_alloc` of `2 × DYNW = 32` bytes (`runtime/lua/fk_abi.lua:700-708`).
2. **Whether bytes `0x00` and `0x80`–`0xFF` survive that shape** — the `hex` and `len` legs measure it in both directions independently, so a mangled byte can be attributed to one side rather than to "the wire".

If (2) comes back negative, the contingency ladder is: base64url in the guest (pure arithmetic, no host call, ~3900 → ~2925 payload bytes), then `helpers.encode_string`/`decode_string` (members 2148/—, both bound; base64 over deflate, so it *compresses*, at the price of one host call per frame in each direction and an external implementation of Factorio's exact codec). Neither changes the header.

---

## Sessions and the epoch

### The guest cannot mint a unique session id, and this is not a limitation to work around

It is a theorem. Everything a guest can compute is a deterministic function of its own state, and its own state time-travels: load a save twice and the guest computes the same value both times, by construction. A counter in guest memory rewinds with the save. A hash of the map seed and the tick is identical across two loads of one save. The WASI `random_get` shim is a seeded PRNG in `storage` precisely so that it *is* deterministic. **There is no deterministic function of guest state that distinguishes two loads of the same save**, and any function that did would be a desync.

So the uniqueness comes from the side that has entropy.

### The handshake

```
guest -> peer   HELLO      epoch = boot      corr = C
peer  -> guest  HELLO_ACK  epoch = TOKEN     corr = C
guest             adopts TOKEN as its epoch for the rest of the session
```

`boot` is the guest's own load counter — best-effort, monotone within a timeline, and only ever used to fill the field before a token exists. `TOKEN` is a u32 the peer draws from real entropy. From `HELLO_ACK` onward every frame in both directions carries `TOKEN`, and **a frame whose epoch is not the current one is dropped and counted.**

**The one exception, stated because two implementations will otherwise disagree about it:** `HELLO_ACK` carries an epoch the guest does not yet know, by definition. It is matched on `corr` against the outstanding `HELLO` instead, and it is the only frame type for which the epoch test is skipped.

**DECIDED — adopting a peer-chosen value into guest state is legal, and the reason is the cost model.** The token arrives in a datagram, which arrives via `recv_udp`, which enters game state as an InputAction, which the engine replicates to every peer at the same tick. Every peer's guest adopts the same token at the same tick. This is the expensive direction paying for itself: the replication that makes inbound costly is exactly what makes it safe to branch on.

### `boot` is the SESSION GENERATION, and a load does not move it

`boot` lives in guest memory, which is persisted (M6). It used to be a **load** counter incremented in `fk_after_load`, which turned out to be the one place it could not be incremented — see [the rule above](#the-rule-the-cost-model-implies-which-this-design-got-wrong-once). It is now incremented at a session BOUNDARY, in `resetSession`, whose every caller is a replicated signal.

Nothing about the theorem changes. `boot` still lives in guest memory, so it still time-travels with the save and still aliases across two loads of one save, and **the peer must still never compare it** — if anything the statement is sharper, because two loads of one save now produce *literally the number the save carries* rather than that number plus a bump. What it is good for is a human reading a log and asking whether a flapping session is this guest re-sessioning or the companion restarting.

`Open`'s split is unchanged and is worth restating because it is what makes a no-op `Reload` sound. `_initialize` runs from control.lua's main chunk, and under any persistence mode the memory it builds is *immediately replaced by the saved one* — so anything `Open` writes to guest state on a load is discarded anyway. What survives from `Open` is the part that is not guest state at all: the `fk.subscribe` registration, which is host-side and must happen on every load because Factorio does not save event registrations. That split is the same one `agents/abi.md` records for the callback seam's decision 1. **What changed is that `fk_after_load` is no longer a required export**; `Reload` exists and does nothing, so the wiring line every guest carries goes on compiling.

### What each side does on a session boundary

**The guest, on a load: NOTHING.** `Reload()` is a no-op with respect to guest memory, deliberately and by measurement. A joining client adopts the downloaded mid-session state verbatim and behaves identically to every other peer, because its inbound — the replicated InputAction stream — is identical. Three things replace what the load-reset used to provide, and every one of them is driven by a replicated signal:

- **The companion restarted too.** It does not recognise the epoch the restored guest is still using, so it drops those frames and answers `BYE`. That is inbound, so every peer resets identically and the guest re-`HELLO`s. This path already existed.
- **The companion kept running across the guest's rollback.** The epoch still matches and the guest's per-channel seq counters have gone *backwards*, so every telemetry frame reads as `d <= 0` at the peer and is dropped as stale — forever, with heartbeats still flowing and both sides believing the session is healthy. That is the wedge, and the peer is the side that can see it: it watches the tick every `HEARTBEAT` carries and tears the session down on a regression past `RollbackTicks`. See [Rollback detection](#rollback-detection-the-clock-owning-side-owns-the-clock-anomaly).
- **Nobody is there at all.** Liveness fires within `LivenessTicks` and the guest quiesces and searches, exactly as when a peer dies mid-session.

**The guest also has half of the rollback detector**, and it is legal for the reason nothing peer-local is: it is a function of guest state and the replicated tick, so every peer decides it identically on the same tick. `serviceSession` declares the session down when `int32(tick - lastRx) < 0` — the clock has gone backwards past the last frame this link accepted, so the session in memory belongs to a future that no longer happened. It catches every rollback larger than the time since the last inbound frame, which with a peer heartbeating once a second is most of them. What it *cannot* catch is a save taken just after an inbound frame and restored much later: `tick` and `lastRx` move together, the difference stays small, and nothing the guest can compute is wrong. That one is the peer's, because only the peer has a clock that did not travel with the save.

**The peer, on receiving a `HELLO`**: unconditionally treat it as a new session — mint a new token, discard everything about the old one, and tell the application through `OnSession`. It does not compare `boot`; `boot` aliases across two loads of one save and a peer that trusted it would carry state across a boundary the guest has already forgotten. **The `HELLO` is the session boundary. The epoch is the frame filter.** Two jobs, two mechanisms, and conflating them is how this gets subtly wrong.

**And the SOURCE PORT is the MOD filter — the third mechanism, and it exists because several mods share one game socket.** Every inbound datagram raises `on_udp_packet_received` in EVERY subscribed mod, so each link drops a frame whose `source_port` is neither 0 nor its `Config.Port`, counted in `ForeignDrops`, before any other accounting (`src == 0` is accepted because zero is not a valid UDP source port — it means "the engine did not say", and refusing on silence would make a guest deaf on a build that stops filling the field). Without it, the `HELLO_ACK` epoch-test exemption has a corr collision: two freshly-loaded mods both send `HELLO` with `corr = 1`, both companions answer, and each mod adopts whichever ACK lands first — measured with the filter removed as a PERMANENT LIVELOCK, 18 sessions minted per mod in ~50 s and zero telemetry frames delivered. The SDK side needs no code: each `Session` binds its own socket, so the DESTINATION port routes and the OS does the filtering — proven by `TestTwoSessionsDoNotSeeEachOthersTraffic` over real sockets rather than assumed. *Enforced by `TestAFrameFromAnotherModsCompanionIsRefused` and its Rust mirror, each with a positive control (a `BYE` from the CONFIGURED port must still tear the session down), both confirmed to fail with the filter stubbed.*

**And the NAME is the SCHEMA filter — the fourth mechanism, and the only one that can refuse a peer whose transport is entirely correct.** The three above answer transport questions. None of them can answer *is the process I was pointed at the one I was BUILT against*, and that gap is not hypothetical: swap two mods' companion ports, or leave last week's companion running, and the handshake succeeds at every layer while the two ends disagree about what channel 1 means. The symptom is a slider that drives nothing, an RPC answered with `NO_HANDLER`, or — worse — a channel whose payloads happen to parse.

So `HELLO` and `HELLO_ACK` carry an identity token in the `name` field they have always had (**zero wire-format change**), and each side may state what it requires of the other:

| | states its own | requires the peer's |
|---|---|---|
| guest | `Config.Name` / `config.name` | `Config.ExpectPeer` / `config.expect_peer` |
| SDK | `Options.Name` | `Options.ExpectedName` |

**Empty means no check**, in all four places, so every guest and every companion written before this existed behaves exactly as it did. **One token names the CONTRACT rather than either party**, so the usual configuration is one string in all four slots — and a side that sets only the expectation sends it as its own name too, because there is no useful configuration in which a peer checks its partner's identity and withholds its own.

**The convention is `"<mod-name>/<schema-tag>"`, and the tag is the author's claim about CHANNEL-CONTRACT compatibility** — a design-time UUID or a version string, bumped when the meaning of a channel changes. **Deliberately NOT FkLua's per-build id**: that moves on every rebuild, so it would break the pairing every time either side is recompiled and turn a correctness check into a nuisance. The question the token answers is "do we agree about the channels", not "were we compiled on the same afternoon".

What each side does:

- **The peer, on a `HELLO` whose name is wrong**: refuse it — *before* everything else, which is the whole ordering decision. "A `HELLO` is always a new session" is the rule for a guest this companion is FOR; below the check, one stray datagram from a swapped port config would tear down a live conversation with the mod it IS for. Nothing about the current session moves, no token is minted, `Stats.NameRejects` counts it, `Stats.RejectedName` carries what was offered, and `OnSession` is told **`SessionRejected`** — a distinct event, because "the wrong mod is on this port" and "nothing is there" need different words from a GUI that would otherwise show a spinner forever. A `BYE` goes out under the **same rate limiter** the unknown-epoch and rollback `BYE`s use: an fkipc guest drops it (it carries a `boot` value, not an epoch the guest adopted), so it is purely diagnostic — a refusal that puts *nothing* on the wire is indistinguishable, in a capture, from a companion nobody started — and a mismatched guest re-`HELLO`s once a second forever, so answering each one is the amplification the shared limiter exists to prevent.
- **The guest, on a `HELLO_ACK` whose name is wrong**: do not adopt the token, count it in `LinkStats.NameRejects` / `Stats::name_rejects`, stay peerless and go on searching at the ordinary `SearchTicks` cadence.

**The retry continuation is the subtle half, and it is two rules.** A rejected ACK **consumes nothing**: `helloCorr` is left set, so a correct ACK on the *same* outstanding `HELLO` — the companion restarted with the right identity while that `HELLO` was in flight, or two companions answered and the right one was second — is still adopted, where zeroing it would leave the guest deaf until the next search. And `lastHello`/`helloDue` are left alone, so the reject does **not** accelerate the search: a mismatched companion answers every `HELLO`, so "reject, then re-`HELLO` at once" is a frame per tick in both directions for as long as the misconfiguration lasts — the livelock shape the source-port filter was built to end, met from a new direction. A rejected ACK therefore costs exactly one counted drop, and a fresh `HELLO`'s corr is adopted on the ordinary schedule.

**THE DETERMINISM ANALYSIS, because a guest-side check is a guest-side write.** It is legal for exactly the reason adopting the token is legal: the `HELLO_ACK` arrives through `recv_udp`, which enters game state as an InputAction, which the engine replicates to every peer at the same tick. Every peer sees the same ACK at the same tick, so *refusing* it, *counting* the refusal and *continuing to search* are identical operations on every peer — exactly as adopting it would have been. The thing it is compared against is a **build-time constant**, which is identical on every peer by construction; a token computed at run time would have to be a deterministic function of guest state (the same theorem that stops a guest minting its own epoch), and one that was not deterministic would be a desync. **What a check here may NEVER do is branch on anything peer-local** — that is the rule the load-reset design broke and the send-status counters broke after it. The SDK side has a real clock and no such constraint. *Enforced by `TestAJoiningPeerStaysByteIdenticalToTheServer`, which stays green precisely because the counters are driven by replicated inbound.*

**IT IS A CORRECTNESS CHECK AND NOT AN AUTH BOUNDARY, and the distinction is not a caveat to be softened.** The token is a constant in a mod zip anybody can read — `fklua mod` ships the guest's own bytes and the string is in them — the transport is unauthenticated localhost UDP, and any process on the machine can send a `HELLO_ACK` carrying whatever name it likes. It stops *misconfiguration*, not an *adversary*. **Bearer-secret authentication is explicitly out of scope**, and one reason is specific to this runtime rather than general: a secret a guest held would live in `storage.fk_mem`, which is written into every save and shipped to every joining multiplayer client — so "the secret" would be readable by everyone in the game and would survive in every replay of it. A real answer belongs on the transport (a unix socket with filesystem permissions, or a proper handshake outside the sandbox), not in this field.

*Enforced by `TestAHelloAckFromTheWrongApplicationIsNotAdopted` and `TestARejectedHelloAckDoesNotChangeTheSearchCadence` in both languages, each with a no-expectation control and an explicit recovery arm; `TestSchemaMismatchIsRefusedAtBothEndsAndAMatchedPairIsNot`, which drives both real state machines through all three arms; `TestARefusedHelloLeavesALiveSessionAlone` for the ordering; and `TestBothGuestLibrariesSpeakTheSameWire`, whose script now refuses an ACK and then accepts one on the same corr through the verbatim runtime in both languages, byte for byte.*

**The peer, on a frame with an unknown epoch**: drop, count, and — at most once per `LivenessTicks` worth of real time — reply `BYE`. That accelerates a guest that is retransmitting into a session nobody remembers, which is the common shape after a client restart.

### Rollback detection: the clock-owning side owns the clock anomaly

**`sdk/go/fkipc` tracks the highest guest tick it has seen this session and tears the session down when a `HEARTBEAT` reports one more than `RollbackTicks` below it.** The teardown is a `BYE` at the live epoch — sent *before* the local state is cleared, because a `BYE` at epoch 0 is a frame the guest drops — followed by the ordinary down path, so pending requests fail `ErrSessionLost` and the application is told. The guest hears the `BYE` through `recv_udp`, which is an InputAction, so **every peer resets at the same tick** and re-`HELLO`s. A guest that decided this for itself out of local knowledge would be doing exactly the thing this whole section forbids.

Three details are load-bearing:

- **A high-water mark, not the last reading.** Measuring against the previous heartbeat would make one stale datagram look like a rollback and then make the real rollback look like a recovery.
- **RFC-1982 serial arithmetic**, the same `SerialDelta` the per-channel seq comparison uses, so the u32 wrap — about 2.27 years of game time — is a forward step of `+1` rather than a regression of 2³² and needs no special case.
- **`DefaultRollbackTicks = 60`, and the number is the SELF-HEAL BUDGET rather than a fudge factor.** A rollback of R ticks rewinds the guest's seq by whatever it sends in R ticks, and the channel un-wedges by itself once the counter climbs back past where it was — R ticks later, whatever the frame rate. Below the tolerance, waiting is cheaper than a re-handshake, which costs a `HELLO` round trip and fails everything in flight. Above it — an autosave restored twenty minutes on — the channel is deaf for the whole rollback and no amount of waiting fixes it. It is *not* paying for reordering: the transport is localhost datagrams, which do not reorder, and if it ever had to it would have to clear the heartbeat interval, which is also 60.

**This is why the guest's HEARTBEAT became unconditional.** The tick crosses in no other frame, and the old rule — send one only if nothing else went out in the window — meant a guest streaming telemetry every tick heartbeated *never*, so the peer's reading of the guest clock froze at the `HELLO` for the whole session, and so did the `rx`/`drops`/`gaps` counters this file already described as arriving "one frame per second, for free". The cost is one 40-byte datagram per second of game time in the direction that is free.

### In-flight requests across a session boundary

Failed locally with **`ErrSessionLost`**, never retried into the new session. The distinction the API must make loudly: this is **not** "the request failed", it is "**the outcome is unknown**". The save may predate a response the peer already sent, or predate the peer *executing* the request and not yet replying. Automatically retrying it into a new session would re-execute it outside the dedup window, which is precisely the guarantee `corr`-based dedup exists to provide. The application re-derives or re-asks; that is what "idempotent RPC" buys and it is the reason this protocol asks for idempotence at all.

### ~~The first-tick ordering hazard~~ — GONE, because nothing happens on a load

The hazard was: `fk_on_tick`'s dispatcher is registered while control.lua runs, the `fk_after_load` one-shot is armed later from `script.on_load`, and the per-event dispatcher list is walked in registration order — so on the first tick after a load `Pump` ran once with the restored session state *before* `Reload()` cleared it. That put **two `HELLO`s on the wire one tick apart**, and since a `HELLO` is unconditionally a new session at the peer, a companion listening for both minted two and failed anything in flight with `ErrSessionLost` (P6, seen for real by `scripts/run-ipc.sh` as `sessions=2` on one run and `sessions=1` on the next).

**There is no window now, because there is nothing on either side of it.** `Reload()` does not clear the session, so the first pump after a load carries on under the epoch the save holds and sends no `HELLO` at all. The ordering of the two dispatchers stopped mattering, which is a better outcome than the structural fix P6 was waiting for — a `fk_mod.lua` reordering with a golden-file and `Hooks`-mirror cost. `sdk/go/cmd/ipcgate` asserts `sessions == 1` as a verdict now, where the count used to sit in the STATS line precisely because it was a race.

---

## Reliability, per flow class

**All timers are in GAME TICKS.** There is no wall clock in the sandbox and there is not going to be one; a tick is 16.67 ms of nominal game time and a variable amount of real time, which is exactly right for a peer whose pauses are the game's pauses. The external side keeps its own timers in real time, and the two are reconciled by the tick each `HEARTBEAT` carries.

The shipped values, which are these with two changes the probe forced and one the implementation added (`guest/go/fkipc/link.go`, mirrored constant for constant in `guest/rust/fkipc/src/link.rs`):

| constant | value | why |
|---|--:|---|
| `RetryTicksServer` / `Client` | **15** / **6** | MEASURED. Round trip on a headless server through the InputAction path is median 31.5 ms (~1.89 ticks), min 8.4, p90 94.8 (~5.7 ticks) — so a server-profile retry under about ten ticks retransmits frames that were merely in flight, and the client value sits at that p90 because a single-player client has no replication fan-out |
| `RetryBackoffCap` | ×2, capped at 60 | |
| `MaxRetries` | **4** | 15 + 30 + 60 + 60 = 165 ticks, ~2.8 s at the server default |
| `DedupTicks` | **600** | > the sum of the retry schedule, with margin |
| `MaxDedup` | **256** entries | the dedup table is guest memory, i.e. it is in the save |
| `MaxDedupPayload` | **512 B** | ditto — a cached response is save weight |
| `HeartbeatTicks` | **60** | one per second of game time, **unconditionally**. It was "only if nothing else went out in the window" until the join fix, which meant a telemetry-heavy guest never sent one — and the heartbeat is the only frame carrying the guest's TICK, which is what the peer's rollback detector reads. Outbound is free |
| `LivenessTicks` | **180** | three missed heartbeats — and, since the join fix, a clock that has gone BACKWARDS at all |
| `RollbackTicks` (SDK) | **60** | how far the guest's clock may regress before the peer tears the session down. The self-heal budget, not a tolerance for reordering — see [Rollback detection](#rollback-detection-the-clock-owning-side-owns-the-clock-anomaly) |
| `ReassemblyTicks` | **120** | blueprint-share's number, and it has held up |
| `SearchTicks` | **60** | how often a peerless guest sends `HELLO` |
| `SendBudget` | **8** frames/tick | bounds the WORST TICK, not the bandwidth. The engine neither coalesces nor rate-limits: ten `send_udp` calls in one tick produced ten datagrams, in order, on loopback |
| `DrainMax` | **1** `recv_udp` call/tick | CHANGED BY THE PROBE, from 8. One call drained a 20-packet backlog blasted in 0.34 ms — all twenty within the tick, in order, complete — so one per tick is the shape and this is the knob if a build ever delivers one packet per call |
| `MaxQueue` | **64** frames per priority class | the send queue is guest memory, so it is in the save |
| `MaxPending` | **16** | NOT IN THE DESIGN. A retried request keeps its whole message so it can be resent, and at the 62 KB message ceiling an unbounded pending table is an unbounded save |

### RPC — `REQ` / `RESP`

Retry the **same** `corr` on the schedule above. The responder keys its dedup table on `(epoch, channel, corr)`; a hit replays the cached `RESP` rather than re-invoking the handler. `RETRY` is set on every retransmission so the responder can count "how often does my reply get lost" separately from "how often does my request get lost".

A response larger than `MaxDedupPayload` is **not** cached; its `corr` is remembered anyway, and a retry gets `RESP | ERROR` with code 3 `DUPLICATE`. The application learns that the operation executed and the result is gone, which is strictly better than the two alternatives (silently re-executing, or growing the save without bound). A handler with a large result should write a file and answer with a `FILE_NOTIFY`, which is the right shape for a large result regardless.

`corr` is minted from a **counter**, never from randomness — determinism, and it also makes the dedup window's arithmetic trivial.

### Telemetry — `MSG`

Each channel has its own `seq`, incremented per **frame** (so a lost fragment is a detectable gap, not a silently short message).

Serial comparison, spelled out because this is the one place two implementations silently disagree. With `d = int32(seq - last)`:

| `d` | | |
|---|---|---|
| `d > 1` | **gap** | deliver the frame, raise the gap, `last = seq` |
| `d == 1` | in order | deliver, `last = seq` |
| `d <= 0` | old | **drop** |

That is RFC-1982-style serial arithmetic, so u32 wraparound is a non-event — at one frame per tick a channel wraps after roughly two thousand years, and the comparison would be correct anyway.

**Dropping `d <= 0` is a deliberate semantic choice with a consequence worth stating loudly: a channel carries STATE, not a LOG.** An out-of-order or duplicated frame describes an older world than one already delivered, and this protocol's standing position is that stale game state is worse than useless. An application that needs an append-only record must number its own entries inside the payload; the transport will not do it, and asking it to would mean buffering and reordering — which means a reorder buffer in guest memory, which means it is in the save.

**Two exemptions the soak test earned, both AS BUILT and both binding on any future implementation.** A `SNAPSHOT` frame is exempt from the `d <= 0` drop and RESETS `last` rather than advancing it — any receiver whose `last` ever jumps ahead of its sender is otherwise deaf on that channel forever: every frame reads as old, so no gap is raised, so no RESYNC is sent, and nothing says anything. And **channel 0 carries no seq and is exempt from gap detection** — a lost heartbeat is normal and must not read as a gap in application state. Related guidance: **do not mix RPC and telemetry on one channel** — seq is per channel across frame classes, so a lost REQ on a mixed channel raises a spurious resync.

**A gap is answered with a snapshot, never a replay.** The receiver sends `RESYNC` naming the channel; the producer answers with a `MSG` carrying `SNAPSHOT`, which resets the receiver's `last` and clears the gap. There is no retransmit queue anywhere in this design, and the reason is not economy: **the producer usually cannot replay, because the state it described no longer exists.** A resend of "entity 4471 at 30% health" three seconds later is a lie, and a lie that arrives is worse than a gap that is noticed.

`RESYNC` names its target in the header's `channel` field and carries no payload.

### Fragmentation

A message longer than the negotiated frame is split across up to `MaxFragments` frames sharing one `corr`, with `frag`/`nfrag` giving position. Reassembly is keyed by `(channel, corr)` and **at most one reassembly is open per channel** — which bounds the buffer, and imposes the rule that a peer must not interleave two fragmented messages on one channel. A reassembly is abandoned on `ReassemblyTicks`, on an `nfrag` that disagrees with the one in progress, on a new `corr`, or on any epoch change.

**A fragmented `MSG` that loses a fragment is lost entirely** and shows up as a gap, which resyncs. That is correct and it is also the guidance: **a sender that needs a large message *delivered* uses `REQ`/`RESP`, not `MSG`.** The whole message is retried on the ordinary RPC schedule; at 6 frames on a loopback link that is a cheap and rare event.

Outbound, a guest should prefer `WriteBulk` above one frame's worth. That is advice rather than a rule, and it is good advice because localhost-only transport means the peer is always on the same filesystem — the file path is *always* available outbound, and it is one datagram instead of sixteen.

### File + notify

`write_file` into `script-output`, then a `FILE_NOTIFY` frame. This is the only path for screenshots (`take_screenshot` writes to `script-output` and raises no completion event) and the sane path for anything above ~10 KB outbound.

**Nothing documents a flush guarantee**, so the peer must not treat the notify as "the bytes are all there". Two cases:

- **Guest-written** (`WriteBulk`): the guest knows the length and computes an FNV-1a-32 over its own bytes, so `HAS_DIGEST` is set and the peer's test is exact — read until `bytes` and the checksum matches, or keep waiting.
- **Engine-written** (a screenshot): the guest has never held the bytes and cannot describe them. `HAS_DIGEST` is clear and the peer falls back to stabilize-polling — size unchanged across two polls.

The notify is a `MSG`-class frame: seq'd, gap-detectable, **not retried**. The file is durable, so a lost notify is recoverable by a `RESYNC` on that channel (the guest re-notifies what it has written since the last snapshot) or by the peer scanning the directory. Retrying a notify would be retrying a claim about a file that may since have been overwritten.

### Liveness, and "the peer is gone" means the mod keeps playing

**The guest sends `HEARTBEAT` every `HeartbeatTicks`, unconditionally**; the peer sends one every `Heartbeat` **only if it has sent nothing else in that window**, because any frame is a liveness signal. The asymmetry is deliberate and is the rollback detector's: the guest's heartbeat is the only frame carrying the guest's tick, and a guest that suppressed it while busy would leave the peer with a clock reading frozen at the `HELLO`. A side that has heard nothing for `LivenessTicks` declares the peer down — and the guest additionally declares it down when its own clock has gone *backwards* past the last frame it accepted, which is a save restored under a live session.

A guest whose peer is down **quiesces**: it stops sending everything except a `HELLO` every `SearchTicks`, fails pending requests with `ErrSessionLost`, drops its send queue, and raises `SessionDown` to the application. It does not retry harder, it does not buffer against the peer's return, and it never blocks. The last is a property rather than an aspiration — a guest built with `-scheduler=none` has nothing to block *on* (`guest/go/fk/fk.go:44-46`: "a Factorio tick cannot block") — but the application-visible half is real discipline: **the mod's own behaviour must be defined with no peer**, and the library makes that the easy path by turning `Send` into a counted no-op rather than an error to be handled at every call site.

Every `send_udp` and `recv_udp` call is wrapped in the guest's error path anyway. This API's history is crash-on-error — `send_udp` to an occupied port crashed the game before 2.0.61 — and while the pinned build is past that, a status is cheaper than a trust assumption. Note the limit honestly: **a C++ crash is not catchable from Lua**, so what this protects against is a Lua-level raise, and what protects against the other thing is the probe.

---

## The pump

`recv_udp` must be called or nothing arrives, and the OS buffer is 256 KB with **silent** loss between polls — including while the game is paused or saving. Poll cadence is therefore a correctness parameter, not a tuning one.

### The three options, and why the answer is the boring one

| | cost when idle | cost when live |
|---|---|---|
| **(a) permanent `fk_on_tick`** | 1 dispatch/tick | 1 dispatch + 1 host call/tick |
| **(b) self-re-arming `fk.defer()`** | 1 dispatch + `off_event` + `on_event` + 2 `storage` writes/tick | the same, plus the host call |
| **(c) hybrid: armed only while a session is live** | **zero** — and permanently deaf |

**(c) is the repo's standing bias and it does not survive contact with this problem.** A guest with no peer must still be able to *notice* a peer appearing, and the only way to notice is to call `recv_udp`. A guest that registers nothing is a guest that can never be reached again. So the quiescent state cannot be zero; it can only be slow.

And a slow poll costs the same registration. `fk.defer` is a **next-tick** one-shot with no way to skip ticks (`runtime/lua/fk_mod.lua:894-899`), so "poll every 60 ticks" built on it is "re-arm every tick and act on every 60th" — which `agents/guests.md` already scores exactly: *"a guest that re-arms unconditionally has an on_tick subscription for the life of the mod, which is exactly the permanent cost `fk.Defer()` exists to avoid"*, plus the arm/teardown churn (b) pays over (a).

**DECIDED — the pump is a permanent `fk_on_tick`, and what varies with session state is the work inside it, not the registration.**

The idle-cost claim is therefore not "an idle guest registers nothing". It is:

> An IPC guest with no peer pays one dispatch and one integer compare per tick, and one `recv_udp` every `SearchTicks`. An IPC guest with a live peer pays one dispatch and one `recv_udp` per tick.

At the cross-confirmed ~12.5 µs per host call (`agents/abi.md`, "What a call costs"), a live pump is **12.5 µs of a 16,667 µs frame — 0.075%**, or 0.75 ms per second of game time. **That is still a derived number and is still not an fkipc measurement**: `recv_udp` may cost more than a bare call because it also dispatches events, and nothing has separated the two. What the live gate does say is that a headless 2.1.14 running this pump every tick holds a full session with no measurable trouble; it does not say what the pump costs.

### Draining

**ANSWERED: one call drains the whole backlog, so `DrainMax` is 1.** Twenty packets blasted in 0.34 ms all arrived in one tick, in order, complete — the engine raises them as a batch of `on_udp_packet_received` events from inside the `recv_udp` call. `Pump` therefore calls it once per tick and the burst arrives as N nested dispatches whose cost is bounded by the frame budget. The constant survives as the knob for a build that ever delivers one packet per call.

**Pause and resume.** Ticks stop, the pump stops, the buffer fills, and the loss is silent. Two halves of the mitigation:

1. **The guest drains on resume** — the first ticks after a pause take up to `DrainMax` polls each until `recv_udp` reports nothing new, then fall back to one per tick.
2. **The guest's silence is the flow-control signal, and this is the important half.** The peer has a real clock; the guest's heartbeats stop when the game does. A peer that has heard nothing for `PeerQuietMillis` (working assumption 3000, TBD-PROBE against the probe's save-time observations) **stops sending everything but `HEARTBEAT`**. That, and not a bigger buffer, is what keeps a long pause or a slow save from costing 256 KB of dropped frames. The heartbeat payload's `rx`/`drops`/`gaps` counters give the peer a real rate to aim at, one frame per second, for free.

### Wiring, and why it is three lines rather than one

A wasm module has **one export per name**, so `fkipc` cannot own `fk_on_tick` or `fk_on_event` — the guest author's program owns those, and routes into the library:

```go
func init() { fkipc.Open(fkipc.Config{Port: 29434}) }   // subscribes; sends NOTHING

//go:wasmexport fk_on_tick
func onTick(tick uint32) { fkipc.Pump(tick) }

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
    if fkipc.OnEvent(id, ptr) { return }
    // ... your own events
}
```

**It was four lines until the join fix**, the fourth being `//go:wasmexport fk_after_load func afterLoad() { fkipc.Reload() }`. `Reload` is a no-op now, so the export is optional: keeping it costs nothing and breaks nothing, and `guest/{go,rust}/examples/ipc` keep it deliberately, because that is the shape every guest written against the old wiring still has and the join-parity test drives those examples. A guest with no other use for `fk_after_load` may drop it.

Three unavoidable lines, and each is unavoidable for a different reason:

- **`Open` from `init()`, not from `fk_on_init`.** Package initialisers run inside `_initialize`, which runs on **every load** (`runtime/lua/fk_mod.lua:953-961`), and event registrations are not saved. `fk_on_init` fires on a new map only. This is decision 1 of the callback seam applied unchanged.
- **`Open` sends nothing.** `_initialize` is control.lua's **main chunk**, and 2.1's documentation says a non-zero `for_player` on `send_udp`/`recv_udp`/ `write_file` is *silently skipped* there. The first frame goes out from the first `Pump`, which is inside an event dispatch. (Whether `for_player = 0` specifically is affected is TBD-PROBE, and the rule costs nothing either way.)
- **`OnEvent` returning `bool`** so the event-id constant stays inside `fkipc`. That matters: `fklua mod` prunes the event table by scanning for an `i32.const` reaching `fk.subscribe` (`internal/factorio/used.go:38-53`), and an id it cannot prove constant ships all 219 descriptors (the default 2.0.77 pin's count; 224 at 2.1.14 — it is a per-pin number). The constant must appear at the `fkapi.Subscribe` call site and the wrapper must inline — the property `TestTheEventIdSurvivesTheGeneratedSubscribeWrapper` exists to hold, and the Rust half of which was a real defect (R6: `subscribe_filtered` lacked `#[inline]` and shipped 85 KB of Lua per load). **`fkipc` owes the same assertion for its own call site.**

---

## The guest API — `guest/go/fkipc`

**DECIDED — a hand-written package beside `fkapi`, modelled on `fkgc`.** Not generated, not part of the bindings, no census row: it calls the generated bindings and never names a member or event id itself. `guest/go/fkgc` is the shape — its own directory in the guest module, its own doc.go that explains the *why*, and a build-tag split for the parts that cannot exist off-target (`guest/go/fkgc/off.go:1`).

```go
// Package fkipc is a message-oriented IPC link between a FkLua guest and a
// companion process on the same machine.
package fkipc

type Profile uint8

const (
    ProfileServer Profile = 0 // headless, for_player = 0 (default)
    ProfileClient Profile = 1 // graphical single player
)

type Config struct {
    Port      uint16  // the PEER's port. Must differ from --enable-lua-udp's.
    Profile   Profile
    ForPlayer int32   // 0 = the server. N = player N. -1 = every peer sends its own.
    MaxFrame  uint16  // what we will ACCEPT; 0 = DefaultMaxFrame
    Name      string  // this guest's IDENTITY TOKEN, carried in HELLO

    // ExpectPeer is what the COMPANION must call itself; "" = no check. A
    // HELLO_ACK whose name differs is not adopted, is counted in
    // LinkStats.NameRejects, and the link goes on searching. See the schema
    // filter above -- it must be a BUILD-TIME CONSTANT, and it is a correctness
    // check rather than an auth boundary.
    ExpectPeer string
}

// Open registers the subscription and the session state. Call it from init().
// It sends nothing -- see the wiring note above.
func Open(cfg Config) Status

// The two routes the guest author wires. See the three-line block above.
func Pump(tick uint32)
func OnEvent(id, ptr uint32) bool

// Reload is the optional fk_after_load route and DOES NOTHING. Kept so the old
// four-line wiring compiles; a load is not a session boundary.
func Reload()

// --- channels -------------------------------------------------------------

type Channel struct{ id uint16 }

// Chan names a channel. Priority is a property of the channel and NOT a wire
// field: the receiver never needs it, and a field the receiver ignores is a
// field that eventually disagrees with the sender's behaviour.
func Chan(id uint16, pri Priority) Channel

type Priority uint8
const (PriControl Priority = 0; PriBulk Priority = 1)

// Send queues one MSG. The payload is COPIED into the library's frame buffer
// before returning, so the caller may reuse its slice.
func (c Channel) Send(payload []byte) Status

// Snapshot is Send with the SNAPSHOT flag -- the answer to a RESYNC.
func (c Channel) Snapshot(payload []byte) Status

// Request queues a REQ and registers the completion. There are no goroutines
// and no channels on this target; every asynchronous result in FkLua arrives
// as a callback from a dispatch, and this is no different.
func (c Channel) Request(payload []byte, onReply func(Reply)) (Corr, Status)

type Corr uint32

type Reply struct {
    Corr    Corr
    Payload []byte // VALID ONLY INSIDE THE CALLBACK -- a view into the frame buffer
    Err     error  // nil, ErrTimeout, ErrSessionLost, or a peer error record
}

// --- inbound --------------------------------------------------------------

// The payload handed to any of these is a view into the library's own buffer
// and is invalid the moment the handler returns. Copy what you keep. This is
// the same rule as a transient handle and the string scratch region, for the
// same reason.
func (c Channel) OnMessage(h func(m Message))
func (c Channel) OnRequest(h func(r Request) []byte) // return = the RESP payload
func (c Channel) OnResync(h func())                  // "send me a snapshot"
func (c Channel) OnGap(h func(missed uint32))

func OnSession(h func(ev SessionEvent))

type SessionEvent uint8
// SessionReset is never raised any more -- it meant "Reload ran" -- and is kept
// so the numbering does not move and a downstream switch still compiles.
const (SessionUp SessionEvent = iota; SessionDown; SessionReset)

// --- bulk -----------------------------------------------------------------

// WriteBulk writes data to script-output/<name> and sends a FILE_NOTIFY on c,
// with a length and an FNV-1a-32 the peer can verify. Prefer it to a fragmented
// message for anything above one frame: it is one datagram instead of sixteen,
// and the transport is localhost-only, so the peer is always on this filesystem.
func WriteBulk(c Channel, name string, data []byte) Status

// NotifyFile announces a file this guest did NOT write -- a screenshot. No
// digest, so the peer must stabilize-poll.
func NotifyFile(c Channel, name string) Status

// --- observability --------------------------------------------------------

func Stats() LinkStats  // the TYPE is LinkStats -- a Go package cannot hold a
                        // func and a type of one name (fkgc's Stats()/MemStats
                        // precedent); Rust has no collision and keeps Stats
type Stats struct {
    Epoch                        uint32
    Up                           bool
    TxFrames, TxBytes            uint32
    RxFrames, RxBytes            uint32
    Drops                        uint32 // bad magic/version/type/length/epoch
    Gaps                         uint32
    Retries, Timeouts, DupHits   uint32
    QueueDepth, QueueDrops       uint32
    ScratchOverflows             uint32 // frames that fell back to fk_alloc
}
```

`ScratchOverflows` earns its place: it is the counter that makes the arena hazard visible instead of silent, and a non-zero value on a live session means the negotiated frame size is wrong for what the handler does.

### As built (wave 2a) — the deviations, each with its reason

The Go implementation exists (`guest/go/fkipc`, `guest/go/fkipc/wire`, `sdk/go`, conformance suite, e2e under lua52f, pruning assertion). Thirteen deviations from the draft above, recorded here so the Rust mirror inherits them rather than rediscovering them; the two hard corrections are already folded in above.

- **The version gate reads `helpers.game_version`**, not `script.active_mods`: one call, one short string, no container materialised into the guest heap, and available from the main chunk where `Open` runs. The stale-gate-after-a-load window cannot be wrongly permissive: Factorio refuses a save from a newer build, so a restored "receiving is safe" can only come from an engine ≤ this one.
- `MinEngineVersion = {2,1,14}` — the measured floor, and below it the library is INERT rather than send-only. It was `BaseFloorRecv` while it gated only the receive path; the rename is the whole of what the widening changed about its meaning, and `scripts/lib-engine.sh` reads the declaration.
- `MaxPending = 16`: a retried request keeps its whole message, and at the 62 KB ceiling an unbounded pending table is an unbounded save.
- `ProfileClient` with `ForPlayer` unset OMITS the argument (the probe measured `for_player = 0` as a silent no-op where no server exists).
- `LinkStats` splits `Drops` by cause (`BadFrames`/`EpochDrops`/`StaleDrops`) and adds `Refusals`/`BaseVersion`/`Enabled`/`Boot`. (`RecvRefused` and `RecvEnabled` were the send-only mode's names and went with it.)
- The package singleton is a non-nil configured-by-`Open` `&Link{}`, because package-level `var ch = fkipc.Chan(…)` runs before `init()` and a dead handle there would silently discard handlers. Pinned by a test.
- An inbound `FILE_NOTIFY` to the GUEST is dropped and counted (no file reads).
- SDK dedup carries an `inflight` marker answering `BUSY` (handlers run outside the mutex; without it a concurrent retry double-executes).
- SDK `PeerTimeout` defaults to NEVER — a paused game is silent for as long as the player likes, and the guest's liveness is in ticks, which do not advance while paused.
- SDK adds `RequestAsync`, `Snapshot`, and a `Manual`/`Transport`/`Now`/`Rand` determinism seam for the conformance harness.
- **`RollbackTicks`, `Stats.GuestHigh` and `Stats.Rollbacks`**, from the join fix: the peer owns the clock, so it owns the clock anomaly. See [Rollback detection](#rollback-detection-the-clock-owning-side-owns-the-clock-anomaly).
- **Known and stated**: the guest retry budget (225 ticks to `ErrTimeout`) exceeds `LivenessTicks` (180), so a request whose answers are all lost while the peer is otherwise silent dies `ErrSessionLost`, not `ErrTimeout` — `ErrTimeout` is only reachable from a demonstrably-alive peer.
- Fragmentation is implemented twice (guest and SDK) with different allocation disciplines — the AD5 shape; the conformance suite driving them against each other was the mitigation, and `TestBothGuestLibrariesSpeakTheSameWire` now exists and is the stronger one.
- A corrupted `seq` poisons a channel until a snapshot arrives and nothing triggers one — unreachable on loopback UDP (checksummed), recorded not fixed.

### The Rust mirror — `guest/rust/fkipc`, as built (wave 2b)

Line for line, `snake_case`, `Result<_, Status>` where Go returns `Status`, and `&[u8]` where Go takes `[]byte`. Same constants, same state machine, same thirteen deviations from the draft. **Five places where it is NOT a line-for-line mirror, each forced by Rust rather than chosen** — this is the table to read before changing either side, because a "fix" that makes one look like the other is a fix that undoes one of these:

| | Go | Rust | why it could not be the same |
|---|---|---|---|
| **handler signature** | `func(m Message)`, reaching the link back through the package singleton — `state.Snapshot(…)` from inside `OnResync` is the documented shape | `fn(&mut Link, Message)`, and the handler uses the `&mut Link` it was handed | Reaching a singleton from inside a call that already holds `&mut` to it is **two live `&mut` to one object** — undefined behaviour, not a style question. Handing the borrow to the handler is the same capability, costs nothing, and the compiler checks it. `Channel`'s own methods still exist and answer `Status::NotOpen` from inside a handler rather than aliasing, so the unsupported route is a counted refusal |
| **the pump is two halves** | one `Pump(tick)` | `pump_begin` / `pump_end`, with the transport LIFTED OUT of the link across the poll | `recv_udp` dispatches every queued datagram as an event **from inside the call**, and each re-enters the module through `fk_on_event`, which reaches the singleton. Holding the borrow across the poll is the same UB. Go does not mind a second reference to its package's link |
| **…and its one visible consequence** | — | `write_bulk`/`notify_file` **from inside an inbound handler answer `NoTransport`** | for the duration of the poll the link holds no transport. It is loud rather than silent, and the shape it forces — set a flag, write from `fk_on_tick` — is the better one anyway in **both** languages: a `write_file` from inside an event dispatch is a host call nested inside a host call. `examples/ipc` does it that way in both, which is what the live gate's `bulk` leg drives |
| **naming a channel** | `fkipc.Chan(id, pri)`, one call, valid at package-`var` time | `Channel::new(id)` is `const` and `Channel::open(pri)` registers it from `_initialize` | Go runs package-level `var` initialisers BEFORE `init()`, so a channel really can be named before `Open` — which is why the Go singleton is a non-nil `&Link{}` from the first line of package initialisation, pinned by a test. Rust has no such phase in a cdylib reactor: a `static` takes a `const` initialiser and everything else happens inside `_initialize` in an order the guest wrote. **The hazard is absent rather than worked around** |
| **the peer's error record** | `PeerError{Code uint16; Message string}` | `PeerError{code: u16}`, with the message travelling as the reply's own borrowed `payload` | a `Reply` reaches a `fn` pointer, and an owned `String` would be an allocation on every failed request in a heap that is in the save. Same pair, different ownership |

Two smaller ones, stated so nobody counts them as drift: `stats()` returns `Stats` on this side, where Go must say `Stats() LinkStats` because a Go package cannot hold a function and a type of one name; and handlers are `fn` pointers rather than closures in both spirit and fact, because a `Box<dyn Fn>` in a `#[global_allocator]`-owned heap is a retention the collector would have to be told about.

**The two backends must stay level, member for member and constant for constant.** That is the standing lesson of "The Rust generator was four milestones behind" in `CLAUDE.md`: a feature added to one backend and not the other is invisible unless something diffs them. The instrument here is not a census row — nothing generates this — so it is three tests, in increasing strength: both suites assert the same wire constants; both codecs read `testdata/ipc/wire-vectors.txt` and must re-encode it byte for byte (`sdk/go/fkipc/vectors_test.go`, `guest/rust/fkipc/tests/vectors.rs`); and **`TestBothGuestLibrariesSpeakTheSameWire`** drives both compiled guests through the verbatim runtime under one host stub with one script and requires the frame sequences to be **byte-identical**, in the shape `TestADictionaryFieldCrossesInsideAnEventPayload` already uses for `guest/rust/examples/dict`.

**One asymmetry is real and is NOT fkipc's**: a packaged Rust guest wires no `fk_arena_mark`/`fk_arena_release`, because `guest/rust/fk` has no marshalling arena yet. It is recorded in `agents/abi.md` ("A Lua string is BYTES", last paragraph) and it means a Rust guest's per-call tier-2 wire blocks are not reclaimed at the dispatch bracket the way a Go guest's are.

---

## The external SDK — `sdk/go/fkipc`

**The two ends are not mirrors, and that is deliberate: one of them has a clock.** The external side may block, may use contexts, may use goroutines and may keep timers in real time. Forcing it into the guest's callback shape would be cargo-culting a constraint that does not apply.

```go
package fkipc // module github.com/Techrocket9/fklua/sdk/go

type Options struct {
    GamePort     uint16 // --enable-lua-udp <port>: the game's ONE socket
    ListenPort   uint16 // ours; MUST differ -- see below
    ScriptOutput string // for file pickup; DefaultScriptOutput() if empty
    MaxFrame     uint16
    Logger       *slog.Logger

    // Name is this companion's IDENTITY TOKEN, carried in HELLO_ACK.
    // ExpectedName is what the GUEST must call itself; "" = no check. A HELLO
    // whose name differs mints nothing, disturbs nothing, is counted in
    // Stats.NameRejects with the offered token in Stats.RejectedName, answers a
    // rate-limited BYE, and raises SessionRejected. Setting only ExpectedName
    // uses it as Name: one token names the CONTRACT.
    Name         string
    ExpectedName string

    // RollbackTicks: how far the GUEST's clock may run backwards before this
    // side calls the session dead. In GAME TICKS, because that is the clock
    // being watched. 0 = DefaultRollbackTicks (60).
    RollbackTicks uint32
}

func Dial(o Options) (*Session, error)

func (s *Session) OnSession(h func(ev SessionEvent, epoch uint32))
func (s *Session) Subscribe(channel uint16, h func(Message))
func (s *Session) Handle(channel uint16, h func(Request) ([]byte, error))
func (s *Session) Request(ctx context.Context, channel uint16, p []byte) ([]byte, error)
func (s *Session) Send(channel uint16, p []byte) error
func (s *Session) Resync(channel uint16) error
func (s *Session) OnFile(h func(FileNotify, io.ReadCloser))
func (s *Session) Stats() Stats
func (s *Session) Close() error

// DefaultScriptOutput returns the platform's Factorio script-output directory.
func DefaultScriptOutput() (string, error)
```

Three things the SDK owns because the guest cannot:

- **The clock.** Retry deadlines, the `PeerQuietMillis` throttle, and the stabilize-poll for a digest-less file are all real time here — **and, since the join fix, noticing that the GUEST's clock went backwards**, which is the one failure mode nothing on the guest side can see because everything a guest can compute travelled with the save.
- **File pickup.** `OnFile` waits for the file to satisfy the notify — exact length and checksum when `HAS_DIGEST` is set, size-stable across two polls when it is not — then hands the caller an open reader. **The path is configuration with a default, not a guess.** `scripts/run-probe.sh:72-74` guesses three ways and that is right for a harness that runs on one machine; an SDK a downstream author points at their own install must take it and only fall back.
- **`ListenPort != GamePort`, checked at `Dial`.** `--enable-lua-udp` binds ONE socket, which is both the game's receive socket and the source port of everything it sends (`scripts/run-ipcprobe.sh:36-40`, `:55-56`). A companion on the same port is not a subtle bug — it is the game talking to itself — and `Dial` refuses rather than producing a session that never receives anything.

### One codec, two consumers

The frame encoder/decoder lives in **`guest/go/fkipc/wire`** — no build tags, no imports outside the standard library — and the SDK module depends on it. Go builds per package, so a host program importing `guest/go/fkipc/wire` never compiles `fkapi`'s `//go:wasmimport` declarations. **The alternative, a copy in each module, is the shape this repo has already been burned by twice** — the Rust generator four milestones behind, and `AD5`, the identical defect in the identical function fixed on one side and left standing on the other because the test was written against one backend. **OPEN** if the orchestrator would rather have a third module for it.

---

## Limits and budgets — what a mod author must know

| | value | note |
|---|--:|---|
| frame ceiling | **3900 B** | under the 4 KiB string scratch and under the measured inbound wall (4,000 B arrives, 8,192 silently does not). An oversized `send_udp` **fails silently**, so the cap is enforced in the library |
| default negotiated frame | **2048 B** | leaves ~2 KiB of scratch for the handler's own host calls |
| message ceiling | **~62 KB** | 16 fragments; above that, `WriteBulk` |
| send budget | **8 frames/tick** | ≈ 31 KB/tick; bounds the worst tick, not the bandwidth. The engine neither coalesces nor rate-limits |
| outbound cost, full frame | **~54 µs** derived | 3876 B × 14 ns/byte, `fk_str`'s batched rate |
| inbound cost, full frame | **~40 µs** derived | 25 µs `fk_wstr` + ~14 µs `mem_copy` into a guest string |
| pump cost, live | **~12.5 µs/tick** derived | 0.075% of a frame |
| **inbound wall, ≥1 player** | **~6 kB/s** | ~100 B/tick of InputAction segment rate; **1.5 kB/s at ≥20 players** |
| inbound wall, 0 players | flat ~20 ms up to 500 kB | the headless/benchmark case |
| inbound latency | ≥1 tick SP, **~6 ticks MP** | latency hiding does not apply to InputActions |
| OS receive buffer | **256 KB**, silent overflow | including while paused or saving |
| `write_file` outbound | ~4 MB observed, **no flush guarantee** | hence the digest |

Every "derived" row above is arithmetic over numbers measured elsewhere in this repo (`agents/abi.md`'s 12.5 µs host call and the tier-2 string rates), not a measurement of this path. **None of them may be quoted as an fkipc measurement until something has measured fkipc.**

**The multiplayer statement, in one sentence a mod author can act on:** every inbound byte is replicated to every connected client through the multiplayer server and lands in the replay, so on a populated server the entire inbound budget is about one full frame every forty ticks — design the server profile as outbound telemetry with a control trickle back, and put anything bulk on the outbound side or in a file.

**Pause:** no ticks, no pump, silent loss after 256 KB. The peer's obligation is to go quiet on the guest's silence; the guest's is to drain on resume. Neither alone is sufficient.

---

## The test story

Four layers, and the first needs neither Factorio nor a toolchain — which matters, because CI has neither (`FKLUA_NO_GUEST_TOOLCHAIN=1` is declared in the job's `env:`). Layer 0, the codec vectors, is new since the draft and is the cheapest cross-language pin there is.

### 0. The committed wire vectors — `testdata/ipc/wire-vectors.txt`

Both codecs read the same file and each must decode every field out of it and **re-encode the identical bytes**: `sdk/go/fkipc/vectors_test.go` on the Go side (which is also the generator, under `-update`) and `guest/rust/fkipc/tests/vectors.rs` on the Rust one. It covers every frame type, every flag bit this version defines, a non-first fragment, an empty payload, the u32 extremes, and all 256 byte values.

It is the AD5 mitigation in its cheapest form — **make the BYTES the shared artifact rather than the parallel authorship** — and it needs neither wasm nor a toolchain, so it is the only cross-language pin that could run in CI. What it cannot catch is a change made to a codec and to the file in one commit; the golden diff is the review artifact for that, exactly as it is for the emitter's own goldens.

### 1. Protocol conformance, host-side, with no wasm anywhere

**The reference implementation of the guest state machine IS the guest library**, compiled for the host. That is the design constraint that makes this layer worth having rather than a second thing to keep in sync: `fkipc` talks to the outside through a small `transport` interface, whose `fkapi` implementation lives behind `//go:build tinygo.wasm` — NOT `wasm`, and not a `*_wasm.go` filename: `tinygo -target=wasm-unknown` reports **GOOS=linux GOARCH=arm**, so a `wasm` build tag matches nothing and the failure COMPILES — the off-target file wins, the fkapi path is dead-code-eliminated, and the mod loads and never speaks. The pruning assertion (0 event ids where it wants exactly 1) is what catches it — and whose test implementation is an in-memory link with an injectable fault model. The precedent is `guest/go/fkgc`'s own build-tag split.

Driven against `sdk/go/fkipc` over that link, with a fake tick source, the conformance suite asserts the things two implementations get wrong:

- handshake, token adoption, and the `HELLO_ACK` epoch-test exception;
- a session boundary mid-flight — pending requests fail `ErrSessionLost` and are **not** retried. The boundary is a `BYE`, because every boundary is a replicated signal now and the test has to arrive through the wire like the real thing;
- **a load ends nothing**: `Reload` moves no epoch, no `boot`, no channel seq, raises no session event, fails no request in flight, sends no `HELLO`, and the request that was in flight is answered afterwards under the same epoch;
- **the guest notices its own clock going backwards**, and the peer notices the one the guest cannot — a `HEARTBEAT` whose tick regresses past `RollbackTicks` is a `BYE`, a teardown, and a clean re-`HELLO`, with the u32 wrap and a within-tolerance regression as the two negative controls;
- **a busy guest still heartbeats**, which is what keeps the detector above fed;
- **two loads of one save produce the same `boot` and different tokens**, and the peer resyncs on the `HELLO` rather than on the epoch value;
- dedup: a retried `REQ` replays a cached `RESP` and the handler runs **once**;
- a response above `MaxDedupPayload` answers `DUPLICATE` on retry instead of re-executing;
- serial arithmetic at the u32 wrap boundary, both the gap and the drop arm;
- gap → `RESYNC` → `SNAPSHOT` clears the gap;
- fragment loss produces a gap and not a short message; reassembly timeout, `nfrag` disagreement, and interleaved `corr` on one channel;
- retry budget exhaustion, and the peer-down quiesce (`Send` becomes a counted no-op, not an error).

### 2. Guest end to end under `bin/lua52f`

A real TinyGo (and Rust) `examples/ipc` guest, packaged, run against the **verbatim** `fk_mod.lua` and `fk_abi.lua`, with `helpers` stubbed the way `commands` and `remote` already are in `internal/guest/callback_test.go:206-239` — engine-shaped, not convenient:

```lua
local inbox, sent = {}, {}
helpers = {
  send_udp = function(port, data, for_player) sent[#sent+1] = {port, data, for_player} end,
  recv_udp = function(for_player)
    -- Deliver queued datagrams the way the engine does: as events, on the
    -- caller's tick, through the registered on_udp_packet_received dispatcher.
    while #inbox > 0 do
      local p = table.remove(inbox, 1)
      handlers[EV_UDP]({ payload = p, source_port = 25411, player_index = 0,
                         name = EV_UDP, tick = TICK })
    end
  end,
}
```

The stub is a **fixture for the probe's findings**, and it carries them: one `recv_udp` drains the whole inbox as a batch of events, the payload is bytes, `game_version` reports the floor, and every method asserts its exact arity. That coupling is deliberate — a stub that models something the game does not do is a test that passes for the wrong reason, which is the failure the engine-shaped `commands`/`remote` stubs exist to avoid (`agents/abi.md`: a `function(self, x)` in a plain table is the shape that hid `Arguments count error` on every method in the API).

What it must assert that layer 1 cannot:

- **no frame is sent from `_initialize`** — `sent` is empty until the first `Pump`;
- a full-size frame arrives with **`ScratchOverflows == 0`**, and one byte over the negotiated size does not;
- the outbound `data` argument has the LocalisedString shape the probe found, and the bytes round-trip;
- a simulated load — re-`require("control")` with the persisted memory restored, then the after-load one-shot — sends **no `HELLO`**, and the epoch that was live before it still answers a `REQ` after it.

### 2b. THE JOIN — two module instances, word for word

`TestAJoiningPeerStaysByteIdenticalToTheServer` is the test the desync should have had, and it is a different question from anything above: not "does the protocol work" but **"do two peers running the same bytes over the same inbound stay byte-identical".**

It builds the SERVER the way the engine does — a fresh module instance, `on_init`, a full handshake and traffic — deep-copies `storage` at that point, which is the save a joining client downloads, then builds the JOINER over that copy: a second module instance in the same interpreter, `_initialize` rebuilding and `state_load` replacing it, and then `script.on_load`, **which is where `fk_mod.lua` arms the `fk_after_load` one-shot**. Both are then driven with the same ticks, and `storage.fk_mem`, `storage.fk_globals`, the size mirror and the persistent handle space are compared after every dispatch.

Four things about it are load-bearing:

- **`--persist=table` and `-opt=3`**, because that is what `fklua mod` defaults to and it is the mode in which guest memory IS `storage.fk_mem`. Under `--persist=none` — which the other two fkipc tests use, since they assert bytes on a socket — there is nothing in `storage` to diverge and the test would pass vacuously.
- **A `BuildID`**, because `state_load` refuses to adopt a heap whose build stamp does not match, and a joiner that adopted nothing would pass by starting from the same fresh memory the server would never have.
- **Both languages**, from one Go test over one Lua script, for the AD5 reason.
- **It fails against the load-reset design at the first joined tick**, with the `boot` word as the first divergence — confirmed by running it before the fix.

It also carried a **phase 2 that pinned a defect that was not fkipc's** (P12); that is closed, and its traffic is in the joined window now.

#### The corpus is FOUR guests, and the two that were added are the ones a mod resembles

`examples/{go,rust}/ipc` are WIRING fixtures. They exist to prove the exports and the library behind them, and their handlers deliberately touch nothing but a byte buffer — so what they could ever catch is a defect in the library or in the runtime under it, and never one in the code an author writes on top. The demo mods are the other half: **`demo-daylight` (Go) and `demo-circle` (Rust) drive a handler that calls the API, stores what it read, and streams a frame assembled out of the world**, which is what a mod is made of and is the shape `run-ipcdemo.sh --play` desynced on for real.

Three things the harness needed to carry them, each of which was a finding:

- **An engine-shaped WORLD, one per peer.** A shared one lets a write by the server repair a read by the joiner, so the comparison silently loses the ability to see the divergence it exists to find. The joiner's is built FROM the server's, because a joining client downloads game state — a joiner whose surface still read the default daytime would assemble a different telemetry frame out of a perfectly honest guest, i.e. a harness bug wearing a desync's clothes.
- **The arity check is a RANGE over the optional tail.** A method's argument count is exact, but a *trailing optional* argument the guest omits is genuinely absent rather than nil — measured here rather than assumed: `ProfileServer` calls `send_udp` with **three** arguments and `recv_udp` with **one**, and `ProfileClient` calls them with **two** and **zero**. A stub asserting the server's count makes every client-profile call raise inside `fk_abi`'s pcall, come back as `ERR_CALL_FAILED`, and leave the guest silently doing nothing — which is how the demo arms first "passed", identically and worthlessly, on both peers.
- **An anti-vacuity assertion on what the guest COMPUTED.** Every failure mode the harness has fails on both peers in the same way, so "identical" survives it. The last telemetry frame of the joined window is compared across the two peers *and* against what the arm says the guest must have derived from the world it was given — `daytime=900 frozen=1 player=1 px=-350 py=1225` for the Go arm, `radius=40 hue=210 evo=134000 entities=80` for the Rust one, where `entities` is the stub's `floor(radius × 2)` and therefore a number no guest can produce without having reached the host and come back.

#### The joiner's SOCKET IS NOT BOUND, which is what makes the second instance testable at all

The window's asymmetry used to be only that the joiner runs `on_load` and the server does not — which catches the `fk_after_load` class and nothing else. It now also models the condition that produced the measured desync: **the server was started with `--enable-lua-udp` and the joining graphical client was not**, so `send_udp` answers differently on the two peers on every frame of the session, with no companion and no inbound datagram anywhere near it.

The stub RECORDS what the guest assembled and THEN refuses, so the harness still sees the buffer — a fact about guest memory, which must match — while the guest sees only the error, which must reach nothing. *Confirmed by mutation: restoring the pre-fix `if l.tr.Send(f) == StatusOK { TxFrames++ } else { QueueDrops++ }` turns the `demo-daylight` arm red at tick 90 on three words — `TxFrames` 6 vs 5, `TxBytes` 330 vs 292, `QueueDrops` 0 vs 1 — where before this arm existed the same mutation was invisible to every test in the repo except the two links' own counter comparison.* The library's structural answer is the void transport seam above; this is where it is measured rather than declared.

### 3. The live gate — `scripts/run-ipc.sh`, as built

`scripts/run-ipcprobe.sh` is the first thing in this repo that talks to a running Factorio; **this is the first that speaks the PROTOCOL at one**, and it is the standing gate rather than a one-shot measurement. A headless server with `--enable-lua-udp` and `auto_pause: false` — the probe's load-bearing key: a headless server with nobody connected pauses, `on_tick` never fires, the pump never runs, and that reads exactly like "UDP does not work on this build" — plus a companion, `sdk/go/cmd/ipcgate`, **built from the SDK module the way an outside tool would build it**. That is a gate in itself: `sdk/go`'s only dependency on this repo is `guest/go/fkipc/wire`, so a `//go:wasmimport` leaking into that package fails here rather than in somebody's build.

Eight legs, each a named `PASS`/`FAIL` line: the session coming up, an RPC round trip carrying **all 256 byte values**, a periodic telemetry `MSG`, the guest's own **clock** seen to advance in its heartbeats, a `RESYNC` answered with a `SNAPSHOT`, a `WriteBulk` file picked up and verified against its digest **and its content**, **exactly one session** for the whole run, and a clean `BYE` — whose guest-side half the script reads out of the game's own log, because only the guest can say it tore the session down. **Both ends of this gate now state the same identity token** (`fk-ipc/1`, in `examples/ipc` in both languages and in `ipcgate`), which costs no leg and makes every run a POSITIVE CONTROL for the schema filter: a check that is only ever exercised in its refusing direction is a check nobody has watched succeed. The refusing direction is `run-ipcdemo.sh --smoke`'s identity leg. `LANG_=rust` runs the identical conversation against the Rust example guest, the `run-guest.sh` precedent for language arms. Every derived path is per-language, including the map: a save records the mods it was made with.

Two of those are new with the join fix and both exist because something that was silent became measurable. **`clock`** samples `Stats().GuestTick` twice a second and a half apart: the tick crosses only in a `HEARTBEAT`, and while the heartbeat was suppressed by any other outbound frame this reading was frozen at the `HELLO` for the whole session with nothing saying so. **`sessions`** asserts the count is one, which is where P6 is buried — see below.

**What is comparable across two runs, and what is not, is the interesting part.** `run-guest.sh` compares two `--benchmark` runs line for line because a replay of one save is the same computation twice. This is not that, and two things in an fkipc session are entropy or a race:

- **the epoch is peer-minted** — the guest cannot mint a unique session id, and that is [a theorem](#the-guest-cannot-mint-a-unique-session-id-and-this-is-not-a-limitation-to-work-around) rather than a limitation — so every frame in a session differs from the last run's in four bytes, by design;
- **the tick a datagram lands on is a race** between the companion's real clock and the game's update loop, so seq numbers, the tick inside a telemetry payload, and the number of heartbeats in a window all move.

So the gate compares the two things that *are* determined: the guest's own session-state progression, read out of the game log (`up`, `down`, in that order, every run, in both languages), and each leg's **verdict** with the detail dropped. `awk '{print $1, $2}'` is that distinction, and it is why the companion prints the epoch in the detail field and never in the verdict.

**Those two lines used to be three, and the missing one is P6 closing.** The first was `reset`: starting a headless server LOADS the map, so the guest reloaded on its first tick, and the first-tick ordering window put one `HELLO` on the wire carrying the pre-reload `boot` and a second carrying the post-reload one, one tick apart. A `HELLO` is unconditionally a new session at the peer, so a companion listening for both minted two and the request in flight across the boundary died `ErrSessionLost` — and whether a run saw it was a race between when the companion bound its socket and when the guest first ticked, measured on 2.1.14 as `sessions=2` and `sessions=1` on two consecutive runs of one arm. **Nothing resets on a load now, so the count is determined and it is a verdict rather than a field in the STATS line.** `ask()`'s recovery is kept in the worked example, because a session can still end under a request — a `BYE`, liveness, or the companion noticing the guest's clock go backwards — and `ErrSessionLost` means the outcome is UNKNOWN, which only the application can resolve.

---

## Open items

Eight of the twelve are closed. They are kept with their outcomes rather than pruned, because what a decision *was* is how the next one gets made.

| | item | owner |
|---|---|---|
| ~~**P1**~~ | **CLOSED — the probe ran, twice.** Every constant it was waiting on is measured and every table above carries the number instead of the marker; the two re-run tables near the top of this file are the evidence, and two working assumptions moved (`DrainMax` 8 → 1, and `BaseFloorRecv` from "unknown" to a measured 2.1.14). One thing it could not answer stays open and is now stated as such rather than marked: `recv_udp`'s own per-call cost is still the ~12.5 µs *derived* figure, because nothing has separated the poll from the event dispatch it performs. | closed |
| ~~**P2**~~ | **CLOSED — the arena fix shipped in the same branch and is validated in game.** `fk_arena_mark`/`fk_arena_release` bracket the outermost dispatch, so an over-scratch inbound frame is a per-packet cost rather than a permanent leak. Both halves the spec asked for: it ships *with* fkipc, and `MaxFrameCeiling` was re-checked against it and **stays 3900** — the argument moved from correctness to cost, and the inbound wall the 2.1.14 re-run bracketed (4,000 arrives, 8,192 does not) binds tighter than the arena does. `scripts/run-ipc.sh` exercises the bracket on every inbound frame of every run. | closed |
| ~~**P3**~~ | **CLOSED — yes.** `agents/ipc.md` has an index row in `CLAUDE.md`'s `agents/` table as of the closeout wave, and the draft header is deleted. | closed |
| ~~**P4**~~ | **CLOSED — no, and the as-built confirms it.** The port and profile are guest source; nothing in `fklua.toml` mentions IPC and nothing in `internal/factorio` knows the feature exists. A manifest section would have put the port in two places, which is the "two commands disagreeing about one manifest key" shape this repo has already been bitten by twice. | closed |
| ~~**P5**~~ | **CLOSED — `guest/go/fkipc/wire`, shared, and the fixture that keeps the two codecs honest exists.** No build tags, no imports outside the standard library, and `sdk/go` depends on it; Go builds per package, so a host program never compiles `fkapi`'s `//go:wasmimport` declarations. The third module was not needed. `testdata/ipc/wire-vectors.txt` now has readers on **both** sides — see layer 0 of the test story — where it had only the Rust one. | closed |
| ~~**P6**~~ | **CLOSED, and not the way it was going to be.** The first-tick window was `Pump` running once with the restored session state before `Reload()` cleared it, so a load put two `HELLO`s on the wire one tick apart and a peer that heard both minted two sessions. The prescribed fix was a `fk_mod.lua` ordering change — `fk_after_load` before `fk_on_tick` — with a golden-file and `Hooks`-mirror cost. **It was never the right fix, because it would have kept the reset**, and the reset was the actual defect: `fk_after_load` fires on a joining multiplayer client and on no other peer, so writing guest state there desyncs the joiner. A load resets nothing now, so there is nothing on either side of the window and the ordering stopped mattering. `sdk/go/cmd/ipcgate` asserts `sessions == 1` as a verdict. | closed |
| ~~**P12**~~ | **CLOSED — the caches are mirrored into `storage`, and the fix reached one key further than the report did.** The defect was that `fk_mod.lua`'s `event_buffer(level)` cached the per-nesting-level event scratch buffer in a Lua LOCAL while allocating it out of the guest HEAP: a load rebuilds the local empty and the heap comes back from the save, so the first dispatch after a load allocated a second buffer beside one already there — `event_scratch` bytes per level per load, one more entry pinned in `kept`, and every later allocation landing that much further up than on an instance that never reloaded. **It bound every mod that receives an event with a payload, not only an IPC one.** <br><br>**AS BUILT: `storage.fk_bufs`, a live table aliased into `storage` exactly as `storage.fk_handles` is** — published by `state_init` on a fresh heap, adopted back by `state_load` under the `same_build()` gate it already applies to the heap, because a buffer address means nothing outside the heap the build that made it laid out. Three details are load-bearing. `state_load` only READS `storage` and writes two upvalues, which is the one thing `on_load` may do; the WRITE lives at the allocation, inside a dispatch, which is the only replicated place it could go — and it is legal there for a reason worth stating, that an allocation which already mutates `storage.fk_mem` cannot be on a peer-local path without the allocation itself having been the desync. And it is called at the allocation rather than only from `state_init` so that a save written by an OLDER runtime heals on its first load instead of never: the build stamp is over the guest wasm and the packaged `--api` pin and nothing about FkLua itself, so upgrading FkLua and repackaging leaves `same_build()` true over a save that carries no mirror. <br><br>**THE CALL BUFFER HAD THE SAME DEFECT AND IS FIXED IN THE SAME LINE.** `call_buffer(level)` — the two tier-2 slots a command or remote-method trampoline dispatches through — is the same lazy `fk_alloc_static` cache one key over, and taking only the event buffer would have been this repo's own recorded failure shape. `storage.fk_bufs` carries both (`.ev`, `.call`). <br><br>**AND THE SIZE TRAVELS WITH THE ADDRESSES, which is the one part of this that is not bookkeeping.** An address says where a buffer starts and nothing about how much room is behind it, and the size is **not a constant of the guest**: `API.event_scratch` is the largest subscribed event's payload, computed from the PACKAGED event table, so two packages of one wasm against two API pins can disagree about it, and `fk_migrate_adopt` hands over another build's heap outright. (The stamp folds the `--api` pin in since 2026-08-07, so the first of those two takes the rebuild path now; the size guard stays for `fk_migrate_adopt`, which hands over a different build's heap on purpose, and for the mirror-less save above.) Reusing a buffer allocated smaller than what `write_struct` is about to put in it is a silent overwrite of whatever the guest allocated next, which is strictly worse than the leak being fixed, so `state_load` refuses a mirror whose recorded size is not the size being asked for and lets the allocation happen again — one buffer, once, against a class of corruption with no error message. `storage.fk_bufs` therefore carries `.evn` and `.calln` beside the two tables. <br><br>**What was and was not established stands.** The in-game consequence was never demonstrated — the closing `--play --launch` run held a graphical client joined for 120 s with inbound firing throughout and logged zero desync lines — so this closes as a per-load leak plus a host-side divergence that is now gone, not as a desync that was cured. `TestAJoiningPeerStaysByteIdenticalToTheServer`'s phase 2 flipped to failing under the fix in **both** language arms (`JOINEV identical`), which was its whole design; it is deleted and its traffic is in the joined window, so those 65 ticks now compare **event-carrying** dispatches — six datagrams, one from a foreign source port as the attribution leg. Pinned directly by `TestTheEventBufferIsAllocatedOncePerHeapAndNotOncePerLoad`, six sessions of one guest through the real `control.lua`, asserting at the address and the allocation count: new map, a same-build load that REUSES, a rebuilt guest that must NOT (the negative — a stale pointer would read zero allocations in a fresh heap), a mirror-less save that pays for exactly one twin and then heals, and a mirror recorded at another size that must be refused. Against the unfixed runtime it reads `32768 → 32896 → 33024` with `kept 1 → 2 → 3`. | closed |
| **P7** | **Aligning the scratch bump to 8** so a frame's base address is aligned and the header's aligned layout actually buys aligned loads. One line in `fk_abi.lua`; worth taking in a phase that is already editing it. | later phase |
| **P8** | **Custom-input events** — the push-to-talk requirement, and the one genuinely new non-IPC ABI gap. Custom inputs subscribe **by string name** with dynamic ids, and `fk.subscribe` is numeric over the generated event table; the fix shape is a third `fk.register` kind, the same seam as commands. Needed by the interactive-agent profile, not by the server profile. Explicitly a separate follow-on milestone. | follow-on |
| **P9** | **`localised_print`** stays unbound. UDP-only deleted the only reason phase 1 had to bind it, and `agents/abi.md`'s "left unbound" stands. Revisit only with a use case that UDP does not serve. | not scheduled |
| **P10** | **Multiplayer-client IPC.** `for_player = N` is deterministic when N is fixed and identical on every peer, so "designate player 3's client as the bridge" is expressible today. What is *not* expressible is a guest branching on which peer it is. Nothing in this spec forbids the former; nothing in phase 1 tests it. **Its sibling — SEVERAL MODS in one game — is expressible and GATED as of the slider demo**: the source-port filter is what makes it sound, and `run-ipcdemo.sh --smoke` holds two mods' sessions concurrently with an isolation leg. **The IDENTITY leg lives there too, and it is the one leg that has to disturb the sessions every other leg needs**: a companion binds one socket per mod port, so proving a token mismatch against a live game means closing the matched session and opening a mismatched one on the same port. It runs last, in three arms — the companion refusing the guest's `HELLO`, the guest refusing the companion's `HELLO_ACK` (which needs a companion that ANSWERS with the wrong token, because a fully-swapped pair never gets as far as an ACK), and the matched pairing restored as the positive control — and the guest's own half is read out of the game log as **exactly two `session up` lines with none in between**. Also measured there: **ProfileClient (bare `recv_udp()`) holds a full session on a HEADLESS 2.1.14 server** — the last unmeasured cell from the probe — so one profile serves both a graphical session and a headless gate. **Both are gates as of 2026-08-07**: `--smoke-single` runs the identical leg set against one graphical single-player process, so the two mods sharing one socket are now proved in the environment where a mod author will actually meet them. **"omitted `for_player`" was a claim about the CONFIG and not about the wire until 2026-08-07**, and the difference is the whole of why this row's headless measurement was green while the same guest could not hold a session in graphical single player: `Open` built the transport before the omission was applied, so ProfileClient sent with `for_player = 0`, and 0 IS the server on the very environment this row measured. Fixed in both languages; see "The graphical single-player re-run". | later phase |
| **P11** | **A muxing daemon.** Deleted from phase 1 as mandatory infrastructure — the deliverable is a protocol spec plus client SDKs, and external tools speak the framing directly. A daemon earns its place the first time one game needs several independent consumers. Same for a Clusterio-compatibility translator, which is entirely external-side and needs no game change ever. | on demand |
