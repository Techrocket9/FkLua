//! The real transport: `send_udp`, `recv_udp`, `write_file` and the version
//! read, all through the generated bindings.
//!
//! THIS FILE IS WHERE THE EVENT ID LIVES, and that is not an accident of
//! layout. `fklua mod` prunes the event table (`events` in census.json is how
//! many there are) by scanning the wasm for an `i32.const` reaching
//! `fk.subscribe`; an id it cannot prove constant ships all of them. So the
//! constant has to appear AT the `fkapi::subscribe` call site and the wrapper
//! has to inline -- which is a property, not a hope, and `internal/guest`
//! asserts it against a real cargo build. It was a live defect on this side
//! once (R6: `subscribe_filtered` lacked `#[inline]` and shipped 85 KB per
//! load).
//!
//! WHAT THAT COSTS, ATTRIBUTED, because this comment used to charge it to the
//! wrong table: the full event descriptor table is about 55 KB of Lua at the
//! 2.1.14 pin. The ~600 KB this once claimed is the MEMBER table's magnitude
//! (about a megabyte at the same pin), which is pruned by its own scan and is
//! unaffected by whether an event id inlines. R6's 85 KB is a whole-mod delta
//! measured downstream on two builds of one real guest, so it is larger than
//! the descriptor table alone -- the guest's own filter-building code is in it
//! too.
//!
//! NOTHING HERE NAMES A NUMBER. The event constant and the member ids come from
//! the generated bindings by symbol, so a version bump that renumbers them is
//! transparent -- which matters more than usual here, because the RUNTIME id of
//! `on_udp_packet_received` is a different number again (208 on 2.0.77, 212 on
//! 2.1.14) and the two namespaces are both correct.

use fkapi::{read_on_udp_packet_received, LuaStr, Value, EVENT_ON_UDP_PACKET_RECEIVED, HELPERS};

use crate::api::{Config, Profile, Status};
use crate::transport::Transport;
use crate::version::{parse_version, Version};

pub(crate) struct UdpTransport {
    port: u16,

    /// The `for_player` arguments, pre-resolved so the hot path has no branch.
    /// They differ by profile and they differ from EACH OTHER: a server sends
    /// `for_player = 0` and pumps with `recv_udp(0)`, a client omits it on
    /// both.
    send_fp: Option<u32>,
    recv_fp: Option<u32>,

    /// THE WHOLE OUTBOUND ARGUMENT, held across sends rather than rebuilt.
    ///
    /// `{"", frame}` as a tier-2 value, whose second element is the frame
    /// buffer. `send_udp` takes it by reference, so a send refills that buffer
    /// in place and allocates nothing -- which is the shape the Go transport
    /// gets from `unsafe.String` over its own slice. See
    /// [`UdpTransport::send`].
    out: Value,
}

/// The whole `{"", frame}` argument, built once so that a send can refill it.
///
/// The empty first element is the locale key that makes the second a literal --
/// see [`UdpTransport::send`].
fn frame_value(frame: &[u8]) -> Value {
    Value::Array(alloc::vec![
        Value::Str(LuaStr::new()),
        Value::Str(LuaStr::from(frame)),
    ])
}

/// The frame buffer inside a [`frame_value`], for refilling in place.
///
/// `None` rather than an `unreachable!()`: a panic in a guest traps the tick
/// for the whole game, and the caller has a rebuild to fall back on that costs
/// one allocation.
fn frame_slot(v: &mut Value) -> Option<&mut LuaStr> {
    if let Value::Array(items) = v {
        if let Some(Value::Str(s)) = items.get_mut(1) {
            return Some(s);
        }
    }
    None
}

/// The bytes of one inbound datagram, or `None` if the event was not ours.
///
/// A FREE FUNCTION AND NOT A TRANSPORT METHOD, because it runs from
/// `fk_on_event` -- a nested dispatch raised by the engine from inside
/// `recv_udp` -- where the link is deliberately unborrowed and the transport
/// inside it is therefore unreachable. See [`crate::Link::pump_begin`]. The
/// event id constant is still here, at the one place `fklua mod`'s pruning scan
/// needs it.
///
/// THIS READS THE PAYLOAD THROUGH THE GENERATED STRUCT, and until 2026-08-06
/// it could not. `get_str` was `String::from_utf8_lossy(..).into_owned()`, so a
/// frame header carrying an epoch of `0x51C0FFEE` came back as U+FFFD sequences
/// and the wrong LENGTH -- while the Go generator's byte-exact `string(b)` read
/// the same wire correctly. What stood here instead was a scan that located the
/// (pointer, length) pair by asking the generated ENCODER where it puts one, so
/// that this file could read the bytes itself. The fix went where the defect
/// was: string fields are `LuaStr` now, which is bytes, and the scan is gone.
/// See `agents/abi.md`, "A Lua string is bytes, and Rust's String is not".
///
/// The payload arrives COPIED, one `Vec` per datagram, which is exactly what
/// the Go half pays for `string(b)` -- the two mirrors allocate alike, and
/// allocation count is guest state. Reading the host's buffer without a copy
/// would be a win for both languages and belongs in both generators, not here.
///
/// # Safety
///
/// `ptr` must be the pointer `fk_on_event` was handed for THIS event id.
/// `source_port` IS FORWARDED RATHER THAN FILTERED ON HERE. `--enable-lua-udp`
/// binds one socket for the whole game, so this event fires in EVERY mod for
/// EVERY mod's datagrams; the sender's port is what tells them apart, and the
/// decision belongs to the link, which owns the peer port and the counters.
/// See [`crate::Link::deliver_datagram`].
pub(crate) fn inbound(id: u32, ptr: u32, deliver: &mut dyn FnMut(u16, &[u8])) -> bool {
    if id != EVENT_ON_UDP_PACKET_RECEIVED {
        return false;
    }
    let ev = read_on_udp_packet_received(ptr);
    if ev.payload.is_empty() {
        return true;
    }
    deliver(ev.source_port, ev.payload.as_bytes());
    true
}

pub(crate) fn new_transport(cfg: Config) -> (Option<alloc::boxed::Box<dyn Transport>>, Status) {
    let mut t = UdpTransport {
        port: cfg.port,
        send_fp: None,
        recv_fp: None,
        out: frame_value(&[]),
    };
    if cfg.for_player >= 0 {
        t.send_fp = Some(cfg.for_player as u32);
    }
    // The RECEIVE side is not the send side's mirror. Profile::Client pumps
    // with a bare `recv_udp()`, which is what every graphical-client mod in the
    // ecosystem does; Profile::Server pumps with `recv_udp(0)`, which is the
    // arm the probe verified working on 2.1.14 -- and the arm that kills
    // 2.0.77, which is why pump asks the version gate first.
    if cfg.profile != Profile::Client {
        t.recv_fp = Some(if cfg.for_player >= 0 {
            cfg.for_player as u32
        } else {
            0
        });
    }
    // The status is deliberately not propagated. The only way this fails is a
    // guest that exports no `fk_on_event`, which is an authoring mistake rather
    // than a runtime condition -- and refusing `open` over it would take the
    // OUTBOUND half down too, which works on every version and is the direction
    // that is free. `fk_mod.lua` logs the refusal, and `internal/guest`'s
    // end-to-end test is what actually catches it.
    fkapi::subscribe(EVENT_ON_UDP_PACKET_RECEIVED);
    (Some(alloc::boxed::Box::new(t)), Status::Ok)
}

impl Transport for UdpTransport {
    /// Puts one frame on the wire as `{"", frame}`.
    ///
    /// A BARE STRING IS A LOCALE KEY. The probe measured all four
    /// LocalisedString forms carrying binary byte-exact, but the bare form was
    /// measured on a headless server with nobody to localise FOR; `{"", s}` is
    /// the documented literal-concat form and is literal BY CONSTRUCTION, so it
    /// costs one extra `dyn_alloc` of 32 bytes of arena and buys not having to
    /// think about it again on a client with a locale loaded.
    ///
    /// # Nothing is allocated per send, which took a change in the generator
    ///
    /// This used to hold a `String`, refill it, and then CLONE it into a
    /// `Value::Str` the call took by value -- one heap copy of the frame per
    /// send, dropped inside the call, ordinary garbage under
    /// `--features fk/fkgc` and a leak proportional to bytes sent under the
    /// default bump arena. Two generator changes retire it: `Value::Str` holds
    /// a `LuaStr`, which is bytes, and every tier-2 ARGUMENT is taken by
    /// reference, so the whole `{"", frame}` value lives in the transport and a
    /// send refills its buffer in place. That is what the Go half has always
    /// had from `unsafe.String` over its own slice.
    ///
    /// What is still allocated per send is the tier-2 WIRE -- two 16-byte
    /// element blocks from `fk_alloc` -- which is the ABI's, not this
    /// transport's, and which Go's marshalling arena reclaims at the call
    /// bracket where `guest/rust/fk`'s bump allocator does not. That gap is
    /// recorded there.
    ///
    /// No `from_utf8_unchecked` anywhere any more. A frame is binary by
    /// construction, and building a `String` over bytes that are not UTF-8 was
    /// library UB in the shipped mirror -- true even though nothing read the
    /// value as text, because the invariant belongs to the type.
    /// It returns NOTHING, and on this arm that is the whole point: this is the
    /// one implementation that runs inside a lockstep game, so this is where a
    /// return value would become a word in `storage.fk_mem` that differs
    /// between a server started with `--enable-lua-udp` and a client that was
    /// not. There is no value to return and therefore no branch to write. See
    /// the seam's own comment in `transport.rs`.
    fn send(&mut self, frame: &[u8]) {
        if frame.is_empty() {
            return;
        }
        match frame_slot(&mut self.out) {
            // Reuses the buffer's capacity across sends. Deterministic across
            // peers: identical builds fed identical frames grow it identically.
            Some(s) => s.set(frame),
            None => self.out = frame_value(frame),
        }
        // The error is DROPPED HERE rather than carried one frame further. Its
        // value is a fact about how this peer was launched, which is exactly
        // the class of fact guest state may not hold.
        let _ = HELPERS.send_udp(self.port, &self.out, self.send_fp);
    }

    /// Calls `recv_udp`, and the datagrams do NOT come back through `deliver`:
    /// the engine dispatches them as `on_udp_packet_received` events inside
    /// this call, which reach the link through [`Transport::event`]. It always
    /// reports false, so the pump's drain loop runs exactly once -- which is
    /// the measured shape, one call draining a 20-packet backlog within the
    /// tick, in order, complete.
    fn poll(&mut self, _deliver: &mut dyn FnMut(u16, &[u8])) -> bool {
        let _ = HELPERS.recv_udp(self.recv_fp);
        false
    }

    /// A snapshot file, whose contents are binary for the same reason a frame
    /// is. One allocation per call and no buffer held across them: this runs
    /// when a guest asks for a dump, not every tick.
    ///
    /// It returns NOTHING, for [`Transport::send`]'s reason. See
    /// `transport.rs`.
    fn write_file(&mut self, name: &str, data: &[u8]) {
        let v = Value::Str(LuaStr::from(data));
        let append = false;
        let _ = HELPERS.write_file(name, &v, Some(append), self.send_fp);
    }

    /// Reads `helpers.game_version`, ONCE, and it is the cheapest bound surface
    /// that answers the question: one host call, one short string, no container
    /// -- against `script.active_mods`, which materialises every mod's name and
    /// version into the guest heap to find one of them.
    ///
    /// It is also available where this has to run. `open` is called from
    /// `_initialize`, i.e. from control.lua's main chunk, where `game` does not
    /// exist yet; `helpers` does, and `game_version` carries no stage
    /// restriction.
    ///
    /// Deterministic on every peer by construction: a multiplayer game requires
    /// identical builds.
    fn base_version(&mut self) -> Option<Version> {
        match HELPERS.game_version() {
            // as_str is the CHECKED conversion off the byte string, and a
            // version that is not UTF-8 is not a version -- so None, which is
            // the same answer an unparseable one gets.
            Ok(s) => s.as_str().and_then(parse_version),
            Err(_) => None,
        }
    }

    /// Goes to the game log, which is not CRC'd -- the only sanctioned sink for
    /// a per-peer fact. See the seam's comment in `transport.rs`.
    fn log(&mut self, msg: &str) {
        fk::log(msg);
    }
}
