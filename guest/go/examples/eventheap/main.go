// Command eventheap measures how much guest heap a HOST-INITIATED dispatch
// keeps forever.
//
// examples/heap asks the same question of a GUEST-initiated call and answers 0,
// because every generated binding brackets the marshalling arena: mark before,
// release after. Nothing on the guest side makes a host-initiated dispatch --
// an event Factorio raised, a console command somebody typed, a remote method
// another mod called -- so until the outermost dispatch took a bracket of its
// own there was nothing to release what the host allocated to get the payload
// in here.
//
// THE CLIFF IS THE STRING SCRATCH REGION. A string field is written into the
// 4 KiB region when it fits and falls back to fk_alloc when it does not, so an
// event whose payload is small is free and one whose payload is large advances
// the arena by its own size, per dispatch, for the session. No event in this
// repo's corpus had ever carried a large string, which is why the corpus could
// not see it -- and carrying payloads is the entire purpose of the feature this
// was found ahead of.
//
// The measurement is the allocator's own bump pointer, the way examples/heap
// reads it. The handler deliberately reads NOTHING in the measured window: a
// Go string copied out of an event is the caller's own value and allocates by
// design, so a handler that kept one would report its own cost as the ABI's.
// The first dispatch of each leg reads the payload and logs its length, which
// is what says the string really crossed rather than being quietly dropped.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o eventheap.wasm ./examples/eventheap
package main

import (
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// Enough dispatches that a per-dispatch cost dominates the two one-byte probes
// that bracket the window, and few enough that the run stays quick. A leak of
// one arena chunk per dispatch is thousands of bytes, so this is generous.
const (
	warm  = 3
	iters = 50
)

// The remote method this guest declares. The id space is its own.
const mSend = 1

// Somewhere for a payload to go that the optimizer cannot prove is unread.
var sink string

var (
	events    int
	eventsAt  uintptr
	eventsEnd uintptr
	calls     int
	callsAt   uintptr
	callsEnd  uintptr
)

// REGISTRATION FROM init, WHICH IS WHAT _initialize RUNS. A remote interface is
// not saved -- Factorio re-executes control.lua on every load -- so it has to be
// declared on every load, and this is the only place that happens by
// construction. Same reasoning as examples/callback.
func init() {
	fkapi.Subscribe(fkapi.EventOnConsoleChat)
	fkapi.AddInterface("fk-eventheap", fkapi.InterfaceMethod{Name: "send", ID: mSend})
}

// heapTop is the allocator's bump pointer, read the only way a guest can read
// it: by asking for a byte and looking at where it landed.
func heapTop() uintptr {
	b := make([]byte, 1)
	return uintptr(unsafe.Pointer(&b[0]))
}

// BOTH WINDOWS CLOSE BEFORE EITHER IS REPORTED, and that is not tidiness: the
// two legs are interleaved by the driver, so a log line emitted the moment the
// event window closed landed INSIDE the call window -- and building that line
// allocates. It measured 1 B/dispatch of the probe's own reporting and read
// exactly like a residual leak. Nothing may allocate between a window's two
// heapTop reads except the thing under test.
func reportBoth() {
	if eventsEnd == 0 || callsEnd == 0 {
		return
	}
	report("event string", eventsAt, eventsEnd)
	report("call string", callsAt, callsEnd)
}

func report(what string, before, after uintptr) {
	fk.Log(what + ": " + itoa(uint32(int(after-before)/iters)) + " B/dispatch")
}

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	events++
	if events == 1 {
		// The one read, outside the window: proof the message arrived whole.
		// getStr copies it into a Go string, which is a permanent allocation
		// under -gc=leaking and is exactly why the measured dispatches below do
		// not do this.
		sink = fkapi.ReadOnConsoleChat(ptr).Message
		fk.Log("event msg " + itoa(uint32(len(sink))))
	}
	if events == warm {
		eventsAt = heapTop()
	}
	if events == warm+iters {
		eventsEnd = heapTop()
		reportBoth()
	}
}

//go:wasmexport fk_on_call
func onCall(id, argp, retp uint32) uint32 {
	if id != mSend {
		return uint32(fkapi.StatusNoMember)
	}
	calls++
	if calls == 1 {
		args := fkapi.ReadDyn(argp).Array
		if len(args) == 1 {
			sink = args[0].Str
		}
		fk.Log("call arg " + itoa(uint32(len(sink))))
	}
	if calls == warm {
		callsAt = heapTop()
	}
	if calls == warm+iters {
		callsEnd = heapTop()
		reportBoth()
	}
	return 0
}

// itoa, because a guest that imported strconv would link the whole formatting
// machinery for one number -- agents/guests.md's heap diet, arrived at cheaply.
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
