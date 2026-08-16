package luagen

import (
	"fmt"
	"strings"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// luaExpr is one Lua expression together with the three facts the peephole
// needs before it may move it.
//
// Every op whose lowering is a single `dst = <expression>` lives here rather
// than in emitStep, so that the text a step emits and the text the peephole
// substitutes into a consumer are produced by the same code. Keeping two
// copies of a lowering is how a "% 2^32" goes missing on one path only.
type luaExpr struct {
	text string
	// prim marks a primary expression -- a name, a numeral or a call -- which
	// needs no parentheses when it is substituted into a larger expression.
	prim bool
	// dup marks an expression that costs nothing to evaluate twice: a bare name
	// or a numeral. Only these may be substituted into a lowering that names its
	// operand more than once.
	dup bool
	// traps marks an expression that can raise a wasm trap. At most one of these
	// may end up inside a single emitted expression, because Lua does not fix
	// the evaluation order of an operator's operands and two traps in flight
	// would make WHICH trap fires depend on that order.
	traps bool
}

// sub renders the expression for substitution into a larger one.
func (e luaExpr) sub() string {
	if e.prim {
		return e.text
	}
	return "(" + e.text + ")"
}

// atom builds an expression for a bare name or numeral.
func atom(text string) luaExpr { return luaExpr{text: text, prim: true, dup: true} }

// numLit builds an expression for a numeric literal, which is NOT always a
// primary expression: a negative one is a unary minus applied to a numeral.
//
// This is not pedantry. Substituting `-0.0` into the neg lowering produces
// `--0.0`, and `--` starts a Lua COMMENT -- so the rest of the line vanishes and
// the chunk fails to parse, a long way from anything to do with floats.
func numLit(text string) luaExpr {
	return luaExpr{text: text, prim: !strings.HasPrefix(text, "-"), dup: true}
}

// call builds an expression for a helper call, which needs no parentheses but
// is not free to evaluate twice.
func call(format string, args ...any) luaExpr {
	return luaExpr{text: fmt.Sprintf(format, args...), prim: true}
}

// infix builds an expression made of Lua operators, which must be parenthesised
// when it is substituted anywhere.
func infix(format string, args ...any) luaExpr {
	return luaExpr{text: fmt.Sprintf(format, args...)}
}

// constOf reports operand k of step i as a compile-time constant.
//
// Two sources, and which one is authoritative depends on the level. At -opt=0
// the only constants known are the ones operand forwarding carried in from an
// adjacent i32.const, which is what the M4 emitter did. From -opt=1 the range
// analysis is the source, and it sees strictly more: a constant that reached
// the operand through a local, or through arithmetic on other constants.
//
// It matters that this is ONE decision and not two. The range analysis records
// whether a wrap may be dropped on the assumption that a particular lowering
// was chosen; if the emitter picked its lowering from a different notion of
// "constant", the recorded range would describe code that was never emitted.
func (b *builder) constOf(fw *forwarding, i, k int) (uint32, bool) {
	if b.opt.Peephole() {
		return b.w.ArgRange(i, k).ConstU32()
	}
	return constArg(fw, i, k)
}

// constDivIsExact is the argument the constant-divisor lowerings rest on, kept
// next to them because "it is obviously fine" is how a rounding bug ships.
//
// Under Invariant A operand 0 holds an integral double a in [0, 2^32) -- the
// same thing the div_u/rem_u helpers already require of it -- and the divisor
// is an integer c in [1, 2^32). Then, in IEEE-754 doubles:
//
//   - `a % c` is exact. Lua's % is fmod plus a sign fixup, and fmod is exact
//     for every pair of finite doubles; the true remainder is an integer in
//     [0, c) and therefore representable.
//   - `a - a % c` is exact: both terms are integers below 2^53 and so is the
//     difference, which is q*c for q = floor(a/c).
//   - `(q*c) / c` is exactly q. Division is correctly rounded, the exact
//     quotient q is an integer below 2^32 and hence representable, so the
//     correctly-rounded result is q itself.
//
// VERIFIED under bin/lua52f rather than argued: 200,812 (a, c) pairs -- the
// structured corners (c = 1, 2^31, 2^32-1; a = 0, 2^31, 2^32-1) plus 200,000
// xorshift32-generated pairs -- checked against a Go uint32/int32 oracle, and
// 10,000,000 more with every divisor in [1, 2e6] against the five largest
// dividends, where the quotient and any rounding error would be biggest. Zero
// mismatches. Comparing against the runtime helpers would have proved nothing:
// div_u's body IS this expression.
//
// This is a doc comment on a name that is never called, deliberately -- the
// reasoning has to live where the next person editing these lowerings reads it.
const constDivIsExact = true

// constDivisor reports step i's divisor as a known non-zero constant.
//
// It is false at -opt=0, which has to keep reproducing the M4 emitter byte for
// byte. From -opt=1 the constant comes from the range analysis, exactly as
// constOf's does -- so a divisor that reached the operand through a local or
// through arithmetic on other constants counts, and c == 0 is refused so the
// division still traps.
func (b *builder) constDivisor(i int) (uint32, bool) {
	if !b.opt.Peephole() {
		return 0, false
	}
	k, ok := b.w.ArgRange(i, 1).ConstU32()
	if !ok || k == 0 {
		return 0, false
	}
	return k, true
}

// constDivisorS is constDivisor for the SIGNED pair, which needs more.
//
// Invariant A puts an unsigned value in the slot, so div_s must first recover
// the signed one -- a conditional subtract, which is a statement and a scratch
// register rather than an expression, and the win over one helper call is then
// not obvious. What IS clearly safe is the case the range analysis can prove:
// a dividend below 2^31 and a constant divisor in [1, 2^31) are both
// non-negative as signed values, so truncating signed division agrees with
// unsigned division digit for digit, and the two traps are both unreachable --
// c is not 0, and the INT_MIN/-1 overflow needs a divisor of -1, which is
// 0xFFFFFFFF and not below 2^31.
//
// A dividend the analysis cannot bound keeps the helper call. That is the
// common case on TinyGo output, where a signed quantity is usually built by
// arbitrary arithmetic rather than by a loop guard.
func (b *builder) constDivisorS(i int) (uint32, bool) {
	k, ok := b.constDivisor(i)
	if !ok || k >= 1<<31 {
		return 0, false
	}
	// Below(2^31) implies Lo >= 0, which also rules out a deferred wrap having
	// left something merely CONGRUENT in the slot: a deferral is only taken
	// when the value fits [0, 2^32) anyway, or when the consumer re-reduces
	// mod 2^32 -- and no division is such a consumer (analysis.absorbs).
	if !b.w.ArgRange(i, 0).Below(1 << 31) {
		return 0, false
	}
	return k, true
}

// constDivIsNative reports step i as a division that will be emitted as
// arithmetic rather than as a helper call, and therefore cannot trap.
//
// It is the single place the peephole and stepExpr have to agree about, so it
// is written once and asked twice.
func (b *builder) constDivIsNative(i int, op wasm.Op) bool {
	switch op {
	case wasm.OpI32DivU, wasm.OpI32RemU:
		_, ok := b.constDivisor(i)
		return ok
	case wasm.OpI32DivS, wasm.OpI32RemS:
		_, ok := b.constDivisorS(i)
		return ok
	}
	return false
}

// stepExpr renders the ops that lower to a single assignment.
//
// Returning false means the op needs more than one statement -- a scratch
// register, a bounds check with two writes, a multi-value call -- and emitStep
// handles it. Those are exactly the ops the peephole cannot forward, which is
// not a coincidence: an op that cannot be written as one expression cannot be
// substituted into one either.
func stepExpr(b *builder, f *ir.Func, i int, fw *forwarding) (luaExpr, bool) {
	s := f.Steps[i]
	var a, c string
	if len(fw.args[i]) > 0 {
		a = fw.args[i][0]
	}
	if len(fw.args[i]) > 1 {
		c = fw.args[i][1]
	}
	konst := func(k int) (uint32, bool) { return b.constOf(fw, i, k) }
	// pass hands an operand straight back as the result, for the lowerings that
	// are the identity. It has to carry the operand's own dupability: the text
	// may be a whole substituted expression, and claiming a composite is free to
	// evaluate twice is how an operand named four times becomes four loads.
	pass := func(k int) luaExpr {
		return luaExpr{text: fw.args[i][k], prim: true, dup: fw.dupable[i][k]}
	}

	// A promoted load is not a memory access any more: it reads the Lua local
	// the matching store wrote. Wide ones go through emitStep, which can name
	// both halves.
	if fs, ok := b.fr.LoadAt(i); ok {
		if fs.Type.Slots() > 1 {
			return luaExpr{}, false
		}
		return atom(b.slotName(fs.Base)), true
	}

	switch s.Op {
	// -- moves --------------------------------------------------------------
	case wasm.OpI32Const:
		return atom(u32(s.Instr.I32)), true

	case wasm.OpLocalGet:
		lt := f.LocalType(s.Instr.LocalIndex)
		if lt.Slots() > 1 {
			return luaExpr{}, false
		}
		return atom(b.slotName(f.LocalSlot(s.Instr.LocalIndex))), true

	case wasm.OpGlobalGet:
		if s.DstType.Slots() > 1 {
			return luaExpr{}, false
		}
		return atom(globalName(int(s.Instr.GlobalIndex))), true

	case wasm.OpI32WrapI64:
		// The low half IS the wrapped value.
		return atom(b.slotName(s.Args[0])), true

	// -- arithmetic ---------------------------------------------------------
	//
	// % is the measured-cheapest wrap: 2.81 ns against 3.66-5.34 for a
	// conditional fixup and 19.15 for bit32. It is also branch-free, so the
	// cost does not depend on whether the value actually overflowed.
	case wasm.OpI32Add:
		if b.w.Elided(i) {
			return infix("%s + %s", a, c), true
		}
		return infix("(%s + %s) %% %s", a, c, wrapMod), true

	case wasm.OpI32Sub:
		// Lua's % is floored, so a negative difference wraps to the correct
		// two's-complement value without any explicit fixup.
		if b.w.Elided(i) {
			return infix("%s - %s", a, c), true
		}
		return infix("(%s - %s) %% %s", a, c, wrapMod), true

	case wasm.OpI32Mul:
		if k, ok := konst(1); ok && k < 1<<21 {
			// Constant-multiply specialisation: 2.88 ns against 53.99 for the
			// general split, an 18.75x difference. Struct offsets, array strides
			// and hash multipliers are nearly all small constants, so this is
			// not an optional optimisation.
			if b.w.Elided(i) {
				return infix("%s * %s", a, u32(k)), true
			}
			return infix("(%s * %s) %% %s", a, u32(k), wrapMod), true
		}
		return call("mul32(%s, %s)", a, c), true

	// -- division -----------------------------------------------------------
	//
	// A constant non-zero divisor turns the helper CALL into native arithmetic.
	// The call is the expensive part: breaking one i32 load apart measured the
	// CALL at 34% of its cost, the largest single component, and a division
	// helper is the same shape. See constDivIsExact for why the arithmetic is
	// exact rather than merely close, and mayNotEvaluate for the hazard that
	// every constant-specialised lowering carries.
	case wasm.OpI32DivU:
		if k, ok := b.constDivisor(i); ok {
			if k == 1 {
				return pass(0), true // x / 1, and operand 0 is still named
			}
			// (a - a % k) / k. A power of two is not a special case: this IS
			// the shr_u lowering, measured at 5.04 ns against 12.99 for
			// math.floor(a/2^n) and 17.46 for bit32.rshift, so there is
			// nothing cheaper to reach for.
			return infix("(%s - %s %% %s) / %s", a, a, u32(k), u32(k)), true
		}
		return luaExpr{text: fmt.Sprintf("div_u(%s, %s)", a, c), prim: true, traps: true}, true

	case wasm.OpI32RemU:
		if k, ok := b.constDivisor(i); ok {
			// Lua's % is already the remainder, and for a power of two it is
			// already the and-with-low-mask lowering. One operator either way.
			return infix("%s %% %s", a, u32(k)), true
		}
		return luaExpr{text: fmt.Sprintf("rem_u(%s, %s)", a, c), prim: true, traps: true}, true

	// The signed pair specialises only when the range analysis has proved the
	// DIVIDEND below 2^31 as well, because Invariant A puts an unsigned value
	// in the slot and recovering the signed one costs a conditional -- which is
	// three statements and a scratch register, not an expression. Under that
	// proof both operands are non-negative signed values, so signed and
	// unsigned division agree exactly and the INT_MIN/-1 overflow trap is
	// unreachable (it needs a divisor of -1, i.e. 0xFFFFFFFF, which is not
	// below 2^31).
	case wasm.OpI32DivS:
		if k, ok := b.constDivisorS(i); ok {
			if k == 1 {
				return pass(0), true
			}
			return infix("(%s - %s %% %s) / %s", a, a, u32(k), u32(k)), true
		}
		return luaExpr{text: fmt.Sprintf("div_s(%s, %s)", a, c), prim: true, traps: true}, true

	case wasm.OpI32RemS:
		if k, ok := b.constDivisorS(i); ok {
			return infix("%s %% %s", a, u32(k)), true
		}
		return luaExpr{text: fmt.Sprintf("rem_s(%s, %s)", a, c), prim: true, traps: true}, true

	// -- bitwise ------------------------------------------------------------
	case wasm.OpI32And:
		if k, ok := konst(1); ok {
			if n, isMask := lowMask(k); isMask {
				return infix("%s %% %s", a, u32(1<<n)), true // and with 2^n-1
			}
			if n, isAlign := highMask(k); isAlign {
				return infix("%s - %s %% %s", a, a, u32(1<<n)), true // align down
			}
			if k == 0xFFFFFFFF {
				return pass(0), true
			}
			if k == 0 {
				return atom("0"), true
			}
		}
		return call("band(%s, %s)", a, c), true

	case wasm.OpI32Or:
		if k, ok := konst(1); ok {
			if k == 0 {
				return pass(0), true
			}
			if k == 0xFFFFFFFF {
				return atom(maxU32), true
			}
		}
		return call("bor(%s, %s)", a, c), true

	case wasm.OpI32Xor:
		if k, ok := konst(1); ok {
			if k == 0xFFFFFFFF {
				return infix("%s - %s", maxU32, a), true // complement
			}
			if k == 0 {
				return pass(0), true
			}
		}
		return call("bxor(%s, %s)", a, c), true

	// -- shifts and rotates -------------------------------------------------
	case wasm.OpI32Shl:
		if k, ok := konst(1); ok {
			n := k % 32
			if n == 0 {
				return pass(0), true
			}
			// Mask first: a * 2^n alone can reach 2^63 and lose precision.
			return infix("(%s %% %s) * %s", a, u32(1<<(32-n)), u32(1<<n)), true
		}
		return call("shl32(%s, %s)", a, c), true

	case wasm.OpI32ShrU:
		if k, ok := konst(1); ok {
			n := k % 32
			if n == 0 {
				return pass(0), true
			}
			// (a - a%2^n)/2^n measured 5.04 ns against 12.99 for
			// math.floor(a/2^n) and 17.46 for bit32.rshift.
			return infix("(%s - %s %% %s) / %s", a, a, u32(1<<n), u32(1<<n)), true
		}
		return call("shr_u32(%s, %s)", a, c), true

	case wasm.OpI32ShrS:
		if _, ok := konst(1); ok {
			return luaExpr{}, false // needs a scratch register
		}
		return call("shr_s32(%s, %s)", a, c), true

	case wasm.OpI32Rotl:
		if _, ok := konst(1); ok {
			return luaExpr{}, false
		}
		return call("rotl32(%s, %s)", a, c), true
	case wasm.OpI32Rotr:
		if _, ok := konst(1); ok {
			return luaExpr{}, false
		}
		return call("rotr32(%s, %s)", a, c), true

	// -- unary --------------------------------------------------------------
	case wasm.OpI32Clz:
		return call("clz32(%s)", a), true
	case wasm.OpI32Ctz:
		return call("ctz32(%s)", a), true
	case wasm.OpI32Popcnt:
		return call("popcnt32(%s)", a), true

	// -- comparisons ---------------------------------------------------------
	//
	// A comparison is a Lua BOOLEAN that has to become 0 or 1 to live in a slot.
	// When its consumer is a branch, condExpr hands over the boolean directly
	// and the `and 1 or 0` never happens.
	case wasm.OpI32Eqz, wasm.OpI32Eq, wasm.OpI32Ne,
		wasm.OpI32LtU, wasm.OpI32LeU, wasm.OpI32GtU, wasm.OpI32GeU,
		wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS,
		wasm.OpF32Eq, wasm.OpF32Ne, wasm.OpF32Lt, wasm.OpF32Gt, wasm.OpF32Le, wasm.OpF32Ge,
		wasm.OpF64Eq, wasm.OpF64Ne, wasm.OpF64Lt, wasm.OpF64Gt, wasm.OpF64Le, wasm.OpF64Ge:
		cond, ok := condExpr(b, f, i, fw, false)
		if !ok {
			return luaExpr{}, false
		}
		return infix("%s and 1 or 0", cond), true

	// -- i64 predicates, which yield an i32 -----------------------------------
	case wasm.OpI64Eq:
		return call("i64_eq(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64Ne:
		return call("i64_ne(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64LtS:
		return call("i64_lts(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64LtU:
		return call("i64_ltu(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64GtS:
		return call("i64_gts(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64GtU:
		return call("i64_gtu(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64LeS:
		return call("i64_les(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64LeU:
		return call("i64_leu(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64GeS:
		return call("i64_ges(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64GeU:
		return call("i64_geu(%s, %s)", b.wideArg(s, 0), b.wideArg(s, 1)), true
	case wasm.OpI64Eqz:
		return call("i64_eqz(%s)", b.wideArg(s, 0)), true

	// -- linear memory --------------------------------------------------------
	case wasm.OpI32Load:
		// At -opt=3 an i32 load is INLINED, which means it stops being an
		// expression: the aligned fast path needs a scratch slot and a branch,
		// and neither fits inside a Lua expression. Refusing to forward here
		// is what routes it to emitStep's statement form.
		//
		// This is a real trade rather than a free win. The load can no longer
		// be folded into a larger expression, so -opt=2's forwarding gives up
		// ground to buy the call. Measured on `chase`, which is two loads in a
		// loop, buying the call wins by 1.38x.
		if b.inlineLoads() {
			return luaExpr{}, false
		}
		return loadExpr("ld32(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset)), true
	case wasm.OpI32Load8U:
		// Same trade as i32.load one width up: at the inlining level this stops
		// being an expression so emitStep can expand it, giving up folding into
		// a larger expression to buy the call.
		if b.inlineByteLoads() {
			return luaExpr{}, false
		}
		return loadExpr("ld8(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset)), true
	case wasm.OpI32Load16U:
		if b.inlineByteLoads() {
			return luaExpr{}, false
		}
		return loadExpr("ld16(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset)), true
	case wasm.OpF32Load:
		if b.exact() {
			return loadExpr("xld_f32(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset)), true
		}
		return loadExpr("bits_to_f32(ld32(MEM, MEMSIZE, %s))", addrExpr(a, s.Instr.MemOffset)), true
	case wasm.OpF64Load:
		// Same trade as the i32 load: at -opt=3 the aligned fast path is
		// inlined, which needs scratch slots and two nested branches, and
		// neither fits inside a Lua expression. Refusing here is what routes it
		// to emitStep's statement form.
		if b.inlineLoads() {
			return luaExpr{}, false
		}
		return loadExpr(b.pfx()+"ld_f64(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset)), true
	case wasm.OpMemorySize:
		return infix("MEMSIZE / 65536"), true

	// -- floating point -------------------------------------------------------
	case wasm.OpF32Const:
		if b.exact() && isNonCanonicalNaN32(s.Instr.F32) {
			return call("boxf32(%d)", s.Instr.F32), true
		}
		return numLit(f32Literal(s.Instr.F32)), true
	case wasm.OpF64Const:
		if b.exact() && isNonCanonicalNaN64(s.Instr.F64) {
			return call("boxf64(%d, %d)",
				uint32(s.Instr.F64&0xFFFFFFFF), uint32(s.Instr.F64>>32)), true
		}
		return numLit(f64Literal(s.Instr.F64)), true

	case wasm.OpF64Add:
		if b.exact() {
			return call("xadd(%s, %s)", a, c), true
		}
		return infix("%s + %s", a, c), true
	case wasm.OpF64Sub:
		if b.exact() {
			return call("xsub(%s, %s)", a, c), true
		}
		return infix("%s - %s", a, c), true
	case wasm.OpF64Mul:
		if b.exact() {
			return call("xmul(%s, %s)", a, c), true
		}
		return infix("%s * %s", a, c), true
	case wasm.OpF64Div:
		if b.exact() {
			return call("xdiv(%s, %s)", a, c), true
		}
		return infix("%s / %s", a, c), true
	case wasm.OpF32Add:
		if b.exact() {
			return call("xf32(xadd(%s, %s))", a, c), true
		}
		return call("f32(%s + %s)", a, c), true
	case wasm.OpF32Sub:
		if b.exact() {
			return call("xf32(xsub(%s, %s))", a, c), true
		}
		return call("f32(%s - %s)", a, c), true
	case wasm.OpF32Mul:
		if b.exact() {
			return call("xf32(xmul(%s, %s))", a, c), true
		}
		return call("f32(%s * %s)", a, c), true
	case wasm.OpF32Div:
		if b.exact() {
			return call("xf32(xdiv(%s, %s))", a, c), true
		}
		return call("f32(%s / %s)", a, c), true

	case wasm.OpF32Min, wasm.OpF64Min:
		if b.exact() {
			return call("xmin(%s, %s)", a, c), true
		}
		return call("fmin(%s, %s)", a, c), true
	case wasm.OpF32Max, wasm.OpF64Max:
		if b.exact() {
			return call("xmax(%s, %s)", a, c), true
		}
		return call("fmax(%s, %s)", a, c), true
	case wasm.OpF32Copysign, wasm.OpF64Copysign:
		if b.exact() {
			return call("xcopysign(%s, %s)", a, c), true
		}
		return call("copysign(%s, %s)", a, c), true

	case wasm.OpF32Abs, wasm.OpF64Abs:
		if b.exact() {
			return call("xabs(%s)", a), true
		}
		// Not math.abs: a comparison is cheaper than a C call, and -0.0 needs
		// handling either way. Names the operand four times, which is why
		// duplicatesOperand refuses a composite forward into it.
		return infix("%s < 0.0 and -%s or (%s == 0.0 and 0.0 or %s)", a, a, a, a), true
	case wasm.OpF32Neg, wasm.OpF64Neg:
		if b.exact() {
			return call("xneg(%s)", a), true
		}
		return infix("-%s", a), true
	case wasm.OpF32Ceil, wasm.OpF64Ceil:
		if b.exact() {
			return call("xceil(%s)", a), true
		}
		return call("fceil(%s)", a), true
	case wasm.OpF32Floor, wasm.OpF64Floor:
		if b.exact() {
			return call("xfloor(%s)", a), true
		}
		return call("ffloor(%s)", a), true
	case wasm.OpF32Trunc, wasm.OpF64Trunc:
		if b.exact() {
			return call("xtrunc(%s)", a), true
		}
		return call("ftrunc(%s)", a), true
	case wasm.OpF32Nearest, wasm.OpF64Nearest:
		if b.exact() {
			return call("xnearest(%s)", a), true
		}
		return call("fnearest(%s)", a), true
	case wasm.OpF32Sqrt:
		if b.exact() {
			return call("xf32(xsqrt(%s))", a), true
		}
		return call("f32(fsqrt(%s))", a), true
	case wasm.OpF64Sqrt:
		if b.exact() {
			return call("xsqrt(%s)", a), true
		}
		return call("fsqrt(%s)", a), true

	// -- conversions ----------------------------------------------------------
	case wasm.OpI32TruncSatF32S, wasm.OpI32TruncSatF64S:
		return call("%strunc_sat_s(%s)", b.pfx(), a), true
	case wasm.OpI32TruncSatF32U, wasm.OpI32TruncSatF64U:
		return call("%strunc_sat_u(%s)", b.pfx(), a), true
	case wasm.OpI32TruncF32S, wasm.OpI32TruncF64S:
		return luaExpr{text: fmt.Sprintf("%strunc_s(%s)", b.pfx(), a), prim: true, traps: true}, true
	case wasm.OpI32TruncF32U, wasm.OpI32TruncF64U:
		return luaExpr{text: fmt.Sprintf("%strunc_u(%s)", b.pfx(), a), prim: true, traps: true}, true
	case wasm.OpF64ConvertI32S:
		return call("conv_s(%s)", a), true
	case wasm.OpF64ConvertI32U:
		// i32 is already an unsigned double, so this is the identity.
		return pass(0), true
	case wasm.OpF32ConvertI32S:
		// An i32 is never a NaN, so no box can appear here.
		return call("f32(conv_s(%s))", a), true
	case wasm.OpF32ConvertI32U:
		return call("f32(%s)", a), true
	case wasm.OpF32DemoteF64:
		if b.exact() {
			return call("xdemote(%s)", a), true
		}
		return call("f32(%s)", a), true
	case wasm.OpF64PromoteF32:
		if b.exact() {
			return call("xpromote(%s)", a), true
		}
		// Every f32 is exactly representable as an f64.
		return pass(0), true
	case wasm.OpI32ReinterpretF32:
		return call("%sf32_to_bits(%s)", b.pfx(), a), true
	case wasm.OpF32ReinterpretI32:
		return call("%sbits_to_f32(%s)", b.pfx(), a), true
	case wasm.OpF32ConvertI64S:
		// NOT f32(i64_to_f64_s(...)): that rounds twice and lands on the wrong
		// side of an f32 tie. i64_to_f32_* rounds once, via a sticky bit.
		return call("i64_to_f32_s(%s)", b.wideArg(s, 0)), true
	case wasm.OpF32ConvertI64U:
		return call("i64_to_f32_u(%s)", b.wideArg(s, 0)), true
	case wasm.OpF64ConvertI64S:
		return call("i64_to_f64_s(%s)", b.wideArg(s, 0)), true
	case wasm.OpF64ConvertI64U:
		return call("i64_to_f64_u(%s)", b.wideArg(s, 0)), true
	case wasm.OpF64ReinterpretI64:
		return call("%sbits_to_f64(%s)", b.pfx(), b.wideArg(s, 0)), true
	}
	return luaExpr{}, false
}

// wideArg names both halves of a wide operand. A wide value is never forwarded,
// so its slots always hold what they claim to.
func (b *builder) wideArg(s ir.Step, k int) string { return b.slotNames(s.Args[k], wasm.I64) }

func loadExpr(format, addr string) luaExpr {
	return luaExpr{text: fmt.Sprintf(format, addr), prim: true, traps: true}
}

// condExpr renders a comparison as a Lua BOOLEAN, optionally negated.
//
// Two things fall out of having it. A comparison whose consumer is a branch
// skips materialising 0 or 1 into a slot and testing it again -- three VM
// instructions per branch, in every loop a guest has. And the negated form is
// what lets `if` jump to its else-label without a `not`.
func condExpr(b *builder, f *ir.Func, i int, fw *forwarding, negate bool) (string, bool) {
	s := f.Steps[i]
	var a, c string
	if len(fw.args[i]) > 0 {
		a = fw.args[i][0]
	}
	if len(fw.args[i]) > 1 {
		c = fw.args[i][1]
	}

	rel := func(op, inv string) (string, bool) {
		if negate {
			op = inv
		}
		return fmt.Sprintf("%s %s %s", a, op, c), true
	}

	switch s.Op {
	case wasm.OpI32Eqz:
		if negate {
			return a + " ~= 0", true
		}
		return a + " == 0", true

	case wasm.OpI32Eq:
		return rel("==", "~=")
	case wasm.OpI32Ne:
		return rel("~=", "==")

	// Unsigned comparisons are direct, which is the payoff of Invariant A.
	case wasm.OpI32LtU:
		return rel("<", ">=")
	case wasm.OpI32LeU:
		return rel("<=", ">")
	case wasm.OpI32GtU:
		return rel(">", "<=")
	case wasm.OpI32GeU:
		return rel(">=", "<")

	case wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS:
		if !b.opt.Peephole() {
			// -opt=0 keeps the M4 lowering, which needs two scratch registers
			// and so is not an expression at all.
			return "", false
		}
		op := map[wasm.Op]string{
			wasm.OpI32LtS: "<", wasm.OpI32LeS: "<=",
			wasm.OpI32GtS: ">", wasm.OpI32GeS: ">=",
		}[s.Op]
		if negate {
			op = map[string]string{"<": ">=", "<=": ">", ">": "<=", ">=": "<"}[op]
		}
		ra, rc := b.w.ArgRange(i, 0), b.w.ArgRange(i, 1)
		if ra.Below(1<<31) && rc.Below(1<<31) {
			// Both operands are provably non-negative as signed values, so the
			// unsigned order IS the signed order and the compare is direct.
			return fmt.Sprintf("%s %s %s", a, op, c), true
		}
		// Otherwise bias both sides by 2^31. Signed order is the unsigned order
		// rotated by half the range, so adding 2^31 modulo 2^32 turns one into
		// the other -- branch-free, and one expression rather than the three
		// statements a pair of conditional sign fixups needs.
		return fmt.Sprintf("%s %s %s", biased(a, ra), op, biased(c, rc)), true

	// Float comparisons: NaN makes every one of them false, which Lua's
	// operators already do. In exact mode an operand may be a boxed table, so
	// they route through helpers instead and cannot be a bare boolean.
	//
	// NEGATION IS NOT AN OPERATOR SWAP HERE -- see frel.
	case wasm.OpF32Eq, wasm.OpF64Eq:
		if b.exact() {
			return xcond(b, "xeq", a, c, negate), true
		}
		return frel("==", a, c, negate), true
	case wasm.OpF32Ne, wasm.OpF64Ne:
		if b.exact() {
			return xcond(b, "xne", a, c, negate), true
		}
		return frel("~=", a, c, negate), true
	case wasm.OpF32Lt, wasm.OpF64Lt:
		if b.exact() {
			return xcond(b, "xlt", a, c, negate), true
		}
		return frel("<", a, c, negate), true
	case wasm.OpF32Gt, wasm.OpF64Gt:
		if b.exact() {
			return xcond(b, "xgt", a, c, negate), true
		}
		return frel(">", a, c, negate), true
	case wasm.OpF32Le, wasm.OpF64Le:
		if b.exact() {
			return xcond(b, "xle", a, c, negate), true
		}
		return frel("<=", a, c, negate), true
	case wasm.OpF32Ge, wasm.OpF64Ge:
		if b.exact() {
			return xcond(b, "xge", a, c, negate), true
		}
		return frel(">=", a, c, negate), true
	}
	return "", false
}

// frel renders a FLOAT comparison, whose negation cannot be an operator swap.
//
// Every float comparison is false when either operand is NaN, so `lt` and `ge`
// are NOT complements -- both are false for a NaN. Swapping the operator the
// way rel() does therefore leaves the negated test false as well, and `if`,
// which jumps to its else-label when the condition is FALSE, walks into the
// then-arm on a NaN where wasm takes the else-arm.
//
// Negating the comparison itself is the true complement. It costs one VM
// instruction, and only on the negated path: `if` is the only consumer that
// asks for one, because br_if and select test the condition directly.
//
// This is the same trap xcond exists for one mode over, which is why exact mode
// never had the bug: it negates the HELPER's 0/1 result rather than the
// operator.
func frel(op, a, c string, negate bool) string {
	if negate {
		return fmt.Sprintf("not (%s %s %s)", a, op, c)
	}
	return fmt.Sprintf("%s %s %s", a, op, c)
}

// xcond wraps an exact-mode comparison helper, which returns 0 or 1 rather than
// a boolean, back into a boolean.
//
// NOT a plain negation of the operator: a float comparison is false when either
// operand is NaN, so `lt` and `ge` are not complements. Negating the HELPER's
// 0/1 result keeps that right, at the cost of one extra compare.
func xcond(b *builder, name, a, c string, negate bool) string {
	op := "~="
	if negate {
		op = "=="
	}
	return fmt.Sprintf("%s(%s, %s) %s 0", name, a, c, op)
}

// biased renders `(x + 2^31) % 2^32`, folding it away when x is a constant.
func biased(x string, r analysis.Range) string {
	if k, ok := r.ConstU32(); ok {
		return u32(k + 1<<31)
	}
	return fmt.Sprintf("(%s + %s) %% %s", x, signMin, wrapMod)
}

// duplicatesOperand reports a lowering that names operand k more than once.
//
// Substituting a composite expression into one of these would evaluate it
// twice: twice the work, and -- if it can trap -- twice the chance to trap,
// from a program point where the wasm had one operation. A bare name or numeral
// is fine, which is why the M4 forwarding of local.get and i32.const never had
// to think about this.
func duplicatesOperand(op wasm.Op, k int) bool {
	switch op {
	case wasm.OpF32Abs, wasm.OpF64Abs:
		return true
	case wasm.OpLocalTee:
		return true
	case wasm.OpI64ExtendI32S:
		return true
	case wasm.OpI32And, wasm.OpI32ShrU, wasm.OpI32Rotl, wasm.OpI32Rotr,
		wasm.OpI32DivU, wasm.OpI32DivS:
		// Only their constant-shift lowerings repeat the operand, but the
		// peephole asks before the lowering is chosen, so it answers for the
		// worst case. The cost of the extra caution is one forward on a bare
		// name, which was allowed anyway.
		//
		// The two divisions are here for the same reason: `(a - a % k) / k`
		// names its dividend twice, and substituting a load or a call there
		// would run it twice -- twice the work, and for a trapping expression
		// two chances to trap where the wasm had one. rem is NOT here: `a % k`
		// names it once.
		return k == 0
	}
	return false
}

// sideEffects reports a step that writes memory, a global, or anything else a
// pending expression might have read.
func sideEffects(op wasm.Op) bool {
	switch op {
	case wasm.OpI32Store, wasm.OpI32Store8, wasm.OpI32Store16,
		wasm.OpI64Store, wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32,
		wasm.OpF32Store, wasm.OpF64Store,
		wasm.OpMemoryGrow, wasm.OpMemoryCopy, wasm.OpMemoryFill, wasm.OpGlobalSet,
		wasm.OpCall, wasm.OpCallIndirect:
		return true
	}
	return false
}

// readsMemory reports a step whose value depends on linear memory or on its
// size, so a pending expression containing it cannot outlive a store or a grow.
func readsMemory(op wasm.Op) bool {
	// memory.copy reads memory as well as writing it, so a pending expression
	// that loaded from the source range cannot be forwarded past one.
	return isLoad(op) || op == wasm.OpMemorySize || op == wasm.OpMemoryCopy
}
