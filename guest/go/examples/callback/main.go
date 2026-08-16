// Command callback exercises the callback seam in both directions: a console
// command and a remote interface this guest declares, and a remote.call it makes
// into somebody else's.
//
// IT EXISTS BECAUSE THE THREE MEMBERS BEHIND IT CANNOT BE BOUND.
// LuaCommandProcessor::add_command and LuaRemote::add_interface take a Lua
// FUNCTION, and LuaRemote::call is the API's one variadic method -- so all three
// are recorded as host skips in api/<version>/census.json and no generated
// binding will ever reach them. What this guest demonstrates is the seam that
// stands in for them: the host synthesises the closure, and dispatches back in
// here by an id this guest chose.
//
// Read runtime/lua/fk_mod.lua's "Commands and remote interfaces" section for the
// design; this is the other end of it.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o callback.wasm ./examples/callback
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// The id space is this guest's own: the host stores nothing but the closure that
// captures one, so these are not registered anywhere and cannot collide with
// anything outside this file.
const (
	cmdEcho   = 1
	mAdd      = 2
	mGreet    = 3
	mArity    = 4
	mNoReturn = 5
)

var calls uint32

// REGISTRATION HAPPENS IN init, WHICH IS WHAT _initialize RUNS, and that is not
// a style choice. A command registration is not saved: Factorio re-executes
// control.lua on every load, so it has to be made on every load. A registration
// made from fk_on_init would exist in the session that created the map and in no
// other -- and the failure would be invisible until somebody loaded a save.
func init() {
	fkapi.AddCommand(cmdEcho, "fk-echo", fkapi.OfString("echoes its parameter"))
	fkapi.AddInterface("fk-callback-demo",
		fkapi.InterfaceMethod{Name: "add", ID: mAdd},
		fkapi.InterfaceMethod{Name: "greet", ID: mGreet},
		fkapi.InterfaceMethod{Name: "arity", ID: mArity},
		fkapi.InterfaceMethod{Name: "no_return", ID: mNoReturn})
}

// fk_on_call is the whole inbound surface: one export, id-dispatched, exactly
// like fk_on_event. argp is a tier-2 ARRAY of the arguments as they arrived and
// retp is one tier-2 slot for the result, which a command's trampoline ignores.
//
//go:wasmexport fk_on_call
func onCall(id, argp, retp uint32) uint32 {
	calls++
	args := fkapi.ReadDyn(argp).Array
	switch id {
	case cmdEcho:
		// A command handler is handed exactly one argument, the
		// CustomCommandData table: name, tick, and optionally player_index and
		// parameter. It arrives as a tier-2 map rather than a generated struct
		// because the OTHER thing this export serves -- a remote method -- has no
		// description anywhere to generate one from.
		name, param := "", ""
		var tick float64
		if len(args) == 1 {
			for _, kv := range args[0].Map {
				switch kv.Key.Str {
				case "name":
					name = kv.Val.Str
				case "parameter":
					param = kv.Val.Str
				case "tick":
					tick = kv.Val.Number
				}
			}
		}
		fk.Log("cmd " + name + " param=" + param + " tick=" + itoa(uint32(tick)))
		// ...AND OUT AGAIN, FROM INSIDE A DISPATCH. This is the re-entrant case
		// the scratch bracket in invoke_callback exists for: the guest is running
		// inside a trampoline, and the call it makes lands in the SAME trampoline
		// at a deeper level, encoding its own arguments into the region the outer
		// invocation's arguments are still sitting in.
		if v, st := fkapi.RemoteCall("fk-callback-demo", "add",
			fkapi.OfNumber(4), fkapi.OfNumber(5)); st == fkapi.StatusOK {
			fk.Log("outbound " + itoa(uint32(v.Number)))
		} else {
			fk.Log("outbound FAILED " + itoa(uint32(st)))
		}
		// A missing interface is an ordinary condition -- the other mod may
		// simply not be installed -- so the guest gets a status, not a trap.
		if _, st := fkapi.RemoteCall("no-such-interface", "nope"); st != fkapi.StatusOK {
			fk.Log("missing " + itoa(uint32(st)))
		}
		// The outer invocation's arguments must still be readable AFTER those two
		// nested calls, which is the whole point of the mark/release pair.
		again := ""
		for _, kv := range fkapi.ReadDyn(argp).Array[0].Map {
			if kv.Key.Str == "parameter" {
				again = kv.Val.Str
			}
		}
		fk.Log("still " + again)
	case mAdd:
		var sum float64
		for _, a := range args {
			sum += a.Number
		}
		fkapi.WriteDyn(retp, fkapi.OfNumber(sum))
	case mGreet:
		if len(args) == 1 {
			fkapi.WriteDyn(retp, fkapi.OfString("hello, "+args[0].Str))
		}
	case mArity:
		// THE ARITY IS THE POINT. A caller writing f(1, nil, 3) must be heard as
		// three arguments, not one -- which is what H.write_varargs buys and what
		// a `{...}` would have lost silently, for that caller only.
		fkapi.WriteDyn(retp, fkapi.OfNumber(float64(len(args))))
	case mNoReturn:
		// Writes nothing. The host cleared the slot before dispatching, so this
		// must read back as nil rather than as the previous call's result.
	default:
		return uint32(fkapi.StatusNoMember)
	}
	return 0
}

// fk_on_tick reports how many callbacks have landed, so a test can assert the
// count without reaching a guest export through control.lua -- which does not
// publish them.
//
//go:wasmexport fk_on_tick
func onTick(tick uint32) { fk.Log("calls " + itoa(calls)) }

// itoa, because a guest that imported strconv would link the whole formatting
// machinery for one number -- the lesson agents/guests.md records as the heap
// diet, arrived at the cheap way.
func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func main() {}
