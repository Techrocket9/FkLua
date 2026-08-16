//go:build gchuge

package main

// The PAST-THE-OLD-CAP arm, and it is sharding stage C's acceptance vehicle in
// the game rather than in a harness.
//
// 16 MiB was `fkgc.HeapCap`: a HARD cap a guest trapped against, with a bare
// `unreachable`, because the mark bitmap and the span table were statically
// reserved .bss. This arm holds about 36 MiB, which the collector could not
// have described at any build-tag setting short of the 64 MiB one -- and that
// one cost 583 KiB of .bss before the guest allocated anything.
//
// THE LIVE SET IS BULK RATHER THAN NODES, and that is about the map-creation
// tick and not about the collector. 36 MiB of 48-byte nodes is 780,000
// allocations in one fk_on_init, which is the tick `--create` runs; the same
// bytes as 36 one-megabyte blocks is 36 allocations, each a run of spans, and a
// span run is the arm the metadata chunks can block. It is the shape worth
// testing anyway.
const nlive = 44000
const nbulk = 36

// nbulkkeep is how many of those blocks survive the first tick, and the drop is
// what makes this a COLLECTOR arm rather than a size report.
//
// The heap has to GROW past 32 MiB, which is what the deleted cap refused --
// and then a paced collection has to complete at that size, which a 38 MiB LIVE
// set cannot do at the default 0.5 ms budget: marking alone is 2.4 million
// granules, i.e. tens of thousands of ticks, and agents/gc.md's reclaim-rate
// table says so in advance. So the blocks are taken, the memory is grown, and
// all but six megabytes are dropped on tick 1. What is then measured is a paced
// collection over a 40 MiB heap with an 8 MiB live set, with the surviving
// blocks' checksum re-derived every hundred ticks.
const nbulkkeep = 6
