// Command databytes is the data stage's BYTE-EXACTNESS fixture: a Go program
// that reads two kinds of string out of the stage, logs each one's length and
// bytes, and writes both back through the two write primitives.
//
// It exists because a data-stage string is an arbitrary byte sequence and the
// two guest libraries have to agree about that to the byte. The engine carries
// invalid UTF-8 and control characters through mod-settings.dat unchanged, so a
// stored setting value is where a guest meets one for real; a prototype field
// another mod wrote is the other place, and a prototype NAME another mod wrote
// is the third. (A NUL truncates the stored value inside the engine, which is
// the engine's behaviour and not this ABI's; the NUL here is driving
// fk_data.lua, which is the ABI.) The Rust half used to decode both
// with String::from_utf8_lossy, which rewrote every byte outside UTF-8 into
// U+FFFD and changed the length while it did it, so the two halves saw
// different strings for one wire. Nothing in this repo's corpus could see that:
// every string in the datastage example is a prototype name, which is ASCII by
// engine constraint.
//
// IT IS ALSO THE RUST MIRROR'S TWIN. guest/rust/examples/databytes is a
// line-for-line port, and one test drives both through one fk_data.lua against
// one stand-in and requires identical transcripts out of each.
//
// Each value is logged as <len>:<hex>, because a lossy reader gets the LENGTH
// wrong as well as the contents and a check that asserted only one of them
// could have passed.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o databytes.wasm ./examples/databytes
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fkdata"
)

// The mod's own prefix, so nothing here can collide with a real mod.
const prefix = "fkb-"

// The prototype this reads its fields out of and writes its echoes back into.
const (
	probeType = prefix + "probe"
	probeName = prefix + "source"
)

// The prototype type this ENUMERATES. Its prototype names hold bytes outside
// UTF-8, because a name is a key in a table any mod can write and Keys is the
// one surface that hands back a whole set of them.
const keyedType = prefix + "keyed"

// The three shapes a byte string comes in, named so the transcript says which
// one moved. In sorted order, which is also the order the host hands the
// settings map back in, so the log reads in one order rather than two.
var probes = []string{"all", "empty", "nul"}

//go:wasmexport fk_data
func onData() {
	echo := []fkdata.KV{
		fkdata.KVs("type", fkdata.Str(prefix+"echo")),
		fkdata.KVs("name", fkdata.Str(prefix+"echo-out")),
	}

	for _, p := range probes {
		// IN through env(3): a stored startup setting's value, which is the
		// one string at this stage that a player can put arbitrary bytes into.
		if s, ok := fkdata.StartupSetting(prefix + p); ok {
			fkdata.Log("databytes: setting " + p + " " + hexline(s.String()))
			// OUT through set: straight back into data.raw, unaltered. The
			// guest never looks at what it is carrying.
			fkdata.Set(fkdata.Str(s.String()), probeType, probeName, "echo-"+p)
		} else {
			fkdata.Log("databytes: setting " + p + " ABSENT")
		}

		// IN through get: a string FIELD of a prototype, which is a different
		// decode path over the same wire.
		if f, ok := fkdata.Get(probeType, probeName, p); ok {
			fkdata.Log("databytes: field " + p + " " + hexline(f.String()))
			// OUT through extend, below.
			echo = append(echo, fkdata.KVs(p, fkdata.Str(f.String())))
		} else {
			fkdata.Log("databytes: field " + p + " ABSENT")
		}
	}

	// IN through keys: the prototype NAMES at a type, sorted. A name is a key
	// in a global namespace any mod can write, so it is bytes like everything
	// else -- and this is the read path a fixture that only ever enumerated
	// ASCII names cannot hold to the contract.
	for _, k := range fkdata.Keys(keyedType) {
		fkdata.Log("databytes: key " + hexline(k))
		// And BACK IN as a path element: the name a mod wrote, reaching the
		// prototype that carries it. A Go string is bytes, so the path takes
		// it as it is; the Rust twin spells the same position as P::B.
		if v, ok := fkdata.Get(keyedType, k, "name"); ok {
			fkdata.Log("databytes: roundtrip " + hexline(v.String()))
		} else {
			fkdata.Log("databytes: roundtrip ABSENT")
		}
	}

	fkdata.Extend(fkdata.Obj(echo...))
}

// hexline renders a byte string as <len>:<hex>.
//
// A Go string is a byte string, so len(s) is the length the engine wrote and
// s[i] is the byte it wrote. The Rust twin says the same thing about a &[u8]
// out of V::bytes.
func hexline(s string) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, digits[s[i]>>4], digits[s[i]&0x0f])
	}
	return strconv.Itoa(len(s)) + ":" + string(out)
}
