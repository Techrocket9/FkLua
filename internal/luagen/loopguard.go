package luagen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
)

// Hoisting a loop's bounds checks and alignment tests behind one entry guard.
//
// A guarded access computes no address at all. The loop keeps a WORD INDEX per
// base -- the base over four, plus one -- stepped alongside the base itself, so
// the access is one table read:
//
//	if lg41 then v11 = MEM[lw41_0 + 3] else
//	  t0 = ((v1 + 12) % 4294967296.0)      -- the address SINKS into this arm
//	  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
//	  ...
//	end
//
// **Measured, real compiler, checksums compared, interleaved under lua52f:**
// `pure_sum` 0.424x (TinyGo) and 0.417x (Rust); `pure_dot` 0.761x once 8-byte
// accesses and a second base were admitted. The two halves of the guard
// separately are 0.773x (bounds) and 0.934x (alignment), and for scale the
// counted-loop `for` is worth 0.950x on the same loop: per-access memory
// overhead outweighs loop overhead by about seven to one on real guest code.
//
// The unguarded arm is kept verbatim rather than replaced by a trap. A guard
// that fails is a loop this pass could not describe, not a program about to
// fault, and the commonest reason to fail is an unaligned base -- which the
// general path handles perfectly well.
//
// # What the guard evaluates
//
// Everything, at runtime, once, before the loop's label -- so the back edge
// jumps past it. `t0` is the scratch the body will reuse, which is free because
// the guard has finished before the body starts:
//
//	t0 = v11 - v5                             -- bound minus counter
//	if t0 > 0 and t0 % 4 == 0 then            -- reachable in whole steps
//	  t0 = t0 / 4 - 1                         -- iterations, less one
//	  lg41 = <base 0 in range and aligned> and <base 1 ...>
//	  if lg41 then lw41_0 = ... lw41_1 = ... end
//	else lg41 = false end
//
// The divisibility test is what an `i32.ne` loop needs to have a trip count at
// all: the counter walks past its bound and wraps if the difference is not a
// whole number of steps, and then the loop runs about four billion times rather
// than the handful the guard assumed. Refusing that is the correctness
// condition, not a precision loss.

// guardName is a loop's guard flag; wordName is the word index for one of its
// bases. One local each, declared in the prologue like everything else --
// Invariant B admits no `local` after the first `::label::`, and a guard sits at
// a loop header, which is one.
//
// # Why the `l` prefix, and why it is not cosmetic
//
// Both names are indexed by a STEP index, and every other name family the
// emitter emits is indexed by something else entirely: a global by its MODULE
// GLOBAL index, a slot by its slot number, a promoted callee by its function
// index. Two families sharing a spelling therefore collide whenever their two
// unrelated counters happen to meet.
//
// These were `g%d` and `w%d_%d`, and `g%d` is also `globalName` -- so a guarded
// loop whose header step index was below the module's global count declared a
// function-scoped local with a module global's name. Lua's scoping does the rest
// silently: for the whole of that function the global is SHADOWED, a
// `global.set` writes the guard flag instead, a `global.get` reads a boolean,
// and the emitter has no way to notice. `g0` is TinyGo's shadow-stack pointer
// and step 0 is an ordinary header, so the reachable case was the worst one.
//
// The rule that prevents the next instance is not "pick a different letter", it
// is that **a name family indexed by a step index owns a prefix nothing else
// uses**. `lg`/`lw` are that prefix here, `fk%d` (the counted loop's control
// variable, also a step index) is the other one, and
// TestNoNameFamilyCanCollideWithAnother enumerates every family the emitter can
// emit and proves the sets disjoint over any indices at all.
func guardName(header int) string { return fmt.Sprintf("lg%d", header) }

func wordName(header, base int) string { return fmt.Sprintf("lw%d_%d", header, base) }

// shardName is the base's SHARD TABLE, hoisted beside its word index.
//
// The guard proves the whole span lies inside ONE shard, so the shard is
// loop-invariant and the access is one table index off a local -- which is the
// whole point: `ls41_0[lw41_0 + 3]` is exactly what `MEM[lw41_0 + 3]` was, and
// the word index simply became a WITHIN-SHARD index. Measured flat at 24-43 ns
// from 1 MiB to 8 MiB where the flat guarded form goes 26.8 -> 3,886 ns.
//
// Same prefix rule as the other two: `ls` is indexed by a STEP index and owns a
// prefix nothing else uses. It is in agents/codegen.md's table and in
// TestNoNameFamilyCanCollideWithAnother, which is a proof over every index
// rather than over the guests that happen to be checked in.
//
// COST: one more local per base, on top of the flag and the word index -- so up
// to 9 rather than 6 at three guards of three bases. maxGuardsPerFunc is what
// keeps that bounded.
func shardName(header, base int) string { return fmt.Sprintf("ls%d_%d", header, base) }

// maxGuardsPerFunc caps the flags one function may declare.
//
// A function already spends up to ir.MaxSlots (180) locals plus four scratch
// registers plus its promoted frame slots, against Lua's hard 200. The guards
// come out of what is left -- and each now costs one local per BASE on top of
// its flag -- so they are capped rather than allowed to push a function over a
// cliff whose error message names neither the loop nor the pass.
const maxGuardsPerFunc = 3

// planLoopGuards decides which loops in f get a guard.
func (b *builder) planLoopGuards(f *ir.Func) {
	b.lg, b.lgAccess = nil, nil
	if !b.loopGuards() || f.Unsupported != nil {
		return
	}
	found := analysis.LoopGuards(f)
	if len(found) == 0 {
		return
	}
	// Deterministic order: a map's iteration order is not stable, and which
	// loops get the last flags must not depend on it. Generated code that
	// differs between runs is a desync in a lockstep game, not a cosmetic
	// nuisance.
	headers := make([]int, 0, len(found))
	for h := range found {
		headers = append(headers, h)
	}
	sort.Ints(headers)

	b.lg, b.lgAccess = map[int]*analysis.LoopGuard{}, map[int]*analysis.LoopGuard{}
	for _, h := range headers {
		if len(b.lg) >= maxGuardsPerFunc {
			break
		}
		g := found[h]
		// A spilled base, counter or bound lives in FS[fb+k]. The guard could
		// name it, but the frame stack is a capability that runs at every level
		// and the arithmetic below assumes plain locals, so refuse rather than
		// put a table index in the hot path.
		if b.guardSpills(f, g) {
			continue
		}
		b.lg[h] = g
		for s := range g.Steps {
			b.lgAccess[s] = g
		}
	}
}

// guardSpills reports any local the guard must name living on the frame stack.
func (b *builder) guardSpills(f *ir.Func, g *analysis.LoopGuard) bool {
	spilled := func(local uint32) bool {
		_, ok := b.sp.At(f.LocalSlot(local))
		return ok
	}
	if spilled(g.Ctr) || (!g.BoundIsConst && spilled(g.Bound)) {
		return true
	}
	for _, base := range g.Bases {
		if spilled(base.Local) {
			return true
		}
		if base.Affine && (spilled(base.AffineIV) || spilled(base.AffineInv)) {
			return true
		}
	}
	return false
}

// loopGuards gates the pass at O3, where the accesses it specialises are already
// inlined. Below that a load is a call and there is no per-access check in the
// generated code to hoist.
func (b *builder) loopGuards() bool { return b.opt >= analysis.O3 && b.inlineLoads() }

// guardLocals is the prologue declaration: one flag per guard and one word index
// per base, initialised rather than merely declared.
//
// Initialised because on a path where the guard is false a word index is never
// read but IS still stepped, and `nil + 8` is an error where a wasted add is not.
func (b *builder) guardLocals() []string {
	if len(b.lg) == 0 {
		return nil
	}
	hs := make([]int, 0, len(b.lg))
	for h := range b.lg {
		hs = append(hs, h)
	}
	sort.Ints(hs)
	var names, inits []string
	for _, h := range hs {
		names = append(names, guardName(h))
		inits = append(inits, "false")
		for k := range b.lg[h].Bases {
			names = append(names, wordName(h, k))
			inits = append(inits, "0")
			// The shard table initialises to `false`, not to a real table, and
			// that asymmetry with the word index is deliberate. A word index is
			// STEPPED on a path where the guard is false, so `nil + 8` has to be
			// impossible; a shard table is only ever INDEXED, and only under the
			// flag. Seeding it with a plausible table would turn a missing seed
			// into silently-wrong data, which is the failure class the guard
			// audit exists for. `false` makes it an error at the first read.
			names = append(names, shardName(h, k))
			inits = append(inits, "false")
		}
	}
	return []string{strings.Join(names, ", ") + " = " + strings.Join(inits, ", ")}
}

// baseEntry prints a base's value at loop entry, where every local it names
// still holds the value it entered with.
func (b *builder) baseEntry(f *ir.Func, gb analysis.GuardBase) string {
	if !gb.Affine {
		return b.slotName(f.LocalSlot(gb.Local))
	}
	// An affine base is not yet assigned when the guard runs -- the loop
	// recomputes it from the induction variable each iteration -- so the guard
	// reconstructs it from the two locals it is the sum of.
	return fmt.Sprintf("((%s + %s) %% %s)",
		b.slotName(f.LocalSlot(gb.AffineIV)),
		b.slotName(f.LocalSlot(gb.AffineInv)), wrapMod)
}

// emitLoopGuard prints the entry test, immediately before the loop's label.
func (b *builder) emitLoopGuard(f *ir.Func, g *analysis.LoopGuard) {
	name := guardName(g.Header)
	ctr := b.slotName(f.LocalSlot(g.Ctr))
	bound := fmt.Sprintf("%d", g.BoundConst)
	if !g.BoundIsConst {
		bound = b.slotName(f.LocalSlot(g.Bound))
	}

	// The difference is taken in the direction of travel, so a countdown --
	// which is how rustc closes an unrolled loop -- reads the same way as a
	// count up.
	mag := g.Step
	if mag < 0 {
		mag = -mag
	}
	b.line("-- one entry test covers %d access(es) over %d base(s)", len(g.Steps), len(g.Bases))
	if g.Step > 0 {
		b.line("t0 = %s - %s", bound, ctr)
	} else {
		b.line("t0 = %s - %s", ctr, bound)
	}
	if g.ExactTrips && mag != 1 {
		b.line("if t0 > 0 and t0 %% %d == 0 then", mag)
	} else {
		b.line("if t0 > 0 then")
	}
	b.indent++
	if mag != 1 {
		b.line("t0 = t0 / %d - 1", mag)
	} else {
		b.line("t0 = t0 - 1")
	}

	// t0 is now the iteration count less one. Each base contributes its own
	// conjunct: non-negative, 4-aligned, and its whole span inside MEMSIZE. The
	// entry expression is printed rather than held in a scratch, because scratch
	// registers are scarce and this runs once.
	var terms []string
	for _, gb := range g.Bases {
		e := b.baseEntry(f, gb)
		terms = append(terms, fmt.Sprintf("%s >= 0 and %s %% 4 == 0 and %s + %d <= MEMSIZE and %s",
			e, e, b.baseSpan(f, gb), gb.MaxEnd, b.sameShard(f, gb)))
	}
	b.line("%s = %s", name, strings.Join(terms, " and "))

	// The seed. `t1` holds the base's WITHIN-SHARD offset for the length of one
	// base's two assignments -- it is a pre-declared scratch and the guard has
	// finished with everything else by the time this runs, which is the same
	// argument that lets the guard reuse t0 for the trip count. Any function
	// with a guarded access has at least two scratches: every op a guard can
	// cover is one scratchCount already asks two for.
	var seeds []string
	for k, gb := range g.Bases {
		e := b.baseEntry(f, gb)
		seeds = append(seeds, fmt.Sprintf("t1 = %s %% %d %s = t1 / 4 + 1 %s = MEM[(%s - t1) / %d + 1]",
			e, shardBytes, wordName(g.Header, k), shardName(g.Header, k), e, shardBytes))
	}
	b.line("if %s then %s end", name, strings.Join(seeds, " "))

	// The whole written span, marked ONCE, so no guarded store carries its own
	// page marking. The range is the one the bounds test just proved, which is
	// why this is nearly free: it is already computed, and it over-estimates
	// only in the direction that costs a repacked page rather than a lost one.
	//
	// Marking up front is sound for the same reason the bounds check is: an
	// early exit writes FEWER bytes than the span, never more.
	//
	// `MEMPACK.mark` is called unconditionally rather than behind the cached-page
	// test the store leaves use. The test is a saving for a store executed
	// millions of times; this runs once per loop ENTRY, and skipping it keeps
	// the emitted line honest about marking a whole span the cache cannot
	// describe anyway.
	if g.HasStore {
		b.line("if %s and MEMDIRTY then", name)
		b.indent++
		for _, gb := range g.Bases {
			e, span := b.baseEntry(f, gb), b.baseSpan(f, gb)
			b.line("MEMPACK.mark(%s, %s + %d)", e, span, gb.MaxEnd-1)
		}
		b.indent--
		b.line("end")
	}
	b.indent--
	b.line("else %s = false end", name)
}

// baseSpan prints the far end of a base's walk: its entry value plus one stride
// for every iteration after the first. `t0` holds that count at the point this
// is printed.
func (b *builder) baseSpan(f *ir.Func, gb analysis.GuardBase) string {
	e := b.baseEntry(f, gb)
	if gb.Stride == 0 {
		return e
	}
	return fmt.Sprintf("%s + t0 * %d", e, gb.Stride)
}

// sameShard prints the conjunct that proves a base's WHOLE SPAN lies inside one
// shard -- the only thing sharding adds to the entry test.
//
// The obvious spelling is "shard of the first byte == shard of the last", which
// is two floors of two compound expressions. This is the same predicate with
// the algebra done: the span starts at `E % 2097152` inside its shard and runs
// `t0 * stride + MaxEnd` bytes, so it stays inside iff
//
//	E % 2097152 + t0 * stride + MaxEnd <= 2097152
//
// One modulo and one multiply-add, reusing the very `t0 * stride` shape the
// bounds conjunct beside it already prints. `t0` holds the iteration count less
// one at this point, exactly as it does for baseSpan.
//
// SOUND ONLY BECAUSE A STRIDE IS NON-NEGATIVE, which analysis.LoopGuards
// enforces (`c < 0 || c%4 != 0` refuses the base outright), so the far end of
// the walk is always the high end and there is no downward case to fold in.
//
// A SPAN THAT CROSSES A BOUNDARY SIMPLY FAILS THE GUARD and every access in the
// loop takes the shard-0 fast path instead. That is stage B's answer and it
// costs little: a guarded loop's span is almost always far smaller than 2 MiB,
// so it crosses with probability about span/2 MiB. Strip-mining the loop into
// one guarded run per shard piece is the general fix and is stage C's.
func (b *builder) sameShard(f *ir.Func, gb analysis.GuardBase) string {
	if gb.Stride == 0 {
		return fmt.Sprintf("%s %% %d + %d <= %d",
			b.baseEntry(f, gb), shardBytes, gb.MaxEnd, shardBytes)
	}
	return fmt.Sprintf("%s %% %d + t0 * %d + %d <= %d",
		b.baseEntry(f, gb), shardBytes, gb.Stride, gb.MaxEnd, shardBytes)
}

// guardRef prints the whole guarded reference: the base's hoisted SHARD TABLE
// indexed by its within-shard word index plus this access's own offset.
//
// This is the form the design calls guard-hoisted, and it is one table index
// with no arithmetic on the address at all -- the shard select happened once,
// at loop entry, in sameShard's conjunct and the seed beside it.
func (b *builder) guardRef(g *analysis.LoopGuard, i int) string {
	a, ok := analysis.GuardedAccessOffset(g, i)
	if !ok {
		return fmt.Sprintf("%s[%s]", shardName(g.Header, 0), wordName(g.Header, 0))
	}
	return fmt.Sprintf("%s[%s]", shardName(g.Header, a.Base), b.wordIndex(g, i))
}

// wordIndex prints the guarded WITHIN-SHARD word index for the access at step
// i: its base's word index plus its own offset, in words.
func (b *builder) wordIndex(g *analysis.LoopGuard, i int) string {
	a, ok := analysis.GuardedAccessOffset(g, i)
	if !ok {
		return wordName(g.Header, 0)
	}
	w := wordName(g.Header, a.Base)
	if a.Off == 0 {
		return w
	}
	return fmt.Sprintf("%s + %d", w, a.Off/4)
}

// emitWordSteps advances every word index whose base advances at this step.
func (b *builder) emitWordSteps(step int) {
	for _, h := range b.sortedGuardHeaders() {
		g := b.lg[h]
		for k, gb := range g.Bases {
			if gb.Inc == step && gb.Stride != 0 {
				w := wordName(g.Header, k)
				b.line("%s = %s + %d", w, w, gb.Stride/4)
			}
		}
	}
}

func (b *builder) sortedGuardHeaders() []int {
	hs := make([]int, 0, len(b.lg))
	for h := range b.lg {
		hs = append(hs, h)
	}
	sort.Ints(hs)
	return hs
}

// emitGuardedLoad32 is the inlined i32 load with the guard's fast arm in front.
func (b *builder) emitGuardedLoad32(g *analysis.LoopGuard, i int, dst, addr string, memOff uint32) {
	// The address computation SINKS into the unguarded arm. It is arithmetic on
	// a local with no side effect and nothing else reads t0, so moving it is
	// sound -- and it is most of what the guarded arm was still paying for.
	b.line("if %s then %s = %s else", guardName(g.Header), dst, b.guardRef(g, i))
	b.indent++
	b.line("t0 = %s", addrExpr(addr, memOff))
	b.line("if %s then %s = S1[t0 / 4 + 1] else", shardFast(4, false), dst)
	b.line("  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
	b.line("  if t0 %% 4 == 0 then %s = %s", dst, shardSlowRefNoTmp())
	b.line("  else %s = ld32(MEM, MEMSIZE, t0) end", dst)
	b.line("end")
	b.indent--
	b.line("end")
}

// emitGuardedLoadF64 is the same for an 8-byte float load.
//
// The guard proves 4-alignment, which is exactly what the inlined 8-byte path
// gates on -- it reads its two words separately -- so one runtime test covers
// both widths. What the guard does NOT prove is that the bits are a normal
// double, so the exponent check and its fallback stay: zero, subnormals,
// infinities and NaNs still route to the helper, which is also what keeps
// exact-NaN mode correct for free, since a boxed NaN can only arrive that way.
func (b *builder) emitGuardedLoadF64(g *analysis.LoopGuard, i int, dst, addr string, memOff uint32) {
	sh := shardName(g.Header, 0)
	if a, ok := analysis.GuardedAccessOffset(g, i); ok {
		sh = shardName(g.Header, a.Base)
	}
	ix := b.wordIndex(g, i)
	b.line("if %s then", guardName(g.Header))
	b.indent++
	// Both words come out of the SAME hoisted shard, and that is what the
	// guard's same-shard conjunct bought: an 8-byte access is the one shape
	// that can straddle, and inside a guard it provably does not.
	b.line("t1 = %s t2 = %s[t1 + 1] t1 = %s[t1]", ix, sh, sh)
	b.line("t3 = t2 %% %s", signMin)
	b.line("t3 = (t3 - t3 %% 1048576.0) / 1048576.0")
	b.line("if t3 > 0 and t3 < 2047 then")
	b.line("  %s = (t2 >= %s and -1.0 or 1.0) * ((t2 %% 1048576.0) * %s + t1 + 4503599627370496.0) * PE[t3]",
		dst, signMin, wrapMod)
	// The non-normal fallback converts the two words already in hand instead of
	// reconstructing a byte address from the word index -- which under sharding
	// it could not do anyway, since the index is now WITHIN a shard and the
	// shard base is not on this path. Strictly cheaper than the call it
	// replaces, and it removes the last place a within-shard index had to be
	// turned back into an absolute address.
	b.line("else %s = %sbits_to_f64(t1, t2) end", dst, b.pfx())
	b.indent--
	b.line("else")
	b.indent++
	b.line("t0 = %s", addrExpr(addr, memOff))
	b.line("if %s then", shardFast(8, false))
	b.line("  t1 = t0 / 4 + 1 t2 = S1[t1 + 1] t1 = S1[t1]")
	b.line("  t3 = t2 %% %s", signMin)
	b.line("  t3 = (t3 - t3 %% 1048576.0) / 1048576.0")
	b.line("  if t3 > 0 and t3 < 2047 then")
	b.line("    %s = (t2 >= %s and -1.0 or 1.0) * ((t2 %% 1048576.0) * %s + t1 + 4503599627370496.0) * PE[t3]",
		dst, signMin, wrapMod)
	b.line("  else %s = %sbits_to_f64(t1, t2) end", dst, b.pfx())
	b.line("else %s = %sld_f64(MEM, MEMSIZE, t0) end", dst, b.pfx())
	b.indent--
	b.line("end")
}

// emitGuardedStore32 is the i32 store, and it still owes the dirty-page mark on
// the UNGUARDED arm.
//
// The guarded arm owes nothing, because the guard marked the whole span once.
// The unguarded one must carry its own: on that path the guard marked nothing.
func (b *builder) emitGuardedStore32(g *analysis.LoopGuard, s ir.Step, fw *forwarding, i int, addr, val string) {
	v := val
	if !fw.dupable[i][1] {
		b.line("t1 = %s", fw.raw[i][1])
		v = "t1"
	}
	b.line("if %s then %s = %s %% %s else", guardName(g.Header), b.guardRef(g, i), v, wrapMod)
	b.indent++
	b.line("t0 = %s", addrExpr(addr, s.Instr.MemOffset))
	b.line("if %s then", shardFast(4, false))
	b.line("  if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end")
	b.line("  S1[t0 / 4 + 1] = %s %% %s", v, wrapMod)
	b.line("else")
	b.line("  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
	b.line("  if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end")
	b.line("  if t0 %% 4 == 0 then %s = %s %% %s else st32(MEM, MEMSIZE, t0, %s) end",
		shardSlowRefNoTmp(), v, wrapMod, v)
	b.line("end")
	b.indent--
	b.line("end")
}
