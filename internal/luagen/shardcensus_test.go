package luagen

// THE CORPUS CENSUS for sharded linear memory -- agents/sharding.md §4.
//
// It exists because the milestone's opening design assumed the range analysis
// could prove a useful fraction of addresses under one shard's bound, and this
// is the measurement that says it cannot: 10 of 962 class-A sites in this
// repo's corpus come from the range analysis and 952 are literal constants.
// A design that had not been measured here would have shipped a fast path
// covering 0.16% of accesses.
//
// It reproduces agents/optimizer.md's published guard census guest for guest,
// which is what says it sees the plan the emitter would actually emit rather
// than a plan of its own -- it reconstructs emitFunc's exact setup, including
// maxGuardsPerFunc and the spill refusal. TestShardCensusClassifier pins four
// accesses whose class is known by construction, one of them a COMPUTED address
// the range analysis really does bound, so a zero in that column would be a
// result rather than a blind spot.
//
// Classifies every emitted memory access in the guest corpus into the three
// forms a sharded MEM would need:
//
//	A  the range analysis already proves address+offset+width <= 2^21, so the
//	   emitter could keep today's flat S0[i] with no shard selection at all;
//	B  the access is covered by a loop guard, whose ENTRY TEST already computes
//	   the whole span at runtime and could be extended by one conjunct to prove
//	   the span lies inside a single shard;
//	C  neither -- runtime shard selection per access.
//
// Run with:
//	go test ./internal/luagen -run TestShardCensus -v -timeout 30m
// Extra modules (a downstream mod's .wasm) via FKLUA_SHARD_EXTRA=path[,path].

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The shard size in BYTES is shardBytes, now a real emitter constant in
// shard.go rather than a proposal here. The census keeps using it so that what
// it classifies and what the emitter emits can never drift apart.

type census struct {
	name string

	total   int // static memory-access sites emitted
	flat    int // A
	guarded int // B (and not A)
	both    int // A and B -- reported so neither column double-counts
	runtime int // C
	// dynamic weighting: loopWeight(depth) summed over sites
	wTotal, wFlat, wGuarded, wRuntime int64
	// how many sites sit inside at least one loop
	inLoop, inLoopFlat, inLoopGuarded int

	// what the range analysis knows about the address operand, independent of
	// the shard question
	knownAny   int // arg range is narrower than the full u32 range
	exactAddr  int // address operand is a compile-time constant
	bound      map[string]int
	guardLoops int

	// the honesty split on class A: how much of it is a literal address the
	// emitter could already have folded, and how much is a genuine
	// range-analysis result about a COMPUTED address.
	flatConst    int
	flatComputed int
	// a literal address that does NOT fit under the first shard
	constAbove int
	// class C with nothing at all known about the address
	runtimeUnknown int
	// class C whose address is `$fp + constant`: exactly what a stack-pointer
	// range would buy, measured rather than tainted.
	frameC    int
	wFrameC   int64
	frameAll  int
	frameA    int
	frameB    int
	spInit    uint64
	spMutable bool

	// accesses lying textually INSIDE a guarded loop, and how many of those the
	// guard declined to specialise (an undescribable access is skipped, not
	// fatal, so a guarded loop can still contain unguarded accesses).
	inGuardedLoop int
	guardSkipped  int
	// dynamic weight of the class-A split
	wFlatConst, wFlatComputed int64

	// module facts
	memMinPages uint32
	dataTop     int64

	// where a class-C address comes from, by forward taint from its roots.
	// OVER-APPROXIMATE: the taint is flow-insensitive and unions through every
	// operator, so a label is an upper bound on that root's involvement.
	croot  map[string]int
	wcroot map[string]int64
}

func newCensus(name string) *census {
	return &census{name: name, bound: map[string]int{},
		croot: map[string]int{}, wcroot: map[string]int64{}}
}

// Root kinds an address can be built from.
const (
	rootConst    = 1 << iota
	rootSPGlobal // global.get of a MUTABLE i32 global -- the shadow-stack pointer
	rootGlobal   // any other global
	rootParam
	rootLoad // a value read back out of linear memory: a heap pointer
	rootCall
	rootOther
)

func rootLabel(m uint32) string {
	if m == 0 {
		return "(nothing)"
	}
	var p []string
	for _, e := range []struct {
		bit  uint32
		name string
	}{
		{rootConst, "const"}, {rootSPGlobal, "SP"}, {rootGlobal, "global"},
		{rootParam, "param"}, {rootLoad, "load"}, {rootCall, "call"},
		{rootOther, "other"},
	} {
		if m&e.bit != 0 {
			p = append(p, e.name)
		}
	}
	return strings.Join(p, "+")
}

// framePointerAddrs marks every step whose ADDRESS operand is `$fp + constant`,
// where $fp is the shadow-stack frame pointer LLVM's standard prologue sets up.
//
// This is precise in the same way analysis.Frames is -- a straight-line derived
// set reset at every control-flow boundary -- but it does NOT require the frame
// to be private. Frames refuses as soon as $fp reaches a call, which is the
// common case in TinyGo output; this counts the accesses anyway, because for the
// shard question what matters is that the address is stack-pointer-relative, not
// that the frame is promotable.
func framePointerAddrs(f *ir.Func) map[int]bool {
	out := map[int]bool{}
	if f.Mod == nil || len(f.Steps) < 5 {
		return out
	}
	s := f.Steps
	if s[0].Op != wasm.OpGlobalGet || s[1].Op != wasm.OpI32Const ||
		s[2].Op != wasm.OpI32Sub || s[3].Op != wasm.OpLocalTee ||
		s[4].Op != wasm.OpGlobalSet {
		return out
	}
	sp := s[0].Instr.GlobalIndex
	if int(sp) >= len(f.Mod.Globals) {
		return out
	}
	if g := f.Mod.Globals[sp]; g.Type != wasm.I32 || !g.Mutable {
		return out
	}
	fp := s[3].Instr.LocalIndex

	derived := map[ir.Slot]bool{}
	for i := 5; i < len(f.Steps); i++ {
		st := &f.Steps[i]
		switch st.Op {
		case wasm.OpBlock, wasm.OpLoop, wasm.OpIf, wasm.OpElse, wasm.OpEnd,
			wasm.OpBr, wasm.OpBrIf, wasm.OpBrTable, wasm.OpReturn:
			derived = map[ir.Slot]bool{}
			continue
		case wasm.OpLocalGet:
			// A slot the frame pointer is copied INTO is a frame address; a slot
			// anything else is copied into stops being one. Slots are reused, so
			// failing to clear here counts unrelated accesses as frame accesses.
			if st.Dst != ir.NoSlot {
				if st.Instr.LocalIndex == fp {
					derived[st.Dst] = true
				} else {
					delete(derived, st.Dst)
				}
			}
			continue
		case wasm.OpI32Add:
			// $fp + const is still a frame address. $fp + anything else is not
			// nameable, and is deliberately not counted.
			if st.Dst != ir.NoSlot {
				ok := false
				if len(st.Args) == 2 {
					a0, a1 := derived[st.Args[0]], derived[st.Args[1]]
					if a0 != a1 {
						other := st.Args[1]
						if a1 {
							other = st.Args[0]
						}
						ok = isConstSlot(f, i, other)
					}
				}
				if ok {
					derived[st.Dst] = true
				} else {
					delete(derived, st.Dst)
				}
			}
			continue
		}
		if isShardMemAccess(st.Op) && len(st.Args) > 0 && derived[st.Args[0]] {
			out[i] = true
		}
		// Anything else writing a slot destroys whatever the slot held.
		if st.Dst != ir.NoSlot {
			n := st.DstType.Slots()
			if len(st.ResultTypes) > 0 {
				n = 0
				for _, rt := range st.ResultTypes {
					n += rt.Slots()
				}
			}
			if n < 1 {
				n = 1
			}
			for k := 0; k < n; k++ {
				delete(derived, st.Dst+ir.Slot(k))
			}
		}
	}
	return out
}

// isConstSlot reports that slot s was last written by an i32.const before step i,
// scanning backward in the same straight run.
func isConstSlot(f *ir.Func, i int, s ir.Slot) bool {
	for j := i - 1; j >= 0; j-- {
		st := &f.Steps[j]
		switch st.Op {
		case wasm.OpBlock, wasm.OpLoop, wasm.OpIf, wasm.OpElse, wasm.OpEnd:
			return false
		}
		if st.Dst == s {
			return st.Op == wasm.OpI32Const
		}
	}
	return false
}

// addrRoots is a flow-insensitive forward taint over one function: what a slot's
// value is ultimately built from.
//
// It is deliberately over-approximate -- it unions through every operator and
// never kills -- so a root appearing in a label is an UPPER bound on that root's
// involvement, never a proof. It exists to say where the unprovable addresses
// come from, not to prove anything about them. MEASURED RESULT: it collapses --
// almost every class-C address is reachable from a load somewhere -- so it is
// kept only to document that the coarse question has no useful answer.
func addrRoots(f *ir.Func, mod *ir.Module) []uint32 {
	mask := make([]uint32, f.NumSlots+8)
	get := func(s ir.Slot) uint32 {
		if s < 0 || int(s) >= len(mask) {
			return rootOther
		}
		return mask[s]
	}
	set := func(s ir.Slot, m uint32) bool {
		if s < 0 || int(s) >= len(mask) {
			return false
		}
		if mask[s]|m == mask[s] {
			return false
		}
		mask[s] |= m
		return true
	}
	for i := range f.Params {
		set(f.LocalSlot(uint32(i)), rootParam)
	}
	for round := 0; round < 24; round++ {
		changed := false
		for i := range f.Steps {
			s := &f.Steps[i]
			var in uint32
			for _, a := range s.Args {
				in |= get(a)
			}
			switch s.Op {
			case wasm.OpI32Const, wasm.OpI64Const, wasm.OpF32Const, wasm.OpF64Const:
				in = rootConst
			case wasm.OpLocalGet:
				in = get(f.LocalSlot(s.Instr.LocalIndex))
			case wasm.OpLocalSet:
				changed = set(f.LocalSlot(s.Instr.LocalIndex), in) || changed
				continue
			case wasm.OpLocalTee:
				changed = set(f.LocalSlot(s.Instr.LocalIndex), in) || changed
			case wasm.OpGlobalGet:
				gi := s.Instr.GlobalIndex
				in = rootGlobal
				if mod != nil && mod.Source != nil && int(gi) < len(mod.Source.Globals) &&
					mod.Source.Globals[gi].Mutable {
					in = rootSPGlobal
				}
			case wasm.OpCall, wasm.OpCallIndirect:
				in = rootCall
			default:
				if isShardMemAccess(s.Op) && s.Dst != ir.NoSlot {
					in = rootLoad
				}
			}
			if s.Dst != ir.NoSlot {
				changed = set(s.Dst, in) || changed
				if s.DstType.Slots() > 1 {
					changed = set(s.Dst+1, in) || changed
				}
			}
		}
		if !changed {
			break
		}
	}
	return mask
}

func (c *census) add(o *census) {
	c.total += o.total
	c.flat += o.flat
	c.guarded += o.guarded
	c.both += o.both
	c.runtime += o.runtime
	c.wTotal += o.wTotal
	c.wFlat += o.wFlat
	c.wGuarded += o.wGuarded
	c.wRuntime += o.wRuntime
	c.inLoop += o.inLoop
	c.inLoopFlat += o.inLoopFlat
	c.inLoopGuarded += o.inLoopGuarded
	c.knownAny += o.knownAny
	c.exactAddr += o.exactAddr
	c.guardLoops += o.guardLoops
	c.flatConst += o.flatConst
	c.flatComputed += o.flatComputed
	c.constAbove += o.constAbove
	c.runtimeUnknown += o.runtimeUnknown
	c.frameC += o.frameC
	c.wFrameC += o.wFrameC
	c.frameAll += o.frameAll
	c.frameA += o.frameA
	c.frameB += o.frameB
	c.inGuardedLoop += o.inGuardedLoop
	c.guardSkipped += o.guardSkipped
	if o.spInit > c.spInit {
		c.spInit = o.spInit
	}
	for k, v := range o.croot {
		c.croot[k] += v
	}
	for k, v := range o.wcroot {
		c.wcroot[k] += v
	}
	c.wFlatConst += o.wFlatConst
	c.wFlatComputed += o.wFlatComputed
	if o.dataTop > c.dataTop {
		c.dataTop = o.dataTop
	}
	if o.memMinPages > c.memMinPages {
		c.memMinPages = o.memMinPages
	}
	for k, v := range o.bound {
		c.bound[k] += v
	}
}

// memWidth is the byte width of an access, mirroring the emitter's own bounds
// checks. Width matters: the shard test is over the LAST byte touched.
func memWidth(op wasm.Op) int64 {
	switch op {
	case wasm.OpI32Load8S, wasm.OpI32Load8U, wasm.OpI32Store8,
		wasm.OpI64Load8S, wasm.OpI64Load8U, wasm.OpI64Store8:
		return 1
	case wasm.OpI32Load16S, wasm.OpI32Load16U, wasm.OpI32Store16,
		wasm.OpI64Load16S, wasm.OpI64Load16U, wasm.OpI64Store16:
		return 2
	case wasm.OpI32Load, wasm.OpI32Store, wasm.OpF32Load, wasm.OpF32Store,
		wasm.OpI64Load32S, wasm.OpI64Load32U, wasm.OpI64Store32:
		return 4
	case wasm.OpI64Load, wasm.OpI64Store, wasm.OpF64Load, wasm.OpF64Store:
		return 8
	}
	return 0
}

func isShardMemAccess(op wasm.Op) bool { return memWidth(op) != 0 }

// loopDepths returns the loop-nesting depth of every step, by the same block
// walk analysis.HotCallees uses.
func loopDepths(f *ir.Func) []int {
	out := make([]int, len(f.Steps))
	depth := 0
	var stack []wasm.Op
	for i, s := range f.Steps {
		switch s.Op {
		case wasm.OpBlock, wasm.OpIf:
			stack = append(stack, s.Op)
		case wasm.OpLoop:
			stack = append(stack, s.Op)
			depth++
		case wasm.OpEnd:
			if n := len(stack); n > 0 {
				if stack[n-1] == wasm.OpLoop {
					depth--
				}
				stack = stack[:n-1]
			}
		}
		out[i] = depth
	}
	return out
}

// guardedLoopSpans marks every step inside the body of a loop the emitter gave a
// guard, so an access the guard SKIPPED can be told from one in an unguarded
// loop. The extent is found by matching the loop's own `end`.
func guardedLoopSpans(f *ir.Func, lg map[int]*analysis.LoopGuard) map[int]bool {
	out := map[int]bool{}
	for h := range lg {
		depth := 0
		for i := h; i < len(f.Steps); i++ {
			switch f.Steps[i].Op {
			case wasm.OpBlock, wasm.OpLoop, wasm.OpIf:
				depth++
			case wasm.OpEnd:
				depth--
			}
			out[i] = true
			if depth == 0 {
				break
			}
		}
	}
	return out
}

func loopWeightOf(depth int) int64 {
	if depth > 3 {
		depth = 3
	}
	w := int64(1)
	for i := 0; i < depth; i++ {
		w *= 10
	}
	return w
}

// takeCensus reproduces exactly what emitFunc sets up before it lowers a step,
// so the guard plan and the range analysis are the ones the emitter would see.
func takeCensus(name string, m *ir.Module, opts Options) *census {
	c := newCensus(name)
	if m.Source != nil {
		c.memMinPages = m.Source.Memory.Min
		// The shadow-stack pointer's INITIAL value is the precondition for any
		// "$fp is under 2 MiB" argument: it is the top of the shadow stack, and
		// nothing in a call can exceed it.
		for _, g := range m.Source.Globals {
			if g.Type == wasm.I32 && g.Mutable && g.InitBits > c.spInit {
				c.spInit = g.InitBits
				c.spMutable = true
			}
		}
		for _, d := range m.Source.Data {
			if top := int64(d.Offset) + int64(len(d.Bytes)); top > c.dataTop {
				c.dataTop = top
			}
		}
	}

	b := &builder{opts: opts, opt: opts.Opt, plans: map[uint32]*analysis.Spill{}}
	for _, f := range m.Funcs {
		if f.Unsupported != nil {
			continue
		}
		if sp := analysis.Plan(f, ir.MaxSlots); sp.Active() {
			b.plans[f.Index] = sp
			b.spilled = true
		}
	}
	if b.inlineLoads() {
		b.ga = analysis.Globals(m)
	}

	for _, f := range m.Funcs {
		if f.Unsupported != nil {
			continue
		}
		b.w, b.fr, b.al = nil, nil, nil
		b.sp = b.plans[f.Index]
		if b.opt.Peephole() {
			b.w = analysis.Ranges(f)
		}
		if b.opt.Slots() {
			b.fr = analysis.Frames(f)
		}
		if b.inlineLoads() {
			b.al = analysis.Aligns(f, b.w, b.ga)
		}
		b.planCountedLoops(f)
		b.planLoopGuards(f)
		c.guardLoops += len(b.lg)

		// A function whose body the emitter replaces wholesale emits none of its
		// own accesses.
		if b.nativeIntrinsics() {
			switch analysis.NativeIntrinsic(f) {
			case analysis.IntrinsicCopy, analysis.IntrinsicFill:
				continue
			}
		}

		depths := loopDepths(f)
		roots := addrRoots(f, m)
		frames := framePointerAddrs(f)
		inGuarded := guardedLoopSpans(f, b.lg)
		for i := range f.Steps {
			s := &f.Steps[i]
			if !isShardMemAccess(s.Op) {
				continue
			}
			if b.clDrop[i] {
				continue
			}
			w := memWidth(s.Op)
			off := int64(s.Instr.MemOffset)
			r := b.w.ArgRange(i, 0)

			depth := depths[i]
			wt := loopWeightOf(depth)
			c.total++
			c.wTotal += wt
			if frames[i] {
				c.frameAll++
			}
			if inGuarded[i] {
				c.inGuardedLoop++
				if _, ok := b.lgAccess[i]; !ok {
					c.guardSkipped++
				}
			}
			if depth > 0 {
				c.inLoop++
			}

			_, isConst := r.ConstU32()
			if r != analysis.FullU32 {
				c.knownAny++
			}
			if isConst {
				c.exactAddr++
			}
			// Histogram of the proven last-byte bound.
			c.bound[bucket(r, off, w)]++

			// (A) proven flat: every address the operand can take, plus the
			// static offset and the access width, stays under the first shard.
			flat := r.Lo >= 0 && r.Hi+off+w <= shardBytes
			// (B) the emitter's own guard plan covers this access.
			_, gd := b.lgAccess[i]

			if isConst && !flat {
				c.constAbove++
			}
			switch {
			case flat:
				if gd {
					c.both++
				}
				c.flat++
				c.wFlat += wt
				if isConst {
					c.flatConst++
					c.wFlatConst += wt
				} else {
					c.flatComputed++
					c.wFlatComputed += wt
					if os.Getenv("FKLUA_SHARD_DUMP") != "" {
						fmt.Printf("  COMPUTED-A %s: %s step %d range [%d,%d] off %d w %d\n",
							f.Name, s.Op, i, r.Lo, r.Hi, off, w)
					}
				}
				if frames[i] {
					c.frameA++
				}
				if depth > 0 {
					c.inLoopFlat++
				}
			case gd:
				c.guarded++
				c.wGuarded += wt
				if depth > 0 {
					c.inLoopGuarded++
				}
				if frames[i] {
					c.frameB++
				}
			default:
				c.runtime++
				c.wRuntime += wt
				if r == analysis.FullU32 {
					c.runtimeUnknown++
				}
				if frames[i] {
					c.frameC++
					c.wFrameC += wt
				}
				var rm uint32
				if len(s.Args) > 0 && int(s.Args[0]) < len(roots) && s.Args[0] >= 0 {
					rm = roots[s.Args[0]]
				}
				c.croot[rootLabel(rm)]++
				c.wcroot[rootLabel(rm)] += wt
			}
		}
	}
	return c
}

func bucket(r analysis.Range, off, w int64) string {
	if r.Lo < 0 {
		return "negative-or-deferred"
	}
	end := r.Hi + off + w
	switch {
	case end <= shardBytes:
		return "<=2MiB (shard 0)"
	case end <= 4<<20:
		return "<=4MiB"
	case end <= 16<<20:
		return "<=16MiB"
	case end <= 1<<32:
		return "<=4GiB"
	}
	return ">4GiB"
}

func (c *census) line() string {
	pct := func(n int) string {
		if c.total == 0 {
			return "   -"
		}
		return fmt.Sprintf("%5.1f%%", 100*float64(n)/float64(c.total))
	}
	return fmt.Sprintf("%-34s %6d %6d %s %6d %s %6d %s   Aconst=%d Acomp=%d loops=%d inGuardedLoop=%d(skipped %d) mem=%dKiB data<=%d",
		c.name, c.total,
		c.flat, pct(c.flat),
		c.guarded, pct(c.guarded),
		c.runtime, pct(c.runtime),
		c.flatConst, c.flatComputed,
		c.guardLoops, c.inGuardedLoop, c.guardSkipped,
		int64(c.memMinPages)*64, c.dataTop)
}

// TestShardCensusClassifierAgrees is the gate on the census itself: a census
// that quietly classifies everything into one bucket would look like a result.
// Four accesses whose correct class is known by construction.
func TestShardCensusClassifier(t *testing.T) {
	// $lo:   a literal address well under the shard -- class A.
	// $hi:   a literal address well over it -- class C.
	// $unk:  a parameter -- nothing provable, class C.
	// $narrow: a parameter masked to 16 bits, so the range analysis DOES bound
	//          it under 2^21 -- class A, and the only shape that exercises the
	//          computed half of A.
	m := mod(t, `(module (memory 64)
	  (func $lo (result i32) (i32.load (i32.const 1024)))
	  (func $hi (result i32) (i32.load (i32.const 3000000)))
	  (func $unk (param i32) (result i32) (i32.load (local.get 0)))
	  (func $narrow (param i32) (result i32)
	    (i32.load (i32.and (local.get 0) (i32.const 65535)))))`)
	c := takeCensus("classifier", m, Options{Opt: analysis.O3, Persist: PersistTable})
	if c.total != 4 {
		t.Fatalf("total = %d, want 4", c.total)
	}
	if c.flat != 2 || c.runtime != 2 || c.guarded != 0 {
		t.Errorf("A=%d B=%d C=%d, want A=2 B=0 C=2", c.flat, c.guarded, c.runtime)
	}
	if c.flatConst != 1 || c.flatComputed != 1 {
		t.Errorf("A split = const %d / computed %d, want 1 / 1",
			c.flatConst, c.flatComputed)
	}
	if c.constAbove != 1 {
		t.Errorf("literal above the shard = %d, want 1", c.constAbove)
	}
}

func TestShardCensus(t *testing.T) {
	root := luagenRepoRoot(t)
	tmp := t.TempDir()
	opts := Options{Opt: analysis.O3, Persist: PersistTable}

	var rows []*census
	tinygoAll, rustAll := newCensus("TOTAL TinyGo"), newCensus("TOTAL Rust")

	run := func(agg *census, name, path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		m, err := wasm.Decode(raw)
		if err != nil {
			t.Errorf("%s: decode: %v", name, err)
			return
		}
		im, err := ir.BuildModule(m)
		if err != nil {
			t.Errorf("%s: ir: %v", name, err)
			return
		}
		c := takeCensus(name, im, opts)
		rows = append(rows, c)
		if agg != nil {
			agg.add(c)
		}
	}

	if ok, why := guest.Available(); !ok {
		t.Logf("skipping the TinyGo half: %s", why)
	} else {
		for _, g := range guardCorpus {
			name := "tinygo " + g.dir + " " + g.pkg
			out := filepath.Join(tmp,
				strings.NewReplacer("/", "-", ".", "").Replace(name)+".wasm")
			if err := guest.Build(filepath.Join(root, filepath.FromSlash(g.dir)), g.pkg, out); err != nil {
				t.Errorf("building %s: %v", name, err)
				continue
			}
			run(tinygoAll, name, out)
		}
	}

	if ok, why := guest.RustAvailable(); !ok {
		t.Logf("skipping the Rust half: %s", why)
	} else {
		for _, g := range guardCorpusRust {
			name := "rust " + g.workspace + " " + g.pkg
			// The collected arm is a different module and gets its own row and
			// its own target directory: the collector's ~36 KiB of .bss is what
			// moves __heap_base, and the "data <= N" column is the census
			// question -- how close a guest's statics come to the 2 MiB shard
			// line. Building it into the leaking arm's directory would print the
			// leaking module twice under two names.
			build := guest.BuildRust
			cargo := filepath.Join(tmp, "cargo")
			if g.collected {
				build = guest.BuildRustCollected
				name += " (collected)"
				cargo = filepath.Join(tmp, "cargo-collected")
			}
			p, err := build(filepath.Join(root, filepath.FromSlash(g.workspace)), g.pkg, cargo)
			if err != nil {
				t.Errorf("building %s: %v", name, err)
				continue
			}
			if g.lower {
				lowered, err := lowerBulkMemory(t, p, filepath.Join(tmp, g.pkg+"-lowered.wasm"))
				if err != nil {
					t.Logf("skipping %s: %v", name, err)
					continue
				}
				p = lowered
			}
			run(rustAll, name, p)
		}
	}

	for _, extra := range strings.Split(os.Getenv("FKLUA_SHARD_EXTRA"), ",") {
		if strings.TrimSpace(extra) == "" {
			continue
		}
		run(nil, "extra "+filepath.Base(extra), strings.TrimSpace(extra))
	}

	t.Log("")
	t.Logf("%-34s %6s %6s %6s %6s %6s %6s %6s",
		"guest", "total", "A:flat", "", "B:guard", "", "C:rt", "")
	total := newCensus("TOTAL ALL")
	for _, c := range rows {
		t.Log(c.line())
		total.add(c)
	}
	t.Log(tinygoAll.line())
	t.Log(rustAll.line())
	t.Log(total.line())

	t.Log("")
	t.Logf("overlap (A and B): %d sites counted under A", total.both)
	t.Logf("address operand narrower than full u32: %d/%d", total.knownAny, total.total)
	t.Logf("address operand a compile-time constant: %d/%d", total.exactAddr, total.total)
	t.Logf("sites inside >=1 loop: %d/%d (A:%d B:%d)",
		total.inLoop, total.total, total.inLoopFlat, total.inLoopGuarded)
	t.Logf("loop-depth-weighted: total=%d A=%d B=%d C=%d",
		total.wTotal, total.wFlat, total.wGuarded, total.wRuntime)
	t.Logf("class A split: %d from a LITERAL address, %d from a COMPUTED address",
		total.flatConst, total.flatComputed)
	t.Logf("class A split, loop-weighted: literal=%d computed=%d",
		total.wFlatConst, total.wFlatComputed)
	t.Logf("literal addresses that do NOT fit shard 0: %d", total.constAbove)
	t.Logf("class C with nothing known about the address at all: %d/%d",
		total.runtimeUnknown, total.runtime)
	t.Logf("accesses at `$fp + const` (shadow stack): %d of %d total  (A:%d B:%d C:%d; C weighted %d of %d)",
		total.frameAll, total.total, total.frameA, total.frameB, total.frameC,
		total.wFrameC, total.wRuntime)
	t.Log("")
	t.Log("shadow-stack pointer initial value per module (the precondition for `$fp < 2 MiB`):")
	for _, c := range rows {
		t.Logf("  %-34s SP init = %d  (%.2f MiB)  frame accesses = %d",
			c.name, c.spInit, float64(c.spInit)/(1<<20), c.frameAll)
	}

	t.Log("")
	t.Log("proven last-byte bound on the address operand:")
	keys := make([]string, 0, len(total.bound))
	for k := range total.bound {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("  %-24s %6d  %5.1f%%", k, total.bound[k],
			100*float64(total.bound[k])/float64(total.total))
	}

	t.Log("")
	t.Log("class C addresses by ROOT (over-approximate forward taint, upper bounds):")
	ck := make([]string, 0, len(total.croot))
	for k := range total.croot {
		ck = append(ck, k)
	}
	sort.Slice(ck, func(a, b int) bool { return total.croot[ck[a]] > total.croot[ck[b]] })
	for _, k := range ck {
		t.Logf("  %-30s %6d  %5.1f%%   weighted %8d  %5.1f%%",
			k, total.croot[k], 100*float64(total.croot[k])/float64(total.runtime),
			total.wcroot[k], 100*float64(total.wcroot[k])/float64(total.wRuntime))
	}
}
