//! The Rust mirror of `guest/go/examples/api`: a guest that calls the Factorio
//! API and RECEIVES EVENTS.
//!
//! `array` covers the marshalling far more thoroughly, but it never subscribes
//! to anything. This one exists for the half `array` cannot reach --
//! `fk.subscribe`, the host's event scratch buffer, and `fk_on_event` -- which
//! was the last part of the corpus with no Rust side at all.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::vec::Vec;
use core::cell::UnsafeCell;
use core::sync::atomic::{AtomicU32, Ordering};

use fkapi::{
    defines_direction_east, defines_inventory_chest, read_on_player_created, read_on_tick,
    LuaChunkIterator, LuaEntity, LuaInventory, LuaStr, LuaSurface, Object, Value,
    EVENT_ON_BUILT_ENTITY, EVENT_ON_PLAYER_CREATED, EVENT_ON_PLAYER_MINED_ENTITY,
    EVENT_ON_ROBOT_BUILT_ENTITY, EVENT_ON_ROBOT_MINED_ENTITY, EVENT_ON_TICK, GAME, SCRIPT,
    SKIP_ON_BUILT_ENTITY_TAGS,
};

static TICKS_SEEN: AtomicU32 = AtomicU32::new(0);

#[no_mangle]
pub extern "C" fn fk_on_event(id: u32, ptr: u32) {
    match id {
        EVENT_ON_PLAYER_CREATED => {
            // A GENERATED STRUCT, not a cast at a hand-derived offset. This
            // example used to read `*(ptr as *const u32)` with the layout in a
            // comment, and fields are placed by the API's `order`, so one new
            // optional field upstream shifts everything after it and the guest
            // quietly reads a neighbouring value instead. It was also the shape
            // every Rust port in the campaign copied, twelve to seventeen
            // offsets at a time, because until 2026-08-03 there was nothing else
            // to copy.
            let e = read_on_player_created(ptr);
            fk::log(&format!("event: player created, index {}", e.player_index));
        }
        EVENT_ON_TICK => {
            // The tick arrives as an encoded field rather than a call argument,
            // which is the difference between this path and the legacy
            // fk_on_tick hook.
            let n = TICKS_SEEN.fetch_add(1, Ordering::Relaxed) + 1;
            if n == 20 {
                let e = read_on_tick(ptr);
                fk::log(&format!("event: on_tick #{} carries tick {}", n, e.tick));
            }
        }
        _ => {}
    }
}

/// Where a Rust guest's subscriptions go.
///
/// NOT `fk_on_init`, and the difference is load-bearing. `script.on_init` fires
/// once, when a save is CREATED; `_initialize` is called by control.lua on
/// every load. A subscription made in `fk_on_init` therefore vanishes the first
/// time the save is reloaded, which is exactly what a headless benchmark does --
/// the API calls kept working and the events silently stopped arriving.
///
/// A Go guest gets this for free: TinyGo runs package `init()` from
/// `_initialize`. Rust has no pre-main initialiser in a cdylib reactor, so the
/// guest exports the hook itself.
#[no_mangle]
pub extern "C" fn _initialize() {
    fkapi::subscribe(EVENT_ON_PLAYER_CREATED);
    fkapi::subscribe(EVENT_ON_TICK);

    // ...AND FOUR WITH FACTORIO'S OWN FILTERS, which the engine applies in C++
    // before the guest is entered.
    //
    // FOUR RATHER THAN ONE, AND THE COUNT IS THE TEST. These are here for the
    // PRUNING as much as for the filtering: fklua mod ships only the event
    // descriptors it can prove a guest subscribes to, by scanning the wasm for a
    // constant reaching the import, and it is all-or-nothing -- one id it cannot
    // prove and the whole table ships. subscribe_filtered is several times the
    // size of subscribe, so without an inline attribute whether the id survives
    // is rustc's cost heuristic's decision, taken per call site: measured on this
    // very guest, ONE filtered call site inlined and FOUR did not. A mod crossed
    // that line by GROWING, with nothing to tell it.
    //
    // Reported from the field before it was reproduced here (a downstream Rust
    // port): 991,040 bytes of Lua against 906,393 for the same
    // mod with the filters taken out -- 85 KB parsed by the game on every load,
    // in every save. Gated by
    // TestTheEventIdSurvivesTheGeneratedRustSubscribeWrapper.
    let only_chests = fkapi::name_filter(&["iron-chest"]);
    // ...AND ONE OF THEM ALSO MASKED, which is the third subscribe shape and
    // the one with no Rust binding at all until 2026-08-03: the ABI's third
    // subscribe argument existed and the generated import declared two, so a
    // Rust guest could not decline a field however expensive it was.
    // on_built_entity's `tags` is a dictionary this guest never reads, and a
    // masked container arrives EMPTY rather than stale.
    fkapi::subscribe_filtered_masked(
        EVENT_ON_BUILT_ENTITY,
        SKIP_ON_BUILT_ENTITY_TAGS,
        &only_chests,
    );
    fkapi::subscribe_filtered(EVENT_ON_ROBOT_BUILT_ENTITY, &only_chests);
    fkapi::subscribe_filtered(EVENT_ON_PLAYER_MINED_ENTITY, &only_chests);

    // ...AND THE FOURTH BY prototype TYPE RATHER THAN BY NAME, which is the
    // other filter helper and the one nothing exercised until now. Mirrors the
    // Go example line for line, which is what makes the two counts comparable.
    //
    // Same event count and same wire shape -- one map term, two keys -- so
    // nothing calibrated moves; what it demonstrates is the choice.
    // `iron-chest` is one prototype and `container` is every chest there is,
    // including ones a mod added. Terms OR together within a call, so the mixed
    // form is a name_filter Vec extended with a type_filter one.
    fkapi::subscribe_filtered(
        EVENT_ON_ROBOT_MINED_ENTITY,
        &fkapi::type_filter(&["container"]),
    );
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    fk::log("api example: reaching Factorio from Rust");

    // A DEFINE, ASKED FOR RATHER THAN WRITTEN DOWN. defines.direction.east is
    // 4 in this Factorio and that number is not in the API description at all
    // -- runtime-api.json carries the name and the order, never the value -- so
    // a hand-written 4 is a guess that happens to be right today. Every Rust
    // port in the campaign hand-declared the fk.define import and re-derived
    // the ids from the Go generator's source, because there was no Rust
    // accessor to call. The accessor resolves by name at load and caches; the
    // compiler proves the id constant and ships one path out of the whole set
    // (`define_values` in census.json).
    fk::log(&format!(
        "defines.direction.east = {}",
        defines_direction_east()
    ));
}

#[no_mangle]
pub extern "C" fn fk_on_tick(tick: u32) {
    // Once, rather than sixty times a second.
    if tick != 30 {
        return;
    }

    // `game` is handle 2, fixed by the ABI. Every other handle is reached by
    // calling something.
    let speed = match GAME.speed() {
        Ok(s) => s,
        Err(e) => {
            // A host call never raises into wasm -- there are no coroutines, so
            // an error crossing that boundary could not unwind the frame it came
            // from. It arrives as a Status instead.
            fk::log(&format!("reading game.speed failed: {}", e.as_str()));
            return;
        }
    };
    fk::log(&format!("game.speed = {:.2}", speed));

    if let Ok(t) = GAME.tick() {
        fk::log(&format!("game.tick = {}", t));
    }

    // The round trip proves the value really crossed rather than being read back
    // out of the guest's own buffer.
    if let Err(e) = GAME.set_speed(speed * 2.0) {
        fk::log(&format!("writing game.speed failed: {}", e.as_str()));
        return;
    }
    if let Ok(d) = GAME.speed() {
        fk::log(&format!("game.speed doubled = {:.2}", d));
    }

    // A DICTIONARY RETURN KEYED BY A UNION, which is `game.surfaces` and which
    // this backend emitted NOTHING for until 2026-08-03: the key is
    // `uint32 | string`, so it is tier 2, so it is neither Ord nor Hash and
    // cannot key a BTreeMap. It is a Vec of pairs instead, and the order is the
    // host's -- which is the whole reason it is not a map: surface order decides
    // registration order decides ids, and two clients disagreeing about that is
    // a desync rather than a cosmetic difference. Two ports walked
    // get_surface(1), get_surface(2), ... instead, and gave up after a run of
    // misses they had to pick a length for.
    //
    // The key arrives as whatever `pairs()` over the engine's LuaCustomTable
    // yields, which for this one is the NAME rather than the index.
    match GAME.surfaces() {
        Ok(s) => {
            let mut out = format!("surfaces= {}", s.len());
            for (k, _) in &s {
                if let Value::Str(n) = k {
                    out.push(' ');
                    // A surface name is engine ASCII; to_string_lossy names the
                    // conversion for a log line, and as_str is the checked one.
                    out.push_str(&n.to_string_lossy());
                }
            }
            fk::log(&out);
        }
        Err(e) => fk::log(&format!("game.surfaces failed: {}", e.as_str())),
    }

    // A HOST-SIDE STRING PREDICATE, mirroring the Go example. `surface.name` is
    // a string, and asking whether it EQUALS one never brings the string across:
    // the comparison happens in Lua and a bool comes back, so the guest keeps
    // nothing at all.
    if let Ok(Some(s)) = GAME.get_surface(&Value::Number(1.0)) {
        let surface = LuaSurface(s);
        match surface.name_is("nauvis") {
            Ok(v) => fk::log(&format!(
                "surface.name == \"nauvis\" is {}, and no string crossed", v
            )),
            Err(e) => fk::log(&format!("surface.name_is failed: {}", e.as_str())),
        }

        // ...and a CONTAINER RETURN into a Vec the caller keeps. Same member id
        // and same host call as find_entities_filtered; only the container
        // differs, and it is the whole per-compile residual a downstream mod
        // was leaving behind.
        let buf = unsafe { &mut *ENT_BUF.0.get() };
        match surface.find_entities_filtered_into(buf, Default::default()) {
            Ok(()) => fk::log(&format!(
                "find_entities_filtered into a reused buffer: {} entities, cap {}",
                buf.len(),
                buf.capacity()
            )),
            Err(e) => fk::log(&format!("find_entities_filtered_into failed: {}", e.as_str())),
        }

        // A CLASS OPERATOR: the Lua `it()`, which is LuaChunkIterator's whole
        // useful surface and was unreachable in either language until the
        // generators learned to read Class.Operators. An operator has no name
        // to resolve, so the ABI dispatches on the kind alone.
        if let Ok(it) = surface.get_chunks() {
            match LuaChunkIterator(it).call() {
                Err(e) => fk::log(&format!("chunk iterator failed: {}", e.as_str())),
                Ok(None) => fk::log("chunk operator: iterator was already done"),
                Ok(Some(c)) => fk::log(&format!("chunk operator: first chunk {},{}", c.x, c.y)),
            }
        }

        // ...and the INDEX and LENGTH operators, `#inv` and `inv[1]`, on a
        // chest this makes for the purpose: a headless run has no player, so
        // there is no inventory lying about to read.
        let chest = surface.create_entity(&Value::Map(alloc::vec![
            (
                Value::Str(LuaStr::from("name")),
                Value::Str(LuaStr::from("iron-chest"))
            ),
            (
                Value::Str(LuaStr::from("position")),
                Value::Array(alloc::vec![Value::Number(8.0), Value::Number(8.0)])
            ),
            (
                Value::Str(LuaStr::from("force")),
                Value::Str(LuaStr::from("player"))
            ),
        ]));
        match chest {
            Ok(Some(c)) => match LuaEntity(c).get_inventory(defines_inventory_chest()) {
                Ok(Some(inv)) => {
                    let b = LuaInventory(inv);
                    match (b.length(), b.get(1)) {
                        (Ok(n), Ok(slot)) => fk::log(&format!(
                            "inventory operators: #inv = {}, inv[1] valid {}",
                            n,
                            slot.is_valid()
                        )),
                        _ => fk::log("inventory operators failed"),
                    }
                }
                _ => fk::log("the chest had no chest inventory"),
            },
            _ => fk::log("create_entity(iron-chest) did not produce one"),
        }

        // A MEMBER RETURNING SEVERAL VALUES, deferred for four milestones on
        // naming rules alone -- and this one is the ONLY way to arm
        // on_object_destroyed, so no guest in either language could subscribe
        // to that event in any useful sense. Rust returns a tuple.
        match SCRIPT.register_on_object_destroyed(&Value::Obj(s)) {
            Ok((reg, useful, kind)) => fk::log(&format!(
                "multi-return: registration {} useful {} kind {}",
                reg, useful, kind
            )),
            Err(e) => fk::log(&format!(
                "register_on_object_destroyed failed: {}",
                e.as_str()
            )),
        }
    }
}

/// The destination the Into call reuses. A static buffer is the shape a real
/// mod wants: allocated once, refilled every call, never handed to anything
/// that outlives the next one.
///
/// A guest is single-threaded by construction -- Factorio calls one export at a
/// time -- so the `Sync` here is the same claim every other static in this
/// corpus makes.
struct EntBuf(UnsafeCell<Vec<Object>>);
unsafe impl Sync for EntBuf {}
static ENT_BUF: EntBuf = EntBuf(UnsafeCell::new(Vec::new()));
