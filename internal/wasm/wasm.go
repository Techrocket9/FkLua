// Package wasm decodes WebAssembly modules into the narrow representation the
// FkLua compiler consumes.
//
// It wraps github.com/eliben/watgo, but deliberately does not expose watgo's
// types. Two reasons: the decoder is meant to be replaceable (we plan to own it
// by M3), and -- more immediately -- mod authors need diagnostics that name the
// offending instruction and say when it will be supported, rather than a generic
// "invalid module".
package wasm

import (
	"fmt"
	"strings"

	"github.com/eliben/watgo"
	"github.com/eliben/watgo/wasmir"
)

// ValType is a WebAssembly value type. Only the four numeric types exist in the
// feature surface we target; reference and vector types are rejected at decode.
type ValType uint8

const (
	I32 ValType = iota
	I64
	F32
	F64
)

// Slots is how many Lua locals a value of this type occupies.
//
// An i64 is a (lo, hi) pair of unsigned doubles, because a Lua number has only
// 53 bits of mantissa and cannot hold 64 bits exactly. Everything else fits in
// one slot. This is what makes "stack depth -> slot" stop being the identity
// map, and it is why the IR carries types alongside slots.
func (v ValType) Slots() int {
	if v == I64 {
		return 2
	}
	return 1
}

func (v ValType) String() string {
	switch v {
	case I32:
		return "i32"
	case I64:
		return "i64"
	case F32:
		return "f32"
	case F64:
		return "f64"
	}
	return fmt.Sprintf("valtype(%d)", uint8(v))
}

// Op is an instruction opcode in the subset FkLua supports. It is a separate
// enum from watgo's InstrKind so that swapping the decoder does not ripple into
// the IR and emitter.
type Op uint16

const (
	OpUnreachable Op = iota
	OpNop
	OpEnd
	OpReturn
	OpDrop

	OpLocalGet
	OpLocalSet
	OpLocalTee

	OpI32Const

	// Binary arithmetic
	OpI32Add
	OpI32Sub
	OpI32Mul
	OpI32DivS
	OpI32DivU
	OpI32RemS
	OpI32RemU

	// Bitwise
	OpI32And
	OpI32Or
	OpI32Xor
	OpI32Shl
	OpI32ShrS
	OpI32ShrU
	OpI32Rotl
	OpI32Rotr

	// Unary
	OpI32Clz
	OpI32Ctz
	OpI32Popcnt
	OpI32Extend8S
	OpI32Extend16S
	OpI32Eqz

	// Comparison
	OpI32Eq
	OpI32Ne
	OpI32LtS
	OpI32LtU
	OpI32LeS
	OpI32LeU
	OpI32GtS
	OpI32GtU
	OpI32GeS
	OpI32GeU

	// Control flow
	OpBlock
	OpLoop
	OpIf
	OpElse
	OpBr
	OpBrIf
	OpBrTable
	OpSelect

	// Calls and globals
	OpCall
	OpCallIndirect
	OpGlobalGet
	OpGlobalSet

	// Linear memory
	OpI32Load
	OpI32Load8S
	OpI32Load8U
	OpI32Load16S
	OpI32Load16U
	OpI32Store
	OpI32Store8
	OpI32Store16
	OpMemorySize
	OpMemoryGrow
	OpMemoryCopy
	OpMemoryFill

	// Floating point
	OpF32Const
	OpF32Add
	OpF32Sub
	OpF32Mul
	OpF32Div
	OpF32Min
	OpF32Max
	OpF32Copysign
	OpF32Abs
	OpF32Neg
	OpF32Ceil
	OpF32Floor
	OpF32Trunc
	OpF32Nearest
	OpF32Sqrt
	OpF32Eq
	OpF32Ne
	OpF32Lt
	OpF32Gt
	OpF32Le
	OpF32Ge
	OpF64Const
	OpF64Add
	OpF64Sub
	OpF64Mul
	OpF64Div
	OpF64Min
	OpF64Max
	OpF64Copysign
	OpF64Abs
	OpF64Neg
	OpF64Ceil
	OpF64Floor
	OpF64Trunc
	OpF64Nearest
	OpF64Sqrt
	OpF64Eq
	OpF64Ne
	OpF64Lt
	OpF64Gt
	OpF64Le
	OpF64Ge
	OpI32TruncF32S
	OpI32TruncF32U
	OpI32TruncF64S
	OpI32TruncF64U
	OpF32ConvertI32S
	OpF32ConvertI32U
	OpF64ConvertI32S
	OpF64ConvertI32U
	OpF32DemoteF64
	OpF64PromoteF32
	OpI32ReinterpretF32
	OpF32ReinterpretI32
	OpF32Load
	OpF64Load
	OpF32Store
	OpF64Store

	// 64-bit integers, held as a (lo, hi) pair of unsigned doubles
	OpI64Const
	OpI64Add
	OpI64Sub
	OpI64Mul
	OpI64DivS
	OpI64DivU
	OpI64RemS
	OpI64RemU
	OpI64And
	OpI64Or
	OpI64Xor
	OpI64Shl
	OpI64ShrS
	OpI64ShrU
	OpI64Rotl
	OpI64Rotr
	OpI64Eq
	OpI64Ne
	OpI64LtS
	OpI64LtU
	OpI64GtS
	OpI64GtU
	OpI64LeS
	OpI64LeU
	OpI64GeS
	OpI64GeU
	OpI64Clz
	OpI64Ctz
	OpI64Popcnt
	OpI64Eqz
	OpI64Extend8S
	OpI64Extend16S
	OpI64Extend32S
	OpI32WrapI64
	OpI64ExtendI32S
	OpI64ExtendI32U
	OpI64TruncF32S
	OpI64TruncF32U
	OpI64TruncF64S
	OpI64TruncF64U
	OpF32ConvertI64S
	OpF32ConvertI64U
	OpF64ConvertI64S
	OpF64ConvertI64U
	OpI64ReinterpretF64
	OpF64ReinterpretI64
	OpI64Load
	OpI64Load8S
	OpI64Load8U
	OpI64Load16S
	OpI64Load16U
	OpI64Load32S
	OpI64Load32U
	OpI64Store
	OpI64Store8
	OpI64Store16
	OpI64Store32

	// Saturating float->int. TinyGo's wasm-unknown target emits these
	// unconditionally, so they are not optional for our flagship guest.
	OpI32TruncSatF32S
	OpI32TruncSatF32U
	OpI32TruncSatF64S
	OpI32TruncSatF64U
	OpI64TruncSatF32S
	OpI64TruncSatF32U
	OpI64TruncSatF64S
	OpI64TruncSatF64U

	numOps
)

var opNames = [numOps]string{
	OpUnreachable: "unreachable", OpNop: "nop", OpEnd: "end",
	OpReturn: "return", OpDrop: "drop",
	OpLocalGet: "local.get", OpLocalSet: "local.set", OpLocalTee: "local.tee",
	OpI32Const: "i32.const",
	OpI32Add:   "i32.add", OpI32Sub: "i32.sub", OpI32Mul: "i32.mul",
	OpI32DivS: "i32.div_s", OpI32DivU: "i32.div_u",
	OpI32RemS: "i32.rem_s", OpI32RemU: "i32.rem_u",
	OpI32And: "i32.and", OpI32Or: "i32.or", OpI32Xor: "i32.xor",
	OpI32Shl: "i32.shl", OpI32ShrS: "i32.shr_s", OpI32ShrU: "i32.shr_u",
	OpI32Rotl: "i32.rotl", OpI32Rotr: "i32.rotr",
	OpI32Clz: "i32.clz", OpI32Ctz: "i32.ctz", OpI32Popcnt: "i32.popcnt",
	OpI32Extend8S: "i32.extend8_s", OpI32Extend16S: "i32.extend16_s",
	OpI32Eqz: "i32.eqz",
	OpI32Eq:  "i32.eq", OpI32Ne: "i32.ne",
	OpI32LtS: "i32.lt_s", OpI32LtU: "i32.lt_u",
	OpI32LeS: "i32.le_s", OpI32LeU: "i32.le_u",
	OpI32GtS: "i32.gt_s", OpI32GtU: "i32.gt_u",
	OpI32GeS: "i32.ge_s", OpI32GeU: "i32.ge_u",
	OpBlock: "block", OpLoop: "loop", OpIf: "if", OpElse: "else",
	OpBr: "br", OpBrIf: "br_if", OpBrTable: "br_table", OpSelect: "select",
	OpCall: "call", OpCallIndirect: "call_indirect",
	OpGlobalGet: "global.get", OpGlobalSet: "global.set",
	OpI32Load: "i32.load", OpI32Load8S: "i32.load8_s", OpI32Load8U: "i32.load8_u",
	OpI32Load16S: "i32.load16_s", OpI32Load16U: "i32.load16_u",
	OpI32Store: "i32.store", OpI32Store8: "i32.store8", OpI32Store16: "i32.store16",
	OpMemorySize: "memory.size", OpMemoryGrow: "memory.grow",
	OpMemoryCopy: "memory.copy", OpMemoryFill: "memory.fill",
	OpF32Const:          "f32.const",
	OpF32Add:            "f32.add",
	OpF32Sub:            "f32.sub",
	OpF32Mul:            "f32.mul",
	OpF32Div:            "f32.div",
	OpF32Min:            "f32.min",
	OpF32Max:            "f32.max",
	OpF32Copysign:       "f32.copysign",
	OpF32Abs:            "f32.abs",
	OpF32Neg:            "f32.neg",
	OpF32Ceil:           "f32.ceil",
	OpF32Floor:          "f32.floor",
	OpF32Trunc:          "f32.trunc",
	OpF32Nearest:        "f32.nearest",
	OpF32Sqrt:           "f32.sqrt",
	OpF32Eq:             "f32.eq",
	OpF32Ne:             "f32.ne",
	OpF32Lt:             "f32.lt",
	OpF32Gt:             "f32.gt",
	OpF32Le:             "f32.le",
	OpF32Ge:             "f32.ge",
	OpF64Const:          "f64.const",
	OpF64Add:            "f64.add",
	OpF64Sub:            "f64.sub",
	OpF64Mul:            "f64.mul",
	OpF64Div:            "f64.div",
	OpF64Min:            "f64.min",
	OpF64Max:            "f64.max",
	OpF64Copysign:       "f64.copysign",
	OpF64Abs:            "f64.abs",
	OpF64Neg:            "f64.neg",
	OpF64Ceil:           "f64.ceil",
	OpF64Floor:          "f64.floor",
	OpF64Trunc:          "f64.trunc",
	OpF64Nearest:        "f64.nearest",
	OpF64Sqrt:           "f64.sqrt",
	OpF64Eq:             "f64.eq",
	OpF64Ne:             "f64.ne",
	OpF64Lt:             "f64.lt",
	OpF64Gt:             "f64.gt",
	OpF64Le:             "f64.le",
	OpF64Ge:             "f64.ge",
	OpI32TruncF32S:      "i32.trunc_f32_s",
	OpI32TruncF32U:      "i32.trunc_f32_u",
	OpI32TruncF64S:      "i32.trunc_f64_s",
	OpI32TruncF64U:      "i32.trunc_f64_u",
	OpF32ConvertI32S:    "f32.convert_i32_s",
	OpF32ConvertI32U:    "f32.convert_i32_u",
	OpF64ConvertI32S:    "f64.convert_i32_s",
	OpF64ConvertI32U:    "f64.convert_i32_u",
	OpF32DemoteF64:      "f32.demote_f64",
	OpF64PromoteF32:     "f64.promote_f32",
	OpI32ReinterpretF32: "i32.reinterpret_f32",
	OpF32ReinterpretI32: "f32.reinterpret_i32",
	OpF32Load:           "f32.load",
	OpF64Load:           "f64.load",
	OpF32Store:          "f32.store",
	OpF64Store:          "f64.store",
	OpI64Const:          "i64.const",
	OpI64Add:            "i64.add",
	OpI64Sub:            "i64.sub",
	OpI64Mul:            "i64.mul",
	OpI64DivS:           "i64.div_s",
	OpI64DivU:           "i64.div_u",
	OpI64RemS:           "i64.rem_s",
	OpI64RemU:           "i64.rem_u",
	OpI64And:            "i64.and",
	OpI64Or:             "i64.or",
	OpI64Xor:            "i64.xor",
	OpI64Shl:            "i64.shl",
	OpI64ShrS:           "i64.shr_s",
	OpI64ShrU:           "i64.shr_u",
	OpI64Rotl:           "i64.rotl",
	OpI64Rotr:           "i64.rotr",
	OpI64Eq:             "i64.eq",
	OpI64Ne:             "i64.ne",
	OpI64LtS:            "i64.lt_s",
	OpI64LtU:            "i64.lt_u",
	OpI64GtS:            "i64.gt_s",
	OpI64GtU:            "i64.gt_u",
	OpI64LeS:            "i64.le_s",
	OpI64LeU:            "i64.le_u",
	OpI64GeS:            "i64.ge_s",
	OpI64GeU:            "i64.ge_u",
	OpI64Clz:            "i64.clz",
	OpI64Ctz:            "i64.ctz",
	OpI64Popcnt:         "i64.popcnt",
	OpI64Eqz:            "i64.eqz",
	OpI64Extend8S:       "i64.extend8_s",
	OpI64Extend16S:      "i64.extend16_s",
	OpI64Extend32S:      "i64.extend32_s",
	OpI32WrapI64:        "i32.wrap_i64",
	OpI64ExtendI32S:     "i64.extend_i32_s",
	OpI64ExtendI32U:     "i64.extend_i32_u",
	OpI64TruncF32S:      "i64.trunc_f32_s",
	OpI64TruncF32U:      "i64.trunc_f32_u",
	OpI64TruncF64S:      "i64.trunc_f64_s",
	OpI64TruncF64U:      "i64.trunc_f64_u",
	OpF32ConvertI64S:    "f32.convert_i64_s",
	OpF32ConvertI64U:    "f32.convert_i64_u",
	OpF64ConvertI64S:    "f64.convert_i64_s",
	OpF64ConvertI64U:    "f64.convert_i64_u",
	OpI64ReinterpretF64: "i64.reinterpret_f64",
	OpF64ReinterpretI64: "f64.reinterpret_i64",
	OpI64Load:           "i64.load",
	OpI64Load8S:         "i64.load8_s",
	OpI64Load8U:         "i64.load8_u",
	OpI64Load16S:        "i64.load16_s",
	OpI64Load16U:        "i64.load16_u",
	OpI64Load32S:        "i64.load32_s",
	OpI64Load32U:        "i64.load32_u",
	OpI64Store:          "i64.store",
	OpI64Store8:         "i64.store8",
	OpI64Store16:        "i64.store16",
	OpI64Store32:        "i64.store32",
	OpI32TruncSatF32S:   "i32.trunc_sat_f32_s",
	OpI32TruncSatF32U:   "i32.trunc_sat_f32_u",
	OpI32TruncSatF64S:   "i32.trunc_sat_f64_s",
	OpI32TruncSatF64U:   "i32.trunc_sat_f64_u",
	OpI64TruncSatF32S:   "i64.trunc_sat_f32_s",
	OpI64TruncSatF32U:   "i64.trunc_sat_f32_u",
	OpI64TruncSatF64S:   "i64.trunc_sat_f64_s",
	OpI64TruncSatF64U:   "i64.trunc_sat_f64_u",
}

func (o Op) String() string {
	if o < numOps && opNames[o] != "" {
		return opNames[o]
	}
	return fmt.Sprintf("op(%d)", uint16(o))
}

// supportedOps maps watgo's kinds onto ours. Anything absent is rejected with a
// diagnostic naming the instruction, so the set of supported opcodes is exactly
// this table -- there is no silent pass-through.
var supportedOps = map[wasmir.InstrKind]Op{
	wasmir.InstrUnreachable: OpUnreachable,
	wasmir.InstrNop:         OpNop,
	wasmir.InstrEnd:         OpEnd,
	wasmir.InstrReturn:      OpReturn,
	wasmir.InstrDrop:        OpDrop,

	wasmir.InstrLocalGet: OpLocalGet,
	wasmir.InstrLocalSet: OpLocalSet,
	wasmir.InstrLocalTee: OpLocalTee,

	wasmir.InstrI32Const: OpI32Const,

	wasmir.InstrI32Add:  OpI32Add,
	wasmir.InstrI32Sub:  OpI32Sub,
	wasmir.InstrI32Mul:  OpI32Mul,
	wasmir.InstrI32DivS: OpI32DivS,
	wasmir.InstrI32DivU: OpI32DivU,
	wasmir.InstrI32RemS: OpI32RemS,
	wasmir.InstrI32RemU: OpI32RemU,

	wasmir.InstrI32And:  OpI32And,
	wasmir.InstrI32Or:   OpI32Or,
	wasmir.InstrI32Xor:  OpI32Xor,
	wasmir.InstrI32Shl:  OpI32Shl,
	wasmir.InstrI32ShrS: OpI32ShrS,
	wasmir.InstrI32ShrU: OpI32ShrU,
	wasmir.InstrI32Rotl: OpI32Rotl,
	wasmir.InstrI32Rotr: OpI32Rotr,

	wasmir.InstrI32Clz:       OpI32Clz,
	wasmir.InstrI32Ctz:       OpI32Ctz,
	wasmir.InstrI32Popcnt:    OpI32Popcnt,
	wasmir.InstrI32Extend8S:  OpI32Extend8S,
	wasmir.InstrI32Extend16S: OpI32Extend16S,
	wasmir.InstrI32Eqz:       OpI32Eqz,

	wasmir.InstrI32Eq:  OpI32Eq,
	wasmir.InstrI32Ne:  OpI32Ne,
	wasmir.InstrI32LtS: OpI32LtS,
	wasmir.InstrI32LtU: OpI32LtU,
	wasmir.InstrI32LeS: OpI32LeS,
	wasmir.InstrI32LeU: OpI32LeU,
	wasmir.InstrI32GtS: OpI32GtS,
	wasmir.InstrI32GtU: OpI32GtU,
	wasmir.InstrI32GeS: OpI32GeS,
	wasmir.InstrI32GeU: OpI32GeU,

	wasmir.InstrBlock:   OpBlock,
	wasmir.InstrLoop:    OpLoop,
	wasmir.InstrIf:      OpIf,
	wasmir.InstrElse:    OpElse,
	wasmir.InstrBr:      OpBr,
	wasmir.InstrBrIf:    OpBrIf,
	wasmir.InstrBrTable: OpBrTable,
	wasmir.InstrSelect:  OpSelect,

	wasmir.InstrCall:         OpCall,
	wasmir.InstrCallIndirect: OpCallIndirect,
	wasmir.InstrGlobalGet:    OpGlobalGet,
	wasmir.InstrGlobalSet:    OpGlobalSet,

	wasmir.InstrI32Load:    OpI32Load,
	wasmir.InstrI32Load8S:  OpI32Load8S,
	wasmir.InstrI32Load8U:  OpI32Load8U,
	wasmir.InstrI32Load16S: OpI32Load16S,
	wasmir.InstrI32Load16U: OpI32Load16U,
	wasmir.InstrI32Store:   OpI32Store,
	wasmir.InstrI32Store8:  OpI32Store8,
	wasmir.InstrI32Store16: OpI32Store16,
	wasmir.InstrMemorySize: OpMemorySize,
	wasmir.InstrMemoryGrow: OpMemoryGrow,
	wasmir.InstrMemoryCopy: OpMemoryCopy,
	wasmir.InstrMemoryFill: OpMemoryFill,

	wasmir.InstrF32Const:          OpF32Const,
	wasmir.InstrF32Add:            OpF32Add,
	wasmir.InstrF32Sub:            OpF32Sub,
	wasmir.InstrF32Mul:            OpF32Mul,
	wasmir.InstrF32Div:            OpF32Div,
	wasmir.InstrF32Min:            OpF32Min,
	wasmir.InstrF32Max:            OpF32Max,
	wasmir.InstrF32Copysign:       OpF32Copysign,
	wasmir.InstrF32Abs:            OpF32Abs,
	wasmir.InstrF32Neg:            OpF32Neg,
	wasmir.InstrF32Ceil:           OpF32Ceil,
	wasmir.InstrF32Floor:          OpF32Floor,
	wasmir.InstrF32Trunc:          OpF32Trunc,
	wasmir.InstrF32Nearest:        OpF32Nearest,
	wasmir.InstrF32Sqrt:           OpF32Sqrt,
	wasmir.InstrF32Eq:             OpF32Eq,
	wasmir.InstrF32Ne:             OpF32Ne,
	wasmir.InstrF32Lt:             OpF32Lt,
	wasmir.InstrF32Gt:             OpF32Gt,
	wasmir.InstrF32Le:             OpF32Le,
	wasmir.InstrF32Ge:             OpF32Ge,
	wasmir.InstrF64Const:          OpF64Const,
	wasmir.InstrF64Add:            OpF64Add,
	wasmir.InstrF64Sub:            OpF64Sub,
	wasmir.InstrF64Mul:            OpF64Mul,
	wasmir.InstrF64Div:            OpF64Div,
	wasmir.InstrF64Min:            OpF64Min,
	wasmir.InstrF64Max:            OpF64Max,
	wasmir.InstrF64Copysign:       OpF64Copysign,
	wasmir.InstrF64Abs:            OpF64Abs,
	wasmir.InstrF64Neg:            OpF64Neg,
	wasmir.InstrF64Ceil:           OpF64Ceil,
	wasmir.InstrF64Floor:          OpF64Floor,
	wasmir.InstrF64Trunc:          OpF64Trunc,
	wasmir.InstrF64Nearest:        OpF64Nearest,
	wasmir.InstrF64Sqrt:           OpF64Sqrt,
	wasmir.InstrF64Eq:             OpF64Eq,
	wasmir.InstrF64Ne:             OpF64Ne,
	wasmir.InstrF64Lt:             OpF64Lt,
	wasmir.InstrF64Gt:             OpF64Gt,
	wasmir.InstrF64Le:             OpF64Le,
	wasmir.InstrF64Ge:             OpF64Ge,
	wasmir.InstrI32TruncF32S:      OpI32TruncF32S,
	wasmir.InstrI32TruncF32U:      OpI32TruncF32U,
	wasmir.InstrI32TruncF64S:      OpI32TruncF64S,
	wasmir.InstrI32TruncF64U:      OpI32TruncF64U,
	wasmir.InstrF32ConvertI32S:    OpF32ConvertI32S,
	wasmir.InstrF32ConvertI32U:    OpF32ConvertI32U,
	wasmir.InstrF64ConvertI32S:    OpF64ConvertI32S,
	wasmir.InstrF64ConvertI32U:    OpF64ConvertI32U,
	wasmir.InstrF32DemoteF64:      OpF32DemoteF64,
	wasmir.InstrF64PromoteF32:     OpF64PromoteF32,
	wasmir.InstrI32ReinterpretF32: OpI32ReinterpretF32,
	wasmir.InstrF32ReinterpretI32: OpF32ReinterpretI32,
	wasmir.InstrF32Load:           OpF32Load,
	wasmir.InstrF64Load:           OpF64Load,
	wasmir.InstrF32Store:          OpF32Store,
	wasmir.InstrF64Store:          OpF64Store,

	// 64-bit integers
	wasmir.InstrI64Const:          OpI64Const,
	wasmir.InstrI64Add:            OpI64Add,
	wasmir.InstrI64Sub:            OpI64Sub,
	wasmir.InstrI64Mul:            OpI64Mul,
	wasmir.InstrI64DivS:           OpI64DivS,
	wasmir.InstrI64DivU:           OpI64DivU,
	wasmir.InstrI64RemS:           OpI64RemS,
	wasmir.InstrI64RemU:           OpI64RemU,
	wasmir.InstrI64And:            OpI64And,
	wasmir.InstrI64Or:             OpI64Or,
	wasmir.InstrI64Xor:            OpI64Xor,
	wasmir.InstrI64Shl:            OpI64Shl,
	wasmir.InstrI64ShrS:           OpI64ShrS,
	wasmir.InstrI64ShrU:           OpI64ShrU,
	wasmir.InstrI64Rotl:           OpI64Rotl,
	wasmir.InstrI64Rotr:           OpI64Rotr,
	wasmir.InstrI64Eq:             OpI64Eq,
	wasmir.InstrI64Ne:             OpI64Ne,
	wasmir.InstrI64LtS:            OpI64LtS,
	wasmir.InstrI64LtU:            OpI64LtU,
	wasmir.InstrI64GtS:            OpI64GtS,
	wasmir.InstrI64GtU:            OpI64GtU,
	wasmir.InstrI64LeS:            OpI64LeS,
	wasmir.InstrI64LeU:            OpI64LeU,
	wasmir.InstrI64GeS:            OpI64GeS,
	wasmir.InstrI64GeU:            OpI64GeU,
	wasmir.InstrI64Clz:            OpI64Clz,
	wasmir.InstrI64Ctz:            OpI64Ctz,
	wasmir.InstrI64Popcnt:         OpI64Popcnt,
	wasmir.InstrI64Eqz:            OpI64Eqz,
	wasmir.InstrI64Extend8S:       OpI64Extend8S,
	wasmir.InstrI64Extend16S:      OpI64Extend16S,
	wasmir.InstrI64Extend32S:      OpI64Extend32S,
	wasmir.InstrI32WrapI64:        OpI32WrapI64,
	wasmir.InstrI64ExtendI32S:     OpI64ExtendI32S,
	wasmir.InstrI64ExtendI32U:     OpI64ExtendI32U,
	wasmir.InstrI64TruncF32S:      OpI64TruncF32S,
	wasmir.InstrI64TruncF32U:      OpI64TruncF32U,
	wasmir.InstrI64TruncF64S:      OpI64TruncF64S,
	wasmir.InstrI64TruncF64U:      OpI64TruncF64U,
	wasmir.InstrF32ConvertI64S:    OpF32ConvertI64S,
	wasmir.InstrF32ConvertI64U:    OpF32ConvertI64U,
	wasmir.InstrF64ConvertI64S:    OpF64ConvertI64S,
	wasmir.InstrF64ConvertI64U:    OpF64ConvertI64U,
	wasmir.InstrI64ReinterpretF64: OpI64ReinterpretF64,
	wasmir.InstrF64ReinterpretI64: OpF64ReinterpretI64,
	wasmir.InstrI64Load:           OpI64Load,
	wasmir.InstrI64Load8S:         OpI64Load8S,
	wasmir.InstrI64Load8U:         OpI64Load8U,
	wasmir.InstrI64Load16S:        OpI64Load16S,
	wasmir.InstrI64Load16U:        OpI64Load16U,
	wasmir.InstrI64Load32S:        OpI64Load32S,
	wasmir.InstrI64Load32U:        OpI64Load32U,
	wasmir.InstrI64Store:          OpI64Store,
	wasmir.InstrI64Store8:         OpI64Store8,
	wasmir.InstrI64Store16:        OpI64Store16,
	wasmir.InstrI64Store32:        OpI64Store32,

	// Saturating float->int
	wasmir.InstrI32TruncSatF32S: OpI32TruncSatF32S,
	wasmir.InstrI32TruncSatF32U: OpI32TruncSatF32U,
	wasmir.InstrI32TruncSatF64S: OpI32TruncSatF64S,
	wasmir.InstrI32TruncSatF64U: OpI32TruncSatF64U,
	wasmir.InstrI64TruncSatF32S: OpI64TruncSatF32S,
	wasmir.InstrI64TruncSatF32U: OpI64TruncSatF32U,
	wasmir.InstrI64TruncSatF64S: OpI64TruncSatF64S,
	wasmir.InstrI64TruncSatF64U: OpI64TruncSatF64U,
}

// Instr is one instruction. Which immediate fields are meaningful depends on Op;
// the rest are zero.
type Instr struct {
	Op Op

	// LocalIndex is the immediate for local.get/set/tee.
	LocalIndex uint32

	// I32 is the immediate for i32.const, held UNSIGNED per Invariant A: an i32
	// is an unsigned integral value in [0, 2^32). Signed interpretation is the
	// emitter's job, at the few opcodes that need it.
	I32 uint32

	// BlockResults is the number of values a block/loop/if leaves on the stack.
	// Multi-value is disabled in the feature surface we target, so this is 0 or 1.
	BlockResults int
	// BlockType is that value's type, which the IR needs to size the result slot.
	BlockType ValType

	// BranchDepth is the label immediate for br and br_if; BranchTable and
	// BranchDefault are br_table's.
	BranchDepth   uint32
	BranchTable   []uint32
	BranchDefault uint32

	// FuncIndex is call's callee. TypeIndex is call_indirect's signature.
	FuncIndex uint32
	TypeIndex uint32

	// GlobalIndex is the immediate for global.get/set.
	GlobalIndex uint32

	// MemOffset is the static byte offset added to a memory address. Per spec it
	// is added in INFINITE precision and traps rather than wrapping, so the
	// emitter never has to mask it.
	MemOffset uint32

	// F32 and F64 are float constants held as raw IEEE-754 BITS, not as Go
	// floats. Round-tripping through a float would risk losing a NaN payload,
	// and the emitter needs the exact bits anyway to print an exact literal.
	F32 uint32
	F64 uint64

	// I64 is the immediate for i64.const, held UNSIGNED like I32.
	I64 uint64
}

// FuncType is a function signature.
type FuncType struct {
	Params  []ValType
	Results []ValType
}

func (t FuncType) String() string {
	ps := make([]string, len(t.Params))
	for i, p := range t.Params {
		ps[i] = p.String()
	}
	rs := make([]string, len(t.Results))
	for i, r := range t.Results {
		rs[i] = r.String()
	}
	return fmt.Sprintf("(%s) -> (%s)", strings.Join(ps, ", "), strings.Join(rs, ", "))
}

// Func is a module-defined function.
type Func struct {
	// Index is the position in the module's function index space.
	Index uint32
	// Name comes from the name section when present; otherwise it is synthesised
	// as "func[N]" so diagnostics always have something to point at.
	Name   string
	Type   FuncType
	Locals []ValType
	Body   []Instr

	// Unsupported is non-nil when this function uses something FkLua cannot
	// compile yet. The module still loads and its other functions still work;
	// calling this one raises. Failing the whole module instead would mean a
	// single i64 helper zeroes an entire spec file, hiding real progress.
	Unsupported error
}

// Import is an imported function: a function the host supplies rather than the
// module defining it.
//
// Only function imports are represented. The other kinds are refused by the
// decoder, because an imported memory, table or global shifts an index space
// the emitter numbers itself.
type Import struct {
	// Module and Name are the two-level name a host binds against, e.g.
	// "env" and "fk_log".
	Module string
	Name   string
	Type   FuncType
	// Index is the position in the function index space. Imported functions
	// occupy it from 0 upward, before any module-defined function, so a `call`
	// immediate is read against imports and definitions together.
	Index uint32
}

// Global is a module-defined global.
type Global struct {
	Type    ValType
	Mutable bool
	// InitBits is the constant initialiser, held as RAW BITS whatever the type:
	// an unsigned integer for i32/i64, an IEEE-754 pattern for f32/f64. Bits
	// rather than a Go value so a float initialiser cannot lose a NaN payload
	// on the way through, and so one field serves all four types.
	InitBits uint64
	// InitGlobal is the index of an earlier global this one copies, or -1 for a
	// literal. Those are the only two forms a constant expression can take.
	InitGlobal int
}

// Memory is the module's linear memory, in 64 KiB pages.
type Memory struct {
	Min uint32
	Max uint32 // 0 means unbounded
	Has bool
}

// DataSegment is an active data segment: bytes copied into memory at startup.
type DataSegment struct {
	Offset uint32
	Bytes  []byte
}

// ElemSegment is an active element segment: function indices written into the
// table at startup.
type ElemSegment struct {
	Offset uint32
	Funcs  []uint32
}

// Export is an exported function. Only function exports are represented; other
// export kinds are ignored, since nothing consumes them yet.
type Export struct {
	Name      string
	FuncIndex uint32
}

// Module is a decoded module.
type Module struct {
	Types []FuncType
	// Imports holds the imported functions, in declaration order. They come
	// FIRST in the function index space; Funcs is numbered after them, which is
	// why Func.Index is stored rather than inferred from position.
	Imports  []Import
	Funcs    []Func
	Exports  []Export
	Globals  []Global
	Memory   Memory
	Data     []DataSegment
	Elems    []ElemSegment
	TableMin uint32
	HasTable bool

	// Start is the index of the start function, run once at instantiation, or
	// -1 when the module has none. Ignoring it is silent wrongness: the function
	// still compiles, it simply never runs, so a guest that initialises there
	// starts up with everything unset.
	Start int

	// Custom holds the binary's custom sections other than "name", in the order
	// they appeared. Nothing in the compiler reads them: they carry the guest's
	// DWARF, which the debug map resolves into source file and line, and a
	// decoder that dropped them would leave that information unreachable
	// without a second pass over the file.
	//
	// Nil for a module decoded from text -- WAT carries no custom sections.
	Custom []CustomSection

	// CodeSpans is where each DEFINED function's body sits in the binary,
	// parallel to Funcs and in the same order. Offsets are relative to the CODE
	// SECTION PAYLOAD, which is the coordinate system DWARF's DW_AT_low_pc uses
	// for a wasm target: measured on both toolchains, low_pc equals the body's
	// payload offset exactly, and equals nothing at all when read as a file
	// offset.
	//
	// Nil for a module decoded from text, and nil rather than partial when the
	// code section cannot be walked -- the join it feeds is best effort, and a
	// wrong offset would attribute one function's source line to another.
	CodeSpans []CodeSpan
}

// CustomSection is one custom section preserved from the binary. Payload is the
// bytes after the section's name field, uninterpreted.
type CustomSection struct {
	Name    string
	Payload []byte
}

// CodeSpan is one function body's byte range within the code section payload.
// Lo is the first byte of the body (the local declarations), Hi one past its
// last.
type CodeSpan struct {
	Lo, Hi uint32
}

// CustomSectionByName returns a custom section's payload, and whether the
// module carries one by that name.
func (m *Module) CustomSectionByName(name string) ([]byte, bool) {
	for i := range m.Custom {
		if m.Custom[i].Name == name {
			return m.Custom[i].Payload, true
		}
	}
	return nil, false
}

// NumFuncs is the size of the function index space: imports plus definitions.
func (m *Module) NumFuncs() int { return len(m.Imports) + len(m.Funcs) }

// FuncTypeAt returns the signature of the function at an ABSOLUTE index, which
// spans imports first and module-defined functions after.
//
// Every consumer of a `call` immediate, an element segment or an export must go
// through this rather than indexing Funcs, which is offset by the import count.
func (m *Module) FuncTypeAt(i uint32) (FuncType, bool) {
	if int(i) < len(m.Imports) {
		return m.Imports[i].Type, true
	}
	j := int(i) - len(m.Imports)
	if j >= len(m.Funcs) {
		return FuncType{}, false
	}
	return m.Funcs[j].Type, true
}

// externalKindName names an import kind for a diagnostic.
func externalKindName(k wasmir.ExternalKind) string {
	switch k {
	case wasmir.ExternalKindFunction:
		return "function"
	case wasmir.ExternalKindTable:
		return "table"
	case wasmir.ExternalKindMemory:
		return "memory"
	case wasmir.ExternalKindGlobal:
		return "global"
	case wasmir.ExternalKindTag:
		return "tag"
	}
	return fmt.Sprintf("kind#%d", uint8(k))
}

// FuncByExport finds an exported function by name.
func (m *Module) FuncByExport(name string) (*Func, bool) {
	for _, e := range m.Exports {
		if e.Name == name {
			for i := range m.Funcs {
				if m.Funcs[i].Index == e.FuncIndex {
					return &m.Funcs[i], true
				}
			}
		}
	}
	return nil, false
}

// UnsupportedError reports an instruction FkLua cannot compile yet. It names the
// instruction and where it appeared, and says which milestone adds it -- the
// reason this package exists rather than exposing watgo directly.
type UnsupportedError struct {
	Op       string // WebAssembly text-format opcode name
	Func     string
	Offset   int    // instruction index within the function body
	Planned  string // milestone that adds it, or "" if not planned
	Detail   string // optional extra context
	FuncType string
}

func (e *UnsupportedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "unsupported instruction %s", e.Op)
	if e.Func != "" {
		fmt.Fprintf(&b, " in function %q", e.Func)
	}
	if e.Offset >= 0 {
		fmt.Fprintf(&b, " at body offset %d", e.Offset)
	}
	if e.Planned != "" {
		fmt.Fprintf(&b, ": planned for %s", e.Planned)
	} else {
		b.WriteString(": not planned")
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " (%s)", e.Detail)
	}
	return b.String()
}

// milestoneFor guesses which milestone adds an opcode, from its name. A rough
// answer that orients the reader beats no answer; the alternative is "this does
// not work" with no indication of whether that is temporary.
func milestoneFor(op string) (milestone, detail string) {
	switch {
	case strings.HasPrefix(op, "v128.") || strings.Contains(op, "x16.") ||
		strings.Contains(op, "x8.") || strings.Contains(op, "x4.") ||
		strings.Contains(op, "x2."):
		return "", "SIMD is not on the roadmap: 128-bit vectors have no efficient form in a Lua sandbox"
	case strings.HasPrefix(op, "ref.") || strings.HasPrefix(op, "struct.") ||
		strings.HasPrefix(op, "array.") || strings.HasPrefix(op, "any.") ||
		strings.HasPrefix(op, "extern.") || strings.HasPrefix(op, "i31."):
		return "", "reference types and GC are not on the roadmap"
	case strings.HasPrefix(op, "i64.") || strings.HasPrefix(op, "f32.") ||
		strings.HasPrefix(op, "f64."):
		return "M3", "i64 and floating point"
	// Conversions are named for their RESULT type, so i32.trunc_f32_s and
	// i32.wrap_i64 are i32-prefixed but still need the wide types.
	case strings.Contains(op, "i64") || strings.Contains(op, "f32") ||
		strings.Contains(op, "f64"):
		return "M3", "i64 and floating point"
	// The rest of bulk memory. memory.copy and memory.fill are BUILT -- they
	// are the two the guest toolchains actually emit, and a word-wise runtime
	// helper beats the byte loop binaryen lowers them into by 45x. The
	// segment-indexed ones need the data/elem sections kept live past
	// instantiation, which is a different change.
	case op == "memory.init" || op == "data.drop" ||
		op == "table.copy" || op == "table.init" || op == "elem.drop":
		return "", "the segment-indexed half of bulk memory, which needs the data " +
			"and elem sections kept live past instantiation. No guest toolchain has " +
			"been observed emitting one, so it is unscheduled rather than pending"
	case strings.HasPrefix(op, "memory.") || strings.Contains(op, ".load") ||
		strings.Contains(op, ".store"):
		return "M2", "linear memory"
	case op == "block" || op == "loop" || op == "if" || op == "else" ||
		op == "br" || op == "br_if" || op == "br_table" ||
		op == "call" || op == "call_indirect" || op == "select" ||
		op == "global.get" || op == "global.set":
		return "M2", "control flow and calls"
	}
	return "", ""
}

// Decode decodes and validates a binary module.
func Decode(b []byte) (*Module, error) {
	wm, err := watgo.DecodeWASM(b)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := watgo.ValidateModule(wm); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	m, err := convert(wm)
	if err != nil {
		return nil, err
	}
	// The body offsets, which only the raw bytes carry -- see codespans.go. A
	// count that disagrees with the decoded function list means the walk and
	// the decoder read different things, so the whole answer is dropped rather
	// than half-trusted.
	if spans := codeSpans(b); len(spans) == len(m.Funcs) {
		m.CodeSpans = spans
	}
	return m, nil
}

// DecodeWAT compiles WebAssembly text and decodes it. Used by tests, which are
// far more readable in WAT than as byte slices.
func DecodeWAT(src string) (*Module, error) {
	wm, err := watgo.ParseAndValidateWAT([]byte(src))
	if err != nil {
		return nil, fmt.Errorf("parse wat: %w", err)
	}
	return convert(wm)
}

func convert(wm *wasmir.Module) (*Module, error) {
	m := &Module{Start: -1}

	// Custom sections ride through untouched. watgo already handles "name"
	// separately and never puts it here, which is what we want: the names are
	// on Func, and what is left is the DWARF the debug map reads.
	for _, cs := range wm.CustomSections {
		m.Custom = append(m.Custom, CustomSection{Name: cs.Name, Payload: cs.Payload})
	}

	for i, td := range wm.Types {
		if td.Kind != wasmir.TypeDefKindFunc {
			return nil, fmt.Errorf("type %d: only function types are supported", i)
		}
		ft, err := convertFuncType(td)
		if err != nil {
			return nil, fmt.Errorf("type %d: %w", i, err)
		}
		m.Types = append(m.Types, ft)
	}

	// Imported functions occupy the low end of the function index space, so
	// they have to be counted before a single module-defined function is
	// numbered. Every other index space works the same way, which is why the
	// kinds we do not implement are refused rather than skipped: an ignored
	// global import shifts every global.get by one and produces a module that
	// runs and is wrong.
	for _, im := range wm.Imports {
		switch im.Kind {
		case wasmir.ExternalKindFunction:
			if int(im.TypeIdx) >= len(m.Types) {
				return nil, fmt.Errorf("import %q.%q: type index %d out of range",
					im.Module, im.Name, im.TypeIdx)
			}
			m.Imports = append(m.Imports, Import{
				Module: im.Module, Name: im.Name,
				Type:  m.Types[im.TypeIdx],
				Index: uint32(len(m.Imports)),
			})
		default:
			return nil, fmt.Errorf("import %q.%q: only function imports are supported, "+
				"and an imported %s shifts an index space we do not track",
				im.Module, im.Name, externalKindName(im.Kind))
		}
	}

	for i, wf := range wm.Funcs {
		name := wf.Name
		if name == "" {
			name = fmt.Sprintf("func[%d]", len(m.Imports)+i)
		}
		if int(wf.TypeIdx) >= len(m.Types) {
			return nil, fmt.Errorf("function %q: type index %d out of range", name, wf.TypeIdx)
		}
		ft := m.Types[wf.TypeIdx]

		fn := Func{Index: uint32(len(m.Imports) + i), Name: name, Type: ft}

		for j, lv := range wf.Locals {
			vt, err := convertValType(lv)
			if err != nil {
				fn.Unsupported = fmt.Errorf("local %d: %w", j, err)
				break
			}
			fn.Locals = append(fn.Locals, vt)
		}

		if fn.Unsupported == nil {
			body, err := convertBody(wf.Body, name, ft, wm)
			if err != nil {
				// One unsupported instruction disables this function only. The
				// module still loads, so a spec file with a single i64 helper
				// still exercises everything else.
				fn.Unsupported = err
			} else {
				fn.Body = body
			}
		}
		m.Funcs = append(m.Funcs, fn)
	}

	for _, ex := range wm.Exports {
		if ex.Kind != wasmir.ExternalKindFunction {
			continue
		}
		m.Exports = append(m.Exports, Export{Name: ex.Name, FuncIndex: ex.Index})
	}

	if wm.StartFuncIndex != nil {
		m.Start = int(*wm.StartFuncIndex)
	}

	for i, g := range wm.Globals {
		vt, err := convertValType(g.Type)
		if err != nil {
			return nil, fmt.Errorf("global %d: %w", i, err)
		}
		out := Global{Type: vt, Mutable: g.Mutable, InitGlobal: -1}
		// A constant expression is a single typed constant or a global.get,
		// then end. All four numeric types are accepted; rejecting f32/f64 here
		// used to fail the whole module, which is what made unreachable.wast
		// unrunnable despite having nothing to do with globals.
		for _, in := range g.Init {
			switch in.Kind {
			case wasmir.InstrI32Const:
				out.InitBits = uint64(uint32(in.I32Const))
			case wasmir.InstrI64Const:
				out.InitBits = uint64(in.I64Const)
			case wasmir.InstrF32Const:
				out.InitBits = uint64(in.F32Const)
			case wasmir.InstrF64Const:
				out.InitBits = in.F64Const
			case wasmir.InstrGlobalGet:
				out.InitGlobal = int(in.GlobalIndex)
			case wasmir.InstrEnd:
			default:
				return nil, fmt.Errorf(
					"global %d: initialiser is not a constant expression", i)
			}
		}
		m.Globals = append(m.Globals, out)
	}

	if len(wm.Memories) > 1 {
		return nil, fmt.Errorf("multiple memories are not supported")
	}
	if len(wm.Memories) == 1 {
		mem := wm.Memories[0]
		m.Memory.Has = true
		m.Memory.Min = uint32(mem.Min)
		if mem.Max != nil {
			m.Memory.Max = uint32(*mem.Max)
		}
	}

	for i, d := range wm.Data {
		if d.Mode != wasmir.DataSegmentModeActive {
			return nil, fmt.Errorf("data segment %d: only active segments are supported", i)
		}
		off, err := constOffset(d.OffsetExpr, m.Globals)
		if err != nil {
			return nil, fmt.Errorf("data segment %d: %w", i, err)
		}
		m.Data = append(m.Data, DataSegment{Offset: off, Bytes: d.Init})
	}

	if len(wm.Tables) > 1 {
		return nil, fmt.Errorf("multiple tables are not supported")
	}
	if len(wm.Tables) == 1 {
		m.HasTable = true
		m.TableMin = uint32(wm.Tables[0].Min)
	}
	for i, e := range wm.Elements {
		if e.Mode != wasmir.ElemSegmentModeActive {
			return nil, fmt.Errorf("element segment %d: only active segments are supported", i)
		}
		off, err := constOffset(e.OffsetExpr, m.Globals)
		if err != nil {
			return nil, fmt.Errorf("element segment %d: %w", i, err)
		}
		m.Elems = append(m.Elems, ElemSegment{Offset: off, Funcs: e.FuncIndices})
	}

	return m, nil
}

// constOffset evaluates a data/element segment offset, which the spec limits to
// a constant expression.
func constOffset(expr []wasmir.Instruction, globals []Global) (uint32, error) {
	for _, in := range expr {
		switch in.Kind {
		case wasmir.InstrI32Const:
			return uint32(in.I32Const), nil
		case wasmir.InstrGlobalGet:
			gi := int(in.GlobalIndex)
			if gi >= len(globals) {
				return 0, fmt.Errorf("offset reads global %d, which is not declared", gi)
			}
			return uint32(globals[gi].InitBits), nil
		case wasmir.InstrEnd:
		default:
			return 0, fmt.Errorf("offset is not a constant expression")
		}
	}
	return 0, nil
}

// convertBody lowers one function body, or reports the first instruction that
// cannot be compiled.
func convertBody(body []wasmir.Instruction, name string, ft FuncType, wm *wasmir.Module) ([]Instr, error) {
	out := make([]Instr, 0, len(body))
	for j, in := range body {
		op, ok := supportedOps[in.Kind]
		if !ok {
			opName, known := kindNames[in.Kind]
			if !known {
				opName = fmt.Sprintf("kind#%d", uint16(in.Kind))
			}
			ms, detail := milestoneFor(opName)
			return nil, &UnsupportedError{
				Op: opName, Func: name, Offset: j,
				Planned: ms, Detail: detail, FuncType: ft.String(),
			}
		}
		ci := Instr{Op: op}
		switch op {
		case OpLocalGet, OpLocalSet, OpLocalTee:
			ci.LocalIndex = in.LocalIndex
		case OpI32Const:
			ci.I32 = uint32(in.I32Const)
		case OpI64Const:
			ci.I64 = uint64(in.I64Const)
		case OpF32Const:
			ci.F32 = in.F32Const
		case OpF64Const:
			ci.F64 = in.F64Const
		case OpBlock, OpLoop, OpIf:
			// Multi-value is outside the feature surface, so a block type is
			// either empty or one result; an indexed block type means the
			// module uses multi-value after all.
			if in.BlockTypeUsesIndex {
				return nil, &UnsupportedError{
					Op: "multi-value " + op.String(), Func: name, Offset: j,
					Detail: "blocks with parameters or multiple results",
				}
			}
			if in.BlockType != nil {
				vt, err := convertValType(*in.BlockType)
				if err != nil {
					return nil, fmt.Errorf("%s at offset %d: %w", op, j, err)
				}
				ci.BlockResults = 1
				ci.BlockType = vt
			}
		case OpBr, OpBrIf:
			ci.BranchDepth = in.BranchDepth
		case OpBrTable:
			ci.BranchTable = in.BranchTable
			ci.BranchDefault = in.BranchDefault
		case OpCall:
			ci.FuncIndex = in.FuncIndex
		case OpCallIndirect:
			ci.TypeIndex = in.CallTypeIndex
			if in.TableIndex != 0 {
				return nil, &UnsupportedError{
					Op: "call_indirect", Func: name, Offset: j,
					Detail: "only table 0 is supported",
				}
			}
		case OpGlobalGet, OpGlobalSet:
			ci.GlobalIndex = in.GlobalIndex
		case OpI32Load, OpI32Load8S, OpI32Load8U, OpI32Load16S, OpI32Load16U,
			OpI32Store, OpI32Store8, OpI32Store16,
			OpF32Load, OpF64Load, OpF32Store, OpF64Store,
			OpI64Load, OpI64Load8S, OpI64Load8U, OpI64Load16S, OpI64Load16U,
			OpI64Load32S, OpI64Load32U,
			OpI64Store, OpI64Store8, OpI64Store16, OpI64Store32:
			if in.MemoryOffset > 0xFFFFFFFF {
				return nil, fmt.Errorf("%s at offset %d: memory offset out of range", op, j)
			}
			ci.MemOffset = uint32(in.MemoryOffset)
		}
		out = append(out, ci)
	}
	return out, nil
}

func convertFuncType(td wasmir.TypeDef) (FuncType, error) {
	var ft FuncType
	for i, p := range td.Params {
		vt, err := convertValType(p)
		if err != nil {
			return ft, fmt.Errorf("param %d: %w", i, err)
		}
		ft.Params = append(ft.Params, vt)
	}
	for i, r := range td.Results {
		vt, err := convertValType(r)
		if err != nil {
			return ft, fmt.Errorf("result %d: %w", i, err)
		}
		ft.Results = append(ft.Results, vt)
	}
	return ft, nil
}

func convertValType(v wasmir.ValueType) (ValType, error) {
	switch v.Kind {
	case wasmir.ValueKindI32:
		return I32, nil
	case wasmir.ValueKindI64:
		return I64, nil
	case wasmir.ValueKindF32:
		return F32, nil
	case wasmir.ValueKindF64:
		return F64, nil
	}
	return 0, fmt.Errorf("unsupported value type (kind %d): only i32, i64, f32 and f64 are supported",
		uint8(v.Kind))
}
