//! THE OUTBOUND SEAM CARRIES NO RETURN VALUE, AND THAT IS A TEXT PROPERTY OR IT
//! IS NOTHING.
//!
//! `a_failed_send_is_invisible_to_guest_state` in tests/link.rs proves the state
//! machine as it stands does not branch on a send's outcome. It cannot prove the
//! NEXT edit will not: "so these count what this link attempted" is a comment,
//! and this repo has watched a comment lose to a plausible-looking change more
//! than once -- the dead loop-guard seed, the missed page mark, and the
//! send-status counters that desynced a multiplayer join in the first place.
//!
//! So the guard is on the DECLARATION. `Transport::send`,
//! `Transport::write_file` and `Transport::log` return `()`, and the
//! `UdpTransport` methods that implement them on the game target return `()` --
//! which means no future edit anywhere in this crate can write
//! `if tr.send(f) == Status::Ok`, because there is no value to compare. The
//! compiler holds the rule; this file holds the compiler to it.
//!
//! LOG IS IN THE LIST AND IS THE MOST TEMPTING OF THE THREE. It is the one
//! sanctioned per-peer sink -- the game log is not CRC'd, which is the whole
//! reason it may carry a fact about how this peer was launched -- so a value
//! coming BACK out of it would be that same per-peer fact re-entering guest
//! state through the door built to keep it out. It arrived with the engine
//! gate's one refusal line; it is unit from the day it existed and it stays
//! unit.
//!
//! Whether `send_udp` or `write_file` succeeds is a fact about THIS PEER'S
//! COMMAND LINE (`--enable-lua-udp` binds the socket; a joining graphical client
//! has no such flag), and under `--persist=table` guest memory IS
//! `storage.fk_mem`, which Factorio CRCs across every peer. See `agents/ipc.md`,
//! "The rule the cost model implies", and CLAUDE.md's determinism rule.
//!
//! `Status` is deliberately NOT swept away with them: it still answers the
//! DETERMINISTIC refusals -- `QueueFull`, `TooLarge`, `NotOpen`, `NoSession`,
//! `NoTransport`, `Disabled` -- each of which is a function of guest state
//! alone and therefore the same answer on every peer. That is the whole
//! classification, and it is why this file names three methods rather than a
//! file.
//!
//! THE TWO HALVES BELOW ARE ENFORCED BY DIFFERENT THINGS, which is why both are
//! here. Putting a return type back on the TRAIT does not reach this assertion
//! at all -- it fails to compile, because `TestTransport` in tests/harness
//! implements it -- and that is the stronger guard. The wasm arm is the weak
//! one: `transport_guest` is behind `#[cfg(target_family = "wasm")]`, so
//! `cargo test -p fkipc` never compiles it against the trait, and a return type
//! put back there compiles clean on the target and is caught by nothing else.

/// The signature scan. Finds `fn <name>(` at any indentation, walks the
/// parameter list balancing parentheses (so an `&[u8]` or a `&mut dyn FnMut(..)`
/// does not fool it), then reports what follows: a `;` is a trait declaration
/// with no return, a `{` is a body with no return, and anything else is a `->`.
fn returns_nothing(src: &str, name: &str) -> Option<bool> {
    let needle = alloc_needle(name);
    let at = src.find(&needle)?;
    let bytes = src.as_bytes();
    let mut i = at + needle.len() - 1; // on the '('
    let mut depth = 0usize;
    while i < bytes.len() {
        match bytes[i] {
            b'(' => depth += 1,
            b')' => {
                depth -= 1;
                if depth == 0 {
                    break;
                }
            }
            _ => {}
        }
        i += 1;
    }
    if depth != 0 {
        return None;
    }
    i += 1;
    while i < bytes.len() && (bytes[i] as char).is_whitespace() {
        i += 1;
    }
    Some(matches!(bytes.get(i), Some(b';') | Some(b'{')))
}

fn alloc_needle(name: &str) -> String {
    let mut s = String::from("fn ");
    s.push_str(name);
    s.push('(');
    s
}

fn read(rel: &str) -> String {
    let p = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(rel);
    std::fs::read_to_string(&p).unwrap_or_else(|e| panic!("reading {}: {e}", p.display()))
}

#[test]
fn the_outbound_transport_seam_has_no_return_value() {
    // The seam itself. A method here with a return type is a value the link is
    // free to read, whatever the implementations do.
    let seam = read("src/transport.rs");
    assert!(
        seam.contains("pub trait Transport"),
        "the Transport trait was not found in src/transport.rs -- this test is \
         looking at the wrong thing, which is worse than it failing"
    );

    // ...and the implementation that actually runs in the game, which
    // `cargo test` never type-checks against the trait above.
    let guest = read("src/transport_guest.rs");
    assert!(
        guest.contains("impl Transport for UdpTransport"),
        "the wasm transport's impl block was not found in src/transport_guest.rs"
    );

    for (what, src) in [
        ("the Transport trait", &seam),
        ("the wasm transport", &guest),
    ] {
        for name in ["send", "write_file", "log"] {
            match returns_nothing(src, name) {
                Some(true) => {}
                Some(false) => panic!(
                    "{what}'s `{name}` declares a return type. AN OUTBOUND CALL'S \
                     OUTCOME IS PER-PEER, so a value there is a desync one `if` \
                     away: it is what `if tr.send(f) == Status::Ok \
                     {{ tx_frames += 1 }} else {{ queue_drops += 1 }}` was, and \
                     that shipped and desynced a joining client on the first \
                     tick it simulated. Keep the deterministic refusals in \
                     Status and leave this a unit return."
                ),
                None => panic!(
                    "{what} declares no `{name}` -- if it was renamed, rename it \
                     here too. The property is about the outbound half, not \
                     about a spelling, and not finding it is a failure rather \
                     than a skip."
                ),
            }
        }
    }
}
