//! The engine gate.
//!
//! THIS IS THE ONE PLACE WHERE GETTING IT WRONG KILLS THE PROCESS, so it is the
//! one place that refuses by default. On Factorio 2.0.77 a headless server that
//! calls `recv_udp` with a packet queued aborts in C++ at `TickClosure.cpp:91`
//! -- not a Lua error, so no pcall anywhere on the bridge survives it, and the
//! mod's own logging stops mid-tick. The probe reproduced it five times in five
//! runs, deterministic to the map tick on a fresh map.
//!
//! The crash needs BOTH halves: `recv_udp` on an empty socket is safe (1,500
//! calls, fine) and a socket nobody reads is safe (20 packets piled up, fine).
//! Only the pump and a queued packet together do it. It is also specifically
//! reading FOR THE SERVER -- `recv_udp(0)` and bare `recv_udp()` both crash,
//! `recv_udp(1)` for a player who does not exist is a safe no-op that delivers
//! nothing.
//!
//! BELOW THE FLOOR THE LIBRARY IS INERT, WHICH IS WIDER THAN THE CRASH. It used
//! to run SEND-ONLY down there, on the reasoning that outbound is free and a
//! telemetry guest could still be useful with nobody listening. That is true of
//! the datagrams and false of the PROTOCOL: a session is established by a
//! HELLO_ACK, an ACK arrives inbound, and inbound is the direction that is shut
//! off -- so a send-only link HELLOs once a second forever, never comes up, and
//! every send it is handed is refused for want of a session. What it produces
//! is a steady trickle of frames no peer can answer and a mod whose author is
//! told nothing. So the gate is not a tuning knob and it is not a partial mode
//! either: below the floor `open` says so, once, and nothing else happens.
//!
//! IT GATES ON THE ENGINE, NOT ON THE API PIN, and the two are separate axes.
//! The pin is a build-time fact -- which `runtime-api.json` the bindings came
//! from -- and every member this crate touches (`send_udp`, `recv_udp`,
//! `write_file`, `game_version`, `on_udp_packet_received`) exists in the 2.0.77
//! description, which shipped with 2.0.59. The engine is what
//! `helpers.game_version` reports at RUN TIME. So a mod built at the
//! general-availability pin gets the whole crate on a 2.1.14 engine with no
//! rebuild, no repin and no second build of the guest.

/// A Factorio version triple.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct Version {
    pub major: u16,
    pub minor: u16,
    pub patch: u16,
}

impl Version {
    pub const ZERO: Version = Version {
        major: 0,
        minor: 0,
        patch: 0,
    };

    pub const fn new(major: u16, minor: u16, patch: u16) -> Version {
        Version {
            major,
            minor,
            patch,
        }
    }

    /// A plain lexicographic compare over the triple.
    pub fn less(self, o: Version) -> bool {
        if self.major != o.major {
            return self.major < o.major;
        }
        if self.minor != o.minor {
            return self.minor < o.minor;
        }
        self.patch < o.patch
    }

    /// A version nothing has filled in, which the gate treats as "below the
    /// floor". A failed read must never open the link.
    pub fn zero(self) -> bool {
        self == Version::ZERO
    }
}

/// The lowest base-game version this crate will run on at all. Below it `open`
/// refuses, `pump` does nothing, and every API call answers `Status::Disabled`.
///
/// IT WAS CALLED `BASE_FLOOR_RECV` while the floor gated only the receive path,
/// and the rename is the whole of what the hard-disable changed about its
/// meaning. "Recv" named a mechanism; this names the axis -- the ENGINE, which
/// is not the API pin -- and the scope, which is now the crate rather than one
/// call.
///
/// THE VALUE IS WHAT WAS MEASURED, NOT WHERE THE FIX LANDED. The crash is
/// confirmed present at 2.0.77, and was reported upstream at 2.1.9; inbound is
/// confirmed WORKING at 2.1.14 -- the arm that kills 2.0.77 survived 25 s and
/// delivered 467 events, and a full handshake ran over 61. The versions between
/// are unverified, so the floor is the version that was actually observed to
/// work rather than the version somebody's changelog says was fixed. Lowering
/// it wants a probe run at the version being lowered to, not an argument.
///
/// A `const` where the Go half has a `var`, and nothing is lost: the Go tests
/// never reassign it either -- they vary what the TRANSPORT reports, which is
/// the input the gate actually reads.
pub const MIN_ENGINE_VERSION: Version = Version::new(2, 1, 14);

/// Reads `"2.1.14"`, and anything trailing it.
///
/// Hand-rolled rather than `split` + `parse`: both pull real weight into a
/// guest, and the scaffold's own comment already names the strconv-and-plus
/// shape as the trap a downstream mod measured as its entire guest heap. This
/// allocates nothing.
pub fn parse_version(s: &str) -> Option<Version> {
    let b = s.as_bytes();
    let mut v = Version::ZERO;
    let mut i = 0usize;
    for f in 0..3 {
        let start = i;
        let mut n: u32 = 0;
        while i < b.len() && b[i] >= b'0' && b[i] <= b'9' {
            n = n * 10 + (b[i] - b'0') as u32;
            if n > 65535 {
                return None;
            }
            i += 1;
        }
        if i == start {
            return None;
        }
        match f {
            0 => v.major = n as u16,
            1 => v.minor = n as u16,
            _ => v.patch = n as u16,
        }
        if f < 2 {
            if i >= b.len() || b[i] != b'.' {
                return None;
            }
            i += 1;
        }
    }
    Some(v)
}

/// Appends `"2.1.14"` to `s`, without `format!`.
///
/// Hand-rolled for `parse_version`'s reason: this is a `no_std` guest crate and
/// the formatting machinery is real weight for what is, in total, two strings
/// logged at most once per load.
pub(crate) fn append_version(s: &mut alloc::string::String, v: Version) {
    append_u16(s, v.major);
    s.push('.');
    append_u16(s, v.minor);
    s.push('.');
    append_u16(s, v.patch);
}

fn append_u16(s: &mut alloc::string::String, mut v: u16) {
    if v == 0 {
        s.push('0');
        return;
    }
    let mut b = [0u8; 5];
    let mut i = b.len();
    while v > 0 {
        i -= 1;
        b[i] = b'0' + (v % 10) as u8;
        v /= 10;
    }
    for &c in &b[i..] {
        s.push(c as char);
    }
}

/// The one line the engine gate logs.
///
/// IT IS A LOG LINE AND NOT A COUNTER, deliberately, and it is the one thing
/// this crate does that is per-peer on purpose. Whether an engine is below the
/// floor is identical on every peer -- Factorio refuses a multiplayer
/// connection between two different builds -- so this COULD be guest state.
/// What must not be guest state is anything about whether the log call itself
/// worked, and the game log is not CRC'd, which is exactly why `fk::log` is
/// this repo's only sanctioned sink for a per-peer fact. See the join-safety
/// contract in `lib.rs`.
///
/// BYTE-IDENTICAL TO THE GO HALF'S `disabledMessage`, which
/// `tests/control.rs` pins: the two libraries are held to one wire and one
/// vocabulary, and a mod author reading a log should not be able to tell which
/// language the guest was written in.
pub(crate) fn disabled_message(have: Version) -> alloc::string::String {
    let mut s = alloc::string::String::from("fkipc: disabled -- requires Factorio >= ");
    append_version(&mut s, MIN_ENGINE_VERSION);
    s.push_str("; this engine is ");
    if have.zero() {
        // A failed read is treated as below the floor, and saying "0.0.0" would
        // invite somebody to go looking for a 0.0.0 Factorio.
        s.push_str("unreadable");
    } else {
        append_version(&mut s, have);
    }
    s
}
