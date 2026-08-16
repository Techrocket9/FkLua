// Command dict is the end-to-end fixture for a DICTIONARY FIELD INSIDE A
// STRUCT, which is the shape five event payloads carry and the Go generator
// refused until 2026-08-02 -- and, since 2026-08-06, for A BINARY STRING
// through a generated reader.
//
// on_built_entity is the first event, and it is not an arbitrary choice: `tags`
// is what deferred it, and it plus on_robot_built_entity are what a mod that
// builds things subscribes to. A guest that could not read this payload as a
// struct read it at hand-derived byte offsets instead, which is silent when
// wrong.
//
// on_console_chat is the second, and it is here for its Message -- a plain
// mandatory STRING field on an event payload. This half is the LEVEL side of a
// backend asymmetry rather than a Go defect: getStr has always been string(b)
// and byte-exact, while the Rust reader was from_utf8_lossy, so the same wire
// read back differently in the two languages. The mirror is what proves it does
// not any more, which is why the Go guest prints the same hex.
//
// The compile gate cannot reach any of this. TinyGo removes every member a
// guest does not call, so it proves the decoder type-checks and stops there;
// a pair stride off by the key's padding, a value read from the key's offset,
// or a string reader that rewrites what it reads all live past the type checker
// and are only visible when the values come back.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

func init() {
	fkapi.Subscribe(fkapi.EventOnBuiltEntity)
	fkapi.Subscribe(fkapi.EventOnConsoleChat)
}

// tag looks an entry up by key rather than printing the slice in order.
//
// The host builds the tags table with `pairs`, whose order this ABI explicitly
// does not promise, so a test that printed the slice would be asserting on
// something nobody owes it. What IS owed is that every pair arrives intact and
// that the count is right, and this asserts exactly that.
func tag(e []fkapi.EntryStringValue, k string) string {
	for i := range e {
		if e[i].Key != k {
			continue
		}
		v := e[i].Val
		switch v.Tag {
		case fkapi.TagString:
			return "'" + v.Str + "'"
		case fkapi.TagNumber:
			return strconv.FormatFloat(v.Number, 'f', -1, 64)
		case fkapi.TagBool:
			return strconv.FormatBool(v.Bool)
		}
		return "?"
	}
	return "MISSING"
}

// tagHex reads a tag as BYTES rather than as text -- the tier-2 half of the
// same claim, since a tags value crosses as a tier-2 string while the Message
// below crosses as a struct field.
func tagHex(e []fkapi.EntryStringValue, k string) string {
	for i := range e {
		if e[i].Key != k {
			continue
		}
		if e[i].Val.Tag != fkapi.TagString {
			return "?"
		}
		s := e[i].Val.Str
		return strconv.Itoa(len(s)) + ":" + hex(s)
	}
	return "MISSING"
}

// hex is lower-case hex, hand-rolled so the guest pulls in nothing for it and
// so the two mirrors render bytes identically.
func hex(s string) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, d[s[i]>>4], d[s[i]&15])
	}
	return string(out)
}

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	if id == fkapi.EventOnConsoleChat {
		// A MANDATORY STRING FIELD, printed as bytes and as a length. Both
		// halves matter: the Rust side's lossy reader changed the length as
		// well as the contents, so a test asserting only one of them could
		// have passed.
		e := fkapi.ReadOnConsoleChat(ptr)
		fk.Log("chat: " + strconv.Itoa(len(e.Message)) + ":" + hex(e.Message))
		// ...AND STRAIGHT BACK OUT, which is the other direction of the same
		// claim: the host prints what it received, so the assertion is on
		// bytes that made a full round trip through both marshalling
		// directions.
		no := false
		if err := fkapi.Helpers.WriteFile("echo.bin", fkapi.OfString(e.Message), &no, nil); err != nil {
			fk.Log("write_file failed: " + err.Error())
		}
		return
	}
	e := fkapi.ReadOnBuiltEntity(ptr)
	fk.Log("tags: " + strconv.Itoa(len(e.Tags)) +
		" colour=" + tag(e.Tags, "colour") +
		" count=" + tag(e.Tags, "count") +
		" live=" + tag(e.Tags, "live") +
		" blob=" + tagHex(e.Tags, "blob"))
	// The scalar fields AFTER the dictionary in the layout. A dict field whose
	// (ptr, count) header were the wrong width would leave these reading
	// somebody else's bytes, and they are the half a guest actually acts on.
	fk.Log("player=" + strconv.FormatUint(uint64(e.PlayerIndex), 10) +
		" tick=" + strconv.FormatUint(e.Tick, 10))
	// And a handle from the same payload still resolves, which is what says the
	// fields before the dictionary did not move either.
	name, err := fkapi.LuaEntity{Object: e.Entity}.Name()
	if err != nil {
		fk.Log("entity: " + err.Error())
		return
	}
	fk.Log("entity=" + name)
}

func main() {}
