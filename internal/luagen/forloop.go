package luagen

import (
	"fmt"
	"strconv"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
)

// Lowering a counted loop to Lua's numeric `for`.
//
// The emitted loop stops being a label, an increment, a compare and a goto, and
// becomes one FORLOOP opcode with the counter in a register the loop owns:
//
//	::L1::                                for v1 = v1, v0 - 1 do
//	v2 = (v2 + v1) % 4294967296.0    -->     v2 = (v2 + v1) % 4294967296.0
//	v1 = v1 + 1                           end
//	if v1 < v0 then goto L1 end
//
// Two things about that output are load-bearing and neither is obvious.
//
// The control variable REUSES the wasm local's name. Lua scopes a `for`
// variable to its own loop, so the name inside the loop is a fresh local that
// shadows the outer one -- which is exactly what the body wants, since the
// analysis has already refused any loop whose body writes the counter. It also
// means `for v1 = v1, ...` reads the OUTER v1 for its initial value, because
// Lua evaluates the three header expressions in the enclosing scope before it
// creates the loop variable. That is what lets the lowering work without
// knowing what the counter started at.
//
// The outer name is therefore STALE after the loop, and `Materialise` says when
// that matters. Nothing else in the emitter has to know: the analysis has
// already refused the cases where the right final value is not simply the bound.
//
// This is the one place the emitter declares a local after the prologue, so it
// is the one place Invariant B is bent. Not broken: the invariant exists because
// Lua rejects a goto that jumps INTO a local's scope, and wasm's structured
// control flow has no way to name a label inside a loop body from outside it.
// third_party/lua-5.2.1/sandbox_check.lua asserts the three shapes that follow
// from putting a `for` in flat, goto-based output.

// planCountedLoops decides which loops in f are lowered, and indexes them by the
// steps the emitter will meet.
func (b *builder) planCountedLoops(f *ir.Func) {
	b.cl, b.clEnd, b.clDrop = nil, nil, nil
	if !b.countedLoops() || f.Unsupported != nil {
		return
	}
	found := analysis.CountedLoops(f, b.w)
	if len(found) == 0 {
		return
	}
	b.cl = map[int]*analysis.Counted{}
	b.clEnd = map[int]*analysis.Counted{}
	b.clDrop = map[int]bool{}
	for h, c := range found {
		// A spilled counter lives in FS[fb+k], and a table element cannot be a
		// `for` control variable. The frame stack is a capability rather than an
		// optimization -- it runs at every level -- so this is a real case and
		// not a theoretical one.
		if _, spilled := b.sp.At(c.Slot); spilled {
			continue
		}
		b.cl[h] = c
		b.clEnd[c.Close] = c
		for s := range c.Drop {
			b.clDrop[s] = true
		}
	}
}

// countedLoops gates the lowering. O1, because it assumes nothing beyond the
// wasm spec -- no shadow-stack convention, no whole-module property -- which is
// the line this project draws between level 1 and the levels above it.
func (b *builder) countedLoops() bool { return b.opt >= analysis.O1 }

// emitForHeader prints the `for` that replaces a loop's label.
func (b *builder) emitForHeader(f *ir.Func, c *analysis.Counted, fw *forwarding) {
	name := b.slotName(c.Slot)
	limit := b.loopBound(c, fw, c.Adjust)

	// A fuel charge still belongs at the loop header: every iteration passes
	// through it exactly once. It goes INSIDE the `for`, which is where the
	// iterations are, rather than in front of it.
	// A loop with more than one way out gets a control variable of the pass's
	// own, and one copy into the wasm local per iteration. That keeps the local
	// current at EVERY point in the body, so an edge leaving from the middle
	// needs nothing said about it -- which is what makes the multi-exit shape
	// lowerable at all, since Lua's `for` variable does not outlive its loop.
	//
	// Measured on `count`, whose body is small enough for one extra OP_MOVE to
	// show if it were going to: 0.844x with the copy against 0.847x without,
	// A/A floor 1.9%. No detected cost, so this is not a reason to prefer the
	// direct form -- the direct form is kept only because it is what the
	// single-exit case already measured and ships.
	ctrl := name
	if c.CopyEachIteration {
		ctrl = forCtrlName(c.Header)
	}
	if c.Step == 1 {
		b.line("for %s = %s, %s do", ctrl, name, limit)
	} else {
		b.line("for %s = %s, %s, %d do", ctrl, name, limit, c.Step)
	}
	b.indent++
	if c.CopyEachIteration {
		b.line("%s = %s", name, ctrl)
	}
	if b.opts.Fuel > 0 {
		b.line("FUEL = FUEL - 1 if FUEL < 0 then trap_fuel() end")
	}
}

// emitForEnd closes the loop, restores the counter if anything reads it, and
// takes the exit edge.
func (b *builder) emitForEnd(c *analysis.Counted, fw *forwarding) {
	b.indent--
	b.line("end")
	if c.Materialise {
		b.line("%s = %s", b.slotName(c.Slot), b.loopBound(c, fw, c.FinalAdjust))
	}
	// A top-tested loop left by a br_if, and falling out of the `for` has to
	// take that edge explicitly -- the body no longer contains anything that
	// jumps there. Skipped when the label it names is the next thing emitted,
	// which is the common case (the loop is the last thing in its block) and
	// would otherwise put `goto L0` immediately above `::L0::`.
	if c.TopTested && !c.ExitTo.IsReturn() && !c.ExitFallsThrough {
		b.line("goto %s", labelName(c.ExitTo.Label))
	}
}

// loopBound prints the loop's bound plus a constant, folding when the bound is
// itself a numeral so the common `for i = i, 9` does not go out as `10 - 1`.
//
// The bound comes from the forwarding table rather than from the slot name
// because that is where the emitted text lives -- but the analysis has already
// restricted it to a constant or a loop-invariant local, so what comes back is
// a numeral or a bare name and never an expression with its own side effects or
// its own evaluation order.
func (b *builder) loopBound(c *analysis.Counted, fw *forwarding, adjust int64) string {
	// LimitFrom < 0 is the bare-value condition, whose bound is an implicit
	// zero that no step holds.
	raw := "0"
	if c.LimitFrom >= 0 && c.LimitFrom < len(fw.raw) && c.LimitArg < len(fw.raw[c.LimitFrom]) {
		raw = fw.raw[c.LimitFrom][c.LimitArg]
	}
	if adjust == 0 {
		return raw
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return strconv.FormatInt(n+adjust, 10)
	}
	if adjust < 0 {
		return fmt.Sprintf("%s - %d", raw, -adjust)
	}
	return fmt.Sprintf("%s + %d", raw, adjust)
}

// nativeIntrinsics gates replacing a guest's memcpy/memset with the runtime's
// own. O1, because it assumes nothing about the module beyond what
// analysis.NativeIntrinsic checks for itself -- and because -opt=0 has to keep
// reproducing the M4 emitter byte for byte.
func (b *builder) nativeIntrinsics() bool { return b.opt >= analysis.O1 }
