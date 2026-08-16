// Command array exercises the variable-length marshalling: arrays over every
// element shape, dictionaries, and tier-2 dynamic values, in both directions.
//
// It exists because the type-check gate cannot reach this code. TinyGo
// dead-code-eliminates every member a guest does not call, so a package that
// compiles proves the encoders parse and type-check and nothing more -- and the
// three bugs that got past the parse-only gate at M7 were all of the "compiles,
// does the wrong thing" kind. Only running it says the offsets, strides and the
// allocation bracket are right.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o array.wasm ./examples/array
package main

import (
	"strconv"
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

func u(n int) string { return strconv.FormatUint(uint64(n), 10) }

//go:wasmexport fk_on_init
func onInit() {
	// 1. An array of HANDLES coming back. 110 of the 215 array returns are
	//    this shape, which makes it the one worth checking first.
	players, err := fkapi.Game.ConnectedPlayers()
	if err != nil {
		fk.Log("connected_players failed: " + err.Error())
		return
	}
	fk.Log("handles: " + u(len(players)))
	if len(players) == 0 {
		fk.Log("no players, so the element loop never ran -- the rest would prove nothing")
		return
	}

	// A handle is a handle: the class a guest wraps it in decides which members
	// it can reach, and the host looks the member up by id either way. That is
	// what lets this reach three classes from one array.
	first := players[0]

	// 2. An array of STRINGS. The elements are (ptr, len) pairs the host wrote
	//    through fk_alloc, so this is the case where a wrong stride reads a
	//    pointer out of the middle of the previous element.
	seeds, err := fkapi.LuaEntityPrototype{Object: first}.AcceptedSeeds()
	if err != nil {
		fk.Log("accepted_seeds failed: " + err.Error())
	} else {
		out := "strings: " + u(len(seeds))
		for _, s := range seeds {
			out += " " + s
		}
		fk.Log(out)
	}

	// 2b. THE SAME OPTIONAL ARRAY, ABSENT. `accepted_seeds` is declared
	//     optional, so "no value" and "the empty list" are different answers and
	//     the API means them differently -- reported by fklua-ports'
	//     fuel-train-stop (FTS1) against both backends. Go says it with nil,
	//     because make([]T, 0) is guaranteed NON-nil, so `== nil` really is the
	//     absent test; Rust says it with Option, because a Vec has no absent
	//     value. Same host, same stub, same log line, two conventions.
	if bare, berr := (fkapi.LuaEntity{Object: first}).LastUser(); berr != nil {
		fk.Log("last_user failed: " + berr.Error())
	} else if bare == nil {
		fk.Log("optional array: no last_user")
	} else if none, nerr := (fkapi.LuaEntityPrototype{Object: *bare}).AcceptedSeeds(); nerr != nil {
		fk.Log("absent accepted_seeds failed: " + nerr.Error())
	} else if none == nil {
		fk.Log("optional array: absent")
	} else {
		fk.Log("optional array: present " + u(len(none)))
	}

	// 3. An array of STRUCTS, whose stride is the element's PADDED size rather
	//    than the sum of its fields. MapPosition is two f64s, so a stride bug
	//    here shows up as coordinates from the wrong element.
	dests, err := fkapi.LuaEntity{Object: first}.AutopilotDestinations()
	if err != nil {
		fk.Log("autopilot_destinations failed: " + err.Error())
	} else {
		out := "structs: " + u(len(dests))
		for _, p := range dests {
			out += " (" + strconv.FormatFloat(p.X, 'f', 1, 64) +
				"," + strconv.FormatFloat(p.Y, 'f', 1, 64) + ")"
		}
		fk.Log(out)
	}

	// 4. A DICTIONARY, which is the same walk over key/value pairs -- the pair
	//    is a two-field block, so the value's offset is the key's padded size
	//    rather than the key's width.
	//
	//    AN ORDERED SLICE OF PAIRS, NOT A GO MAP, since qol-research reported
	//    Q3: a Go map's iteration order is randomised per process and Factorio
	//    is lockstep, so a guest that walked one did host-visible work -- and
	//    ALLOCATED, which the save records -- in a different order on every
	//    client. Looked up by name below, which is the same three lines a map
	//    would have cost the generator and is now the guest's to write.
	fluids, err := fkapi.LuaEntity{Object: first}.GetFluidContents()
	if err != nil {
		fk.Log("get_fluid_contents failed: " + err.Error())
	} else {
		fluid := func(name string) float64 {
			for _, e := range fluids {
				if e.Key == name {
					return e.Val
				}
			}
			return -1
		}
		fk.Log("dict: " + u(len(fluids)) +
			" water=" + strconv.FormatFloat(fluid("water"), 'f', 1, 64) +
			" steam=" + strconv.FormatFloat(fluid("steam"), 'f', 1, 64))
	}

	// 5. And an array going OUT, which is the direction that allocates. The
	//    host reads it during the call and the buffer is freed on return.
	if err := (fkapi.LuaControl{Object: first}).
		SetCharacterAdditionalMiningCategories([]string{"basic-solid", "hard-solid"}); err != nil {
		fk.Log("set mining categories failed: " + err.Error())
	}

	// 6. Empty is not the same as absent, and it is the case a loop written
	//    against len-1 gets wrong. The host must see ptr=0, count=0.
	if err := (fkapi.LuaControl{Object: first}).
		SetCharacterAdditionalMiningCategories(nil); err != nil {
		fk.Log("set empty failed: " + err.Error())
	}

	// 7. TIER 2, coming back. One Go type for every union the API leaves open,
	//    tagged rather than typed, and nested arbitrarily deep.
	name, err := fkapi.LuaEntity{Object: first}.GhostLocalisedName()
	if err != nil {
		fk.Log("ghost_localised_name failed: " + err.Error())
	} else {
		fk.Log("dyn in: " + render(name))
	}

	// 8. And going out, nested: an array holding a string, a number and an
	//    inner array. The buffers for both arrays come from fk_alloc and are
	//    released by the bracket the binding opened, however deep they go.
	//
	//    The trailing OfNil is there to show what it costs: a nil inside a Lua
	//    sequence IS the end of that sequence, so the host receives [true] and
	//    there is nothing tier 2 could do about it. Encode an absent element as
	//    something Lua can hold -- false, or a sentinel string -- if it has to
	//    survive the crossing.
	err = (fkapi.LuaControl{Object: first}).SetCursorGhost(fkapi.OfArray(
		fkapi.OfString("item-name.iron-plate"),
		fkapi.OfNumber(42),
		fkapi.OfArray(fkapi.OfBool(true), fkapi.OfNil()),
	))
	if err != nil {
		fk.Log("set_cursor_ghost failed: " + err.Error())
	}

	// 9. An array field INSIDE a struct. Two of them here, at different
	//    offsets, which is what catches a decoder that reads both headers from
	//    the first field's slot.
	belts, err := fkapi.LuaEntity{Object: first}.BeltNeighbours()
	if err != nil {
		fk.Log("belt_neighbours failed: " + err.Error())
	} else {
		fk.Log("struct arrays: inputs=" + u(len(belts.Inputs)) +
			" outputs=" + u(len(belts.Outputs)))
	}

	// 10. A VARIANT PARAMETER GROUP method. create_entity's argument table is a
	//     discriminated union -- which fields are legal depends on `name` -- so
	//     it crosses as one tier-2 value rather than a generated struct per
	//     variant. This is the whole surface of a mod that builds things.
	ent, err := (fkapi.LuaSurface{Object: first}).CreateEntity(fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("iron-chest")},
		fkapi.KeyValue{Key: fkapi.OfString("force"), Val: fkapi.OfString("player")},
		fkapi.KeyValue{Key: fkapi.OfString("bar"), Val: fkapi.OfNumber(4)},
	))
	if err != nil {
		fk.Log("create_entity failed: " + err.Error())
	} else {
		fk.Log("create_entity returned " + boolStr(ent != nil))
	}

	// 11. THE DESTINATION-SLICE VARIANT of the same array returns. Same member
	//     ids, same blocks, same host calls -- the only difference is where the
	//     slice comes from, so what has to be checked is that the CONTENTS are
	//     the ones the allocating form returns and that a destination big
	//     enough is not reallocated. Either half alone passes for a variant
	//     that is wrong in the other way.
	//
	//     THE EQUALITY CHECK IS ON STRINGS AND NOT ON HANDLES, and that is not
	//     fussiness: every array of objects comes back as fresh TRANSIENT
	//     handles, so two calls to connected_players return different numbers
	//     for the same three players and comparing them would fail on correct
	//     code. Handles carry the reuse half, strings carry the equality half.
	sdst := make([]string, 0, 4)
	sdst, err = (fkapi.LuaEntityPrototype{Object: first}).AcceptedSeedsInto(sdst)
	if err != nil {
		fk.Log("accepted_seeds_into failed: " + err.Error())
		return
	}
	out := "into strings: " + u(len(sdst))
	for _, s := range sdst {
		out += " " + s
	}
	fk.Log(out)

	dst := make([]fkapi.Object, 0, 8)
	dst, err = fkapi.Game.ConnectedPlayersInto(dst)
	if err != nil {
		fk.Log("connected_players_into failed: " + err.Error())
		return
	}
	base := backing(dst)
	stable := true
	for i := 0; i < 3; i++ {
		dst, err = fkapi.Game.ConnectedPlayersInto(dst)
		if err != nil || backing(dst) != base {
			stable = false
		}
	}
	fk.Log("into: " + u(len(dst)) + " same-buffer=" + yn(stable))

	// ...and a destination that CANNOT hold the answer, which has to allocate.
	// The line says only the count, because that is the strongest thing BOTH
	// mirrors can truthfully say: Go's `dst []T` cannot grow the caller's slice
	// and returns a new one, leaving `small` untouched, while Rust's
	// `&mut Vec<T>` grows the caller's own vector in place. Asserting either
	// language's version of "who owns the new buffer" would make the two logs
	// differ, and the whole value of these mirrors is that they do not.
	small := make([]fkapi.Object, 0, 1)
	grown, err := fkapi.Game.ConnectedPlayersInto(small)
	if err != nil {
		fk.Log("connected_players_into (small) failed: " + err.Error())
		return
	}
	fk.Log("into grown: " + u(len(grown)))
}

// backing is the address of a slice's first element, which is how "did it
// reallocate" is asked without a heap probe.
func backing(s []fkapi.Object) uintptr {
	if len(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func boolStr(b bool) string {
	if b {
		return "an entity"
	}
	return "nothing"
}

// render prints a dynamic value in a form the test can assert on. Recursive,
// because the value is.
func render(v fkapi.Value) string {
	switch v.Tag {
	case fkapi.TagNil:
		return "nil"
	case fkapi.TagBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case fkapi.TagNumber:
		return strconv.FormatFloat(v.Number, 'f', -1, 64)
	case fkapi.TagString:
		return "'" + v.Str + "'"
	case fkapi.TagObject:
		return "obj"
	case fkapi.TagArray:
		out := "["
		for i, e := range v.Array {
			if i > 0 {
				out += " "
			}
			out += render(e)
		}
		return out + "]"
	case fkapi.TagMap:
		out := "{"
		for i, kv := range v.Map {
			if i > 0 {
				out += " "
			}
			out += render(kv.Key) + "=" + render(kv.Val)
		}
		return out + "}"
	}
	return "?"
}

func main() {}
