// Command hello is the M4 end-to-end guest: a Go program that becomes a
// Factorio mod.
//
// It is also the fixture the end-to-end test builds and runs, so it is written
// to exercise the parts of the compiler a "hello world" would not: a map, a
// slice that grows, string formatting, 64-bit arithmetic and floating point.
// A guest that only adds two integers proves the pipeline connects; this one
// proves it carries a program.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o hello.wasm ./examples/hello
//	fklua mod hello.wasm --name fk-hello --version 0.1.0 --author you
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

// Package state. Under the default --persist=table this SURVIVES a save: the
// whole linear memory is carried, so the map, the slice and TinyGo's allocator
// state all come back as they were. `scripts/run-roundtrip.sh` proves it in a
// real Factorio save cycle, and `--persist=none` is what restores the old
// rebuild-from-data-segments behaviour.
//
// Nothing here exports fk_migrate, and that is the right default for a Go
// guest: see "Why a TinyGo guest should almost never export fk_migrate" in
// agents/guests.md.
var (
	counts  = map[string]int{}
	history []uint32
	total   uint64
)

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("hello from Go, running as Lua inside Factorio")
	fk.Log("guest built with TinyGo: " + describe())
}

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	history = append(history, tick)
	total += uint64(tick)

	switch {
	case tick%15 == 0:
		counts["fizzbuzz"]++
	case tick%3 == 0:
		counts["fizz"]++
	case tick%5 == 0:
		counts["buzz"]++
	default:
		counts["plain"]++
	}

	// Report on a schedule rather than every tick: this is the shape a real mod
	// wants, and it also keeps the test's expected output small.
	if tick%10 == 0 {
		fk.Log(report(tick))
	}
}

// report exercises string building, map lookup, i64 and f64 in one line, since
// each of those is a different part of the emitter.
func report(tick uint32) string {
	mean := 0.0
	if len(history) > 0 {
		mean = float64(total) / float64(len(history))
	}
	return "tick " + strconv.FormatUint(uint64(tick), 10) +
		" seen=" + strconv.Itoa(len(history)) +
		" fizz=" + strconv.Itoa(counts["fizz"]) +
		" buzz=" + strconv.Itoa(counts["buzz"]) +
		" fizzbuzz=" + strconv.Itoa(counts["fizzbuzz"]) +
		" sum=" + strconv.FormatUint(total, 10) +
		" mean=" + strconv.FormatFloat(mean, 'f', 2, 64)
}

func describe() string {
	// A 64-bit multiply and a shift, which lower to (lo, hi) pair arithmetic
	// rather than anything native.
	var h uint64 = 1469598103934665603
	for _, c := range []byte("fklua") {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return "fnv64(fklua)=" + strconv.FormatUint(h, 16)
}

// TinyGo builds this as a c-shared reactor, so main never runs. It exists
// because the package must still be package main.
func main() {}
