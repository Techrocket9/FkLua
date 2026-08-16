//! THE CONFORMANCE HARNESS: the shipping guest state machine, on the host,
//! over an in-memory link with an injectable fault model and a fake tick.
//!
//! The reference implementation of the guest half IS `fkipc` compiled for the
//! host -- that is what its transport seam is for, and it is what makes this
//! layer worth having rather than a second thing to keep in sync. Neither wasm
//! nor Factorio nor a toolchain is involved.
//!
//! What it cannot say is anything about the real transport: that a datagram
//! really carries NUL bytes, that `recv_udp` really drains a backlog in one
//! tick, that an oversized send really fails silently. Those are the probe's,
//! and the constants they produced are what this suite runs against.
//!
//! Everything is `thread_local!` rather than a plain `static`, because
//! `cargo test` runs each test on its own thread and handlers are `fn` pointers
//! with nothing to capture. One test's bus is therefore invisible to another's,
//! which is what lets the whole file run in parallel.

#![allow(dead_code)]

use std::cell::RefCell;
use std::collections::HashMap;

use fkipc::wire::{self, Flags, Header, Type};
use fkipc::{Config, Link, Transport, Version, MIN_ENGINE_VERSION};

/// The wire. A fault function sees every frame and returns what actually lands
/// -- none of it to drop, two copies to duplicate, a shortened copy to truncate
/// -- which is enough to express every failure this protocol claims to survive
/// without any of them being a special case in the harness.
pub struct Bus {
    pub to_guest: Vec<Vec<u8>>,
    /// Everything the guest ever offered, faults not applied, so a test can
    /// assert on what it TRIED to send.
    pub from_guest: Vec<Vec<u8>>,
    pub files: HashMap<String, Vec<u8>>,
    /// Every line the engine gate logged. Host-side, unreachable from library
    /// code, and per-peer by nature: the game log is not CRC'd, which is what
    /// makes it the sanctioned sink for a fact about how this peer was launched
    /// -- and exactly why nothing in the crate may read it back.
    pub logs: Vec<String>,
    pub ver: Option<Version>,
    /// Applied to frames the guest sends.
    pub out_fault: Option<fn(&[u8]) -> bool>,
    pub send_fails: bool,
    /// The port the peer's datagrams appear to come FROM, which the link tests
    /// against its configured `Config::port`. It is [`GUEST_PEER_PORT`] here:
    /// this harness is one game running one ipc mod. The cross-mod case is
    /// `a_frame_from_another_mods_companion_is_refused` in tests/link.rs.
    pub src_port: u16,
}

/// The `Config::port` every harness uses, and therefore the port the peer's
/// datagrams must appear to come from.
pub const GUEST_PEER_PORT: u16 = 29434;

impl Default for Bus {
    fn default() -> Bus {
        Bus {
            to_guest: Vec::new(),
            from_guest: Vec::new(),
            files: HashMap::new(),
            logs: Vec::new(),
            ver: Some(MIN_ENGINE_VERSION),
            out_fault: None,
            send_fails: false,
            src_port: GUEST_PEER_PORT,
        }
    }
}

thread_local! {
    static BUS: RefCell<Bus> = RefCell::new(Bus::default());
}

pub fn bus<R>(f: impl FnOnce(&mut Bus) -> R) -> R {
    BUS.with(|b| f(&mut b.borrow_mut()))
}

/// A ZST, so the transport can be handed to `attach` while the test keeps
/// reaching the same wire.
pub struct TestTransport;

impl Transport for TestTransport {
    /// RECORDS but does not REPORT, which is the whole shape of a test double
    /// under a void seam: the bus is host-side and no library code can reach
    /// it, so a test may assert on it from outside the state machine without
    /// any of it becoming guest state. `send_fails` still models a peer whose
    /// socket is not bound -- the frame simply does not land -- and the point
    /// of `a_failed_send_is_invisible_to_guest_state` is that the link cannot
    /// tell.
    fn send(&mut self, frame: &[u8]) {
        bus(|b| {
            if b.send_fails {
                return;
            }
            b.from_guest.push(frame.to_vec());
        })
    }

    /// Drains everything queued and reports false, which is the measured shape:
    /// twenty packets blasted in 0.34 ms all arrived within one tick, in order,
    /// complete, from one `recv_udp`.
    fn poll(&mut self, deliver: &mut dyn FnMut(u16, &[u8])) -> bool {
        let (batch, src) = bus(|b| (core::mem::take(&mut b.to_guest), b.src_port));
        for dg in &batch {
            deliver(src, dg);
        }
        false
    }

    fn write_file(&mut self, name: &str, data: &[u8]) {
        // Fails with the send, because both are the same fact: a peer that was
        // not started with `--enable-lua-udp`, or a stage where a non-zero
        // `for_player` is silently skipped, is a peer where the whole outbound
        // surface answers differently. The library is not told either way.
        if bus(|b| b.send_fails) {
            return;
        }
        bus(|b| b.files.insert(name.to_string(), data.to_vec()));
    }

    fn base_version(&mut self) -> Option<Version> {
        bus(|b| b.ver)
    }

    fn log(&mut self, msg: &str) {
        bus(|b| b.logs.push(msg.to_string()));
    }
}

pub struct Harness {
    pub g: Link,
    pub tick: u32,
    pub cfg: Config,
}

pub struct Opts {
    pub max_frame: u16,
    pub base_version: Option<Version>,
    pub profile: fkipc::Profile,
    /// The guest's own identity token. Left empty ALONGSIDE an empty
    /// `expect_peer` it becomes the harness's historical `"guest"`, so every
    /// test written before identities existed is unmoved; left empty beside a
    /// non-empty `expect_peer` it stays empty, so what a test then observes is
    /// the LIBRARY's own "one token names the contract" defaulting rather than
    /// the harness's idea of it.
    pub name: &'static str,
    /// What the guest requires of its companion; empty is no check.
    pub expect_peer: &'static str,
}

impl Default for Opts {
    fn default() -> Self {
        Opts {
            max_frame: 0,
            base_version: Some(MIN_ENGINE_VERSION),
            profile: fkipc::Profile::Server,
            name: "",
            expect_peer: "",
        }
    }
}

pub fn new_harness(o: Opts) -> Harness {
    bus(|b| {
        *b = Bus::default();
        b.ver = o.base_version;
    });
    let cfg = Config {
        port: GUEST_PEER_PORT,
        profile: o.profile,
        for_player: 0,
        max_frame: o.max_frame,
        name: if o.name.is_empty() && o.expect_peer.is_empty() {
            "guest"
        } else {
            o.name
        },
        expect_peer: o.expect_peer,
    };
    // attach SUCCEEDS below the engine floor -- a disabled link is fully
    // configured and the verdict is on the link (`Link::enabled`), which is
    // what the sub-floor test reads.
    let g = fkipc::attach(cfg, Box::new(TestTransport)).expect("attach");
    Harness { g, tick: 0, cfg }
}

impl Harness {
    pub fn step(&mut self, n: u32) {
        for _ in 0..n {
            self.tick += 1;
            self.g.pump(self.tick);
        }
    }

    /// Models what a LOAD does to guest memory: `_initialize` rebuilds the
    /// crate state and the saved bytes then replace it, which is why the boot
    /// counter is handed over separately.
    pub fn new_guest(&mut self) {
        self.g = fkipc::attach(self.cfg, Box::new(TestTransport)).expect("attach");
    }

    /// Brings the session all the way up by playing the peer: read the HELLO,
    /// answer it with a token.
    pub fn up(&mut self, token: u32) {
        self.step(1);
        let corr = self
            .last_of(Type::HELLO)
            .expect("the first pump sent no HELLO")
            .0
            .corr;
        ack(token, corr);
        self.step(1);
        assert!(self.g.stats().up, "no session after the HELLO_ACK");
    }

    /// Every frame the guest has offered, decoded.
    pub fn frames(&self) -> Vec<(Header, Vec<u8>)> {
        bus(|b| {
            b.from_guest
                .iter()
                .filter_map(|f| wire::decode(f).ok().map(|(h, p)| (h, p.to_vec())))
                .collect()
        })
    }

    pub fn count(&self, ty: Type) -> usize {
        self.frames().iter().filter(|(h, _)| h.ty == ty).count()
    }

    pub fn last_of(&self, ty: Type) -> Option<(Header, Vec<u8>)> {
        self.frames().into_iter().filter(|(h, _)| h.ty == ty).last()
    }

    pub fn sent(&self) -> usize {
        bus(|b| b.from_guest.len())
    }

    pub fn clear_sent(&self) {
        bus(|b| b.from_guest.clear());
    }
}

/// Builds a frame as if the peer had sent it, so a test can put a header on the
/// wire that neither implementation would produce.
pub fn craft(h: Header, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    wire::append_frame(&mut out, h, payload).expect("craft");
    out
}

/// Puts a frame in front of the guest.
pub fn inject(frame: Vec<u8>) {
    bus(|b| b.to_guest.push(frame));
}

/// The peer's answer to a HELLO: a token the guest has never seen, matched on
/// the HELLO's own corr.
pub fn ack(token: u32, corr: u32) {
    ack_named(token, corr, "peer")
}

/// [`ack`] with the peer's IDENTITY TOKEN spelled out, which is what
/// `Config::expect_peer` is tested against.
pub fn ack_named(token: u32, corr: u32, name: &str) {
    let mut body = Vec::new();
    wire::control::append_hello(
        &mut body,
        &wire::Hello {
            proto_min: wire::VERSION,
            proto_max: wire::VERSION,
            max_frame: wire::DEFAULT_MAX_FRAME,
            max_fragments: wire::MAX_FRAGMENTS as u16,
            boot: 0,
            tick: 0,
            profile: wire::Profile::SERVER,
            name: name.to_string(),
        },
    )
    .unwrap();
    inject(craft(
        Header {
            ty: Type::HELLO_ACK,
            epoch: token,
            corr,
            ..Default::default()
        },
        &body,
    ));
}

pub fn msg(channel: u16, epoch: u32, seq: u32, payload: &[u8]) -> Vec<u8> {
    craft(
        Header {
            ty: Type::MSG,
            channel,
            epoch,
            seq,
            ..Default::default()
        },
        payload,
    )
}

pub fn snap(channel: u16, epoch: u32, seq: u32, payload: &[u8]) -> Vec<u8> {
    craft(
        Header {
            ty: Type::MSG,
            flags: Flags::SNAPSHOT,
            channel,
            epoch,
            seq,
            ..Default::default()
        },
        payload,
    )
}

pub fn req(channel: u16, epoch: u32, seq: u32, corr: u32, retry: bool, payload: &[u8]) -> Vec<u8> {
    craft(
        Header {
            ty: Type::REQ,
            flags: if retry { Flags::RETRY } else { Flags::NONE },
            channel,
            epoch,
            seq,
            corr,
            ..Default::default()
        },
        payload,
    )
}

pub fn resp(channel: u16, epoch: u32, seq: u32, corr: u32, payload: &[u8]) -> Vec<u8> {
    craft(
        Header {
            ty: Type::RESP,
            channel,
            epoch,
            seq,
            corr,
            ..Default::default()
        },
        payload,
    )
}
