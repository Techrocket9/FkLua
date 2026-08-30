// Command custominput is the in-game fixture for a NAME-ADDRESSED subscription,
// which is how Factorio delivers a keybind.
//
// A mod declares a custom-input prototype at the data stage and subscribes to it
// by that prototype's own name: script.on_event("my-input", f). The event has no
// defines.events entry at all -- measured on 2.0.77, the table holds 233 keys
// and CustomInputEvent is not one of them -- so the numeric form could never
// reach it, and until fk.subscribe took a name a whole genre of mod was
// unwritable on FkLua.
//
// Its data-stage half is ./examples/custominputdata, which defines the two
// prototypes this subscribes to. scripts/run-custominput.sh packages the pair
// and runs them in a real Factorio.
//
// WHAT A HEADLESS RUN CAN AND CANNOT PROVE, measured rather than assumed:
//
//   - it CAN prove the registration succeeds, that two custom inputs share one
//     guest registration, that a name this game does not have is refused as a
//     STATUS with the engine's own words rather than as a mod that will not
//     load, and that no false log line is written for any of it;
//   - it CANNOT press a key, and it cannot fake one either: script.raise_event
//     refuses a custom input outright. The refusal is captured here verbatim
//     through fk.LastError, which is the same tripwire the lasterror fixture
//     exists to be -- the day Factorio starts allowing that raise, this line
//     changes and the gate says so.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o custominput.wasm ./examples/custominput
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// The two prototypes ./examples/custominputdata defines, and one that no mod
// defines. The third is the typo case, and it is here because a keybind name is
// a string a guest author types by hand.
const (
	inputPrimary = "fkci-primary"
	inputSecond  = "fkci-second"
	inputAbsent  = "fkci-no-such-input"
)

//go:wasmexport fk_on_event
func onEvent(id, ptr uint32) {
	if id != fkapi.EventCustomInputEvent {
		return
	}
	// ONE HANDLER FOR BOTH INPUTS, disambiguated on the payload's own
	// input_name. Every custom input encodes through the same CustomInputEvent
	// descriptor and therefore arrives under the same guest id, which is the
	// whole reason the payload carries the field.
	e := fkapi.ReadCustomInputEvent(ptr)
	fk.Log("custominput: fired name=" + e.InputName +
		" player=" + strconv.FormatUint(uint64(e.PlayerIndex), 10))
}

func init() {
	// Subscribing from an initialiser is what a real guest does: it runs during
	// _initialize, before any event can fire, and a registration made from
	// fk_on_init would exist only in the session that created the map.
	fk.Log("custominput: primary st=" +
		st(fkapi.SubscribeNamed(fkapi.EventCustomInputEvent, inputPrimary)))

	// THE MASKED FORM, on the second input. CustomInputEvent has three maskable
	// fields and this guest reads none of them, so the host stops encoding the
	// selected-prototype struct and the GUI element handle on every keypress.
	fk.Log("custominput: second st=" +
		st(fkapi.SubscribeNamedMasked(fkapi.EventCustomInputEvent,
			fkapi.SkipCustomInputEventSelectedPrototype|
				fkapi.SkipCustomInputEventElement|
				fkapi.SkipCustomInputEventCursorDirection, inputSecond)))

	// A NAME NO PROTOTYPE HAS. The engine raises `Unknown event name: ...`;
	// fklua catches it, rolls its own registration back, logs the engine's
	// sentence and returns a status. A mod with a typo in a keybind keeps
	// running without the keybind, which is the same call the filter path makes.
	fk.Log("custominput: absent st=" +
		st(fkapi.SubscribeNamed(fkapi.EventCustomInputEvent, inputAbsent)))

	// ...AND THE UNNAMED FORM, which is the trap this feature exists to close.
	// The id is a real constant and the binding is complete, so this compiles,
	// passes the pruning scan, and cannot work. What it must NOT produce is the
	// old sentence claiming this Factorio has no such event.
	fk.Log("custominput: unnamed st=" +
		st(fkapi.Subscribe(fkapi.EventCustomInputEvent)))
}

//go:wasmexport fk_on_init
func onInit() {
	// CAN A SCRIPT FIRE A CUSTOM INPUT? No, and the refusal is captured verbatim
	// rather than described. This is the lasterror fixture's own argument: a
	// check that asserted only "it failed" would go on passing on the day the
	// engine starts allowing it, over a path that had silently become testable
	// for real.
	id, err := fkapi.Script.GetEventId(fkapi.OfString(inputPrimary))
	if err != nil {
		fk.Log("custominput: get_event_id failed: " + err.Error())
	} else {
		fk.Log("custominput: get_event_id=" + strconv.FormatUint(uint64(id), 10))
	}

	err = fkapi.Script.RaiseEvent(fkapi.OfString(inputPrimary), fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("player_index"), Val: fkapi.OfNumber(1)},
		fkapi.KeyValue{Key: fkapi.OfString("input_name"), Val: fkapi.OfString(inputPrimary)},
	))
	if err == nil {
		fk.Log("custominput: raise ok=true -- the engine now allows this, and " +
			"the gate's own expectation is stale")
	} else {
		fk.Log("custominput: raise ok=false err=[" + fk.LastError() + "]")
	}
}

// st renders a Status as the ABI's own number. Zero is OK; 3 is ERR_NO_MEMBER,
// which is what a name this game does not have comes back as.
func st(s fkapi.Status) string {
	return strconv.FormatUint(uint64(uint32(s)), 10)
}
