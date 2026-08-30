//! The Rust half of the TYPED ARGUMENT form, end to end.
//!
//! A line-for-line mirror of `guest/go/examples/typedargs`, so the SAME host
//! stub drives both and the two transcripts are compared against one
//! expectation. It exists for the reason every mirror here exists: the two
//! generators are separate code and this repo has already run the
//! single-language experiment.
//!
//! What each leg asserts is in the Go file; the short form is that the same GUI
//! element spec is built as a tier-2 map and as a typed block, and the stub must
//! render the table it was handed identically both times.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::vec;

use alloc::string::String;

use fkapi::{
    LuaEntity, LuaGui, LuaGuiElement, LuaGuiElementAddArgs, LuaPlayer, LuaStr, LuaSurface,
    LuaSurfaceCreateEntityArgs, MapPosition, Object, Value, GAME,
};

#[no_mangle]
pub extern "C" fn fk_on_init() {
    gui_legs();
    entity_legs();
}

/// The GUI legs, which no HEADLESS run can reach: LuaGuiElement::add needs a
/// player. See the Go file.
fn gui_legs() {
    let p = match GAME.get_player(&Value::Number(1.0)) {
        Ok(Some(p)) => p,
        Ok(None) => {
            fk::log("gui: no player");
            return;
        }
        Err(e) => {
            fk::log(&format!("typedargs: {}", e.as_str()));
            return;
        }
    };
    let g = match LuaPlayer(p).gui() {
        Ok(g) => g,
        Err(e) => {
            fk::log(&format!("typedargs: {}", e.as_str()));
            return;
        }
    };
    let sc = match LuaGui(g).screen() {
        Ok(s) => s,
        Err(e) => {
            fk::log(&format!("typedargs: {}", e.as_str()));
            return;
        }
    };
    let screen = LuaGuiElement(sc);

    // LEG 1 -- THE DYN FORM.
    fk::log("leg dyn");
    let spec = Value::Map(vec![
        (Value::Str(LuaStr::from("type")), Value::Str(LuaStr::from("button"))),
        (Value::Str(LuaStr::from("name")), Value::Str(LuaStr::from("row-7"))),
        (
            Value::Str(LuaStr::from("caption")),
            Value::Str(LuaStr::from("Launch")),
        ),
        (
            Value::Str(LuaStr::from("style")),
            Value::Str(LuaStr::from("green_button")),
        ),
        (Value::Str(LuaStr::from("enabled")), Value::Bool(true)),
    ]);
    if let Err(e) = screen.add(&spec) {
        fk::log(&format!("typedargs: dyn add: {}", e.as_str()));
        return;
    }

    // LEG 2 -- THE SAME SPEC, TYPED.
    fk::log("leg typed");
    let args = LuaGuiElementAddArgs {
        r#type: LuaStr::from("button"),
        name: Some(LuaStr::from("row-7")),
        caption: Some(Value::Str(LuaStr::from("Launch"))),
        style: Some(LuaStr::from("green_button")),
        enabled: Some(true),
        ..Default::default()
    };
    if let Err(e) = screen.add_typed(args, None) {
        fk::log(&format!("typedargs: typed add: {}", e.as_str()));
        return;
    }

    // LEG 3 -- THE VARIANT TAIL.
    fk::log("leg tail");
    let tail = Value::Map(vec![
        (
            Value::Str(LuaStr::from("sprite")),
            Value::Str(LuaStr::from("item/iron-plate")),
        ),
        (Value::Str(LuaStr::from("number")), Value::Number(42.0)),
    ]);
    let args = LuaGuiElementAddArgs {
        r#type: LuaStr::from("sprite-button"),
        name: Some(LuaStr::from("icon")),
        ..Default::default()
    };
    if let Err(e) = screen.add_typed(args, Some(&tail)) {
        fk::log(&format!("typedargs: tail add: {}", e.as_str()));
        return;
    }

    // LEG 4 -- THE TAIL OVERRIDES THE BLOCK.
    fk::log("leg override");
    let tail = Value::Map(vec![(
        Value::Str(LuaStr::from("name")),
        Value::Str(LuaStr::from("tail-said-this")),
    )]);
    let args = LuaGuiElementAddArgs {
        r#type: LuaStr::from("label"),
        name: Some(LuaStr::from("block-said-this")),
        ..Default::default()
    };
    if let Err(e) = screen.add_typed(args, Some(&tail)) {
        fk::log(&format!("typedargs: override add: {}", e.as_str()));
        return;
    }

    // LEG 5 -- AN ABSENT OPTIONAL IS ABSENT.
    fk::log("leg minimal");
    let args = LuaGuiElementAddArgs {
        r#type: LuaStr::from("flow"),
        ..Default::default()
    };
    if let Err(e) = screen.add_typed(args, None) {
        fk::log(&format!("typedargs: minimal add: {}", e.as_str()));
        return;
    }
    fk::log("gui done");
}

/// The entity legs, which a headless run CAN reach. See the Go file.
fn entity_legs() {
    let s = match GAME.get_surface(&Value::Number(1.0)) {
        Ok(Some(s)) => s,
        Ok(None) => {
            fk::log("entity: no surface");
            return;
        }
        Err(e) => {
            fk::log(&format!("typedargs: {}", e.as_str()));
            return;
        }
    };
    let surf = LuaSurface(s);

    let spec = Value::Map(vec![
        (
            Value::Str(LuaStr::from("name")),
            Value::Str(LuaStr::from("iron-chest")),
        ),
        (
            Value::Str(LuaStr::from("position")),
            Value::Array(vec![Value::Number(8.0), Value::Number(8.0)]),
        ),
    ]);
    match surf.create_entity(&spec) {
        Ok(e) => fk::log(&format!("entity dyn: {}", name_of(e))),
        Err(e) => {
            fk::log(&format!("entity: dyn create: {}", e.as_str()));
            return;
        }
    }

    let args = LuaSurfaceCreateEntityArgs {
        name: Value::Str(LuaStr::from("iron-chest")),
        position: MapPosition { x: 12.0, y: 8.0 },
        ..Default::default()
    };
    match surf.create_entity_typed(args, None) {
        Ok(e) => fk::log(&format!("entity typed: {}", name_of(e))),
        Err(e) => {
            fk::log(&format!("entity: typed create: {}", e.as_str()));
            return;
        }
    }
    fk::log("done");
}

/// One more typed call, on a tick, so a headless run has something to compare
/// between its two replays. See the Go file: exporting fk_on_tick IS the
/// subscription, and the tick number is what gates the work.
#[no_mangle]
pub extern "C" fn fk_on_tick() {
    if GAME.tick() != Ok(1) {
        return;
    }
    let s = match GAME.get_surface(&Value::Number(1.0)) {
        Ok(Some(s)) => s,
        _ => return,
    };
    let args = LuaSurfaceCreateEntityArgs {
        name: Value::Str(LuaStr::from("iron-chest")),
        position: MapPosition { x: 16.0, y: 8.0 },
        ..Default::default()
    };
    match LuaSurface(s).create_entity_typed(args, None) {
        Ok(e) => fk::log(&format!("tick typed: {}", name_of(e))),
        Err(e) => fk::log(&format!("tick typed: {}", e.as_str())),
    }
}

/// Reads the entity back through the API rather than trusting the handle.
fn name_of(o: Option<Object>) -> String {
    match o {
        None => String::from("<none>"),
        Some(h) => match LuaEntity(h).name() {
            Ok(n) => match core::str::from_utf8(n.as_bytes()) {
                Ok(s) => String::from(s),
                Err(_) => String::from("?"),
            },
            Err(e) => format!("<{}>", e.as_str()),
        },
    }
}
