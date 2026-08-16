package luagen

import "github.com/Techrocket9/fklua/internal/analysis"

// Inlining the sub-word LOADS.
//
// `ld8` is two nested calls deep -- `ld8(mem, size, a)` bounds-checks and then
// calls `ld8raw`, which does the extraction -- and the load-cost breakdown in
// agents/optimizer.md puts a single call at 34% of an access. Expanding it at
// the use site removes both:
//
//	t0 = <address>
//	if t0 < 0 or t0 + 1 > MEMSIZE then trap_oob() end
//	t1 = t0 % 4                       -- the byte's position in its word
//	t2 = MEM[(t0 - t1) / 4 + 1]       -- the containing word
//	t1 = P2[8 * t1]                   -- 2^(8*position)
//	v9 = ((t2 - t2 % t1) / t1) % 256
//
// **Measured: 0.776x on `real_entities` through the real TinyGo guest**, against
// a ~1% A/A floor with checksums compared -- 1.29x, on the kernel
// agents/benchmarks.md calls the closest thing to a typical mod inner loop.
//
// # Why this was not done with the byte STORES, and why that was a gap
//
// agents/optimizer.md says sub-word accesses stay calls because `st8b`'s body is
// a read-modify-write of the containing word, needing "the byte's position
// within the word, the word index, a power-of-two divisor out of `P2` and the
// old word all live at once -- five values against the two scratch registers a
// function declares".
//
// Every word of that is about a STORE. A load has no read-modify-write, no old
// word to preserve and nothing to write back: it needs three values, and the
// 4-scratch tier the inlined 8-byte access already introduced covers it with
// room to spare. Loads and stores were reasoned about together and only the
// store's constraint was written down, so the load's much weaker one was never
// checked. The stores genuinely do stay calls.
//
// It is also the safest expansion in the emitter, and for a reason worth stating
// because the opposite case has bitten three times: **a load records nothing**.
// The dirty-page mark, which every inlined STORE has to reproduce and which
// has been the source of three separate save-corruption bugs, simply does not
// apply. The bounds check is kept -- this buys the call, not the check -- so
// nothing about memory safety changes either.

// inlineByteLoads gates the expansion. O3, alongside the other inlined accesses,
// and for the same reason: -opt=0 has to keep reproducing the M4 emitter byte
// for byte, and levels 1 and 2 are kept identical by gating on one predicate
// rather than by care.
func (b *builder) inlineByteLoads() bool { return b.opt >= analysis.O3 }

// emitInlineLoad8 expands i32.load8_u / i32.load8_s at the use site.
//
// `t0` is reused for the extracted byte once the address has been consumed, so
// the signed form needs no fourth register of its own.
//
// # Sharding: the NO-ELSE form, and it is the whole reason this stayed free
//
// A single byte cannot straddle anything, so unlike every wider access this has
// no fallback call and one shared tail -- which is exactly the condition
// shardSlow/shardRebase need. `t2` starts as shard 0 and the slow arm rewrites
// it, along with rewriting `t0` into a within-shard offset, so the extraction
// below is written once and there is no jump for the fast path to pay.
//
// Counted against the flat form: `t2 = S1` adds a GETUPVAL and `t2[...]` drops
// one, the merged test replaces the bounds check rather than joining it, and
// the branch count is unchanged. Byte loads are the densest thing in a string
// kernel -- eight in one unrolled FNV loop -- and the if/else shape cost 7-10%
// there. This costs nothing.
func (b *builder) emitInlineLoad8(addr string, memOff uint32, dst string, signed bool) {
	b.line("t0 = %s", addrExpr(addr, memOff))
	b.line("t2 = S1")
	b.line("if %s then", shardSlow(1))
	b.shardRebase("t2", "t1", 1)
	b.line("end")
	b.line("t1 = t0 %% 4")
	b.line("t2 = t2[(t0 - t1) / 4 + 1]")
	b.line("t1 = P2[8 * t1]")
	if !signed {
		b.line("%s = ((t2 - t2 %% t1) / t1) %% 256", dst)
		return
	}
	b.line("t0 = ((t2 - t2 %% t1) / t1) %% 256")
	b.line("%s = t0 >= 128 and t0 + 4294967040.0 or t0", dst)
}

// emitInlineLoad16 expands i32.load16_u / i32.load16_s.
//
// The two bytes are fetched independently rather than through a same-word fast
// path. A 2-byte access that does not straddle a word boundary re-reads the same
// word, which is one wasted table read -- and adding the branch to avoid it
// costs a test on every 16-bit load to save a table read on most of them. The
// measured shape is this one; if a same-word path is ever added it should be
// measured, not assumed.
//
// The bounds check covers both bytes up front, so the second fetch cannot be the
// one that trips -- which is also what the spec requires, since a trapping
// access must leave nothing half-done.
// # Sharding, and why THIS one keeps a call in its else arm
//
// Two bytes CAN land in two shards -- at the last byte of a shard the second
// byte is the first byte of the next -- and this lowering fetches each byte
// through its own address arithmetic. Reproducing the shard select twice, once
// per byte, would double the size of a lowering whose entire purpose was to
// remove a call, for the arm taken only above 2 MiB. So the fast arm is exactly
// the flat body with S1 in place of MEM, and everything else -- out of range,
// above the first shard, and the boundary straddle -- is ld16's, which walks
// byte by byte through ld8raw and selects a shard per byte.
//
// The cost is honest and it is stated: a 16-bit load above 2 MiB is a call
// again, as it was at every level below -opt=3. It is the one access shape
// sharding makes no faster above the wall, and it is not one of the shapes the
// census weights heavily.
func (b *builder) emitInlineLoad16(addr string, memOff uint32, dst string, signed bool) {
	b.line("t0 = %s", addrExpr(addr, memOff))
	b.line("if %s then", shardFast(2, true))
	b.line("  t1 = t0 %% 4")
	b.line("  t2 = S1[(t0 - t1) / 4 + 1]")
	b.line("  t1 = P2[8 * t1]")
	b.line("  t3 = ((t2 - t2 %% t1) / t1) %% 256")
	b.line("  t0 = t0 + 1")
	b.line("  t1 = t0 %% 4")
	b.line("  t2 = S1[(t0 - t1) / 4 + 1]")
	b.line("  t1 = P2[8 * t1]")
	b.line("  t0 = t3 + (((t2 - t2 %% t1) / t1) %% 256) * 256")
	b.line("else t0 = ld16(MEM, MEMSIZE, t0) end")
	if !signed {
		b.line("%s = t0", dst)
		return
	}
	b.line("%s = t0 >= 32768 and t0 + 4294901760.0 or t0", dst)
}
