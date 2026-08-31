//! The data-stage end-to-end guest: a Rust program that becomes a Factorio
//! mod's settings.lua, data.lua and data-final-fixes.lua.
//!
//! This is deliberately a LINE-FOR-LINE mirror of
//! `guest/go/examples/datastage`, because the gate is that the two produce
//! IDENTICAL effects: one test drives both through one `fk_data.lua` against
//! one stand-in `data` table and requires the same call sequence out of each,
//! and the in-game gate dumps both and requires the same SHA-256. Two programs
//! that merely both work would not test the thing the mirror exists to test.
//!
//! It reaches every one of the seven imports rather than being a hello-world:
//! it declares a setting, defines prototypes from a computed loop, reads a
//! value back out of `data.raw` and extends something derived from it,
//! deep-copies a base prototype and patches the copy, enumerates a prototype
//! type in sorted order, and asks whether a prototype it might collide with is
//! already defined.
//!
//! ```sh
//! cargo build --release --target wasm32-unknown-unknown -p datastage
//! fklua mod control.wasm --data-module datastage.wasm --name fk-datastage ...
//! ```

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;
use alloc::vec::Vec;
use fkdata::{arr, bool_, num, obj, str_, P, V};

/// The mod's own prefix, so nothing here can collide with a real mod.
const PREFIX: &str = "fkd-";

fn name(suffix: &str) -> String {
    format!("{PREFIX}{suffix}")
}

#[no_mangle]
pub extern "C" fn fk_settings() {
    fkdata::log("fkdata example: settings stage");

    // A startup setting. At this stage `settings` itself does not exist yet --
    // a mod's settings are not readable while they are being declared -- so
    // asking is the way to show that, rather than a comment.
    if fkdata::startup_setting(&name("enabled")).is_some() {
        fkdata::log("fkdata example: a startup setting is readable at the settings stage");
    }

    fkdata::extend(&[obj(&[
        ("type", str_("bool-setting")),
        ("name", str_(&name("enabled"))),
        ("setting_type", str_("startup")),
        ("default_value", bool_(true)),
        ("order", str_("a")),
    ])]);
}

#[no_mangle]
pub extern "C" fn fk_data() {
    fkdata::log(&format!(
        "fkdata example: data stage, base {}",
        base_version()
    ));

    // The mod's own name, from the packager through env(4): the prefix a
    // library would derive instead of hardcoding, logged so the in-game run
    // and the mirror test both pin that it arrives.
    fkdata::log(&format!("fkdata example: mod name is {}", fkdata::mod_name()));

    // defines.prototypes through env(5): the base-type map a prototype browser
    // needs and data.raw alone cannot answer. One line, both accessors.
    let base = fkdata::base_type("transport-belt").unwrap_or_default();
    fkdata::log(&format!(
        "fkdata example: transport-belt is an {base}; item derives {} types",
        fkdata::derived_types("item").len()
    ));

    // A computed table. Eight sprites out of one loop, with the offset
    // arithmetic done in Rust rather than written out as sixteen magic numbers
    // -- which is the case the whole feature exists for.
    //
    // A `let` RATHER THAN A `const`, to match the Go mirror's `var` -- see the
    // note there. Rust's const arithmetic already follows IEEE f64, so this
    // changes nothing here; it is written the same way so that the two examples
    // differ in as little as possible, which is what makes a difference between
    // them mean something.
    let bias: f64 = 0.104;
    let sides: [(&str, f64, f64); 4] = [
        ("n", 0.0, -1.0),
        ("e", 1.0, 0.0),
        ("s", 0.0, 1.0),
        ("w", -1.0, 0.0),
    ];

    let mut sprites: Vec<V> = Vec::new();
    for (kind, dir) in ["in", "out"].iter().enumerate() {
        let d = if *dir == "out" {
            0.3 + bias
        } else {
            0.3 - bias
        };
        for (i, (s, sx, sy)) in sides.iter().enumerate() {
            sprites.push(obj(&[
                ("type", str_("sprite")),
                ("name", str_(&format!("{PREFIX}arrow-{dir}-{s}"))),
                ("filename", str_("__core__/graphics/empty.png")),
                ("width", num(1.0)),
                ("height", num(1.0)),
                ("x", num(((kind * 4 + i) * 32) as f64)),
                ("scale", num(0.5)),
                ("shift", arr(&[num(sx * d), num(sy * d)])),
                ("flags", arr(&[str_("no-crop")])),
            ]));
        }
    }
    fkdata::extend(&sprites);

    // Read then extend: a technology whose research cost is base's own, rather
    // than a copy of it that goes stale when base moves.
    let count = fkdata::get(&[
        P::S("technology"),
        P::S("logistics"),
        P::S("unit"),
        P::S("count"),
    ]);
    let time = fkdata::get(&[
        P::S("technology"),
        P::S("logistics"),
        P::S("unit"),
        P::S("time"),
    ])
    .unwrap_or(V::Nil);
    let ingredients = fkdata::get(&[
        P::S("technology"),
        P::S("logistics"),
        P::S("unit"),
        P::S("ingredients"),
    ])
    .unwrap_or(V::Nil);
    if let Some(count) = count {
        fkdata::extend(&[obj(&[
            ("type", str_("technology")),
            ("name", str_(&name("marker"))),
            ("icon", str_("__core__/graphics/empty.png")),
            ("icon_size", num(1.0)),
            ("effects", arr(&[])),
            ("prerequisites", arr(&[str_("logistics")])),
            (
                "unit",
                V::Map(alloc::vec![
                    (str_("count"), count),
                    (str_("time"), time),
                    (str_("ingredients"), ingredients),
                ]),
            ),
            ("order", str_(&format!("a-{PREFIX}"))),
        ])]);
    }

    // Clone and patch: the shape a hidden prototype takes, and the one thing
    // that cannot be done by reading a prototype into the guest and writing it
    // back without risking every leaf it does not touch.
    fkdata::clone_("transport-belt", "transport-belt", &name("belt"));
    let belt = name("belt");
    fkdata::set(
        &num(0.25),
        &[P::S("transport-belt"), P::S(&belt), P::S("speed")],
    );
    fkdata::set(
        &V::Nil,
        &[P::S("transport-belt"), P::S(&belt), P::S("minable")],
    );
    fkdata::set(
        &V::Nil,
        &[P::S("transport-belt"), P::S(&belt), P::S("next_upgrade")],
    );
    fkdata::set(
        &arr(&[str_("not-on-map")]),
        &[P::S("transport-belt"), P::S(&belt), P::S("flags")],
    );

    // A NESTED patch, through NUMERIC path elements -- collision_box is an array
    // of two arrays, so this reaches four leaves two levels down.
    //
    // IT IS WHAT TELLS A DEEP COPY FROM A SHALLOW ONE: under a shallow clone
    // `collision_box` is the SOURCE's table, so these four writes would silently
    // shrink base's own transport belt and the acceptance dump would say so.
    // Every patch above is top-level and a shallow clone survives all of them.
    for (i, j, v) in [
        (1.0, 1.0, -0.35),
        (1.0, 2.0, -0.35),
        (2.0, 1.0, 0.35),
        (2.0, 2.0, 0.35),
    ] {
        fkdata::set(
            &num(v),
            &[
                P::S("transport-belt"),
                P::S(&belt),
                P::S("collision_box"),
                P::N(i),
                P::N(j),
            ],
        );
    }

    // The sorted enumeration primitive, and what it is FOR: the fastest belt in
    // the game, with ties broken by a sorted name rather than by whichever
    // order the mods happened to load in.
    let mut best = -1.0_f64;
    let mut best_name = String::new();
    for n in fkdata::keys(&[P::S("transport-belt")]) {
        if let Some(s) = fkdata::get(&[P::S("transport-belt"), P::S(&n), P::S("speed")]) {
            if s.number() > best {
                best = s.number();
                best_name = n;
            }
        }
    }
    fkdata::log(&format!("fkdata example: fastest belt is {best_name}"));
}

#[no_mangle]
pub extern "C" fn fk_data_final_fixes() {
    fkdata::log(&format!("fkdata example: {} stage", fkdata::stage().name()));

    // The absence question, which is the one case the ABI answers with a status
    // rather than a raise: has anybody else already defined this?
    if fkdata::get(&[P::S("item"), P::S(&name("token"))]).is_none() {
        fkdata::extend(&[obj(&[
            ("type", str_("item")),
            ("name", str_(&name("token"))),
            ("icon", str_("__core__/graphics/empty.png")),
            ("icon_size", num(1.0)),
            ("stack_size", num(42.0)),
            ("flags", arr(&[])),
        ])]);
    }
}

/// The version branch every real data stage has.
fn base_version() -> String {
    fkdata::mod_version("base").unwrap_or_else(|| String::from("absent"))
}
