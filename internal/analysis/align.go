package analysis

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The congruence (alignment) analysis.
//
// Every inlined i32 load at -opt=3 pays a modulo, a compare and a branch to ask
// a question the answer to which is almost always the same:
//
//	if t0 % 4 == 0 then v = MEM[t0 / 4 + 1] else v = ld32(MEM, MEMSIZE, t0) end
//
// This pass answers it at compile time where it can. It tracks, per i32 value,
// a CONGRUENCE -- "this value is r modulo m" -- for m a power of two up to 8,
// and the emitter drops the test when m and r together prove the effective
// address divisible by 4.
//
// # Why the Lua value and the wasm value may be swapped freely
//
// The analysis reasons about the WASM value, and the emitter tests the Lua
// number in the slot, which is only guaranteed CONGRUENT to it modulo 2^32
// (that is what wrap deferral buys). Every modulus here divides 2^32, so a
// residue modulo 4 or 8 survives that difference exactly. It also survives a
// deferred subtraction leaving a negative number in the slot: Lua's `%` is
// floored, so -4 % 4 is 0, and -4 / 4 is a whole number. The bounds check --
// which is kept, and is the thing that makes a negative address a trap rather
// than a nil read -- runs first either way.
//
// # The lattice, and which end is the top
//
// UNKNOWN (modulus 1) is the TOP: it is what a value nothing is known about
// gets, and an ABSENT key means exactly that. Getting this backwards produces
// an analysis that proves everything and is wrong about most of it, so every
// operation not reasoned about explicitly below falls through to CongTop.
//
// The lattice is finite and short -- 8 -> 4 -> 2 -> 1 per value -- so unlike
// the range analysis this one needs no widening and no threshold ladder. The
// fixpoint is the same shape otherwise, over the same ir.BuildCFG.

// maxAlign is the largest alignment tracked. 4 is what the i32 load wants; 8 is
// carried alongside because it costs nothing and is what an f64 or i64 access
// would want if one ever asks.
const maxAlign uint32 = 8

// Cong is the statement "this value is congruent to Res modulo Mod".
//
// Mod is always one of 1, 2, 4, 8 and Res is always less than Mod. Mod == 1 is
// the top: every value is congruent to 0 modulo 1, which is to say nothing is
// known.
type Cong struct {
	Mod uint32
	Res uint32
}

// CongTop is what a value is worth when nothing is known about it.
var CongTop = Cong{Mod: 1, Res: 0}

// cong builds a congruence, normalising the residue and refusing a modulus that
// is not one of the tracked powers of two.
func cong(mod, res uint32) Cong {
	switch mod {
	case 1, 2, 4, 8:
	default:
		return CongTop
	}
	return Cong{Mod: mod, Res: res % mod}
}

// DividesBy reports that every value in the congruence class is a multiple of
// n, for n a power of two no larger than maxAlign.
//
// This is the only question the emitter asks, and it is deliberately the whole
// of the public surface: the residue itself is never interesting outside here.
func (c Cong) DividesBy(n uint32) bool {
	if n == 0 || n > maxAlign {
		return false
	}
	return c.Mod%n == 0 && c.Res%n == 0
}

// divisor is the largest tracked power of two that divides every value in the
// class -- 1 when nothing is known.
func (c Cong) divisor() uint32 {
	for n := maxAlign; n > 1; n /= 2 {
		if c.DividesBy(n) {
			return n
		}
	}
	return 1
}

// shift is the congruence of the value plus a compile-time constant, which is
// how a memarg offset folds into the base's residue.
func (c Cong) shift(off uint32) Cong { return cong(c.Mod, c.Res+off%c.Mod) }

// join is the least precise congruence implied by both, which is what a merge
// point needs: the two agree modulo m only for an m dividing both moduli, so
// the modulus walks down until the residues match.
func (c Cong) join(o Cong) Cong {
	m := c.Mod
	if o.Mod < m {
		m = o.Mod
	}
	for m > 1 && c.Res%m != o.Res%m {
		m /= 2
	}
	return cong(m, c.Res)
}

// add is the congruence of a sum. Both operands are known only modulo their own
// modulus, so the sum is known modulo the smaller of the two.
func (c Cong) add(o Cong) Cong {
	m := c.Mod
	if o.Mod < m {
		m = o.Mod
	}
	return cong(m, c.Res+o.Res)
}

// sub is the same for a difference. The addition of m keeps the intermediate
// non-negative; unsigned wrapping would give the same residue but not readably.
func (c Cong) sub(o Cong) Cong {
	m := c.Mod
	if o.Mod < m {
		m = o.Mod
	}
	return cong(m, c.Res%m+m-o.Res%m)
}

// capTo weakens a congruence to a modulus no larger than n, which is what
// "x mod k" proves when n divides k.
func (c Cong) capTo(n uint32) Cong {
	if n >= c.Mod {
		return c
	}
	return cong(n, c.Res)
}

// Align is the result of the congruence analysis for one function.
type Align struct {
	// Addr[i] is the congruence of the EFFECTIVE address of the memory access
	// at step i -- the base operand plus the memarg offset. It is CongTop for
	// every step that is not a memory access, so a caller that asks about the
	// wrong step gets the conservative answer rather than a stale one.
	Addr []Cong
	// Stores lists what every global.set in the function writes, in step order.
	// Nothing in the emitter reads it; it exists so the module-level fixpoint in
	// Globals can check its own assumption.
	Stores []GlobalStore
}

// GlobalStore is one global.set: which global, and the congruence of the value.
type GlobalStore struct {
	Step   int
	Global uint32
	Cong   Cong
}

// AddrDividesBy reports that the effective address of the memory access at step
// i is provably a multiple of n.
//
// Guarded so a nil analysis -- "the optimizer is off" -- answers no rather than
// crashing, exactly as Wrap.Elided does.
func (a *Align) AddrDividesBy(i int, n uint32) bool {
	if a == nil || i < 0 || i >= len(a.Addr) {
		return false
	}
	return a.Addr[i].DividesBy(n)
}

// alignRounds caps the sweeps. The lattice is four elements tall per local and
// the key set only shrinks, so the fixpoint reaches itself long before this;
// hitting it drops the function to knowing nothing, which is a slower load and
// never a wrong one.
const alignRounds = 60

// congLocals is the dataflow state: what is known about each i32 wasm local.
//
// An ABSENT key is CongTop, never bottom -- so a join is a plain intersection of
// key sets, and a predecessor that knows nothing about a local poisons the
// merge. That is the correct direction, and the one that stops this pass from
// proving an address aligned on the strength of a path that never set it.
type congLocals map[uint32]Cong

func (l congLocals) clone() congLocals {
	out := make(congLocals, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (l congLocals) join(o congLocals) congLocals {
	out := make(congLocals, len(l))
	for k, a := range l {
		b, ok := o[k]
		if !ok {
			continue
		}
		j := a.join(b)
		if j.Mod == 1 {
			continue // top: an absent key says it more cheaply
		}
		out[k] = j
	}
	return out
}

func (l congLocals) same(o congLocals) bool {
	if len(l) != len(o) {
		return false
	}
	for k, a := range l {
		if b, ok := o[k]; !ok || a != b {
			return false
		}
	}
	return true
}

// GlobalAlign is a congruence per module global index, valid at every program
// point. An entry past the end, or for a global of any type but i32, is CongTop.
type GlobalAlign []Cong

// At reports what is known about global gi.
func (g GlobalAlign) At(gi uint32) Cong {
	if int(gi) >= len(g) {
		return CongTop
	}
	return g[gi]
}

// globalRounds caps the module-level fixpoint. Each round that does not settle
// weakens at least one global by one rung of a four-rung lattice, so a module
// with a handful of globals converges in two or three; exceeding it drops every
// global to the top.
const globalRounds = 8

// Globals computes a congruence that holds for every module global at every
// program point.
//
// This is what makes the pass worth having. LLVM's shadow stack lives behind a
// mutable global -- `__stack_pointer` -- and on real guest output most memory
// accesses are frame-relative, so an analysis that gives up at `global.get`
// proves about a third of the i32 loads where this one proves two thirds.
//
// It is a proof rather than a convention the way typed-slot promotion is. The
// iteration starts from the declared initialiser and weakens until every
// `global.set` in the module stores something inside the class it assumed:
//
//	init(g) in C[g]                                        -- the base case
//	every `global.set g v` stores v in C[g], given C        -- the step
//
// which is an inductive invariant, so it holds at every reachable state. There
// are no imported globals to worry about -- the decoder refuses them outright,
// because an ignored global import shifts every global index -- so the
// initialiser is the only way a value enters from outside the module.
//
// The one seam is `persist.setglobals`, which writes a saved value straight into
// a global on load. Same build, the value came from this module and is inside
// the class by construction; a DIFFERENT build reached through fk_migrate is the
// exception, and the emitter guards it rather than trusting it -- see
// emitPersist in internal/luagen.
func Globals(m *ir.Module) GlobalAlign {
	if m == nil || m.Source == nil {
		return nil
	}
	src := m.Source
	out := make(GlobalAlign, len(src.Globals))
	for i := range out {
		out[i] = CongTop
	}
	for i := range src.Globals {
		g := src.Globals[i]
		if g.Type != wasm.I32 {
			continue
		}
		if g.InitGlobal >= 0 {
			// A copy of an earlier global, which the decoder guarantees is
			// earlier, so its class is already final.
			if g.InitGlobal < i {
				out[i] = out[g.InitGlobal]
			}
			continue
		}
		out[i] = cong(maxAlign, uint32(g.InitBits)%maxAlign)
	}

	// Nothing to lose means nothing to prove. The iteration only ever weakens,
	// so a module whose globals all start at the top -- which is most of the
	// conformance suite, and every module with no globals at all -- skips a
	// whole-function sweep per function that could not have changed an answer.
	seeded := false
	for _, c := range out {
		if c.Mod > 1 {
			seeded = true
			break
		}
	}
	if !seeded {
		return out
	}

	for round := 0; round < globalRounds; round++ {
		changed := false
		for _, f := range m.Funcs {
			if f == nil || f.Unsupported != nil {
				continue
			}
			// nil Wrap on purpose: a round here costs a whole-function sweep,
			// and the block-local constant map already finds the shift distance
			// or mask written next to its use. A less precise round can only
			// WEAKEN the class it verifies, which is the safe direction.
			a := Aligns(f, nil, out)
			for _, st := range a.Stores {
				if int(st.Global) >= len(out) {
					continue
				}
				next := out[st.Global].join(st.Cong)
				if next != out[st.Global] {
					out[st.Global] = next
					changed = true
				}
			}
		}
		if !changed {
			return out
		}
	}
	for i := range out {
		out[i] = CongTop
	}
	return out
}

// Aligns runs the congruence analysis over one function.
//
// w is the range analysis for the same function and may be nil: it is consulted
// only to recognise a constant shift distance or mask that the block-local scan
// did not see written next to its use. A nil w costs precision and nothing else.
//
// g is what Globals proved about the module's globals, and may be nil -- in
// which case every `global.get` is unknown, which is sound and much weaker.
func Aligns(f *ir.Func, w *Wrap, g GlobalAlign) *Align {
	a := &Align{Addr: make([]Cong, len(f.Steps))}
	for i := range a.Addr {
		a.Addr[i] = CongTop
	}
	if f.Unsupported != nil || len(f.Steps) == 0 {
		return a
	}
	s := &congSolver{f: f, w: w, g: g, cfg: ir.BuildCFG(f), a: a}
	s.solve()
	return a
}

type congSolver struct {
	f   *ir.Func
	w   *Wrap
	g   GlobalAlign
	cfg *ir.CFG
	a   *Align

	in    []congLocals
	seen  []bool
	dirty []bool
}

func (s *congSolver) solve() {
	n := len(s.cfg.Blocks)
	if n == 0 {
		return
	}
	s.in = make([]congLocals, n)
	s.seen = make([]bool, n)
	s.dirty = make([]bool, n)

	// An incomplete graph means an edge is missing, so a merge would be an
	// UNDER-approximation -- the one failure mode here that produces a wrong
	// answer rather than a slow one. Fall back to knowing nothing.
	if s.cfg.Complete {
		s.in[0] = congEntry(s.f)
		s.seen[0], s.dirty[0] = true, true
		for round := 0; round < alignRounds; round++ {
			if !s.sweep() {
				break
			}
			if round == alignRounds-1 {
				for b := range s.in {
					s.in[b] = congLocals{}
				}
			}
		}
	} else {
		for b := range s.in {
			s.in[b] = congLocals{}
		}
	}

	// Record from the converged entry states, once. A block the entry cannot
	// reach is recorded from the top, so every step has an answer.
	for b := range s.cfg.Blocks {
		entry := s.in[b]
		if entry == nil {
			entry = congLocals{}
		}
		s.runBlock(b, entry, true)
	}
}

func (s *congSolver) sweep() bool {
	changed := false
	for _, b := range s.cfg.Order {
		if !s.seen[b] || !s.dirty[b] {
			continue
		}
		s.dirty[b] = false
		exit := s.runBlock(b, s.in[b], false)
		for _, succ := range s.cfg.Blocks[b].Succs {
			if s.merge(succ, exit) {
				changed = true
			}
		}
	}
	return changed
}

func (s *congSolver) merge(b int, st congLocals) bool {
	if !s.seen[b] {
		s.seen[b], s.dirty[b] = true, true
		s.in[b] = st.clone()
		return true
	}
	next := s.in[b].join(st)
	if s.in[b].same(next) {
		return false
	}
	s.in[b] = next
	s.dirty[b] = true
	return true
}

// congEntry is what is known before the first step runs.
//
// A DECLARED local starts at zero -- the spec requires it and the emitter's
// prologue writes it -- and zero is congruent to 0 modulo anything, so every
// declared i32 local starts perfectly aligned. A parameter starts unknown,
// which is the honest answer and the reason a heap pointer arriving as an
// argument proves nothing.
func congEntry(f *ir.Func) congLocals {
	out := congLocals{}
	for i, t := range f.Locals {
		if t == wasm.I32 {
			out[uint32(len(f.Params)+i)] = cong(maxAlign, 0)
		}
	}
	return out
}

// runBlock walks one basic block and is where every congruence comes from.
//
// Slot state is block-local and dies at every boundary instruction, for the
// same reason it does in the range analysis: a label can be entered from
// anywhere, so nothing about a stack slot survives one. Wasm LOCALS cross --
// that is what the fixpoint is for, and it is what lets a pointer bumped by 4
// at the bottom of a loop still be known aligned at the top.
func (s *congSolver) runBlock(b int, entry congLocals, record bool) congLocals {
	f := s.f
	blk := &s.cfg.Blocks[b]

	local := entry.clone()
	slot := map[ir.Slot]Cong{}
	// konst is the block-local constant map. A shift distance or a mask is
	// almost always an i32.const written immediately before its use, and this
	// finds it without needing the range analysis to have run.
	konst := map[ir.Slot]uint32{}

	// forgetFrom drops everything known about slot base and every slot above it.
	//
	// On a stack machine every slot above a step's destination is dead: the
	// destination is the new top of the operand stack, so anything past it was
	// popped. Clearing them costs no precision and removes a whole class of
	// staleness -- a step that writes more slots than DstType describes (a call
	// with several results, say) can no longer leave a live-looking congruence
	// behind for a later push to inherit.
	forgetFrom := func(base ir.Slot) {
		for sl := range slot {
			if sl >= base {
				delete(slot, sl)
			}
		}
		for sl := range konst {
			if sl >= base {
				delete(konst, sl)
			}
		}
	}

	congOf := func(sl ir.Slot) Cong {
		if c, ok := slot[sl]; ok {
			return c
		}
		return CongTop
	}

	for i := blk.Start; i < blk.End; i++ {
		st := &f.Steps[i]

		args := make([]Cong, len(st.Args))
		for k := range st.Args {
			args[k] = CongTop
			if k < len(st.ArgTypes) && st.ArgTypes[k] != wasm.I32 {
				continue
			}
			args[k] = congOf(st.Args[k])
		}

		// The address of a memory access, recorded before anything else can
		// disturb the slot map: a binary op's result lands in its first
		// operand's slot, and a load is no different.
		if record && isMemAccess(st.Op) && len(args) > 0 {
			s.a.Addr[i] = args[0].shift(st.Instr.MemOffset)
		}
		// Every global.set, in step order, so Globals' fixpoint is a function of
		// the module and not of Go's map iteration -- determinism is a
		// correctness property here, not a tidiness one.
		if record && st.Op == wasm.OpGlobalSet && len(args) > 0 && s.i32Global(st.Instr.GlobalIndex) {
			s.a.Stores = append(s.a.Stores, GlobalStore{
				Step: i, Global: st.Instr.GlobalIndex, Cong: args[0]})
		}

		if isBoundary(st.Op) {
			slot = map[ir.Slot]Cong{}
			konst = map[ir.Slot]uint32{}
			continue
		}

		switch st.Op {
		case wasm.OpLocalSet, wasm.OpLocalTee:
			idx := st.Instr.LocalIndex
			c := CongTop
			if len(args) > 0 && f.LocalType(idx) == wasm.I32 {
				c = args[0]
			}
			if c.Mod == 1 {
				delete(local, idx)
			} else {
				local[idx] = c
			}
			if st.Op == wasm.OpLocalTee && st.Dst != ir.NoSlot {
				forgetFrom(st.Dst)
				if c.Mod != 1 {
					slot[st.Dst] = c
				}
			}
			continue
		}

		if st.Dst == ir.NoSlot {
			continue
		}
		if st.DstType != wasm.I32 {
			forgetFrom(st.Dst)
			continue
		}

		c := s.transfer(i, args, local, konst)
		k, isConst := s.constAt(i, konst, -1)

		forgetFrom(st.Dst)
		if c.Mod != 1 {
			slot[st.Dst] = c
		}
		if isConst {
			konst[st.Dst] = k
		}
	}
	return local
}

// constAt reports the constant an operand holds. arg == -1 asks about the step's
// own result, which is a constant only when the step IS an i32.const.
//
// Two sources, and both are sound for the same reason: an exact u32 in the slot
// is the wasm value, because a deferred value is congruent to it modulo 2^32 and
// a number in [0, 2^32) has only one representative in that class.
func (s *congSolver) constAt(i int, konst map[ir.Slot]uint32, arg int) (uint32, bool) {
	st := &s.f.Steps[i]
	if arg < 0 {
		if st.Op == wasm.OpI32Const {
			return st.Instr.I32, true
		}
		return 0, false
	}
	if arg < len(st.Args) {
		if k, ok := konst[st.Args[arg]]; ok {
			return k, true
		}
	}
	return s.w.ArgRange(i, arg).ConstU32()
}

// transfer is the congruence step i's result carries.
//
// Everything not named here falls through to CongTop. That default is the
// safety property of the whole pass: a load result, a division, a global read, a
// call result and every operation nobody has thought about yet are all UNKNOWN,
// and an address built from one is never claimed aligned.
func (s *congSolver) transfer(i int, args []Cong, local congLocals, konst map[ir.Slot]uint32) Cong {
	st := &s.f.Steps[i]
	a, b := congAt(args, 0), congAt(args, 1)

	switch st.Op {
	case wasm.OpI32Const:
		return cong(maxAlign, st.Instr.I32%maxAlign)

	case wasm.OpLocalGet:
		if c, ok := local[st.Instr.LocalIndex]; ok {
			return c
		}
		return CongTop

	// A global is worth whatever the module-level fixpoint proved about it,
	// which for LLVM's `__stack_pointer` is 16-aligned and therefore 8 here.
	case wasm.OpGlobalGet:
		if s.i32Global(st.Instr.GlobalIndex) {
			return s.g.At(st.Instr.GlobalIndex)
		}
		return CongTop

	// Addition and subtraction wrap modulo 2^32, and every modulus tracked here
	// divides 2^32 -- which is the whole reason a pointer bumped by 4 in a loop
	// stays provably aligned however many times it goes round.
	case wasm.OpI32Add:
		return a.add(b)
	case wasm.OpI32Sub:
		return a.sub(b)

	// A product is divisible by whatever either factor is divisible by. `p * 4`
	// is the shape that matters and it needs no constant: i32.const 4 is
	// congruent to 4 modulo 8, which divides by 4.
	case wasm.OpI32Mul:
		d := a.divisor()
		if e := b.divisor(); e > d {
			d = e
		}
		return cong(d, 0)

	// `x & k` clears every bit k does not have, so the result is divisible by
	// the power of two at the bottom of k. It is also divisible by whatever x
	// was, since clearing bits cannot set a low one.
	case wasm.OpI32And:
		d := a.divisor()
		if e := b.divisor(); e > d {
			d = e
		}
		for arg := 0; arg < 2; arg++ {
			if k, ok := s.constAt(i, konst, arg); ok {
				if e := pow2Divisor(k); e > d {
					d = e
				}
			}
		}
		return cong(d, 0)

	// `or` and `xor` leave a low bit set if EITHER operand has it, so only the
	// weaker of the two divisors survives.
	case wasm.OpI32Or, wasm.OpI32Xor:
		d := a.divisor()
		if e := b.divisor(); e < d {
			d = e
		}
		return cong(d, 0)

	// A left shift multiplies by 2^n and then wraps, and the wrap is again
	// invisible modulo 8. A distance of 0 mod 32 is the IDENTITY -- the same
	// trap the wrap-deferral pass fell into -- so it returns the operand
	// unchanged rather than claiming anything new.
	case wasm.OpI32Shl:
		k, ok := s.constAt(i, konst, 1)
		if !ok {
			return CongTop
		}
		n := k % 32
		if n == 0 {
			return a
		}
		m := uint64(a.Mod) << n
		if m > uint64(maxAlign) {
			m = uint64(maxAlign)
		}
		return cong(uint32(m), uint32((uint64(a.Res)<<n)%m))

	// A right shift or a rotate throws the low bits away or moves them, and
	// nothing survives -- except the identity distance, which is the same
	// operand back.
	case wasm.OpI32ShrU, wasm.OpI32ShrS, wasm.OpI32Rotl, wasm.OpI32Rotr:
		if k, ok := s.constAt(i, konst, 1); ok && k%32 == 0 {
			return a
		}
		return CongTop

	// `x % k` differs from x by a multiple of k, so a residue modulo any power
	// of two dividing k comes through unchanged.
	case wasm.OpI32RemU:
		if k, ok := s.constAt(i, konst, 1); ok && k > 0 {
			return a.capTo(pow2Divisor(k))
		}
		return CongTop

	// select yields one of its two value operands, so it yields whatever both
	// of them agree on.
	case wasm.OpSelect:
		if st.DstType == wasm.I32 {
			return a.join(b)
		}
		return CongTop
	}
	return CongTop
}

// i32Global reports a module global of type i32. Anything else -- a float or a
// wide global, or an index the module does not have -- is not tracked at all.
func (s *congSolver) i32Global(gi uint32) bool {
	if s.f.Mod == nil || int(gi) >= len(s.f.Mod.Globals) {
		return false
	}
	return s.f.Mod.Globals[gi].Type == wasm.I32
}

func congAt(args []Cong, k int) Cong {
	if k >= len(args) {
		return CongTop
	}
	return args[k]
}

// pow2Divisor is the largest tracked power of two dividing k. Zero divides by
// everything, which is the right answer for a mask of 0: the result is 0.
func pow2Divisor(k uint32) uint32 {
	if k == 0 {
		return maxAlign
	}
	for n := maxAlign; n > 1; n /= 2 {
		if k%n == 0 {
			return n
		}
	}
	return 1
}

// isMemAccess reports an instruction whose first operand is a linear-memory
// address. memory.copy and memory.fill are absent on purpose: their operands are
// addresses too, but they are lowered to a runtime helper that takes ranges, and
// nothing there asks this question.
func isMemAccess(op wasm.Op) bool {
	switch op {
	case wasm.OpI32Load, wasm.OpI32Load8S, wasm.OpI32Load8U,
		wasm.OpI32Load16S, wasm.OpI32Load16U,
		wasm.OpI64Load, wasm.OpI64Load8S, wasm.OpI64Load8U,
		wasm.OpI64Load16S, wasm.OpI64Load16U,
		wasm.OpI64Load32S, wasm.OpI64Load32U,
		wasm.OpF32Load, wasm.OpF64Load,
		wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
		wasm.OpI64Store, wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32,
		wasm.OpF32Store, wasm.OpF64Store:
		return true
	}
	return false
}
