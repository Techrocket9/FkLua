package fkipc

// The transport seam.
//
// THE REFERENCE IMPLEMENTATION OF THE GUEST STATE MACHINE IS THIS PACKAGE,
// COMPILED FOR THE HOST. That is what this interface is for. The fkapi
// implementation lives behind //go:build wasm and the conformance suite's is an
// in-memory link with an injectable fault model, so the protocol tests exercise
// the code that ships rather than a second implementation somebody has to keep
// in sync. The precedent is fkgc's own build-tag split.
//
// It is deliberately five methods, and it was four until the engine gate
// started refusing outright. Anything richer would start encoding policy --
// retries, budgets, the gate itself -- into the thing the tests replace, and
// then the tests would not be testing the policy. A log SINK is not policy: the
// library decides what to say and when, and this is only where the bytes go.
//
// SEND AND WRITEFILE RETURN NOTHING, AND THAT IS A DESYNC GUARD RATHER THAN A
// TIDY-UP. Whether either one succeeds is a fact about THIS PEER'S COMMAND
// LINE: --enable-lua-udp is what binds the socket, a headless server in this
// project has it and a graphical client joining that server does not. A library
// that could see the outcome could branch on it, and a branch there writes
// different words into storage.fk_mem on two peers of one game -- which
// Factorio CRCs. That is not hypothetical: it shipped, as `if l.tr.Send(f) ==
// StatusOK { TxFrames++ } else { QueueDrops++ }`, and a graphical client joining
// a headless server desynced on the first tick it simulated with no companion
// anywhere and no inbound datagram in the game.
//
// Discipline fixed that instance. A VOID RETURN FIXES THE CLASS: there is no
// value for a future edit to branch on, so the compiler holds the rule that a
// comment was holding. Status survives everywhere it describes a DETERMINISTIC
// refusal -- a full queue, an oversized message, a link that is not open, a
// build with no transport compiled in -- because each of those is a function of
// guest state and therefore identical on every peer.
//
// See agents/ipc.md, "The rule the cost model implies", and Link.rawSend.
type Transport interface {
	// Send puts one datagram on the wire, or does not, and DOES NOT SAY WHICH.
	// It must not retain frame: the caller reuses that buffer on the next send.
	//
	// An oversized send also fails SILENTLY on the real transport -- no error,
	// no raise, nothing arriving -- so even a status could never have meant
	// "the bytes left the machine". The size cap is enforced above this
	// interface for exactly that reason.
	Send(frame []byte)

	// Poll asks the transport for inbound datagrams and hands each to deliver
	// BEFORE RETURNING, then reports whether it delivered any.
	//
	// On the game target this is recv_udp, and the datagrams do not come back
	// through deliver at all: the engine dispatches them as
	// on_udp_packet_received events inside the call, which reach this package
	// through Event below. The callback exists for the host implementation,
	// where there is no dispatcher to route through. Both shapes deliver
	// synchronously inside Poll, which is the property the state machine
	// depends on.
	//
	// DELIVER CARRIES THE SENDER'S PORT, and that is DATA rather than policy --
	// which is why the filter built on it lives in the link and not here. See
	// Link.deliver: --enable-lua-udp binds ONE socket for the whole GAME, so
	// every mod's link sees every other mod's inbound datagrams, and the
	// sender's port is the only thing in the event that tells them apart.
	//
	// IT KEEPS ITS RETURN VALUE WHERE Send LOSES ONE, and the classification is
	// the point rather than an inconsistency: this is INBOUND, which is the
	// replicated direction, so what a poll delivered is by construction the same
	// on every peer. On the game target it is a constant false anyway -- the
	// datagrams arrive as events from inside recv_udp -- so the value the link
	// reads there is not even a fact about the world.
	Poll(deliver func(srcPort uint16, datagram []byte)) bool

	// Event routes a host-initiated event dispatch, and reports whether the
	// event was this transport's own. It is how OnEvent stays a single
	// untagged function while the event id constant stays inside the
	// wasm-only file, where the pruning scan can see it.
	Event(id, ptr uint32, deliver func(srcPort uint16, datagram []byte)) bool

	// WriteFile writes data to script-output/<name>, replacing what is there,
	// and DOES NOT SAY WHETHER IT WORKED, for Send's reason and then some. 2.1
	// documents a non-zero for_player as silently skipped from some stages, and
	// a client is not the server -- so a caller that could branch here would not
	// merely miscount: WriteBulk's early return would skip the FILE_NOTIFY,
	// which consumes the channel's seq. One peer advances the counter, the
	// other does not, and that is guest state diverging AND a permanent gap at
	// the far end.
	WriteFile(name string, data []byte)

	// BaseVersion is the running base-game version. A false means the read
	// failed, which the gate treats as below the floor: refusing to run costs a
	// session, and receiving when we should not costs the process.
	//
	// It also keeps its return, and for the third reason in this file: a
	// multiplayer game REQUIRES IDENTICAL BUILDS, so the version and the verdict
	// built on it are the same on every peer by construction. That is what makes
	// LinkStats.Enabled and LinkStats.Refusals legal counters.
	BaseVersion() (Version, bool)

	// Log puts one line in the game log, and IT IS THE ONE PER-PEER SINK IN
	// THIS SEAM.
	//
	// It returns nothing for the same reason Send does, and it may not be read
	// back for a stronger one: the game log is not CRC'd and is per-peer by
	// nature, which is exactly what makes it the right place for a fact about
	// how THIS peer was launched -- and exactly what would make a value derived
	// from it a desync. Nothing in this package logs on a hot path; the engine
	// gate logs once per load, and that is the whole of it.
	Log(msg string)
}
