//! The transport seam.
//!
//! THE REFERENCE IMPLEMENTATION OF THE GUEST STATE MACHINE IS THIS CRATE,
//! COMPILED FOR THE HOST. That is what this trait is for. The `fkapi`
//! implementation lives behind `#[cfg(target_family = "wasm")]` and the test
//! suite's is an in-memory link with an injectable fault model, so the protocol
//! tests exercise the code that ships rather than a second implementation
//! somebody has to keep in sync. The precedent is the Go half's own
//! `//go:build tinygo.wasm` split, and `fkgc`'s before that.
//!
//! It is deliberately small. Anything richer would start encoding policy --
//! retries, budgets, the engine gate -- into the thing the tests replace, and
//! then the tests would not be testing the policy. A log SINK is not policy:
//! the crate decides what to say and when, and `log` is only where the bytes
//! go.
//!
//! FOUR METHODS WHERE THE GO HALF HAS FIVE, and the missing one is `Event`.
//! Go routes a host-initiated dispatch back through the transport; here that
//! dispatch must NOT go through the link at all. See
//! [`crate::on_event`] and the ordering note on [`crate::pump`] -- the event
//! arrives on a nested call stack, from inside `recv_udp`, and the link cannot
//! be borrowed by the pump when it does. So the event id constant lives in a
//! wasm-only free function instead of a wasm-only method, which is the same
//! place as far as the pruning scan is concerned.
//!
//! `send`, `write_file` AND `log` RETURN NOTHING, AND THAT IS A DESYNC GUARD
//! RATHER THAN A TIDY-UP. Whether either one succeeds is a fact about THIS PEER'S
//! COMMAND LINE: `--enable-lua-udp` is what binds the socket, a headless server
//! in this project has it and a graphical client joining that server does not.
//! A library that could see the outcome could branch on it, and a branch there
//! writes different words into `storage.fk_mem` on two peers of one game --
//! which Factorio CRCs. That is not hypothetical: it shipped, as
//! `if tr.send(f) == Status::Ok { tx_frames += 1 } else { queue_drops += 1 }`,
//! and a graphical client joining a headless server desynced on the first tick
//! it simulated with no companion anywhere and no inbound datagram in the game.
//!
//! Discipline fixed that instance. A UNIT RETURN FIXES THE CLASS: there is no
//! value for a future edit to branch on, so the compiler holds the rule that a
//! comment was holding. `Status` survives everywhere it describes a
//! DETERMINISTIC refusal -- a full queue, an oversized message, a link that is
//! not open, a transport that is out of the link during a poll -- because each
//! of those is a function of guest state and therefore identical on every peer.
//!
//! See `agents/ipc.md`, "The rule the cost model implies", and `Link`'s own
//! `raw_send_slice`.

use crate::version::Version;

pub trait Transport {
    /// Puts one datagram on the wire, or does not, and DOES NOT SAY WHICH. It
    /// must not retain `frame`: the caller reuses that buffer on the next send.
    ///
    /// An oversized send also fails SILENTLY on the real transport -- no error,
    /// no raise, nothing arriving -- so even a status could never have meant
    /// "the bytes left the machine". The size cap is enforced above this trait
    /// for exactly that reason.
    fn send(&mut self, frame: &[u8]);

    /// Asks the transport for inbound datagrams and hands each to `deliver`
    /// BEFORE RETURNING, then reports whether it delivered any.
    ///
    /// On the game target this is `recv_udp`, and the datagrams do not come
    /// back through `deliver` at all: the engine dispatches them as
    /// `on_udp_packet_received` events inside the call, which re-enter the
    /// guest through its own `fk_on_event` export and reach the link through
    /// [`crate::on_event`]. The callback exists for the host implementation,
    /// where there is no dispatcher to route through. Both shapes deliver
    /// synchronously inside `poll`, which is the property the state machine
    /// depends on.
    ///
    /// DELIVER CARRIES THE SENDER'S PORT, and that is DATA rather than policy
    /// -- which is why the filter built on it lives in the link and not here.
    /// See [`crate::Link::deliver_datagram`]: `--enable-lua-udp` binds ONE
    /// socket for the whole GAME, so every mod's link sees every other mod's
    /// inbound datagrams, and the sender's port is the only thing in the event
    /// that tells them apart.
    ///
    /// IT KEEPS ITS RETURN VALUE WHERE `send` LOSES ONE, and the classification
    /// is the point rather than an inconsistency: this is INBOUND, which is the
    /// replicated direction, so what a poll delivered is by construction the
    /// same on every peer. On the game target it is a constant `false` anyway
    /// -- the datagrams arrive as events from inside `recv_udp` -- so the value
    /// the link reads there is not even a fact about the world.
    fn poll(&mut self, deliver: &mut dyn FnMut(u16, &[u8])) -> bool;

    /// Writes `data` to `script-output/<name>`, replacing what is there, and
    /// DOES NOT SAY WHETHER IT WORKED, for [`Transport::send`]'s reason and
    /// then some. 2.1 documents a non-zero `for_player` as silently skipped
    /// from some stages, and a client is not the server -- so a caller that
    /// could branch here would not merely miscount: `write_bulk`'s early return
    /// would skip the `FILE_NOTIFY`, which consumes the channel's seq. One peer
    /// advances the counter, the other does not, and that is guest state
    /// diverging AND a permanent gap at the far end.
    fn write_file(&mut self, name: &str, data: &[u8]);

    /// The running base-game version. `None` means the read failed, which the
    /// gate treats as below the floor: refusing to run costs a session, and
    /// receiving when we should not costs the process.
    ///
    /// It also keeps its return, and for the third reason in this file: a
    /// multiplayer game REQUIRES IDENTICAL BUILDS, so the version and the
    /// verdict built on it are the same on every peer by construction. That is
    /// what makes `Stats::enabled` and `Stats::refusals` legal counters.
    fn base_version(&mut self) -> Option<Version>;

    /// Puts one line in the game log, and IT IS THE ONE PER-PEER SINK IN THIS
    /// SEAM.
    ///
    /// It returns nothing for the same reason `send` does, and it may not be
    /// read back for a stronger one: the game log is not CRC'd and is per-peer
    /// by nature, which is exactly what makes it the right place for a fact
    /// about how THIS peer was launched -- and exactly what would make a value
    /// derived from it a desync. Nothing in this crate logs on a hot path; the
    /// engine gate logs once per load, and that is the whole of it.
    fn log(&mut self, msg: &str);
}
