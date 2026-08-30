# Rules a guest has to follow

Five rules that a guest can break without anything reporting it. Each one produces a mod that loads, runs and looks correct, and fails later: a multiplayer desync, a mod ten times bigger than it needs to be, a save that will not load, or a stall every player feels at once.

Nothing else in FkLua asks you to think about the runtime. These are what is left.

## No peer-local signal may change guest state

Factorio is a lockstep simulation. Every peer runs the same code over the same state, and the state is compared: under the default `--persist=table` your guest's memory IS `storage.fk_mem`, so Factorio checksums it directly. Two peers whose guest memory differs by one byte have desynced.

So the only things a guest may branch on **when it writes** are its own state, the replicated tick, and what arrived through the replicated inbound path (events, commands, remote calls, the deferred flush).

The trap is that some signals are not replicated. `script.on_load` runs on **every peer that loads the state**, which on a running server means the joining client and nobody else. A guest that writes something from a load hook has written it on one peer. That is why `fk_after_load` exists as a read-only opportunity to rebuild caches from the world rather than as a place to change anything, and why a one-shot armed from a load hook is a write from a load hook with one tick of delay.

The same applies to anything a single peer can observe and the others cannot: a wall clock, a file, a socket, or the result of asking whether this peer is a server. If a value can differ between two clients, it may be logged and it may not be stored.

## Iteration order is a fact about the program, not about the run

Two peers must make the same sequence of API calls. Anything that walks a hash table can produce a different order on different runs, and in Go the language randomizes it deliberately.

The bindings are built so that this is hard to get wrong: a tier-2 map is a slice of pairs rather than a map, a dictionary field inside a struct is a slice of pairs, and every generated walk is index-ordered. What is left is your own code. A `map[K]V` you iterate to decide what to build is a desync; collect the keys, sort them, and walk the sorted slice.

Reading a map is fine. Writing to the world in the order you happened to read one is not.

## An id reaching `fk.call` has to be a compile-time constant

A mod ships the members it calls, not the API. `fklua mod` scans the compiled guest for the constant ids reaching `fk.call`, `fk.call_typed`, `fk.subscribe` and `fk.define`, and prunes the packaged tables to what it found. A one-member guest ships a 646-byte member table where the full one is about 840 KB.

An id the scan cannot prove constant makes it ship the whole table instead: a bigger mod, never a broken one, and the build output says so in as many words. The usual way to lose it is subscribing in a loop over a slice of event ids, or computing one. Write the calls out.

## The tick is atomic

A guest handler runs to completion inside one Factorio tick, and there is no way to yield in the middle of one. Goroutines under the wasip1 target are cooperative within a single dispatch; they are not a way to spread work across ticks.

Work that does not fit in a tick is spread by the guest, across ticks it chose. `fk.Defer()` is the mechanism: it registers a one-shot `on_tick` and tears it down again from inside the flush, so an idle guest pays no per-tick cost. A blueprint paste of 200 entities is 200 separate dispatches in one tick, and deferring turns 200 recompiles into one.

There is deliberately no end-of-dispatch hook. Factorio raises one build event per entity from its own loop, so a hook there would fire 200 times and batch nothing.

Work on a schedule rather than after a burst is `fk.OnNthTick(n)` and the `fk_on_nth_tick` export. The engine does the counting, so a guest polling every 600 ticks is entered once per 600 rather than on all of them to decide it has nothing to do; the armed periods survive a save and are re-armed when it is loaded.

## Allocate on a schedule you chose

A guest's heap is in every save and every multiplayer join, and linear memory never shrinks. Under the collector (`gc = "collected"`, the default) allocation is reclaimed but still costs marking and sweeping out of a paced budget; with the collector off it is permanent.

Two consequences worth having in front of you:

**The budget is about 0.2 ms of worst-case tick per MiB of linear memory**, and it is the memory's size rather than the part in use, because the collector walks the table. A guest that has been 64 MiB is walked as 64 MiB for the rest of the session.

**Growth is by doubling**, so a guest that allocates steadily climbs a ladder, and the step from 16 MiB to 32 writes four million fresh words in one tick. Measured in game: 48 ms at the 2 MiB step, 226 ms at 8 MiB, and **782 ms at 16 MiB**, which is a freeze every client feels at once.

The single largest source of this in practice is log lines. `fk.Log("x=" + itoa(n))` allocates every intermediate string; one mod measured its entire guest heap as log lines, at 64 MiB and a 19.9 ms idle worst tick, against under 16 MiB and 2.3 ms once they were built into one reused buffer. `fklog` is that buffer, shipped in both guest trees, and it is what to reach for before anything else.

The whole picture, the tuning knobs and the opt-out are in [memory.md](memory.md).

## What is checked for you

Not everything above can be checked, but some of it is:

- **Determinism across two runs** is scriptable, and it is the cheapest real check there is: create a map, benchmark it twice, compare Factorio's own script checksums. The recipe is in [verifying.md](verifying.md).
- **Pruning** is reported at package time. If the build says it is shipping all 219 event descriptors, an id was not constant.
- **A guest built against different bindings** than the tables packaged beside it is refused (the API pin) or named on stderr (the ABI signature). See [factorio-api.md](factorio-api.md).
- **Lua's own limits** are checked when you package rather than left for your users. See [lua-limits.md](lua-limits.md).
