package factorio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// The ABI SIGNATURE: a generated binding set says, in the compiled guest, WHICH
// BINDINGS compiled it -- not merely which description they came from.
//
// WHAT THIS IS FOR, and it is the half the pin stamp cannot reach. The pin
// proves the guest and the packaged table came from one DESCRIPTION. It cannot
// prove they came from one GENERATION, and at one pin the ids move whenever the
// generator grows: a member kind added, an operator's write half emitted, three
// global functions appended, a handle variant over an attribute. So a wasm built
// against older bindings and packaged with a fresh member table at the SAME pin
// passes every check there is, and every id in it resolves to a different
// member. Reported by BetterBeltBalancer (FKLUA-GAPS item 18); the first symptom
// is in a player's game.
//
// WHAT IS DIGESTED IS THE PACKAGED TABLE ITSELF -- for every member its id,
// class, name, kind and both blocks' rendered layout; for every event its id,
// name, size and layout; for every define its id and dotted path. Not the
// generated SOURCE, which differs between the two languages and would need three
// digests where the thing that has to match is one. This is exactly the pairing:
// what the guest's baked-in ids mean, and what the host will make them mean.
//
// LANGUAGE-INDEPENDENT BY CONSTRUCTION, so a Go guest and a Rust guest generated
// from one description carry the SAME stamp, and a project with both cannot have
// half of it stale. One function, three callers -- both generators and the
// packager -- which is PinExport's own argument: two places computing one digest
// would disagree silently, and a checker that computed a different one would
// find no match and stay quiet.
//
// A WARNING RATHER THAN A REFUSAL, and the reason is that this digest is
// CONSERVATIVE IN THE WRONG DIRECTION. A generator change that only APPENDS
// members leaves every existing id meaning exactly what it meant -- the three
// global functions were appended after every class precisely so that they would
// -- and a whole-table digest cannot tell that from a renumbering. Refusing
// would stop builds that are correct, which is the failure mode checkAPIPin's
// silence-on-absent rule exists to avoid, and this repo's standing rule that a
// check whose repair cannot be run from the consumer's checkout gets reverted
// rather than satisfied. The pin stamp keeps refusing the case that is ALWAYS
// wrong; this one names the case that MAY be.
//
// AN ABSENT STAMP STAYS QUIET, exactly as an absent pin does: bindings older
// than this carry none, and a guest that links no generated bindings carries
// none either.

// SigExportPrefix is what every signature export name begins with.
//
// It does NOT begin with PinExportPrefix, deliberately: GuestPins scans for that
// prefix and a signature sharing it would be read as a second pin stamp and
// refuse the package outright.
const SigExportPrefix = "fk_api_sig_"

// SigExport is the export name a binding set with this signature carries.
func SigExport(sig string) string { return SigExportPrefix + sig }

// APISignature digests the ID ASSIGNMENT AND LAYOUT one description plus this
// generator produce: the pairing a guest's baked-in ids are only meaningful
// against.
//
// TRUNCATED TO 12 HEX CHARACTERS, which is 48 bits. The failure this guards is a
// stale pair rather than an adversary, so what matters is that an unrelated
// generation is overwhelmingly unlikely to collide, and 48 bits is far past that
// for a space whose whole population is the generations of one compiler. What it
// buys is an export name short enough to read in a refusal.
func APISignature(a *API) string {
	r := GenerateMembers(a)
	ev := GenerateEvents(a)
	defs := GenerateDefines(a)

	h := sha256.New()
	fmt.Fprintf(h, "fklua/api-sig/v1\x00%s\x00%d\n", a.ApplicationVersion, a.APIVersion)
	for _, m := range r.Members {
		args, rets, err := m.blocks()
		if err != nil {
			// A member whose layout does not compute is one LuaSourceWith would
			// refuse to render, so the packaged table cannot exist either. Fold
			// the error in rather than dropping the member: two different broken
			// generations must not digest the same.
			fmt.Fprintf(h, "%d\t%s\t%s\t%d\tERR %v\n", m.ID, m.Class, m.Name, m.Kind, err)
			continue
		}
		fmt.Fprintf(h, "%d\t%s\t%s\t%d\t%s\t%s\n",
			m.ID, m.Class, m.Name, m.Kind, args.LuaTable(), rets.LuaTable())
	}
	for _, e := range ev.Events {
		blk, err := LayoutStruct(e.Fields)
		if err != nil {
			fmt.Fprintf(h, "e%d\t%s\tERR %v\n", e.ID, e.Name, err)
			continue
		}
		fmt.Fprintf(h, "e%d\t%s\t%d\t%s\n", e.ID, e.Name, blk.Size, blk.LuaTable())
	}
	// The hook payload is in the packaged table too, and its layout can move
	// without any member id moving -- which is precisely the class of change
	// this digest exists to notice.
	if ev.ConfChanged != nil {
		if blk, err := LayoutStruct(ev.ConfChanged); err == nil {
			fmt.Fprintf(h, "h\t%s\t%d\t%s\n", ConfChangedConcept, blk.Size, blk.LuaTable())
		}
	}
	for _, d := range defs.Defines {
		fmt.Fprintf(h, "d%d\t%s\n", d.ID, d.Path)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// GuestSigs reports the signature export names a compiled guest carries, sorted.
//
// Zero results means UNPROVEN, not matched. See the header above: an absent
// stamp is silence.
func GuestSigs(m *ir.Module) []string {
	var out []string
	for _, e := range m.Exports {
		if strings.HasPrefix(e.Name, SigExportPrefix) {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}
