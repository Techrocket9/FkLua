// Package wire is the fkipc frame codec: the 24-byte header, the nine frame
// types, and the four control payloads both ends must agree on byte for byte.
//
// # One codec, two consumers
//
// This package has no build tags, imports nothing outside the standard library
// and names nothing from fkapi, so the SAME source compiles into the guest
// under TinyGo and into an external tool under the host toolchain. Go builds
// per package, so a host program importing it never reaches fkapi's
// //go:wasmimport declarations one directory up.
//
// That is worth a package boundary rather than a copy in each module because
// this repo has already paid for the alternative twice: the Rust generator
// spent four milestones behind the Go one, and AD5 was the identical defect in
// the identical function, fixed on one backend and left standing on the other
// because the test was written against one backend. A frame format is exactly
// the kind of thing two copies drift on -- one side adds a field, both still
// pass their own tests, and the bug is a live session that silently stops
// parsing.
//
// # The rules that are absolute
//
// LITTLE-ENDIAN, read and written BYTE-WISE. wasm linear memory is
// little-endian by specification and every peer that will ever speak this is
// x86-64 or arm64, so network byte order would cost both ends a swap to please
// neither. The fields are laid out naturally aligned anyway, but nothing here
// takes an aligned load: an inbound payload lands wherever the host's string
// scratch pointer happens to stand, and a misaligned 4-byte load in generated
// Lua takes the checked slow arm. Six shifts against a ~12.5 us host call is
// not a cost worth designing around.
//
// LENGTH IS CARRIED even though a datagram already knows its own size, and the
// redundancy is the point: Length != len(datagram)-HeaderBytes is the cheapest
// detector there is for truncation, coalescing, and a peer that has
// desynchronised its idea of the format. [Decode] treats a mismatch as fatal
// to the frame.
//
// NO PARTIAL PARSE, EVER. Every function here either returns a wholly valid
// value or an error. A caller counts the error and drops the datagram; it never
// gets a header it can act on out of a frame that failed a later check.
//
// # What is deliberately not here
//
// MSG, REQ and RESP payloads are opaque. The protocol says nothing about their
// encoding, so nothing here encodes or decodes one -- requiring JSON would put
// an encoder in the hot path of an app that wanted a packed struct, and would
// make this package's tests depend on some JSON library's escaping rules.
// Control payloads are defined here because both ends must agree on them and
// there is nothing app-shaped about them.
package wire
