# FkIPC

FkIPC ("Factorio, kommunikativ (per IPC)") is a message-oriented link between a mod built with [FkLua](../../../README.md) and a companion process on the same machine, carried over Factorio's UDP surface. It gives a guest (the wasm program FkLua compiles to Lua) sessions, channels, correlated request/response with deduplication, gap detection answered by a snapshot, fragmentation of larger messages, and bulk transfer as a file plus a digest.

It ships as three packages that speak one wire format:

| package | side | language |
|---|---|---|
| [`guest/go/fkipc`](.) | inside the game (TinyGo guest) | Go |
| [`guest/rust/fkipc`](../../rust/fkipc/) | inside the game (Rust guest) | Rust |
| [`sdk/go/fkipc`](../../../sdk/go/fkipc/) | the companion process | Go, its own module |

The codec is one package, [`guest/go/fkipc/wire`](wire/), imported by the Go guest library and by the SDK; the Rust guest library carries a mirror of it, and both read the same committed vectors ([`testdata/ipc/wire-vectors.txt`](../../../testdata/ipc/wire-vectors.txt)) so they cannot drift. FkIPC uses only surfaces the FkLua bindings already expose: no new import, no runtime change, no member-table row.

## Requirements

- **Factorio 2.1.14 or newer.** Below that the library is inert: `Open` returns `StatusDisabled`, logs one line (`fkipc: disabled -- requires Factorio >= 2.1.14; this engine is X`), sends nothing, and every `Send`, `Request`, `WriteBulk` and `NotifyFile` answers `StatusDisabled` (counted in `Stats().Refusals`). The reason is a measured engine defect: on 2.0.77 a headless `recv_udp` with a packet queued aborts the process in C++, which no `pcall` can catch. The floor is about the running engine, not the API pin: every member FkIPC calls exists in the 2.0.77 description, so a mod built at FkLua's default pin gets the whole library the moment it runs on a 2.1.14 engine, with no rebuild. The gate is re-read once a second while shut, so a save moved onto a newer engine comes up by itself. On Steam, 2.1.x is the `2.1.14` entry under the game's Betas tab.
- **`--enable-lua-udp <port>`** on the game's command line (a Steam launch option is the same flag). That binds one socket, which is both the game's receive socket and the source port of everything it sends, so **the companion must listen on a different port**; the SDK's `Dial` refuses a matching pair rather than producing a session that never receives anything.

## The guest side

Three lines, in Go:

```go
func init() { fkipc.Open(fkipc.Config{Port: 29434, Name: "my-mod/1", ExpectPeer: "my-mod/1"}) }

//go:wasmexport fk_on_tick
func onTick(tick uint32) { fkipc.Pump(tick) }

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) { if fkipc.OnEvent(id, ptr) { return } /* your own events */ }
```

and the same three in Rust:

```rust
fn init() { fkipc::open(Config { port: 29434, name: "my-mod/1", expect_peer: "my-mod/1", ..Default::default() }); }

#[no_mangle] pub extern "C" fn fk_on_tick(tick: u32) { fkipc::pump(tick); }

#[no_mangle] pub extern "C" fn fk_on_event(id: u32, ptr: u32) { if fkipc::on_event(id, ptr) { return; } /* your own events */ }
```

Each line is unavoidable for a different reason. A wasm module has one export per name, so the library cannot own `fk_on_tick` or `fk_on_event`; your program owns them and routes in. `Open` goes in an initialiser because that runs during `_initialize`, on every load, which is the only place a subscription may go (`fk_on_init` fires only on a new map, and event registrations are not saved). `OnEvent` returns a bool so the event id stays inside the library, where `fklua mod`'s constant scan can still prove it and prune the event table to one descriptor.

Channels are opened from the same initialiser (`STATE.open(Priority::Bulk)`, `CONTROL.open(Priority::Control)` in the Rust example) with handlers for messages, requests and resyncs. Splitting telemetry and control onto separate channels is the standard shape: a channel's sequence counter is shared by everything on it, so a lost request would otherwise raise a gap, and therefore a snapshot, on the telemetry beside it.

## The companion side

From any Go program, with the SDK module:

```go
s, err := fkipc.Dial(fkipc.Options{GamePort: 29433, ListenPort: 29434, ExpectedName: "my-mod/1"})
s.Subscribe(1, func(m fkipc.Message) { fmt.Println("state:", string(m.Payload)) })
out, err := s.Request(ctx, 2, []byte("ping"))
```

The two ends are deliberately not mirrors: this side has a wall clock, may block, and may use goroutines, so retry deadlines and the quiet-peer throttle are real time here while the guest counts ticks (reconciled by the tick every heartbeat carries). The SDK also owns file pickup (`Session.OnFile` waits for a written file to satisfy its notify) and the port check. It depends on the guest module through a `replace` directive, so an importer outside this repository needs the guest module published or vendored.

**Give the pairing an identity.** Set `Config.ExpectPeer` in the guest and `Options.ExpectedName` in the companion to one build-time token; the convention is `"<mod-name>/<schema-tag>"`, where the tag is your own claim about channel compatibility, not a build id. Each end then refuses a handshake with anything else, so a swapped port or a companion left running from last week is a session that never comes up instead of two ends silently disagreeing about what channel 1 means. It is a correctness check, not an authentication boundary: the token is a constant in a mod zip anyone can read.

## What it costs, and the four numbers to design around

| | |
|---|---|
| **frame cap 3,900 B** | the protocol maximum, negotiated down to 2,048 by default. An oversized `send_udp` fails silently (no error, nothing on the wire), so the library enforces the cap. Above one frame, send a file |
| **inbound is about 6 kB/s** once one player is connected, and 1.5 kB/s at twenty | every inbound byte becomes an InputAction: replicated to every client, written into the replay, quantized to a tick. Talk a lot, listen a little, and make the listening idempotent |
| **outbound is free** | a local side effect that never enters game state |
| **pause is silent loss** | no ticks, no pump, and the OS buffer is 256 KB with no notification when it overflows. The guest drains on resume; the peer must go quiet on the guest's silence, which the SDK does by default |

Direction decides the whole design. Outbound frames are local side effects; inbound datagrams arrive at every peer identically, at the same tick, through the engine's own replication, which is what makes it legal for a guest to branch on them and what lets the companion mint the session epoch (the guest cannot: everything it can compute travels with the save, so nothing it computes can tell two loads of one save apart).

## The join-safety contract

Read this before writing a handler. A multiplayer client that joins a running game downloads guest memory and then simulates alongside every other peer, and Factorio checks that memory every tick, so:

**You may branch on, and store what you decided from:** a message, request or reply payload (inbound is replicated); a session event; the tick handed to `Pump`; `Stats` (every counter is a function of those, of build-time configuration, or of the link's own decisions); your own guest state; and the world you read through the bindings.

**You must never store:** whether an outbound host call succeeded (`send_udp`, `write_file` and `rcon.print` are local side effects and their outcome is a fact about how *this* peer was launched; `--enable-lua-udp` binds the socket and a joining graphical client has no such flag), or anything computed in `fk_after_load` (it fires on the joining peer and on no other). The library's transport seam returns nothing at all, so the first mistake cannot be made through it. If you want to know whether your own write landed, say so with `fk.Log`: the game log is per-peer by nature and is the one sanctioned sink for a per-peer fact.

The full statement, with the reasoning, is in the package documentation of both guest libraries ([`doc.go`](doc.go), [`lib.rs`](../../rust/fkipc/src/lib.rs)).

## Examples and gates

- [`guest/go/examples/ipc`](../examples/ipc/main.go) and [`guest/rust/examples/ipc`](../../rust/examples/ipc/src/lib.rs): the guest wiring, mirrored in both languages and compared byte for byte on the wire.
- [`sdk/go/cmd/ipcgate`](../../../sdk/go/cmd/ipcgate/main.go): the worked SDK consumer, which [`scripts/run-ipc.sh`](../../../scripts/run-ipc.sh) drives through six named legs (session, an RPC carrying all 256 byte values, telemetry, resync and snapshot, a file checked against its digest, a clean bye) against a real headless game in either language.
- [`sdk/go/cmd/ipcdemo`](../../../sdk/go/cmd/ipcdemo/) with [`scripts/run-ipcdemo.sh`](../../../scripts/run-ipcdemo.sh): two mods on one socket, automated (`--smoke`, and `--smoke-single` for graphical single player) or interactive (`--play`), which is where the mod-isolation rules (source port, pairing name) were measured.
- Both gate scripts refuse to start below the engine floor and say why, rather than timing out leg by leg.

The wire format, the session and epoch design, the filter ladder, the measurements behind every number above and what is still open are in the maintainer notes, [`agents/ipc.md`](../../../agents/ipc.md).

## License

FkIPC is part of FkLua and is covered by its [MIT License](../../../LICENSE).
