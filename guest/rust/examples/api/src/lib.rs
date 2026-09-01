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
    defines_direction_east, defines_inventory_chest, lua_entity_health_bulk,
    lua_entity_valid_bulk, read_on_player_created, read_on_tick, BulkOptF32,
    LuaChunkIterator, LuaCustomTable, LuaEntity, LuaEntityPrototype, LuaInventory, LuaProfiler,
    LuaStr, LuaSurface,
    Object, Value, EVENT_CUSTOMINPUTEVENT, EVENT_ON_BUILT_ENTITY, EVENT_ON_PLAYER_CREATED,
    EVENT_ON_PLAYER_MINED_ENTITY,
    EVENT_ON_ROBOT_BUILT_ENTITY, EVENT_ON_ROBOT_MINED_ENTITY, EVENT_ON_TICK, GAME, HELPERS,
    PROTOTYPES, SCRIPT, SETTINGS, SKIP_ON_BUILT_ENTITY_TAGS,
};

static TICKS_SEEN: AtomicU32 = AtomicU32::new(0);

/// What the profiler leg times. A static sink because the release profile would
/// otherwise delete a loop whose result nothing reads, and a profiler around no
/// work reports a duration that says nothing.
static PROF_SINK: AtomicU32 = AtomicU32::new(0);

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

    // ...AND ONE BY NAME, which is the only way a CUSTOM INPUT can be reached.
    //
    // A custom input is Factorio's keybind: a mod declares a custom-input
    // prototype at the data stage and subscribes to it by that prototype's own
    // NAME. It has no defines.events entry at all -- measured, the table has
    // 233 keys and CustomInputEvent is not one of them -- so the numeric form
    // logs that it could not resolve the event and the hotkey never fires.
    //
    // THE PROTOTYPE DELIBERATELY DOES NOT EXIST HERE, so what the in-game gate
    // reaches is the ENGINE's own refusal arriving as one log line with the mod
    // still loading. Mirrors the Go example line for line, which is what makes
    // the two pruning counts comparable.
    fkapi::subscribe_named(EVENT_CUSTOMINPUTEVENT, "fklua-example-input");
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

    // A `table | tuple` CONCEPT, READ OFF A REAL ENGINE, WHICH IS THE ONLY
    // PLACE THE ARRAY FORM IS OBSERVED. `Vector` is declared as a keyed table
    // plus an array shorthand and the description says which one the game picks:
    // "The game will always provide the array format". No stub can prove that --
    // a stub returns whatever it was written to return -- so the descriptor's
    // pos= flag is checked against Factorio here and nowhere else. Before it
    // existed this read 0.00,0.00 with status OK and nothing logged, which is
    // how WormholeBelts found it (item 8).
    //
    // entity_raw rather than entity: the handle route reads one prototype where
    // the materialising form would build every entity prototype in the game.
    match PROTOTYPES.entity_raw() {
        Err(e) => fk::log(&format!(
            "prototypes.entity as a handle failed: {}",
            e.as_str()
        )),
        Ok(raw) => match LuaCustomTable(raw).get(&Value::Str(LuaStr::from("inserter"))) {
            Err(e) => fk::log(&format!("prototypes.entity[inserter] failed: {}", e.as_str())),
            Ok(v) => match v.as_obj() {
                None => fk::log("prototypes.entity[inserter] is not an object"),
                Some(proto) => match LuaEntityPrototype(proto).inserter_drop_position() {
                    Err(e) => fk::log(&format!("inserter_drop_position failed: {}", e.as_str())),
                    Ok(None) => fk::log("shorthand struct: inserter_drop_position absent"),
                    Ok(Some(drop)) => fk::log(&format!(
                        "shorthand struct: inserter_drop_position = {:.2},{:.2}",
                        drop.x, drop.y
                    )),
                },
            },
        },
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
            Ok(Some(c)) => {
                // THE OTHER HALF OF THE PER-MEMBER PAIR, MEASURED IN THE SAME
                // RUN AND OFF AN ENTITY THAT IS ALREADY HERE.
                // `LuaEntity::position` is a MapPosition: the same
                // table-plus-shorthand shape as the Vector above, flagged pos=
                // by the same rule -- and the engine sends it KEYED. So the two
                // lines together are the per-member choice OBSERVED rather than
                // transcribed: one shape arriving in each of its two forms, in
                // one run, through one host. The claim was asserted in six
                // places in this repository and measured in none of them until
                // this read; it costs one host call.
                match LuaEntity(c).position() {
                    Err(e) => fk::log(&format!("entity.position failed: {}", e.as_str())),
                    Ok(p) => fk::log(&format!(
                        "keyed struct: entity.position = {:.2},{:.2}",
                        p.x, p.y
                    )),
                }
                match LuaEntity(c).get_inventory(defines_inventory_chest()) {
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
                }
            }
            _ => fk::log("create_entity(iron-chest) did not produce one"),
        }

        // A BULK ATTRIBUTE READ: one attribute off several handles in ONE
        // crossing, where the loop a guest writes today pays a whole host call
        // per entity.
        //
        // The corroboration is the point rather than the timing. The same
        // attribute is read BOTH ways in the same run, off the same entities,
        // and the log line carries both answers -- so a bulk read that returned
        // plausible-looking numbers from the wrong offsets could not pass. The
        // entities are chests this makes for the purpose: a headless map's own
        // entities are trees and rocks, and health is optional on those.
        let mut bulk_objs: alloc::vec::Vec<Object> = alloc::vec::Vec::new();
        for i in 0..3 {
            let c = surface.create_entity(&Value::Map(alloc::vec![
                (
                    Value::Str(LuaStr::from("name")),
                    Value::Str(LuaStr::from("iron-chest"))
                ),
                (
                    Value::Str(LuaStr::from("position")),
                    Value::Array(alloc::vec![
                        Value::Number(20.0 + (i as f64) * 2.0),
                        Value::Number(20.0)
                    ])
                ),
                (
                    Value::Str(LuaStr::from("force")),
                    Value::Str(LuaStr::from("player"))
                ),
            ]));
            if let Ok(Some(o)) = c {
                bulk_objs.push(o);
            }
        }
        if bulk_objs.is_empty() {
            fk::log("bulk: no entities to read, so this leg proved nothing");
        } else {
            // A MANDATORY BOOL and an OPTIONAL FLOAT, because the two element
            // shapes differ: one is the plain value and one carries the
            // presence byte the getter's own return block has.
            let mut live = alloc::vec![false; bulk_objs.len()];
            let n_live = lua_entity_valid_bulk(&bulk_objs, &mut live);
            let mut hp = alloc::vec![BulkOptF32::default(); bulk_objs.len()];
            let n_hp = lua_entity_health_bulk(&bulk_objs, &mut hp);
            match (n_live, n_hp) {
                (Ok(nl), Ok(nh)) => {
                    // ...and the same attribute one handle at a time, which is
                    // what the bulk answer has to agree with.
                    let mut agree = nl == bulk_objs.len() && nh == bulk_objs.len();
                    for (i, o) in bulk_objs.iter().enumerate() {
                        let e = LuaEntity(*o);
                        match (e.valid(), e.health()) {
                            (Ok(v), Ok(h)) => {
                                if v != live[i] || h.is_some() != hp[i].has {
                                    agree = false;
                                    break;
                                }
                                if let Some(hv) = h {
                                    if hv != hp[i].v {
                                        agree = false;
                                        break;
                                    }
                                }
                            }
                            _ => {
                                agree = false;
                                break;
                            }
                        }
                    }
                    let first = if hp[0].has {
                        format!("{}", hp[0].v as i64)
                    } else {
                        alloc::string::String::from("absent")
                    };
                    fk::log(&format!(
                        "bulk: {} of {} in one call, valid[0] {}, health[0] {}, per-call agrees {}",
                        nh,
                        bulk_objs.len(),
                        live[0],
                        first,
                        agree
                    ));
                }
                _ => fk::log("bulk read failed"),
            }
        }

        // THE INDEX OPERATOR'S WRITE HALF, `t[k] = v`, which is the only way a
        // mod changes its own runtime-global setting:
        //
        //     settings.global["my-setting"] = {value = true}
        //
        // Two calls, and the first is the whole point of the handle route:
        // global_raw hands back the LuaCustomTable itself rather than
        // materialising every setting in the game in order to write one.
        //
        // THIS MOD DECLARES NO SETTINGS, so what the engine answers is its
        // refusal -- "LuaCustomTable doesn't contain key" -- and that is the leg
        // worth having in a real game: a Factorio metamethod raising has to come
        // back as a STATUS, never as an unwind through the wasm frame the call
        // came from. A mod with a setting of its own gets Ok here and the
        // setting changes, per save.
        match SETTINGS.global_raw() {
            Ok(raw) => {
                let refused = LuaCustomTable(raw)
                    .set(
                        &Value::Str(LuaStr::from("fklua-no-such-setting")),
                        &Value::Map(alloc::vec![(
                            Value::Str(LuaStr::from("value")),
                            Value::Bool(true)
                        )]),
                    )
                    .is_err();
                fk::log(&format!(
                    "index-assign: settings.global[undefined] refused {}",
                    refused
                ));
                // ...AND WHAT THE ENGINE SAID, which the status alone cannot
                // carry. A host call returns an i32, so the Err says only that
                // the API raised; fk::last_error is the sentence it raised
                // WITH, and here that is Factorio's own "LuaCustomTable doesn't
                // contain key ...". A downstream suite asserts exactly this kind
                // of text as a tripwire, so that an engine which STOPS refusing
                // something fails a run instead of quietly widening it.
                //
                // Bytes, not a String: a Lua string is an arbitrary byte
                // sequence, so the lossy conversion is done HERE and named.
                if refused {
                    fk::log(&format!(
                        "last-error: [{}]",
                        alloc::string::String::from_utf8_lossy(&fk::last_error())
                    ));
                }
            }
            Err(e) => fk::log(&format!(
                "settings.global as a handle failed: {}",
                e.as_str()
            )),
        }

        // A GLOBAL FUNCTION, and the one that made the kind worth building:
        // `log()` is the ONLY way to read a LuaProfiler's duration. The class
        // has add, divide, reset, restart, stop, object_name, object_name_is
        // and valid -- not one of them returns the number -- and the engine
        // renders it only when the profiler is an ELEMENT of a LocalisedString.
        //
        //     local p = helpers.create_profiler()
        //     ...work...
        //     p.stop()
        //     log{"", "[marker] ", p}
        //
        // What lands in factorio-current.log is `... Duration: 12.368959ms`,
        // and a downstream harness regexes exactly that. There is no other
        // shape: fk::log takes a &str and a string cannot carry an object.
        match HELPERS.create_profiler(None) {
            Ok(p) => {
                // Something to time. The work is beside the point; that the
                // ENGINE renders the elapsed figure is the whole leg.
                let mut sink = 0u32;
                for i in 0..2000u32 {
                    sink = sink.wrapping_add(i);
                }
                PROF_SINK.store(sink, Ordering::Relaxed);
                if let Err(e) = LuaProfiler(p).stop() {
                    fk::log(&format!("profiler stop failed: {}", e.as_str()));
                }
                if let Err(e) = fkapi::log(&Value::Array(alloc::vec![
                    Value::Str(LuaStr::from("")),
                    Value::Str(LuaStr::from("global-fn: profiler ")),
                    Value::Obj(p),
                ])) {
                    fk::log(&format!("global-fn: log() failed: {}", e.as_str()));
                }
                // ...and table_size, the global function with a RETURN. A
                // three-key table the guest built itself, so the answer is
                // known.
                match fkapi::table_size(&Value::Map(alloc::vec![
                    (Value::Str(LuaStr::from("a")), Value::Number(1.0)),
                    (Value::Str(LuaStr::from("b")), Value::Number(2.0)),
                    (Value::Str(LuaStr::from("c")), Value::Number(3.0)),
                ])) {
                    Ok(n) => fk::log(&format!("global-fn: table_size = {}", n)),
                    Err(e) => {
                        fk::log(&format!("global-fn: table_size failed: {}", e.as_str()))
                    }
                }
            }
            Err(e) => fk::log(&format!("create_profiler failed: {}", e.as_str())),
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
