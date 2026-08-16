// Package analysis holds the optimization passes the emitter consults.
//
// Nothing here rewrites IR in place. Each pass answers questions -- "may this
// wrap be dropped?", "which shadow-stack slots are private to this frame?",
// "which locals must spill?" -- and the emitter acts on the answers. That keeps
// the IR a single source of truth and makes every pass switchable at a
// granularity finer than a rebuild: -opt=0 simply never asks.
//
// The two invariants from CLAUDE.md constrain every answer given here:
//
//	Invariant A -- an i32 is an unsigned integral double in [0, 2^32).
//	Invariant B -- no `local` statement appears after the prologue.
//
// Invariant B is why the spill pass produces indices into a chunk-level table
// rather than fresh locals, and Invariant A is what makes the range analysis
// an interval over unsigned values with no sign case at all.
package analysis

import "fmt"

// Level is the -opt setting. Each level is a superset of the one below it.
//
// The split is by risk and by measured payoff, not by ambition: O1 is a
// peephole that cannot change program meaning, O2 changes where a value lives,
// and O3 changes how a call is dispatched.
type Level int

const (
	// O0 disables every pass. Byte-for-byte the output of the M4 emitter, which
	// is what makes it the reference when a miscompile has to be bisected
	// against the optimizer rather than against the lowering.
	O0 Level = iota

	// O1 enables the block-local peephole: expression forwarding, wrap
	// elision driven by range analysis, folding a comparison into the branch
	// that consumes it.
	O1

	// O2 adds typed-slot promotion, which moves a shadow-stack slot whose
	// address never escapes into a Lua local.
	O2

	// O3 adds upvalue promotion, which hoists a caller's hottest callees out of
	// the F table into chunk-level upvalues.
	O3
)

// DefaultLevel is what `fklua compile` and `fklua mod` use when -opt is absent.
//
// O3 since M7. It was O2 for two milestones because upvalue promotion spends
// chunk-level locals, the scarcest resource a generated chunk has -- the
// prelude alone takes 167 of Lua's 200. Three things had to be true before
// defaulting to it, and all three were measured rather than assumed:
//
//   - **It cannot break a build that worked.** upvalueBudget backs promotion off
//     one name for every name the chunk already spends, so a crowded chunk
//     promotes fewer callees rather than overflowing; at 32 globals it still
//     promotes 3, and the chunk lands on the margin instead of past it.
//     TestPromotionLeavesTheMarginItPromises sweeps the fill levels.
//   - **M7 does not compete for those names.** This was the stated reason to
//     wait, and it turned out to be false: the handle table (35 locals), the
//     member table and control.lua are separate `require`d files, so each gets
//     its own 200. The guest chunk is 167 at O2 and 188 at O3 either way.
//   - **It is never slower.** Five runs of `fklua bench --opt`: fib 18.1 -> 15.6
//     ns/op, and sum, count, chase, prng, dot and frame all inside run-to-run
//     noise. A single earlier run showed sum 10% worse and did not reproduce.
//
// A guest that wants the O2 shape -- byte-identical output for a miscompile
// bisect, say -- asks for it.
const DefaultLevel = O3

// ParseLevel maps a -opt flag value onto a level.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "0":
		return O0, nil
	case "1":
		return O1, nil
	case "2":
		return O2, nil
	case "3":
		return O3, nil
	}
	return 0, fmt.Errorf("unknown optimization level %q (want 0, 1, 2 or 3)", s)
}

func (l Level) String() string { return fmt.Sprintf("%d", int(l)) }

// Peephole reports whether the block-local expression peephole runs: operand
// forwarding beyond a bare local.get, wrap elision, comparison folding.
func (l Level) Peephole() bool { return l >= O1 }

// Slots reports whether typed-slot promotion runs.
func (l Level) Slots() bool { return l >= O2 }

// Upvalues reports whether hot callees are promoted to chunk-level upvalues.
func (l Level) Upvalues() bool { return l >= O3 }
