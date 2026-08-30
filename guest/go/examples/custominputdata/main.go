// Command custominputdata is the DATA-STAGE half of the custom-input fixture:
// it defines the two custom-input prototypes ./examples/custominput subscribes
// to by name.
//
// A SECOND wasm module, because a data guest always is one -- a control guest
// hooked into the data family is parsed once per stage for a program the stage
// never calls, and it may not import fkapi at all. See agents/datastage.md.
//
// It exists because there is no other way to make the success path reachable.
// script.on_event with a name is accepted only when a custom-input prototype of
// that name is loaded, so a control-stage-only fixture reaches the refusal and
// nothing else. Which is exactly what examples/api does, deliberately, from the
// other side.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o custominputdata.wasm ./examples/custominputdata
package main

import "github.com/Techrocket9/fklua/guest/go/fkdata"

// input defines one custom-input prototype.
//
// `consuming = "none"` is the permissive setting and is what a mod that only
// listens wants: the keypress still reaches whatever else is bound to it.
func input(name, keys string) {
	fkdata.Extend(fkdata.Obj(
		fkdata.KVs("type", fkdata.Str("custom-input")),
		fkdata.KVs("name", fkdata.Str(name)),
		fkdata.KVs("key_sequence", fkdata.Str(keys)),
		fkdata.KVs("consuming", fkdata.Str("none")),
	))
}

//go:wasmexport fk_data
func onData() {
	fkdata.Log("custominputdata: defining two custom inputs")
	input("fkci-primary", "ALT + J")
	input("fkci-second", "ALT + K")
}
