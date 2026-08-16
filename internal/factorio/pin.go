package factorio

import (
	"sort"
	"strings"

	"github.com/Techrocket9/fklua/internal/ir"
)

// The PIN STAMP: a generated binding set says, in the compiled guest, which
// description its ids were assigned over.
//
// WHAT THIS IS FOR. Member, event and define ids are dense sorted indices over
// ONE description's set, so a member added or removed anywhere shifts every
// later id. The table `fklua mod` packages and the bindings the guest was
// compiled against are therefore only meaningful as a PAIR -- gen.go says so,
// and agents/versioning.md says so -- and until this existed nothing could
// check the pair, because the two halves are produced by different commands at
// different times and both succeed on their own. `fklua mod` knew the pin it
// was packaging at and had no way to ask the guest which pin it was built
// against; the mismatch surfaced as the guest calling entirely different
// members, silently wherever the kinds line up, in a lockstep game.
//
// The instance that produced this: a downstream mod pinned 2.1.14 and linked a
// vendored FkLua checkout whose COMMITTED bindings were still at the default
// 2.0.77, because the library packages that live inside the FkLua guest module
// -- fkipc, notably -- import that module's own fkapi rather than the
// consumer's. fkipc subscribed to event 207 believing it was
// on_udp_packet_received and got on_train_changed_state; it read
// helpers.game_version and got LuaForce.object_name, so its engine-floor gate
// parsed "0.0.0" and the library went inert. The only symptom anywhere was one
// log line about a version.
//
// WHY AN EXPORT NAME, and not a constant the existing scan in used.go proves.
// used.go recovers ids from CALLS to fk.call/fk.subscribe/fk.define, and a call
// only exists in the module if something reaches it: a stamp shaped like a call
// would be dead code in every binding set, and the toolchains delete it.
// Measured, not assumed -- TinyGo runs -opt=2 and then wasm-opt, and the Rust
// profile is lto = true. An EXPORT is a root by definition: it is the module's
// ABI surface, the one thing no optimizer may drop or rewrite. Both toolchains
// carry one out of a dependency (guest/go/fkapi is a package of the guest
// module, guest/rust/fkapi an rlib) with no reference from the guest at all.
//
// WHY THE NAME CARRIES THE VERSION, and not a returned constant. Reading a name
// needs no code analysis and cannot be defeated by an optimizer rewriting a
// body; a version is a string and a wasm result is a number, so a body would
// have to encode one and this does not. It also means a guest linking TWO
// generated binding sets carries TWO stamps and says so, which is the shape the
// downstream instance above actually had in its repository.
//
// WHAT IT DOES NOT COVER. The stamp names the version, so it cannot see a
// description EDITED UNDER A COMMITTED VERSION DIRECTORY -- ids would move with
// the version string standing still. That is `fklua lock --check`'s
// `api_sha256`, which is where it belongs: the lock is about the source tree
// and this is about the compiled artifact.

// PinExportPrefix is what every stamp export name begins with. Nothing else in
// the ABI starts with it, so the scan below needs no other disambiguation.
const PinExportPrefix = "fk_api_pin_"

// PinExport is the export name a binding set generated from `version` carries.
//
// ONE FUNCTION, THREE CALLERS -- both generators and the packager's check --
// because two places that spell one name are this repo's most-repeated failure
// shape, and here they would fail SILENTLY: a checker that mangled differently
// from a generator would find no stamp and stay quiet, which is exactly the
// behaviour it exists to replace.
//
// A wasm export name is an arbitrary UTF-8 string, but the two spellings that
// have to produce it are not: `//go:wasmexport` and Rust's `#[no_mangle]` both
// want something identifier-shaped, so every character outside [0-9A-Za-z]
// becomes '_'. THE MANGLING IS NOT INJECTIVE and nothing depends on it being:
// both sides mangle, so the COMPARISON is always exact, and the only thing that
// reads a stamp back is the refusal message, which recovers the real version by
// mangling the committed version names and matching rather than by unmangling.
func PinExport(version string) string {
	var b strings.Builder
	b.WriteString(PinExportPrefix)
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// GuestPins reports the stamp export names a compiled guest carries, sorted.
//
// Sorted rather than in module order because the result reaches a refusal
// message, and a message whose wording depends on section order is one that
// reads differently for the same defect on two builds.
//
// Zero results means UNPROVEN, not matched: a guest built against bindings
// generated before the stamp existed carries none, and so does one that links
// no generated bindings at all. The caller must treat that as silence -- see
// checkAPIPin in cmd/fklua, and the compatibility argument with it.
func GuestPins(m *ir.Module) []string {
	var out []string
	for _, e := range m.Exports {
		if strings.HasPrefix(e.Name, PinExportPrefix) {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}
