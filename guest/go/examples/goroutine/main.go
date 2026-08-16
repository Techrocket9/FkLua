// Command goroutine is the M10 guest: a Go program using goroutines and
// channels, compiled to Lua and run inside Factorio's sandbox.
//
// This is only possible because TinyGo's asyncify scheduler rewrites the module
// into a resumable state machine INSIDE the wasm. FkLua needs no coroutines to
// run it -- which is the whole point, because Lua 5.2 in Factorio has none.
//
//	tinygo build -target=wasip1 -o goroutine.wasm ./examples/goroutine
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
)

//go:wasmexport fk_on_init
func onInit() {
	fk.Log("goroutines: starting")

	// A channel and two goroutines. Under asyncify each of these becomes a
	// suspend/resume point in the generated state machine rather than an OS
	// thread, so the whole thing runs on one Lua call stack.
	results := make(chan string, 4)
	go func() { results <- "worker-a" }()
	go func() { results <- "worker-b" }()

	// A pipeline: values through a chain of goroutines, which is where the
	// scheduler actually has to interleave rather than just run two closures.
	nums := make(chan int)
	sums := make(chan int)
	go func() {
		for i := 1; i <= 10; i++ {
			nums <- i
		}
		close(nums)
	}()
	go func() {
		total := 0
		for n := range nums {
			total += n
		}
		sums <- total
	}()

	a, b := <-results, <-results
	// Completion order between two goroutines is a scheduler detail, so the
	// test must not depend on it -- sort the pair rather than assert an order.
	if a > b {
		a, b = b, a
	}
	fk.Log("goroutines: " + a + " " + b)
	fk.Log("pipeline sum: " + strconv.Itoa(<-sums))
}

var ticks, spawned int

//go:wasmexport fk_on_tick
func onTick(tick uint32) {
	// A goroutine EVERY TICK, not just once. Running the scheduler repeatedly
	// is the property that matters for a mod: asyncify has to unwind and rewind
	// cleanly on each entry, and a state machine that only worked the first
	// time would pass an init-only test.
	ticks++
	done := make(chan int, 1)
	go func() { done <- int(tick) * 2 }()
	if <-done != int(tick)*2 {
		fk.Log("goroutine returned the wrong value")
		return
	}
	spawned++

	if tick%10 == 0 {
		fk.Log("tick " + strconv.Itoa(int(tick)) +
			" goroutines-run=" + strconv.Itoa(spawned))
	}
}

func main() {}
