package luagen

import "fmt"

// SHARDED LINEAR MEMORY: the emitter's half.
//
// `MEM` is a vector of shards, always -- no mode flag, no runtime transition,
// no compile-time gate. Every emitted access opens with a test it already had,
// and below 2 MiB that test IS the bounds check unchanged. See
// agents/sharding.md, "The bounds check IS the shard test".
//
// # The three forms, and when each is emitted
//
// STATIC FOLD -- the address operand is a compile-time constant and 4-aligned.
// Both the shard index and the word index inside it are folded here, so the
// access is one table index off a literal:
//
//	t0 = <address>                                -- still evaluated, see below
//	if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
//	v9 = S1[513]                                  -- or MEM[3][513] above shard 0
//
// 15.5% of this repo's corpus and 26.2% with the downstream mod -- the biggest
// single class, and 952 of its 962 sites are literal addresses rather than a
// range-analysis result. A CONSTANT ADDRESS DOES NOT NEED SHARD 0, it needs no
// runtime selection for ANY shard, so this is not tied to the first shard's
// bound. Measured: no constant address exceeds shard 0 in any corpus module
// today, but rustc's static image is already 1,053,496 bytes -- half the line.
//
// The address is STILL EVALUATED into t0 and the bounds check is still emitted.
// The range analysis proves the VALUE, not that the expression is free of
// effects or of traps the peephole was allowed to forward into it, and MEMSIZE
// is a runtime quantity that a host `adopt` can move. What the fold buys is the
// index arithmetic and the shard select, which is what it costs.
//
// GUARD-HOISTED -- see loopguard.go. One conjunct on the entry test proves the
// whole span lies in one shard, and the shard's table is hoisted into a local
// beside the word index.
//
// SHARD-0 FAST PATH -- everything else, and 68-77% of the corpus. The bounds
// check and the shard test are one test:
//
//	t0 = <address>
//	if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then v9 = S1[t0 / 4 + 1] else
//	  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
//	  t1 = t0 % 2097152
//	  v9 = MEM[(t0 - t1) / 2097152 + 1][t1 / 4 + 1]
//	end
//
// Below 2 MiB `SHBOUND` IS `MEMSIZE`, so the opening test is the bounds check
// rather than an addition to it, the fast arm is today's access unchanged, and
// the else arm is unreachable for any address that is not a trap. Measured
// paired in game: 0.93-0.97x on loads and 1.00-1.01x on stores; 0.98x end to
// end at 2 MiB. Above 2 MiB the same test decides shard 0 versus everything
// else and the else arm is the divmod, which at 90-100 ns is 35-45x better than
// the flat form it replaces.
//
// ANY IMPLEMENTATION THAT EMITS THE SHARD SELECT *IN ADDITION TO* THE BOUNDS
// CHECK HAS THROWN THE RESULT AWAY -- that form measures 1.46-1.59x below the
// wall, and it is the control arm the e2e harness still carries as `slow`.
//
// # What the else arm may and may not inline
//
// A 4-byte ALIGNED access never straddles a shard: a shard is 524,288 whole
// words. So the 4-byte else arm inlines the divmod, and only the UNALIGNED case
// falls to the runtime helper.
//
// An 8-byte aligned access CAN straddle -- at offset 2097148 its two words are
// in two different tables -- so every 8-byte else arm delegates to st64 / ld32
// / ld_f64 / st_f64, which handle the straddle and, for the store, honour the
// spec's rule that an out-of-range store leaves memory untouched. Reproducing
// that shape inline at every 8-byte access site would be a second copy of the
// one correctness rule with no analogue in the flat representation, which is
// the same trade the dirty-page mark already refuses for wide stores.

const (
	// shardBytes is the shard size in BYTES: 2^19 words of 4. Exactly half the
	// 2^20-key wall, so a shard can never stop being an array however the
	// memory grows, and a power of two so the select is two opcodes.
	shardBytes = int64(1) << 21
	// shardWords is the same in words.
	shardWords = shardBytes / 4
)

// constAddr reports the compile-time value of the address operand of the access
// at step i, when the range analysis has pinned it to one.
//
// Nil-safe on b.w: the range analysis only runs at -opt>=1, and every caller is
// gated at -opt=3, but a future caller should not have to know that.
func (b *builder) constAddr(i int) (int64, bool) {
	if b.w == nil {
		return 0, false
	}
	c, ok := b.w.ArgRange(i, 0).ConstU32()
	if !ok {
		return 0, false
	}
	return int64(c), true
}

// staticRef prints the folded table reference for a constant byte address:
// `S1[j]` inside shard 0, `MEM[k][j]` above it.
//
// The caller must have checked 4-alignment; a shard is a whole number of words,
// so nothing here can straddle.
func staticRef(addr int64) string {
	s := addr / shardBytes
	w := (addr%shardBytes)/4 + 1
	if s == 0 {
		return fmt.Sprintf("S1[%d]", w)
	}
	return fmt.Sprintf("MEM[%d][%d]", s+1, w)
}

// staticFold reports whether the access at step i folds to a constant shard and
// word index, and prints it when it does.
//
// Refused for an unaligned constant -- a 4-byte access at 1 mod 4 is two words
// and the runtime helper owns it -- and for an address at or past 2^32, which
// the range analysis cannot produce for a u32 operand but which the static
// offset can push past.
func (b *builder) staticFold(i int, memOff uint32, width int64) (string, bool) {
	c, ok := b.constAddr(i)
	if !ok {
		return "", false
	}
	a := c + int64(memOff)
	if a%4 != 0 || a < 0 || a+width > int64(1)<<32 {
		return "", false
	}
	return staticRef(a), true
}

// shardFast prints the merged opening test: in range, inside the first shard,
// and (unless the congruence analysis already proved it) 4-aligned.
//
// `aligned` drops the `% 4` conjunct. The bounds half is `t0 >= 0 and
// t0 + w <= SHBOUND`, which below 2 MiB is exactly the bounds check the flat
// form emitted, spelled the other way round because the fast arm is now the
// then-branch.
func shardFast(width int64, aligned bool) string {
	s := fmt.Sprintf("t0 >= 0 and t0 + %d <= SHBOUND", width)
	if !aligned {
		s += " and t0 % 4 == 0"
	}
	return s
}

// shardSlow prints the NEGATION of shardFast: out of range, or out of the first
// shard. It is what the no-else form below branches on.
//
// # Why a second shape exists at all, and it is not a style choice
//
// `if fast then A else B end` compiles in Lua 5.2 to: test, jump-to-else, A,
// JUMP-TO-END, else: B. The fast path therefore pays ONE unconditional jump
// that the flat form -- whose bounds check was `if bad then trap_oob() end`,
// an `if` with no else -- did not.
//
// One jump out of the ~19 VM instructions an inlined byte load costs is ~5%,
// and `real_names` has EIGHT of them in its unrolled FNV loop. Measured: 1.07x
// on TinyGo and 1.10x on Rust with the if/else form, and the only below-wall
// regression in the whole corpus. It is not visible on real_entities, which has
// the same loads and more other work around them.
//
// The no-else form gets it back exactly:
//
//	t2 = S1
//	if t0 < 0 or t0 + 1 > SHBOUND then
//	  if t0 < 0 or t0 + 1 > MEMSIZE then trap_oob() end
//	  t1 = t0 % 2097152
//	  t2, t0 = MEM[(t0 - t1) / 2097152 + 1], t1
//	end
//	t1 = t0 % 4
//	t2 = t2[(t0 - t1) / 4 + 1]
//
// The trick is that the slow arm REWRITES t0 into a within-shard offset and t2
// into that shard's table, so the tail is shared and there is nothing to jump
// over. Counted against the flat form: `t2 = S1` is one GETUPVAL added, and
// `t2[...]` is a register GETTABLE where `MEM[...]` was GETUPVAL plus GETTABLE
// -- so the GETUPVAL simply MOVED. Same instruction count, same branch count.
// Measured back to 1.00x.
//
// It only works where the tail is a single shared expression. An access whose
// slow arm can end in a CALL -- an unaligned 4-byte load falling to ld32, a
// 16-bit load falling to ld16 -- has two different tails and keeps the if/else,
// where the extra jump is paid on a path that was going to be a call anyway.
func shardSlow(width int64) string {
	return fmt.Sprintf("t0 < 0 or t0 + %d > SHBOUND", width)
}

// shardRebase prints the slow arm of the no-else form: trap if genuinely out of
// range, then rewrite `t0` into a within-shard offset and `tab` into its shard.
//
// `tmp` is the scratch that holds the offset between the two assignments. The
// multiple assignment is what makes one scratch enough -- Lua evaluates the
// whole right-hand side before assigning any of it, so `tab, t0 = MEM[...], tmp`
// reads the old t0 inside the index it is about to overwrite.
func (b *builder) shardRebase(tab, tmp string, width int64) {
	b.line("  if t0 < 0 or t0 + %d > MEMSIZE then trap_oob() end", width)
	b.line("  %s = t0 %% %d", tmp, shardBytes)
	b.line("  %s, t0 = MEM[(t0 - %s) / %d + 1], %s", tab, tmp, shardBytes, tmp)
}

// shardSlowRef prints the runtime shard select for a 4-aligned address in t0,
// using `tmp` to hold the within-shard offset.
//
// Two opcodes and a subtraction: `t0 % 2097152` and `(t0 - that) / 2097152`.
// bit32.rshift/band would be two C calls and measured 1.5x worse -- the
// "prefer arithmetic" rule in CLAUDE.md holds here.
func shardSlowRef(tmp string) string {
	return fmt.Sprintf("MEM[(t0 - %s) / %d + 1][%s / 4 + 1]", tmp, shardBytes, tmp)
}

// shardSlowRefNoTmp is the same with no scratch register to spare: the
// within-shard offset is computed twice rather than held.
//
// Used by the inlined STORE, whose second scratch already holds the value. One
// extra modulo on the arm taken only above 2 MiB is a far better trade than
// raising every storing function's scratch count from two to four, which is two
// more locals against Lua's per-function 180 and pushes cold slots onto the
// frame stack.
func shardSlowRefNoTmp() string {
	return fmt.Sprintf("MEM[(t0 - t0 %% %d) / %d + 1][t0 %% %d / 4 + 1]",
		shardBytes, shardBytes, shardBytes)
}
