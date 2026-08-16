//! The Rust half of the two-mod fkipc demo: two sliders in a web page resize
//! and recolour a circle drawn at spawn in a running Factorio.
//!
//! Its sibling, `guest/go/examples/demo-daylight`, is the Go arm, and the two
//! are meant to run IN THE SAME GAME AT THE SAME TIME -- which is the whole
//! point of the pair. `--enable-lua-udp` binds ONE socket for the whole game,
//! so every inbound datagram raises `on_udp_packet_received` in BOTH mods and
//! each library's source-port filter is what keeps the two conversations apart
//! (`fkipc::Link::deliver_datagram`, and its Go mirror). Run one mod and that
//! machinery is never exercised; run two and it is exercised on every frame.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p demo-circle
//! fklua mod demo_circle.wasm --name fk-demo-circle --version 0.1.0 --author you
//! ```
//!
//! Driven by `sdk/go/cmd/ipcdemo`, and packaged, launched and proved end to end
//! by `scripts/run-ipcdemo.sh`.
//!
//! # The application protocol, and why it carries no floats
//!
//! Two channels -- 1 telemetry (MSG out), 2 control (REQ in) -- because a
//! channel's seq is shared by everything on it, so a lost REQ on a mixed
//! channel would raise a gap and therefore a spurious RESYNC on the telemetry.
//!
//! Every number on the wire is a DECIMAL INTEGER in a fixed unit. Formatting an
//! f64 in a guest means a dtoa two implementations would have to agree on digit
//! for digit, in a heap that is in the save; a fixed-point integer is exact,
//! allocation-free and identical in both languages. The companion divides.
//!
//! ```text
//! REQ   "set radius 24"   |  "set hue 210"
//! RESP  "ok radius 24"    -- the value ACTUALLY APPLIED, after clamping
//! RESP  "err <reason>"
//! MSG   "tick=1234 radius=24 hue=210 evo=13400 entities=57"
//! ```
//!
//! `evo` is the enemy force's evolution factor in parts per million, so a
//! fresh map reads 0 and a late one reads several hundred thousand.
//!
//! A set is a set, so the RPC is idempotent by construction -- which is what
//! this protocol asks of every request, because a retried REQ may be executed
//! again outside the dedup window.

#![no_std]

extern crate alloc;

use alloc::vec::Vec;

use fkapi::{
    Color, EntitySearchFilters, LuaForce, LuaRenderObject, LuaRenderingDrawCircleArgs, LuaStr,
    LuaSurface, MapPosition, Value, GAME, RENDERING,
};
use fkipc::{Channel, Config, Link, Priority, Profile, Request, SessionEvent};

/// The COMPANION's port, compiled in because a guest has no configuration
/// file. `ipcdemo`'s `-circle-port` default matches it, and the daylight mod
/// uses the one below.
const PEER_PORT: u16 = 29437;

const TELEMETRY: Channel = Channel::new(1);
const CONTROL: Channel = Channel::new(2);

/// One telemetry frame every half second of game time. Outbound is free --
/// `send_udp` is a local side effect that never enters game state -- so the
/// only cost is the host calls it makes to sample, and half a second is what a
/// slider readout wants.
const TELEMETRY_TICKS: u32 = 30;

const MIN_RADIUS: i32 = 2;
const MAX_RADIUS: i32 = 60;

/// The guest's own scratch. Single-threaded by construction -- wasm without the
/// threads proposal has one thread -- which is the same ground `fk`'s allocator
/// and `fkipc`'s singleton stand on.
struct Scratch {
    ticks: u32,
    radius: i32,
    hue: i32,
    /// Set by a handler, acted on from `fk_on_tick`. See [`on_request`].
    dirty: bool,
    /// The render object's own id, which SURVIVES A SAVE where a handle does
    /// not: Factorio persists script render objects, and this number is in
    /// guest memory, which persistence carries too. 0 means "not drawn yet".
    circle_id: u64,
    resp: Vec<u8>,
    tel: Vec<u8>,
}

static mut SCRATCH: Scratch = Scratch {
    ticks: 0,
    radius: 12,
    hue: 40,
    dirty: false,
    circle_id: 0,
    resp: Vec::new(),
    tel: Vec::new(),
};

#[allow(static_mut_refs)]
fn scratch() -> &'static mut Scratch {
    unsafe { &mut SCRATCH }
}

/// Where a Rust guest's subscriptions go.
///
/// NOT `fk_on_init`, and the difference is load-bearing. `script.on_init` fires
/// once, when a save is CREATED; `_initialize` is called by control.lua on
/// every load, and event registrations are not saved.
#[no_mangle]
pub extern "C" fn _initialize() {
    // `Profile::Client`, AND IT IS MEASURED RATHER THAN CHOSEN FOR THE OBVIOUS
    // REASON. This mod is meant to run in a graphical single-player game, which
    // is what the client profile is for -- it omits `for_player` on `send_udp`
    // and pumps with a bare `recv_udp()`, which is what every graphical-client
    // mod in the ecosystem does. What was NOT known is whether that arm also
    // works on a HEADLESS server: the probe measured the omitted-`for_player`
    // SEND working headless on 2.0.77, but bare `recv_udp()` was one of the two
    // arms that CRASHED that build, and the 2.1.14 re-run only re-measured
    // `recv_udp(0)`.
    //
    // Measured on 2.1.14 by `scripts/run-ipcdemo.sh --smoke` on 2026-08-06: the
    // client arm holds a full session on a headless `--start-server`, with
    // every leg green and zero drops. So ONE PROFILE SERVES BOTH the automated
    // gate and a person's graphical session, and there is no build-time switch
    // to explain to anybody.
    //
    // Its one cost, stated: `RETRY_TICKS_CLIENT` is 6 and a headless server's
    // p90 round trip is ~5.7 ticks, so a slow reply can be retransmitted where
    // the server profile's 15 would have waited. That is what the dedup table
    // is for, and the smoke run measures `retries=0` in practice.
    // THE IDENTITY TOKEN, on both sides of the pairing, and it is the fourth
    // filter: the HELLO is the session boundary, the epoch is the frame filter,
    // the SOURCE PORT is the mod filter, and the NAME is the schema filter --
    // the only one that can refuse a peer whose transport is entirely correct.
    // Cross the two demo mods' `-daylight-port`/`-circle-port` and every layer
    // below this one is satisfied while the two ends disagree about what
    // channel 1 means; here that is a session that never comes up rather than a
    // slider that does nothing.
    //
    // `/1` is the SCHEMA TAG: the author's claim about channel-contract
    // compatibility, bumped when the meaning of a channel changes, and
    // deliberately not a build id -- this crate is rebuilt constantly and the
    // pairing must survive that. `run-ipcdemo.sh --smoke`'s identity leg swaps
    // these against a live game.
    fkipc::open(Config {
        port: PEER_PORT,
        profile: Profile::Client,
        name: "fk-demo-circle/1",
        expect_peer: "fk-demo-circle/1",
        ..Default::default()
    });

    TELEMETRY.open(Priority::Bulk);
    CONTROL.open(Priority::Control);

    CONTROL.on_request(on_request);
    TELEMETRY.on_resync(resync);
    fkipc::on_session(session);
}

/// Optional now, and kept because this mod is what `run-ipcdemo.sh --play`
/// joins a live client to: `fkipc::reload` does nothing, and the export being
/// present is half of what that run proves. See `fkipc::Link::reload`.
#[no_mangle]
pub extern "C" fn fk_after_load() {
    fkipc::reload();
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    scratch().ticks = tick;
    fkipc::pump(tick);

    // THE EFFECT IS APPLIED HERE AND NOT IN THE HANDLER. The handler runs
    // inside a dispatch the engine raised from inside `recv_udp`, so a host
    // call there is a host call nested inside a host call -- and in this
    // mirror the transport is out of the link for the whole poll, so anything
    // touching it is refused outright. A flag plus an apply from `fk_on_tick`
    // is right in both languages, and it collapses several sets in one tick
    // into one round of host calls.
    if scratch().dirty || scratch().circle_id == 0 {
        scratch().dirty = false;
        apply();
    }

    if tick % TELEMETRY_TICKS == 0 {
        let frame = sample();
        TELEMETRY.send(frame);
    }
}

#[no_mangle]
pub extern "C" fn fk_on_event(id: u32, ptr: u32) {
    if fkipc::on_event(id, ptr) {
        return;
    }
    // ... a guest's own events would be matched on here.
}

/// A gap is answered with a SNAPSHOT and never a replay: the producer usually
/// cannot replay, because the state it described no longer exists.
///
/// It reaches the link through the `&mut Link` it was handed rather than
/// through the crate singleton, which is already borrowed here -- the one place
/// this mirror's API differs from the Go half's, and the reason is that two
/// live `&mut` to one object is undefined behaviour rather than a style
/// question.
fn resync(l: &mut Link) {
    let frame = sample();
    l.snapshot(TELEMETRY.id(), frame);
}

fn session(_l: &mut Link, ev: SessionEvent) {
    fk::log(match ev {
        SessionEvent::Up => "fkipc session up",
        SessionEvent::Down => "fkipc session down",
        SessionEvent::Reset => "fkipc session reset",
    });
}

/// The two sliders.
///
/// THE PAYLOAD BORROWS the library's own buffer and the borrow ends when this
/// returns; the return value is copied before it goes on the wire, so answering
/// out of a reused buffer is safe -- and here the compiler is what says so,
/// where the Go half has a comment.
fn on_request(_l: &mut Link, r: Request) -> &'static [u8] {
    let (key, val) = match parse_set(r.payload) {
        Some(kv) => kv,
        None => return resp_err(b"want: set <key> <int>"),
    };
    // Clamped rather than refused, and the RESP carries what was APPLIED: a UI
    // that shows the ack is then showing the truth rather than its own request.
    match key {
        b"radius" => {
            scratch().radius = val.clamp(MIN_RADIUS, MAX_RADIUS);
            scratch().dirty = true;
            resp_ok(b"radius", scratch().radius)
        }
        b"hue" => {
            scratch().hue = val.rem_euclid(360);
            scratch().dirty = true;
            resp_ok(b"hue", scratch().hue)
        }
        _ => resp_err(b"unknown key"),
    }
}

/// Draws the circle if it is not there and pushes the current radius and hue
/// into it if it is.
///
/// The id round trip is what makes this survive a save: `draw_circle` hands
/// back a HANDLE, which is transient and dead after the dispatch that produced
/// it, so what is kept is the render object's own `id` and the handle is
/// re-fetched. A `None` from `get_object_by_id` means somebody destroyed it --
/// or the save came from a build before it existed -- and the answer is to draw
/// a new one.
fn apply() {
    let s = scratch();
    let surface = match surface() {
        Some(v) => v,
        None => return,
    };
    if s.circle_id != 0 {
        if let Ok(Some(o)) = RENDERING.get_object_by_id(s.circle_id) {
            let obj = LuaRenderObject(o);
            let _ = obj.set_radius(s.radius as f64);
            let _ = obj.set_color(hue_color(s.hue));
            return;
        }
        s.circle_id = 0;
    }
    let args = LuaRenderingDrawCircleArgs {
        color: hue_color(s.hue),
        radius: s.radius as f64,
        width: Some(6.0),
        filled: Some(false),
        // A ScriptRenderTarget accepts a MapPosition, which in Lua is a table
        // with x and y. Spawn, so a player who just joined is standing in it.
        target: Value::Map(alloc::vec![
            (Value::Str(LuaStr::from("x")), Value::Number(0.0)),
            (Value::Str(LuaStr::from("y")), Value::Number(0.0)),
        ]),
        surface: surface.0,
        ..Default::default()
    };
    if let Ok(o) = RENDERING.draw_circle(args) {
        if let Ok(id) = LuaRenderObject(o).id() {
            s.circle_id = id;
        }
    }
}

/// One telemetry frame, into a reused buffer, because a guest heap is in the
/// save.
fn sample() -> &'static [u8] {
    let s = scratch();
    s.tel.clear();
    s.tel.extend_from_slice(b"tick=");
    append_i32(&mut s.tel, s.ticks as i32);
    s.tel.extend_from_slice(b" radius=");
    append_i32(&mut s.tel, s.radius);
    s.tel.extend_from_slice(b" hue=");
    append_i32(&mut s.tel, s.hue);

    // Parts per million, so an integer carries the whole useful range of a
    // factor that is almost always well under 1.
    s.tel.extend_from_slice(b" evo=");
    append_i32(&mut s.tel, (evolution() * 1_000_000.0) as i32);

    // THE READBACK THAT MAKES THE SLIDER MEAN SOMETHING: how many entities are
    // actually inside the circle the other slider just resized. Trees and rocks
    // count, which is why a fresh map is not zero.
    s.tel.extend_from_slice(b" entities=");
    append_i32(&mut s.tel, entities_in_circle(s.radius as f64));
    &s.tel
}

fn entities_in_circle(radius: f64) -> i32 {
    let surface = match surface() {
        Some(v) => v,
        None => return -1,
    };
    let filter = EntitySearchFilters {
        position: Some(MapPosition { x: 0.0, y: 0.0 }),
        radius: Some(radius),
        ..Default::default()
    };
    match surface.count_entities_filtered(filter) {
        Ok(n) => n as i32,
        // -1 rather than 0: "the call failed" and "there is nothing there" are
        // different facts and a UI that showed them the same would be lying.
        Err(_) => -1,
    }
}

/// The enemy force's evolution factor.
///
/// `game.forces` is a DICTIONARY and it is walked rather than indexed, because
/// there is no `get_force` in the bindings -- the dictionary return is the
/// bound shape. Three entries on a normal map, and this runs twice a second.
fn evolution() -> f64 {
    let forces = match GAME.forces() {
        Ok(v) => v,
        Err(_) => return 0.0,
    };
    for (k, o) in forces.iter() {
        if let Value::Str(name) = k {
            if name.as_bytes() == b"enemy" {
                return LuaForce(*o).get_evolution_factor(None).unwrap_or(0.0);
            }
        }
    }
    0.0
}

/// A HANDLE IS TRANSIENT: valid for the dispatch that produced it and released
/// when that dispatch returns, so a stored one is `ERR_BAD_HANDLE` on the next
/// tick. The re-read is one host call on the two ticks a second that need it.
fn surface() -> Option<LuaSurface> {
    match GAME.get_surface(&Value::Number(1.0)) {
        Ok(Some(o)) => Some(LuaSurface(o)),
        _ => None,
    }
}

/// HSV -> RGB at full saturation and value, in integer degrees.
///
/// Pure arithmetic on purpose: it is the same computation on every peer, which
/// is what determinism asks, and it needs nothing from the host.
fn hue_color(hue: i32) -> Color {
    let h = hue.rem_euclid(360);
    let sector = h / 60;
    let f = (h % 60) as f32 / 60.0;
    let (r, g, b) = match sector {
        0 => (1.0, f, 0.0),
        1 => (1.0 - f, 1.0, 0.0),
        2 => (0.0, 1.0, f),
        3 => (0.0, 1.0 - f, 1.0),
        4 => (f, 0.0, 1.0),
        _ => (1.0, 0.0, 1.0 - f),
    };
    Color {
        r: Some(r),
        g: Some(g),
        b: Some(b),
        a: Some(0.85),
    }
}

fn resp_ok(key: &[u8], val: i32) -> &'static [u8] {
    let s = scratch();
    s.resp.clear();
    s.resp.extend_from_slice(b"ok ");
    s.resp.extend_from_slice(key);
    s.resp.push(b' ');
    append_i32(&mut s.resp, val);
    &s.resp
}

fn resp_err(why: &[u8]) -> &'static [u8] {
    let s = scratch();
    s.resp.clear();
    s.resp.extend_from_slice(b"err ");
    s.resp.extend_from_slice(why);
    &s.resp
}

/// Reads `set <key> <int>` out of a payload without allocating.
fn parse_set(p: &[u8]) -> Option<(&[u8], i32)> {
    let mut it = p.split(|c| *c == b' ').filter(|t| !t.is_empty());
    if it.next()? != b"set" {
        return None;
    }
    let key = it.next()?;
    let val = atoi(it.next()?)?;
    Some((key, val))
}

fn atoi(b: &[u8]) -> Option<i32> {
    let (neg, digits) = match b.first() {
        Some(b'-') => (true, &b[1..]),
        _ => (false, b),
    };
    if digits.is_empty() || digits.len() > 9 {
        return None;
    }
    let mut v: i32 = 0;
    for c in digits {
        if !c.is_ascii_digit() {
            return None;
        }
        v = v * 10 + (c - b'0') as i32;
    }
    Some(if neg { -v } else { v })
}

fn append_i32(b: &mut Vec<u8>, v: i32) {
    if v < 0 {
        b.push(b'-');
    }
    let mut u = v.unsigned_abs();
    if u == 0 {
        b.push(b'0');
        return;
    }
    let mut d = [0u8; 10];
    let mut i = d.len();
    while u > 0 {
        i -= 1;
        d[i] = b'0' + (u % 10) as u8;
        u /= 10;
    }
    b.extend_from_slice(&d[i..]);
}
