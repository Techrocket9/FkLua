// Package fkipc is a message-oriented IPC link between a FkLua guest and a
// companion process on the same machine, over Factorio's UDP surface.
//
// It is a hand-written package beside fkapi, modelled on fkgc: not generated,
// not part of the bindings, no census row. It calls the generated bindings and
// never names a member or an event id itself.
//
//	func init() { fkipc.Open(fkipc.Config{Port: 29434, Name: "my-mod"}) }
//
//	//go:wasmexport fk_on_tick
//	func onTick(tick uint32) { fkipc.Pump(tick) }
//
//	//go:wasmexport fk_on_event
//	func onEvent(id, ptr uint32) {
//		if fkipc.OnEvent(id, ptr) {
//			return
//		}
//		// ... your own events
//	}
//
// Three lines, and each is unavoidable for a different reason. A wasm module
// has ONE export per name, so this package cannot own fk_on_tick or
// fk_on_event -- the guest author's program owns them and routes in.
//
//   - Open from init(), never from fk_on_init. Package initialisers run inside
//     _initialize, which runs on EVERY load, and event registrations are not
//     saved. fk_on_init fires on a new map only.
//   - Open SENDS NOTHING. _initialize is control.lua's main chunk, where 2.1
//     documents a non-zero for_player on send_udp as silently skipped. The
//     first frame goes out from the first Pump, which is inside a dispatch.
//   - OnEvent returning bool so the event-id constant stays in here. fklua mod
//     prunes the event table by scanning for an i32.const reaching
//     fk.subscribe, and an id it cannot prove constant ships every descriptor
//     there is -- about 55 KB of Lua per load at the 2.1.14 pin, silently.
//
// THERE USED TO BE A FOURTH LINE, `//go:wasmexport fk_after_load func
// afterLoad() { fkipc.Reload() }`, and it is now optional because [Reload] does
// nothing. Keeping it costs nothing and breaks nothing; a guest with no other
// use for fk_after_load may drop the export. WHY it does nothing is the most
// important thing in this package to understand before changing any of it --
// see [Reload].
//
// # The cost model, which decides everything by DIRECTION
//
// OUTBOUND IS FREE. send_udp and write_file are local side effects. Every peer
// in a lockstep game executes the same guest code and would perform the same
// send; for_player is the knob that says which peer's copy actually goes out.
// Nothing about an outbound frame enters game state.
//
// INBOUND IS EXPENSIVE. A received datagram becomes an InputAction: it is
// replicated to every peer through the multiplayer server, it lands in the
// replay, and it is quantized to a tick. The API's own description says so. On
// a populated server the whole inbound budget is about one full frame every
// forty ticks.
//
// So the design brief is TALK A LOT, LISTEN A LITTLE, AND MAKE THE LISTENING
// IDEMPOTENT. It also buys the one thing that makes this protocol legal at all:
// inbound data arrives at every peer identically, at the same tick, through the
// engine's own replication, so a guest may branch on it without desyncing.
// That is what lets the peer mint the session epoch.
//
// # The rule that falls out of it, and it is the one this package got wrong
//
// NO PEER-LOCAL SIGNAL MAY MUTATE GUEST STATE. Under the default
// --persist=table, guest memory IS storage.fk_mem, and Factorio CRCs that
// across every peer in a multiplayer game. So the only things a guest may
// branch on when it writes are its own state, the tick, and what arrived
// through the replicated inbound path. fk_after_load is none of those -- it
// fires on a joining client and on no other peer -- which is why [Reload] is a
// no-op and why every session boundary in here is driven by a BYE, by the
// liveness test, or by the guest's own clock. See [Reload].
//
// # The join-safety contract
//
// This is the whole rule, in the form a mod author needs it. A multiplayer
// client joining a running game downloads guest memory and then simulates
// alongside every other peer; Factorio CRCs that memory every tick. So:
//
// YOU MAY BRANCH ON, AND STORE WHAT YOU DECIDED:
//
//   - a [Message], [Request] or [Reply] payload -- inbound is replicated, which
//     is what makes it the one expensive direction worth paying for;
//   - a [SessionEvent], and anything derived from one;
//   - the tick handed to [Pump];
//   - [Stats] -- every counter in it is a function of the three above, of
//     build-time configuration, or of this link's own decisions;
//   - your own guest state, and the world you read through fkapi.
//
// YOU MUST NEVER STORE:
//
//   - WHETHER AN OUTBOUND HOST CALL SUCCEEDED. send_udp, write_file and
//     rcon.print are local side effects and their outcome is a fact about how
//     THIS peer was launched -- --enable-lua-udp binds the socket and a joining
//     graphical client has no such flag. Attempt it and drop the answer. This
//     package no longer offers you one: its transport seam returns nothing at
//     all, pinned by TestTheOutboundTransportSeamHasNoReturnValue.
//   - ANYTHING COMPUTED IN fk_after_load. It fires on the joining peer and on
//     no other. See [Reload].
//
// If you want to know whether your own write landed, say so with fk.Log. The
// game log is not CRC'd and is per-peer by nature, which is exactly where a
// per-peer fact belongs -- and it is the ONLY sanctioned sink for one.
// [WriteBulk] is the worked example of the pattern: it attempts the write,
// ignores the outcome by construction, and sends the FILE_NOTIFY that consumes
// the channel's seq unconditionally, because a peer that skipped the notify
// would advance that counter differently from its neighbours.
//
// The library's own half of this is enforced by
// TestAFailedSendIsInvisibleToGuestState and, end to end through the real
// runtime with a joiner whose socket is not bound, by internal/guest's
// TestAJoiningPeerStaysByteIdenticalToTheServer. Your half is yours.
//
// # Determinism is a correctness property here, not a style
//
// Nothing in this package iterates a Go map into wire bytes, mints a
// correlation id from randomness, or reads a clock. Every timer is in GAME
// TICKS, because there is no wall clock in the sandbox and a tick is exactly
// the unit whose pauses are the game's pauses. A JSON object built by iterating
// a map produces a different byte string on different peers -- and while
// for_player = 0 means only one peer's bytes reach the socket, the guest STATE
// that produced them differs on every peer, and that is a desync. The same rule
// made every dictionary in the generated bindings an ordered pair slice.
//
// # What the probe measured, and the one law it imposes
//
// PUMPING IS FATAL WHERE IT IS NOT USELESS, BELOW AN ENGINE FLOOR. On Factorio
// 2.0.77 a headless server calling recv_udp with a packet queued dies at
// TickClosure.cpp:91 -- a C++ abort no pcall can catch, reproduced five times
// in five runs. It needs BOTH the pump call and a queued packet; recv_udp on an
// empty socket is safe, and so is a socket nobody reads. On 2.1.14 the same arm
// survives and delivers.
//
// So below [MinEngineVersion] THIS PACKAGE IS INERT. [Open] answers
// [StatusDisabled] and logs one line saying so, [Pump] does nothing at all --
// no poll, no HELLO, no heartbeat, not one datagram -- every [Channel.Send],
// [Channel.Request], [WriteBulk] and [NotifyFile] answers the same deterministic
// refusal, and [Stats] counts them in [LinkStats.Refusals]. It ran SEND-ONLY
// down there until 2026-08-07, which was wrong for a reason worth keeping: a
// session is established by a HELLO_ACK and an ACK arrives INBOUND, so a link
// that can only talk searches forever and refuses every send it is handed.
// "Outbound is free" is true of the cost and false of the usefulness.
//
// THE FLOOR IS ABOUT THE ENGINE AND NOT ABOUT THE API PIN. Those are separate
// axes: the pin is a build-time choice of runtime-api.json and defaults to the
// general-availability release, while this reads helpers.game_version at RUN
// TIME. Every member this package calls exists in the 2.0.77 description, so a
// mod pinned to GA gets the whole library on a newer engine -- no rebuild, no
// repin, no second build of the guest.
//
// Everything else the probe found is baked into the constants rather than
// argued here: the frame ceiling clears the inbound wall (4,000 B arrives,
// 8,192 B silently does not) as well as the guest's 4 KiB string scratch;
// an oversized send fails SILENTLY, so the cap is enforced in this package
// because the transport will not report it; bytes cross byte-exact in both
// directions including NUL, in the {"", frame} LocalisedString form this
// package uses; and one recv_udp per tick drained a 20-packet backlog in one
// tick, in order, complete.
//
// # Four filters, and one of them is yours to configure
//
// A frame reaches a handler only after four independent questions, and it is
// worth knowing which is which because they fail differently:
//
//   - THE HELLO IS THE SESSION BOUNDARY. Everything about the old session goes.
//   - THE EPOCH IS THE FRAME FILTER. A frame under any other session is dropped.
//   - THE SOURCE PORT IS THE MOD FILTER. --enable-lua-udp binds ONE socket for
//     the whole game, so every mod is handed every mod's datagrams.
//   - THE NAME IS THE SCHEMA FILTER, and it is the only one that can refuse a
//     peer whose transport is entirely correct.
//
// The first three are automatic. The fourth is [Config.ExpectPeer], which is
// empty by default and therefore off: set it, and a HELLO_ACK from a companion
// calling itself anything else is refused rather than adopted. That is what
// turns a swapped port config or a companion left running from last week into a
// session that never comes up, instead of two ends that agree about every byte
// of the transport and disagree about what channel 1 means.
//
// It is a CORRECTNESS check, not an authentication boundary: the token is a
// constant in a mod zip anybody can read. See agents/ipc.md.
//
// # What a handler may keep
//
// NOTHING IT DOES NOT COPY. Every payload handed to a handler is a view into
// this package's own receive buffer and is invalid the moment the handler
// returns. That is the same rule as a transient handle and the host's string
// scratch region, and it is the same rule for the same reason: the buffer is
// reused on the next frame.
package fkipc
