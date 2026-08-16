//! One channel's state, and the value a guest names it by.

use alloc::vec::Vec;

use crate::api::{Corr, Priority, Status};
use crate::link::{GapFn, MessageFn, ReplyFn, RequestFn, ResyncFn};
use crate::wire::{self, Flags, Type};

/// One channel's state: its two seq counters, its handlers, and the
/// at-most-one reassembly it may have open.
pub(crate) struct ChannelState {
    pub id: u16,
    pub pri: Priority,

    pub tx_seq: u32,
    pub rx_last: u32,

    /// No separate "gap" flag beside it: a flag nothing reads is bytes in the
    /// save. What a gap MEANS to this channel is entirely "a RESYNC is
    /// outstanding", which is what gates sending another one.
    pub resync_sent: bool,

    pub on_message: Option<MessageFn>,
    pub on_request: Option<RequestFn>,
    pub on_resync: Option<ResyncFn>,
    pub on_gap: Option<GapFn>,

    pub rasm_active: bool,
    pub rasm_corr: u32,
    pub rasm_deadline: u32,
    pub rasm_ty: Type,
    pub rasm_flags: Flags,
    pub rasm_nfrag: u8,
    pub rasm_got: u8,
    pub rasm_seen: [bool; wire::MAX_FRAGMENTS as usize],
    pub rasm_part: [Vec<u8>; wire::MAX_FRAGMENTS as usize],
}

impl ChannelState {
    pub(crate) fn new(id: u16, pri: Priority) -> ChannelState {
        const EMPTY: Vec<u8> = Vec::new();
        ChannelState {
            id,
            pri,
            tx_seq: 0,
            rx_last: 0,
            resync_sent: false,
            on_message: None,
            on_request: None,
            on_resync: None,
            on_gap: None,
            rasm_active: false,
            rasm_corr: 0,
            rasm_deadline: 0,
            rasm_ty: Type(0),
            rasm_flags: Flags(0),
            rasm_nfrag: 0,
            rasm_got: 0,
            rasm_seen: [false; wire::MAX_FRAGMENTS as usize],
            rasm_part: [EMPTY; wire::MAX_FRAGMENTS as usize],
        }
    }

    /// Per CHANNEL and per DIRECTION, counting FRAMES rather than messages --
    /// so a lost fragment is a detectable gap instead of a silently short
    /// message.
    ///
    /// Channel 0 is the protocol's own and never carries one: a lost heartbeat
    /// is normal and must not read as a gap in application state.
    pub(crate) fn next_seq(&mut self) -> u32 {
        if self.id == 0 {
            return 0;
        }
        self.tx_seq = self.tx_seq.wrapping_add(1);
        self.tx_seq
    }

    /// Drops whatever reassembly is open. The part buffers keep their capacity,
    /// because the next message on this channel is the same size as the last
    /// one far more often than not.
    pub(crate) fn abandon(&mut self) {
        self.rasm_active = false;
        self.rasm_got = 0;
        self.rasm_corr = 0;
        self.rasm_nfrag = 0;
        for i in 0..wire::MAX_FRAGMENTS as usize {
            self.rasm_seen[i] = false;
            self.rasm_part[i].clear();
        }
    }
}

/// Names one channel of the guest's link.
///
/// IT IS THE ID AND NOTHING ELSE, and [`Channel::new`] is `const`, so a guest
/// writes
///
/// ```ignore
/// const STATE: fkipc::Channel = fkipc::Channel::new(1);
/// ```
///
/// at module scope and calls [`Channel::open`] from `_initialize`. The Go half
/// spells the pair as one function, `fkipc.Chan(id, pri)`, because Go runs
/// package-level `var` initialisers BEFORE `init()` -- so a channel really can
/// be named before `Open` has run there, and its singleton is non-nil from the
/// first line of package initialisation for exactly that reason. Rust has no
/// such phase in a cdylib reactor: a `static` takes a `const` initialiser, and
/// everything else happens inside `_initialize` in an order the guest wrote.
/// The hazard is absent rather than worked around.
///
/// # Inside a handler, use the [`Link`](crate::Link) you were handed
///
/// These methods reach the crate's singleton, which is exactly what a guest
/// wants from `fk_on_tick`. They cannot be used from INSIDE a handler, because
/// the singleton is already borrowed there -- so they answer
/// [`Status::NotOpen`] rather than aliasing it, and the handler's own `&mut
/// Link` is the route (`l.snapshot(STATE.id(), ...)`).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Channel {
    id: u16,
}

impl Channel {
    pub const fn new(id: u16) -> Channel {
        Channel { id }
    }

    /// The channel's wire number.
    pub const fn id(self) -> u16 {
        self.id
    }

    /// Registers the channel and sets its priority. Calling it twice is the
    /// same channel with a new priority.
    pub fn open(self, pri: Priority) -> Status {
        crate::with_link(|l| {
            l.open_channel(self.id, pri);
            Status::Ok
        })
    }

    pub fn send(self, payload: &[u8]) -> Status {
        crate::with_link(|l| l.send(self.id, payload))
    }

    pub fn snapshot(self, payload: &[u8]) -> Status {
        crate::with_link(|l| l.snapshot(self.id, payload))
    }

    pub fn request(self, payload: &[u8], on_reply: Option<ReplyFn>) -> Result<Corr, Status> {
        crate::with_link_r(
            |l| l.request(self.id, payload, on_reply),
            Err(Status::NotOpen),
        )
    }

    // The inbound handlers.
    //
    // THE PAYLOAD HANDED TO ANY OF THESE BORROWS the library's own buffer and
    // the borrow ends with the handler. Copy what you keep. Same rule as a
    // transient handle and the host's string scratch region, for the same
    // reason -- except that here it is the compiler saying so rather than a
    // comment.

    pub fn on_message(self, h: MessageFn) -> Status {
        crate::with_link(|l| {
            l.set_on_message(self.id, h);
            Status::Ok
        })
    }

    pub fn on_request(self, h: RequestFn) -> Status {
        crate::with_link(|l| {
            l.set_on_request(self.id, h);
            Status::Ok
        })
    }

    /// "Send me a snapshot". A channel with no handler simply does not answer,
    /// which is right for a channel that carries no replayable state.
    pub fn on_resync(self, h: ResyncFn) -> Status {
        crate::with_link(|l| {
            l.set_on_resync(self.id, h);
            Status::Ok
        })
    }

    /// Reports how many frames were missed. The library has already sent the
    /// RESYNC by the time this runs -- the handler is for the application's own
    /// accounting, not for it to decide.
    pub fn on_gap(self, h: GapFn) -> Status {
        crate::with_link(|l| {
            l.set_on_gap(self.id, h);
            Status::Ok
        })
    }

    pub fn write_bulk(self, name: &str, data: &[u8]) -> Status {
        crate::with_link(|l| l.write_bulk(self.id, name, data))
    }

    pub fn notify_file(self, name: &str) -> Status {
        crate::with_link(|l| l.notify_file(self.id, name))
    }
}
