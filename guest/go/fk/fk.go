// Package fk is the guest-side half of the FkLua host boundary.
//
// A guest imports this, writes ordinary Go, and marks its entry points with
// //go:wasmexport. FkLua compiles the resulting wasm to Lua and packages it as
// a mod; the mod's control.lua binds the other half of every function declared
// here.
//
//	//go:wasmexport fk_on_init
//	func onInit() { fk.Log("hello from Go") }
//
// Build with the flags in [BuildFlags]. Nothing here works under any other
// target: wasm-unknown has no WASI, no files and no clock, which is the point --
// Factorio's sandbox has none of those either.
//
// # What is here, and what is next door
//
// This package is the boundary itself: logging, batching, the message behind a
// failed host call, and the build flags a guest cannot work without. THE
// FACTORIO API IS NOT HERE -- it lives in
// [github.com/Techrocket9/fklua/guest/go/fkapi], which is generated from the
// game's own runtime-api.json and committed. That is where `game`, entities,
// events and the handle table are. A guest almost always imports both.
//
// # State survives a save, by default
//
// Guest memory is carried across a save and reload, so a map or a slice a
// guest accumulated during play is still there when the game is loaded again.
// `fklua mod --persist=table` is the default and the cheapest at runtime;
// `--persist=packed` trades a little time per call for a much smaller save;
// `--persist=none` restores the old behaviour, where memory is rebuilt from
// the module's data segments every load and nothing accumulates.
//
// A save records the build it was written by. Recompiling the guest moves its
// heap layout, so a rebuilt mod starts clean unless it exports
// fk_migrate(oldVersion) and takes charge of the old bytes itself.
package fk

import "unsafe"

// BuildFlags are the TinyGo flags a guest must be built with. They are not
// advisory:
//
//   - -target=wasm-unknown is the only target whose feature set FkLua can
//     compile; internal/guest checks that claim against TinyGo's own target
//     JSON on every test run.
//   - -scheduler=none because a Factorio tick cannot block. With a scheduler,
//     any goroutine that parks becomes a busy spin inside the game loop.
//   - -gc=leaking because a collector's cost lands in a lockstep game loop,
//     where one client stalling desyncs everyone. Guest memory is a fixed
//     arena that only grows.
//   - -opt=2 because TinyGo's default is -opt=z, which optimises for SIZE, and
//     size is the one cost this target does not have: the day-0 probe measured
//     Factorio parsing 4 MB of Lua in 106 ms and a generated chunk never
//     appears in a save. Measured against -opt=z through the real compiler:
//     real_names 0.577x, real_grid 0.771x, pure_sum 0.770x, pure_dot 0.847x,
//     real_entities 0.958x. pure_prng is ~2% slower and is the only kernel
//     that does not gain. -opt=0 and -opt=1 are NOT substitutes: -opt=0 fails
//     to build under -scheduler=none, and -opt=1 leaves most of the win.
var BuildFlags = []string{
	"-target=wasm-unknown",
	"-scheduler=none",
	"-gc=leaking",
	"-opt=2",
}

// CollectedBuildFlags are BuildFlags with a garbage collector: the same four,
// with -gc=custom in place of -gc=leaking.
//
// A guest built with these must also import
// [github.com/Techrocket9/fklua/guest/go/fkgc], which supplies the seven
// runtime hooks -gc=custom expects. Importing it is harmless under any other
// -gc -- the package is empty there -- so the import can be unconditional and
// the collector is genuinely one flag.
//
//	import _ "github.com/Techrocket9/fklua/guest/go/fkgc"
//
// What changes: guest memory stops being an arena that only grows. What does
// not change: the collector is stop-the-world today and must be driven from a
// safe point between events (fkgc.CollectIfNeeded, from fk_on_tick). What it
// does not cover is a mass-builder -- see agents/gc.md's reclaim-rate table,
// which says an ordinary event handler is covered with headroom, a blueprint
// paste is covered with about two seconds of latency, and bulk construction is
// not covered at any budget worth having.
var CollectedBuildFlags = []string{
	"-target=wasm-unknown",
	"-scheduler=none",
	"-gc=custom",
	"-opt=2",
}

// wasm has no string type, so a string crosses the boundary as a (pointer,
// length) pair into the guest's linear memory and the host reassembles it.
// These are bound in runtime/lua/fk_mod.lua.

//go:wasmimport env fk_log
func hostLog(ptr, length uint32)

//go:wasmimport env fk_print
func hostPrint(ptr, length uint32)

//go:wasmimport fk defer
func hostDefer() uint32

//go:wasmimport fk last_error
func hostLastError(ptr, capacity uint32) uint32

// errScratch is where LastError asks the host to put the message.
//
// Package-level rather than a local, for the reason every block in the
// generated bindings is: `var b [256]byte` whose address is taken does NOT stay
// on the guest's stack under TinyGo -- the ptrtoint that makes an address
// crossable defeats LLVM's promotion -- so a local would be a heap allocation
// per call, permanent under -gc=leaking. This is linear memory the module
// already has, and TinyGo removes it from a guest that never calls LastError.
//
// 256 bytes because engine refusals are sentences: the longest this repo has met
// is about seventy characters. A longer one is not truncated; it costs a second
// host call. See below.
var errScratch [256]byte

// LastError is what Factorio said when the last host call failed.
//
// A status is an i32 and a message is not, so a binding that returns an error
// can only tell you the KIND of failure -- "the Factorio API raised". This is
// the sentence it raised WITH, which is the difference between knowing that a
// call was refused and knowing why:
//
//	if err := something(); err != nil {
//		fk.Log("refused: " + fk.LastError())
//	}
//
// IT DESCRIBES THE CALL THAT JUST RETURNED. The host clears the slot as each
// host call begins, so this is empty after a call that succeeded rather than
// carrying some earlier tick's failure -- read it immediately, where the error
// is still in hand.
//
// The bytes are the engine's own and are not promised to be UTF-8; Go's string
// is a byte sequence, so nothing is rewritten on the way. The message is copied,
// so what comes back outlives the next host call.
//
// It is DIAGNOSTIC. Log it; do not branch on it. The text is an engine
// implementation detail that a point release may reword, and a mod that behaved
// differently because of a wording is a mod that behaves differently on two
// Factorios. A TEST asserting the exact text is the honest exception, and is
// what this exists for: an engine that stops refusing something should fail a
// suite rather than quietly widen it.
func LastError() string {
	n := hostLastError(uint32(uintptr(unsafe.Pointer(&errScratch[0]))),
		uint32(len(errScratch)))
	if n == 0 {
		return ""
	}
	if n <= uint32(len(errScratch)) {
		return string(errScratch[:n])
	}
	// THE RETURN IS THE FULL LENGTH RATHER THAN WHAT WAS COPIED, which is what
	// makes a fixed buffer safe: a message that did not fit is asked for again
	// with room, instead of silently arriving short. Two host calls for a
	// message over 256 bytes and none for the ones a guest actually meets.
	b := make([]byte, n)
	if m := hostLastError(uint32(uintptr(unsafe.Pointer(&b[0]))), n); m < n {
		return string(b[:m])
	}
	return string(b)
}

// Defer asks for the guest's fk_on_deferred export to be called ONCE on the
// next tick.
//
// This is how a guest batches. Factorio delivers a blueprint paste as one
// on_built_entity PER ENTITY, each of them a separate dispatch raised by the
// engine's own loop, so there is no "after this event's handlers finish" a
// guest could hook to notice that a burst ended -- accumulating during the
// burst and doing the work once is the only shape that costs O(1) rather than
// O(P). Call it as often as you like within a tick: the registration happens
// once and is torn down again the moment the flush runs, so a guest that is
// idle pays nothing per tick.
//
//	//go:wasmexport fk_on_deferred
//	func onDeferred() { rebuildWhateverGotDirty() }
//
// The flush lands on the FOLLOWING tick, not at the end of the current one:
// Factorio has no end-of-tick hook, and on_tick for the current tick has
// already been raised by the time a build event arrives. Work deferred and then
// saved before it ran is re-armed when the save is loaded, so nothing is lost.
//
// A guest with no fk_on_deferred export never gets called back.
func Defer() { hostDefer() }

// Log writes a line to factorio-current.log.
//
// This works everywhere, including while the mod is loading and inside
// on_load, which is why it is the one to reach for when something has gone
// wrong at startup.
func Log(s string) { hostLog(ptrOf(s), uint32(len(s))) }

// Print writes a line to the in-game console.
//
// Before the game exists -- during mod load, and in on_load -- there is no
// console, and the message goes to the log instead rather than raising.
func Print(s string) { hostPrint(ptrOf(s), uint32(len(s))) }

// ptrOf takes the address of a string's bytes.
//
// The empty string has no backing array, and taking the address of nothing
// yields a pointer the host would then be asked to read zero bytes from. Zero
// is safe to pass because the host reads len bytes and len is zero, but relying
// on unsafe.StringData's behaviour for an empty string is not, so it is handled
// here.
func ptrOf(s string) uint32 {
	if len(s) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(unsafe.StringData(s))))
}
