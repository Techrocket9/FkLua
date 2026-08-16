package analysis

import (
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// FrameSlot is one promoted shadow-stack slot: where it now lives and what it
// holds.
type FrameSlot struct {
	// Base is the first extra Lua slot the value occupies. An i64 or an f64
	// stored as an i64 pair takes two, like every other wide value.
	Base ir.Slot
	Type wasm.ValType
	// Offset is the byte offset within the frame it replaced, kept for the
	// comment the emitter writes next to the declaration.
	Offset uint32
}

// Frame is the result of typed-slot promotion for one function.
type Frame struct {
	// Load and Store map a step index onto the promoted slot it now reads or
	// writes. A step in either map is not emitted as a memory access at all.
	Load  map[int]FrameSlot
	Store map[int]FrameSlot
	// Slots is every promoted slot, in offset order, for the prologue.
	Slots []FrameSlot
	// Extra is how many additional Lua locals the promotion costs.
	Extra int
}

// Promoted reports whether anything was promoted, so the emitter can skip the
// whole path cheaply.
func (fr *Frame) Promoted() bool { return fr != nil && len(fr.Slots) > 0 }

// LoadAt reports the promoted slot step i reads, if any.
func (fr *Frame) LoadAt(i int) (FrameSlot, bool) {
	if fr == nil {
		return FrameSlot{}, false
	}
	s, ok := fr.Load[i]
	return s, ok
}

// StoreAt reports the promoted slot step i writes, if any.
func (fr *Frame) StoreAt(i int) (FrameSlot, bool) {
	if fr == nil {
		return FrameSlot{}, false
	}
	s, ok := fr.Store[i]
	return s, ok
}

// Frames runs typed-slot promotion over one function.
//
// # What it looks for
//
// LLVM gives a wasm function a shadow stack because wasm locals cannot have
// addresses. The prologue is always the same five instructions:
//
//	global.get $__stack_pointer
//	i32.const  <frame size>
//	i32.sub
//	local.tee  $fp
//	global.set $__stack_pointer
//
// and every frame access is a load or store whose address is $fp plus a
// constant. When NO other use of $fp exists, the frame is invisible from
// outside the function, and each (offset, width) pair can become a Lua local:
// the store and the matching load both disappear, and for an f64 that removes
// an IEEE-754 disassembly and reassembly per access.
//
// # What it assumes, and why that is stated rather than hidden
//
// A frame is private only because the module respects the stack-pointer
// convention it set up itself. Nothing in the wasm spec stops a module from
// computing the same address some other way and reading it, so this pass is
// sound for compiler output and not for arbitrary wasm. That is why it starts
// at -opt=2 rather than -opt=1: level 1 makes no whole-program assumption at
// all, and is the level for anyone who will not accept this one.
//
// The conditions below are tighter than the assumption needs, because a missed
// promotion costs a few instructions and a wrong one costs correctness.
func Frames(f *ir.Func) *Frame {
	if f.Mod == nil || f.Unsupported != nil {
		return nil
	}
	sp, fp, ok := prologue(f)
	if !ok {
		return nil
	}

	// derived[slot] is the frame-relative byte offset the slot holds.
	derived := map[ir.Slot]uint32{}
	var accesses []frameAccess

	for i := prologueLen; i < len(f.Steps); i++ {
		s := &f.Steps[i]

		// A slot's contents are only known along a straight line. At a label
		// something else could have jumped here with a different value in it.
		if isBoundary(s.Op) {
			derived = map[ir.Slot]uint32{}
			continue
		}

		// The frame pointer must not be reassigned, and the stack pointer must
		// not be re-read: either one could produce a second name for the frame
		// that the escape check below never sees.
		switch s.Op {
		case wasm.OpLocalSet, wasm.OpLocalTee:
			if s.Instr.LocalIndex == fp {
				return nil
			}
		case wasm.OpGlobalGet:
			if s.Instr.GlobalIndex == sp {
				return nil
			}
		}

		// Which operands are frame addresses? Every operand, not the first
		// few: a frame pointer handed to a call as its sixth argument escapes
		// exactly as much as one handed over as its first.
		frameArg := make([]bool, len(s.Args))
		anyFrame := false
		for k, a := range s.Args {
			if _, ok := derived[a]; ok {
				frameArg[k] = true
				anyFrame = true
			}
		}

		switch {
		case s.Op == wasm.OpLocalGet && s.Instr.LocalIndex == fp:
			derived[s.Dst] = 0
			continue

		case s.Op == wasm.OpI32Add && anyFrame:
			// frame + constant is still a frame address; frame + anything else
			// is an address we cannot name statically, so the frame escapes.
			if len(s.Args) != 2 {
				return nil
			}
			base, delta := 0, 1
			if !frameArg[0] {
				base, delta = 1, 0
			}
			if frameArg[0] && frameArg[1] {
				return nil
			}
			k, isConst := constStep(f, i, s.Args[delta])
			if !isConst {
				return nil
			}
			derived[s.Dst] = derived[s.Args[base]] + k
			continue

		case s.Op == wasm.OpGlobalSet && s.Instr.GlobalIndex == sp && anyFrame:
			// The epilogue, restoring the stack pointer. The only place a frame
			// address is allowed to leave.
			continue
		}

		if !anyFrame {
			// A slot that held a frame address and is now something else must
			// stop being one, because slots are recycled as the operand stack
			// pops.
			if s.Dst != ir.NoSlot {
				delete(derived, s.Dst)
				delete(derived, s.Dst+1)
			}
			continue
		}

		typ, width, store, isAccess := memAccess(s.Op)
		if !isAccess || len(frameArg) == 0 || !frameArg[0] {
			// A frame address reaching anything else -- a call argument, a
			// store's VALUE operand, a comparison -- is an escape.
			return nil
		}
		if store && frameArg[1] {
			return nil
		}
		accesses = append(accesses, frameAccess{
			step: i, offset: derived[s.Args[0]] + s.Instr.MemOffset,
			typ: typ, width: width, store: store,
		})
		if s.Dst != ir.NoSlot {
			delete(derived, s.Dst)
		}
	}

	return buildFrame(f, accesses)
}

// prologueLen is how many steps the shadow-stack prologue occupies.
const prologueLen = 5

// prologue matches the five-instruction frame setup and returns the stack
// pointer global and the frame pointer local.
func prologue(f *ir.Func) (sp uint32, fp uint32, ok bool) {
	if len(f.Steps) < prologueLen {
		return 0, 0, false
	}
	s := f.Steps
	if s[0].Op != wasm.OpGlobalGet || s[1].Op != wasm.OpI32Const ||
		s[2].Op != wasm.OpI32Sub || s[3].Op != wasm.OpLocalTee ||
		s[4].Op != wasm.OpGlobalSet {
		return 0, 0, false
	}
	sp = s[0].Instr.GlobalIndex
	if int(sp) >= len(f.Mod.Globals) {
		return 0, 0, false
	}
	g := f.Mod.Globals[sp]
	if g.Type != wasm.I32 || !g.Mutable {
		return 0, 0, false
	}
	if s[4].Instr.GlobalIndex != sp {
		return 0, 0, false
	}
	if len(s[2].Args) != 2 || s[2].Args[0] != s[0].Dst || s[2].Args[1] != s[1].Dst {
		return 0, 0, false
	}
	if len(s[3].Args) != 1 || s[3].Args[0] != s[2].Dst {
		return 0, 0, false
	}
	if len(s[4].Args) != 1 || s[4].Args[0] != s[3].Dst {
		return 0, 0, false
	}
	fp = s[3].Instr.LocalIndex
	if f.LocalType(fp) != wasm.I32 {
		return 0, 0, false
	}
	return sp, fp, true
}

// constStep reports the constant a slot holds, when it was written by an
// i32.const. Only a literal counts: the whole point is a statically known
// offset, and anything else means the address is dynamic.
func constStep(f *ir.Func, use int, slot ir.Slot) (uint32, bool) {
	// Backwards from the USE, not from the end of the function: slots are
	// recycled, so the last step in the whole body that wrote this slot is very
	// often a different value entirely.
	for i := use - 1; i >= 0; i-- {
		if isBoundary(f.Steps[i].Op) {
			return 0, false
		}
		if f.Steps[i].Dst != slot {
			continue
		}
		if f.Steps[i].Op != wasm.OpI32Const {
			return 0, false
		}
		return f.Steps[i].Instr.I32, true
	}
	return 0, false
}

type frameAccess struct {
	step   int
	offset uint32
	typ    wasm.ValType
	width  uint32
	store  bool
}

// buildFrame turns a consistent set of accesses into promoted slots.
//
// Consistent means: every access at a given offset has the same type and
// width, and no two offsets overlap. A frame slot written as an i64 and read
// back as two i32 halves is a union, and promoting it would change what the
// second read sees -- so the whole function is refused rather than that one
// offset, because the refused offset's memory is real and the promoted ones
// would no longer agree with it.
func buildFrame(f *ir.Func, accesses []frameAccess) *Frame {
	if len(accesses) == 0 {
		return nil
	}
	byOffset := map[uint32]frameAccess{}
	for _, a := range accesses {
		prev, seen := byOffset[a.offset]
		if seen && (prev.typ != a.typ || prev.width != a.width) {
			return nil
		}
		byOffset[a.offset] = a
	}
	// Overlap check, over the byte ranges each promoted slot would own.
	covered := map[uint32]uint32{} // byte -> owning offset
	for off, a := range byOffset {
		for b := off; b < off+a.width; b++ {
			if owner, taken := covered[b]; taken && owner != off {
				return nil
			}
			covered[b] = off
		}
	}

	fr := &Frame{Load: map[int]FrameSlot{}, Store: map[int]FrameSlot{}}
	base := ir.Slot(f.NumSlots)
	// Promotion buys speed with Lua locals, and Lua counts those against a hard
	// limit. Spending past the budget would only push the function into the
	// frame-table spill, where a promoted slot is a table index -- slower than
	// the memory access it replaced.
	total := 0
	for _, a := range byOffset {
		total += a.typ.Slots()
	}
	if f.NumSlots+total > ir.MaxSlots {
		return nil
	}
	offsets := make([]uint32, 0, len(byOffset))
	for off := range byOffset {
		offsets = append(offsets, off)
	}
	// Sorted, so the prologue is stable across runs and a diff of generated
	// Lua is reviewable.
	for i := 1; i < len(offsets); i++ {
		for j := i; j > 0 && offsets[j] < offsets[j-1]; j-- {
			offsets[j], offsets[j-1] = offsets[j-1], offsets[j]
		}
	}
	slotOf := map[uint32]FrameSlot{}
	for _, off := range offsets {
		a := byOffset[off]
		fs := FrameSlot{Base: base, Type: a.typ, Offset: off}
		slotOf[off] = fs
		fr.Slots = append(fr.Slots, fs)
		base += ir.Slot(a.typ.Slots())
	}
	fr.Extra = int(base) - f.NumSlots

	for _, a := range accesses {
		if a.store {
			fr.Store[a.step] = slotOf[a.offset]
		} else {
			fr.Load[a.step] = slotOf[a.offset]
		}
	}
	return fr
}

// memAccess classifies a memory instruction.
//
// Only full-width accesses qualify. A narrowing store or a sign-extending load
// reads part of a slot, and a promoted Lua local has no parts.
func memAccess(op wasm.Op) (typ wasm.ValType, width uint32, store bool, ok bool) {
	switch op {
	case wasm.OpI32Load:
		return wasm.I32, 4, false, true
	case wasm.OpI32Store:
		return wasm.I32, 4, true, true
	case wasm.OpI64Load:
		return wasm.I64, 8, false, true
	case wasm.OpI64Store:
		return wasm.I64, 8, true, true
	case wasm.OpF32Load:
		return wasm.F32, 4, false, true
	case wasm.OpF32Store:
		return wasm.F32, 4, true, true
	case wasm.OpF64Load:
		return wasm.F64, 8, false, true
	case wasm.OpF64Store:
		return wasm.F64, 8, true, true
	}
	return 0, 0, false, false
}
