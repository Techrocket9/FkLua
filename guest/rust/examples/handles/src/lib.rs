//! The Rust half of the HANDLE OWNERSHIP surface, end to end.
//!
//! A line-for-line mirror of `guest/go/examples/handles` down to the marker in
//! `fk_on_init`, so one host stub drives both and one expectation covers the
//! shared part. The predicates are the same three questions in both languages;
//! the OWNERSHIP guard is the asymmetry, and this is where it is exercised:
//! `Object::retained` hands back a `Retained` that releases on `Drop`, where the
//! Go twin writes `defer o.Release()`.
//!
//! WHAT COMES AFTER THE MARKER HAS NO GO TWIN, and saying so is better than
//! pretending otherwise: `retained()` returns `Option<Retained>` and refuses
//! every handle a guard cannot own, which is an API with nothing on the Go side
//! to mirror it. Those lines and `fk_on_tick` are asserted on the Rust arm
//! alone.
//!
//! WHY THE SLOT NUMBERS ARE THE ASSERTION. There is no accessor for the size of
//! the host's handle table, and adding one would be a new import for a test's
//! convenience. The slot index a retain hands back is a deterministic proxy:
//! the persistent free list is LIFO during play, so a released slot is the very
//! next one handed out, and a release that did not happen shows up as the next
//! retain taking a NEW number instead.

#![no_std]

extern crate alloc;

use alloc::format;
use alloc::string::String;
use core::cell::UnsafeCell;

use fkapi::{LuaSurface, Object, Retained, Value};

/// Hands back a fresh TRANSIENT handle on every call.
///
/// A separate call rather than one handle used several times, and that is what
/// makes the slot arithmetic below mean anything: each call wraps the object in
/// its own transient handle, so each retain allocates its own persistent slot.
fn surface() -> Object {
    match fkapi::GAME.get_surface(&Value::Number(1.0)) {
        Ok(Some(o)) => o,
        _ => {
            fk::log("handles: game.get_surface(1) failed, so there is nothing to retain");
            Object::default()
        }
    }
}

fn yn(b: bool) -> &'static str {
    if b {
        "true"
    } else {
        "false"
    }
}

/// Names the one space a handle is in. The three predicates partition every
/// number, so exactly one arm can match and "none" is the null handle.
fn class(o: Object) -> &'static str {
    if o.is_global() {
        "global"
    } else if o.is_persistent() {
        "persistent"
    } else if o.is_transient() {
        "transient"
    } else {
        "none"
    }
}

fn predicates(o: Object) -> String {
    format!(
        "persistent={} transient={} global={}",
        yn(o.is_persistent()),
        yn(o.is_transient()),
        yn(o.is_global())
    )
}

/// The guard, doing what the Go twin's `defer o.Release()` does: the slot is
/// freed when the scope ends, on every path out of it.
fn guard_slot() -> u32 {
    // `retained()` takes a TRANSIENT handle and mints a slot of its own, which
    // is the only way a guard is ever born; `None` here would mean `surface()`
    // handed back nothing to promote, and 0 is not a slot, so the transcript
    // moves rather than the failure being swallowed.
    match surface().retained() {
        // `*guard` through Deref is a COPY of the handle and a BORROW of the
        // guard's slot: fine to read, fine to pass to a call, and it cannot
        // become a second owner, because `retained()` refuses a handle that is
        // already persistent. `into_object` is still the only way to take the
        // slot OUT of the guard.
        Some(guard) => (*guard).0,
        None => 0,
    }
}

/// A guard parked in a static, which is what the type's ACROSS A SAVE paragraph
/// is about: the guest heap is what Factorio serializes, so the `u32` inside the
/// guard comes back, and the host's persistent table came back beside it.
///
/// `fk_on_tick` reads it in the SECOND session, after `M.adopt` has rebuilt the
/// free list from the saved table, and calls something on it. A number that
/// resolves to nothing would still be a number.
struct Parked(UnsafeCell<Option<Retained>>);
unsafe impl Sync for Parked {}
static PARKED: Parked = Parked(UnsafeCell::new(None));

#[allow(clippy::mut_from_ref)]
fn parked() -> &'static mut Option<Retained> {
    unsafe { &mut *PARKED.0.get() }
}

fn parked_slot() -> u32 {
    match parked() {
        Some(g) => (**g).0,
        None => 0,
    }
}

#[no_mangle]
pub extern "C" fn fk_on_init() {
    // BOTH SPLIT CONSTANTS, asked of hand-built handles rather than of whatever
    // the host happened to hand out. 9 and 10 straddle the global boundary and
    // 1073741823 and 1073741824 straddle the transient one, so a constant that
    // is off by one in either direction moves this line.
    let mut line = String::from("handles: classify");
    for h in [0u32, 1, 9, 10, 1073741823, 1073741824, 4294967295] {
        line.push_str(&format!(" {}={}", h, class(Object(h))));
    }
    fk::log(&line);

    // What the API hands back is transient. Nothing here asserts WHICH
    // transient number it is: the space is the claim.
    let a = surface();
    fk::log(&format!("handles: fresh {}", predicates(a)));

    // ...and a retain moves it into the persistent space, at the first slot.
    let r = a.retain();
    fk::log(&format!("handles: retained slot={} {}", r.0, predicates(r)));

    // IDEMPOTENCE. Retaining a handle that is already persistent hands the same
    // number back rather than allocating a second slot onto the same object, so
    // a second retain does not LEAK. It buys no ownership either: there is still
    // one slot, and the release that pairs with the second retain frees whatever
    // the next retain took. Release a slot exactly once -- which is what the
    // guard below does for you, and why retained() refuses a persistent handle.
    fk::log(&format!("handles: idempotent slot={}", r.retain().0));

    // The guard's slot, freed by the time this line is logged: guard_slot's
    // Retained went out of scope with the function.
    let g = guard_slot();
    fk::log(&format!("handles: guard slot={}", g));

    // ...and the next retain gets it back. THIS is Drop, observed.
    let b = surface().retain();
    fk::log(&format!("handles: after release slot={}", b.0));

    // The other half: into_object takes the handle OUT of the guard without
    // releasing it, for a caller that means to manage the pair itself.
    let kept = match surface().retained() {
        Some(guard) => guard.into_object(),
        None => Object::default(),
    };
    fk::log(&format!("handles: kept slot={}", kept.0));

    // So the next retain takes a NEW slot, because nothing was freed.
    let c = surface().retain();
    fk::log(&format!("handles: after keep slot={}", c.0));

    // A global is not a slot this guest owns: releasing one is Status::BAD_HANDLE.
    fk::log(&format!("handles: global {}", predicates(fkapi::GAME.0)));

    // And the null handle is in no space at all.
    fk::log(&format!("handles: null {}", predicates(Object::default())));

    // ------------------------------------------------------------------
    // FROM HERE DOWN IS RUST ONLY, because the guard is. See the module doc.
    // ------------------------------------------------------------------

    // THE FOUR REFUSALS, each one a shape `retained()` used to accept.
    //
    // `kept` is a slot somebody already owns by hand; a global is owned by
    // nobody and its release is BAD_HANDLE; the null handle names nothing; and
    // the last is a retain that FAILS -- a transient number the host never
    // handed out -- which used to hand back a guard over `Object(0)`, the third
    // leak in the report rebuilt inside the new API. None of the four
    // allocates, so nothing below moves because of this line.
    fk::log(&format!(
        "guard: refuses owned={} global={} null={} failed={}",
        yn(kept.retained().is_none()),
        yn(fkapi::GAME.0.retained().is_none()),
        yn(Object::default().retained().is_none()),
        yn(Object(4294967295).retained().is_none())
    ));

    // THE ABA, AND WHY IT IS NOW UNREPRESENTABLE.
    //
    // `Retained::new(*guard)` used to compile and hand back a SECOND guard over
    // ONE slot, because `Object` is `Copy`, `Deref` gives one out, and the
    // host's retain is idempotent for a handle already persistent. What
    // followed was not a benign double release: guard A drops, the free list is
    // LIFO so the slot goes to an unrelated guard C on the very next retain,
    // and B's drop then frees the slot C owns -- two live owners naming one
    // slot with no status anywhere.
    //
    // Both spellings answer None now, so C keeps its slot and can still be
    // called on. The host-side probe that shows what the release pair does when
    // that None is bypassed is in internal/factorio.
    if let Some(a) = surface().retained() {
        let slot = (*a).0;
        fk::log(&format!("aba: A owns slot={}", slot));
        fk::log(&format!(
            "aba: second guard over A new={} retained={}",
            yn(Retained::new(*a).is_some()),
            yn((*a).retained().is_some())
        ));
        drop(a);
        match surface().retained() {
            Some(c) => {
                let nauvis = match LuaSurface(*c).name_is("nauvis") {
                    Ok(v) => yn(v),
                    Err(_) => "error",
                };
                fk::log(&format!(
                    "aba: C owns slot={} reused={} nauvis={}",
                    (*c).0,
                    yn((*c).0 == slot),
                    nauvis
                ));
            }
            None => fk::log("aba: C was not promoted"),
        }
    } else {
        fk::log("aba: A was not promoted");
    }

    // PARKED FOR THE SECOND SESSION. fk_on_tick reads it back after a load.
    *parked() = surface().retained();
    fk::log(&format!("save: parked slot={}", parked_slot()));
}

/// The second session's half of the ACROSS A SAVE claim.
///
/// Nothing here re-retains: the guard is the one `fk_on_init` parked, carried in
/// the guest heap, and the slot it names is resolved against a handle table
/// `M.adopt` rebuilt from the save.
#[no_mangle]
pub extern "C" fn fk_on_tick(_tick: u32) {
    let nauvis = match parked() {
        Some(g) => match LuaSurface(**g).name_is("nauvis") {
            Ok(v) => yn(v),
            Err(_) => "error",
        },
        None => "gone",
    };
    fk::log(&format!(
        "save: guard slot={} nauvis={}",
        parked_slot(),
        nauvis
    ));
}
